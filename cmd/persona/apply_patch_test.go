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
	"persona/internal/patchio"
	"persona/internal/testutil"
)

type taggedAlreadyExistsError struct {
	msg string
}

func (e taggedAlreadyExistsError) Error() string {
	return e.msg
}

func (e taggedAlreadyExistsError) AlreadyExists() bool {
	return true
}

func TestOpenPatchStoreReadAllowsInitialMissingPatch(t *testing.T) {
	store, err := patchio.OpenPatchStore(filepath.Join(t.TempDir(), "missing.patch"))
	if err != nil {
		t.Fatalf("open patch store: %v", err)
	}
	defer store.Close()

	file, err := openPatchStoreRead(store, "initial read", true)
	if err != nil {
		t.Fatalf("expected missing initial patch to be allowed, got %v", err)
	}
	if file != nil {
		_ = file.Close()
		t.Fatal("expected nil file for missing initial patch")
	}
}

func TestOpenPatchStoreReadRejectsMissingAfterFirstSuccessfulOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.patch")
	if err := os.WriteFile(path, []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write patch: %v", err)
	}
	store, err := patchio.OpenPatchStore(path)
	if err != nil {
		t.Fatalf("open patch store: %v", err)
	}
	defer store.Close()

	file, err := openPatchStoreRead(store, "initial read", true)
	if err != nil {
		t.Fatalf("expected first open to succeed, got %v", err)
	}
	if file == nil {
		t.Fatal("expected first open to return a file")
	}
	_ = file.Close()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove patch: %v", err)
	}

	file, err = openPatchStoreRead(store, "validation reopen", false)
	if err == nil || !strings.Contains(err.Error(), "patch disappeared during validation reopen") {
		if file != nil {
			_ = file.Close()
		}
		t.Fatalf("expected disappearance error on reopen, got file=%v err=%v", file, err)
	}
}

func TestRetryFilteredExistingNewFilesReturnsOriginalErrorWhenNothingWasSkipped(t *testing.T) {
	wantErr := errors.New("apply failed")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	retried := false

	err := retryFilteredExistingNewFiles(model.ApplyStrict, wantErr, nil, log,
		func() (bool, error) {
			t.Fatal("filteredEmpty callback should stay unused when nothing was skipped")
			return false, nil
		},
		func() error {
			retried = true
			return nil
		},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected original error %v, got %v", wantErr, err)
	}
	if retried {
		t.Fatal("expected retry callback to stay unused")
	}
}

func TestRetryFilteredExistingNewFilesReturnsRetryError(t *testing.T) {
	retryErr := errors.New("retry failed")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := retryFilteredExistingNewFiles(
		model.ApplyStrict,
		taggedAlreadyExistsError{msg: "same.txt: already exists in working directory"},
		[]string{"same.txt"},
		log,
		func() (bool, error) {
			return false, nil
		},
		func() error {
			return retryErr
		},
	)
	if !errors.Is(err, retryErr) {
		t.Fatalf("expected retry error %v, got %v", retryErr, err)
	}
}

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

func TestApplyPatchDataRetriesTaggedAlreadyExistsErrorWithMixedPatch(t *testing.T) {
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
	var filtered bytes.Buffer
	skipped, ferr := patchio.FilterExistingNewFilesReader(strings.NewReader(patch), repoRoot, &filtered)
	if ferr != nil {
		t.Fatalf("FilterExistingNewFiles error: %v", ferr)
	}
	if len(skipped) != 1 || skipped[0] != "same.txt" {
		t.Fatalf("expected only same.txt to be skipped, got %v", skipped)
	}
	if !bytes.Contains(filtered.Bytes(), []byte("other.txt")) {
		t.Fatalf("expected filtered patch to keep other.txt block, got %q", filtered.String())
	}
	if !patchio.IsAlreadyExistsError(taggedAlreadyExistsError{msg: "same.txt: 작업 디렉터리에 이미 있습니다"}) {
		t.Fatal("tagged already-exists marker must be accepted without relying on localized substrings")
	}
	g := &applyRetryGitOps{
		applyErrs: []error{taggedAlreadyExistsError{msg: "same.txt: 작업 디렉터리에 이미 있습니다"}, nil},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(context.Background(), g, model.ApplyStrict, []byte(patch), repoRoot, "", log)
	if err != nil {
		t.Fatalf("expected tagged already-exists error to retry filtered mixed patch, got %v", err)
	}
	if g.applyCalls != 2 {
		t.Fatalf("expected two apply attempts, got %d", g.applyCalls)
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

func TestApplyPatchDataRetriesWhenLocalizedExistingFileErrorLeavesFilteredPatch(t *testing.T) {
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
		"index 0000000..e45c9c2",
		"--- /dev/null",
		"+++ b/other.txt",
		"@@ -0,0 +1 @@",
		"+other",
		"",
	}, "\n")
	g := &applyRetryGitOps{
		applyErrs: []error{errors.New("Datei existiert bereits"), nil},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(context.Background(), g, model.ApplyStrict, []byte(patch), repoRoot, "", log)
	if err != nil {
		t.Fatalf("expected localized existing-file failure to retry filtered patch, got %v", err)
	}
	if g.applyCalls != 2 {
		t.Fatalf("expected two apply attempts, got %d", g.applyCalls)
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

func TestApplyPatchDataRetriesWhenLocalizedErrorNamesSkippedPath(t *testing.T) {
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
		"index 0000000..e45c9c2",
		"--- /dev/null",
		"+++ b/other.txt",
		"@@ -0,0 +1 @@",
		"+other",
		"",
	}, "\n")
	g := &applyRetryGitOps{
		applyErrs: []error{errors.New("same.txt: Datei existiert bereits"), nil},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(context.Background(), g, model.ApplyStrict, []byte(patch), repoRoot, "", log)
	if err != nil {
		t.Fatalf("expected localized already-exists retry to succeed, got %v", err)
	}
	if g.applyCalls != 2 {
		t.Fatalf("expected two apply attempts, got %d", g.applyCalls)
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

func TestApplyPatchDataDoesNotSwallowUnrelatedErrorWhenFilteredPatchBecomesEmpty(t *testing.T) {
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
	wantErr := errors.New("patch does not apply")
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

func TestApplyPatchDataDoesNotRetryForUnrelatedErrorThatMentionsSkippedPath(t *testing.T) {
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
	wantErr := errors.New("same.txt: patch does not apply")
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

func TestApplyPatchDataRejectsPatchOverSizeLimitBeforeGitCall(t *testing.T) {
	g := &applyRetryGitOps{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err := applyPatchData(
		context.Background(),
		g,
		model.ApplyStrict,
		bytes.Repeat([]byte("a"), patchio.MaxPatchBytes+1),
		t.TempDir(),
		"",
		log,
	)
	if err == nil {
		t.Fatal("expected oversize patch to be rejected")
	}
	if g.applyCalls != 0 {
		t.Fatalf("expected no apply attempts, got %d", g.applyCalls)
	}
	if !strings.Contains(err.Error(), "patch exceeds size limit") {
		t.Fatalf("unexpected error: %v", err)
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

func TestApplyPatchStoreUsesReaderApplyPath(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repoRoot, "same.txt"), "same\n")
	patchPath := filepath.Join(t.TempDir(), "state.patch")
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
	if err := os.WriteFile(patchPath, []byte(patch), 0o644); err != nil {
		t.Fatalf("write patch: %v", err)
	}
	store, err := patchio.OpenPatchStore(patchPath)
	if err != nil {
		t.Fatalf("open patch store: %v", err)
	}
	defer store.Close()

	g := &applyRetryGitOps{applyReaderErrs: []error{errors.New("Datei existiert bereits")}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err = applyPatchStore(context.Background(), g, model.ApplyStrict, store, repoRoot, "", t.TempDir(), log)
	if err != nil {
		t.Fatalf("expected identical existing new file to be skipped, got %v", err)
	}
	if g.applyCalls != 0 {
		t.Fatalf("expected byte apply path to stay unused, got %d calls", g.applyCalls)
	}
	if g.applyReaderCalls != 1 {
		t.Fatalf("expected one reader apply attempt, got %d", g.applyReaderCalls)
	}
}

func TestApplyPatchStoreUsesSnapshotAfterStoreClosedDuringFallback(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repoRoot, "same.txt"), "same\n")
	patchPath := filepath.Join(t.TempDir(), "state.patch")
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
	if err := os.WriteFile(patchPath, []byte(patch), 0o644); err != nil {
		t.Fatalf("write patch: %v", err)
	}
	store, err := patchio.OpenPatchStore(patchPath)
	if err != nil {
		t.Fatalf("open patch store: %v", err)
	}
	defer store.Close()

	g := &applyRetryGitOps{
		applyReaderErrs: []error{errors.New("same.txt: already exists in working directory")},
		afterApplyReader: func(call int) {
			if call == 1 {
				_ = store.Close()
			}
		},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err = applyPatchStore(context.Background(), g, model.ApplyStrict, store, repoRoot, "", t.TempDir(), log)
	if err != nil {
		t.Fatalf("snapshot fallback must work after store closes, got %v", err)
	}
	if g.applyReaderCalls != 1 {
		t.Fatalf("expected one reader apply attempt before reopen failure, got %d", g.applyReaderCalls)
	}
}

func TestApplyPatchStoreRetriesTaggedAlreadyExistsErrorWithMixedPatch(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repoRoot, "same.txt"), "same\n")
	patchPath := filepath.Join(t.TempDir(), "state.patch")
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
	if err := os.WriteFile(patchPath, []byte(patch), 0o644); err != nil {
		t.Fatalf("write patch: %v", err)
	}
	store, err := patchio.OpenPatchStore(patchPath)
	if err != nil {
		t.Fatalf("open patch store: %v", err)
	}
	defer store.Close()

	g := &applyRetryGitOps{
		applyReaderErrs: []error{taggedAlreadyExistsError{msg: "same.txt: 작업 디렉터리에 이미 있습니다"}, nil},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err = applyPatchStore(context.Background(), g, model.ApplyStrict, store, repoRoot, "", t.TempDir(), log)
	if err != nil {
		t.Fatalf("expected tagged already-exists error to retry filtered mixed patch, got %v", err)
	}
	if g.applyReaderCalls != 2 {
		t.Fatalf("expected two reader apply attempts, got %d", g.applyReaderCalls)
	}
}

func TestApplyPatchStoreDoesNotSwallowUnrelatedErrorWhenFilteredPatchBecomesEmpty(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repoRoot, "same.txt"), "same\n")
	patchPath := filepath.Join(t.TempDir(), "state.patch")
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
	if err := os.WriteFile(patchPath, []byte(patch), 0o644); err != nil {
		t.Fatalf("write patch: %v", err)
	}
	store, err := patchio.OpenPatchStore(patchPath)
	if err != nil {
		t.Fatalf("open patch store: %v", err)
	}
	defer store.Close()

	wantErr := errors.New("patch does not apply")
	g := &applyRetryGitOps{applyReaderErrs: []error{wantErr}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err = applyPatchStore(context.Background(), g, model.ApplyStrict, store, repoRoot, "", t.TempDir(), log)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected original error %v, got %v", wantErr, err)
	}
	if g.applyReaderCalls != 1 {
		t.Fatalf("expected one reader apply attempt, got %d", g.applyReaderCalls)
	}
}

func TestApplyPatchStoreDoesNotRetryForUnrelatedErrorThatMentionsSkippedPath(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repoRoot, "same.txt"), "same\n")
	patchPath := filepath.Join(t.TempDir(), "state.patch")
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
	if err := os.WriteFile(patchPath, []byte(patch), 0o644); err != nil {
		t.Fatalf("write patch: %v", err)
	}
	store, err := patchio.OpenPatchStore(patchPath)
	if err != nil {
		t.Fatalf("open patch store: %v", err)
	}
	defer store.Close()

	wantErr := errors.New("same.txt: patch does not apply")
	g := &applyRetryGitOps{applyReaderErrs: []error{wantErr}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err = applyPatchStore(context.Background(), g, model.ApplyStrict, store, repoRoot, "", t.TempDir(), log)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected original error %v, got %v", wantErr, err)
	}
	if g.applyReaderCalls != 1 {
		t.Fatalf("expected one reader apply attempt, got %d", g.applyReaderCalls)
	}
}

func TestApplyPatchStoreRetriesWhenLocalizedErrorNamesSkippedPath(t *testing.T) {
	repoRoot := t.TempDir()
	testutil.WriteFile(t, filepath.Join(repoRoot, "same.txt"), "same\n")
	patchPath := filepath.Join(t.TempDir(), "state.patch")
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
		"index 0000000..e45c9c2",
		"--- /dev/null",
		"+++ b/other.txt",
		"@@ -0,0 +1 @@",
		"+other",
		"",
	}, "\n")
	if err := os.WriteFile(patchPath, []byte(patch), 0o644); err != nil {
		t.Fatalf("write patch: %v", err)
	}
	store, err := patchio.OpenPatchStore(patchPath)
	if err != nil {
		t.Fatalf("open patch store: %v", err)
	}
	defer store.Close()

	g := &applyRetryGitOps{
		applyReaderErrs: []error{errors.New("same.txt: Datei existiert bereits"), nil},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	err = applyPatchStore(context.Background(), g, model.ApplyStrict, store, repoRoot, "", t.TempDir(), log)
	if err != nil {
		t.Fatalf("expected localized already-exists retry to succeed, got %v", err)
	}
	if g.applyReaderCalls != 2 {
		t.Fatalf("expected two reader apply attempts, got %d", g.applyReaderCalls)
	}
}

type snapshotApplyGit struct {
	model.GitOps
	beforeApply func(int)
	calls       int
	inputs      [][]byte
}

func (g *snapshotApplyGit) ApplyPatchReader(ctx context.Context, mode model.ApplyMode, workTree, gitDir string, reader io.Reader) error {
	g.calls++
	if g.beforeApply != nil {
		g.beforeApply(g.calls)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	g.inputs = append(g.inputs, data)
	return g.GitOps.ApplyPatchReader(ctx, mode, workTree, gitDir, bytes.NewReader(data))
}

func TestApplyPatchStoreUsesOnePrivateSnapshotForValidationAndFallback(t *testing.T) {
	for _, mutation := range []string{"overwrite", "replace", "remove"} {
		t.Run(mutation, func(t *testing.T) {
			repo := testutil.InitRepo(t)
			testutil.WriteFile(t, filepath.Join(repo, "same.txt"), "same\n")
			patch := []byte("diff --git a/same.txt b/same.txt\nnew file mode 100644\n--- /dev/null\n+++ b/same.txt\n@@ -0,0 +1 @@\n+same\ndiff --git a/other.txt b/other.txt\nnew file mode 100644\n--- /dev/null\n+++ b/other.txt\n@@ -0,0 +1 @@\n+other\n")
			patchPath := filepath.Join(t.TempDir(), "state.patch")
			if err := os.WriteFile(patchPath, patch, 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := patchio.OpenPatchStore(patchPath)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			dangerous := []byte("diff --git a/../sentinel b/../sentinel\n--- a/../sentinel\n+++ b/../sentinel\n@@ -1 +1 @@\n-preserve\n+destroy\n")
			sentinel := filepath.Join(filepath.Dir(repo), "sentinel")
			testutil.WriteFile(t, sentinel, "preserve\n")
			g := &snapshotApplyGit{GitOps: &gitx.Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}}
			g.beforeApply = func(call int) {
				if call != 1 {
					return
				}
				switch mutation {
				case "overwrite":
					if err := os.WriteFile(patchPath, dangerous, 0o600); err != nil {
						t.Fatal(err)
					}
				case "replace":
					if err := os.WriteFile(patchPath+".replacement", dangerous, 0o600); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(patchPath+".replacement", patchPath); err != nil {
						t.Fatal(err)
					}
				case "remove":
					if err := os.Remove(patchPath); err != nil {
						t.Fatal(err)
					}
				}
			}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))
			if err := applyPatchStore(context.Background(), g, model.ApplyStrict, store, repo, filepath.Join(repo, ".git"), t.TempDir(), log); err != nil {
				t.Fatalf("apply snapshot after %s: %v", mutation, err)
			}
			if g.calls != 2 || !bytes.Equal(g.inputs[0], patch) || bytes.Contains(g.inputs[1], []byte("sentinel")) {
				t.Fatalf("apply did not retain validated bytes: calls=%d inputs=%q", g.calls, g.inputs)
			}
			for path, want := range map[string]string{filepath.Join(repo, "same.txt"): "same\n", filepath.Join(repo, "other.txt"): "other\n", sentinel: "preserve\n"} {
				got, err := os.ReadFile(path)
				if err != nil || string(got) != want {
					t.Fatalf("%s: got %q want %q err=%v", path, got, want, err)
				}
			}
		})
	}
}

func TestApplyPatchDataGitRoundTripPreservesFileState(t *testing.T) {
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "absent-config"))
	repo := testutil.InitRepo(t)
	testutil.RunCmd(t, repo, "git", "config", "core.autocrlf", "false")
	names := []string{"doc.patch", "a/b/문서 with spaces.txt", "b/a/tab\tquote\"back\\line\n.txt"}
	for _, name := range names {
		testutil.WriteFile(t, filepath.Join(repo, name), "old\r\nlast line")
	}
	testutil.WriteFile(t, filepath.Join(repo, "delete.txt"), "deleted\n")
	testutil.WriteFile(t, filepath.Join(repo, "rename-from.txt"), "rename content\n")
	if err := os.Symlink("tracked.txt", filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	testutil.RunCmd(t, repo, "git", "add", "-A")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "round-trip baseline")

	expected := map[string][]byte{}
	for _, name := range names {
		expected[name] = []byte("++ /absolute-is-content\r\n-- ../parent-is-content\r\nrename to .git/content\nlast line without newline")
		if err := os.WriteFile(filepath.Join(repo, name), expected[name], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	expected["binary.dat"] = []byte{0, 1, 2, 255, '\r', '\n', 0, 128}
	expected["empty.txt"] = nil
	expected["script.sh"] = []byte("#!/bin/sh\nprintf hello\n")
	expected["rename-to.txt"] = []byte("rename content\n")
	for _, name := range []string{"binary.dat", "empty.txt", "script.sh"} {
		if err := os.WriteFile(filepath.Join(repo, name), expected[name], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(repo, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "delete.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(repo, "rename-from.txt"), filepath.Join(repo, "rename-to.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	const linkTarget = "a/b/문서 with spaces.txt"
	if err := os.Symlink(linkTarget, filepath.Join(repo, "link")); err != nil {
		t.Fatal(err)
	}
	testutil.RunCmd(t, repo, "git", "add", "-A")
	cmd := exec.Command("git", "diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--no-color", "--src-prefix=a/", "--dst-prefix=b/", "--unified=3", "--find-renames", "HEAD")
	cmd.Dir = repo
	patch, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	testutil.RunCmd(t, repo, "git", "reset", "--hard", "HEAD")
	g := &gitx.Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := applyPatchData(context.Background(), g, model.ApplyStrict, patch, repo, g.GitDir, log); err != nil {
		t.Fatalf("apply real Git patch: %v", err)
	}
	for name, want := range expected {
		got, err := os.ReadFile(filepath.Join(repo, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("round-trip %q: got=%q want=%q err=%v", name, got, want, err)
		}
	}
	for _, name := range []string{"delete.txt", "rename-from.txt"} {
		if _, err := os.Lstat(filepath.Join(repo, name)); !os.IsNotExist(err) {
			t.Fatalf("deleted path %q survived: %v", name, err)
		}
	}
	if got, err := os.Readlink(filepath.Join(repo, "link")); err != nil || got != linkTarget {
		t.Fatalf("symlink: got=%q want=%q err=%v", got, linkTarget, err)
	}
	if info, err := os.Stat(filepath.Join(repo, "script.sh")); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("executable mode lost: info=%v err=%v", info, err)
	}
}
