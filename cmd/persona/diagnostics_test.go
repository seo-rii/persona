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
