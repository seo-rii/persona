package patchio

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func FuzzValidatePatchPaths(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("diff --git a/plain.txt b/plain.txt\n--- a/plain.txt\n+++ b/plain.txt\n"),
		[]byte("diff --git \"a/dir with space/file.txt\" \"b/dir with space/file.txt\"\n+++ \"b/dir with space/file.txt\"\n"),
		[]byte("diff --git a/a.txt b/b.txt\nrename from old.txt\nrename to new.txt\n"),
		[]byte("diff --git a/a.txt b/b.txt\ncopy from old.txt\ncopy to /dev/null\n"),
		[]byte("diff --git \"a/unterminated b/x\"\n"),
		[]byte("diff --git a/bin.dat b/bin.dat\nGIT binary patch\nliteral 2\nAAE=\n"),
		[]byte("diff --git a/" + strings.Repeat("a", 1<<20) + " b/" + strings.Repeat("a", 1<<20) + "\n"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, patch []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("ValidatePatchPaths panicked: %v", r)
			}
		}()
		_ = ValidatePatchPaths(patch)
	})
}

func FuzzParsePathToken(f *testing.F) {
	for _, seed := range []string{
		`a/plain.txt`,
		`"a/dir with space/file.txt"`,
		`"a/unterminated`,
		`"a/dir\\quote\".txt"`,
		`/dev/null`,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, token string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("parsePathToken panicked: %v", r)
			}
		}()
		_, _, _ = parsePathToken(token)
	})
}

func FuzzFilterExistingNewFiles(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("diff --git a/new.txt b/new.txt\nnew file mode 100644\n--- /dev/null\n+++ b/new.txt\n@@ -0,0 +1 @@\n+hello\n"),
		[]byte("diff --git a/bin.dat b/bin.dat\nnew file mode 100644\n--- /dev/null\n+++ b/bin.dat\nGIT binary patch\nliteral 2\nAAE=\n"),
		[]byte("diff --git \"a/dir with space/file.txt\" \"b/dir with space/file.txt\"\nnew file mode 100644\n--- /dev/null\n+++ \"b/dir with space/file.txt\"\n@@ -0,0 +1 @@\n+hello\n"),
		[]byte("diff --git a/../evil.txt b/../evil.txt\n"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, patch []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("FilterExistingNewFiles panicked: %v", r)
			}
		}()
		_, _, _ = FilterExistingNewFiles(patch, t.TempDir())
	})
}

// Generate valid patches with Git, then verify bytes/mode after application and
// that skipping an identical new text file preserves the same resulting tree.
func FuzzGitPatchRoundTrip(f *testing.F) {
	f.Add([]byte("++ /absolute-content\r\n-- ../content\nno final newline"), false, uint8(0))
	f.Add([]byte{0, 1, 255, 0, '\n'}, true, uint8(1))
	f.Add([]byte{}, false, uint8(2))
	f.Add([]byte("diff --git a/example b/example\n+++ /content\n"), true, uint8(3))
	f.Fuzz(func(t *testing.T, content []byte, executable bool, nameIndex uint8) {
		if len(content) > 4096 {
			t.Skip()
		}
		names := []string{"plain.txt", "a/b/문서 with spaces.txt", "b/a/tab\tquote\"back\\line\n.txt", "empty or patch.txt"}
		name := names[int(nameIndex)%len(names)]
		root := t.TempDir()
		source, restored := filepath.Join(root, "source"), filepath.Join(root, "restored")
		if err := os.MkdirAll(filepath.Dir(filepath.Join(source, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(restored, 0o700); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if executable {
			mode = 0o755
		}
		path := filepath.Join(source, name)
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		sentinel := filepath.Join(root, "sentinel")
		if err := os.WriteFile(sentinel, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit := func(dir string, input []byte, args ...string) ([]byte, error) {
			cmd := exec.Command("git", args...)
			cmd.Dir = dir
			cmd.Stdin = bytes.NewReader(input)
			for _, item := range os.Environ() {
				if !strings.HasPrefix(item, "GIT_") {
					cmd.Env = append(cmd.Env, item)
				}
			}
			cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
			return cmd.Output()
		}
		patch, err := runGit(source, nil, "diff", "--no-index", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", "--no-color", "--src-prefix=a/", "--dst-prefix=b/", "--unified=3", "--", "/dev/null", name)
		var exitErr *exec.ExitError
		if err != nil && (!errors.As(err, &exitErr) || exitErr.ExitCode() != 1) {
			t.Fatalf("generate patch: %v", err)
		}
		if err := ValidatePatchReader(bytes.NewReader(patch)); err != nil {
			t.Fatalf("valid Git patch rejected: %v\n%s", err, patch)
		}
		if _, err := runGit(restored, patch, "apply", "--binary", "--"); err != nil {
			t.Fatalf("apply generated patch: %v\n%s", err, patch)
		}
		got, err := os.ReadFile(filepath.Join(restored, name))
		if err != nil || !bytes.Equal(got, content) {
			t.Fatalf("round-trip bytes: got=%q want=%q err=%v", got, content, err)
		}
		if info, err := os.Stat(filepath.Join(restored, name)); err != nil || info.Mode().Perm() != mode {
			t.Fatalf("round-trip mode: info=%v want=%o err=%v", info, mode, err)
		}
		var filtered bytes.Buffer
		skipped, err := FilterExistingNewFilesReader(bytes.NewReader(patch), restored, &filtered)
		if err != nil {
			t.Fatal(err)
		}
		if len(skipped) > 0 && (len(skipped) != 1 || skipped[0] != name || filtered.Len() != 0) {
			t.Fatalf("incorrect identical-file filtering: skipped=%q output=%q", skipped, filtered.Bytes())
		}
		if !bytes.Contains(patch, []byte("GIT binary patch")) && len(skipped) != 1 {
			t.Fatalf("identical text/empty file was not skipped: name=%q skipped=%q\n%s", name, skipped, patch)
		}
		if len(skipped) == 0 && !bytes.Equal(filtered.Bytes(), patch) {
			t.Fatal("filter changed a patch without skipping a file")
		}
		if got, err := os.ReadFile(sentinel); err != nil || string(got) != "preserve" {
			t.Fatalf("outside sentinel changed: %q %v", got, err)
		}
	})
}
