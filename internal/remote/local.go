package remote

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/runbear-io/beardrive/internal/store"
)

// localBackend stores objects in a plain directory. Useful for tests and for
// syncing through any mounted network drive.
type localBackend struct {
	root string
}

func newLocal(root string) (*localBackend, error) {
	if root == "" {
		return nil, fmt.Errorf("file:// remote needs an absolute path")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &localBackend{root: root}, nil
}

// path resolves a key under the root and refuses one that climbs out of it.
// Keys come from journals and API callers, so "the key is well-formed" is a
// caller's promise, not a fact — this is the last place to check it before a
// traversal becomes a read or a write anywhere on the hub host.
//
// Two checks, because they answer different questions: the lexical one rejects
// ".." in the key, and store.UnderRoot rejects a key whose path resolves out
// through a symlink planted inside the storage root — os.Open and os.Rename
// follow those, so the string staying under the root says nothing about where
// the bytes come from or land.
func (b *localBackend) path(key string) (string, error) {
	p := filepath.Join(b.root, filepath.FromSlash(key))
	if p != b.root && !strings.HasPrefix(p, b.root+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid key %q", key)
	}
	if !store.UnderRoot(b.root, p) {
		return "", fmt.Errorf("invalid key %q", key)
	}
	return p, nil
}

func (b *localBackend) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	dst, err := b.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".bdrive-tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

func (b *localBackend) Get(_ context.Context, key string) (io.ReadCloser, error) {
	p, err := b.path(key)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (b *localBackend) List(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	err := filepath.WalkDir(b.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".bdrive-tmp-") {
			return nil
		}
		rel, err := filepath.Rel(b.root, p)
		if err != nil {
			return nil
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, Object{Key: key, Size: info.Size(), Modified: info.ModTime()})
		return nil
	})
	return out, err
}

func (b *localBackend) Exists(_ context.Context, key string) (bool, error) {
	p, perr := b.path(key)
	if perr != nil {
		return false, perr
	}
	_, err := os.Stat(p)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (b *localBackend) Delete(_ context.Context, key string) error {
	p, err := b.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	// Prune now-empty parent directories so purging a prefix leaves no husk;
	// os.Remove refuses a non-empty directory, which is the stop condition.
	for dir := filepath.Dir(p); dir != b.root; dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			break
		}
	}
	return nil
}

func (b *localBackend) Close() error { return nil }
