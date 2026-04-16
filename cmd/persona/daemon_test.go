package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"persona/internal/model"
)

type daemonExportApplyGitOps struct {
	exportGitOps
}

func (g *daemonExportApplyGitOps) ApplyPatchReader(_ context.Context, _ model.ApplyMode, _ string, _ string, patchData io.Reader) error {
	_, _ = io.Copy(io.Discard, patchData)
	return nil
}

func (g *daemonExportApplyGitOps) ApplyPatchFromReader(ctx context.Context, mode model.ApplyMode, workTree, gitDir string, patchData io.Reader) error {
	return g.ApplyPatchReader(ctx, mode, workTree, gitDir, patchData)
}

func TestNewRootCmdExposesDaemonSurface(t *testing.T) {
	cmd := newRootCmd()
	for _, args := range [][]string{
		{"daemon"},
		{"daemon", "exec"},
		{"daemon", "info"},
		{"daemon", "list"},
		{"daemon", "flush"},
		{"daemon", "prune"},
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

func TestBuildDaemonSessionConfigNormalizesAndRejectsInvalidValues(t *testing.T) {
	t.Run("normalizes blank base-ref and parses enums", func(t *testing.T) {
		cfg, err := buildDaemonSessionConfig(daemonFlagValues{
			baseMode:    string(model.BaseWorktree),
			baseRef:     "  ",
			allowDirty:  true,
			ignoredMode: string(model.IgnoredReadonly),
			ignoredMax:  7,
			applyMode:   string(model.ApplyReject),
		})
		if err != nil {
			t.Fatalf("buildDaemonSessionConfig: %v", err)
		}
		if cfg.BaseMode != model.BaseWorktree {
			t.Fatalf("expected worktree mode, got %q", cfg.BaseMode)
		}
		if cfg.BaseRef != "HEAD" {
			t.Fatalf("expected blank base-ref to normalize to HEAD, got %q", cfg.BaseRef)
		}
		if !cfg.AllowDirty {
			t.Fatal("expected allow-dirty to propagate")
		}
		if cfg.IgnoredMode != model.IgnoredReadonly || cfg.IgnoredMax != 7 {
			t.Fatalf("unexpected ignored config: %#v", cfg)
		}
		if cfg.ApplyMode != model.ApplyReject {
			t.Fatalf("expected reject apply mode, got %q", cfg.ApplyMode)
		}
	})

	t.Run("rejects repo mode custom base-ref", func(t *testing.T) {
		_, err := buildDaemonSessionConfig(daemonFlagValues{
			baseMode:    string(model.BaseRepo),
			baseRef:     "feature/base",
			ignoredMode: string(model.IgnoredTransparent),
			applyMode:   string(model.ApplyStrict),
		})
		if err == nil || !strings.Contains(err.Error(), "base-ref is only valid with worktree base-mode") {
			t.Fatalf("expected repo-mode base-ref validation error, got %v", err)
		}
	})

	t.Run("rejects negative ignored-max", func(t *testing.T) {
		_, err := buildDaemonSessionConfig(daemonFlagValues{
			baseMode:    string(model.BaseRepo),
			ignoredMode: string(model.IgnoredTransparent),
			ignoredMax:  -1,
			applyMode:   string(model.ApplyStrict),
		})
		if err == nil || !strings.Contains(err.Error(), "ignored-max must be >= 0") {
			t.Fatalf("expected ignored-max validation error, got %v", err)
		}
	})

	t.Run("rejects invalid apply mode", func(t *testing.T) {
		_, err := buildDaemonSessionConfig(daemonFlagValues{
			baseMode:    string(model.BaseRepo),
			ignoredMode: string(model.IgnoredTransparent),
			applyMode:   "surprise",
		})
		if err == nil || !strings.Contains(err.Error(), "apply-mode") {
			t.Fatalf("expected apply-mode parse error, got %v", err)
		}
	})
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

func TestDaemonStateSupportsDistinctSessionsWhenSafeNamesCollide(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &daemonExportApplyGitOps{}
	})
	cfg := daemonTestConfig()
	keys := []string{"agent/alpha", "agent alpha"}

	if daemonSafeName(keys[0]) != daemonSafeName(keys[1]) {
		t.Fatalf("expected colliding safe names for %q and %q", keys[0], keys[1])
	}

	initial := make(map[string]daemonSessionInfo, len(keys))
	for _, key := range keys {
		info, err := state.ensureSession(context.Background(), key, cfg)
		if err != nil {
			t.Fatalf("ensure %q: %v", key, err)
		}
		initial[key] = *info
	}

	if initial[keys[0]].PatchPath == initial[keys[1]].PatchPath {
		t.Fatalf("expected distinct patch paths for colliding safe names, got %q", initial[keys[0]].PatchPath)
	}
	if initial[keys[0]].ViewPath == initial[keys[1]].ViewPath {
		t.Fatalf("expected distinct views for colliding safe names, got %q", initial[keys[0]].ViewPath)
	}

	metaA := daemonSessionMetaPath(gitDir, keys[0])
	metaB := daemonSessionMetaPath(gitDir, keys[1])
	if metaA == metaB {
		t.Fatalf("expected distinct metadata paths, got %q", metaA)
	}
	for _, metaPath := range []string{metaA, metaB} {
		if _, err := os.Stat(metaPath); err != nil {
			t.Fatalf("expected metadata at %q: %v", metaPath, err)
		}
	}

	state.closeAll()
	state2 := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &daemonExportApplyGitOps{}
	})
	sessions, err := state2.listSessions(context.Background())
	if err != nil {
		t.Fatalf("list restored sessions: %v", err)
	}
	if len(sessions) != len(keys) {
		t.Fatalf("expected %d restored sessions, got %d", len(keys), len(sessions))
	}
	restored := daemonSessionsByKey(sessions)
	for _, key := range keys {
		got, ok := restored[key]
		if !ok {
			t.Fatalf("expected restored session for %q, got %#v", key, sessions)
		}
		if got.PatchPath != initial[key].PatchPath {
			t.Fatalf("expected stable patch path for %q, got %q want %q", key, got.PatchPath, initial[key].PatchPath)
		}
		if got.ViewPath == initial[key].ViewPath {
			t.Fatalf("expected fresh recovered view for %q, got %q", key, got.ViewPath)
		}
		if got.RecoveredCount != 1 {
			t.Fatalf("expected recovered count for %q, got %#v", key, got)
		}
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

func TestDaemonStateListSessionsSortsKeysAndReportsBusyMetadata(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &exportGitOps{}
	})
	cfg := daemonTestConfig()

	if _, err := state.ensureSession(context.Background(), "chat-b", cfg); err != nil {
		t.Fatalf("ensure chat-b: %v", err)
	}
	if _, _, err := state.acquireExec(context.Background(), "chat-a", os.Getpid(), cfg); err != nil {
		t.Fatalf("acquire chat-a: %v", err)
	}

	sessions, err := state.listSessions(context.Background())
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}
	if sessions[0].SessionKey != "chat-a" || sessions[1].SessionKey != "chat-b" {
		t.Fatalf("expected sorted session keys, got %#v", sessions)
	}
	if !sessions[0].Busy || sessions[0].BusyOwnerPID != os.Getpid() {
		t.Fatalf("expected busy metadata on chat-a, got %#v", sessions[0])
	}
	if sessions[0].LastUsedUnix == 0 || sessions[0].LastUsedRFC3339 == "" {
		t.Fatalf("expected last-used metadata, got %#v", sessions[0])
	}
	if sessions[0].CreatedUnix == 0 || sessions[0].CreatedRFC3339 == "" {
		t.Fatalf("expected created metadata, got %#v", sessions[0])
	}
	if !sessions[0].Dirty {
		t.Fatalf("expected busy session to report dirty state, got %#v", sessions[0])
	}
	if sessions[1].Busy {
		t.Fatalf("expected chat-b to be idle, got %#v", sessions[1])
	}
}

func TestDaemonStateFlushReportsObservabilityCounters(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	now := time.Unix(1700000000, 0)
	g := &exportGitOps{tracked: []byte("first\n")}
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return g
	})
	state.deps.now = func() time.Time { return now }
	cfg := daemonTestConfig()

	if _, err := state.ensureSession(context.Background(), "chat-a", cfg); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if err := state.flushSession(context.Background(), "chat-a", 0); err != nil {
		t.Fatalf("initial flush: %v", err)
	}
	now = now.Add(30 * time.Second)
	g.tracked = []byte("second\n")
	if err := state.flushSession(context.Background(), "chat-a", time.Hour); err != nil {
		t.Fatalf("min-age flush: %v", err)
	}

	sessions, err := state.listSessions(context.Background())
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	got := sessions[0]
	if got.FlushCount != 1 || got.FlushSkipped != 1 {
		t.Fatalf("expected flush observability counters after skip, got %#v", got)
	}
	if got.Dirty {
		t.Fatalf("expected skipped idle flush to keep session clean, got %#v", got)
	}
	if got.LastFlushedUnix == 0 || got.LastFlushedRFC3339 == "" {
		t.Fatalf("expected last-flushed metadata, got %#v", got)
	}
}

func TestDaemonStateMarkDirtyPersistsDirtyFlag(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &daemonExportApplyGitOps{}
	})
	cfg := daemonTestConfig()

	if _, err := state.ensureSession(context.Background(), "chat-a", cfg); err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if err := state.flushSession(context.Background(), "chat-a", 0); err != nil {
		t.Fatalf("flush session: %v", err)
	}
	if err := state.markDirtySession(context.Background(), "chat-a"); err != nil {
		t.Fatalf("mark dirty: %v", err)
	}
	if !state.sessions["chat-a"].dirty {
		t.Fatal("expected in-memory dirty flag after mark-dirty")
	}

	state.closeAll()
	state2 := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &daemonExportApplyGitOps{}
	})
	sessions, err := state2.listSessions(context.Background())
	if err != nil {
		t.Fatalf("list sessions after restart: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 restored session, got %d", len(sessions))
	}
	if sessions[0].Dirty {
		t.Fatalf("expected recovered session to restart from patch-backed clean state, got %#v", sessions[0])
	}
	if sessions[0].RecoveredCount != 1 {
		t.Fatalf("expected recovered count after dirty restart, got %#v", sessions[0])
	}
}

func TestDaemonHandleRequestRejectsMismatchesAndUnknownMethods(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &daemonExportApplyGitOps{}
	})

	repoResp := state.handleRequest(daemonRequest{Method: "list", RepoRoot: filepath.Join(repoRoot, "other")})
	if repoResp.Code != int(model.ExitEnv) || !strings.Contains(repoResp.Error, "repo root mismatch") {
		t.Fatalf("expected repo mismatch response, got %#v", repoResp)
	}

	gitResp := state.handleRequest(daemonRequest{Method: "list", GitDir: filepath.Join(gitDir, "other")})
	if gitResp.Code != int(model.ExitEnv) || !strings.Contains(gitResp.Error, "git dir mismatch") {
		t.Fatalf("expected git-dir mismatch response, got %#v", gitResp)
	}

	methodResp := state.handleRequest(daemonRequest{Method: "bogus"})
	if methodResp.Code != int(model.ExitEnv) || !strings.Contains(methodResp.Error, "unknown daemon method") {
		t.Fatalf("expected unknown-method response, got %#v", methodResp)
	}
}

func TestDaemonServeConnRoutesLifecycleRequests(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &daemonExportApplyGitOps{}
	})
	cfg := daemonTestConfig()

	ensureResp := daemonServeConnRequest(t, state, daemonRequest{
		Method:     "ensure",
		RepoRoot:   repoRoot,
		GitDir:     gitDir,
		SessionKey: "chat-a",
		Config:     cfg,
	})
	if ensureResp.Error != "" || ensureResp.Session == nil {
		t.Fatalf("expected ensure response with session info, got %#v", ensureResp)
	}
	if ensureResp.Session.SessionKey != "chat-a" {
		t.Fatalf("unexpected ensured session: %#v", ensureResp.Session)
	}

	state.sessions["chat-a"].g = &daemonExportApplyGitOps{exportGitOps: exportGitOps{tracked: []byte("patch-a\n")}}

	markDirtyResp := daemonServeConnRequest(t, state, daemonRequest{
		Method:     "mark_dirty",
		RepoRoot:   repoRoot,
		GitDir:     gitDir,
		SessionKey: "chat-a",
	})
	if markDirtyResp.Error != "" {
		t.Fatalf("expected mark-dirty success, got %#v", markDirtyResp)
	}

	listDirtyResp := daemonServeConnRequest(t, state, daemonRequest{
		Method:   "list",
		RepoRoot: repoRoot,
		GitDir:   gitDir,
	})
	if listDirtyResp.Error != "" || len(listDirtyResp.Sessions) != 1 {
		t.Fatalf("expected single dirty session, got %#v", listDirtyResp)
	}
	if !listDirtyResp.Sessions[0].Dirty {
		t.Fatalf("expected list to report dirty session, got %#v", listDirtyResp.Sessions[0])
	}

	flushResp := daemonServeConnRequest(t, state, daemonRequest{
		Method:      "flush",
		RepoRoot:    repoRoot,
		GitDir:      gitDir,
		SessionKey:  "chat-a",
		MinAgeNanos: 0,
	})
	if flushResp.Error != "" {
		t.Fatalf("expected flush success, got %#v", flushResp)
	}
	patchData, err := os.ReadFile(ensureResp.Session.PatchPath)
	if err != nil {
		t.Fatalf("read flushed patch: %v", err)
	}
	if string(patchData) != "patch-a\n" {
		t.Fatalf("unexpected flushed patch contents: %q", patchData)
	}

	listCleanResp := daemonServeConnRequest(t, state, daemonRequest{
		Method:   "list",
		RepoRoot: repoRoot,
		GitDir:   gitDir,
	})
	if listCleanResp.Error != "" || len(listCleanResp.Sessions) != 1 {
		t.Fatalf("expected single clean session after flush, got %#v", listCleanResp)
	}
	if got := listCleanResp.Sessions[0]; got.Dirty || got.FlushCount != 1 {
		t.Fatalf("expected clean flushed session, got %#v", got)
	}

	pruneResp := daemonServeConnRequest(t, state, daemonRequest{
		Method:         "prune",
		RepoRoot:       repoRoot,
		GitDir:         gitDir,
		IdleForSeconds: 3600,
	})
	if pruneResp.Error != "" || len(pruneResp.Pruned) != 0 {
		t.Fatalf("expected no pruning for fresh session, got %#v", pruneResp)
	}

	endResp := daemonServeConnRequest(t, state, daemonRequest{
		Method:     "end",
		RepoRoot:   repoRoot,
		GitDir:     gitDir,
		SessionKey: "chat-a",
	})
	if endResp.Error != "" || !endResp.Ended {
		t.Fatalf("expected end success, got %#v", endResp)
	}

	listEndedResp := daemonServeConnRequest(t, state, daemonRequest{
		Method:   "list",
		RepoRoot: repoRoot,
		GitDir:   gitDir,
	})
	if listEndedResp.Error != "" || len(listEndedResp.Sessions) != 0 {
		t.Fatalf("expected no sessions after end, got %#v", listEndedResp)
	}
}

func TestDaemonStateRestoresPersistedSessionsAfterRestart(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	now := time.Unix(1700000000, 0)
	g := &daemonExportApplyGitOps{exportGitOps: exportGitOps{tracked: []byte("diff --git a/file.txt b/file.txt\n")}}
	state1 := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return g
	})
	state1.deps.now = func() time.Time { return now }
	cfg := daemonTestConfig()

	info1, leaseID, err := state1.acquireExec(context.Background(), "chat-a", os.Getpid(), cfg)
	if err != nil {
		t.Fatalf("acquire exec: %v", err)
	}
	now = now.Add(time.Minute)
	if err := state1.releaseExec(context.Background(), "chat-a", leaseID); err != nil {
		t.Fatalf("release exec: %v", err)
	}
	state1.closeAll()

	state2 := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return g
	})
	state2.deps.now = func() time.Time { return now.Add(time.Minute) }

	sessions, err := state2.listSessions(context.Background())
	if err != nil {
		t.Fatalf("list restored sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 restored session, got %d", len(sessions))
	}
	got := sessions[0]
	if got.SessionKey != "chat-a" {
		t.Fatalf("unexpected restored session key: %#v", got)
	}
	if got.PatchPath != info1.PatchPath {
		t.Fatalf("expected patch path to survive restart, got %q want %q", got.PatchPath, info1.PatchPath)
	}
	if got.ViewPath == info1.ViewPath {
		t.Fatalf("expected recovered session to get a fresh view path after restart, got %q", got.ViewPath)
	}
	if got.RecoveredCount != 1 {
		t.Fatalf("expected recovered count to increment, got %#v", got)
	}
	if got.FlushCount != 1 || got.LastFlushedUnix == 0 {
		t.Fatalf("expected restored flush metadata, got %#v", got)
	}
}

func TestDaemonStateRestorePreservesOptionMismatchChecks(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state1 := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &daemonExportApplyGitOps{}
	})
	cfg := daemonTestConfig()

	if _, err := state1.ensureSession(context.Background(), "chat-a", cfg); err != nil {
		t.Fatalf("ensure chat-a: %v", err)
	}
	state1.closeAll()

	state2 := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &daemonExportApplyGitOps{}
	})
	cfg.ApplyMode = model.ApplyReject
	if _, err := state2.ensureSession(context.Background(), "chat-a", cfg); err == nil || !strings.Contains(err.Error(), "different options") {
		t.Fatalf("expected options mismatch after restart, got %v", err)
	}
}

func TestDaemonStateEndRemovesPersistedMetadata(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &exportGitOps{}
	})
	cfg := daemonTestConfig()

	if _, err := state.ensureSession(context.Background(), "chat-a", cfg); err != nil {
		t.Fatalf("ensure chat-a: %v", err)
	}
	metaPath := daemonSessionMetaPath(gitDir, "chat-a")
	if _, err := os.Stat(metaPath); err != nil {
		t.Fatalf("expected metadata file, got %v", err)
	}
	if _, err := state.endSession(context.Background(), "chat-a"); err != nil {
		t.Fatalf("end chat-a: %v", err)
	}
	if _, err := os.Stat(metaPath); !os.IsNotExist(err) {
		t.Fatalf("expected metadata removal, got %v", err)
	}
}

func TestDaemonStatePruneUsesPersistedLastUsedAfterRestart(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	now := time.Unix(1700000000, 0)
	state1 := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &daemonExportApplyGitOps{}
	})
	state1.deps.now = func() time.Time { return now }
	cfg := daemonTestConfig()

	for _, key := range []string{"stale", "fresh"} {
		if _, err := state1.ensureSession(context.Background(), key, cfg); err != nil {
			t.Fatalf("ensure %s: %v", key, err)
		}
	}
	state1.sessions["stale"].lastUsed = now.Add(-4 * time.Hour)
	if err := state1.persistSessionLocked(state1.sessions["stale"]); err != nil {
		t.Fatalf("persist stale session: %v", err)
	}
	state1.sessions["fresh"].lastUsed = now.Add(-10 * time.Minute)
	if err := state1.persistSessionLocked(state1.sessions["fresh"]); err != nil {
		t.Fatalf("persist fresh session: %v", err)
	}
	state1.closeAll()

	state2 := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &daemonExportApplyGitOps{}
	})
	state2.deps.now = func() time.Time { return now }
	pruned, err := state2.pruneSessions(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("prune restored sessions: %v", err)
	}
	if len(pruned) != 1 || pruned[0] != "stale" {
		t.Fatalf("expected only stale session to be pruned after restart, got %#v", pruned)
	}
	if _, ok := state2.sessions["fresh"]; !ok {
		t.Fatal("expected fresh restored session to remain")
	}
}

func TestDaemonStateMixedMultiSessionLifecycleRemainsIsolatedAcrossRestart(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	now := time.Unix(1700000000, 0)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &daemonExportApplyGitOps{}
	})
	state.deps.now = func() time.Time { return now }
	cfg := daemonTestConfig()

	infoA, err := state.ensureSession(context.Background(), "agent-a", cfg)
	if err != nil {
		t.Fatalf("ensure agent-a: %v", err)
	}
	infoB, err := state.ensureSession(context.Background(), "agent-b", cfg)
	if err != nil {
		t.Fatalf("ensure agent-b: %v", err)
	}
	if _, err := state.ensureSession(context.Background(), "agent-c", cfg); err != nil {
		t.Fatalf("ensure agent-c: %v", err)
	}

	state.sessions["agent-a"].g = &daemonExportApplyGitOps{exportGitOps: exportGitOps{tracked: []byte("patch-a\n")}}
	state.sessions["agent-b"].g = &daemonExportApplyGitOps{exportGitOps: exportGitOps{tracked: []byte("patch-b\n")}}
	state.sessions["agent-c"].g = &daemonExportApplyGitOps{exportGitOps: exportGitOps{tracked: []byte("patch-c\n")}}

	if err := state.flushSession(context.Background(), "agent-a", 0); err != nil {
		t.Fatalf("flush agent-a: %v", err)
	}
	if err := state.flushSession(context.Background(), "agent-b", 0); err != nil {
		t.Fatalf("flush agent-b: %v", err)
	}
	if err := state.markDirtySession(context.Background(), "agent-b"); err != nil {
		t.Fatalf("mark dirty agent-b: %v", err)
	}
	if !state.sessions["agent-b"].dirty {
		t.Fatal("expected agent-b to remain dirty after mark-dirty")
	}
	if state.sessions["agent-a"].dirty {
		t.Fatal("expected agent-a to remain clean after agent-b mark-dirty")
	}

	state.sessions["agent-a"].lastUsed = now.Add(-2 * time.Hour)
	if err := state.persistSessionLocked(state.sessions["agent-a"]); err != nil {
		t.Fatalf("persist agent-a: %v", err)
	}

	ended, err := state.endSession(context.Background(), "agent-c")
	if err != nil {
		t.Fatalf("end agent-c: %v", err)
	}
	if !ended {
		t.Fatal("expected agent-c to end")
	}

	pruned, err := state.pruneSessions(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("prune sessions: %v", err)
	}
	if len(pruned) != 1 || pruned[0] != "agent-a" {
		t.Fatalf("expected only agent-a to be pruned, got %#v", pruned)
	}
	if _, ok := state.sessions["agent-b"]; !ok {
		t.Fatal("expected agent-b to remain active")
	}
	if _, ok := state.sessions["agent-a"]; ok {
		t.Fatal("expected agent-a to be removed after prune")
	}
	if _, ok := state.sessions["agent-c"]; ok {
		t.Fatal("expected agent-c to stay ended")
	}

	if _, err := os.Stat(daemonSessionMetaPath(gitDir, "agent-a")); !os.IsNotExist(err) {
		t.Fatalf("expected agent-a metadata removal, got %v", err)
	}
	if _, err := os.Stat(daemonSessionMetaPath(gitDir, "agent-c")); !os.IsNotExist(err) {
		t.Fatalf("expected agent-c metadata removal, got %v", err)
	}
	if _, err := os.Stat(daemonSessionMetaPath(gitDir, "agent-b")); err != nil {
		t.Fatalf("expected agent-b metadata to remain, got %v", err)
	}

	patchA, err := os.ReadFile(infoA.PatchPath)
	if err != nil {
		t.Fatalf("read agent-a patch: %v", err)
	}
	if string(patchA) != "patch-a\n" {
		t.Fatalf("expected agent-a patch to remain isolated, got %q", patchA)
	}
	patchB, err := os.ReadFile(infoB.PatchPath)
	if err != nil {
		t.Fatalf("read agent-b patch: %v", err)
	}
	if string(patchB) != "patch-b\n" {
		t.Fatalf("expected agent-b patch to remain isolated, got %q", patchB)
	}

	state.closeAll()
	state2 := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &daemonExportApplyGitOps{}
	})
	state2.deps.now = func() time.Time { return now.Add(time.Minute) }

	sessions, err := state2.listSessions(context.Background())
	if err != nil {
		t.Fatalf("list restored sessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 restored session, got %d", len(sessions))
	}
	got := sessions[0]
	if got.SessionKey != "agent-b" {
		t.Fatalf("expected only agent-b after restart, got %#v", got)
	}
	if got.PatchPath != infoB.PatchPath {
		t.Fatalf("expected stable patch path for agent-b, got %q want %q", got.PatchPath, infoB.PatchPath)
	}
	if got.Dirty {
		t.Fatalf("expected restart to recover agent-b to clean patch-backed state, got %#v", got)
	}
	if got.RecoveredCount != 1 {
		t.Fatalf("expected recovered count for agent-b, got %#v", got)
	}
}

func TestDaemonStateStressConcurrentSessionIsolation(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &daemonExportApplyGitOps{}
	})
	cfg := daemonTestConfig()
	const sessionCount = 6
	const rounds = 3

	type sessionExpectation struct {
		key       string
		patch     string
		patchPath string
	}
	expectations := make([]sessionExpectation, 0, sessionCount)
	for i := 0; i < sessionCount; i++ {
		key := fmt.Sprintf("chat-%d", i)
		info, err := state.ensureSession(context.Background(), key, cfg)
		if err != nil {
			t.Fatalf("ensure %s: %v", key, err)
		}
		g := &daemonExportApplyGitOps{}
		state.sessions[key].g = g
		expectations = append(expectations, sessionExpectation{key: key, patchPath: info.PatchPath})
	}

	start := make(chan struct{})
	errCh := make(chan error, sessionCount)
	var wg sync.WaitGroup
	for i := range expectations {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			g := state.sessions[expectations[i].key].g.(*daemonExportApplyGitOps)
			for round := 0; round < rounds; round++ {
				payload := fmt.Sprintf("%s-round-%d\n", expectations[i].key, round)
				g.tracked = []byte(payload)
				_, leaseID, err := state.acquireExec(context.Background(), expectations[i].key, os.Getpid(), cfg)
				if err != nil {
					errCh <- fmt.Errorf("acquire %s round %d: %w", expectations[i].key, round, err)
					return
				}
				time.Sleep(2 * time.Millisecond)
				if err := state.releaseExec(context.Background(), expectations[i].key, leaseID); err != nil {
					errCh <- fmt.Errorf("release %s round %d: %w", expectations[i].key, round, err)
					return
				}
				expectations[i].patch = payload
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, expectation := range expectations {
		patchData, err := os.ReadFile(expectation.patchPath)
		if err != nil {
			t.Fatalf("read %s patch: %v", expectation.key, err)
		}
		if string(patchData) != expectation.patch {
			t.Fatalf("unexpected patch for %s: got %q want %q", expectation.key, patchData, expectation.patch)
		}
		for _, other := range expectations {
			if other.key == expectation.key || other.patch == "" {
				continue
			}
			if string(patchData) == other.patch {
				t.Fatalf("patch for %s was overwritten by %s payload %q", expectation.key, other.key, other.patch)
			}
		}
	}
}

func TestDaemonStateConcurrentSessionsRetainIndependentMetadataAcrossRestart(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &daemonExportApplyGitOps{}
	})
	cfg := daemonTestConfig()
	const sessionCount = 8
	const rounds = 4

	type sessionExpectation struct {
		key        string
		viewPath   string
		patchPath  string
		finalPatch string
		g          *daemonExportApplyGitOps
	}
	expectations := make([]sessionExpectation, 0, sessionCount)
	for i := 0; i < sessionCount; i++ {
		key := fmt.Sprintf("agent-%02d", i)
		info, err := state.ensureSession(context.Background(), key, cfg)
		if err != nil {
			t.Fatalf("ensure %s: %v", key, err)
		}
		g := &daemonExportApplyGitOps{}
		state.sessions[key].g = g
		expectations = append(expectations, sessionExpectation{
			key:       key,
			viewPath:  info.ViewPath,
			patchPath: info.PatchPath,
			g:         g,
		})
	}

	start := make(chan struct{})
	errCh := make(chan error, sessionCount)
	var wg sync.WaitGroup
	for i := range expectations {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for round := 0; round < rounds; round++ {
				payload := fmt.Sprintf("%s-round-%d\n", expectations[i].key, round)
				expectations[i].g.tracked = []byte(payload)
				_, leaseID, err := state.acquireExec(context.Background(), expectations[i].key, os.Getpid(), cfg)
				if err != nil {
					errCh <- fmt.Errorf("acquire %s round %d: %w", expectations[i].key, round, err)
					return
				}
				if err := state.releaseExec(context.Background(), expectations[i].key, leaseID); err != nil {
					errCh <- fmt.Errorf("release %s round %d: %w", expectations[i].key, round, err)
					return
				}
				expectations[i].finalPatch = payload
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}

	sessionsBeforeRestart, err := state.listSessions(context.Background())
	if err != nil {
		t.Fatalf("list sessions before restart: %v", err)
	}
	if len(sessionsBeforeRestart) != sessionCount {
		t.Fatalf("expected %d sessions before restart, got %d", sessionCount, len(sessionsBeforeRestart))
	}
	beforeByKey := daemonSessionsByKey(sessionsBeforeRestart)
	for _, expectation := range expectations {
		got, ok := beforeByKey[expectation.key]
		if !ok {
			t.Fatalf("missing session before restart for %q", expectation.key)
		}
		if got.FlushCount != rounds {
			t.Fatalf("expected %q flush count %d before restart, got %#v", expectation.key, rounds, got)
		}
		if got.Dirty {
			t.Fatalf("expected %q clean before restart, got %#v", expectation.key, got)
		}
		patchData, err := os.ReadFile(expectation.patchPath)
		if err != nil {
			t.Fatalf("read patch for %q: %v", expectation.key, err)
		}
		if string(patchData) != expectation.finalPatch {
			t.Fatalf("unexpected patch for %q before restart: got %q want %q", expectation.key, patchData, expectation.finalPatch)
		}
	}

	state.closeAll()
	state2 := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &daemonExportApplyGitOps{}
	})
	sessionsAfterRestart, err := state2.listSessions(context.Background())
	if err != nil {
		t.Fatalf("list sessions after restart: %v", err)
	}
	if len(sessionsAfterRestart) != sessionCount {
		t.Fatalf("expected %d sessions after restart, got %d", sessionCount, len(sessionsAfterRestart))
	}
	afterByKey := daemonSessionsByKey(sessionsAfterRestart)
	for _, expectation := range expectations {
		got, ok := afterByKey[expectation.key]
		if !ok {
			t.Fatalf("missing restored session for %q", expectation.key)
		}
		if got.PatchPath != expectation.patchPath {
			t.Fatalf("expected stable patch path for %q, got %q want %q", expectation.key, got.PatchPath, expectation.patchPath)
		}
		if got.ViewPath == expectation.viewPath {
			t.Fatalf("expected fresh recovered view for %q, got %q", expectation.key, got.ViewPath)
		}
		if got.FlushCount != rounds || got.RecoveredCount != 1 {
			t.Fatalf("unexpected restored counters for %q: %#v", expectation.key, got)
		}
		if got.Dirty {
			t.Fatalf("expected %q clean after restart, got %#v", expectation.key, got)
		}
		patchData, err := os.ReadFile(expectation.patchPath)
		if err != nil {
			t.Fatalf("read restored patch for %q: %v", expectation.key, err)
		}
		if string(patchData) != expectation.finalPatch {
			t.Fatalf("unexpected restored patch for %q: got %q want %q", expectation.key, patchData, expectation.finalPatch)
		}
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
	if err := state.flushSession(context.Background(), "chat-a", 0); err != nil {
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
	if err := state.flushSession(context.Background(), "chat-a", 0); err == nil || !strings.Contains(err.Error(), "still busy") {
		t.Fatalf("expected busy flush rejection, got %v", err)
	}
}

func TestDaemonStateFlushMinAgeSkipsRecentFlushes(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	g := &exportGitOps{tracked: []byte("first\n")}
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return g
	})
	cfg := daemonTestConfig()

	info, err := state.ensureSession(context.Background(), "chat-a", cfg)
	if err != nil {
		t.Fatalf("ensure session: %v", err)
	}
	if err := state.flushSession(context.Background(), "chat-a", 0); err != nil {
		t.Fatalf("initial flush: %v", err)
	}
	g.tracked = []byte("second\n")
	if err := state.flushSession(context.Background(), "chat-a", time.Hour); err != nil {
		t.Fatalf("min-age flush: %v", err)
	}
	patchData, err := os.ReadFile(info.PatchPath)
	if err != nil {
		t.Fatalf("read patch: %v", err)
	}
	if string(patchData) != "first\n" {
		t.Fatalf("expected min-age flush to keep prior patch contents, got %q", patchData)
	}
}

func TestDaemonStatePruneRemovesOnlyIdleNonBusySessions(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return &exportGitOps{}
	})
	cfg := daemonTestConfig()

	if _, err := state.ensureSession(context.Background(), "chat-a", cfg); err != nil {
		t.Fatalf("ensure chat-a: %v", err)
	}
	if _, err := state.ensureSession(context.Background(), "chat-b", cfg); err != nil {
		t.Fatalf("ensure chat-b: %v", err)
	}
	if _, _, err := state.acquireExec(context.Background(), "chat-c", os.Getpid(), cfg); err != nil {
		t.Fatalf("acquire chat-c: %v", err)
	}
	baseNow := state.deps.now()
	state.sessions["chat-a"].lastUsed = baseNow.Add(-2 * time.Hour)
	state.sessions["chat-b"].lastUsed = baseNow.Add(-10 * time.Minute)
	state.sessions["chat-c"].lastUsed = baseNow.Add(-3 * time.Hour)

	pruned, err := state.pruneSessions(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("prune sessions: %v", err)
	}
	if len(pruned) != 1 || pruned[0] != "chat-a" {
		t.Fatalf("expected only chat-a to be pruned, got %#v", pruned)
	}
	if _, ok := state.sessions["chat-a"]; ok {
		t.Fatal("expected chat-a to be removed")
	}
	if _, ok := state.sessions["chat-b"]; !ok {
		t.Fatal("expected chat-b to remain")
	}
	if _, ok := state.sessions["chat-c"]; !ok {
		t.Fatal("expected busy chat-c to remain")
	}
}

func TestDaemonStatePruneRecoversStaleBusySessionsBeforeRemovingThem(t *testing.T) {
	repoRoot, gitDir := daemonTestRepo(t)
	g := &exportGitOps{tracked: []byte("diff --git a/stale.txt b/stale.txt\n")}
	state := newTestDaemonState(t, repoRoot, gitDir, func() model.GitOps {
		return g
	})
	cfg := daemonTestConfig()

	info, _, err := state.acquireExec(context.Background(), "chat-a", -1, cfg)
	if err != nil {
		t.Fatalf("acquire stale chat-a: %v", err)
	}
	state.sessions["chat-a"].lastUsed = state.deps.now().Add(-2 * time.Hour)

	pruned, err := state.pruneSessions(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("prune sessions: %v", err)
	}
	if len(pruned) != 1 || pruned[0] != "chat-a" {
		t.Fatalf("expected stale chat-a to be pruned, got %#v", pruned)
	}
	patchData, err := os.ReadFile(info.PatchPath)
	if err != nil {
		t.Fatalf("read pruned patch: %v", err)
	}
	if string(patchData) != string(g.tracked) {
		t.Fatalf("expected stale recovery flush before prune, got %q", patchData)
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

func TestDaemonClientMethodsRoundTripRequestsAndErrors(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "daemon.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	defer listener.Close()

	cfg := daemonTestConfig()
	var requests []daemonRequest
	serverDone := make(chan error, 1)
	go func() {
		for i := 0; i < 8; i++ {
			conn, err := listener.Accept()
			if err != nil {
				serverDone <- err
				return
			}
			var req daemonRequest
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				_ = conn.Close()
				serverDone <- err
				return
			}
			requests = append(requests, req)
			var resp daemonResponse
			switch req.Method {
			case "ensure":
				resp = daemonResponse{Session: &daemonSessionInfo{SessionKey: req.SessionKey, ViewPath: "/tmp/view"}}
			case "list":
				resp = daemonResponse{Sessions: []daemonSessionInfo{{SessionKey: "chat-a"}}}
			case "acquire_exec":
				resp = daemonResponse{
					Session: &daemonSessionInfo{SessionKey: req.SessionKey, ViewPath: "/tmp/view"},
					LeaseID: "lease-123",
				}
			case "release_exec", "flush", "mark_dirty":
				resp = daemonResponse{}
			case "prune":
				resp = daemonResponse{Pruned: []string{"chat-stale"}}
			case "end":
				resp = daemonResponse{Error: "still busy", Code: int(model.ExitEnv)}
			default:
				resp = daemonResponse{Error: "unexpected method"}
			}
			if err := json.NewEncoder(conn).Encode(resp); err != nil {
				_ = conn.Close()
				serverDone <- err
				return
			}
			_ = conn.Close()
		}
		serverDone <- nil
	}()

	client := &daemonClient{
		socketPath: socketPath,
		repoRoot:   "/repo",
		gitDir:     "/repo/.git",
	}

	info, err := client.ensure(context.Background(), "chat-a", cfg)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if info == nil || info.SessionKey != "chat-a" {
		t.Fatalf("unexpected ensure info: %#v", info)
	}

	sessions, err := client.list(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(sessions) != 1 || sessions[0].SessionKey != "chat-a" {
		t.Fatalf("unexpected list response: %#v", sessions)
	}

	acquireResp, err := client.acquireExec(context.Background(), "chat-a", cfg)
	if err != nil {
		t.Fatalf("acquireExec: %v", err)
	}
	if acquireResp == nil || acquireResp.LeaseID != "lease-123" {
		t.Fatalf("unexpected acquire response: %#v", acquireResp)
	}

	if err := client.releaseExec(context.Background(), "chat-a", "lease-123"); err != nil {
		t.Fatalf("releaseExec: %v", err)
	}
	if err := client.flush(context.Background(), "chat-a", 3*time.Second); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := client.markDirty(context.Background(), "chat-a"); err != nil {
		t.Fatalf("markDirty: %v", err)
	}

	pruned, err := client.prune(context.Background(), 90*time.Second)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if len(pruned) != 1 || pruned[0] != "chat-stale" {
		t.Fatalf("unexpected prune response: %#v", pruned)
	}

	err = client.end(context.Background(), "chat-a")
	var personaErr *model.PersonaError
	if !errors.As(err, &personaErr) {
		t.Fatalf("expected PersonaError from end, got %v", err)
	}
	if personaErr.Code != model.ExitEnv || personaErr.Op != "end" {
		t.Fatalf("unexpected end error: %+v", personaErr)
	}

	if err := <-serverDone; err != nil {
		t.Fatalf("daemon client test server: %v", err)
	}
	if len(requests) != 8 {
		t.Fatalf("expected 8 daemon requests, got %d", len(requests))
	}
	for i, req := range requests {
		if req.RepoRoot != "/repo" || req.GitDir != "/repo/.git" {
			t.Fatalf("request %d missing repo context: %#v", i, req)
		}
	}
	expectedMethods := []string{"ensure", "list", "acquire_exec", "release_exec", "flush", "mark_dirty", "prune", "end"}
	for i, method := range expectedMethods {
		if requests[i].Method != method {
			t.Fatalf("request %d expected method %q, got %#v", i, method, requests[i])
		}
	}
	if requests[0].Config != cfg || requests[2].Config != cfg {
		t.Fatalf("expected config to propagate in ensure/acquire requests, got %#v %#v", requests[0], requests[2])
	}
	if requests[2].OwnerPID != os.Getpid() {
		t.Fatalf("expected acquire_exec owner pid %d, got %#v", os.Getpid(), requests[2])
	}
	if requests[3].LeaseID != "lease-123" {
		t.Fatalf("expected release_exec lease to propagate, got %#v", requests[3])
	}
	if requests[4].MinAgeNanos != int64(3*time.Second) {
		t.Fatalf("expected flush min-age to propagate, got %#v", requests[4])
	}
	if requests[6].IdleForSeconds != 90 {
		t.Fatalf("expected prune idle-for seconds to propagate, got %#v", requests[6])
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

func daemonSessionsByKey(sessions []daemonSessionInfo) map[string]daemonSessionInfo {
	byKey := make(map[string]daemonSessionInfo, len(sessions))
	for _, sess := range sessions {
		byKey[sess.SessionKey] = sess
	}
	return byKey
}

func daemonServeConnRequest(t *testing.T, state *daemonState, req daemonRequest) daemonResponse {
	t.Helper()
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	done := make(chan struct{})
	go func() {
		state.serveConn(serverConn)
		close(done)
	}()

	if err := json.NewEncoder(clientConn).Encode(req); err != nil {
		t.Fatalf("encode daemon request: %v", err)
	}
	var resp daemonResponse
	if err := json.NewDecoder(clientConn).Decode(&resp); err != nil {
		t.Fatalf("decode daemon response: %v", err)
	}
	<-done
	return resp
}
