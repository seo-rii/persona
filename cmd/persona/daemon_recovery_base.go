package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"persona/internal/gitx"
)

// A retained upper can only be reopened over the exact lower tree it used
// before the crash. Ownership markers identify the session, not its contents.
func validateRecoveryBase(ctx context.Context, basePath, expectedOID string) error {
	if expectedOID == "" {
		return fmt.Errorf("preserved worktree has no immutable base OID")
	}
	info, err := os.Lstat(basePath)
	if err != nil {
		return fmt.Errorf("inspect preserved worktree: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("preserved worktree is not a real directory: %s", basePath)
	}
	repoRoot, gitDir, err := gitx.DetectRepo(ctx, basePath)
	if err != nil {
		return fmt.Errorf("detect preserved worktree: %w", err)
	}
	baseReal, err := filepath.EvalSymlinks(basePath)
	if err != nil {
		return fmt.Errorf("resolve preserved worktree: %w", err)
	}
	repoReal, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return fmt.Errorf("resolve preserved repository: %w", err)
	}
	if baseReal != repoReal {
		return fmt.Errorf("preserved worktree is no longer its own repository: %s", basePath)
	}
	g := &gitx.Git{RepoRoot: repoRoot, GitDir: gitDir}
	actualOID, err := g.ResolveBase(ctx, "HEAD")
	if err != nil {
		return fmt.Errorf("resolve preserved worktree HEAD: %w", err)
	}
	if actualOID != expectedOID {
		return fmt.Errorf("preserved worktree HEAD differs from base %s: %s", expectedOID, actualOID)
	}
	// The storage pipeline uses a fresh pinned index and checks raw bytes.
	// A plain git status can hide edits behind assume-unchanged, skip-worktree,
	// core.filemode, textconv, or content normalization settings.
	written, err := exportPatchToWriter(ctx, g, basePath, gitDir, false, "", 0, nil, io.Discard)
	if err != nil {
		return fmt.Errorf("validate preserved worktree contents: %w", err)
	}
	if written != 0 {
		return fmt.Errorf("preserved worktree contents differ from base %s", expectedOID)
	}
	// Index drift and ignored additions are absent from an otherwise empty
	// stored patch, but must not silently become part of the recovered lower.
	clean, err := g.IsClean(ctx)
	if err != nil {
		return fmt.Errorf("validate preserved worktree index: %w", err)
	}
	if !clean {
		return fmt.Errorf("preserved worktree index or untracked paths have changed")
	}
	// Unlike status --ignored, ls-files without exclude rules includes both
	// untracked and ignored paths regardless of status.showUntrackedFiles.
	var extra bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "--work-tree", basePath, "ls-files", "--others", "--directory", "-z")
	cmd.Dir = basePath
	cmd.Env = gitx.FilterGitEnv(os.Environ())
	cmd.Stdout = &patchLimitWriter{w: &extra}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("inspect preserved untracked and ignored files: %w", err)
	}
	if extra.Len() != 0 {
		first, _, _ := bytes.Cut(extra.Bytes(), []byte{0})
		return fmt.Errorf("preserved worktree has untracked or ignored additions: %q", first)
	}
	return nil
}
