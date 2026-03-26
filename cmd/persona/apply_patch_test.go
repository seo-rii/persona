package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"persona/internal/gitx"
	"persona/internal/model"
	"persona/internal/testutil"
)

func TestApplyPatchDataRetriesWithoutEnglishErrorString(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repoRoot, "same.txt"), "same\n")
	patch := strings.Join([]string{
		"diff --git a/same.txt b/same.txt",
		"new file mode 100644",
		"index 0000000..2e65efe",
		"--- /dev/null",
		"+++ b/same.txt",
		"@@ -0,0 +1 @@",
		"+same",
		"",
	}, "\n")
	g := &applyRetryGitOps{
		applyErrs: []error{errors.New("Datei existiert bereits")},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(context.Background(), g, model.ApplyStrict, []byte(patch), repoRoot, "", log)
	if err != nil {
		t.Fatalf("expected identical existing new file to be skipped despite localized error, got %v", err)
	}
	if g.applyCalls != 1 {
		t.Fatalf("expected a single apply attempt when filtered patch is empty, got %d", g.applyCalls)
	}
}

func TestApplyPatchDataDoesNotRetryForUnrelatedApplyError(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repoRoot, "same.txt"), "same\n")
	patch := strings.Join([]string{
		"diff --git a/same.txt b/same.txt",
		"new file mode 100644",
		"index 0000000..2e65efe",
		"--- /dev/null",
		"+++ b/same.txt",
		"@@ -0,0 +1 @@",
		"+same",
		"diff --git a/other.txt b/other.txt",
		"new file mode 100644",
		"index 0000000..3e75765",
		"--- /dev/null",
		"+++ b/other.txt",
		"@@ -0,0 +1 @@",
		"+other",
		"",
	}, "\n")
	wantErr := errors.New("patch does not apply")
	g := &applyRetryGitOps{
		applyErrs: []error{wantErr, nil},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(context.Background(), g, model.ApplyStrict, []byte(patch), repoRoot, "", log)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected original error %v, got %v", wantErr, err)
	}
	if g.applyCalls != 1 {
		t.Fatalf("expected a single apply attempt, got %d", g.applyCalls)
	}
}

func TestApplyPatchDataRejectModeDoesNotRetryExistingNewFileSkip(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repoRoot, "same.txt"), "same\n")
	patch := strings.Join([]string{
		"diff --git a/same.txt b/same.txt",
		"new file mode 100644",
		"index 0000000..2e65efe",
		"--- /dev/null",
		"+++ b/same.txt",
		"@@ -0,0 +1 @@",
		"+same",
		"",
	}, "\n")
	wantErr := errors.New("same.txt: already exists in working directory")
	g := &applyRetryGitOps{
		applyErrs: []error{wantErr, nil},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(context.Background(), g, model.ApplyReject, []byte(patch), repoRoot, "", log)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected original error %v, got %v", wantErr, err)
	}
	if g.applyCalls != 1 {
		t.Fatalf("expected a single apply attempt, got %d", g.applyCalls)
	}
}

func TestApplyPatchDataRejectsUnsafePathBeforeApply(t *testing.T) {
	g := &applyRetryGitOps{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(
		context.Background(),
		g,
		model.ApplyStrict,
		[]byte("diff --git a/../evil b/../evil\n"),
		t.TempDir(),
		"",
		log,
	)
	if err == nil {
		t.Fatal("expected error")
	}
	if g.applyCalls != 0 {
		t.Fatalf("expected no apply attempts, got %d", g.applyCalls)
	}
}

func TestApplyPatchDataReturnsOriginalErrorWhenNothingWasSkipped(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repoRoot, "tracked.txt"), "before\n")
	patch := strings.Join([]string{
		"diff --git a/tracked.txt b/tracked.txt",
		"index df967b9..3b18e51 100644",
		"--- a/tracked.txt",
		"+++ b/tracked.txt",
		"@@ -1 +1 @@",
		"-before",
		"+after",
		"",
	}, "\n")
	wantErr := errors.New("apply failed")
	g := &applyRetryGitOps{applyErrs: []error{wantErr}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(context.Background(), g, model.ApplyStrict, []byte(patch), repoRoot, "", log)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected original error %v, got %v", wantErr, err)
	}
	if g.applyCalls != 1 {
		t.Fatalf("expected one apply attempt, got %d", g.applyCalls)
	}
}

func TestApplyPatchDataReturnsRetryError(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repoRoot, "same.txt"), "same\n")
	patch := strings.Join([]string{
		"diff --git a/same.txt b/same.txt",
		"new file mode 100644",
		"index 0000000..2e65efe",
		"--- /dev/null",
		"+++ b/same.txt",
		"@@ -0,0 +1 @@",
		"+same",
		"diff --git a/other.txt b/other.txt",
		"new file mode 100644",
		"index 0000000..3e75765",
		"--- /dev/null",
		"+++ b/other.txt",
		"@@ -0,0 +1 @@",
		"+other",
		"",
	}, "\n")
	firstErr := errors.New("other.txt: already exists in working directory")
	retryErr := errors.New("retry failed")
	g := &applyRetryGitOps{applyErrs: []error{firstErr, retryErr}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(context.Background(), g, model.ApplyStrict, []byte(patch), repoRoot, "", log)
	if !errors.Is(err, retryErr) {
		t.Fatalf("expected retry error %v, got %v", retryErr, err)
	}
	if g.applyCalls != 2 {
		t.Fatalf("expected two apply attempts, got %d", g.applyCalls)
	}
}

func TestApplyPatchDataEmptyPatchIsNoop(t *testing.T) {
	g := &applyRetryGitOps{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(context.Background(), g, model.ApplyStrict, nil, t.TempDir(), "", log)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if g.applyCalls != 0 {
		t.Fatalf("expected no apply attempts, got %d", g.applyCalls)
	}
}

func TestApplyPatchDataRejectModeLeavesRejectAndPartialApply(t *testing.T) {
	const base = "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nline10\n"
	const changed = "LINE1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nLINE10\n"
	const conflicted = "line1\nline2\nline3\nline4\nline5\nline6\nline7\nline8\nline9\nconflict10\n"

	makeRepo := func(t *testing.T, content string) string {
		t.Helper()
		repo := testutil.InitEmptyRepo(t)
		testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), content)
		testutil.RunCmd(t, repo, "git", "add", "tracked.txt")
		testutil.RunCmd(t, repo, "git", "commit", "-m", "baseline")
		return repo
	}

	patchRepo := makeRepo(t, base)
	testutil.WriteFile(t, filepath.Join(patchRepo, "tracked.txt"), changed)
	cmd := exec.Command("git", "diff", "--binary", "--full-index", "HEAD")
	cmd.Dir = patchRepo
	patch, err := cmd.Output()
	if err != nil {
		t.Fatalf("git diff: %v", err)
	}

	strictRepo := makeRepo(t, base)
	testutil.WriteFile(t, filepath.Join(strictRepo, "tracked.txt"), conflicted)
	strictGit := &gitx.Git{RepoRoot: strictRepo, GitDir: filepath.Join(strictRepo, ".git")}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := applyPatchData(context.Background(), strictGit, model.ApplyStrict, patch, strictRepo, strictGit.GitDir, log); err == nil {
		t.Fatal("expected strict apply to fail on conflicted hunk")
	}
	strictText, err := os.ReadFile(filepath.Join(strictRepo, "tracked.txt"))
	if err != nil {
		t.Fatalf("read strict tracked.txt: %v", err)
	}
	if strings.Contains(string(strictText), "LINE1") {
		t.Fatalf("strict apply must not partially apply hunks: %s", string(strictText))
	}

	rejectRepo := makeRepo(t, base)
	testutil.WriteFile(t, filepath.Join(rejectRepo, "tracked.txt"), conflicted)
	rejectGit := &gitx.Git{RepoRoot: rejectRepo, GitDir: filepath.Join(rejectRepo, ".git")}
	err = applyPatchData(context.Background(), rejectGit, model.ApplyReject, patch, rejectRepo, rejectGit.GitDir, log)
	if err == nil {
		t.Fatal("expected reject apply to report reject output")
	}
	rejectText, err := os.ReadFile(filepath.Join(rejectRepo, "tracked.txt"))
	if err != nil {
		t.Fatalf("read reject tracked.txt: %v", err)
	}
	if !strings.Contains(string(rejectText), "LINE1") {
		t.Fatalf("reject apply must keep applied hunk: %s", string(rejectText))
	}
	if !strings.Contains(string(rejectText), "conflict10") {
		t.Fatalf("reject apply must keep conflicted line: %s", string(rejectText))
	}
	rej, err := os.ReadFile(filepath.Join(rejectRepo, "tracked.txt.rej"))
	if err != nil {
		t.Fatalf("read reject file: %v", err)
	}
	if !bytes.Contains(rej, []byte("LINE10")) {
		t.Fatalf("expected rejected hunk in .rej, got %s", string(rej))
	}
}
