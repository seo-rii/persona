package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"persona/internal/gitx"
	"persona/internal/model"
	"persona/internal/patchio"
	"persona/internal/testutil"
)

func TestExportPatchSortAndExclude(t *testing.T) {
	repo := testutil.InitRepo(t)
	testutil.WriteFile(t, filepath.Join(repo, "b.txt"), "b\n")
	testutil.WriteFile(t, filepath.Join(repo, "a.txt"), "a\n")
	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "seed\n")

	g := &gitx.Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	patch1, err := exportPatch(context.Background(), g, repo, g.GitDir, true, "state.patch", model.IgnoredTransparent, 0, nil)
	if err != nil {
		t.Fatalf("exportPatch error: %v", err)
	}
	if bytes.Contains(patch1, []byte("state.patch")) {
		t.Fatalf("expected excluded patch file")
	}
	aIdx := bytes.Index(patch1, []byte("+++ b/a.txt"))
	bIdx := bytes.Index(patch1, []byte("+++ b/b.txt"))
	if aIdx == -1 || bIdx == -1 {
		t.Fatalf("missing expected files in patch: %s", string(patch1))
	}
	if aIdx > bIdx {
		t.Fatalf("expected deterministic sorted order a.txt before b.txt")
	}
	if bytes.Contains(patch1, []byte("+++ b/.git/")) {
		t.Fatalf("unexpected .git path in patch")
	}

	patch2, err := exportPatch(context.Background(), g, repo, g.GitDir, true, "state.patch", model.IgnoredTransparent, 0, nil)
	if err != nil {
		t.Fatalf("exportPatch second call error: %v", err)
	}
	if !bytes.Equal(patch1, patch2) {
		t.Fatalf("expected deterministic output across runs")
	}
}

func TestExportPatchIncludesPatchFileWhenNotExcluded(t *testing.T) {
	repo := testutil.InitRepo(t)
	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "seed\n")

	g := &gitx.Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	patch, err := exportPatch(context.Background(), g, repo, g.GitDir, false, "", model.IgnoredTransparent, 0, nil)
	if err != nil {
		t.Fatalf("exportPatch error: %v", err)
	}
	if !bytes.Contains(patch, []byte("state.patch")) {
		t.Fatalf("expected state.patch to be included when not excluded")
	}
}

func TestExportPatchSkipsVanishedUntrackedFiles(t *testing.T) {
	repo := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repo, "keep.txt"), "keep\n")
	g := exportGitOps{
		untracked: []string{"gone.txt", "keep.txt"},
		diffByPath: map[string][]byte{
			"keep.txt": []byte("diff --git a/keep.txt b/keep.txt\n+++ b/keep.txt\n"),
		},
		errByPath: map[string]error{
			"gone.txt": os.ErrNotExist,
		},
	}

	patch, err := exportPatch(context.Background(), g, repo, "", false, "", model.IgnoredTransparent, 0, nil)
	if err != nil {
		t.Fatalf("exportPatch error: %v", err)
	}
	if !bytes.Contains(patch, []byte("keep.txt")) {
		t.Fatalf("expected surviving untracked file in patch: %s", string(patch))
	}
	if bytes.Contains(patch, []byte("gone.txt")) {
		t.Fatalf("expected vanished untracked file to be skipped: %s", string(patch))
	}
}

func TestExportPatchFailsOnNewIgnoredCandidates(t *testing.T) {
	g := exportGitOps{
		ignored: []string{"late-ignored.txt"},
	}

	_, err := exportPatch(context.Background(), g, t.TempDir(), "", false, "", model.IgnoredReadonly, 1, nil)
	if err == nil {
		t.Fatalf("expected error when new ignored candidates appear after child run")
	}
	if !strings.Contains(err.Error(), "late-ignored.txt") {
		t.Fatalf("expected ignored path in error, got %v", err)
	}
}

func TestExportPatchFailsOnNewIgnoredCandidatesInTransparentMode(t *testing.T) {
	g := exportGitOps{
		ignored: []string{"late-ignored.txt"},
	}

	_, err := exportPatch(context.Background(), g, t.TempDir(), "", false, "", model.IgnoredTransparent, 1, nil)
	if err == nil {
		t.Fatal("expected error when transparent mode ignored set changes after child run")
	}
	if !strings.Contains(err.Error(), "late-ignored.txt") {
		t.Fatalf("expected ignored path in error, got %v", err)
	}
}

func TestExportPatchFailsWhenIgnoredCandidateCapExceeded(t *testing.T) {
	g := exportGitOps{
		ignored: []string{"a.tmp", "b.tmp", "c.tmp"},
	}

	_, err := exportPatch(context.Background(), g, t.TempDir(), "", false, "", model.IgnoredReadonly, 2, []string{"a.tmp", "b.tmp"})
	if err == nil {
		t.Fatal("expected ignored-max overflow to fail")
	}
	if !strings.Contains(err.Error(), "ignored-max 2") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExportPatchIgnoredDriftDetectsNewCandidatesWithinIgnoredMax(t *testing.T) {
	g := exportGitOps{
		ignored: []string{"a.tmp", "b.tmp"},
	}

	_, err := exportPatch(context.Background(), g, t.TempDir(), "", false, "", model.IgnoredReadonly, 2, []string{"a.tmp"})
	if err == nil {
		t.Fatalf("expected new ignored candidate within ignored-max to fail")
	}
	if !strings.Contains(err.Error(), "b.tmp") {
		t.Fatalf("expected new ignored path in error, got %v", err)
	}
}

func TestExportPatchFailsOnIgnoredToUnignoredTransition(t *testing.T) {
	g := exportGitOps{
		ignored: []string{"keep.tmp"},
	}

	_, err := exportPatch(context.Background(), g, t.TempDir(), "", false, "", model.IgnoredReadonly, 2, []string{"keep.tmp", "gone.tmp"})
	if err == nil {
		t.Fatal("expected ignored-to-unignored transition to fail")
	}
	if !strings.Contains(err.Error(), "gone.tmp") || !strings.Contains(err.Error(), "no longer ignored") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExportPatchIgnoredMaxZeroSkipsIgnoredDriftCheck(t *testing.T) {
	g := exportGitOps{
		ignoredErr: errors.New("should not list ignored candidates"),
	}

	patch, err := exportPatch(context.Background(), g, t.TempDir(), "", false, "", model.IgnoredReadonly, 0, nil)
	if err != nil {
		t.Fatalf("exportPatch error: %v", err)
	}
	if len(patch) != 0 {
		t.Fatalf("expected empty patch, got %q", string(patch))
	}
}

func TestExportPatchWarnsAndSkipsSpecialFiles(t *testing.T) {
	repo := t.TempDir()
	fifoPath := filepath.Join(repo, "fifo.pipe")
	if err := syscall.Mkfifo(fifoPath, 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	socketPath := filepath.Join(repo, "socket.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer listener.Close()

	g := exportGitOps{untracked: []string{"fifo.pipe", "socket.sock"}}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe stderr: %v", err)
	}
	oldStderr := os.Stderr
	os.Stderr = stderrW
	patch, patchErr := exportPatch(context.Background(), g, repo, "", false, "", model.IgnoredTransparent, 0, nil)
	_ = stderrW.Close()
	os.Stderr = oldStderr
	warn, readErr := io.ReadAll(stderrR)
	_ = stderrR.Close()
	if patchErr != nil {
		t.Fatalf("exportPatch error: %v", patchErr)
	}
	if readErr != nil {
		t.Fatalf("read stderr: %v", readErr)
	}
	if bytes.Contains(patch, []byte("fifo.pipe")) || bytes.Contains(patch, []byte("socket.sock")) {
		t.Fatalf("expected special files to be skipped from patch: %s", string(patch))
	}
	text := string(warn)
	if !strings.Contains(text, "skip special file fifo.pipe") {
		t.Fatalf("expected fifo warning, got %q", text)
	}
	if !strings.Contains(text, "skip special file socket.sock") {
		t.Fatalf("expected socket warning, got %q", text)
	}
}

func TestBinaryNewFileRoundTrip(t *testing.T) {
	repo := testutil.InitRepo(t)
	want := []byte{0x00, 0x01, 0x02, 0x03, 0x10, 0x20, 0x7f, 0xff}
	if err := os.WriteFile(filepath.Join(repo, "binary.dat"), want, 0o644); err != nil {
		t.Fatalf("write binary.dat: %v", err)
	}
	g := &gitx.Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	patch, err := exportPatch(context.Background(), g, repo, g.GitDir, false, "", model.IgnoredTransparent, 0, nil)
	if err != nil {
		t.Fatalf("exportPatch error: %v", err)
	}
	if !bytes.Contains(patch, []byte("binary.dat")) {
		t.Fatalf("expected binary.dat in patch")
	}

	applyRepo := testutil.InitRepo(t)
	applyGit := &gitx.Git{RepoRoot: applyRepo, GitDir: filepath.Join(applyRepo, ".git")}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := applyPatchData(context.Background(), applyGit, model.ApplyStrict, patch, applyRepo, applyGit.GitDir, log); err != nil {
		t.Fatalf("applyPatchData error: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(applyRepo, "binary.dat"))
	if err != nil {
		t.Fatalf("read applied binary.dat: %v", err)
	}
	if sha256.Sum256(got) != sha256.Sum256(want) {
		t.Fatalf("binary roundtrip mismatch: got=%x want=%x", got, want)
	}
}

func TestExportPatchFailsWhenAccumulatedDiffExceedsSizeLimit(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "late.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write late.txt: %v", err)
	}
	g := exportGitOps{
		tracked:   bytes.Repeat([]byte("t"), patchio.MaxPatchBytes-8),
		untracked: []string{"late.txt"},
		diffByPath: map[string][]byte{
			"late.txt": bytes.Repeat([]byte("u"), 16),
		},
	}

	_, err := exportPatch(context.Background(), g, repo, "", false, "", model.IgnoredTransparent, 0, nil)
	if err == nil {
		t.Fatal("expected accumulated patch size overflow to fail")
	}
	if !strings.Contains(err.Error(), "patch exceeds size limit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestExportPatchExcludesTrackedPatchState(t *testing.T) {
	repo := testutil.InitRepo(t)
	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "seed\n")
	testutil.WriteFile(t, filepath.Join(repo, "state.patch.lock"), "lock\n")
	testutil.RunCmd(t, repo, "git", "add", "state.patch")
	testutil.RunCmd(t, repo, "git", "add", "state.patch.lock")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "track patch")
	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "updated\n")
	testutil.WriteFile(t, filepath.Join(repo, "state.patch.lock"), "updated lock\n")
	testutil.WriteFile(t, filepath.Join(repo, "other.txt"), "other\n")

	g := &gitx.Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	patch, err := exportPatch(context.Background(), g, repo, g.GitDir, true, "state.patch", model.IgnoredTransparent, 0, nil)
	if err != nil {
		t.Fatalf("exportPatch error: %v", err)
	}
	if bytes.Contains(patch, []byte("state.patch")) {
		t.Fatalf("expected tracked patch file to be excluded from export: %s", string(patch))
	}
	if bytes.Contains(patch, []byte("state.patch.lock")) {
		t.Fatalf("expected tracked patch lock file to be excluded from export: %s", string(patch))
	}
	if !bytes.Contains(patch, []byte("other.txt")) {
		t.Fatalf("expected other changes to remain in export: %s", string(patch))
	}
}

func TestExportPatchExcludesUntrackedPatchLockFile(t *testing.T) {
	repo := testutil.InitRepo(t)
	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "")
	testutil.WriteFile(t, filepath.Join(repo, "state.patch.lock"), "lock\n")
	testutil.WriteFile(t, filepath.Join(repo, "other.txt"), "other\n")

	g := &gitx.Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	patch, err := exportPatch(context.Background(), g, repo, g.GitDir, true, "state.patch", model.IgnoredTransparent, 0, nil)
	if err != nil {
		t.Fatalf("exportPatch error: %v", err)
	}
	if bytes.Contains(patch, []byte("state.patch.lock")) {
		t.Fatalf("expected untracked patch lock file to be excluded from export: %s", string(patch))
	}
	if !bytes.Contains(patch, []byte("other.txt")) {
		t.Fatalf("expected other changes to remain in export: %s", string(patch))
	}
}
