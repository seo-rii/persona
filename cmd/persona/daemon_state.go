package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// A gate protects a single session, including creation. The map mutex is never
// held across Git, flock, mount, or filesystem I/O.
func (s *daemonState) lockSession(ctx context.Context, key string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, fmt.Errorf("daemon is shutting down")
	}
	gate := s.sessionGateLocked(key)
	s.mu.Unlock()
	select {
	case gate <- struct{}{}:
		s.mu.Lock()
		closing := s.closing
		s.mu.Unlock()
		if closing {
			<-gate
			return nil, fmt.Errorf("daemon is shutting down")
		}
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *daemonState) sessionGateLocked(key string) chan struct{} {
	if s.gates == nil {
		s.gates = make(map[string]chan struct{})
	}
	if s.gates[key] == nil {
		s.gates[key] = make(chan struct{}, 1)
	}
	return s.gates[key]
}

func (s *daemonState) sessionByKey(key string) *daemonSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[key]
}
func (s *daemonState) putSession(key string, sess *daemonSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[key] = sess
	if s.cached == nil {
		s.cached = make(map[string]daemonSessionInfo)
	}
	s.cached[key] = daemonSessionSnapshot(sess)
}
func (s *daemonState) removeSession(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, key)
	delete(s.cached, key)
}

func (s *daemonState) cacheSession(sess *daemonSession) {
	info := daemonSessionSnapshot(sess)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached == nil {
		s.cached = make(map[string]daemonSessionInfo)
	}
	s.cached[sess.key] = info
}
func (s *daemonState) trySession(ctx context.Context, key string) (func(), *daemonSessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, nil, fmt.Errorf("daemon is shutting down")
	}
	gate := s.sessionGateLocked(key)
	select {
	case gate <- struct{}{}:
		s.mu.Unlock()
		return func() { <-gate }, nil, nil
	default:
		if info, ok := s.cached[key]; ok {
			s.mu.Unlock()
			return nil, &info, nil
		}
	}
	s.mu.Unlock()
	unlock, err := s.lockSession(ctx, key)
	return unlock, nil, err
}
func (s *daemonState) sessionKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.sessions))
	for key := range s.sessions {
		keys = append(keys, key)
	}
	return keys
}

func (s *daemonState) operationKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make(map[string]bool, len(s.gates)+len(s.sessions))
	for key := range s.gates {
		keys[key] = true
	}
	for key := range s.sessions {
		keys[key] = true
	}
	out := make([]string, 0, len(keys))
	for key := range keys {
		out = append(out, key)
	}
	return out
}
func daemonIdleDuration(req daemonRequest) time.Duration {
	if req.IdleForNanos != 0 {
		return time.Duration(req.IdleForNanos)
	}
	return time.Duration(req.IdleForSeconds) * time.Second
}

func daemonOwnerData(key, oid string) string {
	digest := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x\n%s\n", digest, oid)
}
func writeDaemonOwner(root, key, oid string) error {
	file, err := os.OpenFile(filepath.Join(root, ".persona-daemon-owner"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.WriteString(daemonOwnerData(key, oid)); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return syncDaemonDirectory(root)
}
func validateDaemonOwner(root, key, oid string) error {
	path := filepath.Join(root, ".persona-daemon-owner")
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 256 {
		return fmt.Errorf("invalid daemon session ownership marker")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(data) != daemonOwnerData(key, oid) {
		return fmt.Errorf("daemon session ownership/base mismatch")
	}
	return nil
}

func (s *daemonState) abortAcquire(sess *daemonSession, masks []string, cause error) error {
	if err := umountPathsReverse(sess.mount, masks); err != nil {
		sess.ignoredMasks = masks
		sess.busy = true
		sess.busyOwnerPID = 0
		sess.busyOwnerStart = ""
		sess.dirty = true
		sess.lastError = err.Error()
		return errors.Join(cause, err, s.persistSessionLocked(sess))
	}
	return cause
}
