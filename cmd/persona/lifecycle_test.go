package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"persona/internal/model"
)

func TestCleanupStackRetriesOnlyFailures(t *testing.T) {
	var cleanup cleanupStack
	var calls []int
	fail := true
	for _, id := range []int{1, 2, 3} {
		cleanup.Push(func() error {
			calls = append(calls, id)
			if id == 2 && fail {
				return errors.New("retry this resource")
			}
			return nil
		})
	}
	if err := cleanup.Run(); err == nil {
		t.Fatal("injected cleanup error was lost")
	}
	fail = false
	if err := cleanup.Run(); err != nil {
		t.Fatal(err)
	}
	if err := cleanup.Run(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []int{3, 2, 1, 2}) || len(cleanup.fns) != 0 {
		t.Fatalf("successful cleanup was repeated: %v", calls)
	}
}

func TestRunClosesPatchStoreOnCanceledLock(t *testing.T) {
	repo, gitDir := daemonTestRepo(t)
	patchDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 8 {
		err, _ := runWithDeps(ctx, model.Options{PatchPath: filepath.Join(patchDir, "state.patch")}, runDeps{
			getwd:      func() (string, error) { return repo, nil },
			detectRepo: func(context.Context, string) (string, string, error) { return repo, gitDir, nil },
			newGit:     func(string, string, bool) model.GitOps { return &daemonExportApplyGitOps{} },
			newMount:   func() model.NSOps { return &recordingNSOps{} },
			now:        time.Now,
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled patch lock: %v", err)
		}
	}
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, fd := range fds {
		path, _ := os.Readlink(filepath.Join("/proc/self/fd", fd.Name()))
		if path == patchDir {
			t.Fatalf("patch directory descriptor %s leaked after lock failure", fd.Name())
		}
	}
}

type namespaceThreadNS struct {
	recordingNSOps
	thread         int
	unmountThreads []int
}

func (n *namespaceThreadNS) UnshareMountNS() error { n.thread = unix.Gettid(); return nil }
func (n *namespaceThreadNS) MountOverlay(string, model.OverlayOpts) error {
	return errors.New("injected overlay failure")
}
func (n *namespaceThreadNS) Umount(string) error {
	n.unmountThreads = append(n.unmountThreads, unix.Gettid())
	return nil
}

func TestRunRetiresNamespaceThreadAfterCleanup(t *testing.T) {
	repo, gitDir := daemonTestRepo(t)
	mount := &namespaceThreadNS{}
	// Keep the caller on its own original thread while the namespace worker runs.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	caller := unix.Gettid()
	err, _ := runWithDeps(context.Background(), model.Options{
		PatchPath: filepath.Join(t.TempDir(), "state.patch"), BaseMode: model.BaseRepo,
		AllowDirty: true, KeepSession: model.KeepNever,
	}, runDeps{
		getwd:      func() (string, error) { return repo, nil },
		detectRepo: func(context.Context, string) (string, string, error) { return repo, gitDir, nil },
		newGit:     func(string, string, bool) model.GitOps { return &daemonExportApplyGitOps{} },
		newMount:   func() model.NSOps { return mount }, now: time.Now,
	})
	if err == nil || mount.thread == 0 || mount.thread == caller {
		t.Fatalf("namespace worker did not run independently: %v", err)
	}
	if len(mount.unmountThreads) == 0 {
		t.Fatal("fixture did not exercise mount cleanup")
	}
	for _, tid := range mount.unmountThreads {
		if tid != mount.thread {
			t.Fatalf("cleanup moved threads: %d != %d", tid, mount.thread)
		}
	}
	// The Go runtime permanently parks Linux's initial thread on retirement:
	// exiting it would make the entire process look like a zombie to wait(2).
	if mount.thread == os.Getpid() {
		return
	}
	deadline := time.Now().Add(time.Second)
	for {
		_, statErr := os.Stat(fmt.Sprintf("/proc/self/task/%d", mount.thread))
		if errors.Is(statErr, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("namespace worker thread remains after returning")
		}
		time.Sleep(time.Millisecond)
	}
}
