package session

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Session struct {
	ID      string
	Root    string
	Upper   string
	Work    string
	View    string
	MntBase string
	BaseWT  string
	Tmp     string
	gitDir  string
}

const ownerFile = ".persona-session"

func Create(gitDir string) (*Session, error) {
	for _, parent := range []string{
		filepath.Join(gitDir, "persona"),
		filepath.Join(gitDir, "persona", "sessions"),
	} {
		info, err := os.Lstat(parent)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("session parent is symlink: %s", parent)
		}
	}
	sessionsRoot := filepath.Join(gitDir, "persona", "sessions")
	if err := os.MkdirAll(sessionsRoot, 0o700); err != nil {
		return nil, err
	}
	var root, id string
	allocated := false
	for attempt := 0; attempt < 10; attempt++ {
		var err error
		id, err = newSessionID()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(sessionsRoot, id)
		if err = os.Mkdir(root, 0o700); os.IsExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		allocated = true
		break
	}
	if !allocated {
		return nil, fmt.Errorf("cannot allocate exclusive session directory")
	}
	marker, err := os.OpenFile(filepath.Join(root, ownerFile), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	_, writeErr := marker.WriteString(id + "\n")
	syncErr := marker.Sync()
	closeErr := marker.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if syncErr != nil {
		return nil, syncErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	upper := filepath.Join(root, "upper")
	work := filepath.Join(root, "work")
	view := filepath.Join(root, "view")
	mntBase := filepath.Join(root, "mnt", "base")
	baseWT := filepath.Join(root, "basewt")
	tmp := filepath.Join(root, "tmp")

	paths := []string{upper, work, view, mntBase, baseWT, tmp}
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, err
		}
	}

	return &Session{
		ID:      id,
		Root:    root,
		Upper:   upper,
		Work:    work,
		View:    view,
		MntBase: mntBase,
		BaseWT:  baseWT,
		Tmp:     tmp,
		gitDir:  gitDir,
	}, nil
}

func (s *Session) RemoveAll() error {
	if s == nil {
		return nil
	}
	if err := validateRoot(s.gitDir, s.Root); err != nil {
		return err
	}
	mounted, err := HasMounts(s.Root)
	if err != nil {
		return err
	}
	if mounted {
		return fmt.Errorf("refuse to remove mounted session %s", s.Root)
	}
	return os.RemoveAll(s.Root)
}

// Open accepts only a marked, direct child of this repository's sessions root.
// Metadata paths are untrusted and must never authorize recursive deletion.
func validateRoot(gitDir, root string) error {
	parent := filepath.Join(gitDir, "persona", "sessions")
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) || filepath.Dir(root) != filepath.Clean(parent) {
		return fmt.Errorf("session is not a direct child of %s", parent)
	}
	for _, path := range []string{gitDir, filepath.Join(gitDir, "persona"), parent, root} {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("session directory is not a real directory: %s", path)
		}
	}
	id := filepath.Base(root)
	marker := filepath.Join(root, ownerFile)
	info, err := os.Lstat(marker)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 256 {
		return fmt.Errorf("invalid session owner marker")
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		return err
	}
	if string(data) != id+"\n" {
		return fmt.Errorf("session owner marker mismatch")
	}
	return nil
}

func Open(gitDir, root string) (*Session, error) {
	if err := validateRoot(gitDir, root); err != nil {
		return nil, err
	}
	root = filepath.Clean(root)
	id := filepath.Base(root)
	s := &Session{ID: id, Root: root, Upper: filepath.Join(root, "upper"), Work: filepath.Join(root, "work"), View: filepath.Join(root, "view"), MntBase: filepath.Join(root, "mnt", "base"), BaseWT: filepath.Join(root, "basewt"), Tmp: filepath.Join(root, "tmp"), gitDir: gitDir}
	for _, path := range []string{s.Upper, s.Work, s.View, s.MntBase, s.BaseWT, s.Tmp} {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("invalid session subdirectory: %s", path)
		}
	}
	return s, nil
}

func HasMounts(root string) (bool, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	defer file.Close()
	scan := bufio.NewScanner(file)
	decode := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for scan.Scan() {
		fields := strings.Fields(scan.Text())
		if len(fields) < 5 {
			continue
		}
		path := decode.Replace(fields[4])
		if path == root || strings.HasPrefix(path, root+string(os.PathSeparator)) {
			return true, nil
		}
	}
	return false, scan.Err()
}

func newSessionID() (string, error) {
	now := time.Now()
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s_%d_%s", now.Format("20060102_150405"), os.Getpid(), hex.EncodeToString(buf)), nil
}
