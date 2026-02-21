package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Session struct {
	ID        string
	Root      string
	Upper     string
	Work      string
	MntBase   string
	MntGitDir string
	BaseWT    string
	Tmp       string
}

func Create(gitDir string) (*Session, error) {
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(gitDir, "persona", "sessions", id)
	upper := filepath.Join(root, "upper")
	work := filepath.Join(root, "work")
	mnt := filepath.Join(root, "mnt")
	mntBase := filepath.Join(mnt, "base")
	mntGitDir := filepath.Join(mnt, "gitdir")
	baseWT := filepath.Join(root, "basewt")
	tmp := filepath.Join(root, "tmp")

	paths := []string{upper, work, mntBase, mntGitDir, baseWT, tmp}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return nil, err
		}
	}

	return &Session{
		ID:        id,
		Root:      root,
		Upper:     upper,
		Work:      work,
		MntBase:   mntBase,
		MntGitDir: mntGitDir,
		BaseWT:    baseWT,
		Tmp:       tmp,
	}, nil
}

func (s *Session) RemoveAll() error {
	if s == nil {
		return nil
	}
	return os.RemoveAll(s.Root)
}

func newSessionID() (string, error) {
	now := time.Now()
	buf := make([]byte, 3)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s_%d_%s", now.Format("20060102_150405"), os.Getpid(), hex.EncodeToString(buf)), nil
}
