package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	logf := func(format string, args ...any) {
		if !opts.Verbose {
			return
		}
		fmt.Fprintf(os.Stderr, "[persona] "+format+"\n", args...)
	}

	g := gitx.Git{RepoRoot: repoRoot, GitDir: gitDir, Verbose: opts.Verbose}

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
	logf("repo=%s gitdir=%s cwdRel=%s", repoRoot, gitDir, cwdRel)
	logf("patch=%s base-mode=%s apply-mode=%s ignored-mode=%s keep-session=%s", patchPathEffective, opts.BaseMode, opts.ApplyMode, opts.IgnoredMode, opts.KeepSession)
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
	logf("patch bytes=%d", len(patchData))

	sess, err = session.Create(gitDir)
	if err != nil {
		return model.Wrap(model.ExitEnv, "create session", err), 0
	}
	logf("session root=%s", sess.Root)

	cleanup.Push(func() error {
		if sess == nil {
			return nil
		}
		if shouldRemoveSession(retErr, opts) {
			return sess.RemoveAll()
		}
		return nil
	})

	worktreeAdded := false
	if opts.BaseMode == model.BaseRepo {
		if !opts.AllowDirty {
			ignoreUntracked := []string{}
			if patchInRepo && patchRel != "" && patchRel != "." {
				ignoreUntracked = append(ignoreUntracked, patchRel)
			}
			clean, err := g.IsCleanExceptUntracked(ctx, ignoreUntracked)
			if err != nil {
				return model.Wrap(model.ExitRepo, "git clean check", err), 0
			}
			if !clean {
				return model.Wrap(model.ExitRepo, "repository is dirty", fmt.Errorf("uncommitted changes exist")), 0
			}
		}
	} else if opts.BaseMode == model.BaseWorktree {
		if err := g.WorktreeAddDetach(ctx, sess.BaseWT, opts.BaseRef); err != nil {
			return model.Wrap(model.ExitRepo, "git worktree add", err), 0
		}
		worktreeAdded = true
		cleanup.Push(func() error {
			if worktreeAdded {
				return g.WorktreeRemoveForce(context.Background(), sess.BaseWT)
			}
			return nil
		})
	} else {
		return model.Wrap(model.ExitEnv, "invalid base mode", fmt.Errorf("%s", opts.BaseMode)), 0
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := ns.UnshareMountNS(); err != nil {
		reportPermissionHint("unshare mount namespace", err)
		return model.Wrap(model.ExitEnv, "unshare mount namespace", err), 0
	}
	if err := ns.MakeMountsPrivate(); err != nil {
		return model.Wrap(model.ExitEnv, "make mounts private", err), 0
	}

	extRoot, err := os.MkdirTemp("", "persona-session-")
	if err != nil {
		return model.Wrap(model.ExitEnv, "create external session dir", err), 0
	}
	cleanup.Push(func() error {
		return os.RemoveAll(extRoot)
	})
	extEmptyDir := filepath.Join(extRoot, "empty", "emptydir")
	extEmptyFile := filepath.Join(extRoot, "empty", "emptyfile")
	extGitDir := filepath.Join(extRoot, "mnt", "gitdir")
	if err := os.MkdirAll(extEmptyDir, 0o755); err != nil {
		return model.Wrap(model.ExitEnv, "create external empty dir", err), 0
	}
	if err := os.MkdirAll(filepath.Dir(extEmptyFile), 0o755); err != nil {
		return model.Wrap(model.ExitEnv, "create external empty file dir", err), 0
	}
	file, err := os.OpenFile(extEmptyFile, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return model.Wrap(model.ExitEnv, "create external empty file", err), 0
	}
	file.Close()
	basePath := repoRoot
	if opts.BaseMode == model.BaseWorktree {
		basePath = sess.BaseWT
	}

	if err := ns.BindMount(gitDir, extGitDir); err != nil {
		return model.Wrap(model.ExitEnv, "bind mount external gitdir", err), 0
	}
	cleanup.Push(func() error { return ns.Umount(extGitDir) })

	if err := ns.BindMount(basePath, sess.MntBase); err != nil {
		return model.Wrap(model.ExitEnv, "bind mount base", err), 0
	}
	cleanup.Push(func() error { return ns.Umount(sess.MntBase) })
	if err := ns.RemountRO(sess.MntBase); err != nil {
		return model.Wrap(model.ExitEnv, "remount base ro", err), 0
	}

	if err := ns.BindMount(gitDir, sess.MntGitDir); err != nil {
		return model.Wrap(model.ExitEnv, "bind mount gitdir", err), 0
	}
	cleanup.Push(func() error { return ns.Umount(sess.MntGitDir) })
	gitDirForOps := extGitDir
	logf("gitdir-for-ops=%s", gitDirForOps)

	if err := ns.MountOverlay(repoRoot, ns.OverlayOpts{LowerDir: sess.MntBase, UpperDir: sess.Upper, WorkDir: sess.Work}); err != nil {
		reportPermissionHint("mount overlay", err)
		return model.Wrap(model.ExitEnv, "mount overlay", err), 0
	}
	logf("overlay mounted lower=%s upper=%s work=%s target=%s", sess.MntBase, sess.Upper, sess.Work, repoRoot)

	maskTargets := make([]string, 0, 16)
	cleanup.Push(func() error {
		var errs []error
		for i := len(maskTargets) - 1; i >= 0; i-- {
			if err := ns.Umount(maskTargets[i]); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
	cleanup.Push(func() error {
		if err := os.Chdir("/"); err != nil {
			return err
		}
		return ns.Umount(repoRoot)
	})

	ignoredMasks, err := maskIgnoredFiles(ctx, g, repoRoot, gitDirForOps, extEmptyFile, extEmptyDir, opts, logf)
	if err != nil {
		return model.Wrap(model.ExitEnv, "mask ignored files", err), 0
	}
	maskTargets = append(maskTargets, ignoredMasks...)

	if err := applyPatchData(ctx, g, opts.ApplyMode, patchData, repoRoot, gitDirForOps, logf); err != nil {
		return model.Wrap(model.ExitApply, "apply patch", err), 0
	}

	var patchMaskPath string
	if patchInRepo && patchRel != "" && !strings.HasPrefix(patchRel, ".git/") && patchRel != ".git" {
		patchMaskPath = filepath.Join(repoRoot, patchRel)
		if err := ns.MaskPath(patchMaskPath, ns.MaskFile, extEmptyFile, extEmptyDir); err != nil {
			return model.Wrap(model.ExitEnv, "mask patch file", err), 0
		}
		maskTargets = append(maskTargets, patchMaskPath)
	}

	var gitMaskPath string
	gitPath := filepath.Join(repoRoot, ".git")
	if info, err := os.Lstat(gitPath); err == nil {
		kind := ns.MaskDir
		if !info.IsDir() {
			kind = ns.MaskFile
		}
		if err := ns.MaskPath(gitPath, kind, extEmptyFile, extEmptyDir); err != nil {
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
		_ = ns.Umount(patchMaskPath)
	}
	if gitMaskPath != "" {
		_ = ns.Umount(gitMaskPath)
	}

	patchOut, err := exportPatch(postCtx, g, repoRoot, gitDirForOps, patchInRepo, patchRel)
	if err != nil {
		return model.Wrap(model.ExitExport, "export patch", err), 0
	}
	logf("export bytes=%d", len(patchOut))

	if err := patchio.AtomicWriteFileAt(patchDirFile, filepath.Base(patchPathEffective), patchOut); err != nil {
		return model.Wrap(model.ExitWrite, "write patch", err), 0
	}

	if opts.PrintPatchPath {
		fmt.Fprintln(os.Stdout, patchPathEffective)
	}
	return nil, childCode
}

// applyPatchData validates and applies patchData to the overlay.
// If the initial apply fails due to already-existing new files, it retries
// with those files filtered out.
func applyPatchData(ctx context.Context, g gitx.Git, applyMode model.ApplyMode, patchData []byte, repoRoot, gitDirForOps string, logf func(string, ...any)) error {
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
	logf("apply patch: skipping existing new files: %s", strings.Join(skipped, ", "))
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
func maskIgnoredFiles(ctx context.Context, g gitx.Git, repoRoot, gitDirForOps, extEmptyFile, extEmptyDir string, opts model.Options, logf func(string, ...any)) ([]string, error) {
	if opts.IgnoredMode == model.IgnoredTransparent {
		return nil, nil
	}
	ignored, err := g.ListIgnoredCandidates(ctx, repoRoot, gitDirForOps, opts.IgnoredMax)
	if err != nil {
		return nil, fmt.Errorf("list ignored: %w", err)
	}
	logf("ignored count=%d", len(ignored))
	var targets []string
	for _, path := range ignored {
		target := filepath.Join(repoRoot, filepath.FromSlash(path))
		switch opts.IgnoredMode {
		case model.IgnoredReadonly:
			if err := ns.BindMount(target, target); err != nil {
				return targets, fmt.Errorf("bind mount ignored readonly %s: %w", path, err)
			}
			if err := ns.RemountRO(target); err != nil {
				return targets, fmt.Errorf("remount ignored readonly %s: %w", path, err)
			}
			targets = append(targets, target)
		case model.IgnoredMasked:
			info, err := os.Lstat(target)
			if err != nil {
				continue
			}
			kind := ns.MaskDir
			if !info.IsDir() {
				kind = ns.MaskFile
			}
			if err := ns.MaskPath(target, kind, extEmptyFile, extEmptyDir); err != nil {
				return targets, fmt.Errorf("mask ignored %s: %w", path, err)
			}
			targets = append(targets, target)
		}
	}
	return targets, nil
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

	switch baseMode {
	case string(model.BaseRepo):
		opts.BaseMode = model.BaseRepo
	case string(model.BaseWorktree):
		opts.BaseMode = model.BaseWorktree
	default:
		return opts, fmt.Errorf("invalid base-mode: %s", baseMode)
	}
	opts.BaseRef = baseRef
	opts.AllowDirty = allowDirty

	switch ignoredMode {
	case string(model.IgnoredTransparent):
		opts.IgnoredMode = model.IgnoredTransparent
	case string(model.IgnoredReadonly):
		opts.IgnoredMode = model.IgnoredReadonly
	case string(model.IgnoredMasked):
		opts.IgnoredMode = model.IgnoredMasked
	default:
		return opts, fmt.Errorf("invalid ignored-mode: %s", ignoredMode)
	}
	if ignoredMax < 0 {
		return opts, fmt.Errorf("ignored-max must be >= 0")
	}
	opts.IgnoredMax = ignoredMax

	switch applyMode {
	case string(model.ApplyStrict):
		opts.ApplyMode = model.ApplyStrict
	case string(model.ApplyReject):
		opts.ApplyMode = model.ApplyReject
	default:
		return opts, fmt.Errorf("invalid apply-mode: %s", applyMode)
	}

	switch keepSession {
	case string(model.KeepOnFail):
		opts.KeepSession = model.KeepOnFail
	case string(model.KeepAlways):
		opts.KeepSession = model.KeepAlways
	case string(model.KeepNever):
		opts.KeepSession = model.KeepNever
	default:
		return opts, fmt.Errorf("invalid keep-session: %s", keepSession)
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

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		for sig := range sigCh {
			if cmd.Process == nil {
				continue
			}
			_ = syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
		}
	}()

	err := cmd.Run()
	signal.Stop(sigCh)
	close(sigCh)
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	fmt.Fprintln(os.Stderr, err)
	return 127
}

func exportPatch(ctx context.Context, g gitx.Git, repoRoot, gitDir string, patchInRepo bool, patchRel string) ([]byte, error) {
	tracked, err := g.DiffHeadBinary(ctx, repoRoot, gitDir)
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
		if skipSpecial(absPath) {
			fmt.Fprintln(os.Stderr, "skip special file", path)
			continue
		}
		patch, err := g.DiffNewFileNoIndex(ctx, repoRoot, gitDir, path)
		if err != nil {
			return nil, err
		}
		buf.Write(patch)
	}
	return buf.Bytes(), nil
}

func skipSpecial(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	mode := info.Mode()
	if mode&os.ModeNamedPipe != 0 || mode&os.ModeDevice != 0 || mode&os.ModeCharDevice != 0 || mode&os.ModeSocket != 0 {
		return true
	}
	return false
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
