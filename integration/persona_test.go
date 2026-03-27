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
		writeFile(t, filepath.Join(repo, "ignored.txt"), []byte("seed\n"))
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
		if code != 1 {
			t.Fatalf("expected child exit code 1 got %d", code)
		}
		info, err := os.Stat(patchPath)
		if err != nil {
			t.Fatalf("patch file not created: %v", err)
		}
		if info.Size() != 0 {
			t.Fatalf("expected empty patch, got %d", info.Size())
		}
	})
	t.Run("child exit code propagated", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "echo data > child.txt; exit 42"}, nil)
		if code != 42 {
			t.Fatalf("expected child exit code 42 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("child.txt")) {
			t.Fatalf("patch missing child.txt despite child exit")
		}
	})
	t.Run("reserved child exit code remapped", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "echo data > child.txt; exit 12"}, nil)
		if code != 242 {
			t.Fatalf("expected remapped child exit code 242 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("child.txt")) {
			t.Fatalf("patch missing child.txt despite child exit")
		}
	})
	t.Run("child success propagates zero", func(t *testing.T) {
		repo := createRepo(t)
		patchPath := filepath.Join(t.TempDir(), "state.patch")
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", patchPath}, []string{"sh", "-c", "echo ok > ok.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, patchPath)
		if !bytes.Contains(data, []byte("ok.txt")) {
			t.Fatalf("patch missing ok.txt")
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

	t.Run("real and symlink patch paths serialize", func(t *testing.T) {
		repo := createRepo(t)
		patchDir := t.TempDir()
		realPatch := filepath.Join(patchDir, "state.patch")
		linkPatch := filepath.Join(patchDir, "state.link.patch")
		if err := os.Symlink(realPatch, linkPatch); err != nil {
			t.Fatalf("symlink patch: %v", err)
		}
		start := time.Now()
		done := make(chan struct{})
		go func() {
			runPersona(t, persona, repo, []string{"--patch", realPatch}, []string{"sh", "-c", "sleep 2"}, nil)
			close(done)
		}()
		time.Sleep(200 * time.Millisecond)
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", linkPatch}, []string{"sh", "-c", "true"}, nil)
		elapsed := time.Since(start)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		if elapsed < 2*time.Second {
			t.Fatalf("expected serialization between real/symlink patch paths, elapsed %s", elapsed)
		}
		<-done
	})

	t.Run("real and symlink patch paths share state", func(t *testing.T) {
		repo := createRepo(t)
		patchDir := t.TempDir()
		realPatch := filepath.Join(patchDir, "state.patch")
		linkPatch := filepath.Join(patchDir, "state.link.patch")
		if err := os.Symlink(realPatch, linkPatch); err != nil {
			t.Fatalf("symlink patch: %v", err)
		}
		code, _, _ := runPersona(t, persona, repo, []string{"--patch", realPatch}, []string{"sh", "-c", "echo hello > one.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		code, _, _ = runPersona(t, persona, repo, []string{"--patch", linkPatch}, []string{"sh", "-c", "cat one.txt > seen.txt"}, nil)
		if code != 0 {
			t.Fatalf("expected exit 0 got %d", code)
		}
		data := readFile(t, realPatch)
		if !bytes.Contains(data, []byte("one.txt")) {
			t.Fatalf("patch missing one.txt")
		}
		if !bytes.Contains(data, []byte("seen.txt")) {
			t.Fatalf("patch missing seen.txt")
		}
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
