//go:build linux
// +build linux

package integration

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	buildOnce  sync.Once
	personaBin string
	buildErr   error
)

func requireIntegration(t *testing.T) string {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	if os.Getenv("PERSONA_INTEGRATION") != "1" {
		t.Skip("set PERSONA_INTEGRATION=1 to run")
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root for mount namespace")
	}
	return buildPersona(t)
}

func TestPersonaIntegrationBasic(t *testing.T) {
	persona := requireIntegration(t)

	t.Run("empty patch auto create", func(t *testing.T) {
		repo := createRepo(t)
		code, out, _ := runPersona(t, persona, repo, []string{"--print-patch-path"}, []string{"sh", "-c", "true"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		path := strings.TrimSpace(out)
		if path == "" {
			t.Fatalf("expected patch path output")
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("patch file not created: %v", err)
		}
		if info.Size() != 0 {
			t.Fatalf("expected empty patch file, got %d", info.Size())
		}
	})
	t.Run("tracked change and reapply view", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "echo changed > tracked.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("tracked.txt")) {
			t.Fatalf("patch missing tracked change")
		}
		code, out, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"cat", "tracked.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		if strings.TrimSpace(out) != "changed" {
			t.Fatalf("expected patched view, got %q", out)
		}
	})
	t.Run("untracked included", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "echo new > new.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("new file mode")) || !bytes.Contains(data, []byte("new.txt")) {
			t.Fatalf("patch missing new file")
		}
	})
	t.Run("ignored excluded", func(t *testing.T) {
		repo := createRepo(t)
		writeFile(t, filepath.Join(repo, ".gitignore"), []byte("ignored.txt\n"))
		runCmd(t, repo, "git", "add", ".gitignore")
		runCmd(t, repo, "git", "commit", "-m", "add ignore")
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "echo ignored > ignored.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if bytes.Contains(data, []byte("ignored.txt")) {
			t.Fatalf("ignored file should not be in patch")
		}
	})
	t.Run("worktree base ignores dirty", func(t *testing.T) {
		repo := createRepo(t)
		writeFile(t, filepath.Join(repo, "tracked.txt"), []byte("dirty\n"))
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--base-mode", "worktree"}, []string{"sh", "-c", "true"}, nil)
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
	t.Run("command failure still writes patch", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "false"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		info, err := os.Stat(patchPath)
		if err != nil {
			t.Fatalf("patch file not created: %v", err)
		}
		if info.Size() != 0 {
			t.Fatalf("expected empty patch, got %d", info.Size())
		}
	})
	t.Run("git command fails inside view", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "git status > git-status.out 2> git-status.err; echo $? > git-status.code"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
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
			t.Fatalf("expected git failure output in patch")
		}
	})
	t.Run("nested child process inherits view", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "sh -c 'test ! -e .git/HEAD && echo child > child.txt'"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("child.txt")) {
			t.Fatalf("patch missing child.txt")
		}
	})
	t.Run("tracked and untracked combined", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "echo changed > tracked.txt; echo new > combo.txt"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("tracked.txt")) {
			t.Fatalf("patch missing tracked.txt")
		}
		if !bytes.Contains(data, []byte("combo.txt")) {
			t.Fatalf("patch missing combo.txt")
		}
		if !bytes.Contains(data, []byte("new file mode")) {
			t.Fatalf("patch missing new file entry")
		}
	})
}

func TestPersonaIntegrationConcurrency(t *testing.T) {
	persona := requireIntegration(t)

	t.Run("serialize patch lock", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		start := time.Now()
		done := make(chan struct{})
		go func() {
			runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "sleep 2"}, nil)
			close(done)
		}()
		time.Sleep(200 * time.Millisecond)
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "true"}, nil)
		elapsed := time.Since(start)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		if elapsed < 2*time.Second {
			t.Fatalf("expected lock serialization, elapsed %s", elapsed)
		}
		<-done
	})

	t.Run("concurrent same patch accumulates changes", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "echo one > one.txt"}, nil)
		}()
		go func() {
			defer wg.Done()
			runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "echo two > two.txt"}, nil)
		}()
		wg.Wait()
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("one.txt")) {
			t.Fatalf("patch missing one.txt")
		}
		if !bytes.Contains(data, []byte("two.txt")) {
			t.Fatalf("patch missing two.txt")
		}
	})

	t.Run("multi instance sequential", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		files := []string{"seq-1.txt", "seq-2.txt", "seq-3.txt"}
		for _, name := range files {
			code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "echo data > " + name}, nil)
			if code != 0 {
				t.Fatalf("expected exit 0 got %d", code)
			}
		}
		data := readFile(t, patchPath)
		for _, name := range files {
			if !bytes.Contains(data, []byte(name)) {
				t.Fatalf("patch missing %s", name)
			}
		}
	})

	t.Run("multi instance concurrent", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		files := []string{"conc-1.txt", "conc-2.txt", "conc-3.txt", "conc-4.txt"}
		var wg sync.WaitGroup
		errCh := make(chan error, len(files))
		for _, name := range files {
			wg.Add(1)
			go func(fname string) {
				defer wg.Done()
				code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "echo data > " + fname}, nil)
				if code != 0 {
					errCh <- fmt.Errorf("exit %d for %s", code, fname)
				}
			}(name)
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			if err != nil {
				t.Fatal(err)
			}
		}
		data := readFile(t, patchPath)
		for _, name := range files {
			if !bytes.Contains(data, []byte(name)) {
				t.Fatalf("patch missing %s", name)
			}
		}
	})

	t.Run("multi instance concurrent with delay", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		files := []string{"delay-1.txt", "delay-2.txt", "delay-3.txt", "delay-4.txt", "delay-5.txt"}
		var wg sync.WaitGroup
		errCh := make(chan error, len(files))
		for _, name := range files {
			wg.Add(1)
			go func(fname string) {
				defer wg.Done()
				cmd := "sleep 0.2; echo data > " + fname
				code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
				if code != 0 {
					errCh <- fmt.Errorf("exit %d for %s", code, fname)
				}
			}(name)
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			if err != nil {
				t.Fatal(err)
			}
		}
		data := readFile(t, patchPath)
		for _, name := range files {
			if !bytes.Contains(data, []byte(name)) {
				t.Fatalf("patch missing %s", name)
			}
		}
	})

	t.Run("five concurrent same patch same file", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		contents := []string{"A", "B", "C", "D", "E"}
		var wg sync.WaitGroup
		for i, text := range contents {
			wg.Add(1)
			go func(idx int, value string) {
				defer wg.Done()
				cmd := fmt.Sprintf("sleep 0.%d; printf '%s\\n' > shared.txt", idx, value)
				runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
			}(i, text)
		}
		wg.Wait()
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("shared.txt")) {
			t.Fatalf("patch missing shared.txt")
		}
		if !bytes.Contains(data, []byte("\n+")) {
			t.Fatalf("patch missing shared content")
		}
	})

	t.Run("concurrent different patches same repo same file", func(t *testing.T) {
		repo := createRepo(t)
		patchA := filepath.Join(t.TempDir(), "a.patch")
		patchB := filepath.Join(t.TempDir(), "b.patch")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			cmd := "sleep 0.2; echo A > shared.txt"
			runPersona(t, persona, repo, []string{"--patch", patchA}, []string{"sh", "-c", cmd}, nil)
		}()
		go func() {
			defer wg.Done()
			cmd := "sleep 0.2; echo B > shared.txt"
			runPersona(t, persona, repo, []string{"--patch", patchB}, []string{"sh", "-c", cmd}, nil)
		}()
		wg.Wait()
		dataA := readFile(t, patchA)
		dataB := readFile(t, patchB)
		if !bytes.Contains(dataA, []byte("shared.txt")) || !bytes.Contains(dataA, []byte("\n+A\n")) {
			t.Fatalf("patch A missing shared.txt content")
		}
		if !bytes.Contains(dataB, []byte("shared.txt")) || !bytes.Contains(dataB, []byte("\n+B\n")) {
			t.Fatalf("patch B missing shared.txt content")
		}
	})

	t.Run("five concurrent same patch different files", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		files := []string{"c1.txt", "c2.txt", "c3.txt", "c4.txt", "c5.txt"}
		var wg sync.WaitGroup
		for _, name := range files {
			wg.Add(1)
			go func(fname string) {
				defer wg.Done()
				cmd := "sleep 0.1; echo data > " + fname
				runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", cmd}, nil)
			}(name)
		}
		wg.Wait()
		data := readFile(t, patchPath)
		for _, name := range files {
			if !bytes.Contains(data, []byte(name)) {
				t.Fatalf("patch missing %s", name)
			}
		}
	})

	t.Run("lock is per patch file", func(t *testing.T) {
		repo := createRepo(t)
		patchA := filepath.Join(t.TempDir(), "a.patch")
		patchB := filepath.Join(t.TempDir(), "b.patch")
		start := time.Now()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			runPersona(t, persona, repo, []string{"--patch", patchA}, []string{"sh", "-c", "sleep 1"}, nil)
		}()
		go func() {
			defer wg.Done()
			runPersona(t, persona, repo, []string{"--patch", patchB}, []string{"sh", "-c", "sleep 1"}, nil)
		}()
		wg.Wait()
		elapsed := time.Since(start)
		if elapsed > 1800*time.Millisecond {
			t.Fatalf("expected parallel patches, elapsed %s", elapsed)
		}
	})

	t.Run("parallel patches keep independent views", func(t *testing.T) {
		repo := createRepo(t)
		patchA := filepath.Join(t.TempDir(), "view-a.patch")
		patchB := filepath.Join(t.TempDir(), "view-b.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchA}, []string{"sh", "-c", "echo a > a.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		code, _, _ = runPersona(t, persona, repo, []string{"--patch", patchB}, []string{"sh", "-c", "echo b > b.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			runPersona(t, persona, repo, []string{"--patch", patchA}, []string{"sh", "-c", "cat a.txt > seen-a.txt"}, nil)
		}()
		go func() {
			defer wg.Done()
			runPersona(t, persona, repo, []string{"--patch", patchB}, []string{"sh", "-c", "cat b.txt > seen-b.txt"}, nil)
		}()
		wg.Wait()
		dataA := readFile(t, patchA)
		dataB := readFile(t, patchB)
		if !bytes.Contains(dataA, []byte("a.txt")) || !bytes.Contains(dataA, []byte("seen-a.txt")) {
			t.Fatalf("patch A missing expected files")
		}
		if !bytes.Contains(dataB, []byte("b.txt")) || !bytes.Contains(dataB, []byte("seen-b.txt")) {
			t.Fatalf("patch B missing expected files")
		}
	})

	t.Run("same patch different repos serialize", func(t *testing.T) {
		repoA := createRepo(t)
		repoB := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "shared.patch")
		start := time.Now()
		done := make(chan struct{})
		go func() {
			runPersona(t, persona, repoA, []string{"--patch", patchPath}, []string{"sh", "-c", "sleep 2"}, nil)
			close(done)
		}()
		time.Sleep(200 * time.Millisecond)
		code, _, _ := runPersona(t, persona, repoB, []string{"--patch", patchPath}, []string{"sh", "-c", "true"}, nil)
		elapsed := time.Since(start)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		if elapsed < 2*time.Second {
			t.Fatalf("expected lock serialization across repos, elapsed %s", elapsed)
		}
		<-done
	})

	t.Run("concurrent worktree mode different patches", func(t *testing.T) {
		repo := createRepo(t)
		patchA := filepath.Join(t.TempDir(), "wta.patch")
		patchB := filepath.Join(t.TempDir(), "wtb.patch")
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			runPersona(t, persona, repo, []string{"--patch", patchA, "--base-mode", "worktree"}, []string{"sh", "-c", "echo a > wt-a.txt"}, nil)
		}()
		go func() {
			defer wg.Done()
			runPersona(t, persona, repo, []string{"--patch", patchB, "--base-mode", "worktree"}, []string{"sh", "-c", "echo b > wt-b.txt"}, nil)
		}()
		wg.Wait()
		dataA := readFile(t, patchA)
		dataB := readFile(t, patchB)
		if !bytes.Contains(dataA, []byte("wt-a.txt")) {
			t.Fatalf("patch A missing wt-a.txt")
		}
		if !bytes.Contains(dataB, []byte("wt-b.txt")) {
			t.Fatalf("patch B missing wt-b.txt")
		}
	})
}

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
}

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
	t.Run("ignored max limits scope", func(t *testing.T) {
		repo := createRepo(t)
		writeFile(t, filepath.Join(repo, ".gitignore"), []byte("ignored-a.txt\nignored-b.txt\n"))
		runCmd(t, repo, "git", "add", ".gitignore")
		runCmd(t, repo, "git", "commit", "-m", "add ignore")
		writeFile(t, filepath.Join(repo, "ignored-a.txt"), []byte("seed\n"))
		writeFile(t, filepath.Join(repo, "ignored-b.txt"), []byte("seed\n"))
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		cmd := "echo data > ignored-a.txt; echo $? > code-a.txt; echo data > ignored-b.txt; echo $? > code-b.txt"
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath, "--ignored-mode", "readonly", "--ignored-max", "1"}, []string{"sh", "-c", cmd}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("code-a.txt")) || !bytes.Contains(data, []byte("code-b.txt")) {
			t.Fatalf("patch missing code outputs")
		}
		if !containsNonZeroLine(data) {
			t.Fatalf("expected at least one readonly failure")
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
}

func buildPersona(t *testing.T) string {
	buildOnce.Do(func() {
		tmp, err := os.MkdirTemp("", "persona-bin-")
		if err != nil {
			buildErr = fmt.Errorf("create temp dir: %v", err)
			return
		}
		personaBin = filepath.Join(tmp, "persona")
		cmd := exec.Command("go", "build", "-o", personaBin, "./cmd/persona")
		cmd.Dir = repoRoot()
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("build failed: %v: %s", err, string(out))
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return personaBin
}

func repoRoot() string {
	cwd, _ := os.Getwd()
	return filepath.Dir(cwd)
}

func createRepo(t *testing.T) string {
	dir := t.TempDir()
	return createRepoAt(t, dir)
}

func createRepoAt(t *testing.T, dir string) string {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runCmd(t, dir, "git", "init")
	runCmd(t, dir, "git", "config", "user.email", "you@example.com")
	runCmd(t, dir, "git", "config", "user.name", "You")
	writeFile(t, filepath.Join(dir, "tracked.txt"), []byte("base\n"))
	runCmd(t, dir, "git", "add", "tracked.txt")
	runCmd(t, dir, "git", "commit", "-m", "init")
	return dir
}

func createEmptyRepo(t *testing.T) string {
	dir := t.TempDir()
	runCmd(t, dir, "git", "init")
	runCmd(t, dir, "git", "config", "user.email", "you@example.com")
	runCmd(t, dir, "git", "config", "user.name", "You")
	return dir
}

func runPersona(t *testing.T, persona, repo string, args []string, cmdArgs []string, extraEnv map[string]string) (int, string, string) {
	if os.Getenv("PERSONA_TEST_VERBOSE") == "1" {
		args = append(args, "--verbose")
	}
	fullArgs := append([]string{}, args...)
	fullArgs = append(fullArgs, "--")
	fullArgs = append(fullArgs, cmdArgs...)
	cmd := exec.Command(persona, fullArgs...)
	cmd.Dir = repo
	cmd.Env = append([]string{}, os.Environ()...)
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			code = 127
		}
	}
	if os.Getenv("PERSONA_TEST_VERBOSE") == "1" && stderr.Len() > 0 {
		fmt.Fprintln(os.Stderr, "persona stderr:")
		fmt.Fprintln(os.Stderr, stderr.String())
	}
	return code, stdout.String(), stderr.String()
}

func runPersonaWithSignal(t *testing.T, persona, repo string, args []string, cmdArgs []string, extraEnv map[string]string, sig os.Signal, delay time.Duration) (int, string, string) {
	if os.Getenv("PERSONA_TEST_VERBOSE") == "1" {
		args = append(args, "--verbose")
	}
	fullArgs := append([]string{}, args...)
	fullArgs = append(fullArgs, "--")
	fullArgs = append(fullArgs, cmdArgs...)
	cmd := exec.Command(persona, fullArgs...)
	cmd.Dir = repo
	cmd.Env = append([]string{}, os.Environ()...)
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start persona: %v", err)
	}
	time.Sleep(delay)
	_ = cmd.Process.Signal(sig)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		code := 0
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				code = exitErr.ExitCode()
			} else {
				code = 127
			}
		}
		return code, stdout.String(), stderr.String()
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("persona did not exit after signal")
	}
	return 127, stdout.String(), stderr.String()
}

func containsNonZeroLine(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "+") && len(line) > 1 {
			ch := line[1]
			if ch >= '1' && ch <= '9' {
				return true
			}
		}
	}
	return false
}

func runCmd(t *testing.T, dir, name string, args ...string) string {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("command failed: %s %v: %v: %s", name, args, err, string(out))
	}
	return string(out)
}

func writeFile(t *testing.T, path string, data []byte) {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func readFile(t *testing.T, path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	return data
}
