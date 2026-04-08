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
	state := newTestDaemonState(repoRoot, gitDir, func() model.GitOps {
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
	state := newTestDaemonState(repoRoot, gitDir, func() model.GitOps {
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

func TestDaemonStateReleaseExecWritesPatchAndEndCleansView(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	g := &exportGitOps{tracked: []byte("diff --git a/file.txt b/file.txt\n")}
	state := newTestDaemonState(repoRoot, gitDir, func() model.GitOps {
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

func newTestDaemonState(repoRoot, gitDir string, newGit func() model.GitOps) *daemonState {
	return &daemonState{
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
}
