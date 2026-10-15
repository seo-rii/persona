package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"persona/internal/gitx"
	"persona/internal/model"
	"persona/internal/session"
	"persona/internal/testutil"
)

func TestDaemonRecoveryNeverDeletesMetadataPaths(t *testing.T) {
	for _, kind := range []string{"root", "parent", "same", "sibling", "symlink", "child"} {
		t.Run(kind, func(t *testing.T) {
			repo, git := daemonTestRepo(t)
			state := newTestDaemonState(t, repo, git, func() model.GitOps { return &daemonExportApplyGitOps{} })
			owned, err := session.Create(git)
			if err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()
			sentinel := filepath.Join(outside, "sentinel")
			if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := owned.Root
			switch kind {
			case "root":
				path = "/"
			case "parent":
				path = filepath.Join(git, "persona")
			case "same":
				path = filepath.Dir(owned.Root)
			case "sibling":
				path = outside
			case "symlink":
				path = filepath.Join(filepath.Dir(owned.Root), "alias")
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			}
			meta := daemonSessionMeta{SessionKey: "bad", Config: daemonTestConfig(), SessionRoot: path, Dirty: true}
			_, _ = state.restoreSessionLocked(context.Background(), meta, false)
			if got, err := os.ReadFile(sentinel); err != nil || string(got) != "keep" {
				t.Fatalf("sentinel changed: %q %v", got, err)
			}
			if _, err := os.Stat(owned.Root); err != nil {
				t.Fatalf("owned upper unexpectedly removed: %v", err)
			}
		})
	}
}

func TestDaemonShutdownFlushesDirtyAndPreservesFailures(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{true: "failed", false: "durable"}[fail], func(t *testing.T) {
			repo, git := daemonTestRepo(t)
			g := &safetyExportGit{}
			state := newTestDaemonState(t, repo, git, func() model.GitOps { return g })
			info, err := state.ensureSession(context.Background(), "key", daemonTestConfig())
			if err != nil {
				t.Fatal(err)
			}
			if err := state.flushSession(context.Background(), "key", 0); err != nil {
				t.Fatal(err)
			}
			g.content = []byte("new durable patch\n")
			g.fail = fail
			if err := state.markDirtySession(context.Background(), "key"); err != nil {
				t.Fatal(err)
			}
			root := state.sessions["key"].sess.Root
			if err := os.WriteFile(filepath.Join(root, "upper", "sentinel"), []byte("unsaved"), 0o600); err != nil {
				t.Fatal(err)
			}
			state.closeAll()
			meta, err := readDaemonSessionMeta(daemonSessionMetaPath(git, "key"))
			if err != nil {
				t.Fatal(err)
			}
			patch, err := os.ReadFile(info.PatchPath)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if !meta.Dirty || meta.LastError == "" {
					t.Fatalf("missing recovery state: %#v", meta)
				}
				if len(patch) != 0 {
					t.Fatalf("failed export replaced prior patch: %q", patch)
				}
				if data, err := os.ReadFile(filepath.Join(root, "upper", "sentinel")); err != nil || string(data) != "unsaved" {
					t.Fatalf("upper lost: %q %v", data, err)
				}
			} else {
				if meta.Dirty || meta.LastDurableGeneration != meta.Generation {
					t.Fatalf("not durable: %#v", meta)
				}
				if !bytes.Equal(patch, g.content) {
					t.Fatalf("shutdown lost edits: %q", patch)
				}
				if _, err := os.Stat(root); !os.IsNotExist(err) {
					t.Fatalf("successful cleanup retained root: %v", err)
				}
			}
		})
	}
}

type safetyExportGit struct {
	exportGitOps
	content []byte
	fail    bool
	started chan struct{}
	proceed chan struct{}
}

func (g *safetyExportGit) DiffHeadBinaryTo(ctx context.Context, _, _ string, _ []string, out io.Writer) error {
	if g.started != nil {
		close(g.started)
		select {
		case <-g.proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if g.fail {
		return errors.New("injected export failure")
	}
	_, err := out.Write(g.content)
	return err
}

func TestDaemonSlowSessionDoesNotBlockOtherSessionOrList(t *testing.T) {
	repo, git := daemonTestRepo(t)
	state := newTestDaemonState(t, repo, git, func() model.GitOps { return &safetyExportGit{} })
	for _, key := range []string{"slow", "fast"} {
		if _, err := state.ensureSession(context.Background(), key, daemonTestConfig()); err != nil {
			t.Fatal(err)
		}
	}
	slow := state.sessions["slow"].g.(*safetyExportGit)
	slow.started = make(chan struct{})
	slow.proceed = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- state.flushSession(context.Background(), "slow", 0) }()
	<-slow.started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := state.flushSession(ctx, "fast", 0); err != nil {
		t.Fatalf("independent flush blocked: %v", err)
	}
	if infos, err := state.listSessions(ctx); err != nil || len(infos) != 2 {
		t.Fatalf("list blocked: %#v %v", infos, err)
	}
	close(slow.proceed)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Do not run the barrier again in cleanup.
	slow.started = nil
}

func TestDaemonClientCancellationAfterConnect(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(accepted)
		_, _ = io.Copy(io.Discard, conn)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (&daemonClient{socketPath: socket}).call(ctx, daemonRequest{Method: "list"})
		done <- err
	}()
	<-accepted
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled request succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("socket response read ignored cancellation")
	}
}

func TestDaemonServerCancelsWorkWhenCallerDisconnects(t *testing.T) {
	repo, git := daemonTestRepo(t)
	g := &safetyExportGit{started: make(chan struct{}), proceed: make(chan struct{})}
	state := newTestDaemonState(t, repo, git, func() model.GitOps { return g })
	if _, err := state.ensureSession(context.Background(), "key", daemonTestConfig()); err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	done := make(chan struct{})
	go func() { state.serveConn(server); close(done) }()
	if err := json.NewEncoder(client).Encode(daemonRequest{Method: "flush", SessionKey: "key"}); err != nil {
		t.Fatal(err)
	}
	<-g.started
	_ = client.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("server export ignored disconnected caller")
	}
	g.started = nil
	if !state.sessions["key"].dirty || state.sessions["key"].lastError == "" {
		t.Fatal("canceled export did not preserve dirty/error state")
	}
	g.proceed = nil // Cleanup must not block on the test barrier.
}

func TestDaemonLifetimeLockKeepsExistingSocket(t *testing.T) {
	repo, git := daemonTestRepo(t)
	dir, err := os.MkdirTemp("", "rpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "daemon.sock")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runDaemonServe(ctx, repo, git, socket) }()
	deadline := time.Now().Add(time.Second)
	for daemonPingRepo(ctx, socket, repo, git) != nil {
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := runDaemonServe(ctx, repo, git, socket); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("duplicate daemon accepted: %v", err)
	}
	if err := daemonPingRepo(ctx, socket, repo, git); err != nil {
		t.Fatalf("duplicate server damaged active socket: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
}

func TestDaemonLeaseGenerationAndBusyWrites(t *testing.T) {
	repo, git := daemonTestRepo(t)
	state := newTestDaemonState(t, repo, git, func() model.GitOps { return &daemonExportApplyGitOps{} })
	info, lease, err := state.acquireExec(context.Background(), "key", os.Getpid(), daemonTestConfig())
	if err != nil {
		t.Fatal(err)
	}
	if info.ViewGeneration == 0 {
		t.Fatal("missing generation")
	}
	if err := state.markDirtySession(context.Background(), "key"); err == nil {
		t.Fatal("legacy write entered active lease")
	}
	if err := state.releaseExecGeneration(context.Background(), "key", lease, info.ViewGeneration+1); err == nil {
		t.Fatal("stale generation accepted")
	}
	if !state.sessions["key"].busy {
		t.Fatal("bad release cleared busy state")
	}
	if err := state.releaseExecGeneration(context.Background(), "key", lease, info.ViewGeneration); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonAcquireRollsBackWhenMetadataWriteFails(t *testing.T) {
	repo, git := daemonTestRepo(t)
	state := newTestDaemonState(t, repo, git, func() model.GitOps { return &daemonExportApplyGitOps{} })
	if _, err := state.ensureSession(context.Background(), "key", daemonTestConfig()); err != nil {
		t.Fatal(err)
	}
	sess := state.sessions["key"]
	writes := 0
	state.deps.writeMeta = func(path string, meta daemonSessionMeta) error {
		writes++
		if writes == 2 {
			return errors.New("injected acquisition metadata failure")
		}
		return writeDaemonSessionMeta(path, meta)
	}
	if _, _, err := state.acquireExec(context.Background(), "key", os.Getpid(), daemonTestConfig()); err == nil {
		t.Fatal("expected metadata failure")
	}
	if writes != 2 {
		t.Fatalf("did not fail after assigning lease: writes=%d", writes)
	}
	if sess.busy || sess.busyLease != "" || sess.dirty || sess.generation != 0 {
		t.Fatalf("failed acquire left mutation state: %#v", daemonSessionSnapshot(sess))
	}
	state.deps.writeMeta = nil
}

func TestDaemonBrokenMetadataDoesNotPreventListAndPrune(t *testing.T) {
	repo, git := daemonTestRepo(t)
	state := newTestDaemonState(t, repo, git, func() model.GitOps { return &daemonExportApplyGitOps{} })
	if _, err := state.ensureSession(context.Background(), "healthy", daemonTestConfig()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemonSessionMetaPath(git, "broken"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	infos, err := state.listSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("healthy/error rows missing: %#v", infos)
	}
	found := false
	for _, info := range infos {
		if info.State == "recovery_required" && info.LastError != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("corrupt metadata error hidden")
	}
	pruned, err := state.pruneSessions(context.Background(), 0)
	if err != nil || len(pruned) != 1 || pruned[0] != "healthy" {
		t.Fatalf("healthy prune failed: %#v %v", pruned, err)
	}
}

func TestDaemonDurationAndBoundedNames(t *testing.T) {
	for _, duration := range []time.Duration{0, 500 * time.Millisecond, 1500 * time.Millisecond} {
		got := daemonIdleDuration(daemonRequest{IdleForNanos: int64(duration)})
		if got != duration {
			t.Fatalf("lost duration precision: %s -> %s", duration, got)
		}
	}
	if len(daemonSafeName(strings.Repeat("가/path ", 1000))) > 48 {
		t.Fatal("unbounded safe name")
	}
	if len(daemonSocketPath(strings.Repeat("/long/path", 1000))) > 107 {
		t.Fatal("socket path exceeds unix socket limit")
	}
}

func TestDaemonMetadataRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "meta.json")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := readDaemonSessionMeta(path); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted FIFO metadata")
		}
	case <-time.After(time.Second):
		t.Fatal("metadata read blocked on FIFO")
	}
}

func TestDaemonRejectsCrossSessionMetadata(t *testing.T) {
	repo, git := daemonTestRepo(t)
	state := newTestDaemonState(t, repo, git, func() model.GitOps { return &daemonExportApplyGitOps{} })
	if _, err := state.ensureSession(context.Background(), "other", daemonTestConfig()); err != nil {
		t.Fatal(err)
	}
	other := state.sessions["other"]
	meta := daemonSessionMetaSnapshot(other)
	if err := writeDaemonSessionMeta(daemonSessionMetaPath(git, "forged"), meta); err != nil {
		t.Fatal(err)
	}
	if _, err := state.ensureSession(context.Background(), "forged", daemonTestConfig()); err == nil || !strings.Contains(err.Error(), "key mismatch") {
		t.Fatalf("accepted cross-key metadata: %v", err)
	}
	if _, err := state.recoverSession(context.Background(), "forged", false); err == nil {
		t.Fatal("cross-key recovery accepted")
	}
	if err := validateDaemonOwner(other.sess.Root, "forged", other.baseOID); err == nil {
		t.Fatal("accepted another session owner")
	}
	if _, err := os.Stat(other.sess.Root); err != nil {
		t.Fatalf("other session removed: %v", err)
	}
}

func TestDaemonShutdownDrainsSessionCreation(t *testing.T) {
	repo, git := daemonTestRepo(t)
	entered := make(chan struct{})
	proceed := make(chan struct{})
	state := newTestDaemonState(t, repo, git, func() model.GitOps { close(entered); <-proceed; return &daemonExportApplyGitOps{} })
	created := make(chan *daemonSessionInfo, 1)
	go func() {
		info, _ := state.ensureSession(context.Background(), "creating", daemonTestConfig())
		created <- info
	}()
	<-entered
	stopped := make(chan struct{})
	go func() { state.closeAll(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("shutdown ignored in-flight creation")
	case <-time.After(20 * time.Millisecond):
	}
	close(proceed)
	info := <-created
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not drain creation")
	}
	if info == nil {
		t.Fatal("fixture creation failed")
	}
	if _, err := os.Stat(info.ViewPath); !os.IsNotExist(err) {
		t.Fatalf("in-flight session leaked: %v", err)
	}
}

// Real Git performs every diff and apply; the filesystem fake only substitutes
// copying an immutable lower tree for mounting OverlayFS in an unprivileged test.
type copyingDaemonNS struct {
	recordingNSOps
	base map[string]string
}

func (n *copyingDaemonNS) BindMount(src, dst string) error {
	if filepath.Base(dst) == "gitdir" {
		return os.Symlink(src, dst)
	}
	if n.base == nil {
		n.base = map[string]string{}
	}
	n.base[dst] = src
	return nil
}
func (n *copyingDaemonNS) MountOverlay(dst string, opts model.OverlayOpts) error {
	if err := copyDaemonTree(n.base[opts.LowerDir], dst); err != nil {
		return err
	}
	return copyDaemonTree(opts.UpperDir, dst)
}

func TestDaemonRealGitRetainsDirtyUpperAfterCrash(t *testing.T) {
	repo := testutil.InitRepo(t)
	git := filepath.Join(repo, ".git")
	create := func() *daemonState {
		state := newTestDaemonState(t, repo, git, func() model.GitOps { return &gitx.Git{RepoRoot: repo, GitDir: git} })
		state.deps.newMount = func() model.NSOps { return &copyingDaemonNS{} }
		return state
	}
	cfg := daemonTestConfig()
	cfg.BaseMode = model.BaseWorktree
	cfg.AllowDirty = false
	cfg.IgnoredMax = 0
	first := create()
	_, _, err := first.acquireExec(context.Background(), "crash", os.Getpid(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	sess := first.sessions["crash"]
	root := sess.sess.Root
	want := []byte("unsaved before crash\x00\xff\n")
	for _, dir := range []string{sess.sess.Upper, sess.info.ViewPath} {
		if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Crash simulation keeps the physical upper/worktree and durable metadata.
	// No ApplyPatch mock participates in recovering their actual contents.
	sess.busyOwnerPID = 1 << 30
	sess.busyOwnerStart = ""
	if err := first.persistSessionLocked(sess); err != nil {
		t.Fatal(err)
	}
	if err := sess.lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	_ = sess.store.Close()
	first.removeSession("crash")
	second := create()
	info, err := second.recoverSession(context.Background(), "crash", false)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Dirty || info.ViewPath != filepath.Join(root, "view") {
		t.Fatalf("dirty recovery discarded upper: %#v", info)
	}
	got, err := os.ReadFile(filepath.Join(info.ViewPath, "tracked.txt"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("upper bytes lost: %q %v", got, err)
	}
	if err := second.flushSession(context.Background(), "crash", 0); err != nil {
		t.Fatal(err)
	}
	second.closeAll()
	third := create()
	info, err = third.ensureSession(context.Background(), "crash", cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(info.ViewPath, "tracked.txt"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("saved dirty recovery did not round-trip: %q %v", got, err)
	}
}
func copyDaemonTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

func TestDaemonRealGitRestoresBytesAndBaseAfterRefMoves(t *testing.T) {
	repo := testutil.InitRepo(t)
	git := filepath.Join(repo, ".git")
	create := func() *daemonState {
		state := newTestDaemonState(t, repo, git, func() model.GitOps { return &gitx.Git{RepoRoot: repo, GitDir: git} })
		state.deps.newMount = func() model.NSOps { return &copyingDaemonNS{} }
		return state
	}
	cfg := daemonTestConfig()
	cfg.BaseMode = model.BaseWorktree
	cfg.AllowDirty = false
	cfg.IgnoredMax = 0
	first := create()
	info, lease, err := first.acquireExec(context.Background(), "bytes", os.Getpid(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(repo, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	contents := []byte{0, 1, 2, 255, 13, 10, 3}
	if err := os.WriteFile(filepath.Join(info.ViewPath, "new.bin"), contents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(info.ViewPath, "tracked.txt"), []byte("++ /body\r\nno newline"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tracked.txt", filepath.Join(info.ViewPath, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := first.releaseExec(context.Background(), "bytes", lease); err != nil {
		t.Fatal(err)
	}
	base := first.sessions["bytes"].baseOID
	// Move the original HEAD after saving the session. Its base must stay A.
	testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "commit B\n")
	testutil.RunCmd(t, repo, "git", "add", "tracked.txt")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "move original HEAD")
	first.closeAll()
	second := create()
	restored, err := second.ensureSession(context.Background(), "bytes", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if restored.BaseOID != base || base == "" {
		t.Fatalf("base changed: %q -> %q", base, restored.BaseOID)
	}
	if restored.ViewGeneration <= info.ViewGeneration {
		t.Fatal("view generation did not change")
	}
	got, err := os.ReadFile(filepath.Join(restored.ViewPath, "new.bin"))
	if err != nil || !bytes.Equal(got, contents) {
		t.Fatalf("bytes lost: %v %v", got, err)
	}
	mode, err := os.Stat(filepath.Join(restored.ViewPath, "new.bin"))
	if err != nil || mode.Mode().Perm()&0o111 == 0 {
		t.Fatalf("executable mode lost: %v", err)
	}
	link, err := os.Readlink(filepath.Join(restored.ViewPath, "alias"))
	if err != nil || link != "tracked.txt" {
		t.Fatalf("symlink lost: %q %v", link, err)
	}
	got, err = os.ReadFile(filepath.Join(restored.ViewPath, "tracked.txt"))
	if err != nil || string(got) != "++ /body\r\nno newline" {
		t.Fatalf("text bytes changed: %q %v", got, err)
	}
	if bytes.Equal(original, []byte("commit B\n")) {
		t.Fatal("invalid fixture")
	}
	// JSON exposes the durable state rather than silently declaring dirty recovery clean.
	encoded, err := json.Marshal(restored)
	if err != nil || !bytes.Contains(encoded, []byte("last_durable_generation")) {
		t.Fatalf("missing durability metadata: %s %v", encoded, err)
	}
}

func TestDaemonPatchBaseSurvivesEndRefRewriteAndGC(t *testing.T) {
	repo := testutil.InitRepo(t)
	git := filepath.Join(repo, ".git")
	state := newTestDaemonState(t, repo, git, func() model.GitOps { return &gitx.Git{RepoRoot: repo, GitDir: git} })
	state.deps.newMount = func() model.NSOps { return &copyingDaemonNS{} }
	cfg := daemonTestConfig()
	cfg.BaseMode = model.BaseWorktree
	cfg.AllowDirty = false
	cfg.IgnoredMax = 0
	info, lease, err := state.acquireExec(context.Background(), "retained", os.Getpid(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := "patch after ending\n"
	if err := os.WriteFile(filepath.Join(info.ViewPath, "tracked.txt"), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := state.releaseExec(context.Background(), "retained", lease); err != nil {
		t.Fatal(err)
	}
	base := state.sessions["retained"].baseOID
	if ended, err := state.endSession(context.Background(), "retained"); err != nil || !ended {
		t.Fatalf("end: %v %v", ended, err)
	}
	testutil.RunCmd(t, repo, "git", "checkout", "--orphan", "rewritten")
	testutil.RunCmd(t, repo, "git", "rm", "-rf", ".")
	testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "unrelated base\n")
	testutil.RunCmd(t, repo, "git", "add", "tracked.txt")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "rewrite original history")
	testutil.RunCmd(t, repo, "git", "reflog", "expire", "--expire=now", "--all")
	testutil.RunCmd(t, repo, "git", "gc", "--prune=now")
	restored, err := state.ensureSession(context.Background(), "retained", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if restored.BaseOID != base {
		t.Fatalf("patch base changed after end: %s -> %s", base, restored.BaseOID)
	}
	got, err := os.ReadFile(filepath.Join(restored.ViewPath, "tracked.txt"))
	if err != nil || string(got) != want {
		t.Fatalf("retained patch failed: %q %v", got, err)
	}
}
