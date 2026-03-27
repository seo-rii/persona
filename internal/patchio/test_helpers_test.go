package patchio

import (
	"bytes"
	"strconv"
	"strings"
)

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
	if end == -1 {
		return unescapeGitPath(s), "", true
	}
	token := s[:end]
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
			current = beginPatchBlock(line)
			continue
		}
		if !seenDiff {
			continue
		}
		appendPatchBlockLine(&current, line, raw)
	}
	if len(current.lines) > 0 {
		blocks = append(blocks, current)
	}
	return blocks
}
