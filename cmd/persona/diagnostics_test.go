package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHasCap(t *testing.T) {
	if !hasCap(1<<capSysAdmin, capSysAdmin) {
		t.Fatalf("expected CAP_SYS_ADMIN bit to be detected")
	}
	if hasCap(0, capSysAdmin) {
		t.Fatalf("expected missing capability bit")
	}
	if !hasCap(1<<63, 63) {
		t.Fatalf("expected bit 63 to be supported")
	}
	if hasCap(1<<63, 64) {
		t.Fatalf("cap IDs >=64 must be false")
	}
}

func TestResolveBinaryPath(t *testing.T) {
	t.Run("existing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "persona-bin")
		if err := os.WriteFile(path, []byte("bin"), 0o755); err != nil {
			t.Fatalf("write file: %v", err)
		}
		got, err := resolveBinaryPath(path)
		if err != nil {
			t.Fatalf("resolveBinaryPath error: %v", err)
		}
		if got != path {
			t.Fatalf("expected %q got %q", path, got)
		}
	})

	t.Run("symlink resolves target", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "persona-target")
		if err := os.WriteFile(target, []byte("bin"), 0o755); err != nil {
			t.Fatalf("write target: %v", err)
		}
		link := filepath.Join(dir, "persona-link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		got, err := resolveBinaryPath(link)
		if err != nil {
			t.Fatalf("resolveBinaryPath error: %v", err)
		}
		if got != target {
			t.Fatalf("expected resolved target %q got %q", target, got)
		}
	})

	t.Run("missing file returns error", func(t *testing.T) {
		_, err := resolveBinaryPath(filepath.Join(t.TempDir(), "missing-bin"))
		if err == nil {
			t.Fatalf("expected error for missing binary")
		}
	})
}

func TestActivateCapabilitySpec(t *testing.T) {
	if got := activateCapabilitySpec(false); got != "cap_sys_admin+ep" {
		t.Fatalf("expected least-privilege default spec, got %q", got)
	}
	if got := activateCapabilitySpec(true); got != "cap_sys_admin,cap_dac_override+ep" {
		t.Fatalf("expected opt-in DAC override spec, got %q", got)
	}
}

func TestLeastPrivilegeCapabilityHint(t *testing.T) {
	hint := leastPrivilegeCapabilityHint()
	if !strings.Contains(hint, "cap_sys_admin+ep") {
		t.Fatalf("expected default hint to use CAP_SYS_ADMIN only: %q", hint)
	}
	if strings.Contains(hint, "cap_sys_admin,cap_dac_override+ep") {
		t.Fatalf("expected default hint not to recommend DAC override by default: %q", hint)
	}
	if !strings.Contains(hint, "--allow-dac-override") {
		t.Fatalf("expected hint to document DAC override as opt-in: %q", hint)
	}
}

func TestFindmntInfoParsesQuotedValuesWithSpaces(t *testing.T) {
	tmp := t.TempDir()
	findmnt := filepath.Join(tmp, "findmnt")
	script := strings.Join([]string{
		"#!/bin/sh",
		"echo 'OPTIONS=\"rw,nosuid,nodev,relatime\" FSTYPE=\"ext4\" SOURCE=\"/dev/mapper/root vg\" TARGET=\"/mnt/my mount\"'",
		"",
	}, "\n")
	if err := os.WriteFile(findmnt, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake findmnt: %v", err)
	}

	t.Setenv("PATH", tmp+":"+os.Getenv("PATH"))
	info, err := findmntInfo("/irrelevant/path")
	if err != nil {
		t.Fatalf("findmntInfo error: %v", err)
	}
	if got, want := info["SOURCE"], "/dev/mapper/root vg"; got != want {
		t.Fatalf("SOURCE parse mismatch: got %q want %q", got, want)
	}
	if got, want := info["TARGET"], "/mnt/my mount"; got != want {
		t.Fatalf("TARGET parse mismatch: got %q want %q", got, want)
	}
}

func TestFindSetcapPathUsesAbsoluteCandidatesOnly(t *testing.T) {
	tmp := t.TempDir()
	rogue := filepath.Join(tmp, "setcap")
	if err := os.WriteFile(rogue, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write rogue setcap: %v", err)
	}
	trusted := filepath.Join(tmp, "trusted-setcap")
	if err := os.WriteFile(trusted, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write trusted setcap: %v", err)
	}
	t.Setenv("PATH", tmp+":"+os.Getenv("PATH"))

	got, err := findSetcapPath([]string{"setcap", trusted})
	if err != nil {
		t.Fatalf("findSetcapPath error: %v", err)
	}
	if got != trusted {
		t.Fatalf("expected trusted absolute path %q, got %q", trusted, got)
	}
}

func TestFindSetcapPathErrorsWhenNoTrustedCandidateExists(t *testing.T) {
	t.Setenv("PATH", t.TempDir()+":"+os.Getenv("PATH"))

	_, err := findSetcapPath([]string{"setcap"})
	if err == nil {
		t.Fatal("expected error when only PATH-based candidate exists")
	}
	if !strings.Contains(err.Error(), "setcap not found") {
		t.Fatalf("unexpected error: %v", err)
	}
}
