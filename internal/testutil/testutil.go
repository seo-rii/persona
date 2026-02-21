package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// InitRepo creates a temporary Git repository with a single committed file
// (tracked.txt) and returns the repo root path.
func InitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	RunCmd(t, dir, "git", "init")
	RunCmd(t, dir, "git", "config", "user.email", "you@example.com")
	RunCmd(t, dir, "git", "config", "user.name", "You")
	WriteFile(t, filepath.Join(dir, "tracked.txt"), "base\n")
	RunCmd(t, dir, "git", "add", "tracked.txt")
	RunCmd(t, dir, "git", "commit", "-m", "init")
	return dir
}

// InitEmptyRepo creates a temporary Git repository with no commits.
func InitEmptyRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	RunCmd(t, dir, "git", "init")
	RunCmd(t, dir, "git", "config", "user.email", "you@example.com")
	RunCmd(t, dir, "git", "config", "user.name", "You")
	return dir
}

// WriteFile writes data to path, creating parent directories as needed.
func WriteFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

// RunCmd runs a command and fatals on error.
func RunCmd(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("command failed: %s %v: %v: %s", name, args, err, string(out))
	}
}
