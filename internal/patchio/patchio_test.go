package patchio

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"persona/internal/model"

	"golang.org/x/sys/unix"
)

func TestValidatePatchPathsOK(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/foo.txt b/foo.txt",
		"new file mode 100644",
		"index 0000000..e69de29",
		"--- /dev/null",
		"+++ b/foo.txt",
		"@@ -0,0 +1 @@",
		"+hi",
		"",
	}, "\n")

	if err := ValidatePatchPaths([]byte(patch)); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestValidatePatchPathsRejectAbsolute(t *testing.T) {
	patch := "diff --git a//abs/path b//abs/path\n+++ b//abs/path\n"
	if err := ValidatePatchPaths([]byte(patch)); err == nil {
		t.Fatal("expected error for absolute path")
	}
}

func TestValidatePatchPathsRejectDotDot(t *testing.T) {
	patch := "diff --git a/../foo b/../foo\n+++ b/../foo\n"
	if err := ValidatePatchPaths([]byte(patch)); err == nil {
		t.Fatal("expected error for .. path")
	}
}

func TestValidatePatchPathsRejectDot(t *testing.T) {
	patch := "diff --git a/./foo b/./foo\n+++ b/./foo\n"
	if err := ValidatePatchPaths([]byte(patch)); err == nil {
		t.Fatal("expected error for . path")
	}
}

func TestValidatePatchPathsRejectGit(t *testing.T) {
	patch := "diff --git a/.git/config b/.git/config\n+++ b/.git/config\n"
	if err := ValidatePatchPaths([]byte(patch)); err == nil {
		t.Fatal("expected error for .git path")
	}
}

func TestValidatePatchPathsRejectGitCaseInsensitive(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{"uppercase", ".GIT"},
		{"mixed case", ".Git"},
		{"mixed case 2", ".gIt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			patch := fmt.Sprintf("diff --git a/%s/config b/%s/config\n+++ b/%s/config\n", tc.path, tc.path, tc.path)
			if err := ValidatePatchPaths([]byte(patch)); err == nil {
				t.Fatalf("expected error for %s path", tc.path)
			}
		})
	}
}

func TestFilterUntrackedPaths(t *testing.T) {
	paths := []string{"foo", ".git/config", "patch.patch", "dir/file"}
	filtered := FilterUntrackedPaths(paths, []string{".git/"}, []string{"patch.patch"})
	if len(filtered) != 2 {
		t.Fatalf("expected 2 paths, got %v", filtered)
	}
	if filtered[0] != "foo" || filtered[1] != "dir/file" {
		t.Fatalf("unexpected filtered result: %v", filtered)
	}
}

func TestEnsurePatchPathRelative(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})

	path, err := EnsurePatchPath(model.Options{PatchPath: "state.patch"}, tmp, time.Now())
	if err != nil {
		t.Fatalf("ensure patch path: %v", err)
	}
	if !strings.HasPrefix(path, tmp) {
		t.Fatalf("expected absolute path under temp dir, got %s", path)
	}
}

func TestEnsurePatchPathAuto(t *testing.T) {
	gitDir := t.TempDir()
	path, err := EnsurePatchPath(model.Options{}, gitDir, time.Now())
	if err != nil {
		t.Fatalf("ensure patch path: %v", err)
	}
	expectedDir := filepath.Join(gitDir, "persona", "patches")
	if !strings.HasPrefix(path, expectedDir) {
		t.Fatalf("expected path under %s, got %s", expectedDir, path)
	}
	if !strings.HasSuffix(path, ".patch") {
		t.Fatalf("expected .patch suffix, got %s", path)
	}
}

func TestEnsurePatchPathAutoRejectsSymlinkedPersonaDir(t *testing.T) {
	gitDir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(gitDir, "persona")); err != nil {
		t.Fatalf("symlink persona dir: %v", err)
	}

	path, err := EnsurePatchPath(model.Options{}, gitDir, time.Now())
	if err == nil {
		t.Fatalf("expected symlinked auto patch dir to be rejected, got path %q", path)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink error, got %v", err)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatalf("read outside dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no auto patch dir created outside gitDir, got %d entries", len(entries))
	}
}

func TestEnsurePatchPathExplicitRejectsSymlinkedParentDir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	linkDir := filepath.Join(root, "linkdir")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Fatalf("symlink linkdir: %v", err)
	}

	path, err := EnsurePatchPath(model.Options{PatchPath: filepath.Join(linkDir, "state.patch")}, filepath.Join(root, ".git"), time.Now())
	if err == nil {
		t.Fatalf("expected symlinked explicit patch parent to be rejected, got path %q", path)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink error, got %v", err)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatalf("read outside dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no explicit patch path created outside target dir, got %d entries", len(entries))
	}
}

func TestEnsurePatchDirRejectsSymlinkedParentDir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	linkDir := filepath.Join(root, "linkdir")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Fatalf("symlink linkdir: %v", err)
	}

	path, err := EnsurePatchPath(model.Options{PatchDir: linkDir}, filepath.Join(root, ".git"), time.Now())
	if err == nil {
		t.Fatalf("expected symlinked patch dir to be rejected, got path %q", path)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink error, got %v", err)
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatalf("read outside dir: %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("expected no patch dir created outside target dir, got %d entries", len(entries))
	}
}

func TestValidatePatchPathsRenameCopy(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/old.txt b/new.txt",
		"similarity index 100%",
		"rename from old.txt",
		"rename to new.txt",
		"",
	}, "\n")
	if err := ValidatePatchPaths([]byte(patch)); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestValidatePatchPathsRejectQuotedRenameDotDot(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/old.txt b/new.txt",
		"similarity index 100%",
		"rename from \"../old.txt\"",
		"rename to new.txt",
		"",
	}, "\n")
	if err := ValidatePatchPaths([]byte(patch)); err == nil {
		t.Fatal("expected error for quoted rename .. path")
	}
}

func TestValidatePatchPathsRejectQuotedCopyDotGit(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/old.txt b/new.txt",
		"similarity index 100%",
		"copy from \".git/config\"",
		"copy to new.txt",
		"",
	}, "\n")
	if err := ValidatePatchPaths([]byte(patch)); err == nil {
		t.Fatal("expected error for quoted copy .git path")
	}
}

func TestValidatePatchPathsAllowsLeadingSpaceDotDotLiteralInRename(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/src.txt b/src.txt",
		"similarity index 100%",
		"rename from  ../safe.txt",
		"rename to src.txt",
		"",
	}, "\n")
	if err := ValidatePatchPaths([]byte(patch)); err != nil {
		t.Fatalf("expected leading-space literal path to be allowed, got %v", err)
	}
}

func TestValidatePatchPathsQuotedSpaces(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git \"a/dir with space/file.txt\" \"b/dir with space/file.txt\"",
		"new file mode 100644",
		"index 0000000..e69de29",
		"--- /dev/null",
		"+++ \"b/dir with space/file.txt\"",
		"@@ -0,0 +1 @@",
		"+hi",
		"",
	}, "\n")
	if err := ValidatePatchPaths([]byte(patch)); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestValidatePatchPathsQuotedTabEscape(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git \"a/dir\\tfile.txt\" \"b/dir\\tfile.txt\"",
		"new file mode 100644",
		"index 0000000..e69de29",
		"--- /dev/null",
		"+++ \"b/dir\\tfile.txt\"",
		"@@ -0,0 +1 @@",
		"+hi",
		"",
	}, "\n")
	if err := ValidatePatchPaths([]byte(patch)); err != nil {
		t.Fatalf("expected quoted tab escape to validate, got %v", err)
	}
}

func TestValidatePatchPathsQuotedBackslashAndQuote(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git \"a/dir\\\\quote\\\".txt\" \"b/dir\\\\quote\\\".txt\"",
		"new file mode 100644",
		"index 0000000..e69de29",
		"--- /dev/null",
		"+++ \"b/dir\\\\quote\\\".txt\"",
		"@@ -0,0 +1 @@",
		"+hi",
		"",
	}, "\n")
	if err := ValidatePatchPaths([]byte(patch)); err != nil {
		t.Fatalf("expected quoted backslash/quote escape to validate, got %v", err)
	}
}

func TestParsePathTokenUnterminatedQuoteFailsClosed(t *testing.T) {
	if _, _, ok := parsePathToken("\"a/unterminated"); ok {
		t.Fatal("expected unterminated quote to fail closed")
	}
}

func TestValidatePatchPathsLargeLineNearScannerLimit(t *testing.T) {
	maxToken := MaxPatchBytes
	nameLen := (maxToken - len("diff --git a/ b/\n")) / 2
	name := strings.Repeat("a", nameLen)
	patch := fmt.Sprintf("diff --git a/%s b/%s\n", name, name)
	if len(patch) >= maxToken {
		t.Fatalf("patch line must stay below scanner max, got %d", len(patch))
	}
	if err := ValidatePatchPaths([]byte(patch)); err != nil {
		t.Fatalf("expected near-limit patch line to validate, got %v", err)
	}
}

func TestValidatePatchPathsRejectsPatchOverSizeLimit(t *testing.T) {
	oversized := bytes.Repeat([]byte("a"), MaxPatchBytes+1)

	err := ValidatePatchPaths(oversized)
	if err == nil {
		t.Fatal("expected oversize patch to be rejected")
	}
	if !strings.Contains(err.Error(), "patch exceeds size limit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFilterExistingNewFilesQuotedSpaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dir with space")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	existingPath := filepath.Join(path, "file.txt")
	if err := os.WriteFile(existingPath, []byte("hi\n"), 0o644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	patch := strings.Join([]string{
		"diff --git \"a/dir with space/file.txt\" \"b/dir with space/file.txt\"",
		"new file mode 100644",
		"index 0000000..e69de29",
		"--- /dev/null",
		"+++ \"b/dir with space/file.txt\"",
		"@@ -0,0 +1 @@",
		"+hi",
		"",
	}, "\n")
	filtered, skipped, err := FilterExistingNewFiles([]byte(patch), dir)
	if err != nil {
		t.Fatalf("filter existing new files: %v", err)
	}
	if len(skipped) != 1 || skipped[0] != "dir with space/file.txt" {
		t.Fatalf("expected quoted path skipped, got %v", skipped)
	}
	if len(filtered) != 0 {
		t.Fatalf("expected filtered patch empty")
	}
}

func TestFilterExistingNewFilesQuotedTrailingQuote(t *testing.T) {
	dir := t.TempDir()
	existingPath := filepath.Join(dir, "quote\"")
	if err := os.WriteFile(existingPath, []byte("quoted\n"), 0o644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	patch := strings.Join([]string{
		"diff --git \"a/quote\\\"\" \"b/quote\\\"\"",
		"new file mode 100644",
		"index 0000000..c7dc1e6",
		"--- /dev/null",
		"+++ \"b/quote\\\"\"",
		"@@ -0,0 +1 @@",
		"+quoted",
		"",
	}, "\n")

	filtered, skipped, err := FilterExistingNewFiles([]byte(patch), dir)
	if err != nil {
		t.Fatalf("filter existing new files: %v", err)
	}
	if len(skipped) != 1 || skipped[0] != "quote\"" {
		t.Fatalf("expected quoted trailing-quote path skipped, got %v", skipped)
	}
	if len(filtered) != 0 {
		t.Fatalf("expected filtered patch empty, got %q", string(filtered))
	}
}

func TestAtomicWriteFileAtPreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.patch")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	dh, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer dh.Close()
	if err := AtomicWriteFileAt(dh, "state.patch", []byte("new")); err != nil {
		t.Fatalf("atomic write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 600 got %o", info.Mode().Perm())
	}
}

func TestAtomicWriteFileAtNoTempFileLeftOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.patch")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	dh, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer dh.Close()
	if err := AtomicWriteFileAt(dh, "state.patch", []byte("new")); err != nil {
		t.Fatalf("atomic write: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".persona-") {
			t.Fatalf("temp file %q left behind after successful write", entry.Name())
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("expected content %q got %q", "new", string(data))
	}
}

func TestAtomicWriteFileAtCleansTempOnFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
	})
	dh, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer dh.Close()
	_ = AtomicWriteFileAt(dh, "state.patch", []byte("new"))
	_ = os.Chmod(dir, 0o755)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".persona-") {
			t.Fatalf("temp file %q left behind after failed write", entry.Name())
		}
	}
}

func TestAtomicWriteFileAtReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.patch")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
	})
	dh, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer dh.Close()
	if err := AtomicWriteFileAt(dh, "state.patch", []byte("new")); err == nil {
		t.Fatalf("expected error for read-only dir")
	}
}

func TestAtomicWriteFileAtChownFailureIsSurfaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.patch")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	dh, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer dh.Close()

	prevFchown := fchownFn
	fchownFn = func(int, int, int) error {
		return unix.EPERM
	}
	defer func() {
		fchownFn = prevFchown
	}()

	err = AtomicWriteFileAt(dh, "state.patch", []byte("new"))
	if err == nil {
		t.Fatal("expected chown failure to be surfaced")
	}
	if !strings.Contains(err.Error(), "preserve owner") {
		t.Fatalf("unexpected error: %v", err)
	}

	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read original file: %v", readErr)
	}
	if string(data) != "old" {
		t.Fatalf("expected original content preserved, got %q", string(data))
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".persona-") {
			t.Fatalf("temp file %q left behind after chown failure", entry.Name())
		}
	}
}

func TestAtomicWriteFileAtRejectsPatchOverSizeLimit(t *testing.T) {
	dir := t.TempDir()
	dh, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer dh.Close()

	err = AtomicWriteFileAt(dh, "state.patch", bytes.Repeat([]byte("a"), MaxPatchBytes+1))
	if err == nil {
		t.Fatal("expected oversize atomic write to fail")
	}
	if !strings.Contains(err.Error(), "patch exceeds size limit") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "state.patch")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected oversize atomic write to avoid creating patch file, got %v", statErr)
	}
}

func TestPatchStoreLockReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
	})
	store, err := OpenPatchStore(filepath.Join(dir, "state.patch"))
	if err != nil {
		t.Fatalf("open patch store: %v", err)
	}
	defer store.Close()
	_, err = store.Lock()
	if err == nil {
		t.Fatalf("expected error for read-only dir")
	}
}

func TestPatchStoreLockBlocksAcrossAtomicRename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.patch")

	store1, err := OpenPatchStore(path)
	if err != nil {
		t.Fatalf("open patch store 1: %v", err)
	}
	defer store1.Close()
	lock1, err := store1.Lock()
	if err != nil {
		t.Fatalf("lock1: %v", err)
	}
	defer lock1.Unlock()

	dh, err := os.Open(dir)
	if err != nil {
		t.Fatalf("open dir: %v", err)
	}
	defer dh.Close()
	if err := AtomicWriteFileAt(dh, "state.patch", []byte("new")); err != nil {
		t.Fatalf("atomic write: %v", err)
	}

	lockedCh := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		store2, err := OpenPatchStore(path)
		if err != nil {
			errCh <- err
			return
		}
		defer store2.Close()
		lock2, err := store2.Lock()
		if err != nil {
			errCh <- err
			return
		}
		defer lock2.Unlock()
		close(lockedCh)
	}()

	select {
	case err := <-errCh:
		t.Fatalf("lock2: %v", err)
	case <-lockedCh:
		t.Fatalf("expected second lock to stay blocked until first unlock")
	case <-time.After(150 * time.Millisecond):
	}

	if err := lock1.Unlock(); err != nil {
		t.Fatalf("unlock1: %v", err)
	}
	lock1 = nil

	select {
	case err := <-errCh:
		t.Fatalf("lock2 after unlock: %v", err)
	case <-lockedCh:
	case <-time.After(time.Second):
		t.Fatalf("expected second lock after first unlock")
	}
}

func TestPatchStoreStaysOnOpenedDirectoryAcrossPathMove(t *testing.T) {
	root := t.TempDir()
	originalDir := filepath.Join(root, "patches")
	if err := os.MkdirAll(originalDir, 0o755); err != nil {
		t.Fatalf("mkdir original dir: %v", err)
	}
	path := filepath.Join(originalDir, "state.patch")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("write original patch: %v", err)
	}

	store, err := OpenPatchStore(path)
	if err != nil {
		t.Fatalf("open patch store: %v", err)
	}
	defer store.Close()

	lock, err := store.Lock()
	if err != nil {
		t.Fatalf("lock patch store: %v", err)
	}
	defer lock.Unlock()

	movedDir := filepath.Join(root, "patches-moved")
	if err := os.Rename(originalDir, movedDir); err != nil {
		t.Fatalf("rename original dir: %v", err)
	}
	if err := os.MkdirAll(originalDir, 0o755); err != nil {
		t.Fatalf("recreate original dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(originalDir, "state.patch"), []byte("other"), 0o644); err != nil {
		t.Fatalf("write replacement patch: %v", err)
	}

	data, err := store.ReadAll()
	if err != nil {
		t.Fatalf("store read: %v", err)
	}
	if string(data) != "old" {
		t.Fatalf("expected store to read original content, got %q", string(data))
	}

	if err := store.WriteAll([]byte("new")); err != nil {
		t.Fatalf("store write: %v", err)
	}

	movedData, err := os.ReadFile(filepath.Join(movedDir, "state.patch"))
	if err != nil {
		t.Fatalf("read moved patch: %v", err)
	}
	if string(movedData) != "new" {
		t.Fatalf("expected moved patch to be updated, got %q", string(movedData))
	}

	replacementData, err := os.ReadFile(filepath.Join(originalDir, "state.patch"))
	if err != nil {
		t.Fatalf("read replacement patch: %v", err)
	}
	if string(replacementData) != "other" {
		t.Fatalf("expected replacement patch untouched, got %q", string(replacementData))
	}

	if _, err := os.Stat(filepath.Join(movedDir, "state.patch.lock")); err != nil {
		t.Fatalf("expected moved lock file to remain with original directory: %v", err)
	}
}

func TestFilterExistingNewFiles(t *testing.T) {
	dir := t.TempDir()
	existingPath := filepath.Join(dir, "test.patch")
	existingContent := strings.Join([]string{
		"diff --git a/test.txt b/test.txt",
		"new file mode 100644",
		"index 0000000..6ed281c",
		"--- /dev/null",
		"+++ b/test.txt",
		"@@ -0,0 +1,2 @@",
		"+1",
		"+1",
		"",
	}, "\n")
	if err := os.WriteFile(existingPath, []byte(existingContent), 0o755); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	patch := strings.Join([]string{
		"diff --git a/test.patch b/test.patch",
		"new file mode 100755",
		"index 0000000..1568bcc",
		"--- /dev/null",
		"+++ b/test.patch",
		"@@ -0,0 +1,8 @@",
		"+diff --git a/test.txt b/test.txt",
		"+new file mode 100644",
		"+index 0000000..6ed281c",
		"+--- /dev/null",
		"++++ b/test.txt",
		"+@@ -0,0 +1,2 @@",
		"++1",
		"++1",
		"",
	}, "\n")
	filtered, skipped, err := FilterExistingNewFiles([]byte(patch), dir)
	if err != nil {
		t.Fatalf("filter existing new files: %v", err)
	}
	if len(skipped) != 1 || skipped[0] != "test.patch" {
		t.Fatalf("expected test.patch to be skipped, got %v", skipped)
	}
	if len(filtered) != 0 {
		t.Fatalf("expected filtered patch to be empty")
	}
}

func TestFilterExistingNewFilesMismatchContent(t *testing.T) {
	dir := t.TempDir()
	existingPath := filepath.Join(dir, "foo.txt")
	if err := os.WriteFile(existingPath, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	patch := strings.Join([]string{
		"diff --git a/foo.txt b/foo.txt",
		"new file mode 100644",
		"index 0000000..e69de29",
		"--- /dev/null",
		"+++ b/foo.txt",
		"@@ -0,0 +1 @@",
		"+bye",
		"",
	}, "\n")
	filtered, skipped, err := FilterExistingNewFiles([]byte(patch), dir)
	if err != nil {
		t.Fatalf("filter existing new files: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("expected no skips, got %v", skipped)
	}
	if string(filtered) != patch {
		t.Fatalf("expected patch unchanged")
	}
}

func TestFilterExistingNewFilesMismatchMode(t *testing.T) {
	dir := t.TempDir()
	existingPath := filepath.Join(dir, "foo.txt")
	if err := os.WriteFile(existingPath, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	patch := strings.Join([]string{
		"diff --git a/foo.txt b/foo.txt",
		"new file mode 100755",
		"index 0000000..e69de29",
		"--- /dev/null",
		"+++ b/foo.txt",
		"@@ -0,0 +1 @@",
		"+hello",
		"",
	}, "\n")
	filtered, skipped, err := FilterExistingNewFiles([]byte(patch), dir)
	if err != nil {
		t.Fatalf("filter existing new files: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("expected no skips, got %v", skipped)
	}
	if string(filtered) != patch {
		t.Fatalf("expected patch unchanged")
	}
}

func TestFilterExistingNewFilesMultipleBlocks(t *testing.T) {
	dir := t.TempDir()
	existingPath := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(existingPath, []byte("keep\n"), 0o644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	patch := strings.Join([]string{
		"diff --git a/keep.txt b/keep.txt",
		"new file mode 100644",
		"index 0000000..e69de29",
		"--- /dev/null",
		"+++ b/keep.txt",
		"@@ -0,0 +1 @@",
		"+keep",
		"",
		"diff --git a/fresh.txt b/fresh.txt",
		"new file mode 100644",
		"index 0000000..e69de29",
		"--- /dev/null",
		"+++ b/fresh.txt",
		"@@ -0,0 +1 @@",
		"+fresh",
		"",
	}, "\n")
	filtered, skipped, err := FilterExistingNewFiles([]byte(patch), dir)
	if err != nil {
		t.Fatalf("filter existing new files: %v", err)
	}
	if len(skipped) != 1 || skipped[0] != "keep.txt" {
		t.Fatalf("expected keep.txt skipped, got %v", skipped)
	}
	if !strings.Contains(string(filtered), "fresh.txt") {
		t.Fatalf("expected filtered patch to include fresh.txt")
	}
	if strings.Contains(string(filtered), "keep.txt") {
		t.Fatalf("expected filtered patch to exclude keep.txt")
	}
}

func TestFilterExistingNewFilesNoFinalNewline(t *testing.T) {
	dir := t.TempDir()
	existingPath := filepath.Join(dir, "nonl.txt")
	if err := os.WriteFile(existingPath, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	patch := strings.Join([]string{
		"diff --git a/nonl.txt b/nonl.txt",
		"new file mode 100644",
		"index 0000000..e69de29",
		"--- /dev/null",
		"+++ b/nonl.txt",
		"@@ -0,0 +1 @@",
		"+hello",
		"\\ No newline at end of file",
		"",
	}, "\n")
	filtered, skipped, err := FilterExistingNewFiles([]byte(patch), dir)
	if err != nil {
		t.Fatalf("filter existing new files: %v", err)
	}
	if len(skipped) != 1 || skipped[0] != "nonl.txt" {
		t.Fatalf("expected nonl.txt skipped, got %v", skipped)
	}
	if len(filtered) != 0 {
		t.Fatalf("expected filtered patch to be empty")
	}
}

func TestFilterExistingNewFilesBinaryNotSkipped(t *testing.T) {
	dir := t.TempDir()
	existingPath := filepath.Join(dir, "bin.dat")
	if err := os.WriteFile(existingPath, []byte{0x00, 0x01}, 0o644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	patch := strings.Join([]string{
		"diff --git a/bin.dat b/bin.dat",
		"new file mode 100644",
		"index 0000000..1111111",
		"--- /dev/null",
		"+++ b/bin.dat",
		"GIT binary patch",
		"literal 2",
		"AAE=",
		"",
	}, "\n")
	filtered, skipped, err := FilterExistingNewFiles([]byte(patch), dir)
	if err != nil {
		t.Fatalf("filter existing new files: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("expected no skips, got %v", skipped)
	}
	if string(filtered) != patch {
		t.Fatalf("expected binary patch unchanged")
	}
}

func TestFilterExistingNewFilesSymlinkNotSkipped(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write target file: %v", err)
	}
	link := filepath.Join(dir, "foo.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	patch := strings.Join([]string{
		"diff --git a/foo.txt b/foo.txt",
		"new file mode 100644",
		"index 0000000..e69de29",
		"--- /dev/null",
		"+++ b/foo.txt",
		"@@ -0,0 +1 @@",
		"+hello",
		"",
	}, "\n")

	filtered, skipped, err := FilterExistingNewFiles([]byte(patch), dir)
	if err != nil {
		t.Fatalf("filter existing new files: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("expected symlink path not to be skipped, got %v", skipped)
	}
	if string(filtered) != patch {
		t.Fatalf("expected patch to stay unchanged for symlink collision")
	}
}

func TestValidatePatchPathsRejectEscapedDotDot(t *testing.T) {
	patch := "diff --git a/\\056\\056/evil b/\\056\\056/evil\n+++ b/\\056\\056/evil\n"
	if err := ValidatePatchPaths([]byte(patch)); err == nil {
		t.Fatal("expected error for escaped .. path")
	}
}

func TestValidatePatchPathsRejectEscapedAbsolute(t *testing.T) {
	patch := "diff --git a/\\057tmp/evil b/\\057tmp/evil\n+++ b/\\057tmp/evil\n"
	if err := ValidatePatchPaths([]byte(patch)); err == nil {
		t.Fatal("expected error for escaped absolute path")
	}
}

func TestValidatePatchPathsRejectEscapedDotGit(t *testing.T) {
	patch := "diff --git a/\\056git/config b/\\056git/config\n+++ b/\\056git/config\n"
	if err := ValidatePatchPaths([]byte(patch)); err == nil {
		t.Fatal("expected error for escaped .git path")
	}
}

func TestValidatePatchPathsRejectEscapedRenameDotDot(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/a.txt b/b.txt",
		"rename from \\056\\056/evil.txt",
		"rename to b.txt",
		"",
	}, "\n")
	if err := ValidatePatchPaths([]byte(patch)); err == nil {
		t.Fatal("expected error for escaped rename .. path")
	}
}

func TestValidatePatchPathsRejectEscapedCopyDotGit(t *testing.T) {
	patch := strings.Join([]string{
		"diff --git a/a.txt b/b.txt",
		"copy from \\056git/config",
		"copy to b.txt",
		"",
	}, "\n")
	if err := ValidatePatchPaths([]byte(patch)); err == nil {
		t.Fatal("expected error for escaped copy .git path")
	}
}

func TestPatchStoreReadAllMissingReturnsNil(t *testing.T) {
	store, err := OpenPatchStore(filepath.Join(t.TempDir(), "missing.patch"))
	if err != nil {
		t.Fatalf("open patch store: %v", err)
	}
	defer store.Close()
	data, err := store.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if data != nil {
		t.Fatalf("expected nil data for missing file, got %q", string(data))
	}
}

func TestPatchStoreReadAllRejectsPatchOverSizeLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.patch")
	if err := os.WriteFile(path, bytes.Repeat([]byte("a"), MaxPatchBytes+1), 0o644); err != nil {
		t.Fatalf("write patch: %v", err)
	}

	store, err := OpenPatchStore(path)
	if err != nil {
		t.Fatalf("open patch store: %v", err)
	}
	defer store.Close()
	data, err := store.ReadAll()
	if err == nil {
		t.Fatal("expected oversize patch read to fail")
	}
	if data != nil {
		t.Fatalf("expected nil data on oversize failure, got %d bytes", len(data))
	}
	if !strings.Contains(err.Error(), "patch exceeds size limit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestPatchStoreWriteAllRejectsPatchOverSizeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.patch")
	store, err := OpenPatchStore(path)
	if err != nil {
		t.Fatalf("open patch store: %v", err)
	}
	defer store.Close()

	err = store.WriteAll(bytes.Repeat([]byte("a"), MaxPatchBytes+1))
	if err == nil {
		t.Fatal("expected oversize patch write to fail")
	}
	if !strings.Contains(err.Error(), "patch exceeds size limit") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("expected oversize write to avoid creating patch file, got %v", statErr)
	}
}

func TestPatchLockUnlockNilSafe(t *testing.T) {
	var nilLock *PatchLock
	if err := nilLock.Unlock(); err != nil {
		t.Fatalf("expected nil lock unlock to succeed: %v", err)
	}

	empty := &PatchLock{}
	if err := empty.Unlock(); err != nil {
		t.Fatalf("expected empty lock unlock to succeed: %v", err)
	}
}

func TestFilterUntrackedPathsPrefixBoundary(t *testing.T) {
	paths := []string{"foo", "foo/bar", "foobar", "bar/foo"}
	filtered := FilterUntrackedPaths(paths, []string{"foo/"}, nil)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 paths got %v", filtered)
	}
	if filtered[0] != "foobar" || filtered[1] != "bar/foo" {
		t.Fatalf("unexpected filtered paths: %v", filtered)
	}
}

func TestFilterExistingNewFilesFallbackPlusPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fallback.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	patch := strings.Join([]string{
		"diff --git",
		"new file mode 100644",
		"index 0000000..e69de29",
		"--- /dev/null",
		"+++ b/fallback.txt",
		"@@ -0,0 +1 @@",
		"+data",
		"",
	}, "\n")
	filtered, skipped, err := FilterExistingNewFiles([]byte(patch), dir)
	if err != nil {
		t.Fatalf("filter existing new files: %v", err)
	}
	if len(skipped) != 1 || skipped[0] != "fallback.txt" {
		t.Fatalf("expected fallback.txt skipped, got %v", skipped)
	}
	if len(filtered) != 0 {
		t.Fatalf("expected filtered patch to be empty")
	}
}

func TestFilterExistingNewFilesRejectsPatchOverSizeLimit(t *testing.T) {
	oversized := bytes.Repeat([]byte("a"), MaxPatchBytes+1)

	filtered, skipped, err := FilterExistingNewFiles(oversized, t.TempDir())
	if err == nil {
		t.Fatal("expected oversize patch filter to fail")
	}
	if filtered != nil {
		t.Fatalf("expected nil filtered patch on oversize failure, got %d bytes", len(filtered))
	}
	if len(skipped) != 0 {
		t.Fatalf("expected no skipped paths on oversize failure, got %v", skipped)
	}
	if !strings.Contains(err.Error(), "patch exceeds size limit") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestFilterExistingNewFilesPreHeaderLinesDiscarded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatalf("write existing file: %v", err)
	}
	// Patch with leading comment lines before the first diff header.
	// When the new-file block is skipped (identical content), the pre-header
	// lines must also be discarded — they don't belong to any diff block.
	patch := strings.Join([]string{
		"# some comment",
		"# another comment",
		"diff --git a/existing.txt b/existing.txt",
		"new file mode 100644",
		"index 0000000..e69de29",
		"--- /dev/null",
		"+++ b/existing.txt",
		"@@ -0,0 +1 @@",
		"+data",
		"",
	}, "\n")
	filtered, skipped, err := FilterExistingNewFiles([]byte(patch), dir)
	if err != nil {
		t.Fatalf("filter existing new files: %v", err)
	}
	if len(skipped) != 1 || skipped[0] != "existing.txt" {
		t.Fatalf("expected existing.txt skipped, got %v", skipped)
	}
	if len(filtered) != 0 {
		t.Fatalf("expected filtered patch to be empty, got %q", string(filtered))
	}
}

func TestParsePatchBlocksIgnoresPreHeaderLines(t *testing.T) {
	lines := splitLinesKeepEOL(strings.Join([]string{
		"# leading comment",
		"some preamble text",
		"diff --git a/foo.txt b/foo.txt",
		"new file mode 100644",
		"--- /dev/null",
		"+++ b/foo.txt",
		"@@ -0,0 +1 @@",
		"+hello",
		"",
	}, "\n"))
	blocks := parsePatchBlocks(lines)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].path != "foo.txt" {
		t.Fatalf("expected path foo.txt, got %q", blocks[0].path)
	}
}
