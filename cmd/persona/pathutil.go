package main

import (
	"context"
	"errors"
	"fmt"
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

func patchStateRelPaths(patchRel string) []string {
	if patchRel == "" {
		return nil
	}
	return []string{patchRel, patchRel + ".lock"}
}

func closeAndRemoveTempFile(file *os.File) {
	if file == nil {
		return
	}
	name := file.Name()
	_ = file.Close()
	_ = os.Remove(name)
}

func pushKeepSessionCleanup(cleanup *cleanupStack, retErr *error, opts model.Options, fn func() error) {
	if cleanup == nil || fn == nil {
		return
	}
	cleanup.Push(func() error {
		if retErr != nil && !shouldRemoveSession(*retErr, opts) {
			return nil
		}
		return fn()
	})
}

func pushUmountCleanup(cleanup *cleanupStack, mount model.NSOps, path string) {
	if cleanup == nil || mount == nil || path == "" {
		return
	}
	cleanup.Push(func() error {
		return mount.Umount(path)
	})
}

func listIgnoredCandidatesChecked(ctx context.Context, g model.GitOps, repoRoot, gitDir string, max int) ([]string, error) {
	ignored, err := g.ListIgnoredCandidates(ctx, repoRoot, gitDir, max)
	if err != nil {
		return nil, err
	}
	if err := checkIgnoredCandidateLimit(ignored, max); err != nil {
		return nil, err
	}
	return ignored, nil
}

func checkIgnoredCandidateLimit(ignored []string, max int) error {
	if max > 0 && len(ignored) > max {
		return fmt.Errorf("ignored candidate count exceeds ignored-max %d", max)
	}
	return nil
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
