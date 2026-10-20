//go:build linux

package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func daemonCLI(t *testing.T, binary, repo string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, append([]string{"daemon"}, args...)...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "PERSONA_REPO_PATH="+repo)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("daemon %v: %v: %s", args, err, out)
	}
	return out
}

func startTestDaemon(t *testing.T, binary, repo string) func() {
	return startManagedTestDaemon(t, binary, repo).stop
}

type testDaemon struct {
	stop           func()
	stopPreserving func()
	crash          func()
	beginShutdown  func()
}

func testDaemonSocket(repo string) string {
	digest := sha256.Sum256([]byte(filepath.Join(repo, ".git")))
	return filepath.Join("/tmp", fmt.Sprintf("persona-%d", os.Getuid()), hex.EncodeToString(digest[:16])+".sock")
}

func startManagedTestDaemon(t *testing.T, binary, repo string) testDaemon {
	t.Helper()
	log, err := os.CreateTemp(t.TempDir(), "daemon-*.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "daemon", "serve", "--repo-root", repo, "--git-dir", filepath.Join(repo, ".git"))
	cmd.Dir = repo
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	shutdown := func(sig syscall.Signal, expectPreserved bool) {
		if stopped {
			return
		}
		stopped = true
		_ = cmd.Process.Signal(sig)
		select {
		case err := <-done:
			if expectPreserved && err == nil {
				t.Error("daemon silently reported a successful shutdown while preserving unsaved state")
			} else if err != nil && !expectPreserved && sig != syscall.SIGKILL {
				t.Errorf("daemon exit: %v", err)
			}
		case <-time.After(40 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("daemon shutdown timed out")
		}
		_ = log.Close()
	}
	stop := func() { shutdown(syscall.SIGTERM, false) }
	t.Cleanup(stop)
	socket := testDaemonSocket(repo)
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("unix", socket, 50*time.Millisecond)
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
			_ = json.NewEncoder(conn).Encode(map[string]string{"method": "ping", "repo_root": repo, "git_dir": filepath.Join(repo, ".git")})
			var pong struct {
				Version int `json:"version"`
			}
			err = json.NewDecoder(conn).Decode(&pong)
			_ = conn.Close()
			if err == nil && pong.Version == 3 {
				return testDaemon{
					stop:           stop,
					stopPreserving: func() { shutdown(syscall.SIGTERM, true) },
					crash:          func() { shutdown(syscall.SIGKILL, false) },
					beginShutdown: func() {
						if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
							t.Fatal(err)
						}
					},
				}
			}
		}
		select {
		case err := <-done:
			stopped = true
			t.Fatalf("daemon startup failed: %v; log %s", err, log.Name())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon startup timeout; log %s", log.Name())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestPersonaIntegrationDaemonAbsolutePathsAndShutdown(t *testing.T) {
	binary := requireIntegration(t)
	repo := createRepo(t)
	original := readFile(t, filepath.Join(repo, "tracked.txt"))
	writeFile(t, filepath.Join(repo, "edit.sh"), []byte("printf changed > \"$PERSONA_REPO_PATH/tracked.txt\"\n"))
	runCmd(t, repo, "git", "add", "edit.sh")
	runCmd(t, repo, "git", "commit", "-m", "add absolute-path script")
	stop := startTestDaemon(t, binary, repo)
	for _, kind := range []string{"relative", "absolute", "environment", "script"} {
		t.Run(kind, func(t *testing.T) {
			args := []string{"exec", "--session-key", kind, "--", "sh", "-c", "printf changed > tracked.txt"}
			switch kind {
			case "absolute":
				args = []string{"exec", "--session-key", kind, "--", "sh", "-c", "printf changed > \"$1\"", "writer", filepath.Join(repo, "tracked.txt")}
			case "environment":
				args = []string{"exec", "--session-key", kind, "--", "sh", "-c", "printf changed > \"$PERSONA_REPO_PATH/tracked.txt\""}
			case "script":
				args = []string{"exec", "--session-key", kind, "--", "sh", "edit.sh"}
			}
			daemonCLI(t, binary, repo, args...)
			if got := readFile(t, filepath.Join(repo, "tracked.txt")); !bytes.Equal(got, original) {
				t.Fatalf("original checkout changed: %q", got)
			}
			got := daemonCLI(t, binary, repo, "exec", "--session-key", kind, "--", "cat", "tracked.txt")
			if string(got) != "changed" {
				t.Fatalf("view lost mutation: %q", got)
			}
			daemonCLI(t, binary, repo, "end", "--session-key", kind)
		})
	}
	// The recursive mapping must retain nested .git and ignored-file masks.
	writeFile(t, filepath.Join(repo, ".gitignore"), []byte("ignored.txt\n"))
	writeFile(t, filepath.Join(repo, "ignored.txt"), []byte("original ignored\n"))
	runCmd(t, repo, "git", "add", ".gitignore")
	runCmd(t, repo, "git", "commit", "-m", "ignore fixture")
	for _, mode := range []string{"readonly", "masked"} {
		t.Run("nested "+mode, func(t *testing.T) {
			payload := "test ! -e .git/HEAD && test ! -e \"$PERSONA_REPO_PATH/.git/HEAD\""
			if mode == "readonly" {
				payload += "; if printf corrupt > \"$PERSONA_REPO_PATH/ignored.txt\" 2>/dev/null; then exit 9; fi"
			} else {
				payload += " && test ! -s \"$PERSONA_REPO_PATH/ignored.txt\""
			}
			daemonCLI(t, binary, repo, "exec", "--session-key", mode, "--base-mode", "repo", "--ignored-mode", mode, "--", "sh", "-c", payload)
			if got := readFile(t, filepath.Join(repo, "ignored.txt")); string(got) != "original ignored\n" {
				t.Fatalf("original ignored file changed: %q", got)
			}
			daemonCLI(t, binary, repo, "end", "--session-key", mode)
		})
	}
	var info struct {
		ViewPath string `json:"view_path"`
		Dirty    bool   `json:"dirty"`
		BaseOID  string `json:"base_oid"`
	}
	if err := json.Unmarshal(daemonCLI(t, binary, repo, "info", "--session-key", "shutdown", "--json"), &info); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(info.ViewPath, "tracked.txt"), []byte("shutdown saved\n"))
	daemonCLI(t, binary, repo, "mark-dirty", "--session-key", "shutdown")
	base := info.BaseOID
	stop()
	stop = startTestDaemon(t, binary, repo)
	if err := json.Unmarshal(daemonCLI(t, binary, repo, "info", "--session-key", "shutdown", "--json"), &info); err != nil {
		t.Fatal(err)
	}
	if info.Dirty || info.BaseOID != base {
		t.Fatalf("shutdown state incorrect: %#v", info)
	}
	if got := readFile(t, filepath.Join(info.ViewPath, "tracked.txt")); string(got) != "shutdown saved\n" {
		t.Fatalf("normal shutdown lost bytes: %q", got)
	}
	daemonCLI(t, binary, repo, "end", "--session-key", "shutdown")
	stop()
}

// CI explicitly opts into cap_sys_admin and cap_dac_override and runs this as
// an ordinary user. OverlayFS needs DAC bypass for its mode-000 work directories.
// Keep this separate from the root-only suite so it cannot silently skip.
func TestPersonaIntegrationCapabilityPath(t *testing.T) {
	if os.Getenv("PERSONA_CAPABILITY_TEST") != "1" {
		t.Skip("set PERSONA_CAPABILITY_TEST=1 and PERSONA_CAPABILITY_BIN")
	}
	if os.Geteuid() == 0 {
		t.Fatal("capability test must run as a non-root user")
	}
	binary := os.Getenv("PERSONA_CAPABILITY_BIN")
	if !filepath.IsAbs(binary) {
		t.Fatal("PERSONA_CAPABILITY_BIN must be absolute")
	}
	repo := createRepo(t)
	patch := filepath.Join(t.TempDir(), "state.patch")
	code, _, stderr := runPersona(t, binary, repo, []string{"--patch", patch}, []string{"sh", "-c", "printf capability > tracked.txt"}, nil)
	if code != 0 {
		t.Fatalf("file capability execution failed: %d %s", code, stderr)
	}
	code, out, stderr := runPersona(t, binary, repo, []string{"--patch", patch}, []string{"cat", "tracked.txt"}, nil)
	if code != 0 || strings.TrimSpace(out) != "capability" {
		t.Fatalf("capability round-trip failed: %d %q %s", code, out, stderr)
	}
	if strings.TrimSpace(string(readFile(t, filepath.Join(repo, "tracked.txt")))) == "capability" {
		t.Fatal("capability execution modified source checkout")
	}
}
