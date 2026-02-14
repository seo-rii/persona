package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsClean(t *testing.T) {
	repo := initRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	clean, err := g.IsClean()
	if err != nil {
		t.Fatalf("IsClean error: %v", err)
	}
	if !clean {
		t.Fatalf("expected clean repo")
	}

	writeFile(t, filepath.Join(repo, "untracked.txt"), "dirty\n")
	clean, err = g.IsClean()
	if err != nil {
		t.Fatalf("IsClean error with untracked file: %v", err)
	}
	if clean {
		t.Fatalf("expected dirty repo with untracked file")
	}
	if err := os.Remove(filepath.Join(repo, "untracked.txt")); err != nil {
		t.Fatalf("remove untracked file: %v", err)
	}

	writeFile(t, filepath.Join(repo, "tracked.txt"), "dirty\n")
	clean, err = g.IsClean()
	if err != nil {
		t.Fatalf("IsClean error on dirty repo: %v", err)
	}
	if clean {
		t.Fatalf("expected dirty repo")
	}
}

func TestListIgnoredCandidates(t *testing.T) {
	repo := initRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	writeFile(t, filepath.Join(repo, ".gitignore"), "ignored.txt\n")
	runCmd(t, repo, "git", "add", ".gitignore")
	runCmd(t, repo, "git", "commit", "-m", "ignore")

	writeFile(t, filepath.Join(repo, "ignored.txt"), "skip\n")
	ignored, err := g.ListIgnoredCandidates(repo, g.GitDir, 10)
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

func TestDiffNewFileNoIndex(t *testing.T) {
	repo := initRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	writeFile(t, filepath.Join(repo, "new.txt"), "hello\n")
	patch, err := g.DiffNewFileNoIndex(repo, g.GitDir, "new.txt")
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
	repo := initRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	writeFile(t, filepath.Join(repo, "tracked.txt"), "changed\n")
	patch, err := g.DiffHeadBinary(repo, g.GitDir)
	if err != nil {
		t.Fatalf("DiffHeadBinary error: %v", err)
	}
	if !strings.Contains(string(patch), "tracked.txt") {
		t.Fatalf("expected tracked file in diff")
	}
}

func TestDiffHeadBinaryNoHead(t *testing.T) {
	repo := initEmptyRepo(t)
	g := Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}

	patch, err := g.DiffHeadBinary(repo, g.GitDir)
	if err != nil {
		t.Fatalf("DiffHeadBinary error: %v", err)
	}
	if len(patch) != 0 {
		t.Fatalf("expected empty diff, got %q", string(patch))
	}
}

func TestDetectRepoIgnoresEnv(t *testing.T) {
	repo := initRepo(t)
	t.Setenv("GIT_DIR", "/tmp/nogit")
	t.Setenv("GIT_WORK_TREE", "/tmp/nogit")

	root, gitDir, err := DetectRepo(repo)
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

func initRepo(t *testing.T) string {
	dir := t.TempDir()
	runCmd(t, dir, "git", "init")
	runCmd(t, dir, "git", "config", "user.email", "you@example.com")
	runCmd(t, dir, "git", "config", "user.name", "You")
	writeFile(t, filepath.Join(dir, "tracked.txt"), "base\n")
	runCmd(t, dir, "git", "add", "tracked.txt")
	runCmd(t, dir, "git", "commit", "-m", "init")
	return dir
}

func initEmptyRepo(t *testing.T) string {
	dir := t.TempDir()
	runCmd(t, dir, "git", "init")
	runCmd(t, dir, "git", "config", "user.email", "you@example.com")
	runCmd(t, dir, "git", "config", "user.name", "You")
	return dir
}

func writeFile(t *testing.T, path, data string) {
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func runCmd(t *testing.T, dir, name string, args ...string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("command failed: %s %v: %v: %s", name, args, err, string(out))
	}
}
