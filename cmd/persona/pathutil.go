package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"persona/internal/model"
)

type cleanupStack struct {
	fns []func() error
}

const forceMountFailEnv = "PERSONA_FORCE_MOUNT_FAIL"

func (c *cleanupStack) Push(fn func() error) {
	if fn == nil {
		return
	}
	c.fns = append(c.fns, fn)
}

func (c *cleanupStack) Run() error {
	var errs []error
	for i := len(c.fns) - 1; i >= 0; i-- {
		if err := c.fns[i](); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

type exitError struct {
	code model.ExitCode
}

func (e *exitError) Error() string {
	return ""
}

func isSubpath(root, path string) (bool, string) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false, ""
	}
	rel = filepath.Clean(rel)
	if rel == "." {
		return true, rel
	}
	parentPrefix := ".." + string(filepath.Separator)
	if rel == ".." || strings.HasPrefix(rel, parentPrefix) {
		return false, ""
	}
	return true, rel
}

func resolvePath(path string) string {
	real, err := filepath.EvalSymlinks(path)
	if err == nil {
		return real
	}
	path = filepath.Clean(path)
	if path == "" || !filepath.IsAbs(path) {
		return path
	}
	for depth := 0; depth < 255; depth++ {
		current := string(filepath.Separator)
		parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
		changed := false
		for i, part := range parts {
			if part == "" {
				continue
			}
			next := filepath.Join(current, part)
			info, statErr := os.Lstat(next)
			if statErr != nil {
				if os.IsNotExist(statErr) {
					return filepath.Clean(filepath.Join(append([]string{current}, parts[i:]...)...))
				}
				return path
			}
			if info.Mode()&os.ModeSymlink != 0 {
				target, readErr := os.Readlink(next)
				if readErr != nil {
					return path
				}
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(next), target)
				}
				path = filepath.Clean(filepath.Join(append([]string{target}, parts[i+1:]...)...))
				changed = true
				break
			}
			current = next
		}
		if !changed {
			return current
		}
	}
	return path
}

func shouldRemoveSession(err error, opts model.Options) bool {
	switch opts.KeepSession {
	case model.KeepAlways:
		return false
	case model.KeepNever:
		return true
	case model.KeepOnFail:
		return err == nil
	default:
		return true
	}
}

func shouldForceMountFail() bool {
	return os.Getenv(forceMountFailEnv) == "1"
}
