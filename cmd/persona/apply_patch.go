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
	info, statErr := file.Stat()
	_ = file.Close()
	if statErr == nil && info.Size() == 0 {
		return nil
	}

	file, err = openPatchStoreRead(store, "validation reopen", false)
	if err != nil {
		return err
	}
	err = patchio.ValidatePatchReader(file)
	_ = file.Close()
	if err != nil {
		return err
	}

	file, err = openPatchStoreRead(store, "apply reopen", false)
	if err != nil {
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

	file, ferr = openPatchStoreRead(store, "filter reopen", false)
	if ferr != nil {
		return ferr
	}
	skipped, ferr := patchio.FilterExistingNewFilesReader(file, repoRoot, filtered)
	_ = file.Close()
	if ferr != nil {
		return ferr
	}
	return retryFilteredExistingNewFiles(applyMode, err, skipped, log,
		func() (bool, error) {
			filteredInfo, statErr := filtered.Stat()
			if statErr != nil {
				return false, statErr
			}
			return filteredInfo.Size() == 0, nil
		},
		func() error {
			if _, seekErr := filtered.Seek(0, io.SeekStart); seekErr != nil {
				return seekErr
			}
			return g.ApplyPatchReader(ctx, applyMode, repoRoot, gitDirForOps, filtered)
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
