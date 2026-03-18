package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"persona/internal/model"
	"persona/internal/session"
)

func prepareBase(ctx context.Context, g model.GitOps, opts model.Options, sess *session.Session, patchInRepo bool, patchRel string, cleanup *cleanupStack, retErr *error) (basePath string, err error) {
	switch opts.BaseMode {
	case model.BaseRepo:
		if !opts.AllowDirty {
			excludePaths := []string{}
			if patchInRepo && patchRel != "" && patchRel != "." {
				excludePaths = append(excludePaths, patchRel, patchRel+".lock")
			}
			clean, err := g.IsCleanExceptPaths(ctx, excludePaths)
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

type mountEnv struct {
	emptyDir     string
	emptyFile    string
	gitDirForOps string
}

func setupMountEnv(repoRoot, gitDir, basePath string, sess *session.Session, mount model.NSOps, cleanup *cleanupStack, log *slog.Logger) (*mountEnv, error) {
	extRoot, err := os.MkdirTemp("", "persona-session-")
	if err != nil {
		return nil, model.Wrap(model.ExitEnv, "create external session dir", err)
	}
	cleanup.Push(func() error { return os.RemoveAll(extRoot) })

	env := &mountEnv{
		emptyDir:     filepath.Join(extRoot, "empty", "dirs"),
		emptyFile:    filepath.Join(extRoot, "empty", "files"),
		gitDirForOps: filepath.Join(extRoot, "mnt", "gitdir"),
	}
	if err := os.MkdirAll(env.emptyDir, 0o755); err != nil {
		return nil, model.Wrap(model.ExitEnv, "mkdir empty dir", err)
	}
	if err := os.MkdirAll(env.emptyFile, 0o755); err != nil {
		return nil, model.Wrap(model.ExitEnv, "mkdir empty file dir", err)
	}
	if err := os.MkdirAll(filepath.Dir(env.gitDirForOps), 0o755); err != nil {
		return nil, model.Wrap(model.ExitEnv, "mkdir git mount dir", err)
	}

	if err := mount.BindMount(gitDir, env.gitDirForOps); err != nil {
		return nil, model.Wrap(model.ExitEnv, "bind mount gitdir", err)
	}
	cleanup.Push(func() error { return mount.Umount(env.gitDirForOps) })

	if shouldForceMountFail() {
		return nil, model.Wrap(model.ExitEnv, "forced mount failure", fmt.Errorf("forced"))
	}

	if err := mount.BindMount(basePath, sess.MntBase); err != nil {
		return nil, model.Wrap(model.ExitEnv, "bind mount base", err)
	}
	cleanup.Push(func() error { return mount.Umount(sess.MntBase) })

	if err := mount.MountOverlay(repoRoot, model.OverlayOpts{
		LowerDir: sess.MntBase,
		UpperDir: sess.Upper,
		WorkDir:  sess.Work,
	}); err != nil {
		return nil, model.Wrap(model.ExitEnv, "mount overlay", err)
	}
	log.Debug("overlay mounted", "repo", repoRoot, "base", basePath)

	return env, nil
}

func prepareMaskBacking(emptyFileRoot, emptyDirRoot, target string) (string, string, error) {
	sum := sha256.Sum256([]byte(target))
	name := hex.EncodeToString(sum[:8])
	emptyFile := filepath.Join(emptyFileRoot, name)
	emptyDir := filepath.Join(emptyDirRoot, name)
	if _, err := os.Stat(emptyFile); err != nil {
		if !os.IsNotExist(err) {
			return "", "", err
		}
		file, err := os.OpenFile(emptyFile, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return "", "", err
		}
		if err := file.Close(); err != nil {
			return "", "", err
		}
	}
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		return "", "", err
	}
	return emptyFile, emptyDir, nil
}

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
				if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
					continue
				}
				return targets, ignored, fmt.Errorf("stat ignored masked %s: %w", path, err)
			}
			kind := model.MaskDir
			if !info.IsDir() {
				kind = model.MaskFile
			}
			maskEmptyFile, maskEmptyDir, err := prepareMaskBacking(extEmptyFile, extEmptyDir, target)
			if err != nil {
				return targets, ignored, fmt.Errorf("prepare mask backing %s: %w", path, err)
			}
			if err := mount.MaskPath(target, kind, maskEmptyFile, maskEmptyDir); err != nil {
				return targets, ignored, fmt.Errorf("mask ignored %s: %w", path, err)
			}
			targets = append(targets, target)
		}
	}
	return targets, ignored, nil
}
