package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

type executionProcess struct {
	parent int
	group  int
	state  string
	start  string
}

func readExecutionProcess(pid int) (executionProcess, error) {
	return readExecutionProcessFS(os.DirFS("/proc"), pid)
}

func readExecutionProcessFS(procFS fs.FS, pid int) (executionProcess, error) {
	data, err := fs.ReadFile(procFS, fmt.Sprintf("%d/stat", pid))
	if err != nil {
		return executionProcess{}, err
	}
	return parseExecutionProcess(string(data))
}

func parseExecutionProcess(data string) (executionProcess, error) {
	// The command name can contain spaces and parentheses. All numeric fields
	// follow its final closing parenthesis.
	end := strings.LastIndex(data, ") ")
	if end < 0 {
		return executionProcess{}, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(data[end+2:])
	if len(fields) < 20 {
		return executionProcess{}, fmt.Errorf("incomplete process stat")
	}
	parent, parentErr := strconv.Atoi(fields[1])
	group, groupErr := strconv.Atoi(fields[2])
	_, startErr := strconv.ParseUint(fields[19], 10, 64)
	if err := errors.Join(parentErr, groupErr, startErr); err != nil {
		return executionProcess{}, err
	}
	return executionProcess{parent: parent, group: group, state: fields[0], start: fields[19]}, nil
}

// The process group remains allocated while ordinary descendants are alive,
// even if the original client and group leader have both exited. Zombies cannot
// write and must not hold a session indefinitely while awaiting another reaper.
func executionGroupAlive(group int, start string) bool {
	if group <= 0 {
		return false
	}
	if err := syscall.Kill(-group, 0); errors.Is(err, syscall.ESRCH) {
		return false
	}
	return executionGroupActive(os.DirFS("/proc"), group, start)
}

func executionGroupActive(procFS fs.FS, group int, start string) bool {
	// Retry a snapshot invalidated by unrelated process creation, but never
	// spin indefinitely or report clean when the membership remains uncertain.
	for range 3 {
		alive, stable := inspectExecutionGroup(procFS, group, start)
		if alive || stable {
			return alive
		}
	}
	return true
}

func inspectExecutionGroup(procFS fs.FS, group int, start string) (alive, stable bool) {
	if leader, err := readExecutionProcessFS(procFS, group); err == nil && start != "" && leader.start != start {
		// A new leader using this PID means the recorded group has ended.
		return false, true
	}
	before, err := fs.ReadFile(procFS, "sys/kernel/ns_last_pid")
	if err != nil {
		return true, true
	}
	entries, err := fs.ReadDir(procFS, ".")
	if err != nil {
		return true, true // Failed inspection must never authorize a save or deletion.
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		process, err := readExecutionProcessFS(procFS, pid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return true, true
		}
		if process.group == group {
			if process.state != "Z" && process.state != "X" {
				return true, true
			}
			// pthread_exit can leave a zombie leader while its other threads
			// are still writing. /proc lists one directory per thread group.
			tasks, err := fs.ReadDir(procFS, fmt.Sprintf("%d/task", pid))
			if err != nil && !errors.Is(err, os.ErrNotExist) || len(tasks) > 1 {
				return true, true
			}
		}
	}
	// A writer can fork and exit between ReadDir and reading its stat. Only
	// accept a zombie-only result when no new PID/TID was allocated during the
	// scan; otherwise an unseen descendant may still be writing.
	after, err := fs.ReadFile(procFS, "sys/kernel/ns_last_pid")
	if err != nil {
		return true, true
	}
	return false, string(before) == string(after)
}

func mutationOwnerAlive(owner int, ownerStart string, group int, groupStart string) bool {
	return processIdentityAlive(owner, ownerStart) || executionGroupAlive(group, groupStart)
}

// An exec-view child attaches its group before entering the view and executing
// user code. If the client died and its lease was already recovered, attachment
// fails and the helper exits without running the command. Persisting first also
// keeps surviving writers protected across a daemon restart.
func (s *daemonState) attachExec(ctx context.Context, key, lease string, generation uint64, pid int) error {
	unlock, err := s.lockSession(ctx, key)
	if err != nil {
		return err
	}
	defer unlock()
	sess := s.sessionByKey(key)
	if sess == nil || sess.closed || !sess.busy || sess.busyLease != lease || lease == "" {
		return fmt.Errorf("session %q lease mismatch", key)
	}
	if generation == 0 || generation != sess.viewGeneration {
		return fmt.Errorf("view generation mismatch")
	}
	process, err := readExecutionProcess(pid)
	if err != nil {
		return fmt.Errorf("inspect execution process: %w", err)
	}
	if pid <= 0 || process.group != pid || process.parent != sess.busyOwnerPID ||
		!processIdentityAlive(sess.busyOwnerPID, sess.busyOwnerStart) {
		return fmt.Errorf("execution must be a new process group belonging to the mutation owner")
	}
	if sess.busyExecPGID != 0 && (sess.busyExecPGID != pid || sess.busyExecStart != process.start) {
		return fmt.Errorf("mutation lease already has an execution process")
	}
	sess.busyExecPGID = pid
	sess.busyExecStart = process.start
	if err := s.persistSessionLocked(sess); err != nil {
		// The helper exits on this error. Retaining its identity in memory is
		// conservative until release confirms its group is no longer running.
		return fmt.Errorf("record execution process: %w", err)
	}
	return nil
}
