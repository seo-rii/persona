package main

import (
	"errors"
	"fmt"
	"io"
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

var setcapCandidates = []string{
	"/usr/sbin/setcap",
	"/usr/bin/setcap",
	"/sbin/setcap",
	"/bin/setcap",
}

var (
	stderrWriter              io.Writer = os.Stderr
	executablePathFn                    = os.Executable
	geteuidFn                           = os.Geteuid
	getxattrFn                          = unix.Getxattr
	readFileFn                          = os.ReadFile
	lookPathFn                          = exec.LookPath
	execCommandFn                       = exec.Command
	requireSetcapCapabilityFn           = requireSetcapCapability
	findSetcapPathFn                    = findSetcapPath
	resolveSetcapPathFn                 = resolveSetcapPath
	overlayfsStatusFn                   = overlayfsStatus
	unshareMountStatusFn                = unshareMountStatus
	collectDiagFn                       = collectDiag
	readCapEffFn                        = readCapEff
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
	Overlayfs      string
	UnshareMount   string
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
			info := collectDiagFn()
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
			if info.Overlayfs != "" {
				fmt.Fprintf(w, "overlayfs=%s\n", info.Overlayfs)
			}
			if info.UnshareMount != "" {
				fmt.Fprintf(w, "unshare_mount=%s\n", info.UnshareMount)
			}
			if !info.HasSysAdmin {
				fmt.Fprintln(w, "hint: CAP_SYS_ADMIN is required for unshare and OverlayFS mount.")
				fmt.Fprintf(w, "hint: %s\n", leastPrivilegeCapabilityHint())
			}
			if strings.Contains(info.MountOptions, "nosuid") {
				fmt.Fprintln(w, "hint: mount has nosuid; file capabilities are ignored. Use sudo or move the binary.")
			}
			if info.Overlayfs == "missing" || strings.HasPrefix(info.Overlayfs, "error:") {
				fmt.Fprintln(w, "hint: overlayfs support is unavailable; load the overlay module or use a kernel/filesystem configuration that supports OverlayFS.")
			}
			if info.UnshareMount != "" && info.UnshareMount != "ok" {
				fmt.Fprintln(w, "hint: `unshare -m true` must succeed for persona to create an isolated mount namespace.")
			}
			return nil
		},
	}
}

func newActivateCmd() *cobra.Command {
	var target string
	var allowDACOverride bool
	cmd := &cobra.Command{
		Use:   "activate",
		Short: "Grant required file capabilities to the persona binary",
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolveBinaryPath(target)
			if err != nil {
				return err
			}
			if err := requireSetcapCapabilityFn(); err != nil {
				fmt.Fprintln(stderrWriter, "persona: hint: run this command with sudo or as root")
				return err
			}
			setcapPath, err := resolveSetcapPathFn(setcapCandidates)
			if err != nil {
				return fmt.Errorf("setcap not found; install libcap2-bin")
			}
			perm := activateCapabilitySpec(allowDACOverride)
			out, err := execCommandFn(setcapPath, perm, path).CombinedOutput()
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
	cmd.Flags().BoolVar(&allowDACOverride, "allow-dac-override", false, "also grant CAP_DAC_OVERRIDE for patch writes that must bypass DAC checks")
	return cmd
}

func activateCapabilitySpec(allowDACOverride bool) string {
	if allowDACOverride {
		return "cap_sys_admin,cap_dac_override+ep"
	}
	return "cap_sys_admin+ep"
}

func leastPrivilegeCapabilityHint() string {
	return fmt.Sprintf(
		"run `sudo persona activate` or `sudo setcap %s /path/to/persona`; add `--allow-dac-override` only when patch writes must bypass DAC checks.",
		activateCapabilitySpec(false),
	)
}

func findSetcapPath(candidates []string) (string, error) {
	for _, candidate := range candidates {
		if !filepath.IsAbs(candidate) {
			continue
		}
		info, err := os.Stat(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", err
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		return candidate, nil
	}
	return "", fmt.Errorf("setcap not found")
}

func resolveSetcapPath(candidates []string) (string, error) {
	override := strings.TrimSpace(os.Getenv("PERSONA_SETCAP_BIN"))
	if override != "" {
		info, err := os.Stat(override)
		if err != nil {
			return "", err
		}
		if info.IsDir() || info.Mode()&0o111 == 0 {
			return "", fmt.Errorf("setcap not found")
		}
		return override, nil
	}
	return findSetcapPathFn(candidates)
}

func resolveBinaryPath(target string) (string, error) {
	if strings.TrimSpace(target) == "" {
		exe, err := executablePathFn()
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
	if geteuidFn() == 0 {
		return nil
	}
	capEff, err := readCapEffFn()
	if err == nil && hasCap(capEff, capSetFcap) {
		return nil
	}
	return fmt.Errorf("insufficient privileges to set file capabilities (need CAP_SETFCAP)")
}

func collectDiag() diagInfo {
	info := diagInfo{EUID: geteuidFn()}
	if exe, err := executablePathFn(); err == nil {
		info.ExePath = exe
		info.ExeResolved = resolvePath(exe)
	}
	if capEff, err := readCapEffFn(); err == nil {
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
	if setcapPath, err := resolveSetcapPathFn(setcapCandidates); err == nil {
		info.SetcapPath = setcapPath
	}
	info.Overlayfs = overlayfsStatusFn()
	info.UnshareMount = unshareMountStatusFn()
	return info
}

func readCapEff() (uint64, error) {
	data, err := readFileFn("/proc/self/status")
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
	size, err := getxattrFn(path, "security.capability", nil)
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
	if _, err := getxattrFn(path, "security.capability", buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("present (len=%d)", size), nil
}

func findmntInfo(path string) (map[string]string, error) {
	findmnt, err := lookPathFn("findmnt")
	if err != nil {
		return nil, err
	}
	out, err := execCommandFn(findmnt, "-P", "-o", "OPTIONS,FSTYPE,SOURCE,TARGET", "-T", path).Output()
	if err != nil {
		return nil, err
	}
	return parseFindmntPairs(string(out)), nil
}

func parseFindmntPairs(raw string) map[string]string {
	s := strings.TrimSpace(raw)
	info := make(map[string]string)
	i := 0
	for i < len(s) {
		for i < len(s) && isFindmntSpace(s[i]) {
			i++
		}
		if i >= len(s) {
			break
		}

		keyStart := i
		for i < len(s) && !isFindmntSpace(s[i]) && s[i] != '=' {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			for i < len(s) && !isFindmntSpace(s[i]) {
				i++
			}
			continue
		}
		key := s[keyStart:i]
		i++
		if key == "" {
			continue
		}

		if i < len(s) && s[i] == '"' {
			i++
			var b strings.Builder
			escaped := false
			for i < len(s) {
				ch := s[i]
				i++
				if escaped {
					b.WriteByte(ch)
					escaped = false
					continue
				}
				if ch == '\\' {
					escaped = true
					continue
				}
				if ch == '"' {
					break
				}
				b.WriteByte(ch)
			}
			info[key] = b.String()
			continue
		}

		valueStart := i
		for i < len(s) && !isFindmntSpace(s[i]) {
			i++
		}
		info[key] = s[valueStart:i]
	}
	return info
}

func isFindmntSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

func reportPermissionHint(op string, err error) {
	if err == nil {
		return
	}
	if !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.EACCES) {
		return
	}
	info := collectDiagFn()
	if info.ExeResolved != "" {
		fmt.Fprintf(stderrWriter, "persona: hint: exe=%s\n", info.ExeResolved)
	}
	fmt.Fprintf(stderrWriter, "persona: hint: %s requires CAP_SYS_ADMIN (euid=%d cap_sys_admin=%t)\n", op, info.EUID, info.HasSysAdmin)
	if info.MountOptions != "" {
		fmt.Fprintf(stderrWriter, "persona: hint: mount_options=%s\n", info.MountOptions)
	}
	fmt.Fprintf(stderrWriter, "persona: hint: %s\n", leastPrivilegeCapabilityHint())
	fmt.Fprintln(stderrWriter, "persona: hint: if still denied, check nosuid mounts or LSM (AppArmor/SELinux) policies.")
}

func overlayfsStatus() string {
	data, err := readFileFn("/proc/filesystems")
	if err != nil {
		return "error: " + err.Error()
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if fields[len(fields)-1] == "overlay" {
			return "available"
		}
	}
	return "missing"
}

func unshareMountStatus() string {
	unsharePath, err := lookPathFn("unshare")
	if err != nil {
		return "missing"
	}
	out, err := execCommandFn(unsharePath, "-m", "true").CombinedOutput()
	if err == nil {
		return "ok"
	}
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	return "blocked: " + msg
}
