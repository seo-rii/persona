package patchio

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"persona/internal/model"
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

func TestLockPatchReadOnlyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
	})
	_, err := LockPatch(filepath.Join(dir, "state.patch"))
	if err == nil {
		t.Fatalf("expected error for read-only dir")
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

func TestAtomicWriteFilePreservesMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.patch")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := AtomicWriteFile(path, []byte("new")); err != nil {
		t.Fatalf("atomic write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("unexpected content %q", string(data))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("expected mode 600 got %o", info.Mode().Perm())
	}
}

func TestReadAllMissingReturnsNil(t *testing.T) {
	data, err := ReadAll(filepath.Join(t.TempDir(), "missing.patch"))
	if err != nil {
		t.Fatalf("ReadAll error: %v", err)
	}
	if data != nil {
		t.Fatalf("expected nil data for missing file, got %q", string(data))
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
