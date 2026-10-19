package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"

	"persona/internal/model"
)

func TestDaemonExecutionAttachmentProtectsLiveGroup(t *testing.T) {
	repo, gitDir := daemonTestRepo(t)
	state := newTestDaemonState(t, repo, gitDir, func() model.GitOps { return &safetyExportGit{} })
	info, lease, err := state.acquireExec(context.Background(), "exec", os.Getpid(), daemonTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sh", "-c", "read token")
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close(); _ = child.Wait() }()
	for _, attempt := range []struct {
		lease      string
		generation uint64
		pid        int
	}{
		{"stale", info.ViewGeneration, child.Process.Pid},
		{lease, info.ViewGeneration + 1, child.Process.Pid},
		{lease, info.ViewGeneration, os.Getpid()},
	} {
		if err := state.attachExec(context.Background(), "exec", attempt.lease, attempt.generation, attempt.pid); err == nil {
			t.Fatal("accepted an unrelated or stale execution process")
		}
	}
	state.deps.writeMeta = func(string, daemonSessionMeta) error { return errors.New("metadata save failed") }
	if err := state.attachExec(context.Background(), "exec", lease, info.ViewGeneration, child.Process.Pid); err == nil {
		t.Fatal("acknowledged execution before its identity was durably saved")
	}
	state.deps.writeMeta = nil
	if err := state.attachExec(context.Background(), "exec", lease, info.ViewGeneration, child.Process.Pid); err != nil {
		t.Fatal(err)
	}
	meta, err := readDaemonSessionMeta(daemonSessionMetaPath(gitDir, "exec"))
	if err != nil || meta.BusyExecPGID != child.Process.Pid || meta.BusyExecStart == "" {
		t.Fatalf("execution identity was not durable: %+v, %v", meta, err)
	}
	for _, change := range []func(*daemonSessionMeta){
		func(m *daemonSessionMeta) { m.BusyExecPGID = -1 },
		func(m *daemonSessionMeta) { m.BusyExecPGID = 0 },
		func(m *daemonSessionMeta) { m.BusyExecStart = "" },
		func(m *daemonSessionMeta) { m.BusyExecStart = "not-a-start-time" },
		func(m *daemonSessionMeta) { m.Dirty = false; m.LastDurableGeneration = m.Generation },
	} {
		invalid := meta
		change(&invalid)
		if err := validateDaemonSessionMeta(invalid); err == nil {
			t.Fatal("accepted an incomplete or clean execution group checkpoint")
		}
	}
	if err := state.releaseExec(context.Background(), "exec", lease); err == nil || !strings.Contains(err.Error(), "live command") {
		t.Fatalf("released an active execution group: %v", err)
	}
	sess := state.sessions["exec"]
	sess.busyOwnerPID, sess.busyOwnerStart = 99999999, ""
	if err := state.recoverBusyLocked(context.Background(), sess); err != nil || !sess.busy {
		t.Fatalf("client death reclaimed a live group's lease: busy=%t error=%v", sess.busy, err)
	}
	_ = input.Close()
	_ = child.Wait()
	if err := state.recoverBusyLocked(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if sess.busy || sess.dirty || sess.busyExecPGID != 0 || sess.busyExecStart != "" {
		t.Fatal("finished group was not saved and released")
	}
}

func TestParseExecutionProcessHandlesCommandParentheses(t *testing.T) {
	data := "123 (worker ) with spaces) S 111 123 111 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 98765 0"
	process, err := parseExecutionProcess(data)
	if err != nil || process.parent != 111 || process.group != 123 || process.start != "98765" || process.state != "S" {
		t.Fatalf("wrong execution identity: %+v %v", process, err)
	}
	for _, data := range []string{"", "123 (bad) S", strings.Replace(data, "98765", "broken", 1)} {
		if _, err := parseExecutionProcess(data); err == nil {
			t.Fatal("accepted malformed process identity")
		}
	}
}

type executionCounterFS struct {
	fstest.MapFS
	counters []string
	reads    int
}

func (p *executionCounterFS) ReadFile(path string) ([]byte, error) {
	if path == "sys/kernel/ns_last_pid" && len(p.counters) != 0 {
		index := min(p.reads, len(p.counters)-1)
		p.reads++
		return []byte(p.counters[index]), nil
	}
	return p.MapFS.ReadFile(path)
}

func TestExecutionGroupSnapshotRequiresStoppedWriters(t *testing.T) {
	stat := "123 (worker) Z 111 123 111 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 98765 0"
	for _, test := range []struct {
		name      string
		counters  []string
		worker    bool
		missing   bool
		wantAlive bool
		wantReads int
	}{
		{name: "stable zombie is finished", counters: []string{"100"}, wantReads: 2},
		{name: "zombie leader has live threads", counters: []string{"100"}, worker: true, wantAlive: true, wantReads: 1},
		{name: "retry after transient process churn", counters: []string{"100", "101", "101", "101"}, wantReads: 4},
		{name: "preserve during continuing process churn", counters: []string{"100", "101", "102", "103", "104", "105"}, wantAlive: true, wantReads: 6},
		{name: "preserve on inspection error", missing: true, wantAlive: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			procFS := &executionCounterFS{MapFS: fstest.MapFS{
				"123/stat":          &fstest.MapFile{Data: []byte(stat)},
				"123/task/123/stat": &fstest.MapFile{Data: []byte(stat)},
			}, counters: test.counters}
			if test.worker {
				procFS.MapFS["123/task/124/stat"] = &fstest.MapFile{Data: []byte(strings.Replace(stat, ") Z", ") S", 1))}
			}
			if !test.missing {
				procFS.MapFS["sys/kernel/ns_last_pid"] = &fstest.MapFile{Data: []byte("100")}
			}
			if got := executionGroupActive(procFS, 123, "98765"); got != test.wantAlive {
				t.Fatalf("live execution=%t, want %t", got, test.wantAlive)
			}
			if procFS.reads != test.wantReads {
				t.Fatalf("unexpected snapshot retries: %d counter reads, want %d", procFS.reads, test.wantReads)
			}
		})
	}
}
