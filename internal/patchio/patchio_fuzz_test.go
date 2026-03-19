package patchio

import (
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
