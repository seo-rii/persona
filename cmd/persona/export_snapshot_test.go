package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"persona/internal/gitx"
	"persona/internal/model"
	"persona/internal/testutil"
)

// Preserve the scheduling hook when the exporter switches to the real Git
// snapshot, so the index transition occurs between its two export phases.
type indexTransitionExportGit struct {
	model.GitOps
	beforeUntracked func()
}

func (g indexTransitionExportGit) SnapshotForExport(ctx context.Context, workTree, gitDir string) (model.GitOps, func(), error) {
	snapshot, release, err := g.GitOps.(model.GitExportSnapshotter).SnapshotForExport(ctx, workTree, gitDir)
	if err != nil {
		return nil, nil, err
	}
	return indexTransitionExportGit{GitOps: snapshot, beforeUntracked: g.beforeUntracked}, release, nil
}

func (g indexTransitionExportGit) ListUntracked(ctx context.Context, workTree, gitDir string) ([]string, error) {
	g.beforeUntracked()
	return g.GitOps.ListUntracked(ctx, workTree, gitDir)
}

func TestExportPatchSnapshotPreservesConcurrentIndexChanges(t *testing.T) {
	for _, change := range []string{"stage", "unstage"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			ctx := context.Background()
			repo := testutil.InitRepo(t)
			g := &gitx.Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
			g.SetBaseMode(model.BaseRepo)
			if _, err := g.ResolveBase(ctx, "HEAD"); err != nil {
				t.Fatal(err)
			}
			restored := filepath.Join(t.TempDir(), "restored")
			if err := g.WorktreeAddDetach(ctx, restored, g.BaseOID); err != nil {
				t.Fatal(err)
			}
			testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "modified tracked contents\n")
			testutil.WriteFile(t, filepath.Join(repo, "session.txt"), "unsaved session contents\r\nlast line")
			if change == "unstage" {
				testutil.RunCmd(t, repo, "git", "add", "session.txt")
			}
			transitions := 0
			interleaved := indexTransitionExportGit{GitOps: g, beforeUntracked: func() {
				transitions++
				if change == "stage" {
					testutil.RunCmd(t, repo, "git", "add", "session.txt")
				} else {
					testutil.RunCmd(t, repo, "git", "reset", "HEAD", "--", "session.txt")
				}
			}}
			var patch bytes.Buffer
			if _, err := exportPatchToWriter(ctx, interleaved, repo, g.GitDir, false, "", 200, nil, &patch); err != nil {
				t.Fatal(err)
			}
			if transitions != 1 {
				t.Fatalf("export did not exercise index transition: got %d calls", transitions)
			}
			if err := applyPatchData(ctx, g, model.ApplyStrict, patch.Bytes(), restored, g.GitDir, slog.Default()); err != nil {
				t.Fatalf("apply saved patch: %v\n%s", err, patch.Bytes())
			}
			for _, path := range []string{"tracked.txt", "session.txt"} {
				want, err := os.ReadFile(filepath.Join(repo, path))
				if err != nil {
					t.Fatal(err)
				}
				got, err := os.ReadFile(filepath.Join(restored, path))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("%s changed after %s during export: got %q want %q", path, change, got, want)
				}
			}
		})
	}
}
