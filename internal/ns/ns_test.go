//go:build linux
// +build linux

package ns

import "testing"

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
