package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"persona/internal/gitx"
	"persona/internal/model"
	"persona/internal/ns"
	"persona/internal/patchio"
	"persona/internal/session"

	"github.com/spf13/cobra"
)

type cleanupStack struct {
	fns []func() error
}

const forceMountFailEnv = "PERSONA_FORCE_MOUNT_FAIL"

func (c *cleanupStack) Push(fn func() error) {
	if fn == nil {
		return
	}
	c.fns = append(c.fns, fn)
}

func (c *cleanupStack) Run() error {
	var errs []error
	for i := len(c.fns) - 1; i >= 0; i-- {
		if err := c.fns[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		var exitErr *exitError
		if errors.As(err, &exitErr) {
			os.Exit(int(exitErr.code))
		}
		var personaErr *model.PersonaError
		if errors.As(err, &personaErr) {
			if personaErr.Error() != "" {
				fmt.Fprintln(os.Stderr, personaErr)
			}
			os.Exit(int(personaErr.Code))
		}
		if err.Error() != "" {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(int(model.ExitEnv))
	}
}

type exitError struct {
	code model.ExitCode
}

func (e *exitError) Error() string {
	return ""
}

func runWithOptions(ctx context.Context, opts model.Options) (retErr error, childCode int) {
	cwd, err := os.Getwd()
	if err != nil {
		return model.Wrap(model.ExitEnv, "getwd", err), 0
	}

	repoRoot, gitDir, err := gitx.DetectRepo(ctx, cwd)
	if err != nil {
		return model.Wrap(model.ExitRepo, "detect repo", err), 0
	}

	cwdRel, err := filepath.Rel(repoRoot, cwd)
	if err != nil {
		cwdRel = "."
	}

	var log *slog.Logger
	if opts.Verbose {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	} else {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	log = log.With("component", "persona")

	var g model.GitOps = &gitx.Git{RepoRoot: repoRoot, GitDir: gitDir, Verbose: opts.Verbose}
	var mount model.NSOps = ns.RealNS{}

	patchPath, err := patchio.EnsurePatchPath(opts, gitDir, time.Now())
	if err != nil {
		return model.Wrap(model.ExitWrite, "ensure patch path", err), 0
	}

	patchPath, err = filepath.Abs(patchPath)
	if err != nil {
		return model.Wrap(model.ExitWrite, "resolve patch path", err), 0
	}
	patchPathEffective := resolvePath(patchPath)
	if patchPathEffective == "" {
		patchPathEffective = patchPath
	}
	if err := os.MkdirAll(filepath.Dir(patchPathEffective), 0o755); err != nil {
		return model.Wrap(model.ExitWrite, "ensure patch directory", err), 0
	}
	log.Debug("detected repo", "repo", repoRoot, "gitdir", gitDir, "cwdRel", cwdRel)
	log.Debug("options", "patch", patchPathEffective, "base-mode", opts.BaseMode, "apply-mode", opts.ApplyMode, "ignored-mode", opts.IgnoredMode, "keep-session", opts.KeepSession)
	repoReal := resolvePath(repoRoot)
	patchReal := resolvePath(patchPathEffective)
	patchInRepo, patchRel := isSubpath(repoReal, patchReal)
	if patchInRepo {
		patchRel = filepath.ToSlash(patchRel)
	}

	patchDirFile, err := os.Open(filepath.Dir(patchPathEffective))
	if err != nil {
		return model.Wrap(model.ExitWrite, "open patch directory", err), 0
	}
	defer patchDirFile.Close()

	lock, err := patchio.LockPatch(patchPathEffective)
	if err != nil {
		return model.Wrap(model.ExitWrite, "lock patch", err), 0
	}
	cleanup := &cleanupStack{}
	cleanup.Push(func() error { return lock.Unlock() })
	var sess *session.Session
	defer func() {
		if err := cleanup.Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			if retErr == nil {
				retErr = model.Wrap(model.ExitEnv, "cleanup", err)
				childCode = 0
			}
		}
	}()

	patchData, err := patchio.ReadAll(patchPathEffective)
	if err != nil {
		return model.Wrap(model.ExitWrite, "read patch", err), 0
	}
	log.Debug("read patch", "bytes", len(patchData))

	sess, err = session.Create(gitDir)
	if err != nil {
		return model.Wrap(model.ExitEnv, "create session", err), 0
	}
	log.Debug("session created", "root", sess.Root)

	cleanup.Push(func() error {
		if sess == nil {
			return nil
		}
		if shouldRemoveSession(retErr, opts) {
			return sess.RemoveAll()
		}
		return nil
	})

	basePath, err := prepareBase(ctx, g, opts, sess, patchInRepo, patchRel, cleanup, &retErr)
	if err != nil {
		return err, 0
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := mount.UnshareMountNS(); err != nil {
		reportPermissionHint("unshare mount namespace", err)
		return model.Wrap(model.ExitEnv, "unshare mount namespace", err), 0
	}
	if err := mount.MakeMountsPrivate(); err != nil {
		return model.Wrap(model.ExitEnv, "make mounts private", err), 0
	}

	menv, err := setupMountEnv(repoRoot, gitDir, basePath, sess, mount, cleanup, log)
	if err != nil {
		return err, 0
	}

	maskTargets := make([]string, 0, 16)
	cleanup.Push(func() error {
		var errs []error
		for i := len(maskTargets) - 1; i >= 0; i-- {
			if err := mount.Umount(maskTargets[i]); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
	cleanup.Push(func() error {
		if err := os.Chdir("/"); err != nil {
			return err
		}
		return mount.Umount(repoRoot)
	})

	if err := applyPatchData(ctx, g, opts.ApplyMode, patchData, repoRoot, menv.gitDirForOps, log); err != nil {
		return model.Wrap(model.ExitApply, "apply patch", err), 0
	}

	ignoredMasks, initialIgnored, err := maskIgnoredFiles(ctx, g, repoRoot, menv.gitDirForOps, menv.emptyFile, menv.emptyDir, opts, mount, log)
	if err != nil {
		return model.Wrap(model.ExitEnv, "mask ignored files", err), 0
	}
	maskTargets = append(maskTargets, ignoredMasks...)

	var patchMaskPath string
	if patchInRepo && patchRel != "" && !strings.HasPrefix(patchRel, ".git/") && patchRel != ".git" {
		patchMaskPath = filepath.Join(repoRoot, patchRel)
		if err := mount.MaskPath(patchMaskPath, model.MaskFile, menv.emptyFile, menv.emptyDir); err != nil {
			return model.Wrap(model.ExitEnv, "mask patch file", err), 0
		}
		maskTargets = append(maskTargets, patchMaskPath)
	}

	var gitMaskPath string
	gitPath := filepath.Join(repoRoot, ".git")
	if info, err := os.Lstat(gitPath); err == nil {
		kind := model.MaskDir
		if !info.IsDir() {
			kind = model.MaskFile
		}
		if err := mount.MaskPath(gitPath, kind, menv.emptyFile, menv.emptyDir); err != nil {
			return model.Wrap(model.ExitEnv, "mask .git", err), 0
		}
		gitMaskPath = gitPath
		maskTargets = append(maskTargets, gitPath)
	}

	childCode = runCommand(repoRoot, cwdRel, opts.Command)

	// After the child exits, use a fresh context for export/write —
	// the signal was intended for the child, not for our cleanup path.
	postCtx := context.Background()

	if patchMaskPath != "" {
		_ = mount.Umount(patchMaskPath)
	}
	if gitMaskPath != "" {
		_ = mount.Umount(gitMaskPath)
	}

	patchOut, err := exportPatch(postCtx, g, repoRoot, menv.gitDirForOps, patchInRepo, patchRel, opts.IgnoredMode, initialIgnored)
	if err != nil {
		return model.Wrap(model.ExitExport, "export patch", err), 0
	}
	log.Debug("export complete", "bytes", len(patchOut))

	if err := patchio.AtomicWriteFileAt(patchDirFile, filepath.Base(patchPathEffective), patchOut); err != nil {
		return model.Wrap(model.ExitWrite, "write patch", err), 0
	}

	if opts.PrintPatchPath {
		fmt.Fprintln(os.Stdout, patchPathEffective)
	}
	return nil, childCode
}

// prepareBase sets up the base layer: either the current repo (with an
// optional dirty check) or a detached worktree at the requested ref.
func prepareBase(ctx context.Context, g model.GitOps, opts model.Options, sess *session.Session, patchInRepo bool, patchRel string, cleanup *cleanupStack, retErr *error) (basePath string, err error) {
	switch opts.BaseMode {
	case model.BaseRepo:
		if !opts.AllowDirty {
			ignoreUntracked := []string{}
			if patchInRepo && patchRel != "" && patchRel != "." {
				ignoreUntracked = append(ignoreUntracked, patchRel)
			}
			clean, err := g.IsCleanExceptUntracked(ctx, ignoreUntracked)
			if err != nil {
				return "", model.Wrap(model.ExitRepo, "git clean check", err)
			}
			if !clean {
				return "", model.Wrap(model.ExitRepo, "repository is dirty", fmt.Errorf("uncommitted changes exist"))
			}
		}
		return g.RepoRootPath(), nil
	case model.BaseWorktree:
		if err := g.WorktreeAddDetach(ctx, sess.BaseWT, opts.BaseRef); err != nil {
			return "", model.Wrap(model.ExitRepo, "git worktree add", err)
		}
		cleanup.Push(func() error {
			if retErr != nil && !shouldRemoveSession(*retErr, opts) {
				return nil
			}
			return g.WorktreeRemoveForce(context.Background(), sess.BaseWT)
		})
		return sess.BaseWT, nil
	default:
		return "", model.Wrap(model.ExitEnv, "invalid base mode", fmt.Errorf("%s", opts.BaseMode))
	}
}

// mountEnv holds paths created during namespace setup that later phases need.
type mountEnv struct {
	emptyDir     string // empty directory for mask-dir bind mounts
	emptyFile    string // empty file for mask-file bind mounts
	gitDirForOps string // bind-mounted .git dir accessible from overlay
}

// setupMountEnv creates the external temp dirs, bind-mounts the git dir and
// base layer, and mounts the overlay.  All mounts are registered on the
// cleanup stack for teardown.
func setupMountEnv(repoRoot, gitDir, basePath string, sess *session.Session, mount model.NSOps, cleanup *cleanupStack, log *slog.Logger) (*mountEnv, error) {
	extRoot, err := os.MkdirTemp("", "persona-session-")
	if err != nil {
		return nil, model.Wrap(model.ExitEnv, "create external session dir", err)
	}
	cleanup.Push(func() error { return os.RemoveAll(extRoot) })

	env := &mountEnv{
		emptyDir:     filepath.Join(extRoot, "empty", "emptydir"),
		emptyFile:    filepath.Join(extRoot, "empty", "emptyfile"),
		gitDirForOps: filepath.Join(extRoot, "mnt", "gitdir"),
	}
	if err := os.MkdirAll(env.emptyDir, 0o755); err != nil {
		return nil, model.Wrap(model.ExitEnv, "create external empty dir", err)
	}
	if err := os.MkdirAll(filepath.Dir(env.emptyFile), 0o755); err != nil {
		return nil, model.Wrap(model.ExitEnv, "create external empty file dir", err)
	}
	f, err := os.OpenFile(env.emptyFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, model.Wrap(model.ExitEnv, "create external empty file", err)
	}
	f.Close()

	if err := mount.BindMount(gitDir, env.gitDirForOps); err != nil {
		return nil, model.Wrap(model.ExitEnv, "bind mount external gitdir", err)
	}
	cleanup.Push(func() error { return mount.Umount(env.gitDirForOps) })

	if err := mount.BindMount(basePath, sess.MntBase); err != nil {
		return nil, model.Wrap(model.ExitEnv, "bind mount base", err)
	}
	cleanup.Push(func() error { return mount.Umount(sess.MntBase) })
	if err := mount.RemountRO(sess.MntBase); err != nil {
		return nil, model.Wrap(model.ExitEnv, "remount base ro", err)
	}

	log.Debug("gitdir for overlay ops", "path", env.gitDirForOps)

	if shouldForceMountFail() {
		return nil, model.Wrap(model.ExitEnv, "mount overlay", errors.New("forced mount failure"))
	}
	if err := mount.MountOverlay(repoRoot, model.OverlayOpts{LowerDir: sess.MntBase, UpperDir: sess.Upper, WorkDir: sess.Work}); err != nil {
		reportPermissionHint("mount overlay", err)
		return nil, model.Wrap(model.ExitEnv, "mount overlay", err)
	}
	log.Debug("overlay mounted", "lower", sess.MntBase, "upper", sess.Upper, "work", sess.Work, "target", repoRoot)

	return env, nil
}

// applyPatchData validates and applies patchData to the overlay.
// If the initial apply fails due to already-existing new files, it retries
// with those files filtered out.
func applyPatchData(ctx context.Context, g model.GitOps, applyMode model.ApplyMode, patchData []byte, repoRoot, gitDirForOps string, log *slog.Logger) error {
	if len(patchData) == 0 {
		return nil
	}
	if err := patchio.ValidatePatchPaths(patchData); err != nil {
		return err
	}
	err := g.ApplyPatch(ctx, applyMode, repoRoot, gitDirForOps, patchData)
	if err == nil {
		return nil
	}
	if !patchio.IsAlreadyExistsError(err) {
		return err
	}
	filtered, skipped, ferr := patchio.FilterExistingNewFiles(patchData, repoRoot)
	if ferr != nil || len(skipped) == 0 {
		return err
	}
	log.Info("apply patch: skipping existing new files", "skipped", skipped)
	if len(filtered) == 0 {
		return nil
	}
	if err2 := g.ApplyPatch(ctx, applyMode, repoRoot, gitDirForOps, filtered); err2 != nil {
		return err2
	}
	return nil
}

// maskIgnoredFiles applies the configured ignored-file policy (readonly bind
// mount or empty-file/dir mask) and returns the list of mount targets created.
func maskIgnoredFiles(ctx context.Context, g model.GitOps, repoRoot, gitDirForOps, extEmptyFile, extEmptyDir string, opts model.Options, mount model.NSOps, log *slog.Logger) ([]string, []string, error) {
	if opts.IgnoredMode == model.IgnoredTransparent {
		return nil, nil, nil
	}
	ignored, err := g.ListIgnoredCandidates(ctx, repoRoot, gitDirForOps, opts.IgnoredMax)
	if err != nil {
		return nil, nil, fmt.Errorf("list ignored: %w", err)
	}
	log.Debug("ignored files", "count", len(ignored))
	var targets []string
	for _, path := range ignored {
		target := filepath.Join(repoRoot, filepath.FromSlash(path))
		switch opts.IgnoredMode {
		case model.IgnoredReadonly:
			if _, err := os.Lstat(target); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return targets, ignored, fmt.Errorf("stat ignored readonly %s: %w", path, err)
			}
			if err := mount.BindMount(target, target); err != nil {
				if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
					continue
				}
				return targets, ignored, fmt.Errorf("bind mount ignored readonly %s: %w", path, err)
			}
			if err := mount.RemountRO(target); err != nil {
				if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
					continue
				}
				return targets, ignored, fmt.Errorf("remount ignored readonly %s: %w", path, err)
			}
			targets = append(targets, target)
		case model.IgnoredMasked:
			info, err := os.Lstat(target)
			if err != nil {
				continue
			}
			kind := model.MaskDir
			if !info.IsDir() {
				kind = model.MaskFile
			}
			if err := mount.MaskPath(target, kind, extEmptyFile, extEmptyDir); err != nil {
				return targets, ignored, fmt.Errorf("mask ignored %s: %w", path, err)
			}
			targets = append(targets, target)
		}
	}
	return targets, ignored, nil
}

func newRootCmd() *cobra.Command {
	var (
		patchPath      string
		patchDir       string
		printPatchPath bool

		baseMode   string
		baseRef    string
		allowDirty bool

		ignoredMode  string
		ignoredMax   int
		ignoredScope string

		applyMode   string
		keepSession string
		verbose     bool
	)

	cmd := &cobra.Command{
		Use:           "persona [OPTIONS] -- <command> [args...]",
		Short:         "Run a command in an overlay Git view backed by a patch file",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(os.Stderr, "command is required")
				_ = cmd.Help()
				return &exitError{code: model.ExitEnv}
			}
			opts, err := buildOptions(
				patchPath, patchDir, printPatchPath,
				baseMode, baseRef, allowDirty,
				ignoredMode, ignoredMax, ignoredScope,
				applyMode, keepSession, verbose,
				args,
			)
			if err != nil {
				return &model.PersonaError{Code: model.ExitEnv, Op: "parse options", Err: err}
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer cancel()
			err, childExit := runWithOptions(ctx, opts)
			if err != nil {
				return err
			}
			if childExit != 0 {
				return &exitError{code: model.ExitCode(childExit)}
			}
			return nil
		},
	}
	cmd.SetOut(os.Stderr)
	cmd.SetErr(os.Stderr)

	cmd.Flags().StringVar(&patchPath, "patch", "", "state patch file path")
	cmd.Flags().StringVar(&patchDir, "patch-dir", "", "patch directory")
	cmd.Flags().BoolVar(&printPatchPath, "print-patch-path", false, "print patch path on exit")

	cmd.Flags().StringVar(&baseMode, "base-mode", string(model.BaseRepo), "base mode: repo or worktree")
	cmd.Flags().StringVar(&baseRef, "base-ref", "HEAD", "base ref for worktree mode")
	cmd.Flags().BoolVar(&allowDirty, "allow-dirty", false, "allow dirty repo for base-mode=repo")

	cmd.Flags().StringVar(&ignoredMode, "ignored-mode", string(model.IgnoredTransparent), "ignored mode: transparent, readonly, masked")
	cmd.Flags().IntVar(&ignoredMax, "ignored-max", 200, "max ignored entries to process")
	cmd.Flags().StringVar(&ignoredScope, "ignored-scope", "exact", "ignored scope (v0.1: exact)")

	cmd.Flags().StringVar(&applyMode, "apply-mode", string(model.ApplyStrict), "apply mode: strict or reject")
	cmd.Flags().StringVar(&keepSession, "keep-session", string(model.KeepOnFail), "keep session: on-fail, always, never")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "verbose logging")

	addDiagnosticCommands(cmd)
	return cmd
}

func parseEnum[T ~string](input, name string, valid ...T) (T, error) {
	for _, v := range valid {
		if input == string(v) {
			return v, nil
		}
	}
	var zero T
	return zero, fmt.Errorf("invalid %s: %s", name, input)
}

func buildOptions(
	patchPath, patchDir string,
	printPatchPath bool,
	baseMode, baseRef string,
	allowDirty bool,
	ignoredMode string,
	ignoredMax int,
	ignoredScope string,
	applyMode string,
	keepSession string,
	verbose bool,
	args []string,
) (model.Options, error) {
	var opts model.Options
	if strings.TrimSpace(ignoredScope) != "exact" {
		return opts, fmt.Errorf("ignored-scope only supports exact in v0.1")
	}

	opts.PatchPath = strings.TrimSpace(patchPath)
	opts.PatchDir = strings.TrimSpace(patchDir)
	opts.PrintPatchPath = printPatchPath

	var err error
	opts.BaseMode, err = parseEnum(baseMode, "base-mode", model.BaseRepo, model.BaseWorktree)
	if err != nil {
		return opts, err
	}
	opts.BaseRef = baseRef
	opts.AllowDirty = allowDirty

	opts.IgnoredMode, err = parseEnum(ignoredMode, "ignored-mode", model.IgnoredTransparent, model.IgnoredReadonly, model.IgnoredMasked)
	if err != nil {
		return opts, err
	}
	if ignoredMax < 0 {
		return opts, fmt.Errorf("ignored-max must be >= 0")
	}
	opts.IgnoredMax = ignoredMax

	opts.ApplyMode, err = parseEnum(applyMode, "apply-mode", model.ApplyStrict, model.ApplyReject)
	if err != nil {
		return opts, err
	}

	opts.KeepSession, err = parseEnum(keepSession, "keep-session", model.KeepOnFail, model.KeepAlways, model.KeepNever)
	if err != nil {
		return opts, err
	}
	opts.Verbose = verbose
	opts.Command = args
	return opts, nil
}

func runCommand(repoRoot, cwdRel string, cmdArgs []string) int {
	if len(cmdArgs) == 0 {
		return 0
	}
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = os.Environ()
	if cwdRel == "." {
		cmd.Dir = repoRoot
	} else {
		cmd.Dir = filepath.Join(repoRoot, cwdRel)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 127
	}

	// Set up signal forwarding only after the process has started,
	// so cmd.Process is guaranteed to be non-nil and we avoid the
	// race where a signal arrives before Start() completes.
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		for sig := range sigCh {
			_ = syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
		}
	}()

	err := cmd.Wait()
	signal.Stop(sigCh)
	close(sigCh)
	pgid := cmd.Process.Pid
	// Prevent late background descendants from racing with export/write-back.
	if killErr := syscall.Kill(-pgid, syscall.SIGTERM); killErr == nil || errors.Is(killErr, syscall.EPERM) {
		deadline := time.Now().Add(200 * time.Millisecond)
		for {
			probeErr := syscall.Kill(-pgid, 0)
			if probeErr != nil && !errors.Is(probeErr, syscall.EPERM) {
				break
			}
			if time.Now().After(deadline) {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
				for i := 0; i < 20; i++ {
					probeErr = syscall.Kill(-pgid, 0)
					if probeErr != nil && !errors.Is(probeErr, syscall.EPERM) {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	fmt.Fprintln(os.Stderr, err)
	return 127
}

func exportPatch(ctx context.Context, g model.GitOps, repoRoot, gitDir string, patchInRepo bool, patchRel string, ignoredMode model.IgnoredMode, initialIgnored []string) ([]byte, error) {
	if ignoredMode != model.IgnoredTransparent {
		ignoredNow, err := g.ListIgnoredCandidates(ctx, repoRoot, gitDir, 0)
		if err != nil {
			return nil, err
		}
		ignoredSet := make(map[string]struct{}, len(initialIgnored))
		for _, path := range initialIgnored {
			ignoredSet[path] = struct{}{}
		}
		var newlyIgnored []string
		for _, path := range ignoredNow {
			if _, ok := ignoredSet[path]; ok {
				continue
			}
			newlyIgnored = append(newlyIgnored, path)
		}
		if len(newlyIgnored) > 0 {
			sort.Strings(newlyIgnored)
			return nil, fmt.Errorf("new ignored paths appeared during run: %s", strings.Join(newlyIgnored, ", "))
		}
	}
	trackedExclude := []string{}
	if patchInRepo && patchRel != "" {
		trackedExclude = append(trackedExclude, filepath.ToSlash(patchRel))
	}
	tracked, err := g.DiffHeadBinary(ctx, repoRoot, gitDir, trackedExclude)
	if err != nil {
		return nil, err
	}
	untracked, err := g.ListUntracked(ctx, repoRoot, gitDir)
	if err != nil {
		return nil, err
	}
	excludePrefixes := []string{".git/"}
	excludeExact := []string{}
	if patchInRepo && patchRel != "" {
		excludeExact = append(excludeExact, filepath.ToSlash(patchRel))
	}
	untracked = patchio.FilterUntrackedPaths(untracked, excludePrefixes, excludeExact)
	sort.Strings(untracked)

	buf := &bytes.Buffer{}
	if len(tracked) > 0 {
		buf.Write(tracked)
	}
	for _, path := range untracked {
		absPath := filepath.Join(repoRoot, filepath.FromSlash(path))
		info, err := os.Lstat(absPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		mode := info.Mode()
		if mode&os.ModeNamedPipe != 0 || mode&os.ModeDevice != 0 || mode&os.ModeCharDevice != 0 || mode&os.ModeSocket != 0 {
			fmt.Fprintln(os.Stderr, "skip special file", path)
			continue
		}
		patch, err := g.DiffNewFileNoIndex(ctx, repoRoot, gitDir, path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
				continue
			}
			return nil, err
		}
		buf.Write(patch)
	}
	return buf.Bytes(), nil
}

func isSubpath(root, path string) (bool, string) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false, ""
	}
	rel = filepath.Clean(rel)
	if rel == "." {
		return true, rel
	}
	parentPrefix := ".." + string(filepath.Separator)
	if rel == ".." || strings.HasPrefix(rel, parentPrefix) {
		return false, ""
	}
	return true, rel
}

func resolvePath(path string) string {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real
	}
	info, statErr := os.Lstat(path)
	if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err == nil {
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(path), target)
			}
			return filepath.Clean(target)
		}
	}
	return path
}

func shouldRemoveSession(err error, opts model.Options) bool {
	switch opts.KeepSession {
	case model.KeepAlways:
		return false
	case model.KeepNever:
		return true
	case model.KeepOnFail:
		return err == nil
	default:
		return true
	}
}

func shouldForceMountFail() bool {
	return os.Getenv(forceMountFailEnv) == "1"
}
