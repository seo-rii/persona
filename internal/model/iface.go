package model

import (
	"context"
	"io"
)

// GitOps abstracts Git operations used by the overlay orchestration layer.
// Implement this interface with a mock/stub to unit-test runWithOptions logic
// without a real repository.
type GitOps interface {
	// Repo identity
	RepoRootPath() string
	GitDirPath() string

	// Working-tree status
	IsCleanExceptUntracked(ctx context.Context, ignoreUntracked []string) (bool, error)
	IsCleanExceptPaths(ctx context.Context, excludePaths []string) (bool, error)

	// Worktree management
	WorktreeAddDetach(ctx context.Context, path, ref string) error
	WorktreeRemoveForce(ctx context.Context, path string) error

	// Patch application
	ApplyPatch(ctx context.Context, mode ApplyMode, workTree, gitDir string, patchData []byte) error
	ApplyPatchReader(ctx context.Context, mode ApplyMode, workTree, gitDir string, patchReader io.Reader) error

	// Diff / export
	DiffHeadBinary(ctx context.Context, workTree, gitDir string, excludePaths []string) ([]byte, error)
	DiffHeadBinaryTo(ctx context.Context, workTree, gitDir string, excludePaths []string, out io.Writer) error
	ListUntracked(ctx context.Context, workTree, gitDir string) ([]string, error)
	DiffNewFileNoIndex(ctx context.Context, workTree, gitDir, relPath string) ([]byte, error)
	DiffNewFileNoIndexTo(ctx context.Context, workTree, gitDir, relPath string, out io.Writer) error

	// Ignored files
	ListIgnoredCandidates(ctx context.Context, workTree, gitDir string, maxN int) ([]string, error)
}

// MaskKind distinguishes file-type bind-mount masks.
type MaskKind int

const (
	MaskFile MaskKind = iota
	MaskDir
)

// OverlayOpts configures an OverlayFS mount.
type OverlayOpts struct {
	LowerDir string
	UpperDir string
	WorkDir  string
}

// NSOps abstracts mount-namespace operations used by the overlay orchestration
// layer.  Implement this interface with no-ops or temp-dir fakes to test
// overlay logic without CAP_SYS_ADMIN.
type NSOps interface {
	UnshareMountNS() error
	MakeMountsPrivate() error
	BindMount(src, dst string) error
	RemountRO(target string) error
	Umount(target string) error
	MountOverlay(target string, opts OverlayOpts) error
	MaskPath(target string, kind MaskKind, emptyFile, emptyDir string) error
}
