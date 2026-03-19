//go:build linux
// +build linux

package integration

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPersonaIntegrationEdge(t *testing.T) {
	persona := requireIntegration(t)

	t.Run("patch path is directory", func(t *testing.T) {
		repo := createRepo(t)
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", repo}, []string{"sh", "-c", "true"}, nil)
		if code != 14 {
			t.Fatalf("expected exit 14 got %d", code)
		}
	})

	t.Run("empty repo without HEAD", func(t *testing.T) {
		repo := createEmptyRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "echo data > foo.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("foo.txt")) {
			t.Fatalf("patch missing foo.txt")
		}
	})

	t.Run("apply skips identical new file", func(t *testing.T) {
		repo := createRepo(t)
		existingContent := strings.Join([]string{
			"diff --git a/test.txt b/test.txt",
			"new file mode 100644",
			"index 0000000..6ed281c",
			"--- /dev/null",
			"+++ b/test.txt",
			"@@ -0,0 +1,2 @@",
			"+1",
			"+1",
			"",
		}, "\n")
		existingPath := filepath.Join(repo, "test.patch")
		writeFile(t, existingPath, []byte(existingContent))
		if err := os.Chmod(existingPath, 0o755); err != nil {
			t.Fatalf("chmod existing test.patch: %v", err)
		}
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		patch := strings.Join([]string{
			"diff --git a/test.patch b/test.patch",
			"new file mode 100755",
			"index 0000000..1568bcc",
			"--- /dev/null",
			"+++ b/test.patch",
			"@@ -0,0 +1,8 @@",
			"+diff --git a/test.txt b/test.txt",
			"+new file mode 100644",
			"+index 0000000..6ed281c",
			"+--- /dev/null",
			"++++ b/test.txt",
			"+@@ -0,0 +1,2 @@",
			"++1",
			"++1",
			"",
		}, "\n")
		writeFile(t, patchPath, []byte(patch))
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--allow-dirty"}, []string{"sh", "-c", "echo 1 >> test.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("test.txt")) {
			t.Fatalf("patch missing test.txt")
		}
	})

	t.Run("apply skips existing among multiple new files", func(t *testing.T) {
		repo := createRepo(t)
		existingPath := filepath.Join(repo, "keep.patch")
		existingContent := strings.Join([]string{
			"diff --git a/keep.txt b/keep.txt",
			"new file mode 100644",
			"index 0000000..6ed281c",
			"--- /dev/null",
			"+++ b/keep.txt",
			"@@ -0,0 +1 @@",
			"+keep",
			"",
		}, "\n")
		writeFile(t, existingPath, []byte(existingContent))
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		patch := strings.Join([]string{
			"diff --git a/keep.patch b/keep.patch",
			"new file mode 100644",
			"index 0000000..1111111",
			"--- /dev/null",
			"+++ b/keep.patch",
			"@@ -0,0 +1,7 @@",
			"+diff --git a/keep.txt b/keep.txt",
			"+new file mode 100644",
			"+index 0000000..6ed281c",
			"+--- /dev/null",
			"++++ b/keep.txt",
			"+@@ -0,0 +1 @@",
			"++keep",
			"",
			"diff --git a/fresh.txt b/fresh.txt",
			"new file mode 100644",
			"index 0000000..e69de29",
			"--- /dev/null",
			"+++ b/fresh.txt",
			"@@ -0,0 +1 @@",
			"+fresh",
			"",
		}, "\n")
		writeFile(t, patchPath, []byte(patch))
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--allow-dirty"}, []string{"sh", "-c", "cat fresh.txt > seen.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("fresh.txt")) {
			t.Fatalf("patch missing fresh.txt")
		}
	})

	t.Run("apply fails when existing new file differs", func(t *testing.T) {
		repo := createRepo(t)
		existingPath := filepath.Join(repo, "conflict.patch")
		writeFile(t, existingPath, []byte("different\n"))
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		patch := strings.Join([]string{
			"diff --git a/conflict.patch b/conflict.patch",
			"new file mode 100644",
			"index 0000000..e69de29",
			"--- /dev/null",
			"+++ b/conflict.patch",
			"@@ -0,0 +1 @@",
			"+same",
			"",
		}, "\n")
		writeFile(t, patchPath, []byte(patch))
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--allow-dirty"}, []string{"sh", "-c", "true"}, nil)
		if code != 12 {
			t.Fatalf("expected exit 12 got %d", code)
		}
	})

	t.Run("ignored readonly enforces failure", func(t *testing.T) {
		repo := createRepo(t)
		writeFile(t, filepath.Join(repo, ".gitignore"), []byte("ignored-a.txt\n"))
		runCmd(t, repo, "git", "add", ".gitignore")
		runCmd(t, repo, "git", "commit", "-m", "add ignore")
		writeFile(t, filepath.Join(repo, "ignored-a.txt"), []byte("seed\n"))
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "echo data > ignored-a.txt; echo $? > code.txt"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--ignored-mode", "readonly"}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("code.txt")) {
			t.Fatalf("patch missing code.txt")
		}
		if !containsNonZeroLine(data) {
			t.Fatalf("expected readonly write failure marker")
		}
	})

	t.Run("ignored masked replaces content", func(t *testing.T) {
		repo := createRepo(t)
		writeFile(t, filepath.Join(repo, ".gitignore"), []byte("ignored-b.txt\n"))
		runCmd(t, repo, "git", "add", ".gitignore")
		runCmd(t, repo, "git", "commit", "-m", "add ignore")
		writeFile(t, filepath.Join(repo, "ignored-b.txt"), []byte("seed\n"))
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "wc -c ignored-b.txt | awk '{print $1}' > masked-size.txt"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--ignored-mode", "masked"}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("masked-size.txt")) {
			t.Fatalf("patch missing masked-size.txt")
		}
		if !bytes.Contains(data, []byte("\n+0\n")) {
			t.Fatalf("expected masked file to be empty")
		}
	})

	t.Run("ignored max zero disables masking", func(t *testing.T) {
		repo := createRepo(t)
		writeFile(t, filepath.Join(repo, ".gitignore"), []byte("ignored-zero.txt\n"))
		runCmd(t, repo, "git", "add", ".gitignore")
		runCmd(t, repo, "git", "commit", "-m", "add ignore")
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "echo data > ignored-zero.txt; echo $? > code.txt"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--ignored-mode", "readonly", "--ignored-max", "0"}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("code.txt")) {
			t.Fatalf("patch missing code.txt")
		}
		if bytes.Contains(data, []byte("ignored-zero.txt")) {
			t.Fatalf("ignored file should be excluded from patch")
		}
	})

	t.Run("ignored directory readonly", func(t *testing.T) {
		repo := createRepo(t)
		writeFile(t, filepath.Join(repo, ".gitignore"), []byte("ignored-dir/\n"))
		runCmd(t, repo, "git", "add", ".gitignore")
		runCmd(t, repo, "git", "commit", "-m", "add ignore dir")
		if err := os.MkdirAll(filepath.Join(repo, "ignored-dir"), 0o755); err != nil {
			t.Fatalf("mkdir ignored-dir: %v", err)
		}
		writeFile(t, filepath.Join(repo, "ignored-dir", "seed.txt"), []byte("seed\n"))
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "echo data > ignored-dir/file.txt; echo $? > code.txt"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--ignored-mode", "readonly"}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("code.txt")) {
			t.Fatalf("patch missing code.txt")
		}
		if !containsNonZeroLine(data) {
			t.Fatalf("expected readonly write failure marker")
		}
	})

	t.Run("ignored directory masked", func(t *testing.T) {
		repo := createRepo(t)
		writeFile(t, filepath.Join(repo, ".gitignore"), []byte("ignored-dir/\n"))
		runCmd(t, repo, "git", "add", ".gitignore")
		runCmd(t, repo, "git", "commit", "-m", "add ignore dir")
		if err := os.MkdirAll(filepath.Join(repo, "ignored-dir"), 0o755); err != nil {
			t.Fatalf("mkdir ignored-dir: %v", err)
		}
		writeFile(t, filepath.Join(repo, "ignored-dir", "seed.txt"), []byte("seed\n"))
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "ls -A ignored-dir | wc -l > count.txt"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--ignored-mode", "masked"}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("count.txt")) {
			t.Fatalf("patch missing count.txt")
		}
		if !bytes.Contains(data, []byte("\n+0\n")) {
			t.Fatalf("expected masked directory to be empty")
		}
	})

	t.Run("ignored max exceeded fails early", func(t *testing.T) {
		repo := createRepo(t)
		writeFile(t, filepath.Join(repo, ".gitignore"), []byte("ignored-a.txt\nignored-b.txt\n"))
		runCmd(t, repo, "git", "add", ".gitignore")
		runCmd(t, repo, "git", "commit", "-m", "add ignore")
		writeFile(t, filepath.Join(repo, "ignored-a.txt"), []byte("seed\n"))
		writeFile(t, filepath.Join(repo, "ignored-b.txt"), []byte("seed\n"))
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--ignored-mode", "readonly", "--ignored-max", "1"}, []string{"sh", "-c", "true"}, nil)
		if code != 10 {
			t.Fatalf("expected exit 10 got %d", code)
		}
	})

	t.Run("new ignored path within ignored max fails export", func(t *testing.T) {
		repo := createRepo(t)
		writeFile(t, filepath.Join(repo, ".gitignore"), []byte("ignored-a.txt\nignored-b.txt\n"))
		runCmd(t, repo, "git", "add", ".gitignore")
		runCmd(t, repo, "git", "commit", "-m", "add ignore")
		writeFile(t, filepath.Join(repo, "ignored-a.txt"), []byte("seed\n"))
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "echo data > ignored-b.txt"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--ignored-mode", "readonly", "--ignored-max", "2"}, []string{"sh", "-c", cmd}, nil)
		if code != 13 {
			t.Fatalf("expected exit 13 got %d", code)
		}
	})

	t.Run("apply reject returns failure", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "bad.patch")
		writeFile(t, patchPath, []byte("diff --git a/missing.txt b/missing.txt\nindex 0000000..1111111 100644\n--- a/missing.txt\n+++ b/missing.txt\n@@ -1 +1 @@\n+oops\n"))
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--apply-mode", "reject"}, []string{"sh", "-c", "true"}, nil)
		if code != 12 {
			t.Fatalf("expected exit 12 got %d", code)
		}
	})

	t.Run("keep session always", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--keep-session", "always"}, []string{"sh", "-c", "true"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		sessions := filepath.Join(repo, ".git", "persona", "sessions")
		entries, err := os.ReadDir(sessions)
		if err != nil || len(entries) == 0 {
			t.Fatalf("expected session directory kept")
		}
	})

	t.Run("keep session never", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--keep-session", "never"}, []string{"sh", "-c", "true"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		sessions := filepath.Join(repo, ".git", "persona", "sessions")
		entries, err := os.ReadDir(sessions)
		if err == nil && len(entries) > 0 {
			t.Fatalf("expected sessions to be removed")
		}
	})

	t.Run("keep session on-fail removes successful session", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--keep-session", "on-fail"}, []string{"sh", "-c", "true"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		sessions := filepath.Join(repo, ".git", "persona", "sessions")
		entries, err := os.ReadDir(sessions)
		if err == nil && len(entries) > 0 {
			t.Fatalf("expected successful on-fail run to remove sessions")
		}
	})

	t.Run("allow dirty toggles", func(t *testing.T) {
		repo := createRepo(t)
		writeFile(t, filepath.Join(repo, "tracked.txt"), []byte("dirty\n"))
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "true"}, nil)
		if code != 11 {
			t.Fatalf("expected exit 11 got %d", code)
		}
		code, _, _ = runPersona(t, persona, repo, []string{"--patch", patchPath, "--allow-dirty"}, []string{"sh", "-c", "true"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
	})

	t.Run("tracked ignored still exported", func(t *testing.T) {
		repo := createRepo(t)
		writeFile(t, filepath.Join(repo, ".gitignore"), []byte("tracked.txt\n"))
		runCmd(t, repo, "git", "add", ".gitignore")
		runCmd(t, repo, "git", "commit", "-m", "ignore tracked")
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "echo changed > tracked.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("tracked.txt")) {
			t.Fatalf("tracked file should be in patch even if ignored")
		}
	})

	t.Run("signal forwarding preserves export", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "trap 'echo sig > sig.txt; exit 0' INT TERM; sleep 5"
		code, _, _ := runPersonaWithSignal(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil, syscall.SIGINT, 500*time.Millisecond)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("sig.txt")) {
			t.Fatalf("patch missing sig.txt")
		}
	})

	t.Run("sigterm returns 143 and reaps ignored grandchild", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := strings.Join([]string{
			"(trap '' TERM; while :; do sleep 1; done) &",
			"child=$!",
			"echo $child > grandchild.pid",
			"echo before > before.txt",
			"wait",
		}, "\n")
		code, out, errOut := runPersonaWithSignal(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil, syscall.SIGTERM, 500*time.Millisecond)
		if code != 143 {
			t.Fatalf("expected exit 143 got %d stdout=%q stderr=%q", code, out, errOut)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("before.txt")) {
			t.Fatalf("patch missing before.txt")
		}
		if !bytes.Contains(data, []byte("grandchild.pid")) {
			t.Fatalf("patch missing grandchild.pid")
		}
		pidData := ""
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if line != "+++ b/grandchild.pid" {
				continue
			}
			for _, patchLine := range lines[i+1:] {
				if strings.HasPrefix(patchLine, "diff --git ") {
					break
				}
				if strings.HasPrefix(patchLine, "+") && !strings.HasPrefix(patchLine, "+++") {
					pidData = strings.TrimSpace(strings.TrimPrefix(patchLine, "+"))
					break
				}
			}
			if pidData != "" {
				break
			}
		}
		if pidData == "" {
			t.Fatalf("failed to extract grandchild pid from patch: %s", string(data))
		}
		pid, err := strconv.Atoi(pidData)
		if err != nil {
			t.Fatalf("parse grandchild pid %q: %v", pidData, err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			err = syscall.Kill(pid, 0)
			if err != nil && !errors.Is(err, syscall.EPERM) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("expected grandchild pid %d to be gone, last err=%v", pid, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	})

	t.Run("patch file permissions preserved", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		writeFile(t, patchPath, []byte(""))
		if err := os.Chmod(patchPath, 0o600); err != nil {
			t.Fatalf("chmod patch: %v", err)
		}
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "true"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		info, err := os.Stat(patchPath)
		if err != nil {
			t.Fatalf("stat patch: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("expected mode 600 got %o", info.Mode().Perm())
		}
	})

	t.Run("repo path with spaces", func(t *testing.T) {
		base := t.TempDir()
		repoPath := filepath.Join(base, "repo with space")
		repo := createRepoAt(t, repoPath)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "echo ok > spaced.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("spaced.txt")) {
			t.Fatalf("patch missing spaced.txt")
		}
	})

	t.Run("invalid base ref", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--base-mode", "worktree", "--base-ref", "nope"}, []string{"sh", "-c", "true"}, nil)
		if code != 11 {
			t.Fatalf("expected exit 11 got %d", code)
		}
	})

	t.Run("binary file export", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "printf '\\000\\001\\002' > binary.dat"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("binary.dat")) {
			t.Fatalf("expected binary.dat in patch")
		}
		if !(bytes.Contains(data, []byte("GIT binary patch")) || bytes.Contains(data, []byte("literal ")) || bytes.Contains(data, []byte("Binary files"))) {
			t.Fatalf("expected binary patch content")
		}
	})

	t.Run("untracked filename with spaces", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "mkdir -p 'dir with space'; echo data > 'dir with space/file name.txt'"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("dir with space/file name.txt")) {
			t.Fatalf("patch missing spaced filename")
		}
	})

	t.Run("untracked filename with leading dash", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "echo data > -dash.txt"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("-dash.txt")) {
			t.Fatalf("patch missing dashed filename")
		}
	})

	t.Run("unicode path and filename", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "dir=$(printf '\\354\\234\\240\\353\\213\\210\\354\\275\\224\\353\\223\\234'); file=$(printf '\\355\\214\\214\\354\\235\\274'); mkdir -p \"$dir\"; echo data > \"$dir/$file.txt\""
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		unicodePath := "\uC720\uB2C8\uCF54\uB4DC/\uD30C\uC77C.txt"
		if !bytes.Contains(data, []byte(unicodePath)) {
			t.Fatalf("patch missing unicode path")
		}
	})

	t.Run("untracked filename with punctuation", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "mkdir -p 'weird[dir]'; echo data > 'weird[dir]/name!@#$.txt'"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("weird[dir]/name!@#$.txt")) {
			t.Fatalf("patch missing punctuation filename")
		}
	})

	t.Run("touching tracked file does not create diff", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "touch tracked.txt"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		info, err := os.Stat(patchPath)
		if err != nil {
			t.Fatalf("patch not found: %v", err)
		}
		if info.Size() != 0 {
			t.Fatalf("expected empty patch, got %d", info.Size())
		}
	})

	t.Run("rename a to b to a with change", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "mv tracked.txt temp.txt; echo changed >> temp.txt; mv temp.txt tracked.txt"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("tracked.txt")) {
			t.Fatalf("patch missing tracked.txt")
		}
		if bytes.Contains(data, []byte("temp.txt")) {
			t.Fatalf("patch should not mention temp.txt")
		}
	})

	t.Run("symlink export", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "ln -s tracked.txt link.txt"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("link.txt")) {
			t.Fatalf("patch missing link.txt")
		}
		if !bytes.Contains(data, []byte("120000")) {
			t.Fatalf("expected symlink mode in patch")
		}
	})

	t.Run("submodule export stays empty", func(t *testing.T) {
		sub := createRepo(t)
		repo := createRepo(t)
		runCmd(t, repo, "git", "-c", "protocol.file.allow=always", "submodule", "add", sub, "sub")
		runCmd(t, repo, "git", "add", ".gitmodules", "sub")
		runCmd(t, repo, "git", "commit", "-m", "add submodule")
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "true"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		info, err := os.Stat(patchPath)
		if err != nil {
			t.Fatalf("patch not found: %v", err)
		}
		if info.Size() != 0 {
			t.Fatalf("expected empty patch, got %d", info.Size())
		}
	})

	t.Run("rename tracked file", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "mv tracked.txt renamed.txt"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if bytes.Contains(data, []byte("rename from")) && bytes.Contains(data, []byte("rename to")) {
			return
		}
		if bytes.Contains(data, []byte("deleted file mode")) && bytes.Contains(data, []byte("new file mode")) {
			return
		}
		t.Fatalf("expected rename or delete+add in patch")
	})

	t.Run("delete tracked file", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "rm tracked.txt"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("deleted file mode")) {
			t.Fatalf("expected delete in patch")
		}
	})

	t.Run("special file skipped", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "mkfifo fifo.pipe"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if bytes.Contains(data, []byte("fifo.pipe")) {
			t.Fatalf("expected fifo to be skipped")
		}
	})

	t.Run("base ref selects worktree view", func(t *testing.T) {
		repo := createRepo(t)
		writeFile(t, filepath.Join(repo, "tracked.txt"), []byte("next\n"))
		runCmd(t, repo, "git", "add", "tracked.txt")
		runCmd(t, repo, "git", "commit", "-m", "next")
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, out, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--base-mode", "worktree", "--base-ref", "HEAD~1"}, []string{"cat", "tracked.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		if strings.TrimSpace(out) != "base" {
			t.Fatalf("expected base content, got %q", out)
		}
	})

	t.Run("apply failure keeps session", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "bad.patch")
		writeFile(t, patchPath, []byte("diff --git a/missing.txt b/missing.txt\nindex 0000000..1111111 100644\n--- a/missing.txt\n+++ b/missing.txt\n@@ -1 +1 @@\n+oops\n"))
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "true"}, nil)
		if code != 12 {
			t.Fatalf("expected exit 12 got %d", code)
		}
		sessions := filepath.Join(repo, ".git", "persona", "sessions")
		entries, err := os.ReadDir(sessions)
		if err != nil {
			t.Fatalf("failed to read sessions: %v", err)
		}
		if len(entries) == 0 {
			t.Fatalf("expected session directory to be kept")
		}
	})

	t.Run("mount failure exit code", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "true"}, map[string]string{"PERSONA_FORCE_MOUNT_FAIL": "1"})
		if code != 10 {
			t.Fatalf("expected exit 10 got %d", code)
		}
	})

	t.Run("custom patch dir auto create and reapply", func(t *testing.T) {
		repo := createRepo(t)
		customDir := filepath.Join(t.TempDir(), "custom-patches")
		code, out, _ := runPersona(t, persona, repo, []string{"--patch-dir", customDir, "--print-patch-path"}, []string{"sh", "-c", "echo custom > custom.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		patchPath := strings.TrimSpace(out)
		if patchPath == "" {
			t.Fatalf("expected printed patch path")
		}
		rel, err := filepath.Rel(customDir, patchPath)
		if err != nil {
			t.Fatalf("rel patch path: %v", err)
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			t.Fatalf("expected patch path under custom dir %s, got %s", customDir, patchPath)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("custom.txt")) {
			t.Fatalf("patch missing custom.txt")
		}
		code, out, _ = runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"cat", "custom.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		if strings.TrimSpace(out) != "custom" {
			t.Fatalf("expected custom patched view, got %q", out)
		}
	})
}
