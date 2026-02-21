package gitx

import (
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

func TestDiffHeadBinary(t *testing.T) {
	repo := testutil.InitRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "changed\n")
	patch, err := g.DiffHeadBinary(context.Background(), repo, g.GitDir)
	if err != nil {
		t.Fatalf("DiffHeadBinary error: %v", err)
	}
	if !strings.Contains(string(patch), "tracked.txt") {
		t.Fatalf("expected tracked file in diff")
	}
}

func TestDiffHeadBinaryNoHead(t *testing.T) {
	repo := testutil.InitEmptyRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	patch, err := g.DiffHeadBinary(context.Background(), repo, g.GitDir)
	if err != nil {
		t.Fatalf("DiffHeadBinary error: %v", err)
	}
	if len(patch) != 0 {
		t.Fatalf("expected empty diff, got %q", string(patch))
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
