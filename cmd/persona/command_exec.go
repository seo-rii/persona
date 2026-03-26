package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"persona/internal/model"
	"persona/internal/patchio"
)

func runCommand(repoRoot, cwdRel string, cmdArgs []string) int {
	if len(cmdArgs) == 0 {
		return 0
	}
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	env := os.Environ()
	filteredEnv := make([]string, 0, len(env))
	for _, item := range env {
		if idx := strings.IndexByte(item, '='); idx > 0 && strings.HasPrefix(item[:idx], "GIT_") {
			continue
		}
		filteredEnv = append(filteredEnv, item)
	}
	cmd.Env = filteredEnv
	if cwdRel == "." {
		cmd.Dir = repoRoot
	} else {
		cmd.Dir = filepath.Join(repoRoot, cwdRel)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 127
	}

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		for sig := range sigCh {
			_ = syscall.Kill(-cmd.Process.Pid, sig.(syscall.Signal))
		}
	}()

	err := cmd.Wait()
	signal.Stop(sigCh)
	close(sigCh)
	pgid := cmd.Process.Pid
	if killErr := syscall.Kill(-pgid, syscall.SIGTERM); killErr == nil || errors.Is(killErr, syscall.EPERM) {
		deadline := time.Now().Add(200 * time.Millisecond)
		for {
			probeErr := syscall.Kill(-pgid, 0)
			if probeErr != nil && !errors.Is(probeErr, syscall.EPERM) {
				break
			}
			if time.Now().After(deadline) {
				_ = syscall.Kill(-pgid, syscall.SIGKILL)
				for i := 0; i < 20; i++ {
					probeErr = syscall.Kill(-pgid, 0)
					if probeErr != nil && !errors.Is(probeErr, syscall.EPERM) {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if status, ok := exitErr.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
		code := exitErr.ExitCode()
		if code >= int(model.ExitEnv) && code <= int(model.ExitWrite) {
			return int(model.ExitChildReservedBase) + (code - int(model.ExitEnv))
		}
		return code
	}
	fmt.Fprintln(os.Stderr, err)
	return 127
}

func exportPatch(ctx context.Context, g model.GitOps, repoRoot, gitDir string, patchInRepo bool, patchRel string, ignoredMode model.IgnoredMode, ignoredMax int, initialIgnored []string) ([]byte, error) {
	if ignoredMax != 0 {
		if ignoredMax > 0 && len(initialIgnored) > ignoredMax {
			return nil, fmt.Errorf("ignored candidate count exceeds ignored-max %d", ignoredMax)
		}
		ignoredNow, err := g.ListIgnoredCandidates(ctx, repoRoot, gitDir, ignoredMax)
		if err != nil {
			return nil, err
		}
		if ignoredMax > 0 && len(ignoredNow) > ignoredMax {
			return nil, fmt.Errorf("ignored candidate count exceeds ignored-max %d", ignoredMax)
		}
		ignoredSet := make(map[string]struct{}, len(initialIgnored))
		for _, path := range initialIgnored {
			ignoredSet[path] = struct{}{}
		}
		ignoredNowSet := make(map[string]struct{}, len(ignoredNow))
		var newlyIgnored []string
		for _, path := range ignoredNow {
			ignoredNowSet[path] = struct{}{}
			if _, ok := ignoredSet[path]; ok {
				continue
			}
			newlyIgnored = append(newlyIgnored, path)
		}
		var noLongerIgnored []string
		for _, path := range initialIgnored {
			if _, ok := ignoredNowSet[path]; ok {
				continue
			}
			noLongerIgnored = append(noLongerIgnored, path)
		}
		if len(newlyIgnored) > 0 || len(noLongerIgnored) > 0 {
			sort.Strings(newlyIgnored)
			sort.Strings(noLongerIgnored)
			var problems []string
			if len(newlyIgnored) > 0 {
				problems = append(problems, "newly ignored: "+strings.Join(newlyIgnored, ", "))
			}
			if len(noLongerIgnored) > 0 {
				problems = append(problems, "no longer ignored: "+strings.Join(noLongerIgnored, ", "))
			}
			return nil, fmt.Errorf("ignored paths changed during run: %s", strings.Join(problems, "; "))
		}
	}
	trackedExclude := []string{}
	if patchInRepo && patchRel != "" {
		trackedExclude = append(trackedExclude, filepath.ToSlash(patchRel), filepath.ToSlash(patchRel+".lock"))
	}
	tracked, err := g.DiffHeadBinary(ctx, repoRoot, gitDir, trackedExclude)
	if err != nil {
		return nil, err
	}
	untracked, err := g.ListUntracked(ctx, repoRoot, gitDir)
	if err != nil {
		return nil, err
	}
	excludePrefixes := []string{".git/"}
	excludeExact := []string{}
	if patchInRepo && patchRel != "" {
		excludeExact = append(excludeExact, filepath.ToSlash(patchRel), filepath.ToSlash(patchRel+".lock"))
	}
	untracked = patchio.FilterUntrackedPaths(untracked, excludePrefixes, excludeExact)
	sort.Strings(untracked)

	buf := &bytes.Buffer{}
	if len(tracked) > 0 {
		if err := patchio.CheckPatchSize(len(tracked)); err != nil {
			return nil, err
		}
		buf.Write(tracked)
	}
	for _, path := range untracked {
		absPath := filepath.Join(repoRoot, filepath.FromSlash(path))
		info, err := os.Lstat(absPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		mode := info.Mode()
		if mode&os.ModeNamedPipe != 0 || mode&os.ModeDevice != 0 || mode&os.ModeCharDevice != 0 || mode&os.ModeSocket != 0 {
			fmt.Fprintln(os.Stderr, "skip special file", path)
			continue
		}
		patch, err := g.DiffNewFileNoIndex(ctx, repoRoot, gitDir, path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
				continue
			}
			return nil, err
		}
		if err := patchio.CheckPatchSize(buf.Len() + len(patch)); err != nil {
			return nil, err
		}
		buf.Write(patch)
	}
	return buf.Bytes(), nil
}
