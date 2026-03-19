//go:build linux
// +build linux

package integration

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersonaIntegrationSecurity(t *testing.T) {
	persona := requireIntegration(t)

	t.Run("git masked", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "if test -e .git/HEAD; then echo yes > git-present.txt; else echo no > git-present.txt; fi"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("git-present.txt")) {
			t.Fatalf("patch missing git-present.txt")
		}
		if !bytes.Contains(data, []byte("\n+no\n")) {
			t.Fatalf("expected .git to be masked")
		}
	})

	t.Run("linked worktree git file masked and patch reapplies", func(t *testing.T) {
		repo := createRepo(t)
		linked := filepath.Join(t.TempDir(), "linked-worktree")
		runCmd(t, repo, "git", "worktree", "add", "--detach", linked)
		info, err := os.Stat(filepath.Join(linked, ".git"))
		if err != nil {
			t.Fatalf("stat linked .git: %v", err)
		}
		if info.IsDir() {
			t.Fatal("expected linked worktree .git to be a file")
		}

		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "wc -c < .git > dotgit-size.txt; git rev-parse --show-toplevel > git.out 2> git.err; rc=$?; echo $rc > git.code; if [ $rc -ne 0 ]; then echo yes > git-failed.txt; else echo no > git-failed.txt; fi; echo linked > linked.txt"
		code, _, _ := runPersona(t, persona, linked, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		code, _, _ = runPersona(t, persona, linked, []string{"--patch", patchPath}, []string{"sh", "-c", "cat linked.txt > seen.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 on reapply got %d", code)
		}

		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("dotgit-size.txt")) {
			t.Fatalf("patch missing dotgit-size.txt")
		}
		if !bytes.Contains(data, []byte("\n+0\n")) {
			t.Fatalf("expected masked linked-worktree .git file to appear empty")
		}
		if !bytes.Contains(data, []byte("git.err")) {
			t.Fatalf("patch missing git.err")
		}
		if !bytes.Contains(data, []byte("git-failed.txt")) {
			t.Fatalf("patch missing git-failed.txt")
		}
		if !bytes.Contains(data, []byte("\n+yes\n")) {
			t.Fatalf("expected child git to fail in linked worktree view, got %s", string(data))
		}
		if !bytes.Contains(data, []byte("linked.txt")) || !bytes.Contains(data, []byte("seen.txt")) {
			t.Fatalf("expected linked worktree patch apply/export to succeed")
		}
	})

	t.Run("patch file masked and excluded", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(repo, "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "echo data > foo.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if bytes.Contains(data, []byte("state.patch")) {
			t.Fatalf("patch file path should be excluded")
		}
		if !bytes.Contains(data, []byte("foo.txt")) {
			t.Fatalf("patch missing foo.txt")
		}
	})

	t.Run("patch file masked in view", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(repo, "state.patch")
		seed := strings.Join([]string{
			"diff --git a/seed.txt b/seed.txt",
			"new file mode 100644",
			"index 0000000..6ed281c",
			"--- /dev/null",
			"+++ b/seed.txt",
			"@@ -0,0 +1 @@",
			"+seed",
			"",
		}, "\n")
		writeFile(t, patchPath, []byte(seed))
		cmd := "if [ -s state.patch ]; then echo nonempty > mask.txt; else echo empty > mask.txt; fi"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("mask.txt")) {
			t.Fatalf("patch missing mask.txt")
		}
		if !bytes.Contains(data, []byte("\n+empty\n")) {
			t.Fatalf("expected masked patch file to appear empty")
		}
	})

	t.Run("patch lock file masked and excluded", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(repo, "state.patch")
		seed := strings.Join([]string{
			"diff --git a/seed.txt b/seed.txt",
			"new file mode 100644",
			"index 0000000..6ed281c",
			"--- /dev/null",
			"+++ b/seed.txt",
			"@@ -0,0 +1 @@",
			"+seed",
			"",
		}, "\n")
		writeFile(t, patchPath, []byte(seed))
		writeFile(t, patchPath+".lock", []byte("seed-lock\n"))
		cmd := "if [ -s state.patch.lock ]; then echo nonempty > lock-mask.txt; else echo empty > lock-mask.txt; fi"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("lock-mask.txt")) {
			t.Fatalf("patch missing lock-mask.txt")
		}
		if !bytes.Contains(data, []byte("\n+empty\n")) {
			t.Fatalf("expected masked patch lock file to appear empty")
		}
		if bytes.Contains(data, []byte("state.patch.lock")) {
			t.Fatalf("patch lock path should be excluded")
		}
	})

	t.Run("patch symlink to repo masked and excluded", func(t *testing.T) {
		repo := createRepo(t)
		realPatch := filepath.Join(repo, "state.patch")
		linkPath := filepath.Join(t.TempDir(), "link.patch")
		if err := os.Symlink(realPatch, linkPath); err != nil {
			t.Fatalf("symlink patch: %v", err)
		}
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", linkPath}, []string{"sh", "-c", "echo data > foo.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, realPatch)
		if bytes.Contains(data, []byte("state.patch")) {
			t.Fatalf("patch file path should be excluded")
		}
		if !bytes.Contains(data, []byte("foo.txt")) {
			t.Fatalf("patch missing foo.txt")
		}
	})

	t.Run("print patch path resolves symlink target", func(t *testing.T) {
		repo := createRepo(t)
		patchDir := t.TempDir()
		realPatch := filepath.Join(patchDir, "state.patch")
		linkPatch := filepath.Join(patchDir, "state.link.patch")
		if err := os.Symlink(realPatch, linkPatch); err != nil {
			t.Fatalf("symlink patch: %v", err)
		}
		code, out, _ := runPersona(t, persona, repo, []string{"--patch", linkPatch, "--print-patch-path"}, []string{"sh", "-c", "true"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		if strings.TrimSpace(out) != realPatch {
			t.Fatalf("expected printed path %q got %q", realPatch, strings.TrimSpace(out))
		}
		if _, err := os.Stat(realPatch); err != nil {
			t.Fatalf("expected real patch file to exist: %v", err)
		}
	})

	t.Run("patch path validation rejects dotdot", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "bad.patch")
		writeFile(t, patchPath, []byte("diff --git a/../evil b/../evil\n"))
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "true"}, nil)
		if code != 12 {
			t.Fatalf("expected exit 12 got %d", code)
		}
	})

	t.Run("patch path validation rejects absolute", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "bad.patch")
		writeFile(t, patchPath, []byte("diff --git a//tmp/evil b//tmp/evil\n"))
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "true"}, nil)
		if code != 12 {
			t.Fatalf("expected exit 12 got %d", code)
		}
	})

	t.Run("patch path validation rejects dotgit", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "bad.patch")
		writeFile(t, patchPath, []byte("diff --git a/.git/config b/.git/config\n"))
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "true"}, nil)
		if code != 12 {
			t.Fatalf("expected exit 12 got %d", code)
		}
	})

	t.Run("patch path validation rejects dot segment", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "bad.patch")
		writeFile(t, patchPath, []byte("diff --git a/./evil b/./evil\n"))
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "true"}, nil)
		if code != 12 {
			t.Fatalf("expected exit 12 got %d", code)
		}
	})

	t.Run("patch path validation rejects quoted dotdot", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "bad.patch")
		writeFile(t, patchPath, []byte("diff --git \"a/../evil\" \"b/../evil\"\n"))
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "true"}, nil)
		if code != 12 {
			t.Fatalf("expected exit 12 got %d", code)
		}
	})

	t.Run("patch path validation rejects rename dotdot", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "bad.patch")
		patch := "diff --git a/a.txt b/b.txt\nrename from ../evil.txt\nrename to b.txt\n"
		writeFile(t, patchPath, []byte(patch))
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "true"}, nil)
		if code != 12 {
			t.Fatalf("expected exit 12 got %d", code)
		}
	})

	t.Run("patch path validation rejects copy dotgit", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "bad.patch")
		patch := "diff --git a/a.txt b/b.txt\ncopy from .git/config\ncopy to b.txt\n"
		writeFile(t, patchPath, []byte(patch))
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "true"}, nil)
		if code != 12 {
			t.Fatalf("expected exit 12 got %d", code)
		}
	})

	t.Run("git env override ignored for persona ops", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		env := map[string]string{
			"GIT_DIR":       "/tmp/nogit",
			"GIT_WORK_TREE": "/tmp/nogit",
		}
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "echo ok > ok.txt"}, env)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("ok.txt")) {
			t.Fatalf("patch missing ok.txt")
		}
	})

	t.Run("git env override scrubbed for child", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		env := map[string]string{
			"GIT_DIR":        "/tmp/nogit",
			"GIT_WORK_TREE":  "/tmp/nogit",
			"GIT_INDEX_FILE": "/tmp/nogit-index",
		}
		cmd := "git status > git-status.out 2> git-status.err; echo $? > git-status.code"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, env)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("git-status.code")) {
			t.Fatalf("patch missing git-status.code")
		}
		if !bytes.Contains(data, []byte("git-status.err")) {
			t.Fatalf("patch missing git-status.err")
		}
		if !bytes.Contains(data, []byte("not a git repository")) {
			t.Fatalf("expected child git failure output in patch, got %s", string(data))
		}
	})

	t.Run("child env dump omits ambient git vars", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		env := map[string]string{
			"GIT_DIR":         "/tmp/nogit",
			"GIT_WORK_TREE":   "/tmp/nogit",
			"GIT_INDEX_FILE":  "/tmp/nogit-index",
			"GIT_SSH_COMMAND": "ssh -F /tmp/nogit-config",
		}
		cmd := "env | sort > env.txt; if grep -q '^GIT_' env.txt; then echo leaked > status.txt; else echo clean > status.txt; fi"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, env)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("status.txt")) {
			t.Fatalf("patch missing status.txt")
		}
		if !bytes.Contains(data, []byte("\n+clean\n")) {
			t.Fatalf("expected child env dump to report clean")
		}
		for _, key := range []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_INDEX_FILE=", "GIT_SSH_COMMAND="} {
			if bytes.Contains(data, []byte(key)) {
				t.Fatalf("patch leaked %s in child env dump", key)
			}
		}
	})
}
