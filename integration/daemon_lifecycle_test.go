//go:build linux

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"persona/internal/ns"
	"persona/internal/session"
)

type daemonSnapshot struct {
	ViewPath              string `json:"view_path"`
	PatchPath             string `json:"patch_path"`
	BaseOID               string `json:"base_oid"`
	RecoveryRoot          string `json:"recovery_root"`
	State                 string `json:"state"`
	LastError             string `json:"last_error"`
	Dirty                 bool   `json:"dirty"`
	Busy                  bool   `json:"busy"`
	Generation            uint64 `json:"generation"`
	LastDurableGeneration uint64 `json:"last_durable_generation"`
	ViewGeneration        uint64 `json:"view_generation"`
}

type fileToolLease struct {
	Session daemonSnapshot `json:"session"`
	ID      string         `json:"lease_id"`
}

func daemonInfo(t *testing.T, binary, repo, key string) daemonSnapshot {
	t.Helper()
	var info daemonSnapshot
	if err := json.Unmarshal(daemonCLI(t, binary, repo, "info", "--session-key", key, "--json"), &info); err != nil {
		t.Fatal(err)
	}
	return info
}

func acquireFileTool(t *testing.T, binary, repo, key string, ownerPID int, options ...string) fileToolLease {
	t.Helper()
	args := append([]string{"acquire", "--session-key", key, "--owner-pid", strconv.Itoa(ownerPID), "--json"}, options...)
	var lease fileToolLease
	if err := json.Unmarshal(daemonCLI(t, binary, repo, args...), &lease); err != nil {
		t.Fatal(err)
	}
	if lease.ID == "" || lease.Session.ViewGeneration == 0 || !lease.Session.Busy || !lease.Session.Dirty {
		t.Fatalf("invalid mutation lease: %#v", lease)
	}
	// Release an unfinished test write before daemon cleanup, without spawning a
	// daemon if the test intentionally stopped or crashed the previous instance.
	t.Cleanup(func() {
		_, _ = testDaemonRPC(repo, map[string]any{"method": "release_exec", "session_key": key,
			"lease_id": lease.ID, "view_generation": lease.Session.ViewGeneration})
	})
	return lease
}

func testDaemonRPC(repo string, req map[string]any) (string, error) {
	conn, err := net.DialTimeout("unix", testDaemonSocket(repo), time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	req["repo_root"], req["git_dir"] = repo, filepath.Join(repo, ".git")
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return "", err
	}
	var reply struct {
		Error string `json:"error"`
	}
	err = json.NewDecoder(conn).Decode(&reply)
	return reply.Error, err
}

func releaseFileTool(t *testing.T, binary, repo, key string, lease fileToolLease) {
	t.Helper()
	daemonCLI(t, binary, repo, "release", "--session-key", key, "--lease-id", lease.ID,
		"--view-generation", strconv.FormatUint(lease.Session.ViewGeneration, 10))
}

func daemonMustFail(t *testing.T, binary, repo, want string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, append([]string{"daemon"}, args...)...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err == nil || ctx.Err() != nil || !strings.Contains(string(out), want) {
		t.Fatalf("daemon %v: wanted prompt error containing %q, got %v: %s", args, want, err, out)
	}
}

func materializeDaemonPatch(t *testing.T, repo string, info daemonSnapshot) string {
	t.Helper()
	checkout := filepath.Join(t.TempDir(), "reapplied")
	runCmd(t, repo, "git", "worktree", "add", "--detach", checkout, info.BaseOID)
	t.Cleanup(func() { runCmd(t, repo, "git", "worktree", "remove", "--force", checkout) })
	runCmd(t, checkout, "git", "apply", "--check", info.PatchPath)
	runCmd(t, checkout, "git", "apply", "--whitespace=nowarn", info.PatchPath)
	return checkout
}

func assertOriginalCheckout(t *testing.T, repo string) {
	t.Helper()
	if got := readFile(t, filepath.Join(repo, "tracked.txt")); !bytes.Equal(got, []byte("base\n")) {
		t.Fatalf("original tracked bytes changed: %q", got)
	}
	if got := strings.TrimSpace(runCmd(t, repo, "git", "status", "--porcelain")); got != "" {
		t.Fatalf("original checkout or index changed: %s", got)
	}
}

func TestPersonaIntegrationDaemonMutationLeaseRoundTrip(t *testing.T) {
	binary := requireIntegration(t)
	repo := createRepo(t)
	daemon := startManagedTestDaemon(t, binary, repo)
	const key = "file-tool-roundtrip"
	daemonInfo(t, binary, repo, key)
	daemonCLI(t, binary, repo, "flush", "--session-key", key)
	lease := acquireFileTool(t, binary, repo, key, os.Getpid())
	view := lease.Session.ViewPath
	files := map[string][]byte{
		"tracked.txt":      []byte("edited by file tool\n"),
		"line endings.txt": []byte("first\r\nsecond\r\n"),
		"빈 파일.txt":         {},
		"binary.dat":       {0, 1, 2, 255, 0, 127},
		"script.sh":        []byte("#!/bin/sh\nprintf done"),
	}
	for name, data := range files {
		writeFile(t, filepath.Join(view, name), data)
	}
	if err := os.Chmod(filepath.Join(view, "script.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tracked.txt", filepath.Join(view, "link")); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, lease.Session.PatchPath)
	for _, request := range [][]string{
		{"flush", "--session-key", key},
		{"end", "--session-key", key},
		{"acquire", "--session-key", key, "--owner-pid", strconv.Itoa(os.Getpid())},
		{"exec", "--session-key", key, "--", "sh", "-c", "printf collision > tracked.txt"},
	} {
		want := "busy"
		if request[0] == "acquire" || request[0] == "exec" {
			want = "already executing"
		}
		daemonMustFail(t, binary, repo, want, request...)
	}
	daemonMustFail(t, binary, repo, "generation mismatch", "release", "--session-key", key,
		"--lease-id", lease.ID, "--view-generation", strconv.FormatUint(lease.Session.ViewGeneration+1, 10))
	if got := readFile(t, lease.Session.PatchPath); !bytes.Equal(got, before) {
		t.Fatal("an operation saved an in-progress file-tool mutation")
	}
	if got := daemonCLI(t, binary, repo, "exec", "--session-key", "independent", "--", "printf", "independent"); string(got) != "independent" {
		t.Fatalf("another session could not execute during a live lease: %q", got)
	}
	releaseFileTool(t, binary, repo, key, lease)
	info := daemonInfo(t, binary, repo, key)
	if info.Dirty || info.Busy || info.LastDurableGeneration != info.Generation {
		t.Fatalf("lease release did not establish a durable checkpoint: %#v", info)
	}
	checkSnapshot := func(root string) {
		t.Helper()
		for name, want := range files {
			if got := readFile(t, filepath.Join(root, name)); !bytes.Equal(got, want) {
				t.Fatalf("%s round-trip mismatch: got %q want %q", name, got, want)
			}
		}
		if info, err := os.Stat(filepath.Join(root, "script.sh")); err != nil || info.Mode().Perm()&0o100 == 0 {
			t.Fatalf("executable mode was not preserved: %v %v", info, err)
		}
		if target, err := os.Readlink(filepath.Join(root, "link")); err != nil || target != "tracked.txt" {
			t.Fatalf("symlink was not preserved: %q %v", target, err)
		}
	}
	checkSnapshot(materializeDaemonPatch(t, repo, info))
	daemon.stop()
	writeFile(t, filepath.Join(repo, "later-commit.txt"), []byte("new source HEAD\n"))
	runCmd(t, repo, "git", "add", "later-commit.txt")
	runCmd(t, repo, "git", "commit", "-m", "advance source HEAD after checkpoint")
	daemon = startManagedTestDaemon(t, binary, repo)
	restored := daemonInfo(t, binary, repo, key)
	if restored.BaseOID != info.BaseOID || restored.ViewGeneration <= info.ViewGeneration {
		t.Fatalf("restart changed base or reused view generation: %#v", restored)
	}
	checkSnapshot(restored.ViewPath)
	if _, err := os.Stat(filepath.Join(restored.ViewPath, "later-commit.txt")); !os.IsNotExist(err) {
		t.Fatalf("restored session followed moving source HEAD: %v", err)
	}
	daemonCLI(t, binary, repo, "end", "--session-key", key)
	daemonCLI(t, binary, repo, "end", "--session-key", "independent")
	daemon.stop()
	assertOriginalCheckout(t, repo)
}

func TestPersonaIntegrationDaemonShutdownSavesDeferredFlush(t *testing.T) {
	binary := requireIntegration(t)
	repo := createRepo(t)
	daemon := startManagedTestDaemon(t, binary, repo)
	const key = "deferred"
	lease := acquireFileTool(t, binary, repo, key, os.Getpid())
	writeFile(t, filepath.Join(lease.Session.ViewPath, "tracked.txt"), []byte("checkpoint\n"))
	releaseFileTool(t, binary, repo, key, lease)
	checkpoint := readFile(t, lease.Session.PatchPath)
	// Exercise the compatibility dirty/flush API with a skipped minimum-age save.
	daemonCLI(t, binary, repo, "mark-dirty", "--session-key", key)
	writeFile(t, filepath.Join(lease.Session.ViewPath, "tracked.txt"), []byte("latest edit\r\n"))
	daemonCLI(t, binary, repo, "flush", "--session-key", key, "--min-age", "1h")
	if !daemonInfo(t, binary, repo, key).Dirty || !bytes.Equal(readFile(t, lease.Session.PatchPath), checkpoint) {
		t.Fatal("fixture did not leave an unflushed mutation")
	}
	daemon.stop()
	daemon = startManagedTestDaemon(t, binary, repo)
	info := daemonInfo(t, binary, repo, key)
	if info.Dirty || info.LastDurableGeneration != info.Generation {
		t.Fatalf("shutdown did not save deferred changes: %#v", info)
	}
	for _, root := range []string{info.ViewPath, materializeDaemonPatch(t, repo, info)} {
		if got := readFile(t, filepath.Join(root, "tracked.txt")); string(got) != "latest edit\r\n" {
			t.Fatalf("shutdown/restart lost latest bytes: %q", got)
		}
	}
	daemonCLI(t, binary, repo, "end", "--session-key", key)
	daemon.stop()
	assertOriginalCheckout(t, repo)
}

func TestPersonaIntegrationDaemonShutdownWaitsForExecution(t *testing.T) {
	binary := requireIntegration(t)
	repo := createRepo(t)
	daemon := startManagedTestDaemon(t, binary, repo)
	barrier := t.TempDir()
	ready, finish := filepath.Join(barrier, "ready"), filepath.Join(barrier, "finish")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, binary, "daemon", "exec", "--session-key", "draining", "--", "sh", "-c",
		"printf ready > \"$1\"; while ! test -e \"$2\"; do sleep 0.02; done; printf completed > tracked.txt", "writer", ready, finish)
	cmd.Dir = repo
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	waited := false
	t.Cleanup(func() {
		if waited {
			return
		}
		// A failed assertion must also let the shell leave the barrier; killing
		// only its CLI parent could otherwise leave a waiting child behind.
		_ = os.WriteFile(finish, []byte("finish"), 0o600)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cancel()
			<-done
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command did not reach the execution barrier")
		}
		time.Sleep(10 * time.Millisecond)
	}
	daemon.beginShutdown()
	deadline = time.Now().Add(5 * time.Second)
	for {
		message, err := testDaemonRPC(repo, map[string]any{"method": "mark_dirty", "session_key": "draining"})
		if err != nil {
			t.Fatalf("daemon exited before its active execution finished: %v", err)
		}
		if strings.Contains(message, "daemon is shutting down") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not stop accepting new mutations: %s", message)
		}
		time.Sleep(10 * time.Millisecond)
	}
	writeFile(t, finish, []byte("finish"))
	execErr := <-done
	waited = true
	if execErr != nil {
		t.Fatalf("active command/release failed during shutdown: %v: %s", execErr, output.String())
	}
	daemon.stop()
	daemon = startManagedTestDaemon(t, binary, repo)
	info := daemonInfo(t, binary, repo, "draining")
	if got := readFile(t, filepath.Join(info.ViewPath, "tracked.txt")); string(got) != "completed" {
		t.Fatalf("shutdown lost the active command's final write: %q", got)
	}
	daemonCLI(t, binary, repo, "end", "--session-key", "draining")
	daemon.stop()
	assertOriginalCheckout(t, repo)
}

func TestPersonaIntegrationDaemonPreservesActiveMutation(t *testing.T) {
	binary := requireIntegration(t)
	for _, termination := range []string{"shutdown", "crash"} {
		t.Run(termination, func(t *testing.T) {
			repo := createRepo(t)
			daemon := startManagedTestDaemon(t, binary, repo)
			owner := exec.Command("sleep", "60")
			if err := owner.Start(); err != nil {
				t.Fatal(err)
			}
			ownerStopped := false
			stopOwner := func() {
				if !ownerStopped {
					ownerStopped = true
					_ = owner.Process.Kill()
					_ = owner.Wait()
				}
			}
			t.Cleanup(stopOwner)
			const key = "unfinished"
			lease := acquireFileTool(t, binary, repo, key, owner.Process.Pid)
			want := []byte("not checkpointed\x00\xff\n")
			writeFile(t, filepath.Join(lease.Session.ViewPath, "tracked.txt"), want)
			if termination == "crash" {
				daemon.crash()
			} else {
				daemon.stopPreserving()
			}
			daemon = startManagedTestDaemon(t, binary, repo)
			daemonMustFail(t, binary, repo, "still alive", "recover", "--session-key", key)
			var pending daemonSnapshot
			if err := json.Unmarshal(daemonCLI(t, binary, repo, "recover", "--session-key", key, "--dry-run"), &pending); err != nil {
				t.Fatal(err)
			}
			if !pending.Dirty || pending.State != "recovery_required" || pending.RecoveryRoot == "" {
				t.Fatalf("unfinished mutation was reported as clean: %#v", pending)
			}
			if got := readFile(t, filepath.Join(lease.Session.ViewPath, "tracked.txt")); !bytes.Equal(got, want) {
				t.Fatalf("active upper layer was removed: %q", got)
			}
			stopOwner()
			var recovered daemonSnapshot
			if err := json.Unmarshal(daemonCLI(t, binary, repo, "recover", "--session-key", key), &recovered); err != nil {
				t.Fatal(err)
			}
			if recovered.ViewGeneration <= lease.Session.ViewGeneration || !recovered.Dirty || recovered.BaseOID != lease.Session.BaseOID {
				t.Fatalf("recovery did not retain the dirty base/generation contract: %#v", recovered)
			}
			if got := readFile(t, filepath.Join(recovered.ViewPath, "tracked.txt")); !bytes.Equal(got, want) {
				t.Fatalf("recovery lost unflushed bytes: %q", got)
			}
			daemonMustFail(t, binary, repo, "generation mismatch", "release", "--session-key", key,
				"--lease-id", lease.ID, "--view-generation", strconv.FormatUint(lease.Session.ViewGeneration, 10))
			daemonCLI(t, binary, repo, "flush", "--session-key", key)
			info := daemonInfo(t, binary, repo, key)
			checkout := materializeDaemonPatch(t, repo, info)
			if got := readFile(t, filepath.Join(checkout, "tracked.txt")); !bytes.Equal(got, want) {
				t.Fatalf("recovered checkpoint cannot be reapplied: %q", got)
			}
			daemonCLI(t, binary, repo, "end", "--session-key", key)
			daemon.stop()
			assertOriginalCheckout(t, repo)
		})
	}
}

func TestPersonaIntegrationDaemonShutdownRetainsFailedExport(t *testing.T) {
	binary := requireIntegration(t)
	repo := createRepo(t)
	writeFile(t, filepath.Join(repo, ".gitignore"), nil)
	runCmd(t, repo, "git", "add", ".gitignore")
	runCmd(t, repo, "git", "commit", "-m", "ignored policy baseline")
	daemon := startManagedTestDaemon(t, binary, repo)
	const key = "failed-save"
	info := daemonInfo(t, binary, repo, key)
	daemonCLI(t, binary, repo, "flush", "--session-key", key)
	checkpoint := readFile(t, info.PatchPath)
	daemonCLI(t, binary, repo, "mark-dirty", "--session-key", key)
	writeFile(t, filepath.Join(info.ViewPath, "tracked.txt"), []byte("preserve despite export failure\n"))
	writeFile(t, filepath.Join(info.ViewPath, "cache.dat"), []byte("new ignored artifact\n"))
	writeFile(t, filepath.Join(info.ViewPath, ".gitignore"), []byte("cache.dat\n"))
	daemon.stopPreserving()
	if !bytes.Equal(readFile(t, info.PatchPath), checkpoint) {
		t.Fatal("failed shutdown export replaced the last durable patch")
	}
	if got := readFile(t, filepath.Join(info.ViewPath, "tracked.txt")); string(got) != "preserve despite export failure\n" {
		t.Fatalf("shutdown discarded upper after failed export: %q", got)
	}
	daemon = startManagedTestDaemon(t, binary, repo)
	var pending daemonSnapshot
	if err := json.Unmarshal(daemonCLI(t, binary, repo, "recover", "--session-key", key, "--dry-run"), &pending); err != nil {
		t.Fatal(err)
	}
	if !pending.Dirty || pending.RecoveryRoot == "" || !strings.Contains(pending.LastError, "ignored paths changed") {
		t.Fatalf("save failure is not recoverable/observable: %#v", pending)
	}
	var recovered daemonSnapshot
	if err := json.Unmarshal(daemonCLI(t, binary, repo, "recover", "--session-key", key), &recovered); err != nil {
		t.Fatal(err)
	}
	// Resolve the failed save policy in the preserved view, then checkpoint it.
	writeFile(t, filepath.Join(recovered.ViewPath, ".gitignore"), nil)
	daemonCLI(t, binary, repo, "flush", "--session-key", key)
	saved := daemonInfo(t, binary, repo, key)
	checkout := materializeDaemonPatch(t, repo, saved)
	if got := readFile(t, filepath.Join(checkout, "tracked.txt")); string(got) != "preserve despite export failure\n" {
		t.Fatalf("recovered failed export lost bytes: %q", got)
	}
	daemonCLI(t, binary, repo, "end", "--session-key", key)
	daemon.stop()
	assertOriginalCheckout(t, repo)
}

func TestPersonaIntegrationDaemonFileToolIgnoredPolicy(t *testing.T) {
	binary := requireIntegration(t)
	for _, mode := range []string{"readonly", "masked"} {
		t.Run(mode, func(t *testing.T) {
			repo := createRepo(t)
			writeFile(t, filepath.Join(repo, ".gitignore"), []byte("ignored.txt\n"))
			runCmd(t, repo, "git", "add", ".gitignore")
			runCmd(t, repo, "git", "commit", "-m", "ignored fixture")
			writeFile(t, filepath.Join(repo, "ignored.txt"), []byte("private ignored bytes\n"))
			daemon := startManagedTestDaemon(t, binary, repo)
			lease := acquireFileTool(t, binary, repo, mode, os.Getpid(), "--base-mode", "repo", "--ignored-mode", mode)
			ignoredPath := filepath.Join(lease.Session.ViewPath, "ignored.txt")
			if mode == "readonly" {
				if got := readFile(t, ignoredPath); string(got) != "private ignored bytes\n" {
					t.Fatalf("readonly policy hid content: %q", got)
				}
				if err := os.WriteFile(ignoredPath, []byte("corrupt"), 0o644); err == nil {
					t.Fatal("file tool bypassed readonly ignored policy")
				}
			} else if got := readFile(t, ignoredPath); len(got) != 0 {
				t.Fatalf("file tool bypassed masked ignored policy: %q", got)
			}
			writeFile(t, filepath.Join(lease.Session.ViewPath, "tracked.txt"), []byte("saved\n"))
			releaseFileTool(t, binary, repo, mode, lease)
			if bytes.Contains(readFile(t, lease.Session.PatchPath), []byte("private ignored bytes")) {
				t.Fatal("ignored content entered durable patch")
			}
			daemonCLI(t, binary, repo, "end", "--session-key", mode)
			daemon.stop()
			if got := readFile(t, filepath.Join(repo, "ignored.txt")); string(got) != "private ignored bytes\n" {
				t.Fatalf("ignored source changed: %q", got)
			}
			assertOriginalCheckout(t, repo)
		})
	}
}

func TestPersonaIntegrationSessionRefusesMountedCleanup(t *testing.T) {
	requireIntegration(t)
	gitDir := filepath.Join(t.TempDir(), ".git")
	s, err := session.Create(gitDir)
	if err != nil {
		t.Fatal(err)
	}
	lower := t.TempDir()
	writeFile(t, filepath.Join(lower, "sentinel"), []byte("preserve original"))
	if err := ns.MountOverlay(s.View, ns.OverlayOpts{LowerDir: lower, UpperDir: s.Upper, WorkDir: s.Work}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ns.Umount(s.View) })
	writeFile(t, filepath.Join(s.View, "sentinel"), []byte("session mutation"))
	if mounted, err := session.HasMounts(s.Root); err != nil || !mounted {
		t.Fatalf("live overlay not detected: %v %v", mounted, err)
	}
	if err := s.RemoveAll(); err == nil || !strings.Contains(err.Error(), "mounted") {
		t.Fatalf("mounted session cleanup was not refused: %v", err)
	}
	if got := readFile(t, filepath.Join(s.View, "sentinel")); string(got) != "session mutation" {
		t.Fatalf("refused cleanup destroyed upper: %q", got)
	}
	if err := ns.Umount(s.View); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveAll(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Root); !os.IsNotExist(err) {
		t.Fatalf("unmounted session was retained: %v", err)
	}
	if got := readFile(t, filepath.Join(lower, "sentinel")); string(got) != "preserve original" {
		t.Fatalf("cleanup modified the lower directory: %q", got)
	}
}
