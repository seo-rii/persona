package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
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

	patchStore, err := patchio.OpenPatchStore(patchPathEffective)
	if err != nil {
		return model.Wrap(model.ExitWrite, "open patch store", err), 0
	}
	cleanup := &cleanupStack{}
	cleanup.Push(func() error { return patchStore.Close() })

	lock, err := patchStore.Lock()
	if err != nil {
		return model.Wrap(model.ExitWrite, "lock patch", err), 0
	}
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

	patchData, err := patchStore.ReadAll()
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

	var patchMaskPaths []string
	if patchInRepo && patchRel != "" && !strings.HasPrefix(patchRel, ".git/") && patchRel != ".git" {
		patchMaskPaths = append(patchMaskPaths, filepath.Join(repoRoot, patchRel))
		patchMaskPaths = append(patchMaskPaths, filepath.Join(repoRoot, patchRel+".lock"))
		for _, patchMaskPath := range patchMaskPaths {
			maskEmptyFile, maskEmptyDir, err := prepareMaskBacking(menv.emptyFile, menv.emptyDir, patchMaskPath)
			if err != nil {
				return model.Wrap(model.ExitEnv, "prepare patch mask backing", err), 0
			}
			if err := mount.MaskPath(patchMaskPath, model.MaskFile, maskEmptyFile, maskEmptyDir); err != nil {
				return model.Wrap(model.ExitEnv, "mask patch file", err), 0
			}
			maskTargets = append(maskTargets, patchMaskPath)
		}
	}

	var gitMaskPath string
	gitPath := filepath.Join(repoRoot, ".git")
	if info, err := os.Lstat(gitPath); err == nil {
		kind := model.MaskDir
		if !info.IsDir() {
			kind = model.MaskFile
		}
		maskEmptyFile, maskEmptyDir, err := prepareMaskBacking(menv.emptyFile, menv.emptyDir, gitPath)
		if err != nil {
			return model.Wrap(model.ExitEnv, "prepare .git mask backing", err), 0
		}
		if err := mount.MaskPath(gitPath, kind, maskEmptyFile, maskEmptyDir); err != nil {
			return model.Wrap(model.ExitEnv, "mask .git", err), 0
		}
		gitMaskPath = gitPath
		maskTargets = append(maskTargets, gitPath)
	}

	childCode = runCommand(repoRoot, cwdRel, opts.Command)

	postCtx := context.Background()

	for i := len(patchMaskPaths) - 1; i >= 0; i-- {
		_ = mount.Umount(patchMaskPaths[i])
	}
	if gitMaskPath != "" {
		_ = mount.Umount(gitMaskPath)
	}

	patchOut, err := exportPatch(postCtx, g, repoRoot, menv.gitDirForOps, patchInRepo, patchRel, opts.IgnoredMode, opts.IgnoredMax, initialIgnored)
	if err != nil {
		return model.Wrap(model.ExitExport, "export patch", err), 0
	}
	log.Debug("export complete", "bytes", len(patchOut))

	if err := patchStore.WriteAll(patchOut); err != nil {
		return model.Wrap(model.ExitWrite, "write patch", err), 0
	}

	if opts.PrintPatchPath {
		fmt.Fprintln(os.Stdout, patchPathEffective)
	}
	return nil, childCode
}

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
				ignoredMode, ignoredMax,
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

	cmd.Flags().StringVar(&patchPath, "patch", "", "patch file path (default: auto-generate)")
	cmd.Flags().StringVar(&patchDir, "patch-dir", "", "directory for auto-generated patch files (default: <gitdir>/persona/patches)")
	cmd.Flags().BoolVar(&printPatchPath, "print-patch-path", false, "print patch path on exit")

	cmd.Flags().StringVar(&baseMode, "base-mode", string(model.BaseRepo), "base mode: repo | worktree")
	cmd.Flags().StringVar(&baseRef, "base-ref", "HEAD", "base ref for worktree mode")
	cmd.Flags().BoolVar(&allowDirty, "allow-dirty", false, "allow dirty repo in repo base mode")

	cmd.Flags().StringVar(&ignoredMode, "ignored-mode", string(model.IgnoredTransparent), "ignored mode: transparent | readonly | masked")
	cmd.Flags().IntVar(&ignoredMax, "ignored-max", 200, "max ignored entries to process")

	cmd.Flags().StringVar(&applyMode, "apply-mode", string(model.ApplyStrict), "apply mode: strict | reject")
	cmd.Flags().StringVar(&keepSession, "keep-session", string(model.KeepOnFail), "keep session: on-fail | always | never")
	cmd.Flags().BoolVar(&verbose, "verbose", false, "enable verbose logging")

	cmd.AddCommand(newDoctorCmd())
	cmd.AddCommand(newActivateCmd())
	return cmd
}

func parseEnum[T ~string](input, name string, valid ...T) (T, error) {
	var zero T
	value := strings.TrimSpace(input)
	for _, item := range valid {
		if value == string(item) {
			return item, nil
		}
	}
	return zero, fmt.Errorf("invalid %s: %s", name, input)
}

func buildOptions(
	patchPath string,
	patchDir string,
	printPatchPath bool,
	baseMode string,
	baseRef string,
	allowDirty bool,
	ignoredMode string,
	ignoredMax int,
	applyMode string,
	keepSession string,
	verbose bool,
	args []string,
) (model.Options, error) {
	var opts model.Options

	opts.PatchPath = strings.TrimSpace(patchPath)
	opts.PatchDir = strings.TrimSpace(patchDir)
	opts.PrintPatchPath = printPatchPath

	mode, err := parseEnum(baseMode, "base-mode", model.BaseRepo, model.BaseWorktree)
	if err != nil {
		return opts, err
	}
	opts.BaseMode = mode
	opts.BaseRef = strings.TrimSpace(baseRef)
	if opts.BaseRef == "" {
		opts.BaseRef = "HEAD"
	}
	if opts.BaseMode == model.BaseRepo && opts.BaseRef != "HEAD" {
		return opts, fmt.Errorf("base-ref is only valid with worktree base-mode")
	}
	opts.AllowDirty = allowDirty

	ignored, err := parseEnum(ignoredMode, "ignored-mode", model.IgnoredTransparent, model.IgnoredReadonly, model.IgnoredMasked)
	if err != nil {
		return opts, err
	}
	if ignoredMax < 0 {
		return opts, fmt.Errorf("ignored-max must be >= 0")
	}
	opts.IgnoredMode = ignored
	opts.IgnoredMax = ignoredMax

	apply, err := parseEnum(applyMode, "apply-mode", model.ApplyStrict, model.ApplyReject)
	if err != nil {
		return opts, err
	}
	opts.ApplyMode = apply

	keep, err := parseEnum(keepSession, "keep-session", model.KeepOnFail, model.KeepAlways, model.KeepNever)
	if err != nil {
		return opts, err
	}
	opts.KeepSession = keep

	opts.Verbose = verbose
	opts.Command = args
	return opts, nil
}
