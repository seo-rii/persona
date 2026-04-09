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
	daemonSocketFile = "persona.sock"
	daemonLaunchLock = "launch.lock"
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
	Method     string              `json:"method"`
	RepoRoot   string              `json:"repo_root,omitempty"`
	GitDir     string              `json:"git_dir,omitempty"`
	SessionKey string              `json:"session_key,omitempty"`
	LeaseID    string              `json:"lease_id,omitempty"`
	OwnerPID   int                 `json:"owner_pid,omitempty"`
	Config     daemonSessionConfig `json:"config"`
}

type daemonResponse struct {
	Error   string             `json:"error,omitempty"`
	Code    int                `json:"code,omitempty"`
	Session *daemonSessionInfo `json:"session,omitempty"`
	LeaseID string             `json:"lease_id,omitempty"`
	Ended   bool               `json:"ended,omitempty"`
}

type daemonSessionInfo struct {
	SessionKey string `json:"session_key"`
	RepoRoot   string `json:"repo_root"`
	GitDir     string `json:"git_dir"`
	ViewPath   string `json:"view_path"`
	PatchPath  string `json:"patch_path"`
}

type daemonDeps struct {
	newGit   func(string, string, bool) model.GitOps
	newMount func() model.NSOps
	now      func() time.Time
}

type daemonState struct {
	mu       sync.Mutex
	repoRoot string
	gitDir   string
	deps     daemonDeps
	log      *slog.Logger
	sessions map[string]*daemonSession
}

type daemonSession struct {
	key            string
	config         daemonSessionConfig
	info           daemonSessionInfo
	store          *patchio.PatchStore
	lock           *patchio.PatchLock
	sess           *session.Session
	cleanup        *cleanupStack
	g              model.GitOps
	mount          model.NSOps
	menv           *mountEnv
	patchInRepo    bool
	patchRel       string
	ignoredMasks   []string
	initialIgnored []string
	busy           bool
	busyOwnerPID   int
	busyLease      string
	lastUsed       time.Time
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
			fmt.Fprintf(cmd.OutOrStdout(), "session_key=%s\nrepo_root=%s\ngit_dir=%s\nview_path=%s\npatch_path=%s\n",
				info.SessionKey, info.RepoRoot, info.GitDir, info.ViewPath, info.PatchPath)
			return nil
		},
	}
	bindDaemonSessionFlags(infoCmd, &infoFlags)
	infoCmd.Flags().BoolVar(&infoJSON, "json", false, "print daemon session info as JSON")

	var flushSessionKey string
	flushCmd := &cobra.Command{
		Use:           "flush --session-key <key>",
		Short:         "Write the current daemon view back into its patch file",
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonFlush(cmd.Context(), strings.TrimSpace(flushSessionKey))
		},
	}
	flushCmd.Flags().StringVar(&flushSessionKey, "session-key", "", "external session key, such as a Claude chat session id")
	_ = flushCmd.MarkFlagRequired("session-key")

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

	daemonCmd.AddCommand(execCmd, infoCmd, flushCmd, endCmd, serveCmd)
	root.AddCommand(daemonCmd)
}

func bindDaemonSessionFlags(cmd *cobra.Command, values *daemonFlagValues) {
	cmd.Flags().StringVar(&values.sessionKey, "session-key", "", "external session key, such as a Claude chat session id")
	cmd.Flags().StringVar(&values.baseMode, "base-mode", string(model.BaseRepo), "base mode: repo | worktree")
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
	childCode := runCommand(resp.Session.ViewPath, cwdRel, args)
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

func runDaemonFlush(ctx context.Context, sessionKey string) error {
	repoRoot, gitDir, _, err := daemonRepoContext(ctx)
	if err != nil {
		return err
	}
	client, err := newDaemonClient(ctx, repoRoot, gitDir)
	if err != nil {
		return err
	}
	return client.flush(ctx, sessionKey)
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
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return model.Wrap(model.ExitEnv, "remove stale daemon socket", err)
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
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				state.closeAll()
				return nil
			default:
			}
			return model.Wrap(model.ExitEnv, "accept daemon connection", err)
		}
		go state.serveConn(conn)
	}
}

func signalNotifyContext(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(parent, signals...)
}

func (s *daemonState) serveConn(conn net.Conn) {
	defer conn.Close()
	var req daemonRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(daemonResponse{Error: err.Error(), Code: int(model.ExitEnv)})
		return
	}
	resp := s.handleRequest(req)
	_ = json.NewEncoder(conn).Encode(resp)
}

func (s *daemonState) handleRequest(req daemonRequest) daemonResponse {
	if req.RepoRoot != "" && req.RepoRoot != s.repoRoot {
		return daemonResponse{Error: fmt.Sprintf("repo root mismatch: %s", req.RepoRoot), Code: int(model.ExitEnv)}
	}
	if req.GitDir != "" && req.GitDir != s.gitDir {
		return daemonResponse{Error: fmt.Sprintf("git dir mismatch: %s", req.GitDir), Code: int(model.ExitEnv)}
	}
	key := strings.TrimSpace(req.SessionKey)
	switch req.Method {
	case "ensure":
		info, err := s.ensureSession(context.Background(), key, req.Config)
		return daemonResponseForInfo(info, err)
	case "acquire_exec":
		info, leaseID, err := s.acquireExec(context.Background(), key, req.OwnerPID, req.Config)
		resp := daemonResponseForInfo(info, err)
		if err == nil {
			resp.LeaseID = leaseID
		}
		return resp
	case "release_exec":
		err := s.releaseExec(context.Background(), key, req.LeaseID)
		return daemonResponseForInfo(nil, err)
	case "flush":
		err := s.flushSession(context.Background(), key)
		return daemonResponseForInfo(nil, err)
	case "end":
		ended, err := s.endSession(context.Background(), key)
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

func (s *daemonState) ensureSession(ctx context.Context, sessionKey string, cfg daemonSessionConfig) (*daemonSessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.ensureSessionLocked(ctx, sessionKey, cfg)
	if err != nil {
		return nil, err
	}
	info := sess.info
	return &info, nil
}

func (s *daemonState) acquireExec(ctx context.Context, sessionKey string, ownerPID int, cfg daemonSessionConfig) (*daemonSessionInfo, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
		_ = umountPathsReverse(sess.mount, masks)
		return nil, "", model.Wrap(model.ExitEnv, "mask ignored files", err)
	}
	leaseID, err := daemonNonce()
	if err != nil {
		_ = umountPathsReverse(sess.mount, masks)
		return nil, "", model.Wrap(model.ExitEnv, "create daemon lease", err)
	}
	sess.ignoredMasks = masks
	sess.initialIgnored = initialIgnored
	sess.busy = true
	sess.busyOwnerPID = ownerPID
	sess.busyLease = leaseID
	sess.lastUsed = s.deps.now()
	info := sess.info
	return &info, leaseID, nil
}

func (s *daemonState) releaseExec(ctx context.Context, sessionKey, leaseID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[sessionKey]
	if sess == nil {
		return model.Wrap(model.ExitEnv, "release daemon session", fmt.Errorf("session %q not found", sessionKey))
	}
	if !sess.busy || sess.busyLease != leaseID {
		return model.Wrap(model.ExitEnv, "release daemon session", fmt.Errorf("session %q lease mismatch", sessionKey))
	}
	return s.finishBusyLocked(ctx, sess)
}

func (s *daemonState) endSession(ctx context.Context, sessionKey string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[sessionKey]
	if sess == nil {
		return false, nil
	}
	if err := s.recoverBusyLocked(ctx, sess); err != nil {
		return false, err
	}
	if sess.busy {
		return false, model.Wrap(model.ExitEnv, "end daemon session", fmt.Errorf("session %q is still busy in pid %d", sessionKey, sess.busyOwnerPID))
	}
	var currentIgnored []string
	if sess.config.IgnoredMax != 0 {
		var err error
		currentIgnored, err = listIgnoredCandidatesChecked(ctx, sess.g, sess.info.ViewPath, sess.menv.gitDirForOps, sess.config.IgnoredMax)
		if err != nil {
			return false, model.Wrap(model.ExitEnv, "list ignored files", err)
		}
	}
	if err := s.writeSessionPatch(ctx, sess, currentIgnored); err != nil {
		return false, err
	}
	if err := sess.cleanup.Run(); err != nil {
		return false, model.Wrap(model.ExitEnv, "cleanup daemon session", err)
	}
	delete(s.sessions, sessionKey)
	return true, nil
}

func (s *daemonState) flushSession(ctx context.Context, sessionKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[sessionKey]
	if sess == nil {
		return nil
	}
	if err := s.recoverBusyLocked(ctx, sess); err != nil {
		return err
	}
	if sess.busy {
		return model.Wrap(model.ExitEnv, "flush daemon session", fmt.Errorf("session %q is still busy in pid %d", sessionKey, sess.busyOwnerPID))
	}
	var currentIgnored []string
	if sess.config.IgnoredMax != 0 {
		var err error
		currentIgnored, err = listIgnoredCandidatesChecked(ctx, sess.g, sess.info.ViewPath, sess.menv.gitDirForOps, sess.config.IgnoredMax)
		if err != nil {
			return model.Wrap(model.ExitEnv, "list ignored files", err)
		}
	}
	if err := s.writeSessionPatch(ctx, sess, currentIgnored); err != nil {
		return err
	}
	sess.lastUsed = s.deps.now()
	return nil
}

func (s *daemonState) closeAll() {
	s.mu.Lock()
	sessions := make([]*daemonSession, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.sessions = make(map[string]*daemonSession)
	s.mu.Unlock()
	for _, sess := range sessions {
		_ = sess.cleanup.Run()
	}
}

func (s *daemonState) ensureSessionLocked(ctx context.Context, sessionKey string, cfg daemonSessionConfig) (*daemonSession, error) {
	if sessionKey == "" {
		return nil, model.Wrap(model.ExitEnv, "daemon session key", fmt.Errorf("session-key is required"))
	}
	if existing := s.sessions[sessionKey]; existing != nil {
		if !existing.config.equal(cfg) {
			return nil, model.Wrap(model.ExitEnv, "daemon session options", fmt.Errorf("session %q already exists with different options; end it before changing base/apply/ignored settings", sessionKey))
		}
		existing.lastUsed = s.deps.now()
		return existing, nil
	}
	sess, err := s.createSessionLocked(ctx, sessionKey, cfg)
	if err != nil {
		return nil, err
	}
	s.sessions[sessionKey] = sess
	return sess, nil
}

func (s *daemonState) createSessionLocked(ctx context.Context, sessionKey string, cfg daemonSessionConfig) (_ *daemonSession, retErr error) {
	patchPath := daemonSessionPatchPath(s.gitDir, sessionKey)
	patchStore, err := patchio.OpenPatchStore(patchPath)
	if err != nil {
		return nil, model.Wrap(model.ExitWrite, "open patch store", err)
	}
	lock, err := patchStore.Lock()
	if err != nil {
		_ = patchStore.Close()
		return nil, model.Wrap(model.ExitWrite, "lock patch", err)
	}
	cleanup := &cleanupStack{}
	cleanup.Push(func() error { return patchStore.Close() })
	cleanup.Push(func() error { return lock.Unlock() })
	defer func() {
		if retErr != nil {
			_ = cleanup.Run()
		}
	}()

	sess, err := session.Create(s.gitDir)
	if err != nil {
		return nil, model.Wrap(model.ExitEnv, "create session", err)
	}
	cleanup.Push(func() error { return sess.RemoveAll() })

	repoReal := resolvePath(s.repoRoot)
	patchReal := resolvePath(patchPath)
	patchInRepo, patchRel := isSubpath(repoReal, patchReal)
	if patchInRepo {
		patchRel = filepath.ToSlash(patchRel)
	}
	g := s.deps.newGit(s.repoRoot, s.gitDir, false)
	baseOpts := cfg.options()
	basePath, err := prepareBase(ctx, g, baseOpts, sess, patchInRepo, patchRel, cleanup, &retErr)
	if err != nil {
		return nil, err
	}
	mount := s.deps.newMount()
	menv, err := setupMountTargetEnv(sess.View, s.gitDir, basePath, sess, mount, cleanup, s.log)
	if err != nil {
		return nil, err
	}
	pushUmountCleanup(cleanup, mount, sess.View)
	maskTargets := make([]string, 0, 4)
	commandMaskPaths := patchStatePathsForRepo(sess.View, patchInRepo && patchRel != "" && !strings.HasPrefix(patchRel, ".git/") && patchRel != ".git", patchRel).abs
	if err := applyPatchStore(ctx, g, cfg.ApplyMode, patchStore, sess.View, menv.gitDirForOps, sess.Root, s.log); err != nil {
		return nil, model.Wrap(model.ExitApply, "apply patch", err)
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
	cleanup.Push(func() error {
		return umountPathsReverse(mount, maskTargets)
	})
	info := daemonSessionInfo{
		SessionKey: sessionKey,
		RepoRoot:   s.repoRoot,
		GitDir:     s.gitDir,
		ViewPath:   sess.View,
		PatchPath:  patchPath,
	}
	return &daemonSession{
		key:         sessionKey,
		config:      cfg,
		info:        info,
		store:       patchStore,
		lock:        lock,
		sess:        sess,
		cleanup:     cleanup,
		g:           g,
		mount:       mount,
		menv:        menv,
		patchInRepo: patchInRepo,
		patchRel:    patchRel,
		lastUsed:    s.deps.now(),
	}, nil
}

func (s *daemonState) recoverBusyLocked(ctx context.Context, sess *daemonSession) error {
	if !sess.busy {
		return nil
	}
	if processAlive(sess.busyOwnerPID) {
		return nil
	}
	return s.finishBusyLocked(ctx, sess)
}

func (s *daemonState) finishBusyLocked(ctx context.Context, sess *daemonSession) error {
	exportErr := s.writeSessionPatch(ctx, sess, sess.initialIgnored)
	unmaskErr := umountPathsReverse(sess.mount, sess.ignoredMasks)
	sess.ignoredMasks = nil
	sess.initialIgnored = nil
	sess.busy = false
	sess.busyOwnerPID = 0
	sess.busyLease = ""
	sess.lastUsed = s.deps.now()
	if exportErr != nil && unmaskErr != nil {
		return errors.Join(exportErr, model.Wrap(model.ExitEnv, "unmask ignored files", unmaskErr))
	}
	if exportErr != nil {
		return exportErr
	}
	if unmaskErr != nil {
		return model.Wrap(model.ExitEnv, "unmask ignored files", unmaskErr)
	}
	return nil
}

func (s *daemonState) writeSessionPatch(ctx context.Context, sess *daemonSession, initialIgnored []string) error {
	exportFile, err := os.CreateTemp(sess.menv.tempRoot, "persona-daemon-export-*.patch")
	if err != nil {
		return model.Wrap(model.ExitExport, "create export temp", err)
	}
	defer closeAndRemoveTempFile(exportFile)
	if _, err := exportPatchToWriter(ctx, sess.g, sess.info.ViewPath, sess.menv.gitDirForOps, sess.patchInRepo, sess.patchRel, sess.config.IgnoredMax, initialIgnored, exportFile); err != nil {
		return model.Wrap(model.ExitExport, "export patch", err)
	}
	if _, err := exportFile.Seek(0, 0); err != nil {
		return model.Wrap(model.ExitWrite, "rewind export patch", err)
	}
	if err := sess.store.WriteFromReader(exportFile); err != nil {
		return model.Wrap(model.ExitWrite, "write patch", err)
	}
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
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func daemonSessionPatchPath(gitDir, sessionKey string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(sessionKey)))
	return filepath.Join(gitDir, "persona", "daemon", "patches", fmt.Sprintf("%s-%s.patch", daemonSafeName(sessionKey), hex.EncodeToString(digest[:8])))
}

func daemonSafeName(raw string) string {
	replacer := strings.NewReplacer("/", "-", "\\", "-", " ", "-")
	name := replacer.Replace(strings.TrimSpace(raw))
	name = strings.Trim(name, "-.")
	if name == "" {
		return "session"
	}
	return name
}

func daemonSocketPath(gitDir string) string {
	return filepath.Join(gitDir, "persona", "daemon", daemonSocketFile)
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
	if err := os.MkdirAll(filepath.Join(daemonDir, "patches"), 0o755); err != nil {
		return "", err
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
	if err := daemonPing(ctx, socketPath); err == nil {
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
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX); err != nil {
		return "", model.Wrap(model.ExitEnv, "lock daemon launch", err)
	}
	defer func() {
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	}()
	if err := daemonPing(ctx, socketPath); err == nil {
		return socketPath, nil
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", model.Wrap(model.ExitEnv, "remove stale daemon socket", err)
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
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
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
		time.Sleep(50 * time.Millisecond)
	}
}

func daemonPing(ctx context.Context, socketPath string) error {
	dialer := &net.Dialer{Timeout: 150 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func (c *daemonClient) call(ctx context.Context, req daemonRequest) (*daemonResponse, error) {
	req.RepoRoot = c.repoRoot
	req.GitDir = c.gitDir
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
	if err != nil {
		return nil, model.Wrap(model.ExitEnv, "connect daemon", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, model.Wrap(model.ExitEnv, "encode daemon request", err)
	}
	var resp daemonResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
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

func (c *daemonClient) flush(ctx context.Context, sessionKey string) error {
	_, err := c.call(ctx, daemonRequest{
		Method:     "flush",
		SessionKey: sessionKey,
	})
	return err
}
