package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"persona/internal/patchio"
)

// Keep the patch lock from validation through recovery cleanup and mounting.
// On successful creation the session takes ownership of these descriptors.
type daemonCheckpoint struct {
	store    *patchio.PatchStore
	lock     *patchio.PatchLock
	checksum string
}

func openDaemonCheckpoint(ctx context.Context, path string) (*daemonCheckpoint, error) {
	store, err := patchio.OpenPatchStore(path)
	if err != nil {
		return nil, err
	}
	lock, err := store.LockContext(ctx)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	checkpoint := &daemonCheckpoint{store: store, lock: lock}
	checkpoint.checksum, err = daemonPatchChecksum(store)
	if err != nil {
		_ = checkpoint.close()
		return nil, err
	}
	return checkpoint, nil
}

func (c *daemonCheckpoint) close() error {
	return errors.Join(c.lock.Unlock(), c.store.Close())
}

func (c *daemonCheckpoint) validate(meta *daemonSessionMeta) error {
	if meta != nil && !meta.Dirty && meta.PatchChecksum != "" && meta.PatchChecksum != c.checksum {
		return fmt.Errorf("recovery_required: saved patch checksum differs from session metadata; preserve the patch and %s", meta.SessionRoot)
	}
	return nil
}

func validDaemonMetaFilename(name string) bool {
	if !strings.HasSuffix(name, ".json") {
		return false
	}
	stem := strings.TrimSuffix(name, ".json")
	sep := strings.LastIndexByte(stem, '-')
	if sep < 1 || sep > 48 || len(stem[sep+1:]) != 16 {
		return false
	}
	if daemonSafeName(stem[:sep]) != stem[:sep] {
		return false
	}
	digest := stem[sep+1:]
	_, err := hex.DecodeString(digest)
	return err == nil && strings.ToLower(digest) == digest
}

func validateDaemonSessionMeta(meta daemonSessionMeta) error {
	if meta.SchemaVersion < 0 || meta.SchemaVersion > daemonMetadataSchema {
		return fmt.Errorf("unsupported session metadata schema %d", meta.SchemaVersion)
	}
	if meta.SessionKey == "" || meta.SessionKey != strings.TrimSpace(meta.SessionKey) || len(meta.SessionKey) > 4096 {
		return fmt.Errorf("invalid session metadata key")
	}
	cfg, err := buildDaemonSessionConfig(daemonFlagValues{
		baseMode: string(meta.Config.BaseMode), baseRef: meta.Config.BaseRef,
		allowDirty: meta.Config.AllowDirty, ignoredMode: string(meta.Config.IgnoredMode),
		ignoredMax: meta.Config.IgnoredMax, applyMode: string(meta.Config.ApplyMode),
	})
	if err != nil {
		return fmt.Errorf("invalid session metadata options: %w", err)
	}
	if cfg != meta.Config {
		return fmt.Errorf("noncanonical session metadata options")
	}
	if meta.LastDurableGeneration > meta.Generation {
		return fmt.Errorf("durable generation exceeds mutation generation")
	}
	if !meta.Dirty && meta.Generation != meta.LastDurableGeneration {
		return fmt.Errorf("clean metadata has an unsaved mutation generation")
	}
	if meta.SchemaVersion == daemonMetadataSchema && meta.SessionRoot != "" && meta.PatchChecksum == "" {
		return fmt.Errorf("session metadata is missing its saved patch checksum")
	}
	if meta.BaseOID != "" {
		if _, err := hex.DecodeString(meta.BaseOID); err != nil || (len(meta.BaseOID) != 40 && len(meta.BaseOID) != 64) {
			return fmt.Errorf("invalid immutable base OID")
		}
	}
	if meta.PatchChecksum != "" {
		if _, err := hex.DecodeString(meta.PatchChecksum); err != nil || len(meta.PatchChecksum) != sha256.Size*2 {
			return fmt.Errorf("invalid patch checksum")
		}
	}
	return nil
}

func daemonPatchChecksum(store *patchio.PatchStore) (string, error) {
	file, err := store.OpenRead()
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	if file != nil {
		defer file.Close()
		n, err := io.Copy(hash, io.LimitReader(file, patchio.MaxPatchBytes+1))
		if err != nil {
			return "", err
		}
		if err := patchio.CheckPatchSize(int(n)); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func syncDaemonDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
