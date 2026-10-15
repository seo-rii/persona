package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"

	"persona/internal/model"
)

func daemonBaseMetaPath(gitDir, key string) string {
	return daemonSessionPatchPath(gitDir, key) + ".base.json"
}
func daemonBaseRef(key string) string {
	return fmt.Sprintf("refs/persona/bases/%x", sha256.Sum256([]byte(key)))
}

// Ending an overlay removes its runtime metadata, but not its patch. Keep the
// patch's base association and a Git ref independently of the runtime lifetime.
func (s *daemonState) loadPatchBase(key string, cfg daemonSessionConfig) (string, error) {
	meta, err := readDaemonSessionMeta(daemonBaseMetaPath(s.gitDir, key))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if meta.SessionKey != key || meta.BaseOID == "" {
		return "", fmt.Errorf("invalid patch base metadata")
	}
	if meta.Config.BaseMode != cfg.BaseMode || meta.Config.BaseRef != cfg.BaseRef {
		return "", fmt.Errorf("existing patch has a different base; use a new session key")
	}
	return meta.BaseOID, nil
}
func (s *daemonState) retainPatchBase(ctx context.Context, g model.GitOps, key string, cfg daemonSessionConfig, oid string) error {
	if oid == "" {
		return nil
	} // Test doubles without immutable-base operations.
	if keeper, ok := g.(interface {
		RetainBase(context.Context, string, string) error
	}); ok {
		if err := keeper.RetainBase(ctx, daemonBaseRef(key), oid); err != nil {
			return model.Wrap(model.ExitRepo, "retain patch base", err)
		}
	}
	return writeDaemonSessionMeta(daemonBaseMetaPath(s.gitDir, key), daemonSessionMeta{SessionKey: key, Config: cfg, BaseOID: oid})
}
