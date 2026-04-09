package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"persona/internal/model"
)

func TestNewRootCmdExposesDaemonSurface(t *testing.T) {
	cmd := newRootCmd()
	for _, args := range [][]string{
		{"daemon"},
		{"daemon", "exec"},
		{"daemon", "info"},
		{"daemon", "flush"},
		{"daemon", "end"},
	} {
		found, _, err := cmd.Find(args)
		if err != nil {
			t.Fatalf("find %v: %v", args, err)
		}
		if found == nil {
			t.Fatalf("expected command for %v", args)
		}
	}
}

func TestDaemonSessionPatchPathIsStablePerKey(t *testing.T) {
	gitDir := filepath.Join(t.TempDir(), ".git")

	a1 := daemonSessionPatchPath(gitDir, "chat-a")
	a2 := daemonSessionPatchPath(gitDir, "chat-a")
	b := daemonSessionPatchPath(gitDir, "chat-b")

	if a1 != a2 {
		t.Fatalf("expected stable patch path, got %q and %q", a1, a2)
	}
	if a1 == b {
		t.Fatalf("expected isolated patch paths, got %q", a1)
	}
	if !strings.Contains(a1, filepath.Join(".git", "persona", "daemon", "patches")) {
		t.Fatalf("expected daemon patch root in git dir, got %q", a1)
	}
}

func TestDaemonStateEnsureSessionReusesKeyAndIsolatesDifferentKeys(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &exportGitOps{}
	})
	cfg := daemonTestConfig()

	info1, err := state.ensureSession(context.Background(), "chat-a", cfg)
	if err != nil {
		t.Fatalf("ensure chat-a: %v", err)
	}
	info1b, err := state.ensureSession(context.Background(), "chat-a", cfg)
	if err != nil {
		t.Fatalf("ensure chat-a again: %v", err)
	}
	info2, err := state.ensureSession(context.Background(), "chat-b", cfg)
	if err != nil {
		t.Fatalf("ensure chat-b: %v", err)
	}

	if info1.ViewPath != info1b.ViewPath || info1.PatchPath != info1b.PatchPath {
		t.Fatalf("expected same session key to reuse view/patch, got %#v and %#v", info1, info1b)
	}
	if info1.ViewPath == info2.ViewPath {
		t.Fatalf("expected different keys to get isolated views, got %q", info1.ViewPath)
	}
	if info1.PatchPath == info2.PatchPath {
		t.Fatalf("expected different keys to get isolated patches, got %q", info1.PatchPath)
	}
}

func TestDaemonStateRejectsOptionMismatchForExistingKey(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &exportGitOps{}
	})
	cfg := daemonTestConfig()

	if _, err := state.ensureSession(context.Background(), "chat-a", cfg); err != nil {
		t.Fatalf("ensure chat-a: %v", err)
	}
	cfg.ApplyMode = model.ApplyReject
	if _, err := state.ensureSession(context.Background(), "chat-a", cfg); err == nil || !strings.Contains(err.Error(), "different options") {
		t.Fatalf("expected options mismatch, got %v", err)
	}
}

func TestDaemonStateRejectsBusySessionForLiveOwner(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &exportGitOps{}
	})
	cfg := daemonTestConfig()

	_, leaseID, err := state.acquireExec(context.Background(), "chat-a", os.Getpid(), cfg)
	if err != nil {
		t.Fatalf("acquire exec: %v", err)
	}
	if _, _, err := state.acquireExec(context.Background(), "chat-a", os.Getpid(), cfg); err == nil || !strings.Contains(err.Error(), "already executing") {
		t.Fatalf("expected busy-session rejection, got %v", err)
	}
	if err := state.releaseExec(context.Background(), "chat-a", leaseID); err != nil {
		t.Fatalf("release exec: %v", err)
	}
}

func TestDaemonStateRejectsWrongLeaseAndKeepsSessionBusy(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &exportGitOps{}
	})
	cfg := daemonTestConfig()

	_, leaseID, err := state.acquireExec(context.Background(), "chat-a", os.Getpid(), cfg)
	if err != nil {
		t.Fatalf("acquire exec: %v", err)
	}
	if err := state.releaseExec(context.Background(), "chat-a", "wrong-lease"); err == nil || !strings.Contains(err.Error(), "lease mismatch") {
		t.Fatalf("expected lease mismatch, got %v", err)
	}
	if !state.sessions["chat-a"].busy {
		t.Fatal("expected session to stay busy after wrong lease release")
	}
	if err := state.releaseExec(context.Background(), "chat-a", leaseID); err != nil {
		t.Fatalf("release exec with correct lease: %v", err)
	}
}

func TestDaemonStateRecoversStaleBusySessionAndFlushesPatch(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	g := &exportGitOps{tracked: []byte("diff --git a/recovered.txt b/recovered.txt\n")}
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return g
	})
	cfg := daemonTestConfig()

	info, _, err := state.acquireExec(context.Background(), "chat-a", 99999999, cfg)
	if err != nil {
		t.Fatalf("acquire exec with stale owner: %v", err)
	}
	reacquired, leaseID, err := state.acquireExec(context.Background(), "chat-a", os.Getpid(), cfg)
	if err != nil {
		t.Fatalf("reacquire after stale owner: %v", err)
	}
	if reacquired.ViewPath != info.ViewPath || reacquired.PatchPath != info.PatchPath {
		t.Fatalf("expected stale recovery to reuse session paths, got %#v vs %#v", reacquired, info)
	}
	patchData, err := os.ReadFile(info.PatchPath)
	if err != nil {
		t.Fatalf("read recovered patch: %v", err)
	}
	if string(patchData) != string(g.tracked) {
		t.Fatalf("expected stale recovery flush, got %q", patchData)
	}
	if err := state.releaseExec(context.Background(), "chat-a", leaseID); err != nil {
		t.Fatalf("release reacquired exec: %v", err)
	}
}

func TestDaemonStateKeepsConcurrentSessionKeysIsolated(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &exportGitOps{}
	})
	cfg := daemonTestConfig()

	infoA, err := state.ensureSession(context.Background(), "chat-a", cfg)
	if err != nil {
		t.Fatalf("ensure chat-a: %v", err)
	}
	infoB, err := state.ensureSession(context.Background(), "chat-b", cfg)
	if err != nil {
		t.Fatalf("ensure chat-b: %v", err)
	}
	diffA := []byte("diff --git a/a.txt b/a.txt\n")
	diffB := []byte("diff --git a/b.txt b/b.txt\n")
	state.sessions["chat-a"].g = &exportGitOps{tracked: diffA}
	state.sessions["chat-b"].g = &exportGitOps{tracked: diffB}

	_, leaseA, err := state.acquireExec(context.Background(), "chat-a", os.Getpid(), cfg)
	if err != nil {
		t.Fatalf("acquire chat-a: %v", err)
	}
	if _, _, err := state.acquireExec(context.Background(), "chat-b", os.Getpid(), cfg); err != nil {
		t.Fatalf("acquire chat-b while chat-a is busy: %v", err)
	}
	leaseB := state.sessions["chat-b"].busyLease
	if err := state.releaseExec(context.Background(), "chat-a", leaseA); err != nil {
		t.Fatalf("release chat-a: %v", err)
	}
	if err := state.releaseExec(context.Background(), "chat-b", leaseB); err != nil {
		t.Fatalf("release chat-b: %v", err)
	}
	patchA, err := os.ReadFile(infoA.PatchPath)
	if err != nil {
		t.Fatalf("read chat-a patch: %v", err)
	}
	patchB, err := os.ReadFile(infoB.PatchPath)
	if err != nil {
		t.Fatalf("read chat-b patch: %v", err)
	}
	if string(patchA) != string(diffA) {
		t.Fatalf("unexpected chat-a patch contents: %q", patchA)
	}
	if string(patchB) != string(diffB) {
		t.Fatalf("unexpected chat-b patch contents: %q", patchB)
	}
	if infoA.PatchPath == infoB.PatchPath || infoA.ViewPath == infoB.ViewPath {
		t.Fatalf("expected isolated paths, got %#v and %#v", infoA, infoB)
	}
}

func TestDaemonStateReleaseExecWritesPatchAndEndCleansView(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	g := &exportGitOps{tracked: []byte("diff --git a/file.txt b/file.txt\n")}
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return g
	})
	cfg := daemonTestConfig()

	info, leaseID, err := state.acquireExec(context.Background(), "chat-a", os.Getpid(), cfg)
	if err != nil {
		t.Fatalf("acquire exec: %v", err)
	}
	if leaseID == "" {
		t.Fatal("expected non-empty lease id")
	}
	if err := state.releaseExec(context.Background(), "chat-a", leaseID); err != nil {
		t.Fatalf("release exec: %v", err)
	}
	patchData, err := os.ReadFile(info.PatchPath)
	if err != nil {
		t.Fatalf("read patch: %v", err)
	}
	if string(patchData) != string(g.tracked) {
		t.Fatalf("unexpected patch contents: %q", patchData)
	}

	ended, err := state.endSession(context.Background(), "chat-a")
	if err != nil {
		t.Fatalf("end session: %v", err)
	}
	if !ended {
		t.Fatal("expected session to end")
	}
	if _, ok := state.sessions["chat-a"]; ok {
		t.Fatal("expected session map entry to be removed")
	}
	if _, err := os.Stat(info.ViewPath); !os.IsNotExist(err) {
		t.Fatalf("expected session view to be removed, got %v", err)
	}
}

func TestDaemonStateFlushWritesPatchWithoutEndingSession(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	g := &exportGitOps{tracked: []byte("diff --git a/file.txt b/file.txt\n")}
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return g
	})
	cfg := daemonTestConfig()

	info, err := state.ensureSession(context.Background(), "chat-a", cfg)
	if err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if err := state.flushSession(context.Background(), "chat-a"); err != nil {
		t.Fatalf("flush session: %v", err)
	}
	patchData, err := os.ReadFile(info.PatchPath)
	if err != nil {
		t.Fatalf("read patch: %v", err)
	}
	if string(patchData) != string(g.tracked) {
		t.Fatalf("unexpected patch contents after flush: %q", patchData)
	}
	if _, ok := state.sessions["chat-a"]; !ok {
		t.Fatal("expected flush to keep session registered")
	}
	if _, err := os.Stat(info.ViewPath); err != nil {
		t.Fatalf("expected flush to keep view path, got %v", err)
	}
}

func TestDaemonStateFlushRejectsBusyLiveSession(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &exportGitOps{}
	})
	cfg := daemonTestConfig()

	if _, _, err := state.acquireExec(context.Background(), "chat-a", os.Getpid(), cfg); err != nil {
		t.Fatalf("acquire exec: %v", err)
	}
	if err := state.flushSession(context.Background(), "chat-a"); err == nil || !strings.Contains(err.Error(), "still busy") {
		t.Fatalf("expected busy flush rejection, got %v", err)
	}
}

func TestDaemonStateRecoversStaleLeaseAndFlushesPriorChanges(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	g := &exportGitOps{tracked: []byte("first\n")}
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return g
	})
	cfg := daemonTestConfig()

	info1, lease1, err := state.acquireExec(context.Background(), "chat-a", -1, cfg)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if lease1 == "" {
		t.Fatal("expected first lease id")
	}

	info2, lease2, err := state.acquireExec(context.Background(), "chat-a", os.Getpid(), cfg)
	if err != nil {
		t.Fatalf("second acquire after stale lease: %v", err)
	}
	if info1.ViewPath != info2.ViewPath {
		t.Fatalf("expected stale recovery to reuse the same view, got %q and %q", info1.ViewPath, info2.ViewPath)
	}
	if lease1 == lease2 || lease2 == "" {
		t.Fatalf("expected a fresh lease after stale recovery, got old=%q new=%q", lease1, lease2)
	}
	patchData, err := os.ReadFile(info2.PatchPath)
	if err != nil {
		t.Fatalf("read recovered patch: %v", err)
	}
	if string(patchData) != "first\n" {
		t.Fatalf("expected stale recovery to flush prior changes, got %q", patchData)
	}

	g.tracked = []byte("second\n")
	if err := state.releaseExec(context.Background(), "chat-a", lease2); err != nil {
		t.Fatalf("release fresh lease: %v", err)
	}
	patchData, err = os.ReadFile(info2.PatchPath)
	if err != nil {
		t.Fatalf("read final patch: %v", err)
	}
	if string(patchData) != "second\n" {
		t.Fatalf("expected current lease flush to update patch, got %q", patchData)
	}
}

func TestDaemonStateDifferentSessionsFlushIndependentPatches(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &exportGitOps{}
	})
	cfg := daemonTestConfig()

	infoA, err := state.ensureSession(context.Background(), "chat-a", cfg)
	if err != nil {
		t.Fatalf("ensure chat-a: %v", err)
	}
	infoB, err := state.ensureSession(context.Background(), "chat-b", cfg)
	if err != nil {
		t.Fatalf("ensure chat-b: %v", err)
	}
	gA := &exportGitOps{tracked: []byte("patch-a\n")}
	gB := &exportGitOps{tracked: []byte("patch-b\n")}
	state.sessions["chat-a"].g = gA
	state.sessions["chat-b"].g = gB

	_, leaseA, err := state.acquireExec(context.Background(), "chat-a", os.Getpid(), cfg)
	if err != nil {
		t.Fatalf("acquire chat-a: %v", err)
	}
	if err := state.releaseExec(context.Background(), "chat-a", leaseA); err != nil {
		t.Fatalf("release chat-a: %v", err)
	}
	_, leaseB, err := state.acquireExec(context.Background(), "chat-b", os.Getpid(), cfg)
	if err != nil {
		t.Fatalf("acquire chat-b: %v", err)
	}
	if err := state.releaseExec(context.Background(), "chat-b", leaseB); err != nil {
		t.Fatalf("release chat-b: %v", err)
	}

	patchA, err := os.ReadFile(infoA.PatchPath)
	if err != nil {
		t.Fatalf("read patch-a: %v", err)
	}
	patchB, err := os.ReadFile(infoB.PatchPath)
	if err != nil {
		t.Fatalf("read patch-b: %v", err)
	}
	if string(patchA) != "patch-a\n" {
		t.Fatalf("unexpected chat-a patch: %q", patchA)
	}
	if string(patchB) != "patch-b\n" {
		t.Fatalf("unexpected chat-b patch: %q", patchB)
	}

	ended, err := state.endSession(context.Background(), "chat-a")
	if err != nil {
		t.Fatalf("end chat-a: %v", err)
	}
	if !ended {
		t.Fatal("expected chat-a to end")
	}
	if _, ok := state.sessions["chat-b"]; !ok {
		t.Fatal("expected chat-b session to remain live")
	}
	if _, err := os.Stat(infoB.ViewPath); err != nil {
		t.Fatalf("expected chat-b view to remain after ending chat-a, got %v", err)
	}
}

func TestDaemonStateEndRejectsBusyLiveSession(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &exportGitOps{}
	})
	cfg := daemonTestConfig()

	_, leaseID, err := state.acquireExec(context.Background(), "chat-a", os.Getpid(), cfg)
	if err != nil {
		t.Fatalf("acquire chat-a: %v", err)
	}
	ended, err := state.endSession(context.Background(), "chat-a")
	if err == nil || !strings.Contains(err.Error(), "still busy") {
		t.Fatalf("expected busy end rejection, got ended=%v err=%v", ended, err)
	}
	if ended {
		t.Fatal("expected end=false while session is busy")
	}
	if _, ok := state.sessions["chat-a"]; !ok {
		t.Fatal("expected busy session to remain registered")
	}
	if err := state.releaseExec(context.Background(), "chat-a", leaseID); err != nil {
		t.Fatalf("release busy session: %v", err)
	}
}

func daemonTestRepo(t *testing.T) (string, string) {
	t.Helper()
	repoRoot := t.TempDir()
	gitDir := filepath.Join(repoRoot, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("mkdir git dir: %v", err)
	}
	return repoRoot, gitDir
}

func daemonTestConfig() daemonSessionConfig {
	return daemonSessionConfig{
		BaseMode:    model.BaseRepo,
		BaseRef:     "HEAD",
		AllowDirty:  true,
		IgnoredMode: model.IgnoredTransparent,
		IgnoredMax:  16,
		ApplyMode:   model.ApplyStrict,
	}
}

func newTestDaemonState(t *testing.T, repoRoot, gitDir string, newGit func() model.GitOps) *daemonState {
	t.Helper()
	state := &daemonState{
		repoRoot: repoRoot,
		gitDir:   gitDir,
		deps: daemonDeps{
			newGit: func(string, string, bool) model.GitOps {
				return newGit()
			},
			newMount: func() model.NSOps {
				return &recordingNSOps{}
			},
			now: func() time.Time {
				return time.Unix(1700000000, 0)
			},
		},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		sessions: make(map[string]*daemonSession),
	}
	t.Cleanup(state.closeAll)
	return state
}
