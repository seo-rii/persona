package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"persona/internal/model"
)

func TestDaemonEndRetriesCleanupWithoutReopeningClosedStore(t *testing.T) {
	repo, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repo, gitDir, func() model.GitOps { return &safetyExportGit{} })
	if _, err := state.ensureSession(context.Background(), "key", daemonTestConfig()); err != nil {
		t.Fatal(err)
	}
	sess := state.sessions["key"]
	fail := true
	calls := 0
	sess.cleanup.Push(func() error {
		calls++
		if fail {
			return errors.New("cleanup interrupted")
		}
		return nil
	})
	if _, err := state.endSession(context.Background(), "key"); err == nil {
		t.Fatal("expected cleanup failure")
	}
	if !sess.closed || daemonSessionSnapshot(sess).State != "cleanup_required" {
		t.Fatal("partially closed session exposed as usable")
	}
	if _, err := state.ensureSession(context.Background(), "key", daemonTestConfig()); err == nil {
		t.Fatal("partially closed session reused")
	}
	if err := state.flushSession(context.Background(), "key", 0); err == nil {
		t.Fatal("partially closed store exported")
	}
	if err := state.markDirtySession(context.Background(), "key"); err == nil {
		t.Fatal("partially closed session accepted a mutation")
	}
	fail = false
	if ended, err := state.endSession(context.Background(), "key"); err != nil || !ended {
		t.Fatalf("cleanup could not be retried: %t %v", ended, err)
	}
	if calls != 2 || state.sessions["key"] != nil {
		t.Fatalf("cleanup retry result: %d", calls)
	}
}

func TestDaemonDrainAllowsOutstandingLeaseRelease(t *testing.T) {
	repo, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repo, gitDir, func() model.GitOps { return &safetyExportGit{content: []byte("last write\n")} })
	info, lease, err := state.acquireExec(context.Background(), "key", os.Getpid(), daemonTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { state.drainMutations(ctx); close(done) }()
	for {
		state.mu.Lock()
		closing := state.closing
		state.mu.Unlock()
		if closing {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("drain did not start")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if _, err := state.ensureSession(ctx, "new", daemonTestConfig()); err == nil {
		t.Fatal("new work entered draining daemon")
	}
	select {
	case <-done:
		t.Fatal("drain ignored the live writer")
	default:
	}
	if err := state.releaseExecGeneration(ctx, "key", lease, info.ViewGeneration); err != nil {
		t.Fatalf("drain refused lease completion: %v", err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("drain did not observe completed lease")
	}
	if err := state.closeSessions(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(info.PatchPath)
	if err != nil || string(data) != "last write\n" {
		t.Fatalf("last write lost during drain: %q %v", data, err)
	}
}

func TestDaemonDrainTimeoutPreservesLiveWriter(t *testing.T) {
	repo, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repo, gitDir, func() model.GitOps { return &safetyExportGit{} })
	if _, _, err := state.acquireExec(context.Background(), "key", os.Getpid(), daemonTestConfig()); err != nil {
		t.Fatal(err)
	}
	upper := state.sessions["key"].sess.Upper
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	state.drainMutations(ctx)
	if err := state.closeSessions(); err == nil {
		t.Fatal("unsaved shutdown failure was hidden")
	}
	if _, err := os.Stat(upper); err != nil {
		t.Fatalf("live writer upper deleted: %v", err)
	}
	meta, err := readDaemonSessionMeta(daemonSessionMetaPath(gitDir, "key"))
	if err != nil || !meta.Dirty || meta.LastError == "" {
		t.Fatalf("missing recovery state: %#v %v", meta, err)
	}
}
