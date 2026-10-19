package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
	"persona/internal/gitx"
	"persona/internal/model"
	"persona/internal/ns"
	"persona/internal/session"
)

func addDaemonLeaseCommands(parent *cobra.Command) {
	var flags daemonFlagValues
	var owner int
	var asJSON bool
	acquire := &cobra.Command{Use: "acquire --session-key <key> --owner-pid <pid>", Short: "Acquire the exclusive mutation lease for a file tool", RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := buildDaemonSessionConfig(flags)
		if err != nil {
			return err
		}
		if owner <= 0 {
			return fmt.Errorf("owner-pid must identify the long-lived tool owner")
		}
		repo, git, _, err := daemonRepoContext(cmd.Context())
		if err != nil {
			return err
		}
		client, err := newDaemonClient(cmd.Context(), repo, git)
		if err != nil {
			return err
		}
		resp, err := client.call(cmd.Context(), daemonRequest{Method: "acquire_exec", SessionKey: strings.TrimSpace(flags.sessionKey), OwnerPID: owner, Config: cfg})
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(resp)
	}}
	bindDaemonSessionFlags(acquire, &flags)
	acquire.Flags().IntVar(&owner, "owner-pid", 0, "PID of the long-lived tool owner")
	acquire.Flags().BoolVar(&asJSON, "json", false, "print the lease as JSON")
	_ = acquire.MarkFlagRequired("owner-pid")
	var key, lease string
	var generation uint64
	release := &cobra.Command{Use: "release --session-key <key> --lease-id <id>", Short: "Release a file-tool lease and durably save its changes", RunE: func(cmd *cobra.Command, args []string) error {
		repo, git, _, err := daemonRepoContext(cmd.Context())
		if err != nil {
			return err
		}
		client, err := newDaemonClient(cmd.Context(), repo, git)
		if err != nil {
			return err
		}
		_, err = client.call(cmd.Context(), daemonRequest{Method: "release_exec", SessionKey: strings.TrimSpace(key), LeaseID: lease, ViewGeneration: generation})
		return err
	}}
	release.Flags().StringVar(&key, "session-key", "", "session key")
	release.Flags().StringVar(&lease, "lease-id", "", "lease returned by acquire")
	release.Flags().Uint64Var(&generation, "view-generation", 0, "generation returned by acquire")
	_ = release.MarkFlagRequired("session-key")
	_ = release.MarkFlagRequired("lease-id")
	var repoRoot, gitDir, viewPath, cwdRel, execKey, execLease string
	var execGeneration uint64
	view := &cobra.Command{Use: "exec-view -- <command...>", Hidden: true, Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if execKey == "" || execLease == "" || !filepath.IsAbs(gitDir) || execGeneration == 0 {
			return fmt.Errorf("exec-view requires an acquired daemon lease")
		}
		client := &daemonClient{socketPath: daemonSocketPath(gitDir), repoRoot: repoRoot, gitDir: gitDir}
		if _, err := client.call(cmd.Context(), daemonRequest{Method: "attach_exec", SessionKey: execKey,
			LeaseID: execLease, ViewGeneration: execGeneration, ExecutionPID: os.Getpid()}); err != nil {
			return err
		}
		return runDaemonView(repoRoot, viewPath, cwdRel, args)
	}}
	view.Flags().StringVar(&repoRoot, "repo-root", "", "original repository root")
	view.Flags().StringVar(&gitDir, "git-dir", "", "original repository git directory")
	view.Flags().StringVar(&viewPath, "view-path", "", "daemon view")
	view.Flags().StringVar(&cwdRel, "cwd-rel", ".", "working directory relative to the repository")
	view.Flags().StringVar(&execKey, "session-key", "", "session owning the command")
	view.Flags().StringVar(&execLease, "lease-id", "", "acquired mutation lease")
	view.Flags().Uint64Var(&execGeneration, "view-generation", 0, "generation returned by acquire")
	var recoverKey string
	var dryRun bool
	recoverCmd := &cobra.Command{Use: "recover --session-key <key> [--dry-run]", Short: "Inspect or reopen a preserved session without discarding its upper layer", RunE: func(cmd *cobra.Command, args []string) error {
		repo, git, _, err := daemonRepoContext(cmd.Context())
		if err != nil {
			return err
		}
		client, err := newDaemonClient(cmd.Context(), repo, git)
		if err != nil {
			return err
		}
		resp, err := client.call(cmd.Context(), daemonRequest{Method: "recover", SessionKey: strings.TrimSpace(recoverKey), DryRun: dryRun})
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(resp.Session)
	}}
	recoverCmd.Flags().StringVar(&recoverKey, "session-key", "", "session key")
	recoverCmd.Flags().BoolVar(&dryRun, "dry-run", false, "inspect metadata without mounting or modifying the session")
	_ = recoverCmd.MarkFlagRequired("session-key")
	parent.AddCommand(acquire, release, view, recoverCmd)
}

// Only this short-lived helper enters the child namespace. Original absolute
// paths, environment paths, and paths embedded inside scripts see the view.
func runDaemonView(repoRoot, viewPath, cwdRel string, args []string) error {
	if !filepath.IsAbs(repoRoot) || !filepath.IsAbs(viewPath) {
		return fmt.Errorf("absolute repo and view paths are required")
	}
	if ok, rel := isSubpath(repoRoot, filepath.Join(repoRoot, cwdRel)); !ok || rel == ".." {
		return fmt.Errorf("working directory escapes repository")
	}
	runtime.LockOSThread()
	// Do not unlock a thread that has joined a private mount namespace. Go
	// retires the locked thread when this helper exits.
	if err := ns.UnshareMountNS(); err != nil {
		return model.Wrap(model.ExitEnv, "unshare daemon command namespace", err)
	}
	if err := ns.MakeMountsPrivate(); err != nil {
		return model.Wrap(model.ExitEnv, "private daemon command mounts", err)
	}
	if err := ns.BindMountRecursive(viewPath, repoRoot); err != nil {
		return model.Wrap(model.ExitEnv, "map original repository into daemon view", err)
	}
	defer ns.Umount(repoRoot)
	if err := os.Chdir(filepath.Join(repoRoot, cwdRel)); err != nil {
		return err
	}
	// Keep the registered helper PID and process group for the actual command.
	// Forking another group here would let it outlive the lease's tracked group.
	path, err := exec.LookPath(args[0])
	if err == nil {
		err = unix.Exec(path, args, gitx.FilterGitEnv(os.Environ()))
	}
	fmt.Fprintln(os.Stderr, err)
	return &exitError{code: 127}
}

func processStartTime(pid int) string {
	if pid <= 0 {
		return ""
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return ""
	}
	end := strings.LastIndex(string(data), ") ")
	if end < 0 {
		return ""
	}
	fields := strings.Fields(string(data)[end+2:])
	if len(fields) < 20 {
		return ""
	}
	if _, err := strconv.ParseUint(fields[19], 10, 64); err != nil {
		return ""
	}
	return fields[19]
}
func processIdentityAlive(pid int, start string) bool {
	return processAlive(pid) && (start == "" || processStartTime(pid) == start)
}

func flockContext(ctx context.Context, file *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func daemonPingRepo(ctx context.Context, socketPath, repo, git string) error {
	ctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	client := &daemonClient{socketPath: socketPath, repoRoot: repo, gitDir: git}
	resp, err := client.call(ctx, daemonRequest{Method: "ping"})
	if err != nil {
		return err
	}
	if resp.Version != daemonProtocol || resp.RepoRoot != repo || resp.GitDir != git {
		return fmt.Errorf("daemon identity or protocol mismatch")
	}
	return nil
}

func (s *daemonState) recoverSession(ctx context.Context, key string, dryRun bool) (*daemonSessionInfo, error) {
	unlock, err := s.lockSession(ctx, key)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if sess := s.sessionByKey(key); sess != nil {
		info := daemonSessionSnapshot(sess)
		return &info, nil
	}
	meta, err := readDaemonSessionMeta(daemonSessionMetaPath(s.gitDir, key))
	if err != nil {
		return nil, err
	}
	if meta.SessionKey != key {
		return nil, fmt.Errorf("session metadata key mismatch")
	}
	info := &daemonSessionInfo{SessionKey: key, RepoRoot: s.repoRoot, GitDir: s.gitDir, PatchPath: daemonSessionPatchPath(s.gitDir, key), RecoveryRoot: meta.SessionRoot, ExternalRoot: meta.ExternalRoot, Dirty: meta.Dirty, BaseOID: meta.BaseOID, State: "recovery_required", LastError: meta.LastError}
	if dryRun {
		return info, nil
	}
	if mutationOwnerAlive(meta.BusyOwnerPID, meta.BusyOwnerStart, meta.BusyExecPGID, meta.BusyExecStart) {
		return nil, fmt.Errorf("previous mutation owner or command group is still alive")
	}
	checkpoint, err := openDaemonCheckpoint(ctx, daemonSessionPatchPath(s.gitDir, key))
	if err != nil {
		return nil, err
	}
	defer func() {
		if checkpoint != nil {
			_ = checkpoint.close()
		}
	}()
	if err := checkpoint.validate(&meta); err != nil {
		return nil, err
	}
	if _, statErr := os.Lstat(meta.SessionRoot); statErr == nil {
		owned, err := session.Open(s.gitDir, meta.SessionRoot)
		if err != nil {
			return nil, err
		}
		if err := validateDaemonOwner(owned.Root, key, meta.BaseOID); err != nil {
			return nil, err
		}
		if err := unmountPreservedSession(owned.Root); err != nil {
			return nil, err
		}
		if !meta.Dirty {
			if meta.Config.BaseMode == model.BaseWorktree {
				g := &gitx.Git{RepoRoot: s.repoRoot, GitDir: s.gitDir}
				if err := g.WorktreeRemoveForce(ctx, owned.BaseWT); err != nil {
					return nil, err
				}
			}
			if err := owned.RemoveAll(); err != nil {
				return nil, err
			}
		}
	} else if !errors.Is(statErr, os.ErrNotExist) || meta.Dirty {
		return nil, fmt.Errorf("preserved session root unavailable: %w", statErr)
	}
	if err := cleanupRecoveryExternal(meta); err != nil {
		return nil, err
	}
	meta.ExternalRoot = ""
	restored, err := s.restoreSessionWithCheckpoint(ctx, meta, true, checkpoint)
	if restored != nil {
		checkpoint = nil
	}
	if err != nil {
		return nil, err
	}
	snap := daemonSessionSnapshot(restored)
	return &snap, nil
}

func cleanupRecoveryExternal(meta daemonSessionMeta) error {
	root := meta.ExternalRoot
	if root == "" {
		return nil
	}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if !filepath.IsAbs(root) || filepath.Dir(root) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(root), "persona-session-") {
		return fmt.Errorf("external recovery root is not an owned temporary directory: %s", root)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("invalid external recovery directory")
	}
	if err := validateDaemonOwner(root, meta.SessionKey, meta.BaseOID); err != nil {
		return err
	}
	if err := unmountPreservedSession(root); err != nil {
		return err
	}
	return cleanupUnmounted(root, func() error { return os.RemoveAll(root) })
}

// Unmount deepest paths first, without lazy detach; an active user must keep
// recovery blocked rather than allowing removal of a still-used overlay.
func unmountPreservedSession(root string) error {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	defer file.Close()
	scan := bufio.NewScanner(file)
	var paths []string
	decode := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) < 5 {
			continue
		}
		path := decode.Replace(fields[4])
		if path == root || strings.HasPrefix(path, root+"/") {
			paths = append(paths, path)
		}
	}
	if err := scan.Err(); err != nil {
		return err
	}
	sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
	for _, path := range paths {
		if err := unix.Unmount(path, 0); err != nil {
			return fmt.Errorf("unmount preserved session %s: %w", path, err)
		}
	}
	return nil
}
