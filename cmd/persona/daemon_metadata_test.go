package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"persona/internal/model"
)

func TestDaemonMetadataIgnoresInterruptedTemporaryFiles(t *testing.T) {
	repo, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repo, gitDir, func() model.GitOps { return &daemonExportApplyGitOps{} })
	if _, err := state.ensureSession(context.Background(), "healthy", daemonTestConfig()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(daemonSessionMetaPath(gitDir, "healthy"))
	for _, name := range []string{".session-meta-1234.tmp", "session-meta-1234.json", "unrelated.json", "session-zzzzzzzzzzzzzzzz.json"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{interrupted"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	paths, err := daemonSessionMetaPaths(gitDir)
	if err != nil || len(paths) != 1 || paths[0] != daemonSessionMetaPath(gitDir, "healthy") {
		t.Fatalf("temporary files entered recovery: %v %v", paths, err)
	}
	infos, err := state.listSessions(context.Background())
	if err != nil || len(infos) != 1 || infos[0].SessionKey != "healthy" {
		t.Fatalf("list: %#v %v", infos, err)
	}
}

func TestDaemonMetadataWriterNeverReplacesReadableCheckpointWithOversizedState(t *testing.T) {
	meta := daemonSessionMeta{SessionKey: "key", Config: daemonTestConfig()}
	path := filepath.Join(t.TempDir(), "state.json")
	if err := writeDaemonSessionMeta(path, meta); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	meta.InitialIgnored = []string{strings.Repeat("ignored/", daemonMetadataLimit)}
	if err := writeDaemonSessionMeta(path, meta); err == nil {
		t.Fatal("writer accepted metadata its reader cannot recover")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("oversized metadata damaged prior checkpoint: %v", err)
	}
	if _, err := readDaemonSessionMeta(path); err != nil {
		t.Fatalf("prior metadata became unreadable: %v", err)
	}
}

func TestDaemonMetadataValidatesSchemaAndSaveIdentity(t *testing.T) {
	base := daemonSessionMeta{SchemaVersion: daemonMetadataSchema, SessionKey: "key", Config: daemonTestConfig()}
	for _, tc := range []struct {
		name   string
		change func(*daemonSessionMeta)
	}{
		{"future schema", func(m *daemonSessionMeta) { m.SchemaVersion++ }},
		{"missing key", func(m *daemonSessionMeta) { m.SessionKey = "" }},
		{"invalid options", func(m *daemonSessionMeta) { m.Config.IgnoredMode = "unknown" }},
		{"relative base", func(m *daemonSessionMeta) { m.BaseOID = "HEAD~1" }},
		{"future durable generation", func(m *daemonSessionMeta) { m.LastDurableGeneration = 1 }},
		{"clean unsaved generation", func(m *daemonSessionMeta) { m.Generation = 1 }},
		{"missing checkpoint checksum", func(m *daemonSessionMeta) { m.SessionRoot = "/preserved/session" }},
		{"bad checksum", func(m *daemonSessionMeta) { m.PatchChecksum = "invalid" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := base
			tc.change(&meta)
			data, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "meta.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readDaemonSessionMeta(path); err == nil {
				t.Fatal("invalid metadata accepted")
			}
		})
	}
}

func TestDaemonInterruptedMetadataSaveKeepsDirtyCheckpoint(t *testing.T) {
	repo, gitDir := daemonTestRepo(t)
	g := &safetyExportGit{content: []byte("saved bytes\n")}
	state := newTestDaemonState(t, repo, gitDir, func() model.GitOps { return g })
	if _, err := state.ensureSession(context.Background(), "key", daemonTestConfig()); err != nil {
		t.Fatal(err)
	}
	state.deps.writeMeta = func(path string, meta daemonSessionMeta) error {
		if !meta.Dirty {
			return errors.New("injected metadata commit failure")
		}
		return writeDaemonSessionMeta(path, meta)
	}
	if err := state.flushSession(context.Background(), "key", 0); err == nil {
		t.Fatal("expected failed final metadata commit")
	}
	meta, err := readDaemonSessionMeta(daemonSessionMetaPath(gitDir, "key"))
	if err != nil {
		t.Fatal(err)
	}
	if !meta.Dirty {
		t.Fatal("patch save had no durable in-progress marker")
	}
	sess := state.sessions["key"]
	if !sess.dirty || sess.lastError == "" || !state.cached["key"].Dirty {
		t.Fatal("failed save was reported clean")
	}
	if _, err := os.Stat(sess.sess.Upper); err != nil {
		t.Fatalf("upper lost after metadata failure: %v", err)
	}
	data, err := os.ReadFile(sess.info.PatchPath)
	if err != nil || string(data) != string(g.content) {
		t.Fatalf("patch was not committed before failure: %q %v", data, err)
	}
	state.deps.writeMeta = nil
	if err := state.flushSession(context.Background(), "key", 0); err != nil {
		t.Fatal(err)
	}
	meta, err = readDaemonSessionMeta(sess.metaPath)
	if err != nil || meta.Dirty || meta.PatchChecksum == "" {
		t.Fatalf("retry did not commit checksum: %#v %v", meta, err)
	}
}

func TestDaemonRecoveryRejectsMismatchedSavedPatch(t *testing.T) {
	repo, gitDir := daemonTestRepo(t)
	create := func() *daemonState {
		return newTestDaemonState(t, repo, gitDir, func() model.GitOps { return &safetyExportGit{} })
	}
	first := create()
	info, err := first.ensureSession(context.Background(), "key", daemonTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.flushSession(context.Background(), "key", 0); err != nil {
		t.Fatal(err)
	}
	first.closeAll()
	if err := os.WriteFile(info.PatchPath, []byte("different patch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := create()
	if _, err := second.ensureSession(context.Background(), "key", daemonTestConfig()); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("mismatched checkpoint accepted: %v", err)
	}
	data, err := os.ReadFile(info.PatchPath)
	if err != nil || string(data) != "different patch\n" {
		t.Fatalf("failed recovery overwrote patch: %q %v", data, err)
	}
}

func TestDaemonRecoveryChecksPatchBeforeRemovingStaleUpper(t *testing.T) {
	repo, gitDir := daemonTestRepo(t)
	create := func() *daemonState {
		return newTestDaemonState(t, repo, gitDir, func() model.GitOps { return &safetyExportGit{} })
	}
	first := create()
	if _, err := first.ensureSession(context.Background(), "key", daemonTestConfig()); err != nil {
		t.Fatal(err)
	}
	if err := first.flushSession(context.Background(), "key", 0); err != nil {
		t.Fatal(err)
	}
	sess := first.sessions["key"]
	sentinel := filepath.Join(sess.sess.Upper, "checkpoint-copy")
	if err := os.WriteFile(sentinel, []byte("keep the saved upper"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after a clean checkpoint, retaining its physical roots.
	if err := sess.lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := sess.store.Close(); err != nil {
		t.Fatal(err)
	}
	first.removeSession("key")
	t.Cleanup(func() { _ = sess.cleanup.Run() })
	if err := os.WriteFile(sess.info.PatchPath, []byte("corrupt checkpoint"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := create()
	if _, err := second.recoverSession(context.Background(), "key", false); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("corrupt checkpoint entered recovery cleanup: %v", err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep the saved upper" {
		t.Fatalf("recovery deleted the remaining good copy: %q %v", data, err)
	}
}
