package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"persona/internal/gitx"
	"persona/internal/model"
	"persona/internal/patchio"
)

type patchLimitWriter struct {
	w     io.Writer
	bytes int
}

func (w *patchLimitWriter) Write(p []byte) (int, error) {
	if err := patchio.CheckPatchSize(w.bytes + len(p)); err != nil {
		return 0, err
	}
	n, err := w.w.Write(p)
	w.bytes += n
	if err != nil {
		return n, err
	}
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

func runCommand(repoRoot, cwdRel string, cmdArgs []string) int {
	if len(cmdArgs) == 0 {
		return 0
	}
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	cmd.Env = gitx.FilterGitEnv(os.Environ())
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
	var out bytes.Buffer
	if _, err := exportPatchToWriter(ctx, g, repoRoot, gitDir, patchInRepo, patchRel, ignoredMode, ignoredMax, initialIgnored, &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func exportPatchToWriter(ctx context.Context, g model.GitOps, repoRoot, gitDir string, patchInRepo bool, patchRel string, ignoredMode model.IgnoredMode, ignoredMax int, initialIgnored []string, out io.Writer) (int, error) {
	if ignoredMax != 0 {
		if err := checkIgnoredCandidateLimit(initialIgnored, ignoredMax); err != nil {
			return 0, err
		}
		ignoredNow, err := listIgnoredCandidatesChecked(ctx, g, repoRoot, gitDir, ignoredMax)
		if err != nil {
			return 0, err
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
			return 0, fmt.Errorf("ignored paths changed during run: %s", strings.Join(problems, "; "))
		}
	}
	trackedExclude := patchStatePathsForRepo("", patchInRepo, filepath.ToSlash(patchRel)).rel
	limited := &patchLimitWriter{w: out}
	if err := g.DiffHeadBinaryTo(ctx, repoRoot, gitDir, trackedExclude, limited); err != nil {
		return 0, err
	}
	untracked, err := g.ListUntracked(ctx, repoRoot, gitDir)
	if err != nil {
		return 0, err
	}
	excludePrefixes := []string{".git/"}
	excludeExact := patchStatePathsForRepo("", patchInRepo, filepath.ToSlash(patchRel)).rel
	untracked = patchio.FilterUntrackedPaths(untracked, excludePrefixes, excludeExact)
	sort.Strings(untracked)

	for _, path := range untracked {
		absPath := filepath.Join(repoRoot, filepath.FromSlash(path))
		info, err := os.Lstat(absPath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return 0, err
		}
		mode := info.Mode()
		if mode&os.ModeNamedPipe != 0 || mode&os.ModeDevice != 0 || mode&os.ModeCharDevice != 0 || mode&os.ModeSocket != 0 {
			fmt.Fprintln(os.Stderr, "skip special file", path)
			continue
		}
		if err := g.DiffNewFileNoIndexTo(ctx, repoRoot, gitDir, path, limited); err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOENT) {
				continue
			}
			return 0, err
		}
	}
	return limited.bytes, nil
}
