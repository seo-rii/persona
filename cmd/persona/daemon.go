package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"persona/internal/gitx"
	"persona/internal/model"
	"persona/internal/ns"
	"persona/internal/patchio"
	"persona/internal/session"

	"github.com/spf13/cobra"
)

const (
	daemonSocketFile     = "persona.sock"
	daemonLaunchLock     = "launch.lock"
	daemonProtocol       = 2
	daemonMetadataSchema = 1
	daemonMetadataLimit  = 64 << 10
	daemonRequestTimeout = 30 * time.Second
)

type daemonFlagValues struct {
	sessionKey  string
	baseMode    string
	baseRef     string
	allowDirty  bool
	ignoredMode string
	ignoredMax  int
	applyMode   string
}

type daemonSessionConfig struct {
	BaseMode    model.BaseMode    `json:"base_mode"`
	BaseRef     string            `json:"base_ref"`
	AllowDirty  bool              `json:"allow_dirty"`
	IgnoredMode model.IgnoredMode `json:"ignored_mode"`
	IgnoredMax  int               `json:"ignored_max"`
	ApplyMode   model.ApplyMode   `json:"apply_mode"`
}

type daemonRequest struct {
	Method         string              `json:"method"`
	RepoRoot       string              `json:"repo_root,omitempty"`
	GitDir         string              `json:"git_dir,omitempty"`
	SessionKey     string              `json:"session_key,omitempty"`
	LeaseID        string              `json:"lease_id,omitempty"`
	OwnerPID       int                 `json:"owner_pid,omitempty"`
	IdleForSeconds int64               `json:"idle_for_seconds,omitempty"`
	IdleForNanos   int64               `json:"idle_for_nanos,omitempty"`
	MinAgeNanos    int64               `json:"min_age_nanos,omitempty"`
	ViewGeneration uint64              `json:"view_generation,omitempty"`
	DeadlineNanos  int64               `json:"deadline_nanos,omitempty"`
	DryRun         bool                `json:"dry_run,omitempty"`
	Config         daemonSessionConfig `json:"config"`
}

type daemonResponse struct {
	Error    string              `json:"error,omitempty"`
	Code     int                 `json:"code,omitempty"`
	Session  *daemonSessionInfo  `json:"session,omitempty"`
	Sessions []daemonSessionInfo `json:"sessions,omitempty"`
	Pruned   []string            `json:"pruned,omitempty"`
	LeaseID  string              `json:"lease_id,omitempty"`
	Ended    bool                `json:"ended,omitempty"`
	Version  int                 `json:"version,omitempty"`
	RepoRoot string              `json:"repo_root,omitempty"`
	GitDir   string              `json:"git_dir,omitempty"`
}

type daemonSessionInfo struct {
	SessionKey            string `json:"session_key"`
	RepoRoot              string `json:"repo_root"`
	GitDir                string `json:"git_dir"`
	ViewPath              string `json:"view_path"`
	PatchPath             string `json:"patch_path"`
	Dirty                 bool   `json:"dirty"`
	Busy                  bool   `json:"busy"`
	BusyOwnerPID          int    `json:"busy_owner_pid,omitempty"`
	CreatedUnix           int64  `json:"created_unix"`
	CreatedRFC3339        string `json:"created_rfc3339"`
	LastUsedUnix          int64  `json:"last_used_unix"`
	LastUsedRFC3339       string `json:"last_used_rfc3339"`
	LastFlushedUnix       int64  `json:"last_flushed_unix"`
	LastFlushedRFC3339    string `json:"last_flushed_rfc3339"`
	FlushCount            int    `json:"flush_count"`
	FlushSkipped          int    `json:"flush_skipped"`
	RecoveredCount        int    `json:"recovered_count"`
	ViewGeneration        uint64 `json:"view_generation"`
	Generation            uint64 `json:"generation"`
	LastDurableGeneration uint64 `json:"last_durable_generation"`
	BaseOID               string `json:"base_oid"`
	State                 string `json:"state"`
	LastError             string `json:"last_error,omitempty"`
	RecoveryRoot          string `json:"recovery_root,omitempty"`
	ExternalRoot          string `json:"external_root,omitempty"`
}

type daemonDeps struct {
	newGit    func(string, string, bool) model.GitOps
	newMount  func() model.NSOps
	now       func() time.Time
	writeMeta func(string, daemonSessionMeta) error
}

type daemonState struct {
	mu       sync.Mutex
	repoRoot string
	gitDir   string
	deps     daemonDeps
	log      *slog.Logger
	sessions map[string]*daemonSession
	gates    map[string]chan struct{}
	cached   map[string]daemonSessionInfo
	closing  bool
}

type daemonSession struct {
	key                   string
	config                daemonSessionConfig
	info                  daemonSessionInfo
	metaPath              string
	store                 *patchio.PatchStore
	lock                  *patchio.PatchLock
	sess                  *session.Session
	cleanup               *cleanupStack
	g                     model.GitOps
	mount                 model.NSOps
	menv                  *mountEnv
	patchInRepo           bool
	patchRel              string
	ignoredMasks          []string
	initialIgnored        []string
	busy                  bool
	busyOwnerPID          int
	busyLease             string
	createdAt             time.Time
	lastUsed              time.Time
	lastFlushed           time.Time
	flushCount            int
	flushSkipped          int
	recoveredCount        int
	dirty                 bool
	baseOID               string
	generation            uint64
	lastDurableGeneration uint64
	viewGeneration        uint64
	lastError             string
	patchChecksum         string
	busyOwnerStart        string
	closed                bool
	preserve              *bool
}

type daemonSessionMeta struct {
	SchemaVersion         int                 `json:"schema_version,omitempty"`
	SessionKey            string              `json:"session_key"`
	Config                daemonSessionConfig `json:"config"`
	SessionRoot           string              `json:"session_root"`
	CreatedUnix           int64               `json:"created_unix"`
	LastUsedUnix          int64               `json:"last_used_unix"`
	LastFlushedUnix       int64               `json:"last_flushed_unix"`
	FlushCount            int                 `json:"flush_count"`
	FlushSkipped          int                 `json:"flush_skipped"`
	RecoveredCount        int                 `json:"recovered_count"`
	Dirty                 bool                `json:"dirty"`
	BaseOID               string              `json:"base_oid,omitempty"`
	Generation            uint64              `json:"generation,omitempty"`
	LastDurableGeneration uint64              `json:"last_durable_generation,omitempty"`
	ViewGeneration        uint64              `json:"view_generation,omitempty"`
	LastError             string              `json:"last_error,omitempty"`
	BusyOwnerPID          int                 `json:"busy_owner_pid,omitempty"`
	BusyOwnerStart        string              `json:"busy_owner_start,omitempty"`
	InitialIgnored        []string            `json:"initial_ignored,omitempty"`
	ExternalRoot          string              `json:"external_root,omitempty"`
	PatchChecksum         string              `json:"patch_checksum,omitempty"`
}

func addDaemonCommands(root *cobra.Command) {
	daemonCmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage persistent overlay sessions for plugins and tools",
	}

	var execFlags daemonFlagValues
	execCmd := &cobra.Command{
		Use:           "exec [OPTIONS] -- <command> [args...]",
		Short:         "Run a command inside a persistent daemon session",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "command is required")
				_ = cmd.Help()
				return &exitError{code: model.ExitEnv}
			}
			cfg, err := buildDaemonSessionConfig(execFlags)
			if err != nil {
				return &model.PersonaError{Code: model.ExitEnv, Op: "parse daemon options", Err: err}
			}
			return runDaemonExec(cmd.Context(), strings.TrimSpace(execFlags.sessionKey), cfg, args)
		},
	}
	bindDaemonSessionFlags(execCmd, &execFlags)

	var infoFlags daemonFlagValues
	var infoJSON bool
	infoCmd := &cobra.Command{
		Use:           "info [OPTIONS]",
		Short:         "Ensure a daemon session exists and print its stable view path",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := buildDaemonSessionConfig(infoFlags)
			if err != nil {
				return &model.PersonaError{Code: model.ExitEnv, Op: "parse daemon options", Err: err}
			}
			info, err := daemonEnsureInfo(cmd.Context(), strings.TrimSpace(infoFlags.sessionKey), cfg)
			if err != nil {
				return err
			}
			if infoJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(info)
			}
			fmt.Fprintf(
				cmd.OutOrStdout(),
				"session_key=%s\nrepo_root=%s\ngit_dir=%s\nview_path=%s\npatch_path=%s\ndirty=%t\ncreated=%s\nlast_used=%s\nlast_flushed=%s\nflush_count=%d\nflush_skipped=%d\nrecovered_count=%d\n",
				info.SessionKey,
				info.RepoRoot,
				info.GitDir,
				info.ViewPath,
				info.PatchPath,
				info.Dirty,
				info.CreatedRFC3339,
				info.LastUsedRFC3339,
				info.LastFlushedRFC3339,
				info.FlushCount,
				info.FlushSkipped,
				info.RecoveredCount,
			)
			return nil
		},
	}
	bindDaemonSessionFlags(infoCmd, &infoFlags)
	infoCmd.Flags().BoolVar(&infoJSON, "json", false, "print daemon session info as JSON")

	var listJSON bool
	listCmd := &cobra.Command{
		Use:           "list",
		Short:         "List daemon sessions for the current repository",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			sessions, err := runDaemonList(cmd.Context())
			if err != nil {
				return err
			}
			if listJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(sessions)
			}
			for _, sess := range sessions {
				fmt.Fprintf(
					cmd.OutOrStdout(),
					"session_key=%s busy=%t dirty=%t last_used=%s last_flushed=%s flush_count=%d flush_skipped=%d recovered_count=%d view_path=%s patch_path=%s\n",
					sess.SessionKey,
					sess.Busy,
					sess.Dirty,
					sess.LastUsedRFC3339,
					sess.LastFlushedRFC3339,
					sess.FlushCount,
					sess.FlushSkipped,
					sess.RecoveredCount,
					sess.ViewPath,
					sess.PatchPath,
				)
			}
			return nil
		},
	}
	listCmd.Flags().BoolVar(&listJSON, "json", false, "print daemon sessions as JSON")

	var flushSessionKey string
	var flushMinAge string
	flushCmd := &cobra.Command{
		Use:           "flush --session-key <key>",
		Short:         "Write the current daemon view back into its patch file",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			minAge, err := time.ParseDuration(strings.TrimSpace(flushMinAge))
			if err != nil {
				return &model.PersonaError{Code: model.ExitEnv, Op: "parse flush min-age", Err: err}
			}
			if minAge < 0 {
				return &model.PersonaError{Code: model.ExitEnv, Op: "parse flush min-age", Err: fmt.Errorf("min-age must be >= 0")}
			}
			return runDaemonFlush(cmd.Context(), strings.TrimSpace(flushSessionKey), minAge)
		},
	}
	flushCmd.Flags().StringVar(&flushSessionKey, "session-key", "", "external session key, such as a Claude chat session id")
	flushCmd.Flags().StringVar(&flushMinAge, "min-age", "0s", "skip the flush when the last successful daemon flush is newer than this duration")
	_ = flushCmd.MarkFlagRequired("session-key")

	var markDirtySessionKey string
	markDirtyCmd := &cobra.Command{
		Use:           "mark-dirty --session-key <key>",
		Short:         "Mark a daemon session as having pending mutations",
		SilenceErrors: true,
		SilenceUsage:  true,
		Hidden:        true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonMarkDirty(cmd.Context(), strings.TrimSpace(markDirtySessionKey))
		},
	}
	markDirtyCmd.Flags().StringVar(&markDirtySessionKey, "session-key", "", "external session key, such as a Claude chat session id")
	_ = markDirtyCmd.MarkFlagRequired("session-key")

	var pruneIdleFor string
	var pruneJSON bool
	pruneCmd := &cobra.Command{
		Use:           "prune --idle-for <duration>",
		Short:         "End idle daemon sessions for the current repository",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			idleFor, err := time.ParseDuration(strings.TrimSpace(pruneIdleFor))
			if err != nil {
				return &model.PersonaError{Code: model.ExitEnv, Op: "parse prune duration", Err: err}
			}
			if idleFor < 0 {
				return &model.PersonaError{Code: model.ExitEnv, Op: "parse prune duration", Err: fmt.Errorf("idle-for must be >= 0")}
			}
			pruned, err := runDaemonPrune(cmd.Context(), idleFor)
			if err != nil {
				return err
			}
			if pruneJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"pruned": pruned})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "pruned=%d\n", len(pruned))
			for _, key := range pruned {
				fmt.Fprintln(cmd.OutOrStdout(), key)
			}
			return nil
		},
	}
	pruneCmd.Flags().StringVar(&pruneIdleFor, "idle-for", "", "prune sessions idle for at least this duration (for example 30m or 24h)")
	pruneCmd.Flags().BoolVar(&pruneJSON, "json", false, "print pruned session keys as JSON")
	_ = pruneCmd.MarkFlagRequired("idle-for")

	var endSessionKey string
	endCmd := &cobra.Command{
		Use:           "end --session-key <key>",
		Short:         "Flush and remove a daemon session",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonEnd(cmd.Context(), strings.TrimSpace(endSessionKey))
		},
	}
	endCmd.Flags().StringVar(&endSessionKey, "session-key", "", "external session key, such as a Claude chat session id")
	_ = endCmd.MarkFlagRequired("session-key")

	var serveRepoRoot string
	var serveGitDir string
	var serveSocket string
	serveCmd := &cobra.Command{
		Use:           "serve",
		Short:         "Run the persona daemon server in the foreground",
		SilenceErrors: true,
		SilenceUsage:  true,
		Hidden:        true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonServe(cmd.Context(), strings.TrimSpace(serveRepoRoot), strings.TrimSpace(serveGitDir), strings.TrimSpace(serveSocket))
		},
	}
	serveCmd.Flags().StringVar(&serveRepoRoot, "repo-root", "", "repo root for the daemon server")
	serveCmd.Flags().StringVar(&serveGitDir, "git-dir", "", "git dir for the daemon server")
	serveCmd.Flags().StringVar(&serveSocket, "socket", "", "unix socket path for the daemon server")

	addDaemonLeaseCommands(daemonCmd)
	daemonCmd.AddCommand(execCmd, infoCmd, listCmd, flushCmd, markDirtyCmd, pruneCmd, endCmd, serveCmd)
	root.AddCommand(daemonCmd)
}

func bindDaemonSessionFlags(cmd *cobra.Command, values *daemonFlagValues) {
	cmd.Flags().StringVar(&values.sessionKey, "session-key", "", "external session key, such as a Claude chat session id")
	cmd.Flags().StringVar(&values.baseMode, "base-mode", string(model.BaseWorktree), "base mode: repo | worktree")
	cmd.Flags().StringVar(&values.baseRef, "base-ref", "HEAD", "base ref for worktree mode")
	cmd.Flags().BoolVar(&values.allowDirty, "allow-dirty", false, "allow dirty repo in repo base mode")
	cmd.Flags().StringVar(&values.ignoredMode, "ignored-mode", string(model.IgnoredTransparent), "ignored mode: transparent | readonly | masked")
	cmd.Flags().IntVar(&values.ignoredMax, "ignored-max", 200, "max ignored entries to process")
	cmd.Flags().StringVar(&values.applyMode, "apply-mode", string(model.ApplyStrict), "apply mode: strict | reject")
	_ = cmd.MarkFlagRequired("session-key")
}

func buildDaemonSessionConfig(values daemonFlagValues) (daemonSessionConfig, error) {
	mode, err := parseEnum(values.baseMode, "base-mode", model.BaseRepo, model.BaseWorktree)
	if err != nil {
		return daemonSessionConfig{}, err
	}
	baseRef := strings.TrimSpace(values.baseRef)
	if baseRef == "" {
		baseRef = "HEAD"
	}
	if mode == model.BaseRepo && baseRef != "HEAD" {
		return daemonSessionConfig{}, fmt.Errorf("base-ref is only valid with worktree base-mode")
	}
	ignoredMode, err := parseEnum(values.ignoredMode, "ignored-mode", model.IgnoredTransparent, model.IgnoredReadonly, model.IgnoredMasked)
	if err != nil {
		return daemonSessionConfig{}, err
	}
	if values.ignoredMax < 0 {
		return daemonSessionConfig{}, fmt.Errorf("ignored-max must be >= 0")
	}
	applyMode, err := parseEnum(values.applyMode, "apply-mode", model.ApplyStrict, model.ApplyReject)
	if err != nil {
		return daemonSessionConfig{}, err
	}
	return daemonSessionConfig{
		BaseMode:    mode,
		BaseRef:     baseRef,
		AllowDirty:  values.allowDirty,
		IgnoredMode: ignoredMode,
		IgnoredMax:  values.ignoredMax,
		ApplyMode:   applyMode,
	}, nil
}

func (cfg daemonSessionConfig) options() model.Options {
	return model.Options{
		BaseMode:    cfg.BaseMode,
		BaseRef:     cfg.BaseRef,
		AllowDirty:  cfg.AllowDirty,
		IgnoredMode: cfg.IgnoredMode,
		IgnoredMax:  cfg.IgnoredMax,
		ApplyMode:   cfg.ApplyMode,
		KeepSession: model.KeepNever,
	}
}

func (cfg daemonSessionConfig) equal(other daemonSessionConfig) bool {
	return cfg == other
}

func defaultDaemonDeps() daemonDeps {
	return daemonDeps{
		newGit: func(repoRoot, gitDir string, verbose bool) model.GitOps {
			return &gitx.Git{RepoRoot: repoRoot, GitDir: gitDir, Verbose: verbose}
		},
		newMount: func() model.NSOps {
			return ns.RealNS{}
		},
		now: time.Now,
	}
}

func runDaemonExec(ctx context.Context, sessionKey string, cfg daemonSessionConfig, args []string) error {
	repoRoot, gitDir, cwdRel, err := daemonRepoContext(ctx)
	if err != nil {
		return err
	}
	client, err := newDaemonClient(ctx, repoRoot, gitDir)
	if err != nil {
		return err
	}
	resp, err := client.acquireExec(ctx, sessionKey, cfg)
	if err != nil {
		return err
	}
	exe, exeErr := os.Executable()
	if exeErr != nil {
		_ = client.releaseExec(context.Background(), sessionKey, resp.LeaseID)
		return exeErr
	}
	helperArgs := []string{exe, "daemon", "exec-view", "--repo-root", repoRoot, "--view-path", resp.Session.ViewPath, "--cwd-rel", cwdRel, "--"}
	helperArgs = append(helperArgs, args...)
	childCode := runCommand(repoRoot, cwdRel, helperArgs)
	if err := client.releaseExec(context.Background(), sessionKey, resp.LeaseID); err != nil {
		return err
	}
	if childCode != 0 {
		return &exitError{code: model.ExitCode(childCode)}
	}
	return nil
}

func daemonEnsureInfo(ctx context.Context, sessionKey string, cfg daemonSessionConfig) (*daemonSessionInfo, error) {
	repoRoot, gitDir, _, err := daemonRepoContext(ctx)
	if err != nil {
		return nil, err
	}
	client, err := newDaemonClient(ctx, repoRoot, gitDir)
	if err != nil {
		return nil, err
	}
	return client.ensure(ctx, sessionKey, cfg)
}

func runDaemonEnd(ctx context.Context, sessionKey string) error {
	repoRoot, gitDir, _, err := daemonRepoContext(ctx)
	if err != nil {
		return err
	}
	client, err := newDaemonClient(ctx, repoRoot, gitDir)
	if err != nil {
		return err
	}
	return client.end(ctx, sessionKey)
}

func runDaemonFlush(ctx context.Context, sessionKey string, minAge time.Duration) error {
	repoRoot, gitDir, _, err := daemonRepoContext(ctx)
	if err != nil {
		return err
	}
	client, err := newDaemonClient(ctx, repoRoot, gitDir)
	if err != nil {
		return err
	}
	return client.flush(ctx, sessionKey, minAge)
}

func runDaemonMarkDirty(ctx context.Context, sessionKey string) error {
	repoRoot, gitDir, _, err := daemonRepoContext(ctx)
	if err != nil {
		return err
	}
	client, err := newDaemonClient(ctx, repoRoot, gitDir)
	if err != nil {
		return err
	}
	return client.markDirty(ctx, sessionKey)
}

func runDaemonList(ctx context.Context) ([]daemonSessionInfo, error) {
	repoRoot, gitDir, _, err := daemonRepoContext(ctx)
	if err != nil {
		return nil, err
	}
	client, err := newDaemonClient(ctx, repoRoot, gitDir)
	if err != nil {
		return nil, err
	}
	return client.list(ctx)
}

func runDaemonPrune(ctx context.Context, idleFor time.Duration) ([]string, error) {
	repoRoot, gitDir, _, err := daemonRepoContext(ctx)
	if err != nil {
		return nil, err
	}
	client, err := newDaemonClient(ctx, repoRoot, gitDir)
	if err != nil {
		return nil, err
	}
	return client.prune(ctx, idleFor)
}

func daemonRepoContext(ctx context.Context) (string, string, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", "", model.Wrap(model.ExitEnv, "getwd", err)
	}
	repoRoot, gitDir, err := gitx.DetectRepo(ctx, cwd)
	if err != nil {
		return "", "", "", model.Wrap(model.ExitRepo, "detect repo", err)
	}
	cwdRel, err := filepath.Rel(repoRoot, cwd)
	if err != nil {
		cwdRel = "."
	}
	return repoRoot, gitDir, cwdRel, nil
}

func runDaemonServe(parent context.Context, repoRoot, gitDir, socketPath string) error {
	if repoRoot == "" || gitDir == "" {
		detectedRepo, detectedGit, _, err := daemonRepoContext(parent)
		if err != nil {
			return err
		}
		if repoRoot == "" {
			repoRoot = detectedRepo
		}
		if gitDir == "" {
			gitDir = detectedGit
		}
	}
	if socketPath == "" {
		socketPath = daemonSocketPath(gitDir)
	}
	if _, err := ensureDaemonDir(gitDir); err != nil {
		return err
	}
	runtimeDir := filepath.Dir(socketPath)
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(runtimeDir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("daemon socket directory must be private: %s", runtimeDir)
	}
	life, err := os.OpenFile(filepath.Join(gitDir, "persona", "daemon", "serve.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer life.Close()
	if err := unix.Flock(int(life.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("daemon already running: %w", err)
	}
	defer unix.Flock(int(life.Fd()), unix.LOCK_UN)
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return model.Wrap(model.ExitEnv, "listen daemon socket", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	if err := os.Chmod(socketPath, 0o600); err != nil {
		return model.Wrap(model.ExitEnv, "chmod daemon socket", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	state := &daemonState{
		repoRoot: repoRoot,
		gitDir:   gitDir,
		deps:     defaultDaemonDeps(),
		log:      log.With("component", "persona-daemon"),
		sessions: make(map[string]*daemonSession),
	}
	ctx, stop := signalNotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	shutdown := make(chan error, 1)
	go func() {
		<-ctx.Done()
		drainCtx, cancel := context.WithTimeout(context.Background(), daemonRequestTimeout)
		state.drainMutations(drainCtx)
		cancel()
		err := state.closeSessions()
		_ = listener.Close()
		shutdown <- err
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return <-shutdown
			default:
			}
			stop()
			return errors.Join(model.Wrap(model.ExitEnv, "accept daemon connection", err), <-shutdown)
		}
		go state.serveConn(conn)
	}
}

func signalNotifyContext(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, signals...)
}

func (s *daemonState) serveConn(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(daemonRequestTimeout))
	var req daemonRequest
	if err := json.NewDecoder(io.LimitReader(conn, 64<<10)).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(daemonResponse{Error: err.Error(), Code: int(model.ExitEnv)})
		return
	}
	deadline := time.Now().Add(daemonRequestTimeout)
	if req.DeadlineNanos > 0 && time.Unix(0, req.DeadlineNanos).Before(deadline) {
		deadline = time.Unix(0, req.DeadlineNanos)
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	// The request occupies one connection. EOF means the caller has gone away.
	go func() { var b [1]byte; _, _ = conn.Read(b[:]); cancel() }()
	resp := s.handleRequestContext(ctx, req)
	_ = json.NewEncoder(conn).Encode(resp)
}

func (s *daemonState) handleRequest(req daemonRequest) daemonResponse {
	ctx, cancel := context.WithTimeout(context.Background(), daemonRequestTimeout)
	defer cancel()
	return s.handleRequestContext(ctx, req)
}
func (s *daemonState) handleRequestContext(ctx context.Context, req daemonRequest) daemonResponse {
	if req.RepoRoot != "" && req.RepoRoot != s.repoRoot {
		return daemonResponse{Error: fmt.Sprintf("repo root mismatch: %s", req.RepoRoot), Code: int(model.ExitEnv)}
	}
	if req.GitDir != "" && req.GitDir != s.gitDir {
		return daemonResponse{Error: fmt.Sprintf("git dir mismatch: %s", req.GitDir), Code: int(model.ExitEnv)}
	}
	key := strings.TrimSpace(req.SessionKey)
	switch req.Method {
	case "ping":
		return daemonResponse{Version: daemonProtocol, RepoRoot: s.repoRoot, GitDir: s.gitDir}
	case "recover":
		info, err := s.recoverSession(ctx, key, req.DryRun)
		return daemonResponseForInfo(info, err)
	case "ensure":
		info, err := s.ensureSession(ctx, key, req.Config)
		return daemonResponseForInfo(info, err)
	case "list":
		sessions, err := s.listSessions(ctx)
		resp := daemonResponseForInfo(nil, err)
		if err == nil {
			resp.Sessions = sessions
		}
		return resp
	case "acquire_exec":
		if req.OwnerPID <= 0 || processStartTime(req.OwnerPID) == "" {
			return daemonResponseForInfo(nil, fmt.Errorf("mutation owner must identify a live process"))
		}
		info, leaseID, err := s.acquireExec(ctx, key, req.OwnerPID, req.Config)
		resp := daemonResponseForInfo(info, err)
		if err == nil {
			resp.LeaseID = leaseID
		}
		return resp
	case "release_exec":
		err := s.releaseExecGeneration(ctx, key, req.LeaseID, req.ViewGeneration)
		return daemonResponseForInfo(nil, err)
	case "flush":
		err := s.flushSession(ctx, key, time.Duration(req.MinAgeNanos))
		return daemonResponseForInfo(nil, err)
	case "mark_dirty":
		err := s.markDirtySession(ctx, key)
		return daemonResponseForInfo(nil, err)
	case "prune":
		pruned, err := s.pruneSessions(ctx, daemonIdleDuration(req))
		resp := daemonResponseForInfo(nil, err)
		if err == nil {
			resp.Pruned = pruned
		}
		return resp
	case "end":
		ended, err := s.endSession(ctx, key)
		resp := daemonResponseForInfo(nil, err)
		if err == nil {
			resp.Ended = ended
		}
		return resp
	default:
		return daemonResponse{Error: fmt.Sprintf("unknown daemon method: %s", req.Method), Code: int(model.ExitEnv)}
	}
}

func daemonResponseForInfo(info *daemonSessionInfo, err error) daemonResponse {
	if err == nil {
		return daemonResponse{Session: info}
	}
	resp := daemonResponse{Error: err.Error()}
	var personaErr *model.PersonaError
	if errors.As(err, &personaErr) {
		resp.Code = int(personaErr.Code)
	}
	return resp
}

func daemonSessionSnapshot(sess *daemonSession) daemonSessionInfo {
	info := sess.info
	info.Dirty = sess.dirty
	info.ExternalRoot = filepath.Dir(sess.menv.tempRoot)
	info.BaseOID = sess.baseOID
	info.Generation = sess.generation
	info.LastDurableGeneration = sess.lastDurableGeneration
	info.ViewGeneration = sess.viewGeneration
	info.LastError = sess.lastError
	info.State = "ready"
	if sess.dirty {
		info.State = "dirty"
	}
	if sess.busy {
		info.State = "mutating"
	}
	if sess.lastError != "" {
		info.State = "recovery_required"
		info.RecoveryRoot = sess.sess.Root
	}
	if sess.closed {
		info.State = "cleanup_required"
		info.RecoveryRoot = sess.sess.Root
	}
	info.Busy = sess.busy
	info.BusyOwnerPID = sess.busyOwnerPID
	if !sess.createdAt.IsZero() {
		info.CreatedUnix = sess.createdAt.Unix()
		info.CreatedRFC3339 = sess.createdAt.UTC().Format(time.RFC3339)
	}
	if !sess.lastUsed.IsZero() {
		info.LastUsedUnix = sess.lastUsed.Unix()
		info.LastUsedRFC3339 = sess.lastUsed.UTC().Format(time.RFC3339)
	}
	if !sess.lastFlushed.IsZero() {
		info.LastFlushedUnix = sess.lastFlushed.Unix()
		info.LastFlushedRFC3339 = sess.lastFlushed.UTC().Format(time.RFC3339)
	}
	info.FlushCount = sess.flushCount
	info.FlushSkipped = sess.flushSkipped
	info.RecoveredCount = sess.recoveredCount
	return info
}

func daemonSessionMetaSnapshot(sess *daemonSession) daemonSessionMeta {
	meta := daemonSessionMeta{
		SchemaVersion:         daemonMetadataSchema,
		SessionKey:            sess.key,
		Config:                sess.config,
		SessionRoot:           sess.sess.Root,
		FlushCount:            sess.flushCount,
		FlushSkipped:          sess.flushSkipped,
		RecoveredCount:        sess.recoveredCount,
		Dirty:                 sess.dirty,
		BaseOID:               sess.baseOID,
		Generation:            sess.generation,
		LastDurableGeneration: sess.lastDurableGeneration,
		ViewGeneration:        sess.viewGeneration,
		LastError:             sess.lastError,
		BusyOwnerPID:          sess.busyOwnerPID,
		BusyOwnerStart:        sess.busyOwnerStart,
		InitialIgnored:        sess.initialIgnored,
		ExternalRoot:          filepath.Dir(sess.menv.tempRoot),
		PatchChecksum:         sess.patchChecksum,
	}
	if !sess.createdAt.IsZero() {
		meta.CreatedUnix = sess.createdAt.Unix()
	}
	if !sess.lastUsed.IsZero() {
		meta.LastUsedUnix = sess.lastUsed.Unix()
	}
	if !sess.lastFlushed.IsZero() {
		meta.LastFlushedUnix = sess.lastFlushed.Unix()
	}
	return meta
}

func daemonTimeFromUnix(unixSeconds int64) time.Time {
	if unixSeconds <= 0 {
		return time.Time{}
	}
	return time.Unix(unixSeconds, 0).UTC()
}

func writeDaemonSessionMeta(path string, meta daemonSessionMeta) error {
	meta.SchemaVersion = daemonMetadataSchema
	if err := validateDaemonSessionMeta(meta); err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > daemonMetadataLimit {
		return fmt.Errorf("session metadata exceeds size limit of %d bytes", daemonMetadataLimit)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".session-meta-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func readDaemonSessionMeta(path string) (daemonSessionMeta, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return daemonSessionMeta{}, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return daemonSessionMeta{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > daemonMetadataLimit {
		return daemonSessionMeta{}, fmt.Errorf("invalid session metadata file")
	}
	data, err := io.ReadAll(io.LimitReader(file, daemonMetadataLimit+1))
	if err != nil {
		return daemonSessionMeta{}, err
	}
	if len(data) > daemonMetadataLimit {
		return daemonSessionMeta{}, fmt.Errorf("session metadata too large")
	}
	var meta daemonSessionMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return daemonSessionMeta{}, err
	}
	if err := validateDaemonSessionMeta(meta); err != nil {
		return daemonSessionMeta{}, err
	}
	return meta, nil
}

func (s *daemonState) ensureSession(ctx context.Context, sessionKey string, cfg daemonSessionConfig) (*daemonSessionInfo, error) {
	unlock, err := s.lockSession(ctx, sessionKey)
	if err != nil {
		return nil, err
	}
	defer unlock()
	sess, err := s.ensureSessionLocked(ctx, sessionKey, cfg)
	if err != nil {
		return nil, err
	}
	info := daemonSessionSnapshot(sess)
	return &info, nil
}

func (s *daemonState) acquireExec(ctx context.Context, sessionKey string, ownerPID int, cfg daemonSessionConfig) (*daemonSessionInfo, string, error) {
	unlock, err := s.lockSession(ctx, sessionKey)
	if err != nil {
		return nil, "", err
	}
	defer unlock()
	sess, err := s.ensureSessionLocked(ctx, sessionKey, cfg)
	if err != nil {
		return nil, "", err
	}
	if err := s.recoverBusyLocked(ctx, sess); err != nil {
		return nil, "", err
	}
	if sess.busy {
		return nil, "", model.Wrap(model.ExitEnv, "daemon session busy", fmt.Errorf("session %q is already executing in pid %d", sessionKey, sess.busyOwnerPID))
	}
	masks, initialIgnored, err := maskIgnoredFiles(ctx, sess.g, sess.info.ViewPath, sess.menv.gitDirForOps, sess.menv.emptyFile, sess.menv.emptyDir, sess.config.options(), sess.mount, s.log)
	if err != nil {
		return nil, "", s.abortAcquire(sess, masks, model.Wrap(model.ExitEnv, "mask ignored files", err))
	}
	leaseID, err := daemonNonce()
	if err != nil {
		return nil, "", s.abortAcquire(sess, masks, model.Wrap(model.ExitEnv, "create daemon lease", err))
	}
	old := daemonSessionMetaSnapshot(sess)
	sess.ignoredMasks = masks
	if !sess.dirty {
		sess.initialIgnored = initialIgnored
	}
	sess.busy = true
	sess.busyOwnerPID = ownerPID
	sess.busyOwnerStart = processStartTime(ownerPID)
	sess.busyLease = leaseID
	sess.generation++
	sess.lastUsed = s.deps.now()
	sess.dirty = true
	if err := s.persistSessionLocked(sess); err != nil {
		unmaskErr := umountPathsReverse(sess.mount, masks)
		if unmaskErr == nil {
			sess.ignoredMasks = nil
			sess.busy = false
			sess.busyOwnerPID = 0
			sess.busyOwnerStart = ""
			sess.busyLease = ""
			sess.dirty = old.Dirty
			sess.generation = old.Generation
			sess.initialIgnored = old.InitialIgnored
		} else {
			sess.lastError = unmaskErr.Error()
		}
		s.cacheSession(sess)
		return nil, "", errors.Join(err, unmaskErr)
	}
	info := daemonSessionSnapshot(sess)
	return &info, leaseID, nil
}

func (s *daemonState) listSessions(ctx context.Context) ([]daemonSessionInfo, error) {
	paths, err := daemonSessionMetaPaths(s.gitDir)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, key := range s.sessionKeys() {
		keys[key] = true
	}
	var result []daemonSessionInfo
	for _, path := range paths {
		meta, err := readDaemonSessionMeta(path)
		if err != nil {
			result = append(result, daemonSessionInfo{SessionKey: filepath.Base(path), State: "recovery_required", LastError: err.Error()})
			continue
		}
		if daemonSessionMetaPath(s.gitDir, meta.SessionKey) != path {
			result = append(result, daemonSessionInfo{SessionKey: filepath.Base(path), State: "recovery_required", LastError: "session metadata filename/key mismatch"})
			continue
		}
		keys[meta.SessionKey] = true
	}
	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	for _, key := range sorted {
		unlock, cached, err := s.trySession(ctx, key)
		if err != nil {
			return nil, err
		}
		if cached != nil {
			result = append(result, *cached)
			continue
		}
		sess, err := s.lookupSessionLocked(ctx, key)
		if err == nil && sess != nil {
			err = s.recoverBusyLocked(ctx, sess)
		}
		if err != nil {
			meta, _ := readDaemonSessionMeta(daemonSessionMetaPath(s.gitDir, key))
			result = append(result, daemonSessionInfo{SessionKey: key, RepoRoot: s.repoRoot, GitDir: s.gitDir, PatchPath: daemonSessionPatchPath(s.gitDir, key), RecoveryRoot: meta.SessionRoot, ExternalRoot: meta.ExternalRoot, Dirty: meta.Dirty, State: "recovery_required", LastError: err.Error(), BaseOID: meta.BaseOID})
		} else if sess != nil {
			result = append(result, daemonSessionSnapshot(sess))
		}
		unlock()
	}
	sort.Slice(result, func(i, j int) bool { return result[i].SessionKey < result[j].SessionKey })
	return result, nil
}

func (s *daemonState) releaseExec(ctx context.Context, sessionKey, leaseID string) error {
	return s.releaseExecGeneration(ctx, sessionKey, leaseID, 0)
}
func (s *daemonState) releaseExecGeneration(ctx context.Context, sessionKey, leaseID string, generation uint64) error {
	unlock, err := s.lockSessionDuringDrain(ctx, sessionKey, true)
	if err != nil {
		return err
	}
	defer unlock()
	sess := s.sessionByKey(sessionKey)
	if sess == nil {
		return model.Wrap(model.ExitEnv, "release daemon session", fmt.Errorf("session %q not found", sessionKey))
	}
	if sess.closed {
		return fmt.Errorf("session %q requires cleanup; retry daemon end", sessionKey)
	}
	if generation != 0 && generation != sess.viewGeneration {
		return model.Wrap(model.ExitEnv, "release daemon session", fmt.Errorf("view generation mismatch"))
	}
	if !sess.busy || sess.busyLease != leaseID {
		return model.Wrap(model.ExitEnv, "release daemon session", fmt.Errorf("session %q lease mismatch", sessionKey))
	}
	return s.finishBusyLocked(ctx, sess)
}

func (s *daemonState) endSession(ctx context.Context, sessionKey string) (bool, error) {
	unlock, err := s.lockSession(ctx, sessionKey)
	if err != nil {
		return false, err
	}
	defer unlock()
	sess, err := s.lookupSessionLocked(ctx, sessionKey)
	if err != nil {
		return false, err
	}
	if sess == nil {
		return false, nil
	}
	if err := s.endSessionLocked(ctx, sessionKey, sess); err != nil {
		return false, err
	}
	return true, nil
}

func (s *daemonState) flushSession(ctx context.Context, sessionKey string, minAge time.Duration) error {
	unlock, err := s.lockSession(ctx, sessionKey)
	if err != nil {
		return err
	}
	defer unlock()
	sess, err := s.lookupSessionLocked(ctx, sessionKey)
	if err != nil {
		return err
	}
	if sess == nil {
		return nil
	}
	return s.flushSessionLocked(ctx, sessionKey, sess, "flush daemon session", minAge)
}

func (s *daemonState) markDirtySession(ctx context.Context, sessionKey string) error {
	unlock, err := s.lockSession(ctx, sessionKey)
	if err != nil {
		return err
	}
	defer unlock()
	sess, err := s.lookupSessionLocked(ctx, sessionKey)
	if err != nil {
		return err
	}
	if sess == nil {
		return model.Wrap(model.ExitEnv, "mark dirty daemon session", fmt.Errorf("session %q not found", sessionKey))
	}
	if sess.closed {
		return fmt.Errorf("session %q requires cleanup; retry daemon end", sessionKey)
	}
	if sess.busy {
		return model.Wrap(model.ExitEnv, "mark dirty daemon session", fmt.Errorf("session %q is still busy", sessionKey))
	}
	sess.dirty = true
	sess.generation++
	sess.lastUsed = s.deps.now()
	return s.persistSessionLocked(sess)
}

func (s *daemonState) pruneSessions(ctx context.Context, idleFor time.Duration) ([]string, error) {
	paths, err := daemonSessionMetaPaths(s.gitDir)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, key := range s.sessionKeys() {
		keys[key] = true
	}
	for _, path := range paths {
		meta, err := readDaemonSessionMeta(path)
		if err == nil && daemonSessionMetaPath(s.gitDir, meta.SessionKey) == path {
			keys[meta.SessionKey] = true
		}
	}
	sorted := make([]string, 0, len(keys))
	for key := range keys {
		sorted = append(sorted, key)
	}
	sort.Strings(sorted)
	now := s.deps.now()
	pruned := []string{}
	for _, key := range sorted {
		unlock, err := s.lockSession(ctx, key)
		if err != nil {
			return pruned, err
		}
		sess, err := s.lookupSessionLocked(ctx, key)
		if err != nil {
			unlock()
			continue
		} // Preserve a broken session; continue managing the others.
		if sess != nil {
			lastUsed := sess.lastUsed
			err = s.recoverBusyLocked(ctx, sess)
			if err == nil && !sess.busy && now.Sub(lastUsed) >= idleFor {
				err = s.endSessionLocked(ctx, key, sess)
				if err == nil {
					pruned = append(pruned, key)
				}
			}
		}
		unlock()
		if err != nil {
			return pruned, err
		}
	}
	return pruned, nil
}

func (s *daemonState) closeAll() {
	_ = s.closeSessions()
}

func (s *daemonState) closeSessions() error {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	var errs []error
	for _, key := range s.operationKeys() {
		s.mu.Lock()
		gate := s.sessionGateLocked(key)
		s.mu.Unlock()
		gate <- struct{}{}
		sess := s.sessionByKey(key)
		if sess == nil {
			<-gate
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), daemonRequestTimeout)
		var err error
		if sess.closed {
			// A prior end has already saved and may have closed the store. Retry
			// just its remaining cleanup actions, never export through closed FDs.
		} else if sess.busy && processIdentityAlive(sess.busyOwnerPID, sess.busyOwnerStart) {
			err = fmt.Errorf("recovery_required: mutation owner %d still alive during shutdown", sess.busyOwnerPID)
		} else if sess.busy {
			err = s.finishBusyLocked(ctx, sess)
		} else if sess.dirty {
			err = s.flushSessionLocked(ctx, key, sess, "shutdown flush", 0)
		}
		if err == nil {
			err = sess.cleanup.Run()
		}
		if err != nil {
			// Keep upper, worktree, mounts, and metadata. In particular never run a
			// recursive cleanup after an unsuccessful export or a live external write.
			sess.lastError = err.Error()
			if sess.preserve != nil {
				*sess.preserve = true
			}
			err = errors.Join(err, s.persistSessionLocked(sess))
			_ = sess.lock.Unlock()
			_ = sess.store.Close()
			s.log.Error("session preserved for recovery", "session", key, "root", sess.sess.Root, "error", err)
			errs = append(errs, fmt.Errorf("session %q preserved at %s: %w", key, sess.sess.Root, err))
		}
		cancel()
		sess.closed = true
		s.removeSession(key)
		<-gate
	}
	return errors.Join(errs...)
}

func (s *daemonState) ensureSessionLocked(ctx context.Context, sessionKey string, cfg daemonSessionConfig) (*daemonSession, error) {
	if sessionKey == "" || len(sessionKey) > 4096 {
		return nil, model.Wrap(model.ExitEnv, "daemon session key", fmt.Errorf("session-key must contain 1 to 4096 bytes"))
	}
	if existing := s.sessionByKey(sessionKey); existing != nil {
		if existing.closed {
			return nil, fmt.Errorf("session %q requires cleanup; retry daemon end", sessionKey)
		}
		if !existing.config.equal(cfg) {
			return nil, model.Wrap(model.ExitEnv, "daemon session options", fmt.Errorf("session %q already exists with different options; end it before changing base/apply/ignored settings", sessionKey))
		}
		existing.lastUsed = s.deps.now()
		if err := s.persistSessionLocked(existing); err != nil {
			return nil, err
		}
		return existing, nil
	}
	if restored, err := s.restoreSessionByKeyLocked(ctx, sessionKey, cfg); err != nil {
		return nil, err
	} else if restored != nil {
		return restored, nil
	}
	sess, err := s.createSessionLocked(ctx, sessionKey, cfg)
	if err != nil {
		return nil, err
	}
	s.putSession(sessionKey, sess)
	return sess, nil
}

func (s *daemonState) lookupSessionLocked(ctx context.Context, sessionKey string) (*daemonSession, error) {
	if existing := s.sessionByKey(sessionKey); existing != nil {
		return existing, nil
	}
	metaPath := daemonSessionMetaPath(s.gitDir, sessionKey)
	meta, err := readDaemonSessionMeta(metaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, model.Wrap(model.ExitEnv, "read daemon session metadata", err)
	}
	if meta.SessionKey != sessionKey {
		return nil, fmt.Errorf("session metadata key mismatch")
	}
	return s.restoreSessionLocked(ctx, meta, false)
}

func (s *daemonState) restoreSessionByKeyLocked(ctx context.Context, sessionKey string, cfg daemonSessionConfig) (*daemonSession, error) {
	metaPath := daemonSessionMetaPath(s.gitDir, sessionKey)
	meta, err := readDaemonSessionMeta(metaPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, model.Wrap(model.ExitEnv, "read daemon session metadata", err)
	}
	if meta.SessionKey != sessionKey {
		return nil, fmt.Errorf("session metadata key mismatch")
	}
	if !meta.Config.equal(cfg) {
		return nil, model.Wrap(model.ExitEnv, "daemon session options", fmt.Errorf("session %q already exists with different options; end it before changing base/apply/ignored settings", sessionKey))
	}
	return s.restoreSessionLocked(ctx, meta, true)
}

func (s *daemonState) restoreSessionLocked(ctx context.Context, meta daemonSessionMeta, bumpLastUsed bool) (*daemonSession, error) {
	return s.restoreSessionWithCheckpoint(ctx, meta, bumpLastUsed, nil)
}

func (s *daemonState) restoreSessionWithCheckpoint(ctx context.Context, meta daemonSessionMeta, bumpLastUsed bool, checkpoint *daemonCheckpoint) (*daemonSession, error) {
	if existing := s.sessionByKey(meta.SessionKey); existing != nil {
		return existing, nil
	}
	if meta.SessionKey == "" {
		return nil, model.Wrap(model.ExitEnv, "restore daemon session", fmt.Errorf("session metadata is missing session_key"))
	}
	// Never delete a path supplied by recovery metadata. Dirty upper layers and
	// their immutable worktree are reopened in place only after ownership checks.
	if meta.Dirty && processIdentityAlive(meta.BusyOwnerPID, meta.BusyOwnerStart) {
		return nil, model.Wrap(model.ExitEnv, "restore daemon session", fmt.Errorf("recovery_required: previous mutation owner %d is still alive; preserved %s", meta.BusyOwnerPID, meta.SessionRoot))
	}
	if !meta.Dirty && meta.SessionRoot != "" {
		if _, err := os.Lstat(meta.SessionRoot); err == nil {
			return nil, fmt.Errorf("recovery_required: stale root retained; run daemon recover --session-key %s: %s", meta.SessionKey, meta.SessionRoot)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if meta.ExternalRoot != "" {
		if _, err := os.Lstat(meta.ExternalRoot); err == nil {
			return nil, fmt.Errorf("recovery_required: stale external mounts/backing retained: %s", meta.ExternalRoot)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	sess, err := s.createSessionWithCheckpoint(ctx, meta.SessionKey, meta.Config, &meta, checkpoint)
	if err != nil {
		return nil, err
	}
	sess.createdAt = daemonTimeFromUnix(meta.CreatedUnix)
	if sess.createdAt.IsZero() {
		sess.createdAt = s.deps.now()
	}
	sess.lastUsed = daemonTimeFromUnix(meta.LastUsedUnix)
	if sess.lastUsed.IsZero() {
		sess.lastUsed = sess.createdAt
	}
	if bumpLastUsed {
		sess.lastUsed = s.deps.now()
	}
	sess.lastFlushed = daemonTimeFromUnix(meta.LastFlushedUnix)
	sess.flushCount = meta.FlushCount
	sess.flushSkipped = meta.FlushSkipped
	sess.recoveredCount = meta.RecoveredCount + 1
	sess.generation = meta.Generation
	sess.lastDurableGeneration = meta.LastDurableGeneration
	sess.dirty = meta.Dirty
	sess.lastError = meta.LastError
	sess.viewGeneration = meta.ViewGeneration + 1
	if sess.viewGeneration == 0 {
		sess.viewGeneration = 1
	}
	s.putSession(meta.SessionKey, sess)
	if err := s.persistSessionLocked(sess); err != nil {
		// The registry now owns the live view and checkpoint descriptors, even
		// when publishing its metadata fails. Preserve them for retry/shutdown.
		return sess, err
	}
	return sess, nil
}

func (s *daemonState) persistSessionLocked(sess *daemonSession) error {
	s.cacheSession(sess)
	write := s.deps.writeMeta
	if write == nil {
		write = writeDaemonSessionMeta
	}
	if err := write(sess.metaPath, daemonSessionMetaSnapshot(sess)); err != nil {
		sess.lastError = err.Error()
		s.cacheSession(sess)
		return model.Wrap(model.ExitWrite, "write daemon session metadata", err)
	}
	return nil
}

func (s *daemonState) createSessionLocked(ctx context.Context, sessionKey string, cfg daemonSessionConfig) (_ *daemonSession, retErr error) {
	return s.createSessionFromMeta(ctx, sessionKey, cfg, nil)
}

func (s *daemonState) createSessionFromMeta(ctx context.Context, sessionKey string, cfg daemonSessionConfig, prior *daemonSessionMeta) (_ *daemonSession, retErr error) {
	return s.createSessionWithCheckpoint(ctx, sessionKey, cfg, prior, nil)
}

func (s *daemonState) createSessionWithCheckpoint(ctx context.Context, sessionKey string, cfg daemonSessionConfig, prior *daemonSessionMeta, checkpoint *daemonCheckpoint) (_ *daemonSession, retErr error) {
	patchPath := daemonSessionPatchPath(s.gitDir, sessionKey)
	metaPath := daemonSessionMetaPath(s.gitDir, sessionKey)
	var err error
	if checkpoint == nil {
		checkpoint, err = openDaemonCheckpoint(ctx, patchPath)
		if err != nil {
			return nil, model.Wrap(model.ExitWrite, "open patch checkpoint", err)
		}
	}
	patchStore, lock := checkpoint.store, checkpoint.lock
	cleanup := &cleanupStack{}
	cleanup.Push(checkpoint.close)
	defer func() {
		if retErr != nil {
			retErr = errors.Join(retErr, cleanup.Run())
		}
	}()
	if err := checkpoint.validate(prior); err != nil {
		return nil, err
	}

	var sess *session.Session
	reuse := prior != nil && prior.Dirty
	preserve := reuse
	if reuse {
		sess, err = session.Open(s.gitDir, prior.SessionRoot)
		if err != nil {
			return nil, model.Wrap(model.ExitEnv, "recovery_required: validate preserved session", err)
		}
		mounted, merr := session.HasMounts(sess.Root)
		if merr != nil {
			return nil, merr
		}
		if mounted {
			return nil, model.Wrap(model.ExitEnv, "recovery_required", fmt.Errorf("preserved session still has mounts; stop previous owners and unmount before recovery: %s", sess.Root))
		}
	} else {
		// A clean stale root is retained for inspection rather than traversed or
		// removed using metadata. Normal successful shutdown already removes it.
		sess, err = session.Create(s.gitDir)
		if err != nil {
			return nil, model.Wrap(model.ExitEnv, "create session", err)
		}
	}
	cleanup.Push(func() error {
		if preserve {
			return nil
		}
		return sess.RemoveAll()
	})

	repoReal := resolvePath(s.repoRoot)
	patchReal := resolvePath(patchPath)
	patchInRepo, patchRel := isSubpath(repoReal, patchReal)
	if patchInRepo {
		patchRel = filepath.ToSlash(patchRel)
	}
	g := s.deps.newGit(s.repoRoot, s.gitDir, false)
	if baseMode, ok := g.(interface{ SetBaseMode(model.BaseMode) }); ok {
		baseMode.SetBaseMode(cfg.BaseMode)
	}
	baseOpts := cfg.options()
	baseOpts.KeepSession = model.KeepAlways
	pinned := ""
	if prior != nil {
		pinned = prior.BaseOID
		if pinned == "" {
			if _, ok := g.(interface{ SetBaseOID(string) }); ok {
				return nil, model.Wrap(model.ExitRepo, "recover base", fmt.Errorf("legacy session has no immutable base OID; preserve and inspect %s", prior.SessionRoot))
			}
		}
	} else {
		pinned, err = s.loadPatchBase(sessionKey, cfg)
		if err != nil {
			return nil, err
		}
	}
	if pinned != "" {
		if cfg.BaseMode == model.BaseRepo {
			probe := s.deps.newGit(s.repoRoot, s.gitDir, false)
			if resolver, ok := probe.(interface {
				ResolveBase(context.Context, string) (string, error)
			}); ok {
				current, err := resolver.ResolveBase(ctx, "HEAD")
				if err != nil {
					return nil, err
				}
				if current != pinned {
					return nil, fmt.Errorf("repo-mode lower HEAD differs from saved base; preserve the patch and use a matching worktree base")
				}
			}
		}
		if real, ok := g.(interface{ SetBaseOID(string) }); ok {
			real.SetBaseOID(pinned)
		}
	}

	var basePath string
	if reuse {
		if err := validateDaemonOwner(sess.Root, sessionKey, prior.BaseOID); err != nil {
			return nil, err
		}
		if cfg.BaseMode == model.BaseWorktree {
			basePath = sess.BaseWT
			if _, realGit := g.(*gitx.Git); realGit {
				if err := validateRecoveryBase(ctx, basePath, prior.BaseOID); err != nil {
					return nil, model.Wrap(model.ExitRepo, "recovery_required: validate preserved base", err)
				}
			}
		} else {
			return nil, model.Wrap(model.ExitRepo, "recovery_required", fmt.Errorf("dirty repo-mode lower layer cannot be verified; preserved %s", sess.Root))
		}
	} else {
		basePath, err = prepareBase(ctx, g, baseOpts, sess, patchInRepo, patchRel, cleanup, &retErr)
		if err != nil {
			return nil, err
		}
	}
	if cfg.BaseMode == model.BaseWorktree {
		cleanup.Push(func() error {
			if preserve {
				return nil
			}
			mounted, err := session.HasMounts(sess.Root)
			if err != nil {
				return err
			}
			if mounted {
				return fmt.Errorf("refuse to remove mounted session worktree %s", sess.BaseWT)
			}
			return g.WorktreeRemoveForce(context.Background(), sess.BaseWT)
		})
	}

	if !reuse {
		oid := ""
		if pinned, ok := g.(interface{ PinnedBaseOID() string }); ok {
			oid = pinned.PinnedBaseOID()
		}
		if err := writeDaemonOwner(sess.Root, sessionKey, oid); err != nil {
			return nil, err
		}
	}
	mount := s.deps.newMount()
	menv, err := setupMountTargetEnv(sess.View, s.gitDir, basePath, sess, mount, cleanup, s.log)
	if err != nil {
		return nil, err
	}
	externalRoot := filepath.Dir(menv.tempRoot)
	oid := ""
	if pinned, ok := g.(interface{ PinnedBaseOID() string }); ok {
		oid = pinned.PinnedBaseOID()
	}
	if err := writeDaemonOwner(externalRoot, sessionKey, oid); err != nil {
		return nil, err
	}
	maskTargets := make([]string, 0, 4)
	cleanup.Push(func() error { return umountPathsReverse(mount, maskTargets) })
	commandMaskPaths := patchStatePathsForRepo(sess.View, patchInRepo && patchRel != "" && !strings.HasPrefix(patchRel, ".git/") && patchRel != ".git", patchRel).abs
	if !reuse {
		if err := applyPatchStore(ctx, g, cfg.ApplyMode, patchStore, sess.View, menv.gitDirForOps, sess.Root, s.log); err != nil {
			return nil, model.Wrap(model.ExitApply, "apply patch", err)
		}
	}
	initialIgnored, err := listIgnoredCandidatesChecked(ctx, g, sess.View, menv.gitDirForOps, cfg.IgnoredMax)
	if cfg.IgnoredMax == 0 {
		initialIgnored = nil
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if reuse {
		initialIgnored = prior.InitialIgnored
	}
	for _, patchMaskPath := range commandMaskPaths {
		if err := maskPathWithBacking(mount, patchMaskPath, model.MaskFile, menv.emptyFile, menv.emptyDir); err != nil {
			return nil, model.Wrap(model.ExitEnv, "mask patch file", err)
		}
		maskTargets = append(maskTargets, patchMaskPath)
	}
	gitPath := filepath.Join(sess.View, ".git")
	if info, err := os.Lstat(gitPath); err == nil {
		kind := model.MaskDir
		if !info.IsDir() {
			kind = model.MaskFile
		}
		if err := maskPathWithBacking(mount, gitPath, kind, menv.emptyFile, menv.emptyDir); err != nil {
			return nil, model.Wrap(model.ExitEnv, "mask .git", err)
		}
		maskTargets = append(maskTargets, gitPath)
	}
	info := daemonSessionInfo{
		SessionKey: sessionKey,
		RepoRoot:   s.repoRoot,
		GitDir:     s.gitDir,
		ViewPath:   sess.View,
		PatchPath:  patchPath,
	}
	createdAt := s.deps.now()
	daemonSess := &daemonSession{
		key:            sessionKey,
		config:         cfg,
		info:           info,
		metaPath:       metaPath,
		store:          patchStore,
		lock:           lock,
		sess:           sess,
		cleanup:        cleanup,
		g:              g,
		mount:          mount,
		menv:           menv,
		patchInRepo:    patchInRepo,
		patchRel:       patchRel,
		createdAt:      createdAt,
		lastUsed:       createdAt,
		initialIgnored: initialIgnored,
		patchChecksum:  checkpoint.checksum,
		viewGeneration: 1,
		preserve:       &preserve,
	}
	if real, ok := g.(interface{ PinnedBaseOID() string }); ok {
		daemonSess.baseOID = real.PinnedBaseOID()
	}
	if err := s.retainPatchBase(ctx, g, sessionKey, cfg, daemonSess.baseOID); err != nil {
		return nil, err
	}
	if prior == nil {
		if err := s.persistSessionLocked(daemonSess); err != nil {
			return nil, err
		}
	}
	preserve = false
	return daemonSess, nil
}

func (s *daemonState) recoverBusyLocked(ctx context.Context, sess *daemonSession) error {
	if !sess.busy {
		return nil
	}
	if processIdentityAlive(sess.busyOwnerPID, sess.busyOwnerStart) {
		return nil
	}
	return s.finishBusyLocked(ctx, sess)
}

func (s *daemonState) flushSessionLocked(ctx context.Context, sessionKey string, sess *daemonSession, op string, minAge time.Duration) error {
	if sess.closed {
		return fmt.Errorf("session %q requires cleanup; retry daemon end", sessionKey)
	}
	if err := s.recoverBusyLocked(ctx, sess); err != nil {
		return err
	}
	if sess.busy {
		return model.Wrap(model.ExitEnv, op, fmt.Errorf("session %q is still busy in pid %d", sessionKey, sess.busyOwnerPID))
	}
	now := s.deps.now()
	if minAge > 0 && !sess.lastFlushed.IsZero() && now.Sub(sess.lastFlushed) < minAge {
		sess.flushSkipped++
		if err := s.persistSessionLocked(sess); err != nil {
			return err
		}
		return nil
	}
	// Publish the in-progress save before replacing the patch. A crash after
	// patch rename but before the final metadata write must retain the upper.
	sess.dirty = true
	if err := s.persistSessionLocked(sess); err != nil {
		return err
	}
	if err := s.writeSessionPatch(ctx, sess, sess.initialIgnored); err != nil {
		sess.dirty = true
		sess.lastError = err.Error()
		_ = s.persistSessionLocked(sess)
		return err
	}
	sess.lastUsed = now
	sess.lastFlushed = now
	sess.flushCount++
	sess.dirty = false
	sess.lastError = ""
	sess.lastDurableGeneration = sess.generation
	if err := s.persistSessionLocked(sess); err != nil {
		sess.dirty = true
		sess.lastError = err.Error()
		s.cacheSession(sess)
		return err
	}
	return nil
}

func (s *daemonState) endSessionLocked(ctx context.Context, sessionKey string, sess *daemonSession) error {
	if !sess.closed {
		if err := s.flushSessionLocked(ctx, sessionKey, sess, "end daemon session", 0); err != nil {
			return err
		}
		sess.closed = true
	}
	if err := sess.cleanup.Run(); err != nil {
		sess.lastError = err.Error()
		_ = s.persistSessionLocked(sess)
		return model.Wrap(model.ExitEnv, "cleanup daemon session", err)
	}
	if err := os.Remove(sess.metaPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return model.Wrap(model.ExitWrite, "remove daemon session metadata", err)
	}
	if err := syncDaemonDirectory(filepath.Dir(sess.metaPath)); err != nil {
		return model.Wrap(model.ExitWrite, "sync removed daemon session metadata", err)
	}
	s.removeSession(sessionKey)
	return nil
}

func (s *daemonState) finishBusyLocked(ctx context.Context, sess *daemonSession) error {
	exportErr := s.writeSessionPatch(ctx, sess, sess.initialIgnored)
	if err := umountPathsReverse(sess.mount, sess.ignoredMasks); err != nil {
		sess.lastError = err.Error()
		sess.dirty = true
		_ = s.persistSessionLocked(sess)
		return errors.Join(exportErr, model.Wrap(model.ExitEnv, "unmask ignored files", err))
	}
	sess.ignoredMasks = nil
	sess.busy = false
	sess.busyOwnerPID = 0
	sess.busyOwnerStart = ""
	sess.busyLease = ""
	sess.lastUsed = s.deps.now()
	if exportErr == nil {
		sess.lastFlushed = sess.lastUsed
		sess.flushCount++
		sess.dirty = false
		sess.lastError = ""
		sess.lastDurableGeneration = sess.generation
	} else {
		sess.dirty = true
		sess.lastError = exportErr.Error()
	}
	if err := s.persistSessionLocked(sess); err != nil {
		sess.dirty = true
		sess.lastError = err.Error()
		s.cacheSession(sess)
		return errors.Join(exportErr, err)
	}
	return exportErr
}

func (s *daemonState) writeSessionPatch(ctx context.Context, sess *daemonSession, initialIgnored []string) error {
	exportFile, err := os.CreateTemp(sess.menv.tempRoot, "persona-daemon-export-*.patch")
	if err != nil {
		return model.Wrap(model.ExitExport, "create export temp", err)
	}
	defer closeAndRemoveTempFile(exportFile)
	hash := sha256.New()
	if _, err := exportPatchToWriter(ctx, sess.g, sess.info.ViewPath, sess.menv.gitDirForOps, sess.patchInRepo, sess.patchRel, sess.config.IgnoredMax, initialIgnored, io.MultiWriter(exportFile, hash)); err != nil {
		return model.Wrap(model.ExitExport, "export patch", err)
	}
	if _, err := exportFile.Seek(0, 0); err != nil {
		return model.Wrap(model.ExitWrite, "rewind export patch", err)
	}
	if err := sess.store.WriteFromReader(exportFile); err != nil {
		return model.Wrap(model.ExitWrite, "write patch", err)
	}
	sess.patchChecksum = hex.EncodeToString(hash.Sum(nil))
	return nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func daemonNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func daemonSessionPatchPath(gitDir, sessionKey string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(sessionKey)))
	return filepath.Join(gitDir, "persona", "daemon", "patches", fmt.Sprintf("%s-%s.patch", daemonSafeName(sessionKey), hex.EncodeToString(digest[:8])))
}

func daemonSessionMetaPath(gitDir, sessionKey string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(sessionKey)))
	return filepath.Join(gitDir, "persona", "daemon", "state", fmt.Sprintf("%s-%s.json", daemonSafeName(sessionKey), hex.EncodeToString(digest[:8])))
}

func daemonSessionMetaPaths(gitDir string) ([]string, error) {
	stateDir := filepath.Join(gitDir, "persona", "daemon", "state")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !validDaemonMetaFilename(entry.Name()) {
			continue
		}
		paths = append(paths, filepath.Join(stateDir, entry.Name()))
	}
	sort.Strings(paths)
	return paths, nil
}

func daemonSafeName(raw string) string {
	var out strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		if out.Len() >= 48 {
			break
		}
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			out.WriteRune(r)
		} else {
			out.WriteByte('-')
		}
	}
	name := strings.Trim(out.String(), "-.")
	if name == "" {
		return "session"
	}
	return name
}
func daemonSocketPath(gitDir string) string {
	digest := sha256.Sum256([]byte(filepath.Clean(gitDir)))
	return filepath.Join("/tmp", fmt.Sprintf("persona-%d", os.Getuid()), hex.EncodeToString(digest[:16])+".sock")
}

func ensureDaemonDir(gitDir string) (string, error) {
	daemonDir := filepath.Join(gitDir, "persona", "daemon")
	for _, path := range []string{filepath.Join(gitDir, "persona"), daemonDir} {
		info, err := os.Lstat(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return "", err
			}
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("daemon parent is symlink: %s", path)
		}
	}
	for _, path := range []string{
		filepath.Join(daemonDir, "patches"),
		filepath.Join(daemonDir, "state"),
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return "", err
		}
	}
	return daemonDir, nil
}

type daemonClient struct {
	socketPath string
	repoRoot   string
	gitDir     string
}

func newDaemonClient(ctx context.Context, repoRoot, gitDir string) (*daemonClient, error) {
	socketPath, err := ensureDaemonRunning(ctx, repoRoot, gitDir)
	if err != nil {
		return nil, err
	}
	return &daemonClient{socketPath: socketPath, repoRoot: repoRoot, gitDir: gitDir}, nil
}

func ensureDaemonRunning(ctx context.Context, repoRoot, gitDir string) (string, error) {
	socketPath := daemonSocketPath(gitDir)
	if err := daemonPingRepo(ctx, socketPath, repoRoot, gitDir); err == nil {
		return socketPath, nil
	}
	daemonDir, err := ensureDaemonDir(gitDir)
	if err != nil {
		return "", model.Wrap(model.ExitEnv, "prepare daemon dir", err)
	}
	lockFile, err := os.OpenFile(filepath.Join(daemonDir, daemonLaunchLock), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return "", model.Wrap(model.ExitEnv, "open daemon launch lock", err)
	}
	defer lockFile.Close()
	if err := flockContext(ctx, lockFile); err != nil {
		return "", model.Wrap(model.ExitEnv, "lock daemon launch", err)
	}
	defer func() {
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	}()
	if err := daemonPingRepo(ctx, socketPath, repoRoot, gitDir); err == nil {
		return socketPath, nil
	}
	if err := spawnDaemonServer(repoRoot, gitDir, socketPath); err != nil {
		return "", err
	}
	if err := waitForDaemon(ctx, socketPath); err != nil {
		return "", err
	}
	return socketPath, nil
}

func spawnDaemonServer(repoRoot, gitDir, socketPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return model.Wrap(model.ExitEnv, "resolve executable", err)
	}
	cmd := exec.Command(exe, "daemon", "serve", "--repo-root", repoRoot, "--git-dir", gitDir, "--socket", socketPath)
	cmd.Stdin = nil
	logPath := filepath.Join(gitDir, "persona", "daemon", "daemon.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return model.Wrap(model.ExitEnv, "start daemon server", err)
	}
	return cmd.Process.Release()
}

func waitForDaemon(ctx context.Context, socketPath string) error {
	deadline := time.Now().Add(3 * time.Second)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	for {
		if err := daemonPing(ctx, socketPath); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return model.Wrap(model.ExitEnv, "wait for daemon server", fmt.Errorf("timed out waiting for %s", socketPath))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func daemonPing(ctx context.Context, socketPath string) error {
	ctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	c := &daemonClient{socketPath: socketPath}
	resp, err := c.call(ctx, daemonRequest{Method: "ping"})
	if err != nil {
		return err
	}
	if resp.Version != daemonProtocol {
		return fmt.Errorf("daemon protocol mismatch")
	}
	return nil
}

func (c *daemonClient) call(ctx context.Context, req daemonRequest) (*daemonResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, daemonRequestTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	req.DeadlineNanos = deadline.UnixNano()
	req.RepoRoot = c.repoRoot
	req.GitDir = c.gitDir
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, model.Wrap(model.ExitEnv, "connect daemon", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, model.Wrap(model.ExitEnv, "encode daemon request", err)
	}
	var resp daemonResponse
	if err := json.NewDecoder(io.LimitReader(conn, 4<<20)).Decode(&resp); err != nil {
		return nil, model.Wrap(model.ExitEnv, "decode daemon response", err)
	}
	if resp.Error != "" {
		if resp.Code != 0 {
			return nil, &model.PersonaError{Code: model.ExitCode(resp.Code), Op: req.Method, Err: errors.New(resp.Error)}
		}
		return nil, fmt.Errorf("%s: %s", req.Method, resp.Error)
	}
	return &resp, nil
}

func (c *daemonClient) ensure(ctx context.Context, sessionKey string, cfg daemonSessionConfig) (*daemonSessionInfo, error) {
	resp, err := c.call(ctx, daemonRequest{
		Method:     "ensure",
		SessionKey: sessionKey,
		Config:     cfg,
	})
	if err != nil {
		return nil, err
	}
	return resp.Session, nil
}

func (c *daemonClient) list(ctx context.Context) ([]daemonSessionInfo, error) {
	resp, err := c.call(ctx, daemonRequest{Method: "list"})
	if err != nil {
		return nil, err
	}
	return resp.Sessions, nil
}

func (c *daemonClient) acquireExec(ctx context.Context, sessionKey string, cfg daemonSessionConfig) (*daemonResponse, error) {
	return c.call(ctx, daemonRequest{
		Method:     "acquire_exec",
		SessionKey: sessionKey,
		OwnerPID:   os.Getpid(),
		Config:     cfg,
	})
}

func (c *daemonClient) releaseExec(ctx context.Context, sessionKey, leaseID string) error {
	_, err := c.call(ctx, daemonRequest{
		Method:     "release_exec",
		SessionKey: sessionKey,
		LeaseID:    leaseID,
	})
	return err
}

func (c *daemonClient) end(ctx context.Context, sessionKey string) error {
	_, err := c.call(ctx, daemonRequest{
		Method:     "end",
		SessionKey: sessionKey,
	})
	return err
}

func (c *daemonClient) flush(ctx context.Context, sessionKey string, minAge time.Duration) error {
	_, err := c.call(ctx, daemonRequest{
		Method:      "flush",
		SessionKey:  sessionKey,
		MinAgeNanos: int64(minAge),
	})
	return err
}

func (c *daemonClient) markDirty(ctx context.Context, sessionKey string) error {
	_, err := c.call(ctx, daemonRequest{
		Method:     "mark_dirty",
		SessionKey: sessionKey,
	})
	return err
}

func (c *daemonClient) prune(ctx context.Context, idleFor time.Duration) ([]string, error) {
	resp, err := c.call(ctx, daemonRequest{
		Method:         "prune",
		IdleForSeconds: int64(idleFor / time.Second),
		IdleForNanos:   int64(idleFor),
	})
	if err != nil {
		return nil, err
	}
	return resp.Pruned, nil
}
