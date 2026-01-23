package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/spf13/cobra"
)

const (
	capDACOverride = 1
	capSysAdmin    = 21
	capSetFcap     = 31
)

type diagInfo struct {
	ExePath        string
	ExeResolved    string
	EUID           int
	CapEff         uint64
	HasSysAdmin    bool
	HasSetFcap     bool
	HasDACOverride bool
	FileCapStatus  string
	MountOptions   string
	MountFstype    string
	MountSource    string
	MountTarget    string
	SetcapPath     string
}

func addDiagnosticCommands(cmd *cobra.Command) {
	cmd.AddCommand(newDoctorCmd())
	cmd.AddCommand(newActivateCmd())
}

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check capabilities, mounts, and prerequisites",
		RunE: func(cmd *cobra.Command, args []string) error {
			info := collectDiag()
			w := cmd.OutOrStdout()
			fmt.Fprintln(w, "persona doctor")
			if info.ExePath != "" {
				fmt.Fprintf(w, "exe=%s\n", info.ExePath)
			}
			if info.ExeResolved != "" && info.ExeResolved != info.ExePath {
				fmt.Fprintf(w, "exe_resolved=%s\n", info.ExeResolved)
			}
			fmt.Fprintf(w, "euid=%d\n", info.EUID)
			fmt.Fprintf(w, "cap_eff=0x%016x\n", info.CapEff)
			fmt.Fprintf(w, "cap_sys_admin=%t cap_setfcap=%t cap_dac_override=%t\n", info.HasSysAdmin, info.HasSetFcap, info.HasDACOverride)
			if info.FileCapStatus != "" {
				fmt.Fprintf(w, "file_capability=%s\n", info.FileCapStatus)
			}
			if info.MountOptions != "" {
				fmt.Fprintf(w, "mount_options=%s\n", info.MountOptions)
			}
			if info.MountFstype != "" {
				fmt.Fprintf(w, "mount_fstype=%s\n", info.MountFstype)
			}
			if info.MountSource != "" {
				fmt.Fprintf(w, "mount_source=%s\n", info.MountSource)
			}
			if info.MountTarget != "" {
				fmt.Fprintf(w, "mount_target=%s\n", info.MountTarget)
			}
			if info.SetcapPath != "" {
				fmt.Fprintf(w, "setcap=%s\n", info.SetcapPath)
			} else {
				fmt.Fprintln(w, "setcap=missing")
			}
			if !info.HasSysAdmin {
				fmt.Fprintln(w, "hint: CAP_SYS_ADMIN is required for unshare and OverlayFS mount.")
				fmt.Fprintln(w, "hint: run `sudo persona activate` or `sudo setcap cap_sys_admin,cap_dac_override+ep /path/to/persona`.")
			}
			if strings.Contains(info.MountOptions, "nosuid") {
				fmt.Fprintln(w, "hint: mount has nosuid; file capabilities are ignored. Use sudo or move the binary.")
			}
			return nil
		},
	}
}

func newActivateCmd() *cobra.Command {
	var target string
	cmd := &cobra.Command{
		Use:   "activate",
		Short: "Grant required file capabilities to the persona binary",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveBinaryPath(target)
			if err != nil {
				return err
			}
			if err := requireSetcapCapability(); err != nil {
				fmt.Fprintln(os.Stderr, "persona: hint: run this command with sudo or as root")
				return err
			}
			setcapPath, err := exec.LookPath("setcap")
			if err != nil {
				return fmt.Errorf("setcap not found; install libcap2-bin")
			}
			perm := "cap_sys_admin,cap_dac_override+ep"
			out, err := exec.Command(setcapPath, perm, path).CombinedOutput()
			if err != nil {
				msg := strings.TrimSpace(string(out))
				if msg == "" {
					msg = err.Error()
				}
				return fmt.Errorf("setcap failed: %s", msg)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "capabilities set on %s\n", path)
			return nil
		},
	}
	cmd.Flags().StringVar(&target, "binary", "", "path to persona binary (default: current executable)")
	return cmd
}

func resolveBinaryPath(target string) (string, error) {
	if strings.TrimSpace(target) == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("resolve executable: %w", err)
		}
		target = exe
	}
	abs, err := filepath.Abs(target)
	if err == nil {
		target = abs
	}
	resolved := resolvePath(target)
	if resolved != "" {
		target = resolved
	}
	if _, err := os.Stat(target); err != nil {
		return "", fmt.Errorf("binary not found: %s", target)
	}
	return target, nil
}

func requireSetcapCapability() error {
	if os.Geteuid() == 0 {
		return nil
	}
	capEff, err := readCapEff()
	if err == nil && hasCap(capEff, capSetFcap) {
		return nil
	}
	return fmt.Errorf("insufficient privileges to set file capabilities (need CAP_SETFCAP)")
}

func collectDiag() diagInfo {
	info := diagInfo{EUID: os.Geteuid()}
	if exe, err := os.Executable(); err == nil {
		info.ExePath = exe
		info.ExeResolved = resolvePath(exe)
	}
	if capEff, err := readCapEff(); err == nil {
		info.CapEff = capEff
		info.HasSysAdmin = hasCap(capEff, capSysAdmin)
		info.HasSetFcap = hasCap(capEff, capSetFcap)
		info.HasDACOverride = hasCap(capEff, capDACOverride)
	}
	pathForCheck := info.ExeResolved
	if pathForCheck == "" {
		pathForCheck = info.ExePath
	}
	if pathForCheck != "" {
		if status, err := fileCapabilityStatus(pathForCheck); err == nil {
			info.FileCapStatus = status
		}
		if mi, err := findmntInfo(pathForCheck); err == nil {
			info.MountOptions = mi["OPTIONS"]
			info.MountFstype = mi["FSTYPE"]
			info.MountSource = mi["SOURCE"]
			info.MountTarget = mi["TARGET"]
		}
	}
	if setcapPath, err := exec.LookPath("setcap"); err == nil {
		info.SetcapPath = setcapPath
	}
	return info
}

func readCapEff() (uint64, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "CapEff:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, fmt.Errorf("CapEff format unexpected")
			}
			return strconv.ParseUint(fields[1], 16, 64)
		}
	}
	return 0, fmt.Errorf("CapEff not found")
}

func hasCap(capEff uint64, capID uint) bool {
	if capID >= 64 {
		return false
	}
	return capEff&(1<<capID) != 0
}

func fileCapabilityStatus(path string) (string, error) {
	size, err := unix.Getxattr(path, "security.capability", nil)
	if err != nil {
		switch {
		case errors.Is(err, unix.ENODATA):
			return "absent", nil
		case errors.Is(err, unix.EOPNOTSUPP):
			return "unsupported", nil
		case errors.Is(err, unix.EPERM):
			return "permission denied", nil
		default:
			return "", err
		}
	}
	if size <= 0 {
		return "absent", nil
	}
	buf := make([]byte, size)
	if _, err := unix.Getxattr(path, "security.capability", buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("present (len=%d)", size), nil
}

func findmntInfo(path string) (map[string]string, error) {
	findmnt, err := exec.LookPath("findmnt")
	if err != nil {
		return nil, err
	}
	out, err := exec.Command(findmnt, "-P", "-o", "OPTIONS,FSTYPE,SOURCE,TARGET", "-T", path).Output()
	if err != nil {
		return nil, err
	}
	info := make(map[string]string)
	for _, field := range strings.Fields(strings.TrimSpace(string(out))) {
		parts := strings.SplitN(field, "=", 2)
		if len(parts) != 2 {
			continue
		}
		info[parts[0]] = strings.Trim(parts[1], "\"")
	}
	return info, nil
}

func reportPermissionHint(op string, err error) {
	if err == nil {
		return
	}
	if !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EACCES) {
		return
	}
	info := collectDiag()
	if info.ExeResolved != "" {
		fmt.Fprintf(os.Stderr, "persona: hint: exe=%s\n", info.ExeResolved)
	}
	fmt.Fprintf(os.Stderr, "persona: hint: %s requires CAP_SYS_ADMIN (euid=%d cap_sys_admin=%t)\n", op, info.EUID, info.HasSysAdmin)
	if info.MountOptions != "" {
		fmt.Fprintf(os.Stderr, "persona: hint: mount_options=%s\n", info.MountOptions)
	}
	fmt.Fprintln(os.Stderr, "persona: hint: try `sudo persona activate` or `sudo setcap cap_sys_admin,cap_dac_override+ep /path/to/persona`.")
	fmt.Fprintln(os.Stderr, "persona: hint: if still denied, check nosuid mounts or LSM (AppArmor/SELinux) policies.")
}
