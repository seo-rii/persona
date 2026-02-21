package ns

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
		return fmt.Errorf("bind mount stat %s: %w", src, err)
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("bind mount mkdir %s: %w", dst, err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("bind mount mkdir parent %s: %w", dst, err)
		}
		if _, err := os.Stat(dst); err != nil {
			if os.IsNotExist(err) {
				file, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR, 0o644)
				if err != nil {
					return fmt.Errorf("bind mount create %s: %w", dst, err)
				}
				file.Close()
			} else {
				return fmt.Errorf("bind mount stat %s: %w", dst, err)
			}
		}
	}
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind mount %s -> %s: %w", src, dst, err)
	}
	return nil
}

func RemountRO(target string) error {
	if err := unix.Mount("", target, "", uintptr(unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY), ""); err != nil {
		return fmt.Errorf("remount ro %s: %w", target, err)
	}
	return nil
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
				return fmt.Errorf("umount (detach) %s: %w", target, err)
			}
			return nil
		}
		return fmt.Errorf("umount %s: %w", target, err)
	}
	return nil
}

type OverlayOpts struct {
	LowerDir string
	UpperDir string
	WorkDir  string
}

func MountOverlay(target string, opt OverlayOpts) error {
	data := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", opt.LowerDir, opt.UpperDir, opt.WorkDir)
	if err := unix.Mount("overlay", target, "overlay", 0, data); err != nil {
		return fmt.Errorf("mount overlay on %s: %w", target, err)
	}
	return nil
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
