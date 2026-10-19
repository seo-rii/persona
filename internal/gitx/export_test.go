package gitx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"persona/internal/model"
	"persona/internal/patchio"
	"persona/internal/testutil"
)

func exportAll(t *testing.T, g *Git, view string) []byte {
	t.Helper()
	ctx := context.Background()
	viewGit, release, err := g.SnapshotForExport(ctx, view, g.GitDir)
	if err != nil {
		t.Fatalf("snapshot export index: %v", err)
	}
	defer release()
	var patch bytes.Buffer
	if err := viewGit.DiffHeadBinaryTo(ctx, view, g.GitDir, nil, &patch); err != nil {
		t.Fatalf("export tracked: %v", err)
	}
	paths, err := viewGit.ListUntracked(ctx, view, g.GitDir)
	if err != nil {
		t.Fatalf("list untracked: %v", err)
	}
	for _, path := range paths {
		if err := viewGit.DiffNewFileNoIndexTo(ctx, view, g.GitDir, path, &patch); err != nil {
			t.Fatalf("export new %q: %v", path, err)
		}
	}
	if err := patchio.ValidatePatchReader(bytes.NewReader(patch.Bytes())); err != nil {
		t.Fatalf("validate exported patch: %v\n%s", err, patch.Bytes())
	}
	return patch.Bytes()
}

func fileState(t *testing.T, root string) map[string]string {
	t.Helper()
	state := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			state[rel] = "symlink:" + target
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		state[rel] = fmt.Sprintf("executable=%t:%x", info.Mode().Perm()&0o111 != 0, sha256.Sum256(data))
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return state
}

func gitText(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func isolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestStoragePatchRoundTripIgnoresDisplayConfig(t *testing.T) {
	configs := []struct{ name, key, value string }{
		{"textconv", "diff.trap.textconv", "printf identical"},
		{"external-driver", "diff.trap.command", "printf 'NOT A PATCH'"},
		{"external-default", "diff.external", "printf 'NOT A PATCH'"},
		{"no-prefix", "diff.noprefix", "true"},
		{"mnemonic-prefix", "diff.mnemonicPrefix", "true"},
		{"relative", "diff.relative", "true"},
		{"zero-context", "diff.context", "0"},
		{"suppress-empty-context", "diff.suppressBlankEmpty", "true"},
		{"forced-color", "color.ui", "always"},
		{"case-insensitive", "core.ignorecase", "true"},
		{"autocrlf", "core.autocrlf", "true"},
		{"filemode-disabled", "core.filemode", "false"},
		{"symlinks-disabled", "core.symlinks", "false"},
		{"submodule-log", "diff.submodule", "log"},
	}
	for _, config := range configs {
		t.Run(config.name, func(t *testing.T) {
			isolateGitConfig(t)
			repo := testutil.InitRepo(t)
			testutil.WriteFile(t, filepath.Join(repo, ".gitattributes"), "*.bin diff=trap\n")
			testutil.WriteFile(t, filepath.Join(repo, "data.bin"), "before\x00binary\xff\n")
			testutil.WriteFile(t, filepath.Join(repo, "remove.txt"), "remove\n")
			testutil.WriteFile(t, filepath.Join(repo, "mode.sh"), "#!/bin/sh\n")
			testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "base\n\nkeep\n")
			if err := os.Symlink("tracked.txt", filepath.Join(repo, "link")); err != nil {
				t.Fatal(err)
			}
			testutil.RunCmd(t, repo, "git", "add", ".")
			testutil.RunCmd(t, repo, "git", "commit", "-m", "round trip base")
			g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
			if _, err := g.ResolveBase(context.Background(), "HEAD"); err != nil {
				t.Fatal(err)
			}
			testutil.RunCmd(t, repo, "git", "config", config.key, config.value)
			restored := filepath.Join(t.TempDir(), "restored")
			if err := g.WorktreeAddDetach(context.Background(), restored, g.BaseOID); err != nil {
				t.Fatal(err)
			}
			testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "changed\n\nkeep\nsecond\nthird\nfourth\n")
			testutil.WriteFile(t, filepath.Join(repo, "TRACKED.txt"), "distinct case-sensitive file\n")
			testutil.WriteFile(t, filepath.Join(repo, "data.bin"), "after\x00binary\xfe\r\n")
			testutil.WriteFile(t, filepath.Join(repo, "new.bin"), "new\x00binary\x80\n")
			testutil.WriteFile(t, filepath.Join(repo, "한국어 space.txt"), "CRLF\r\nlast line")
			testutil.WriteFile(t, filepath.Join(repo, "empty.txt"), "")
			if err := os.Chmod(filepath.Join(repo, "mode.sh"), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{"remove.txt", "link"} {
				if err := os.Remove(filepath.Join(repo, path)); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink("data.bin", filepath.Join(repo, "link")); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"tab\tname.txt", "line\nbreak.txt", "quote\"back\\slash.txt", "a/nested.txt", "b/nested.txt", " leading.txt", "trailing.txt ", "a/ leading b/file "} {
				testutil.WriteFile(t, filepath.Join(repo, name), "++ /absolute-is-content\r\n-- /also-content")
			}
			indexBefore, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
			if err != nil {
				t.Fatal(err)
			}
			patch := exportAll(t, g, repo)
			if err := g.ApplyPatch(context.Background(), model.ApplyStrict, restored, g.GitDir, patch); err != nil {
				t.Fatalf("apply storage patch: %v\n%s", err, patch)
			}
			if got, want := fileState(t, restored), fileState(t, repo); !reflect.DeepEqual(got, want) {
				t.Fatalf("round trip changed contents, links or executable bits:\ngot  %v\nwant %v", got, want)
			}
			indexAfter, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
			if err != nil || !bytes.Equal(indexBefore, indexAfter) {
				t.Fatalf("export modified original index: %v", err)
			}
		})
	}
}

func TestPinnedBaseIgnoresMovingHEADAndIndex(t *testing.T) {
	isolateGitConfig(t)
	repo := testutil.InitRepo(t)
	first := gitText(t, repo, "rev-parse", "HEAD")
	testutil.RunCmd(t, repo, "git", "tag", "base-tag")
	testutil.RunCmd(t, repo, "git", "branch", "base-branch")
	testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "second commit\n")
	testutil.WriteFile(t, filepath.Join(repo, "only-in-second.txt"), "second\n")
	testutil.RunCmd(t, repo, "git", "add", ".")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "second")
	second := gitText(t, repo, "rev-parse", "HEAD")
	for _, ref := range []string{"HEAD", "HEAD~1", "base-tag", "base-branch"} {
		t.Run(ref, func(t *testing.T) {
			g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
			oid, err := g.ResolveBase(context.Background(), ref)
			if err != nil {
				t.Fatal(err)
			}
			view := filepath.Join(t.TempDir(), "view")
			if err := g.WorktreeAddDetach(context.Background(), view, oid); err != nil {
				t.Fatal(err)
			}
			if patch := exportAll(t, g, view); len(patch) != 0 {
				t.Fatalf("unmodified base exported a patch:\n%s", patch)
			}
			testutil.RunCmd(t, repo, "git", "reset", "--hard", first)
			if patch := exportAll(t, g, view); len(patch) != 0 {
				t.Fatalf("moving original HEAD/index changed session export:\n%s", patch)
			}
			if again, err := g.ResolveBase(context.Background(), "HEAD"); err != nil || again != oid {
				t.Fatalf("base changed: %s -> %s (%v)", oid, again, err)
			}
			restored := filepath.Join(t.TempDir(), "restored")
			if err := g.WorktreeAddDetach(context.Background(), restored, oid); err != nil {
				t.Fatal(err)
			}
			testutil.WriteFile(t, filepath.Join(view, "tracked.txt"), "session change\r\n")
			testutil.WriteFile(t, filepath.Join(view, "new.bin"), "\x00new\xff")
			patch := exportAll(t, g, view)
			if err := g.ApplyPatch(context.Background(), model.ApplyStrict, restored, g.GitDir, patch); err != nil {
				t.Fatal(err)
			}
			if got, want := fileState(t, restored), fileState(t, view); !reflect.DeepEqual(got, want) {
				t.Fatalf("pinned base round trip mismatch: got %v want %v", got, want)
			}
		})
		// Restore the second commit so HEAD~1 remains valid for each case.
		testutil.RunCmd(t, repo, "git", "reset", "--hard", second)
	}
}

func TestUnbornRepositoryRoundTripPreservesStagedFiles(t *testing.T) {
	for _, kind := range []string{"empty", "staged", "staged-and-untracked", "orphan-with-other-refs"} {
		t.Run(kind, func(t *testing.T) {
			isolateGitConfig(t)
			repo := testutil.InitEmptyRepo(t)
			if kind == "orphan-with-other-refs" {
				testutil.WriteFile(t, filepath.Join(repo, "old.txt"), "old\n")
				testutil.RunCmd(t, repo, "git", "add", ".")
				testutil.RunCmd(t, repo, "git", "commit", "-m", "old ref")
				testutil.RunCmd(t, repo, "git", "tag", "other-tag")
				testutil.RunCmd(t, repo, "git", "checkout", "--orphan", "new-orphan")
				testutil.RunCmd(t, repo, "git", "rm", "-rf", ".")
			}
			if kind != "empty" {
				testutil.WriteFile(t, filepath.Join(repo, "staged.bin"), "staged\x00bytes\xff")
				testutil.RunCmd(t, repo, "git", "add", "staged.bin")
				// Preserve the final worktree bytes rather than the staged blob.
				testutil.WriteFile(t, filepath.Join(repo, "staged.bin"), "edited after staging\x00\xfe")
			}
			if kind == "staged-and-untracked" || kind == "orphan-with-other-refs" {
				testutil.WriteFile(t, filepath.Join(repo, "untracked.txt"), "untracked\r\n")
			}
			g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
			if _, err := g.ResolveBase(context.Background(), "HEAD"); err != nil {
				t.Fatal(err)
			}
			patch := exportAll(t, g, repo)
			restored := t.TempDir()
			if len(patch) > 0 {
				if err := g.ApplyPatch(context.Background(), model.ApplyStrict, restored, g.GitDir, patch); err != nil {
					t.Fatalf("apply unborn patch: %v\n%s", err, patch)
				}
			}
			if got, want := fileState(t, restored), fileState(t, repo); !reflect.DeepEqual(got, want) {
				t.Fatalf("unborn round trip mismatch: got %v want %v", got, want)
			}
		})
	}
}

func TestBoundedGitErrorOutput(t *testing.T) {
	var output boundedOutput
	if _, bypassesWrite := any(&output).(io.ReaderFrom); bypassesWrite {
		t.Fatal("io.Copy can bypass the output bound")
	}
	input := bytes.Repeat([]byte("x"), 2<<20)
	if n, err := output.Write(input); n != len(input) || err != nil {
		t.Fatalf("write did not consume child output: %d %v", n, err)
	}
	if output.Len() != maxErrOutput+1 {
		t.Fatalf("unbounded error allocation: %d", output.Len())
	}
	_, _ = output.Write([]byte("already exists in "))
	_, _ = output.Write([]byte("working directory"))
	if !output.alreadyExists || output.Len() != maxErrOutput+1 {
		t.Fatal("bounded output lost an apply conflict after truncation")
	}
}

func TestStoragePatchRejectsChangedContentTransformations(t *testing.T) {
	for _, attr := range []string{"text", "text=auto", "eol=crlf", "working-tree-encoding=UTF-8", "filter=trap", "filter=", "ident"} {
		t.Run(attr, func(t *testing.T) {
			isolateGitConfig(t)
			repo := testutil.InitRepo(t)
			g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
			if _, err := g.ResolveBase(context.Background(), "HEAD"); err != nil {
				t.Fatal(err)
			}
			// info/attributes avoids changing the baseline or needing a clean
			// conversion before the preservation check itself can run.
			testutil.WriteFile(t, filepath.Join(repo, ".git", "info", "attributes"), "*.txt "+attr+"\n")
			if _, err := g.DiffHeadBinary(context.Background(), repo, g.GitDir, nil); err != nil {
				t.Fatalf("unchanged raw blob was rejected: %v", err)
			}
			testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "changed\r\n")
			if _, err := g.DiffHeadBinary(context.Background(), repo, g.GitDir, nil); err == nil || !strings.Contains(err.Error(), "content transformation attribute") {
				t.Fatalf("changed transformed content did not fail clearly: %v", err)
			}
			if patch, err := g.DiffHeadBinary(context.Background(), repo, g.GitDir, []string{"tracked.txt"}); err != nil || len(patch) != 0 {
				t.Fatalf("excluded transformed file blocked export: %v\n%s", err, patch)
			}
			testutil.WriteFile(t, filepath.Join(repo, "new.txt"), "new\r\n")
			if _, err := g.DiffNewFileNoIndex(context.Background(), repo, g.GitDir, "new.txt"); err == nil || !strings.Contains(err.Error(), "content transformation attribute") {
				t.Fatalf("new transformed content did not fail clearly: %v", err)
			}
		})
	}
}

func TestStoragePatchRejectsNewNormalizationOfUnchangedRawBlob(t *testing.T) {
	isolateGitConfig(t)
	repo := testutil.InitRepo(t)
	testutil.WriteFile(t, filepath.Join(repo, "tracked.txt"), "committed CRLF\r\n")
	testutil.RunCmd(t, repo, "git", "add", "tracked.txt")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "raw CRLF base")
	g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	if _, err := g.ResolveBase(context.Background(), "HEAD"); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(repo, ".git", "info", "attributes"), "tracked.txt text\n")
	if _, err := g.DiffHeadBinary(context.Background(), repo, g.GitDir, nil); err == nil || !strings.Contains(err.Error(), "content transformation attribute") {
		t.Fatalf("new normalization of unchanged raw blob was not rejected: %v", err)
	}
}

func TestStorageAttributesUsePinnedIndexWhenWorktreeAttributesAreDeleted(t *testing.T) {
	isolateGitConfig(t)
	repo := testutil.InitRepo(t)
	testutil.WriteFile(t, filepath.Join(repo, ".gitattributes"), "tracked.txt text\n")
	testutil.RunCmd(t, repo, "git", "add", ".")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "base attributes")
	g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	if _, err := g.ResolveBase(context.Background(), "HEAD"); err != nil {
		t.Fatal(err)
	}
	view := filepath.Join(t.TempDir(), "view")
	if err := g.WorktreeAddDetach(context.Background(), view, g.BaseOID); err != nil {
		t.Fatal(err)
	}
	// Removing attributes from the source checkout must not change the index
	// fallback used to inspect transformations for the pinned session's diff.
	testutil.RunCmd(t, repo, "git", "rm", ".gitattributes")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "source drops attributes")
	if err := os.Remove(filepath.Join(view, ".gitattributes")); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(view, "tracked.txt"), "changed CRLF\r\n")
	if patch, err := g.DiffHeadBinary(context.Background(), view, g.GitDir, nil); err == nil || !strings.Contains(err.Error(), "content transformation attribute") {
		t.Fatalf("pinned index normalization was not rejected: err=%v patch=%q", err, patch)
	}
}

func TestPinnedBaseIgnoresSourceIndexPathTypeChanges(t *testing.T) {
	for _, kind := range []string{"file", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			isolateGitConfig(t)
			repo := testutil.InitRepo(t)
			baseName, sourceName := "shape", "shape/child.txt"
			if kind == "directory" {
				baseName, sourceName = sourceName, baseName
			}
			if kind == "symlink" {
				testutil.WriteFile(t, filepath.Join(repo, "target-dir/child.txt"), "pinned contents\n")
				if err := os.Symlink("target-dir", filepath.Join(repo, baseName)); err != nil {
					t.Fatal(err)
				}
			} else {
				testutil.WriteFile(t, filepath.Join(repo, baseName), "pinned contents\n")
			}
			testutil.RunCmd(t, repo, "git", "add", ".")
			testutil.RunCmd(t, repo, "git", "commit", "-m", "pinned path shape")
			g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
			if _, err := g.ResolveBase(context.Background(), "HEAD"); err != nil {
				t.Fatal(err)
			}
			view := filepath.Join(t.TempDir(), "view")
			if err := g.WorktreeAddDetach(context.Background(), view, g.BaseOID); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(repo, "shape")); err != nil {
				t.Fatal(err)
			}
			testutil.WriteFile(t, filepath.Join(repo, sourceName), "source-only shape\n")
			testutil.RunCmd(t, repo, "git", "add", "-A")
			if patch := exportAll(t, g, view); len(patch) != 0 {
				t.Fatalf("source index path type change altered the pinned session:\n%s", patch)
			}
		})
	}
}

func TestStoragePatchAllowsDeletionOfTransformedFileByDirectoryReplacement(t *testing.T) {
	isolateGitConfig(t)
	repo := testutil.InitRepo(t)
	testutil.WriteFile(t, filepath.Join(repo, ".gitattributes"), "dir/child.txt text\n")
	testutil.WriteFile(t, filepath.Join(repo, "dir/child.txt"), "base\n")
	testutil.RunCmd(t, repo, "git", "add", ".")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "transformed file base")
	g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	if _, err := g.ResolveBase(context.Background(), "HEAD"); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := g.WorktreeAddDetach(context.Background(), restored, g.BaseOID); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(repo, "dir")); err != nil {
		t.Fatal(err)
	}
	testutil.WriteFile(t, filepath.Join(repo, "dir"), "ordinary replacement\r\n")
	patch := exportAll(t, g, repo)
	if err := g.ApplyPatch(context.Background(), model.ApplyStrict, restored, g.GitDir, patch); err != nil {
		t.Fatalf("apply directory replacement: %v\n%s", err, patch)
	}
	if got, want := fileState(t, restored), fileState(t, repo); !reflect.DeepEqual(got, want) {
		t.Fatalf("directory replacement round trip mismatch:\ngot  %v\nwant %v", got, want)
	}
}

func TestStoragePatchRoundTripPreservesTypeTransitionsAndRenames(t *testing.T) {
	isolateGitConfig(t)
	repo := testutil.InitRepo(t)
	for _, name := range []string{"file-to-dir", "dir-to-file/child", "regular-to-link", "rename source.txt"} {
		testutil.WriteFile(t, filepath.Join(repo, name), "before "+name+"\n")
	}
	if err := os.Symlink("tracked.txt", filepath.Join(repo, "link-to-regular")); err != nil {
		t.Fatal(err)
	}
	testutil.RunCmd(t, repo, "git", "add", ".")
	testutil.RunCmd(t, repo, "git", "commit", "-m", "type transition base")
	g := &Git{RepoRoot: repo, GitDir: filepath.Join(repo, ".git")}
	if _, err := g.ResolveBase(context.Background(), "HEAD"); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
	if err := g.WorktreeAddDetach(context.Background(), restored, g.BaseOID); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"file-to-dir", "dir-to-file", "regular-to-link", "link-to-regular"} {
		if err := os.RemoveAll(filepath.Join(repo, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"file-to-dir/child", "dir-to-file", "link-to-regular"} {
		testutil.WriteFile(t, filepath.Join(repo, name), "after "+name+"\r\n")
	}
	if err := os.Symlink("file-to-dir/child", filepath.Join(repo, "regular-to-link")); err != nil {
		t.Fatal(err)
	}
	renamed := filepath.Join(repo, " a renamed b", "destination ")
	if err := os.MkdirAll(filepath.Dir(renamed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(repo, "rename source.txt"), renamed); err != nil {
		t.Fatal(err)
	}
	testutil.RunCmd(t, repo, "git", "add", "-A")
	patch := exportAll(t, g, repo)
	if !bytes.Contains(patch, []byte("rename from rename source.txt\n")) {
		t.Fatalf("fixture did not exercise Git rename headers:\n%s", patch)
	}
	if err := g.ApplyPatch(context.Background(), model.ApplyStrict, restored, g.GitDir, patch); err != nil {
		t.Fatalf("apply type changes: %v\n%s", err, patch)
	}
	if got, want := fileState(t, restored), fileState(t, repo); !reflect.DeepEqual(got, want) {
		t.Fatalf("type transition round trip mismatch:\ngot  %v\nwant %v", got, want)
	}
}
