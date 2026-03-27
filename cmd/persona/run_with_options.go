package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"persona/internal/gitx"
	"persona/internal/model"
	"persona/internal/ns"
	"persona/internal/patchio"
	"persona/internal/session"
)

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

	if err := applyPatchStore(ctx, g, opts.ApplyMode, patchStore, repoRoot, menv.gitDirForOps, sess.Root, log); err != nil {
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

	exportFile, err := os.CreateTemp(sess.Root, "persona-export-*.patch")
	if err != nil {
		return model.Wrap(model.ExitExport, "create export temp", err), 0
	}
	defer func() {
		name := exportFile.Name()
		_ = exportFile.Close()
		_ = os.Remove(name)
	}()
	written, err := exportPatchToWriter(postCtx, g, repoRoot, menv.gitDirForOps, patchInRepo, patchRel, opts.IgnoredMode, opts.IgnoredMax, initialIgnored, exportFile)
	if err != nil {
		return model.Wrap(model.ExitExport, "export patch", err), 0
	}
	log.Debug("export complete", "bytes", written)
	if _, err := exportFile.Seek(0, 0); err != nil {
		return model.Wrap(model.ExitWrite, "rewind export patch", err), 0
	}
	if err := patchStore.WriteFromReader(exportFile); err != nil {
		return model.Wrap(model.ExitWrite, "write patch", err), 0
	}

	if opts.PrintPatchPath {
		fmt.Fprintln(os.Stdout, patchPathEffective)
	}
	return nil, childCode
}
