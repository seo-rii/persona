package patchio

import (
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

func (s *PatchStore) ReadAll() ([]byte, error) {
	if s == nil || s.dir == nil {
		return nil, errors.New("patch store is closed")
	}
	return readAllAt(s.dir, s.name)
}

func (s *PatchStore) WriteAll(data []byte) error {
	if s == nil || s.dir == nil {
		return errors.New("patch store is closed")
	}
	if err := CheckPatchSize(len(data)); err != nil {
		return err
	}
	return AtomicWriteFileAt(s.dir, s.name, data)
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
	mode := uint32(0o644)
	uid := -1
	gid := -1
	var stat unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &stat, 0); err == nil {
		mode = stat.Mode & 0o777
		uid = int(stat.Uid)
		gid = int(stat.Gid)
	}
	prefix := ".persona-"
	rnd, err := randSuffix()
	if err != nil {
		return err
	}
	tmpName := fmt.Sprintf("%s%s", prefix, rnd)
	fd, err := unix.Openat(int(dir.Fd()), tmpName, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR, mode)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), tmpName)
	renamed := false
	defer func() {
		if !renamed {
			file.Close()
			_ = unix.Unlinkat(int(dir.Fd()), tmpName, 0)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if uid >= 0 || gid >= 0 {
		if err := fchownFn(fd, uid, gid); err != nil {
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

func ValidatePatchPaths(patch []byte) error {
	if err := CheckPatchSize(len(patch)); err != nil {
		return err
	}
	for start := 0; start < len(patch); {
		end := len(patch)
		if next := bytes.IndexByte(patch[start:], '\n'); next != -1 {
			end = start + next + 1
		}
		line := trimLineBytes(patch[start:end])
		start = end
		if bytes.HasPrefix(line, []byte("diff --git ")) {
			text := string(line)
			a, b, ok := parseDiffGitLine(text)
			if ok {
				if err := checkPath(a); err != nil {
					return err
				}
				if err := checkPath(b); err != nil {
					return err
				}
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("+++ ")) || bytes.HasPrefix(line, []byte("--- ")) {
			path := trimLineBytes(line[4:])
			if bytes.Equal(path, []byte("/dev/null")) {
				continue
			}
			parsed := parseMaybeQuotedPath(stripDiffPathMeta(string(path)))
			if err := checkPath(parsed); err != nil {
				return err
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("rename from ")) || bytes.HasPrefix(line, []byte("rename to ")) || bytes.HasPrefix(line, []byte("copy from ")) || bytes.HasPrefix(line, []byte("copy to ")) {
			text := string(line)
			for _, prefix := range []string{"rename from ", "rename to ", "copy from ", "copy to "} {
				if strings.HasPrefix(text, prefix) {
					path := trimLine(strings.TrimPrefix(text, prefix))
					path = parseMaybeQuotedPath(path)
					if err := checkPath(path); err != nil {
						return err
					}
					break
				}
			}
			continue
		}
	}
	return nil
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
	seenDiff := false
	firstDiffStart := -1
	blockStart := -1
	var current patchBlock
	var out bytes.Buffer
	skipped := make([]string, 0)
	flushCurrent := func(nextStart int) {
		if !seenDiff || len(current.lines) == 0 {
			return
		}
		if shouldSkipNewFileBlock(current, workTree) {
			if len(skipped) == 0 {
				out.Grow(len(patch))
				out.Write(patch[firstDiffStart:blockStart])
			}
			skipped = append(skipped, current.path)
			return
		}
		if len(skipped) == 0 {
			return
		}
		out.Write(patch[blockStart:nextStart])
	}
	for start := 0; start < len(patch); {
		end := len(patch)
		if next := bytes.IndexByte(patch[start:], '\n'); next != -1 {
			end = start + next + 1
		}
		line := patch[start:end]
		raw := trimLineBytes(line)
		if bytes.HasPrefix(raw, []byte("diff --git ")) || bytes.Equal(raw, []byte("diff --git")) {
			if seenDiff {
				flushCurrent(start)
			} else {
				seenDiff = true
				firstDiffStart = start
			}
			blockStart = start
			current = patchBlock{lines: [][]byte{line}, path: parseDiffGitPath(string(raw))}
			start = end
			continue
		}
		if !seenDiff {
			start = end
			continue
		}
		current.lines = append(current.lines, line)
		if bytes.HasPrefix(raw, []byte("new file mode ")) {
			current.isNew = true
			current.mode = strings.TrimSpace(strings.TrimPrefix(string(raw), "new file mode "))
		}
		if bytes.HasPrefix(raw, []byte("--- /dev/null")) {
			current.isNew = true
		}
		if bytes.HasPrefix(raw, []byte("GIT binary patch")) || bytes.HasPrefix(raw, []byte("Binary files ")) {
			current.isBinary = true
		}
		if bytes.HasPrefix(raw, []byte("+++ ")) && current.path == "" {
			current.path = parsePlusPath(string(raw))
		}
		start = end
	}
	if !seenDiff {
		return patch, nil, nil
	}
	flushCurrent(len(patch))
	if len(skipped) == 0 {
		return patch, nil, nil
	}
	return out.Bytes(), skipped, nil
}

type patchBlock struct {
	lines    [][]byte
	path     string
	isNew    bool
	isBinary bool
	mode     string
}

func parsePatchBlocks(lines [][]byte) []patchBlock {
	var blocks []patchBlock
	var current patchBlock
	seenDiff := false
	for _, line := range lines {
		raw := trimLineBytes(line)
		if bytes.HasPrefix(raw, []byte("diff --git ")) || bytes.Equal(raw, []byte("diff --git")) {
			if seenDiff && len(current.lines) > 0 {
				blocks = append(blocks, current)
			}
			seenDiff = true
			current = patchBlock{lines: [][]byte{line}, path: parseDiffGitPath(string(raw))}
			continue
		}
		if !seenDiff {
			continue
		}
		current.lines = append(current.lines, line)
		if bytes.HasPrefix(raw, []byte("new file mode ")) {
			current.isNew = true
			current.mode = strings.TrimSpace(strings.TrimPrefix(string(raw), "new file mode "))
		}
		if bytes.HasPrefix(raw, []byte("--- /dev/null")) {
			current.isNew = true
		}
		if bytes.HasPrefix(raw, []byte("GIT binary patch")) || bytes.HasPrefix(raw, []byte("Binary files ")) {
			current.isBinary = true
		}
		if bytes.HasPrefix(raw, []byte("+++ ")) && current.path == "" {
			current.path = parsePlusPath(string(raw))
		}
	}
	if len(current.lines) > 0 {
		blocks = append(blocks, current)
	}
	return blocks
}

func shouldSkipNewFileBlock(block patchBlock, workTree string) bool {
	if !block.isNew || block.isBinary || block.path == "" {
		return false
	}
	content, ok, noFinalNL := extractNewFileContent(block.lines)
	if !ok {
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
	existing, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if noFinalNL {
		return bytes.Equal(existing, content)
	}
	return bytes.Equal(existing, content)
}

func extractNewFileContent(lines [][]byte) ([]byte, bool, bool) {
	var out bytes.Buffer
	inHunk := false
	sawHeader := false
	sawHunk := false
	noFinalNL := false
	for _, line := range lines {
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
			out.Write(raw[1:])
			out.WriteByte('\n')
		}
	}
	if !sawHunk && !sawHeader {
		return nil, false, false
	}
	content := out.Bytes()
	if noFinalNL && bytes.HasSuffix(content, []byte("\n")) {
		content = content[:len(content)-1]
	}
	return content, true, noFinalNL
}

func parseDiffGitPath(line string) string {
	_, b, ok := parseDiffGitLine(line)
	if !ok {
		return ""
	}
	return sanitizePatchPath(b)
}

func parsePlusPath(line string) string {
	path := strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
	if path == "/dev/null" {
		return ""
	}
	path = stripDiffPathMeta(path)
	path = parseMaybeQuotedPath(path)
	return sanitizePatchPath(path)
}

func sanitizePatchPath(path string) string {
	path = strings.TrimPrefix(path, "a/")
	path = strings.TrimPrefix(path, "b/")
	return path
}

func parseDiffGitLine(line string) (string, string, bool) {
	const prefix = "diff --git "
	if !strings.HasPrefix(line, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(line, prefix)
	left, rest, ok := parsePathToken(rest)
	if !ok {
		return "", "", false
	}
	right, _, ok := parsePathToken(rest)
	if !ok {
		return "", "", false
	}
	return sanitizePatchPath(left), sanitizePatchPath(right), true
}

func parsePathToken(input string) (string, string, bool) {
	s := strings.TrimLeft(input, " ")
	if s == "" {
		return "", "", false
	}
	if s[0] == '"' {
		token, rest, ok := readQuotedToken(s)
		if !ok {
			return "", "", false
		}
		unquoted, err := strconv.Unquote(token)
		if err == nil {
			return unquoted, rest, true
		}
		return strings.Trim(token, "\""), rest, true
	}
	end := strings.IndexByte(s, ' ')
	var token string
	if end == -1 {
		token = s
		return unescapeGitPath(token), "", true
	}
	token = s[:end]
	rest := strings.TrimLeft(s[end+1:], " ")
	return unescapeGitPath(token), rest, true
}

func readQuotedToken(s string) (string, string, bool) {
	if len(s) == 0 || s[0] != '"' {
		return "", "", false
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
			rest := strings.TrimLeft(s[i+1:], " ")
			return token, rest, true
		}
	}
	return "", "", false
}

func stripDiffPathMeta(path string) string {
	if idx := strings.IndexRune(path, '\t'); idx != -1 {
		return path[:idx]
	}
	return path
}

func parseMaybeQuotedPath(path string) string {
	if path == "" {
		return path
	}
	if path[0] == '"' {
		token, _, ok := readQuotedToken(path)
		if ok {
			if unquoted, err := strconv.Unquote(token); err == nil {
				return unquoted
			}
			return strings.Trim(token, "\"")
		}
	}
	return unescapeGitPath(path)
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

func splitLinesKeepEOL(input string) [][]byte {
	if input == "" {
		return nil
	}
	lines := bytes.SplitAfter([]byte(input), []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	return lines
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

func trimLine(line string) string {
	line = strings.TrimSuffix(line, "\n")
	return strings.TrimSuffix(line, "\r")
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
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if part == "." {
			return fmt.Errorf("current path in patch: %s", path)
		}
		if part == ".." {
			return fmt.Errorf("parent path in patch: %s", path)
		}
		if strings.EqualFold(part, ".git") {
			return fmt.Errorf(".git path in patch: %s", path)
		}
	}
	return nil
}

func readAllAt(dir *os.File, name string) ([]byte, error) {
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
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	var initialCap int
	if err := unix.Fstat(fd, &stat); err == nil {
		if err := CheckPatchSize(int(stat.Size)); err != nil {
			return nil, err
		}
		initialCap = int(stat.Size)
	}
	buf := bytes.NewBuffer(make([]byte, 0, initialCap))
	if _, err := io.Copy(buf, io.LimitReader(file, MaxPatchBytes+1)); err != nil {
		return nil, err
	}
	data := buf.Bytes()
	if err := CheckPatchSize(len(data)); err != nil {
		return nil, err
	}
	return data, nil
}
