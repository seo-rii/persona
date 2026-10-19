package gitx

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"persona/internal/model"
	"persona/internal/patchio"
	"persona/internal/testutil"
)

func TestExportSnapshotPreservesSourceIndexTransitions(t *testing.T) {
	for _, mode := range []model.BaseMode{model.BaseRepo, model.BaseWorktree} {
		for _, initiallyStaged := range []bool{false, true} {
			name := string(mode) + "/stage"
			if initiallyStaged {
				name = string(mode) + "/unstage"
			}
			t.Run(name, func(t *testing.T) {
				isolateGitConfig(t)
				ctx := context.Background()
				repo := testutil.InitRepo(t)
				g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
				g.SetBaseMode(mode)
				if _, err := g.ResolveBase(ctx, "HEAD"); err != nil {
					t.Fatal(err)
				}
				view := repo
				if mode == model.BaseWorktree {
					view = filepath.Join(t.TempDir(), "view")
					if err := g.WorktreeAddDetach(ctx, view, g.BaseOID); err != nil {
						t.Fatal(err)
					}
				}
				restored := filepath.Join(t.TempDir(), "restored")
				if err := g.WorktreeAddDetach(ctx, restored, g.BaseOID); err != nil {
					t.Fatal(err)
				}
				testutil.WriteFile(t, filepath.Join(repo, "session.txt"), "source contents\n")
				if initiallyStaged {
					testutil.RunCmd(t, repo, "git", "add", "session.txt")
				}
				testutil.WriteFile(t, filepath.Join(view, "session.txt"), "session contents\n")
				viewGit, release, err := g.SnapshotForExport(ctx, view, g.GitDir)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				var patch bytes.Buffer
				if err := viewGit.DiffHeadBinaryTo(ctx, view, g.GitDir, nil, &patch); err != nil {
					t.Fatal(err)
				}
				// Schedule an ordinary checkout index update exactly between the
				// tracked and untracked export phases.
				if initiallyStaged {
					testutil.RunCmd(t, repo, "git", "reset", "HEAD", "--", "session.txt")
				} else {
					testutil.RunCmd(t, repo, "git", "add", "session.txt")
				}
				untracked, err := viewGit.ListUntracked(ctx, view, g.GitDir)
				if err != nil {
					t.Fatal(err)
				}
				for _, path := range untracked {
					if err := viewGit.DiffNewFileNoIndexTo(ctx, view, g.GitDir, path, &patch); err != nil {
						t.Fatal(err)
					}
				}
				if err := patchio.ValidatePatchReader(bytes.NewReader(patch.Bytes())); err != nil {
					t.Fatal(err)
				}
				if err := g.ApplyPatch(ctx, model.ApplyStrict, restored, g.GitDir, patch.Bytes()); err != nil {
					t.Fatalf("apply snapshot export: %v\n%s", err, patch.Bytes())
				}
				if got, want := fileState(t, restored), fileState(t, view); !reflect.DeepEqual(got, want) {
					t.Fatalf("source index update changed stored state: got %v want %v", got, want)
				}
			})
		}
	}
}

func TestPinnedIgnoredPolicyIgnoresSourceIndex(t *testing.T) {
	isolateGitConfig(t)
	ctx := context.Background()
	repo := testutil.InitRepo(t)
	testutil.WriteFile(t, filepath.Join(repo, ".gitignore"), "tracked.txt\nartifact.txt\n")
	testutil.RunCmd(t, repo, "git", "add", ".")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "ignored patterns")
	g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	g.SetBaseMode(model.BaseWorktree)
	if _, err := g.ResolveBase(ctx, "HEAD"); err != nil {
		t.Fatal(err)
	}
	view := filepath.Join(t.TempDir(), "view")
	if err := g.WorktreeAddDetach(ctx, view, g.BaseOID); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(view, "artifact.txt"), "ignored view output\n")
	testutil.WriteFile(t, filepath.Join(repo, "artifact.txt"), "source-only addition\n")
	testutil.RunCmd(t, repo, "git", "config", "status.showUntrackedFiles", "no")
	for _, sourceChange := range []string{"unchanged", "remove-tracked", "stage-ignored"} {
		switch sourceChange {
		case "remove-tracked":
			testutil.RunCmd(t, repo, "git", "rm", "--cached", "tracked.txt")
		case "stage-ignored":
			testutil.RunCmd(t, repo, "git", "add", "-f", "artifact.txt")
		}
		ignored, err := g.ListIgnoredCandidates(ctx, view, g.GitDir, 200)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(ignored, []string{"artifact.txt"}) {
			t.Fatalf("%s changed pinned ignored paths: %v", sourceChange, ignored)
		}
		if patch := exportAll(t, g, view); len(patch) != 0 {
			t.Fatalf("%s included ignored view output:\n%s", sourceChange, patch)
		}
	}
}

func TestRepoSnapshotPreservesForceStagedIgnoredFiles(t *testing.T) {
	for _, unborn := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "unborn"}[unborn], func(t *testing.T) {
			isolateGitConfig(t)
			repo := testutil.InitEmptyRepo(t)
			testutil.WriteFile(t, filepath.Join(repo, ".gitignore"), "staged.txt\n")
			testutil.RunCmd(t, repo, "git", "add", ".gitignore")
			if !unborn {
				testutil.RunCmd(t, repo, "git", "commit", "-m", "ignore staged file")
			}
			testutil.WriteFile(t, filepath.Join(repo, "staged.txt"), "staged\n")
			testutil.RunCmd(t, repo, "git", "add", "-f", "staged.txt")
			testutil.WriteFile(t, filepath.Join(repo, "staged.txt"), "edited since staging\r\n")
			g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
			g.SetBaseMode(model.BaseRepo)
			if _, err := g.ResolveBase(context.Background(), "HEAD"); err != nil {
				t.Fatal(err)
			}
			ignored, err := g.ListIgnoredCandidates(context.Background(), repo, g.GitDir, 200)
			if err != nil || len(ignored) != 0 {
				t.Fatalf("force-staged file became ignored: %v %v", ignored, err)
			}
			patch := exportAll(t, g, repo)
			restored := t.TempDir()
			if !unborn {
				restored = filepath.Join(restored, "view")
				if err := g.WorktreeAddDetach(context.Background(), restored, g.BaseOID); err != nil {
					t.Fatal(err)
				}
			}
			if err := g.ApplyPatch(context.Background(), model.ApplyStrict, restored, g.GitDir, patch); err != nil {
				t.Fatal(err)
			}
			if got, want := fileState(t, restored), fileState(t, repo); !reflect.DeepEqual(got, want) {
				t.Fatalf("staged ignored file lost: got %v want %v", got, want)
			}
		})
	}
}

func TestExportSnapshotIgnoredScanPreservesRawCRLFChange(t *testing.T) {
	isolateGitConfig(t)
	ctx := context.Background()
	repo := testutil.InitRepo(t)
	g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	if _, err := g.ResolveBase(ctx, "HEAD"); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := g.WorktreeAddDetach(ctx, restored, g.BaseOID); err != nil {
		t.Fatal(err)
	}
	testutil.RunCmd(t, repo, "git", "config", "core.autocrlf", "true")
	testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "base\r\n")
	viewGit, release, err := g.SnapshotForExport(ctx, repo, g.GitDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// Ignored discovery precedes diff in the real exporter. It must not
	// refresh the shared index using content-normalizing user configuration.
	if _, err := viewGit.ListIgnoredCandidates(ctx, repo, g.GitDir, 200); err != nil {
		t.Fatal(err)
	}
	patch, err := viewGit.DiffHeadBinary(ctx, repo, g.GitDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.ApplyPatch(ctx, model.ApplyStrict, restored, g.GitDir, patch); err != nil {
		t.Fatal(err)
	}
	if got, want := fileState(t, restored), fileState(t, repo); !reflect.DeepEqual(got, want) {
		t.Fatalf("ignored scan lost raw line endings: got %v want %v", got, want)
	}
}
