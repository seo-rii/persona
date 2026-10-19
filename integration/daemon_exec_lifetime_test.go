//go:build linux

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A normal grandchild must retain the mutation lease even when both the CLI
// owner and the executed command's group leader die. This deliberately does
// not use setsid or otherwise escape the registered process group.
func TestPersonaIntegrationDaemonExecOwnerDeathPreservesWriters(t *testing.T) {
	binary := requireIntegration(t)
	for _, restart := range []bool{false, true} {
		t.Run(fmt.Sprintf("restart=%t", restart), func(t *testing.T) {
			repo := createRepo(t)
			daemon := startManagedTestDaemon(t, binary, repo)
			const key = "surviving-writer"
			info := daemonInfo(t, binary, repo, key)
			barriers := t.TempDir()
			leaderPath := filepath.Join(barriers, "leader")
			writerPath := filepath.Join(barriers, "writer")
			releasePath := filepath.Join(barriers, "release")
			donePath := filepath.Join(barriers, "done")
			payload := `printf '%s' "$$" > "$LEADER_FILE"; sh -c 'printf "%s" "$$" > "$WRITER_FILE"; while [ ! -e "$RELEASE_FILE" ]; do sleep 0.02; done; printf "orphan write\n" > tracked.txt; printf done > "$DONE_FILE"' & wait`
			client := exec.Command(binary, "daemon", "exec", "--session-key", key, "--", "sh", "-c", payload)
			client.Dir = repo
			client.Env = append(os.Environ(), "LEADER_FILE="+leaderPath, "WRITER_FILE="+writerPath,
				"RELEASE_FILE="+releasePath, "DONE_FILE="+donePath)
			commandLog, err := os.Create(filepath.Join(barriers, "command.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer commandLog.Close()
			client.Stdout, client.Stderr = commandLog, commandLog
			if err := client.Start(); err != nil {
				t.Fatal(err)
			}
			clientDone := make(chan error, 1)
			go func() { clientDone <- client.Wait() }()
			var group int
			t.Cleanup(func() {
				_ = client.Process.Kill()
				if group > 0 {
					_ = syscall.Kill(-group, syscall.SIGKILL)
				}
			})
			waitExecutionFile(t, leaderPath)
			waitExecutionFile(t, writerPath)
			group, _ = strconv.Atoi(strings.TrimSpace(string(readFile(t, leaderPath))))
			writer, _ := strconv.Atoi(strings.TrimSpace(string(readFile(t, writerPath))))
			if group <= 0 || writer <= 0 {
				t.Fatal("missing execution identity")
			}
			if got, err := syscall.Getpgid(writer); err != nil || got != group {
				t.Fatalf("ordinary grandchild escaped its registered group: %d %v", got, err)
			}
			if err := client.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-clientDone:
			case <-time.After(5 * time.Second):
				t.Fatal("client did not exit")
			}
			if err := syscall.Kill(group, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			waitExecutionExit(t, group)
			daemonMustFail(t, binary, repo, "already executing", "exec", "--session-key", key, "--", "true")
			daemonMustFail(t, binary, repo, "still busy", "flush", "--session-key", key)
			if got := daemonInfo(t, binary, repo, key); !got.Busy || !got.Dirty {
				t.Fatalf("surviving writer was reported clean: %+v", got)
			}
			if restart {
				daemon.crash()
				daemon = startManagedTestDaemon(t, binary, repo)
				daemonMustFail(t, binary, repo, "still alive", "recover", "--session-key", key)
			}
			writeFile(t, releasePath, []byte("continue"))
			waitExecutionFile(t, donePath)
			waitExecutionExit(t, writer)
			if restart {
				daemonCLI(t, binary, repo, "recover", "--session-key", key)
			}
			// Normal shutdown must save the completed orphan's write instead of
			// treating the old client's death as a durable checkpoint.
			daemon.stop()
			materialized := materializeDaemonPatch(t, repo, info)
			if got := string(readFile(t, filepath.Join(materialized, "tracked.txt"))); got != "orphan write\n" {
				t.Fatalf("normal shutdown lost the surviving writer: %q", got)
			}
			assertOriginalCheckout(t, repo)
		})
	}
}

func waitExecutionFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) != 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("execution barrier not reached: %s", path)
}

func waitExecutionExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if os.IsNotExist(err) {
			return
		}
		if end := strings.LastIndex(string(data), ") "); end >= 0 {
			state := strings.Fields(string(data[end+2:]))
			if len(state) > 0 && (state[0] == "Z" || state[0] == "X") {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("execution process %d remains alive", pid)
}
