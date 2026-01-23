package ns

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func UnshareMountNS() error {
	return unix.Unshare(unix.CLONE_NEWNS)
}

func MakeMountsPrivate() error {
	return unix.Mount("", "/", "", uintptr(unix.MS_REC|unix.MS_PRIVATE), "")
}

func BindMount(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(dirOf(dst), 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(dst); err != nil {
			if os.IsNotExist(err) {
				file, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR, 0o644)
				if err != nil {
					return err
				}
				file.Close()
			} else {
				return err
			}
		}
	}
	return unix.Mount(src, dst, "", unix.MS_BIND, "")
}

func RemountRO(target string) error {
	return unix.Mount("", target, "", uintptr(unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY), "")
}

func Umount(target string) error {
	if err := unix.Unmount(target, 0); err != nil {
		if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
			return nil
		}
		if errors.Is(err, unix.EBUSY) {
			if err := unix.Unmount(target, unix.MNT_DETACH); err != nil {
				if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOENT) {
					return nil
				}
				return err
			}
			return nil
		}
		return err
	}
	return nil
}

type OverlayOpts struct {
	LowerDir string
	UpperDir string
	WorkDir  string
}

func MountOverlay(target string, opt OverlayOpts) error {
	if os.Getenv("PERSONA_FORCE_MOUNT_FAIL") == "1" {
		return fmt.Errorf("forced mount failure")
	}
	data := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", opt.LowerDir, opt.UpperDir, opt.WorkDir)
	return unix.Mount("overlay", target, "overlay", 0, data)
}

type MaskKind int

const (
	MaskFile MaskKind = iota
	MaskDir
)

func MaskPath(target string, kind MaskKind, emptyFile, emptyDir string) error {
	switch kind {
	case MaskFile:
		return BindMount(emptyFile, target)
	case MaskDir:
		return BindMount(emptyDir, target)
	default:
		return fmt.Errorf("unknown mask kind")
	}
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			return path[:i]
		}
	}
	return "."
}
