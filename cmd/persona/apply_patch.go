package main

import (
	"context"
	"io"
	"log/slog"
	"os"

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

func applyPatchStore(ctx context.Context, g model.GitOps, applyMode model.ApplyMode, store *patchio.PatchStore, repoRoot, gitDirForOps, scratchDir string, log *slog.Logger) error {
	file, err := store.OpenRead()
	if err != nil || file == nil {
		return err
	}
	info, statErr := file.Stat()
	_ = file.Close()
	if statErr == nil && info.Size() == 0 {
		return nil
	}

	file, err = store.OpenRead()
	if err != nil || file == nil {
		return err
	}
	err = patchio.ValidatePatchReader(file)
	_ = file.Close()
	if err != nil {
		return err
	}

	file, err = store.OpenRead()
	if err != nil || file == nil {
		return err
	}
	err = g.ApplyPatchReader(ctx, applyMode, repoRoot, gitDirForOps, file)
	_ = file.Close()
	if err == nil {
		return nil
	}

	filtered, ferr := os.CreateTemp(scratchDir, "persona-filter-*.patch")
	if ferr != nil {
		return ferr
	}
	defer closeAndRemoveTempFile(filtered)

	file, ferr = store.OpenRead()
	if ferr != nil || file == nil {
		return err
	}
	skipped, ferr := patchio.FilterExistingNewFilesReader(file, repoRoot, filtered)
	_ = file.Close()
	if ferr != nil || len(skipped) == 0 {
		return err
	}
	if applyMode != model.ApplyStrict {
		return err
	}
	log.Info("apply patch: skipping existing new files", "skipped", skipped)
	if _, seekErr := filtered.Seek(0, io.SeekStart); seekErr != nil {
		return seekErr
	}
	info, statErr = filtered.Stat()
	if statErr != nil {
		return statErr
	}
	if info.Size() == 0 {
		return nil
	}
	if !patchio.IsAlreadyExistsError(err) {
		return err
	}
	if err2 := g.ApplyPatchReader(ctx, applyMode, repoRoot, gitDirForOps, filtered); err2 != nil {
		return err2
	}
	return nil
}
