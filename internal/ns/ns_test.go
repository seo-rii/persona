//go:build linux
// +build linux

package ns

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMountOverlayEscapesOptionPaths(t *testing.T) {
	origMountFn := mountFn
	t.Cleanup(func() {
		mountFn = origMountFn
	})

	var gotData string
	mountFn = func(source string, target string, fstype string, flags uintptr, data string) error {
		gotData = data
		return nil
	}

	err := MountOverlay("/target", OverlayOpts{
		LowerDir: "/tmp/lower:a,b\\c",
		UpperDir: "/tmp/up,per",
		WorkDir:  "/tmp/work:dir",
	})
	if err != nil {
		t.Fatalf("MountOverlay error: %v", err)
	}

	want := "lowerdir=/tmp/lower\\:a\\,b\\\\c,upperdir=/tmp/up\\,per,workdir=/tmp/work\\:dir"
	if gotData != want {
		t.Fatalf("unexpected overlay mount data: got %q want %q", gotData, want)
	}
}

func TestBindMountCreatesPlaceholderForFile(t *testing.T) {
	origMountFn := mountFn
	t.Cleanup(func() {
		mountFn = origMountFn
	})

	src := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(src, []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "nested", "target.txt")

	var gotSource string
	var gotTarget string
	var gotFlags uintptr
	mountFn = func(source string, target string, fstype string, flags uintptr, data string) error {
		gotSource = source
		gotTarget = target
		gotFlags = flags
		return nil
	}

	if err := BindMount(src, dst); err != nil {
		t.Fatalf("BindMount error: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if info.IsDir() {
		t.Fatal("expected file placeholder at target")
	}
	if gotSource != src || gotTarget != dst {
		t.Fatalf("unexpected mount args: %q -> %q", gotSource, gotTarget)
	}
	if gotFlags != unix.MS_BIND {
		t.Fatalf("unexpected mount flags: got %d want %d", gotFlags, unix.MS_BIND)
	}
}

func TestBindMountCreatesTargetDirForDirectory(t *testing.T) {
	origMountFn := mountFn
	t.Cleanup(func() {
		mountFn = origMountFn
	})

	src := filepath.Join(t.TempDir(), "source-dir")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "nested", "target-dir")

	var gotSource string
	var gotTarget string
	mountFn = func(source string, target string, fstype string, flags uintptr, data string) error {
		gotSource = source
		gotTarget = target
		return nil
	}

	if err := BindMount(src, dst); err != nil {
		t.Fatalf("BindMount error: %v", err)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("expected directory placeholder at target")
	}
	if gotSource != src || gotTarget != dst {
		t.Fatalf("unexpected mount args: %q -> %q", gotSource, gotTarget)
	}
}

func TestRemountROUsesBindRemountReadonlyFlags(t *testing.T) {
	origMountFn := mountFn
	t.Cleanup(func() {
		mountFn = origMountFn
	})

	var gotTarget string
	var gotFlags uintptr
	mountFn = func(source string, target string, fstype string, flags uintptr, data string) error {
		gotTarget = target
		gotFlags = flags
		return nil
	}

	if err := RemountRO("/target"); err != nil {
		t.Fatalf("RemountRO error: %v", err)
	}
	if gotTarget != "/target" {
		t.Fatalf("unexpected target: %q", gotTarget)
	}
	want := uintptr(unix.MS_BIND | unix.MS_REMOUNT | unix.MS_RDONLY)
	if gotFlags != want {
		t.Fatalf("unexpected remount flags: got %d want %d", gotFlags, want)
	}
}

func TestUmountIgnoresMissingOrNonMountTargets(t *testing.T) {
	origUnmountFn := unmountFn
	t.Cleanup(func() {
		unmountFn = origUnmountFn
	})

	cases := []error{unix.EINVAL, unix.ENOENT}
	for _, tc := range cases {
		unmountFn = func(target string, flags int) error {
			if target != "/target" || flags != 0 {
				t.Fatalf("unexpected unmount call: target=%q flags=%d", target, flags)
			}
			return tc
		}
		if err := Umount("/target"); err != nil {
			t.Fatalf("expected nil for %v, got %v", tc, err)
		}
	}
}

func TestUmountFallsBackToDetachOnBusy(t *testing.T) {
	origUnmountFn := unmountFn
	t.Cleanup(func() {
		unmountFn = origUnmountFn
	})

	var calls []int
	unmountFn = func(target string, flags int) error {
		if target != "/target" {
			t.Fatalf("unexpected unmount target: %q", target)
		}
		calls = append(calls, flags)
		if len(calls) == 1 {
			return unix.EBUSY
		}
		return nil
	}

	if err := Umount("/target"); err != nil {
		t.Fatalf("Umount error: %v", err)
	}
	if len(calls) != 2 || calls[0] != 0 || calls[1] != unix.MNT_DETACH {
		t.Fatalf("unexpected unmount calls: %v", calls)
	}
}

func TestUmountReturnsDetachError(t *testing.T) {
	origUnmountFn := unmountFn
	t.Cleanup(func() {
		unmountFn = origUnmountFn
	})

	unmountFn = func(target string, flags int) error {
		if flags == 0 {
			return unix.EBUSY
		}
		return errors.New("detach failed")
	}

	err := Umount("/target")
	if err == nil {
		t.Fatal("expected detach error")
	}
	if !strings.Contains(err.Error(), "detach failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMaskPathChoosesEmptyFileOrDir(t *testing.T) {
	origMountFn := mountFn
	t.Cleanup(func() {
		mountFn = origMountFn
	})

	emptyRoot := t.TempDir()
	emptyFile := filepath.Join(emptyRoot, "empty-file")
	if err := os.WriteFile(emptyFile, nil, 0o644); err != nil {
		t.Fatalf("write empty file: %v", err)
	}
	emptyDir := filepath.Join(emptyRoot, "empty-dir")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatalf("mkdir empty dir: %v", err)
	}

	var gotSources []string
	mountFn = func(source string, target string, fstype string, flags uintptr, data string) error {
		gotSources = append(gotSources, source)
		return nil
	}

	if err := MaskPath(filepath.Join(t.TempDir(), "masked.txt"), MaskFile, emptyFile, emptyDir); err != nil {
		t.Fatalf("MaskPath file error: %v", err)
	}
	if err := MaskPath(filepath.Join(t.TempDir(), "masked-dir"), MaskDir, emptyFile, emptyDir); err != nil {
		t.Fatalf("MaskPath dir error: %v", err)
	}
	if len(gotSources) != 2 || gotSources[0] != emptyFile || gotSources[1] != emptyDir {
		t.Fatalf("unexpected mask sources: %v", gotSources)
	}
}

func TestMaskPathRejectsUnknownKind(t *testing.T) {
	err := MaskPath("/target", MaskKind(99), "/empty-file", "/empty-dir")
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "unknown mask kind" {
		t.Fatalf("unexpected error: %v", err)
	}
}
