package main

import (
	"bytes"
	"context"
	"fmt"
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
	if err := patchio.ValidatePatchReader(bytes.NewReader(patchData)); err != nil {
		return err
	}
	err := g.ApplyPatch(ctx, applyMode, repoRoot, gitDirForOps, patchData)
	if err == nil {
		return nil
	}
	var filtered bytes.Buffer
	skipped, ferr := patchio.FilterExistingNewFilesReader(bytes.NewReader(patchData), repoRoot, &filtered)
	if ferr != nil {
		return err
	}
	return retryFilteredExistingNewFiles(applyMode, err, skipped, log,
		func() (bool, error) {
			return filtered.Len() == 0, nil
		},
		func() error {
			return g.ApplyPatch(ctx, applyMode, repoRoot, gitDirForOps, filtered.Bytes())
		},
	)
}

func applyPatchStore(ctx context.Context, g model.GitOps, applyMode model.ApplyMode, store *patchio.PatchStore, repoRoot, gitDirForOps, scratchDir string, log *slog.Logger) error {
	file, err := openPatchStoreRead(store, "initial read", true)
	if err != nil {
		return err
	}
	if file == nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() > patchio.MaxPatchBytes {
		return fmt.Errorf("patch exceeds size limit of %d bytes: %d", patchio.MaxPatchBytes, info.Size())
	}
	// Read once into a bounded, private snapshot. Reopening or merely seeking
	// the original file lets an uncooperative writer replace or alter the
	// bytes between validation, apply, and the existing-file fallback.
	data, err := io.ReadAll(io.LimitReader(file, patchio.MaxPatchBytes+1))
	if err != nil {
		return err
	}
	if err := patchio.CheckPatchSize(len(data)); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	if err := patchio.ValidatePatchReader(bytes.NewReader(data)); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err = g.ApplyPatchReader(ctx, applyMode, repoRoot, gitDirForOps, bytes.NewReader(data))
	if err == nil {
		return nil
	}

	var filtered bytes.Buffer
	skipped, ferr := patchio.FilterExistingNewFilesReader(bytes.NewReader(data), repoRoot, &filtered)
	if ferr != nil {
		return ferr
	}
	return retryFilteredExistingNewFiles(applyMode, err, skipped, log,
		func() (bool, error) {
			return filtered.Len() == 0, nil
		},
		func() error {
			return g.ApplyPatchReader(ctx, applyMode, repoRoot, gitDirForOps, bytes.NewReader(filtered.Bytes()))
		},
	)
}

func retryFilteredExistingNewFiles(applyMode model.ApplyMode, applyErr error, skipped []string, log *slog.Logger, filteredEmpty func() (bool, error), retry func() error) error {
	if len(skipped) == 0 {
		return applyErr
	}
	if applyMode != model.ApplyStrict {
		return applyErr
	}
	if !shouldRetryExistingNewFileSkip(applyErr) {
		return applyErr
	}
	log.Info("apply patch: skipping existing new files", "skipped", skipped)
	empty, err := filteredEmpty()
	if err != nil {
		return err
	}
	if empty {
		return nil
	}
	return retry()
}

func shouldRetryExistingNewFileSkip(err error) bool {
	return patchio.IsAlreadyExistsError(err)
}

func openPatchStoreRead(store *patchio.PatchStore, stage string, allowMissing bool) (*os.File, error) {
	file, err := store.OpenRead()
	if err != nil {
		return nil, err
	}
	if file == nil && !allowMissing {
		return nil, fmt.Errorf("patch disappeared during %s", stage)
	}
	return file, nil
}
