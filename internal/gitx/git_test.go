package gitx

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"persona/internal/model"
	"persona/internal/testutil"
)

func TestIsClean(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	clean, err := g.IsClean(context.Background())
	if err != nil {
		t.Fatalf("IsClean error: %v", err)
	}
	if !clean {
		t.Fatalf("expected clean repo")
	}

	testutil.WriteFile(t, filepath.Join(repo, "untracked.txt"), "dirty\n")
	clean, err = g.IsClean(context.Background())
	if err != nil {
		t.Fatalf("IsClean error with untracked file: %v", err)
	}
	if clean {
		t.Fatalf("expected dirty repo with untracked file")
	}
	if err := os.Remove(filepath.Join(repo, "untracked.txt")); err != nil {
		t.Fatalf("remove untracked file: %v", err)
	}

	testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "dirty\n")
	clean, err = g.IsClean(context.Background())
	if err != nil {
		t.Fatalf("IsClean error on dirty repo: %v", err)
	}
	if clean {
		t.Fatalf("expected dirty repo")
	}
}

func TestIsCleanExceptUntracked(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "seed\n")

	clean, err := g.IsCleanExceptUntracked(context.Background(), []string{"state.patch"})
	if err != nil {
		t.Fatalf("IsCleanExceptUntracked error: %v", err)
	}
	if !clean {
		t.Fatalf("expected clean when excluded untracked path is the only change")
	}

	clean, err = g.IsCleanExceptUntracked(context.Background(), []string{"other.patch"})
	if err != nil {
		t.Fatalf("IsCleanExceptUntracked error with non-matching exclude: %v", err)
	}
	if clean {
		t.Fatalf("expected dirty with non-matching exclude")
	}
}

func TestIsCleanExceptPaths(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "seed\n")
	testutil.RunCmd(t, repo, "git", "add", "state.patch")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "track patch")
	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "updated\n")
	testutil.WriteFile(t, filepath.Join(repo, "state.patch.lock"), "lock\n")

	clean, err := g.IsCleanExceptPaths(context.Background(), []string{"state.patch", "state.patch.lock"})
	if err != nil {
		t.Fatalf("IsCleanExceptPaths error: %v", err)
	}
	if !clean {
		t.Fatalf("expected clean when only excluded patch state changed")
	}

	testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "dirty\n")
	clean, err = g.IsCleanExceptPaths(context.Background(), []string{"state.patch", "state.patch.lock"})
	if err != nil {
		t.Fatalf("IsCleanExceptPaths error with extra tracked change: %v", err)
	}
	if clean {
		t.Fatalf("expected dirty when other tracked changes exist")
	}
}

func TestListIgnoredCandidates(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	testutil.WriteFile(t, filepath.Join(repo, ".gitignore"), "ignored.txt\n")
	testutil.RunCmd(t, repo, "git", "add", ".gitignore")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "ignore")

	testutil.WriteFile(t, filepath.Join(repo, "ignored.txt"), "skip\n")
	ignored, err := g.ListIgnoredCandidates(context.Background(), repo, g.GitDir, 10)
	if err != nil {
		t.Fatalf("ListIgnoredCandidates error: %v", err)
	}
	found := false
	for _, item := range ignored {
		if item == "ignored.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected ignored.txt in %v", ignored)
	}
}

func TestListIgnoredCandidatesPreservesLeadingSpace(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	testutil.WriteFile(t, filepath.Join(repo, ".gitignore"), " lead.txt\n")
	testutil.RunCmd(t, repo, "git", "add", ".gitignore")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "ignore leading-space file")

	testutil.WriteFile(t, filepath.Join(repo, " lead.txt"), "skip\n")
	ignored, err := g.ListIgnoredCandidates(context.Background(), repo, g.GitDir, 10)
	if err != nil {
		t.Fatalf("ListIgnoredCandidates error: %v", err)
	}
	if !containsPath(ignored, " lead.txt") {
		t.Fatalf("expected exact ignored path with leading space, got %q", ignored)
	}
	if containsPath(ignored, "lead.txt") {
		t.Fatalf("leading space must not be trimmed, got %q", ignored)
	}
}

func TestListIgnoredCandidatesTrimsDirectorySlash(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	testutil.WriteFile(t, filepath.Join(repo, ".gitignore"), "ignored-dir/\n")
	testutil.RunCmd(t, repo, "git", "add", ".gitignore")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "ignore dir")
	if err := os.MkdirAll(filepath.Join(repo, "ignored-dir"), 0o755); err != nil {
		t.Fatalf("mkdir ignored dir: %v", err)
	}
	testutil.WriteFile(t, filepath.Join(repo, "ignored-dir", "file.txt"), "skip\n")

	ignored, err := g.ListIgnoredCandidates(context.Background(), repo, g.GitDir, 10)
	if err != nil {
		t.Fatalf("ListIgnoredCandidates error: %v", err)
	}
	if !containsPath(ignored, "ignored-dir") {
		t.Fatalf("expected trimmed ignored dir entry, got %q", ignored)
	}
	if containsPath(ignored, "ignored-dir/") {
		t.Fatalf("directory slash must be trimmed, got %q", ignored)
	}
}

func TestListIgnoredCandidatesReturnsOverflowSentinel(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	testutil.WriteFile(t, filepath.Join(repo, ".gitignore"), "a.tmp\nb.tmp\nc.tmp\n")
	testutil.RunCmd(t, repo, "git", "add", ".gitignore")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "ignore many")
	for _, name := range []string{"a.tmp", "b.tmp", "c.tmp"} {
		testutil.WriteFile(t, filepath.Join(repo, name), "skip\n")
	}

	ignored, err := g.ListIgnoredCandidates(context.Background(), repo, g.GitDir, 2)
	if err != nil {
		t.Fatalf("ListIgnoredCandidates error: %v", err)
	}
	if len(ignored) != 3 {
		t.Fatalf("expected overflow sentinel entry, got %q", ignored)
	}
}

func TestDiffNewFileNoIndex(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	testutil.WriteFile(t, filepath.Join(repo, "new.txt"), "hello\n")
	patch, err := g.DiffNewFileNoIndex(context.Background(), repo, g.GitDir, "new.txt")
	if err != nil {
		t.Fatalf("DiffNewFileNoIndex error: %v", err)
	}
	if !strings.Contains(string(patch), "new file mode") {
		t.Fatalf("expected new file mode in patch")
	}
	if !strings.Contains(string(patch), "new.txt") {
		t.Fatalf("expected filename in patch")
	}
}

func TestDiffNewFileNoIndexTo(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	testutil.WriteFile(t, filepath.Join(repo, "new.txt"), "hello\n")
	var out bytes.Buffer
	if err := g.DiffNewFileNoIndexTo(context.Background(), repo, g.GitDir, "new.txt", &out); err != nil {
		t.Fatalf("DiffNewFileNoIndexTo error: %v", err)
	}
	if !strings.Contains(out.String(), "new file mode") || !strings.Contains(out.String(), "new.txt") {
		t.Fatalf("expected streamed new file diff, got %q", out.String())
	}
}

func TestDiffHeadBinary(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "changed\n")
	patch, err := g.DiffHeadBinary(context.Background(), repo, g.GitDir, nil)
	if err != nil {
		t.Fatalf("DiffHeadBinary error: %v", err)
	}
	if !strings.Contains(string(patch), "tracked.txt") {
		t.Fatalf("expected tracked file in diff")
	}
}

func TestDiffHeadBinaryTo(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "changed\n")
	var out bytes.Buffer
	if err := g.DiffHeadBinaryTo(context.Background(), repo, g.GitDir, nil, &out); err != nil {
		t.Fatalf("DiffHeadBinaryTo error: %v", err)
	}
	if !strings.Contains(out.String(), "tracked.txt") {
		t.Fatalf("expected tracked file in streamed diff")
	}
}

func TestDiffHeadBinaryExcludePaths(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "seed\n")
	testutil.RunCmd(t, repo, "git", "add", "state.patch")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "track patch")
	testutil.WriteFile(t, filepath.Join(repo, "state.patch"), "updated\n")
	testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "changed\n")

	patch, err := g.DiffHeadBinary(context.Background(), repo, g.GitDir, []string{"state.patch"})
	if err != nil {
		t.Fatalf("DiffHeadBinary error: %v", err)
	}
	if strings.Contains(string(patch), "state.patch") {
		t.Fatalf("expected excluded tracked path to be omitted from diff: %s", string(patch))
	}
	if !strings.Contains(string(patch), "tracked.txt") {
		t.Fatalf("expected other tracked changes to remain in diff: %s", string(patch))
	}
}

func TestDiffHeadBinaryExcludePathsTreatsPatchPathLiterally(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	testutil.WriteFile(t, filepath.Join(repo, "state[1].patch"), "seed\n")
	testutil.RunCmd(t, repo, "git", "add", "state[1].patch")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "track patch with metachar")
	testutil.WriteFile(t, filepath.Join(repo, "state[1].patch"), "updated\n")
	testutil.WriteFile(t, filepath.Join(repo, "state1.patch"), "other\n")
	testutil.RunCmd(t, repo, "git", "add", "state1.patch")

	patch, err := g.DiffHeadBinary(context.Background(), repo, g.GitDir, []string{"state[1].patch"})
	if err != nil {
		t.Fatalf("DiffHeadBinary error: %v", err)
	}
	if strings.Contains(string(patch), "state[1].patch") {
		t.Fatalf("expected excluded patch path to be omitted: %s", string(patch))
	}
	if !strings.Contains(string(patch), "state1.patch") {
		t.Fatalf("expected unrelated tracked diff to remain despite metacharacters: %s", string(patch))
	}
}

func TestDiffHeadBinaryNoHead(t *testing.T) {
	repo := testutil.InitEmptyRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	patch, err := g.DiffHeadBinary(context.Background(), repo, g.GitDir, nil)
	if err != nil {
		t.Fatalf("DiffHeadBinary error: %v", err)
	}
	if len(patch) != 0 {
		t.Fatalf("expected empty diff, got %q", string(patch))
	}
}

func TestDiffHeadBinaryCorruptedHeadReturnsError(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	headData, err := os.ReadFile(filepath.Join(repo, ".git", "HEAD"))
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}
	refLine := strings.TrimSpace(string(headData))
	refPath := strings.TrimPrefix(refLine, "ref: ")
	refPath = filepath.Join(repo, ".git", filepath.FromSlash(refPath))
	if err := os.WriteFile(refPath, []byte("deadbeef\n"), 0o644); err != nil {
		t.Fatalf("corrupt ref: %v", err)
	}

	_, err = g.DiffHeadBinary(context.Background(), repo, g.GitDir, nil)
	if err == nil {
		t.Fatalf("expected corruption to surface as error")
	}
}

func TestDetectRepoIgnoresEnv(t *testing.T) {
	repo := testutil.InitRepo(t)
	t.Setenv("GIT_DIR", "/tmp/nogit")
	t.Setenv("GIT_WORK_TREE", "/tmp/nogit")

	root, gitDir, err := DetectRepo(context.Background(), repo)
	if err != nil {
		t.Fatalf("DetectRepo error: %v", err)
	}
	if root != repo {
		t.Fatalf("expected repo root %s got %s", repo, root)
	}
	expectedGitDir := filepath.Join(repo, ".git")
	if gitDir != expectedGitDir {
		t.Fatalf("expected git dir %s got %s", expectedGitDir, gitDir)
	}
}

func TestDetectRepoOnLinkedWorktree(t *testing.T) {
	repo := testutil.InitRepo(t)
	linked := filepath.Join(t.TempDir(), "linked-worktree")
	testutil.RunCmd(t, repo, "git", "worktree", "add", "--detach", linked)

	root, gitDir, err := DetectRepo(context.Background(), linked)
	if err != nil {
		t.Fatalf("DetectRepo error: %v", err)
	}
	if root != linked {
		t.Fatalf("expected linked worktree root %q got %q", linked, root)
	}
	if !filepath.IsAbs(gitDir) {
		t.Fatalf("expected absolute git dir, got %q", gitDir)
	}
	if gitDir == filepath.Join(linked, ".git") {
		t.Fatalf("expected linked worktree gitdir outside .git file, got %q", gitDir)
	}
}

func TestFilterGitEnvStripsAllGitVarsButKeepsOthers(t *testing.T) {
	env := []string{
		"GIT_DIR=/tmp/gitdir",
		"GIT_WORK_TREE=/tmp/worktree",
		"GIT_INDEX_FILE=/tmp/index",
		"PATH=/usr/bin",
		"HOME=/tmp/home",
	}

	filtered := FilterGitEnv(env)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 env vars left, got %v", filtered)
	}
	if filtered[0] != "PATH=/usr/bin" || filtered[1] != "HOME=/tmp/home" {
		t.Fatalf("unexpected filtered env: %v", filtered)
	}
}

func TestEnvWithForcesCLocaleOverCallerLocale(t *testing.T) {
	t.Setenv("LANG", "de_DE.UTF-8")
	t.Setenv("LC_ALL", "ko_KR.UTF-8")
	g := Git{RepoRoot: "/repo", GitDir: "/repo/.git"}

	env := g.envWith("/repo", "/repo/.git")
	var foundLang, foundLCAll bool
	for _, item := range env {
		switch item {
		case "LANG=C":
			foundLang = true
		case "LC_ALL=C":
			foundLCAll = true
		case "LANG=de_DE.UTF-8", "LC_ALL=ko_KR.UTF-8":
			t.Fatalf("caller locale must not leak into internal git env: %v", env)
		}
	}
	if !foundLang || !foundLCAll {
		t.Fatalf("expected internal git env to force C locale, got %v", env)
	}
}

func TestTruncateOutputAddsMarker(t *testing.T) {
	msg := strings.Repeat("x", maxErrOutput+1)
	got := truncateOutput(msg)
	if !strings.HasSuffix(got, "... (truncated)") {
		t.Fatalf("expected truncation marker, got %q", got)
	}
	if len(got) <= maxErrOutput {
		t.Fatalf("expected marker to extend output length, got %d", len(got))
	}
	if !strings.HasPrefix(got, strings.Repeat("x", maxErrOutput)) {
		t.Fatalf("expected prefix to preserve first %d bytes", maxErrOutput)
	}
}

func TestListUntracked(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	testutil.WriteFile(t, filepath.Join(repo, "a.txt"), "a\n")
	if err := os.MkdirAll(filepath.Join(repo, "dir"), 0o755); err != nil {
		t.Fatalf("mkdir dir: %v", err)
	}
	testutil.WriteFile(t, filepath.Join(repo, "dir", "b.txt"), "b\n")

	paths, err := g.ListUntracked(context.Background(), repo, g.GitDir)
	if err != nil {
		t.Fatalf("ListUntracked error: %v", err)
	}
	if !containsPath(paths, "a.txt") || !containsPath(paths, "dir/b.txt") {
		t.Fatalf("expected untracked paths in %v", paths)
	}
}

func TestListUntrackedIgnoresGitIndexFileEnv(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "alt.index"))
	paths, err := g.ListUntracked(context.Background(), repo, g.GitDir)
	if err != nil {
		t.Fatalf("ListUntracked error: %v", err)
	}
	if containsPath(paths, "tracked.txt") {
		t.Fatalf("tracked file must not become untracked via GIT_INDEX_FILE: %v", paths)
	}
}

func TestIsCleanExceptUntrackedIgnoresGitIndexFileEnv(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "alt.index"))
	clean, err := g.IsCleanExceptUntracked(context.Background(), nil)
	if err != nil {
		t.Fatalf("IsCleanExceptUntracked error: %v", err)
	}
	if !clean {
		t.Fatalf("clean repo must stay clean despite GIT_INDEX_FILE")
	}
}

func TestApplyPatchStrictSuccess(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	patch := strings.Join([]string{
		"diff --git a/new.txt b/new.txt",
		"new file mode 100644",
		"index 0000000..e69de29",
		"--- /dev/null",
		"+++ b/new.txt",
		"@@ -0,0 +1 @@",
		"+hello",
		"",
	}, "\n")

	if err := g.ApplyPatch(context.Background(), model.ApplyStrict, repo, g.GitDir, []byte(patch)); err != nil {
		t.Fatalf("ApplyPatch strict error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(repo, "new.txt"))
	if err != nil {
		t.Fatalf("read new file: %v", err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("unexpected new.txt content %q", string(data))
	}
}

func TestApplyPatchStrictFailure(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	patch := strings.Join([]string{
		"diff --git a/tracked.txt b/tracked.txt",
		"index 1111111..2222222 100644",
		"--- a/tracked.txt",
		"+++ b/tracked.txt",
		"@@ -1 +1 @@",
		"-nope",
		"+changed",
		"",
	}, "\n")

	if err := g.ApplyPatch(context.Background(), model.ApplyStrict, repo, g.GitDir, []byte(patch)); err == nil {
		t.Fatalf("expected strict apply failure")
	}
}

func TestApplyPatchReaderStrict(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	patch := strings.Join([]string{
		"diff --git a/new.txt b/new.txt",
		"new file mode 100644",
		"index 0000000..e69de29",
		"--- /dev/null",
		"+++ b/new.txt",
		"@@ -0,0 +1 @@",
		"+hello",
		"",
	}, "\n")

	if err := g.ApplyPatchReader(context.Background(), model.ApplyStrict, repo, g.GitDir, strings.NewReader(patch)); err != nil {
		t.Fatalf("ApplyPatchReader strict error: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(repo, "new.txt"))
	if err != nil {
		t.Fatalf("read new file: %v", err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("unexpected new.txt content %q", string(data))
	}
}

func TestApplyPatchRejectFailure(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	patch := strings.Join([]string{
		"diff --git a/tracked.txt b/tracked.txt",
		"index 1111111..2222222 100644",
		"--- a/tracked.txt",
		"+++ b/tracked.txt",
		"@@ -1 +1 @@",
		"-nope",
		"+changed",
		"",
	}, "\n")

	if err := g.ApplyPatch(context.Background(), model.ApplyReject, repo, g.GitDir, []byte(patch)); err == nil {
		t.Fatalf("expected reject apply failure")
	}
}

func TestWorktreeAddDetachAndRemoveForce(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	wt := filepath.Join(t.TempDir(), "wt")

	if err := g.WorktreeAddDetach(context.Background(), wt, "HEAD"); err != nil {
		t.Fatalf("WorktreeAddDetach error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, "tracked.txt")); err != nil {
		t.Fatalf("expected tracked.txt in worktree: %v", err)
	}
	if err := g.WorktreeRemoveForce(context.Background(), wt); err != nil {
		t.Fatalf("WorktreeRemoveForce error: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("expected worktree path removed, got %v", err)
	}
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}
