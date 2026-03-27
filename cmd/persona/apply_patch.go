package main

import (
	"context"
	"log/slog"

	"persona/internal/model"
	"persona/internal/patchio"
)

func applyPatchData(ctx context.Context, g model.GitOps, applyMode model.ApplyMode, patchData []byte, repoRoot, gitDirForOps string, log *slog.Logger) error {
	if len(patchData) == 0 {
		return nil
	}
	if err := patchio.ValidatePatchPaths(patchData); err != nil {
		return err
	}
	err := g.ApplyPatch(ctx, applyMode, repoRoot, gitDirForOps, patchData)
	if err == nil {
		return nil
	}
	filtered, skipped, ferr := patchio.FilterExistingNewFiles(patchData, repoRoot)
	if ferr != nil || len(skipped) == 0 {
		return err
	}
	if applyMode != model.ApplyStrict {
		return err
	}
	log.Info("apply patch: skipping existing new files", "skipped", skipped)
	if len(filtered) == 0 {
		return nil
	}
	if !patchio.IsAlreadyExistsError(err) {
		return err
	}
	if err2 := g.ApplyPatch(ctx, applyMode, repoRoot, gitDirForOps, filtered); err2 != nil {
		return err2
	}
	return nil
}
