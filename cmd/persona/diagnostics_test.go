package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
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

func TestResolveSetcapPathUsesOverrideBeforeCandidates(t *testing.T) {
	override := filepath.Join(t.TempDir(), "custom-setcap")
	if err := os.WriteFile(override, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write override: %v", err)
	}
	fallback := filepath.Join(t.TempDir(), "fallback-setcap")
	if err := os.WriteFile(fallback, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fallback: %v", err)
	}
	t.Setenv("PERSONA_SETCAP_BIN", override)

	got, err := resolveSetcapPath([]string{fallback})
	if err != nil {
		t.Fatalf("resolveSetcapPath error: %v", err)
	}
	if got != override {
		t.Fatalf("expected override %q, got %q", override, got)
	}
}

func TestResolveSetcapPathRejectsMissingOverrideWithoutFallback(t *testing.T) {
	fallback := filepath.Join(t.TempDir(), "fallback-setcap")
	if err := os.WriteFile(fallback, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fallback: %v", err)
	}
	t.Setenv("PERSONA_SETCAP_BIN", filepath.Join(t.TempDir(), "missing-setcap"))

	_, err := resolveSetcapPath([]string{fallback})
	if err == nil {
		t.Fatal("expected override resolution error")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("expected missing override error, got %v", err)
	}
}

func TestDoctorCommandPrintsNoSysAdminHint(t *testing.T) {
	origCollectDiagFn := collectDiagFn
	t.Cleanup(func() {
		collectDiagFn = origCollectDiagFn
	})

	collectDiagFn = func() diagInfo {
		return diagInfo{
			ExePath:       "/persona",
			EUID:          1000,
			FileCapStatus: "absent",
			SetcapPath:    "/usr/bin/setcap",
		}
	}

	var out bytes.Buffer
	cmd := newDoctorCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("doctor RunE error: %v", err)
	}
	if !strings.Contains(out.String(), "hint: CAP_SYS_ADMIN is required") {
		t.Fatalf("expected CAP_SYS_ADMIN hint, got %q", out.String())
	}
	if !strings.Contains(out.String(), "hint: run `sudo persona activate`") {
		t.Fatalf("expected least privilege hint, got %q", out.String())
	}
}

func TestDoctorCommandPrintsNosuidHint(t *testing.T) {
	origCollectDiagFn := collectDiagFn
	t.Cleanup(func() {
		collectDiagFn = origCollectDiagFn
	})

	collectDiagFn = func() diagInfo {
		return diagInfo{
			ExePath:      "/persona",
			EUID:         0,
			HasSysAdmin:  true,
			MountOptions: "rw,nosuid,nodev",
			SetcapPath:   "/usr/bin/setcap",
		}
	}

	var out bytes.Buffer
	cmd := newDoctorCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("doctor RunE error: %v", err)
	}
	if !strings.Contains(out.String(), "hint: mount has nosuid; file capabilities are ignored.") {
		t.Fatalf("expected nosuid hint, got %q", out.String())
	}
}

func TestActivateCommandSetcapMissing(t *testing.T) {
	origRequireSetcapCapabilityFn := requireSetcapCapabilityFn
	origResolveSetcapPathFn := resolveSetcapPathFn
	t.Cleanup(func() {
		requireSetcapCapabilityFn = origRequireSetcapCapabilityFn
		resolveSetcapPathFn = origResolveSetcapPathFn
	})

	requireSetcapCapabilityFn = func() error { return nil }
	resolveSetcapPathFn = func([]string) (string, error) {
		return "", errors.New("missing")
	}

	binary := filepath.Join(t.TempDir(), "persona-bin")
	if err := os.WriteFile(binary, []byte("bin"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	cmd := newActivateCmd()
	cmd.SetArgs([]string{"--binary", binary})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "install libcap2-bin") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestActivateCommandUsesDACOverrideSpec(t *testing.T) {
	origRequireSetcapCapabilityFn := requireSetcapCapabilityFn
	origResolveSetcapPathFn := resolveSetcapPathFn
	t.Cleanup(func() {
		requireSetcapCapabilityFn = origRequireSetcapCapabilityFn
		resolveSetcapPathFn = origResolveSetcapPathFn
	})

	requireSetcapCapabilityFn = func() error { return nil }

	tmp := t.TempDir()
	argsPath := filepath.Join(tmp, "args.txt")
	scriptPath := filepath.Join(tmp, "setcap")
	script := strings.Join([]string{
		"#!/bin/sh",
		"printf '%s\\n' \"$@\" > \"" + argsPath + "\"",
		"",
	}, "\n")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write setcap script: %v", err)
	}
	resolveSetcapPathFn = func([]string) (string, error) {
		return scriptPath, nil
	}

	binary := filepath.Join(tmp, "persona-bin")
	if err := os.WriteFile(binary, []byte("bin"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	var out bytes.Buffer
	cmd := newActivateCmd()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--binary", binary, "--allow-dac-override"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("activate Execute error: %v", err)
	}
	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	if len(lines) != 2 {
		t.Fatalf("unexpected setcap args: %q", string(argsData))
	}
	if lines[0] != "cap_sys_admin,cap_dac_override+ep" {
		t.Fatalf("unexpected capability spec: %q", lines[0])
	}
	if lines[1] != binary {
		t.Fatalf("unexpected binary path: %q", lines[1])
	}
}

func TestActivateCommandUsesSetcapOverride(t *testing.T) {
	origRequireSetcapCapabilityFn := requireSetcapCapabilityFn
	t.Cleanup(func() {
		requireSetcapCapabilityFn = origRequireSetcapCapabilityFn
	})

	requireSetcapCapabilityFn = func() error { return nil }

	tmp := t.TempDir()
	argsPath := filepath.Join(tmp, "args.txt")
	scriptPath := filepath.Join(tmp, "custom-setcap")
	script := strings.Join([]string{
		"#!/bin/sh",
		"printf '%s\\n' \"$@\" > \"" + argsPath + "\"",
		"",
	}, "\n")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write setcap script: %v", err)
	}
	t.Setenv("PERSONA_SETCAP_BIN", scriptPath)

	binary := filepath.Join(tmp, "persona-bin")
	if err := os.WriteFile(binary, []byte("bin"), 0o755); err != nil {
		t.Fatalf("write binary: %v", err)
	}

	cmd := newActivateCmd()
	cmd.SetArgs([]string{"--binary", binary})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("activate Execute error: %v", err)
	}
	argsData, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(argsData)), "\n")
	if len(lines) != 2 {
		t.Fatalf("unexpected setcap args: %q", string(argsData))
	}
	if lines[0] != "cap_sys_admin+ep" || lines[1] != binary {
		t.Fatalf("unexpected setcap override invocation: %q", string(argsData))
	}
}

func TestRequireSetcapCapability(t *testing.T) {
	origGeteuidFn := geteuidFn
	origReadCapEffFn := readCapEffFn
	t.Cleanup(func() {
		geteuidFn = origGeteuidFn
		readCapEffFn = origReadCapEffFn
	})

	cases := []struct {
		name       string
		euid       int
		capEff     uint64
		readErr    error
		wantErr    bool
	}{
		{name: "root", euid: 0},
		{name: "non-root with cap_setfcap", euid: 1000, capEff: 1 << capSetFcap},
		{name: "non-root without cap_setfcap", euid: 1000, capEff: 0, wantErr: true},
		{name: "non-root cap read error", euid: 1000, readErr: errors.New("read failed"), wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			geteuidFn = func() int { return tc.euid }
			readCapEffFn = func() (uint64, error) {
				return tc.capEff, tc.readErr
			}
			err := requireSetcapCapability()
			if tc.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestFileCapabilityStatusMapsENODATAEOPNOTSUPPAndEPERM(t *testing.T) {
	origGetxattrFn := getxattrFn
	t.Cleanup(func() {
		getxattrFn = origGetxattrFn
	})

	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "no data", err: unix.ENODATA, want: "absent"},
		{name: "unsupported", err: unix.EOPNOTSUPP, want: "unsupported"},
		{name: "permission denied", err: unix.EPERM, want: "permission denied"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getxattrFn = func(path string, attr string, dest []byte) (int, error) {
				return 0, tc.err
			}
			got, err := fileCapabilityStatus("/persona")
			if err != nil {
				t.Fatalf("fileCapabilityStatus error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("expected %q got %q", tc.want, got)
			}
		})
	}
}

func TestReportPermissionHintOnlyOnPermissionErrors(t *testing.T) {
	origCollectDiagFn := collectDiagFn
	origStderrWriter := stderrWriter
	t.Cleanup(func() {
		collectDiagFn = origCollectDiagFn
		stderrWriter = origStderrWriter
	})

	collectDiagFn = func() diagInfo {
		return diagInfo{
			ExeResolved:   "/persona",
			EUID:          1000,
			HasSysAdmin:   false,
			MountOptions:  "rw,nosuid",
			FileCapStatus: "absent",
		}
	}

	var out bytes.Buffer
	stderrWriter = &out
	reportPermissionHint("mount overlay", fmt.Errorf("wrapped: %w", syscall.EPERM))
	if !strings.Contains(out.String(), "requires CAP_SYS_ADMIN") {
		t.Fatalf("expected permission hint, got %q", out.String())
	}

	out.Reset()
	reportPermissionHint("mount overlay", errors.New("other error"))
	if out.Len() != 0 {
		t.Fatalf("expected no hint for non-permission error, got %q", out.String())
	}
}
