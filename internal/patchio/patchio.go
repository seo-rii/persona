package patchio

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"persona/internal/model"
)

type PatchLock struct {
	file *os.File
}

const MaxPatchBytes = 16 * 1024 * 1024

var fchownFn = unix.Fchown

type PatchStore struct {
	dir  *os.File
	name string
}

func CheckPatchSize(size int) error {
	if size <= MaxPatchBytes {
		return nil
	}
	return fmt.Errorf("patch exceeds size limit of %d bytes: %d", MaxPatchBytes, size)
}

func OpenPatchStore(path string) (*PatchStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	return &PatchStore{dir: dir, name: filepath.Base(path)}, nil
}

func (s *PatchStore) Close() error {
	if s == nil || s.dir == nil {
		return nil
	}
	err := s.dir.Close()
	s.dir = nil
	return err
}

func (s *PatchStore) Lock() (*PatchLock, error) {
	if s == nil || s.dir == nil {
		return nil, errors.New("patch store is closed")
	}
	return lockPatchFileAt(s.dir, s.name+".lock")
}

func (s *PatchStore) OpenRead() (*os.File, error) {
	if s == nil || s.dir == nil {
		return nil, errors.New("patch store is closed")
	}
	return openReadAt(s.dir, s.name)
}

func (s *PatchStore) ReadAll() ([]byte, error) {
	file, err := s.OpenRead()
	if err != nil || file == nil {
		return nil, err
	}
	defer file.Close()
	return readAllFile(file)
}

func (s *PatchStore) WriteAll(data []byte) error {
	return s.WriteFromReader(bytes.NewReader(data))
}

func (s *PatchStore) WriteFromReader(reader io.Reader) error {
	if s == nil || s.dir == nil {
		return errors.New("patch store is closed")
	}
	return writeFileAt(s.dir, s.name, reader)
}

func lockPatchFileAt(dir *os.File, name string) (*PatchLock, error) {
	if dir == nil {
		return nil, errors.New("dir is nil")
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_CREAT|unix.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &PatchLock{file: file}, nil
}

func (l *PatchLock) Unlock() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return l.file.Close()
}

func EnsurePatchPath(opts model.Options, gitDir string, now time.Time) (string, error) {
	if strings.TrimSpace(opts.PatchPath) != "" {
		path := opts.PatchPath
		if !filepath.IsAbs(path) {
			abs, err := filepath.Abs(path)
			if err != nil {
				return "", err
			}
			path = abs
		}
		if err := rejectSymlinkPathParents(filepath.Dir(path), "patch path"); err != nil {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		return path, nil
	}

	patchDir := opts.PatchDir
	defaultPatchDir := strings.TrimSpace(patchDir) == ""
	if defaultPatchDir {
		patchDir = filepath.Join(gitDir, "persona", "patches")
	}
	if !filepath.IsAbs(patchDir) {
		abs, err := filepath.Abs(patchDir)
		if err != nil {
			return "", err
		}
		patchDir = abs
	}
	if err := rejectSymlinkPathParents(patchDir, "patch dir"); err != nil {
		return "", err
	}
	if err := os.MkdirAll(patchDir, 0o755); err != nil {
		return "", err
	}
	stamp := now.Format("20060102_150405")
	ms := now.Nanosecond() / int(time.Millisecond)
	rnd, err := randSuffix()
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s_%03d_%d_%s.patch", stamp, ms, os.Getpid(), rnd)
	return filepath.Join(patchDir, name), nil
}

func rejectSymlinkPathParents(path, label string) error {
	path = filepath.Clean(path)
	if path == "." || path == "" {
		return nil
	}
	for current := path; ; {
		info, err := os.Lstat(current)
		if err != nil {
			if !os.IsNotExist(err) {
				return err
			}
		} else if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s parent is symlink: %s", label, current)
		}
		next := filepath.Dir(current)
		if next == current {
			return nil
		}
		current = next
	}
}

func AtomicWriteFileAt(dir *os.File, name string, data []byte) error {
	if dir == nil {
		return errors.New("dir is nil")
	}
	if err := CheckPatchSize(len(data)); err != nil {
		return err
	}
	return writeFileAt(dir, name, bytes.NewReader(data))
}

type atomicTargetInfo struct {
	mode uint32
	uid  int
	gid  int
}

func statAtomicTargetAt(dir *os.File, name string) atomicTargetInfo {
	info := atomicTargetInfo{mode: 0o644, uid: -1, gid: -1}
	var stat unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &stat, 0); err == nil {
		info.mode = stat.Mode & 0o777
		info.uid = int(stat.Uid)
		info.gid = int(stat.Gid)
	}
	return info
}

func createAtomicTempFileAt(dir *os.File, mode uint32) (*os.File, string, error) {
	prefix := ".persona-"
	rnd, err := randSuffix()
	if err != nil {
		return nil, "", err
	}
	tmpName := fmt.Sprintf("%s%s", prefix, rnd)
	fd, err := unix.Openat(int(dir.Fd()), tmpName, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR, mode)
	if err != nil {
		return nil, "", err
	}
	return os.NewFile(uintptr(fd), tmpName), tmpName, nil
}

func commitAtomicTempFileAt(dir *os.File, name string, target atomicTargetInfo, file *os.File, tmpName string) error {
	renamed := false
	defer func() {
		if !renamed {
			file.Close()
			_ = unix.Unlinkat(int(dir.Fd()), tmpName, 0)
		}
	}()
	if err := file.Sync(); err != nil {
		return err
	}
	if target.uid >= 0 || target.gid >= 0 {
		if err := fchownFn(int(file.Fd()), target.uid, target.gid); err != nil {
			return fmt.Errorf("preserve owner for %s: %w", name, err)
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := unix.Renameat(int(dir.Fd()), tmpName, int(dir.Fd()), name); err != nil {
		return err
	}
	renamed = true
	return unix.Fsync(int(dir.Fd()))
}

func writeFileAt(dir *os.File, name string, reader io.Reader) error {
	if dir == nil {
		return errors.New("dir is nil")
	}
	target := statAtomicTargetAt(dir, name)
	file, tmpName, err := createAtomicTempFileAt(dir, target.mode)
	if err != nil {
		return err
	}
	written, err := io.Copy(file, io.LimitReader(reader, MaxPatchBytes+1))
	if err != nil {
		file.Close()
		_ = unix.Unlinkat(int(dir.Fd()), tmpName, 0)
		return err
	}
	if err := CheckPatchSize(int(written)); err != nil {
		file.Close()
		_ = unix.Unlinkat(int(dir.Fd()), tmpName, 0)
		return err
	}
	return commitAtomicTempFileAt(dir, name, target, file, tmpName)
}

func walkPatchLines(reader io.Reader, fn func([]byte) error) error {
	buf := bufio.NewReader(reader)
	size := 0
	for {
		line, err := buf.ReadBytes('\n')
		if len(line) > 0 {
			size += len(line)
			if err := CheckPatchSize(size); err != nil {
				return err
			}
			if err := fn(trimLineBytes(line)); err != nil {
				return err
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func validatePatchPathReader(reader io.Reader) error {
	return walkPatchLines(reader, func(line []byte) error {
		if bytes.HasPrefix(line, []byte("diff --git ")) {
			rest := bytes.TrimLeft(line[len("diff --git "):], " ")
			if len(rest) == 0 {
				return nil
			}
			var left []byte
			if rest[0] == '"' {
				token, next, ok := readQuotedTokenBytes(rest)
				if !ok {
					return nil
				}
				left = token
				rest = next
			} else {
				split := bytes.IndexByte(rest, ' ')
				if split == -1 {
					return nil
				}
				left = rest[:split]
				rest = bytes.TrimLeft(rest[split+1:], " ")
			}
			if len(rest) == 0 {
				return nil
			}
			var right []byte
			if rest[0] == '"' {
				token, _, ok := readQuotedTokenBytes(rest)
				if !ok {
					return nil
				}
				right = token
			} else {
				split := bytes.IndexByte(rest, ' ')
				if split == -1 {
					right = rest
				} else {
					right = rest[:split]
				}
			}
			if err := checkPath(sanitizePatchPath(parseMaybeQuotedPathBytes(left))); err != nil {
				return err
			}
			return checkPath(sanitizePatchPath(parseMaybeQuotedPathBytes(right)))
		}
		if bytes.HasPrefix(line, []byte("+++ ")) || bytes.HasPrefix(line, []byte("--- ")) {
			parsed := parsePatchHeaderPath(line[4:])
			if parsed == "" {
				return nil
			}
			return checkPath(parsed)
		}
		if bytes.HasPrefix(line, []byte("rename from ")) || bytes.HasPrefix(line, []byte("rename to ")) || bytes.HasPrefix(line, []byte("copy from ")) || bytes.HasPrefix(line, []byte("copy to ")) {
			for _, prefix := range [][]byte{[]byte("rename from "), []byte("rename to "), []byte("copy from "), []byte("copy to ")} {
				if bytes.HasPrefix(line, prefix) {
					path := parseMaybeQuotedPathBytes(trimLineBytes(line[len(prefix):]))
					if err := checkPath(path); err != nil {
						return err
					}
					break
				}
			}
		}
		return nil
	})
}

func ValidatePatchPaths(patch []byte) error {
	if err := CheckPatchSize(len(patch)); err != nil {
		return err
	}
	return validatePatchPathReader(bytes.NewReader(patch))
}

func ValidatePatchReader(reader io.Reader) error {
	return validatePatchPathReader(reader)
}

func IsAlreadyExistsError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "already exists in working directory")
}

func FilterExistingNewFiles(patch []byte, workTree string) ([]byte, []string, error) {
	if len(patch) == 0 {
		return patch, nil, nil
	}
	if err := CheckPatchSize(len(patch)); err != nil {
		return nil, nil, err
	}
	var out bytes.Buffer
	skipped, err := FilterExistingNewFilesReader(bytes.NewReader(patch), workTree, &out)
	if err != nil {
		return nil, nil, err
	}
	if len(skipped) == 0 {
		return patch, nil, nil
	}
	return out.Bytes(), skipped, nil
}

func FilterExistingNewFilesReader(reader io.Reader, workTree string, out io.Writer) ([]string, error) {
	if out == nil {
		out = io.Discard
	}
	buf := bufio.NewReader(reader)
	seenDiff := false
	var current patchBlock
	skipped := make([]string, 0)
	size := 0
	flushCurrent := func() error {
		if !seenDiff || len(current.lines) == 0 {
			return nil
		}
		if shouldSkipNewFileBlock(current, workTree) {
			skipped = append(skipped, current.path)
			return nil
		}
		for _, line := range current.lines {
			if _, err := out.Write(line); err != nil {
				return err
			}
		}
		return nil
	}
	for {
		line, err := buf.ReadBytes('\n')
		if len(line) > 0 {
			size += len(line)
			if err := CheckPatchSize(size); err != nil {
				return nil, err
			}
			lineCopy := append([]byte(nil), line...)
			raw := trimLineBytes(lineCopy)
			if bytes.HasPrefix(raw, []byte("diff --git ")) || bytes.Equal(raw, []byte("diff --git")) {
				if seenDiff {
					if err := flushCurrent(); err != nil {
						return nil, err
					}
				}
				seenDiff = true
				current = beginPatchBlock(lineCopy)
			} else if seenDiff {
				appendPatchBlockLine(&current, lineCopy, raw)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if !seenDiff {
		return nil, nil
	}
	if err := flushCurrent(); err != nil {
		return nil, err
	}
	return skipped, nil
}

type patchBlock struct {
	lines    [][]byte
	path     string
	isNew    bool
	isBinary bool
	mode     string
}

func beginPatchBlock(line []byte) patchBlock {
	return patchBlock{lines: [][]byte{line}}
}

func appendPatchBlockLine(block *patchBlock, line []byte, raw []byte) {
	block.lines = append(block.lines, line)
	if bytes.HasPrefix(raw, []byte("new file mode ")) {
		block.isNew = true
		block.mode = string(bytes.TrimSpace(raw[len("new file mode "):]))
	}
	if bytes.HasPrefix(raw, []byte("--- /dev/null")) {
		block.isNew = true
	}
	if bytes.HasPrefix(raw, []byte("GIT binary patch")) || bytes.HasPrefix(raw, []byte("Binary files ")) {
		block.isBinary = true
	}
	if bytes.HasPrefix(raw, []byte("+++ ")) && block.path == "" {
		block.path = parsePatchHeaderPath(raw[4:])
	}
}

func shouldSkipNewFileBlock(block patchBlock, workTree string) bool {
	if !block.isNew || block.isBinary || block.path == "" {
		return false
	}
	path := filepath.Join(workTree, filepath.FromSlash(block.path))
	info, err := os.Lstat(path)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		return false
	}
	if block.mode != "" {
		if perm, ok := parseFileMode(block.mode); ok {
			if info.Mode().Perm() != perm {
				return false
			}
		}
	}
	var expectedSize int64
	inHunk := false
	sawHeader := false
	sawHunk := false
	noFinalNL := false
	for _, line := range block.lines {
		raw := trimLineBytes(line)
		if bytes.HasPrefix(raw, []byte("+++ ")) {
			sawHeader = true
		}
		if bytes.HasPrefix(raw, []byte("@@ ")) {
			inHunk = true
			sawHunk = true
			continue
		}
		if !inHunk {
			continue
		}
		if bytes.Equal(raw, []byte("\\ No newline at end of file")) {
			noFinalNL = true
			continue
		}
		if len(raw) > 0 && raw[0] == '+' {
			expectedSize += int64(len(raw))
		}
	}
	if !sawHunk && !sawHeader {
		return false
	}
	if noFinalNL && expectedSize > 0 {
		expectedSize--
	}
	if info.Size() != expectedSize {
		return false
	}
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	scratch := make([]byte, 32*1024)
	inHunk = false
	remaining := expectedSize
	for _, line := range block.lines {
		raw := trimLineBytes(line)
		if bytes.HasPrefix(raw, []byte("@@ ")) {
			inHunk = true
			continue
		}
		if !inHunk || bytes.Equal(raw, []byte("\\ No newline at end of file")) {
			continue
		}
		if len(raw) == 0 || raw[0] != '+' {
			continue
		}
		content := raw[1:]
		for len(content) > 0 {
			n := len(content)
			if n > len(scratch) {
				n = len(scratch)
			}
			if _, err := io.ReadFull(reader, scratch[:n]); err != nil {
				return false
			}
			if !bytes.Equal(scratch[:n], content[:n]) {
				return false
			}
			content = content[n:]
			remaining -= int64(n)
		}
		if remaining == 0 {
			continue
		}
		b, err := reader.ReadByte()
		if err != nil || b != '\n' {
			return false
		}
		remaining--
	}
	_, err = reader.ReadByte()
	return err == io.EOF && remaining == 0
}

func parsePatchHeaderPath(path []byte) string {
	path = trimLineBytes(path)
	if bytes.Equal(path, []byte("/dev/null")) {
		return ""
	}
	if idx := bytes.IndexByte(path, '\t'); idx != -1 {
		path = path[:idx]
	}
	return sanitizePatchPath(parseMaybeQuotedPathBytes(path))
}

func sanitizePatchPath(path string) string {
	path = strings.TrimPrefix(path, "a/")
	path = strings.TrimPrefix(path, "b/")
	return path
}

func readQuotedTokenBytes(s []byte) ([]byte, []byte, bool) {
	if len(s) == 0 || s[0] != '"' {
		return nil, nil, false
	}
	escaped := false
	for i := 1; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			token := s[:i+1]
			rest := bytes.TrimLeft(s[i+1:], " ")
			return token, rest, true
		}
	}
	return nil, nil, false
}

func parseMaybeQuotedPathBytes(path []byte) string {
	if len(path) == 0 {
		return ""
	}
	if path[0] == '"' {
		token, _, ok := readQuotedTokenBytes(path)
		if ok {
			text := string(token)
			if unquoted, err := strconv.Unquote(text); err == nil {
				return unquoted
			}
			return strings.Trim(text, "\"")
		}
	}
	return unescapeGitPath(string(path))
}

func unescapeGitPath(path string) string {
	if !strings.Contains(path, "\\") {
		return path
	}
	var out strings.Builder
	out.Grow(len(path))
	for i := 0; i < len(path); i++ {
		ch := path[i]
		if ch != '\\' || i+1 >= len(path) {
			out.WriteByte(ch)
			continue
		}
		next := path[i+1]
		switch next {
		case '\\':
			out.WriteByte('\\')
			i++
		case '"':
			out.WriteByte('"')
			i++
		case 't':
			out.WriteByte('\t')
			i++
		case 'n':
			out.WriteByte('\n')
			i++
		case 'r':
			out.WriteByte('\r')
			i++
		case ' ':
			out.WriteByte(' ')
			i++
		default:
			if next >= '0' && next <= '7' {
				val := int(next - '0')
				j := i + 2
				for k := 0; k < 2 && j < len(path); k++ {
					c := path[j]
					if c < '0' || c > '7' {
						break
					}
					val = val*8 + int(c-'0')
					j++
				}
				out.WriteByte(byte(val))
				i = j - 1
			} else {
				out.WriteByte(next)
				i++
			}
		}
	}
	return out.String()
}

func parseFileMode(mode string) (os.FileMode, bool) {
	val, err := strconv.ParseUint(strings.TrimSpace(mode), 8, 32)
	if err != nil {
		return 0, false
	}
	return os.FileMode(val) & 0o777, true
}

func trimLineBytes(line []byte) []byte {
	if n := len(line); n > 0 && line[n-1] == '\n' {
		line = line[:n-1]
	}
	if n := len(line); n > 0 && line[n-1] == '\r' {
		line = line[:n-1]
	}
	return line
}

func FilterUntrackedPaths(paths []string, excludePrefixes []string, excludeExact []string) []string {
	if len(paths) == 0 {
		return nil
	}
	exact := map[string]struct{}{}
	for _, item := range excludeExact {
		if item == "" {
			continue
		}
		exact[item] = struct{}{}
	}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if _, ok := exact[path]; ok {
			continue
		}
		skip := false
		for _, prefix := range excludePrefixes {
			if prefix == "" {
				continue
			}
			if strings.HasPrefix(path, prefix) || path == strings.TrimSuffix(prefix, "/") {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		out = append(out, path)
	}
	return out
}

func randSuffix() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func checkPath(path string) error {
	path = strings.TrimPrefix(path, "a/")
	path = strings.TrimPrefix(path, "b/")
	if path == "" || path == "/dev/null" {
		return nil
	}
	if strings.HasPrefix(path, "/") {
		return fmt.Errorf("absolute path in patch: %s", path)
	}
	for start := 0; start <= len(path); {
		end := start
		for end < len(path) && path[end] != '/' {
			end++
		}
		part := path[start:end]
		if part == "." {
			return fmt.Errorf("current path in patch: %s", path)
		}
		if part == ".." {
			return fmt.Errorf("parent path in patch: %s", path)
		}
		if strings.EqualFold(part, ".git") {
			return fmt.Errorf(".git path in patch: %s", path)
		}
		if end == len(path) {
			break
		}
		start = end + 1
	}
	return nil
}

func openReadAt(dir *os.File, name string) (*os.File, error) {
	if dir == nil {
		return nil, errors.New("dir is nil")
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, nil
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func readAllFile(file *os.File) ([]byte, error) {
	if file == nil {
		return nil, nil
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err == nil {
		if err := CheckPatchSize(int(stat.Size)); err != nil {
			return nil, err
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFREG {
			data := make([]byte, int(stat.Size))
			if _, err := io.ReadFull(file, data); err != nil {
				return nil, err
			}
			return data, nil
		}
	}
	buf := bytes.NewBuffer(nil)
	if _, err := io.Copy(buf, io.LimitReader(file, MaxPatchBytes+1)); err != nil {
		return nil, err
	}
	data := buf.Bytes()
	if err := CheckPatchSize(len(data)); err != nil {
		return nil, err
	}
	return data, nil
}
