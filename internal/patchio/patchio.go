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

func LockPatch(path string) (*PatchLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		file.Close()
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
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		return path, nil
	}

	patchDir := opts.PatchDir
	if strings.TrimSpace(patchDir) == "" {
		patchDir = filepath.Join(gitDir, "persona", "patches")
	}
	if !filepath.IsAbs(patchDir) {
		abs, err := filepath.Abs(patchDir)
		if err != nil {
			return "", err
		}
		patchDir = abs
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

func AtomicWriteFileAt(dir *os.File, name string, data []byte) error {
	if dir == nil {
		return errors.New("dir is nil")
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
		_ = unix.Fchown(fd, uid, gid)
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
	scanner := bufio.NewScanner(strings.NewReader(string(patch)))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "diff --git ") {
			a, b, ok := parseDiffGitLine(line)
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
		if strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "--- ") {
			path := trimLine(line[4:])
			if path == "/dev/null" {
				continue
			}
			path = stripDiffPathMeta(path)
			path = parseMaybeQuotedPath(path)
			if err := checkPath(path); err != nil {
				return err
			}
			continue
		}
		for _, prefix := range []string{"rename from ", "rename to ", "copy from ", "copy to "} {
			if strings.HasPrefix(line, prefix) {
				path := trimLine(strings.TrimPrefix(line, prefix))
				path = parseMaybeQuotedPath(path)
				if err := checkPath(path); err != nil {
					return err
				}
				break
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
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
	lines := splitLinesKeepEOL(string(patch))
	blocks := parsePatchBlocks(lines)
	if len(blocks) == 0 {
		return patch, nil, nil
	}
	var out strings.Builder
	skipped := make([]string, 0)
	for _, block := range blocks {
		if shouldSkipNewFileBlock(block, workTree) {
			skipped = append(skipped, block.path)
			continue
		}
		for _, line := range block.lines {
			out.WriteString(line)
		}
	}
	return []byte(out.String()), skipped, nil
}

type patchBlock struct {
	lines    []string
	path     string
	isNew    bool
	isBinary bool
	mode     string
}

func parsePatchBlocks(lines []string) []patchBlock {
	var blocks []patchBlock
	var current patchBlock
	seenDiff := false
	for _, line := range lines {
		raw := trimLine(line)
		if strings.HasPrefix(raw, "diff --git ") || raw == "diff --git" {
			if seenDiff && len(current.lines) > 0 {
				blocks = append(blocks, current)
			}
			seenDiff = true
			current = patchBlock{lines: []string{line}, path: parseDiffGitPath(raw)}
			continue
		}
		if !seenDiff {
			continue
		}
		current.lines = append(current.lines, line)
		if strings.HasPrefix(raw, "new file mode ") {
			current.isNew = true
			current.mode = strings.TrimSpace(strings.TrimPrefix(raw, "new file mode "))
		}
		if strings.HasPrefix(raw, "--- /dev/null") {
			current.isNew = true
		}
		if strings.HasPrefix(raw, "GIT binary patch") || strings.HasPrefix(raw, "Binary files ") {
			current.isBinary = true
		}
		if strings.HasPrefix(raw, "+++ ") && current.path == "" {
			current.path = parsePlusPath(raw)
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
		return string(existing) == content
	}
	return bytes.Equal(existing, []byte(content))
}

func extractNewFileContent(lines []string) (string, bool, bool) {
	var out strings.Builder
	inHunk := false
	sawHeader := false
	sawHunk := false
	noFinalNL := false
	for _, line := range lines {
		raw := trimLine(line)
		if strings.HasPrefix(raw, "+++ ") {
			sawHeader = true
		}
		if strings.HasPrefix(raw, "@@ ") {
			inHunk = true
			sawHunk = true
			continue
		}
		if !inHunk {
			continue
		}
		if raw == "\\ No newline at end of file" {
			noFinalNL = true
			continue
		}
		if strings.HasPrefix(raw, "+") {
			out.WriteString(raw[1:])
			out.WriteString("\n")
		}
	}
	if !sawHunk && !sawHeader {
		return "", false, false
	}
	content := out.String()
	if noFinalNL && strings.HasSuffix(content, "\n") {
		content = strings.TrimSuffix(content, "\n")
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

func splitLinesKeepEOL(input string) []string {
	if input == "" {
		return nil
	}
	lines := strings.SplitAfter(input, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
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

func ReadAll(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}
