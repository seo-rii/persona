//go:build linux
// +build linux

package ns

import (
	"os"
	"testing"
)

func TestMountOverlayForcedFail(t *testing.T) {
	os.Setenv("PERSONA_FORCE_MOUNT_FAIL", "1")
	t.Cleanup(func() { _ = os.Unsetenv("PERSONA_FORCE_MOUNT_FAIL") })
	if err := MountOverlay("/tmp/nonexistent", OverlayOpts{LowerDir: "/tmp", UpperDir: "/tmp", WorkDir: "/tmp"}); err == nil {
		t.Fatalf("expected forced mount failure")
	}
}
