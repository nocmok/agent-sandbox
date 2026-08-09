package storage

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	nfs4 "github.com/aurorasolar/go-nfs-client/nfs4"
	"github.com/google/uuid"
)

// fakeNfs is a hand-written, in-memory nfs4.NfsInterface standing in for a
// real NFSv4 server, following this repo's convention (see
// internal/sandbox/fakes_test.go) of hand-written fakes over mocking
// libraries. It only implements as much fidelity as internal/storage/nfs.go
// actually exercises.
type fakeNfs struct {
	mu    sync.Mutex
	nodes map[string]*fakeNfsNode // path -> node; "" is the always-present root dir
}

type fakeNfsNode struct {
	isDir bool
	data  []byte
}

func newFakeNfs() *fakeNfs {
	return &fakeNfs{nodes: map[string]*fakeNfsNode{"": {isDir: true}}}
}

func notFound(path string) error {
	return &nfs4.NfsError{Path: path, ErrorCode: nfs4.ERROR_NOENT, ErrorString: "no such file or directory"}
}

func (f *fakeNfs) Ping() error { return nil }
func (f *fakeNfs) Close()      {}

func (f *fakeNfs) GetFileInfo(p string) (nfs4.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.nodes[p]
	if !ok {
		return nfs4.FileInfo{}, notFound(p)
	}
	return nfs4.FileInfo{Name: p, IsDir: n.isDir, Size: uint64(len(n.data))}, nil
}

func (f *fakeNfs) GetFileList(dir string) ([]nfs4.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.nodes[dir]; !ok {
		return nil, notFound(dir)
	}
	prefix := dir + "/"
	if dir == "" {
		prefix = ""
	}
	var out []nfs4.FileInfo
	for p, n := range f.nodes {
		if p == dir || p == "" || !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		if strings.Contains(rest, "/") {
			continue // not a direct child
		}
		out = append(out, nfs4.FileInfo{Name: rest, IsDir: n.isDir, Size: uint64(len(n.data))})
	}
	return out, nil
}

func (f *fakeNfs) ReadFileAll(p string, w io.Writer) (uint64, error) {
	f.mu.Lock()
	n, ok := f.nodes[p]
	f.mu.Unlock()
	if !ok {
		return 0, notFound(p)
	}
	written, err := w.Write(n.data)
	return uint64(written), err
}

func (f *fakeNfs) ReadFile(p string, offset, count uint64, w io.Writer) (uint64, error) {
	return f.ReadFileAll(p, w)
}

func (f *fakeNfs) ReWriteFile(p string, r io.Reader) (uint64, error) {
	return f.WriteFile(p, true, 0, r)
}

func (f *fakeNfs) WriteFile(p string, truncate bool, offset uint64, r io.Reader) (uint64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes[p] = &fakeNfsNode{data: data}
	return uint64(len(data)), nil
}

func (f *fakeNfs) DeleteFile(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.nodes[p]; !ok {
		return notFound(p)
	}
	delete(f.nodes, p)
	return nil
}

func (f *fakeNfs) MakePath(p string) error {
	if p == "" {
		return nil
	}
	segs := strings.Split(p, "/")
	cur := ""
	for _, seg := range segs {
		if cur == "" {
			cur = seg
		} else {
			cur = cur + "/" + seg
		}
		f.mu.Lock()
		n, ok := f.nodes[cur]
		if !ok {
			f.nodes[cur] = &fakeNfsNode{isDir: true}
		} else if !n.isDir {
			f.mu.Unlock()
			return &nfs4.NfsError{Path: cur, ErrorCode: nfs4.ERROR_NOTDIR, ErrorString: "not a directory"}
		}
		f.mu.Unlock()
	}
	return nil
}

var _ nfs4.NfsInterface = (*fakeNfs)(nil)

func newTestNFS(t *testing.T) (*NFS, uuid.UUID) {
	t.Helper()
	n := newWithClient(newFakeNfs())
	id := uuid.New()
	if err := n.AllocateSandboxDir(id); err != nil {
		t.Fatalf("AllocateSandboxDir: %v", err)
	}
	return n, id
}

func TestWriteThenReadFile_ValidNestedPath(t *testing.T) {
	n, id := newTestNFS(t)
	if err := n.WriteFile(id, "sub/dir/file.txt", strings.NewReader("hello")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f, err := n.ReadFile(id, "sub/dir/file.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("expected 'hello', got %q", got)
	}
}

func TestWriteFile_RejectsAbsolutePath(t *testing.T) {
	n, id := newTestNFS(t)
	err := n.WriteFile(id, "/etc/passwd", strings.NewReader("x"))
	if !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("expected ErrInvalidPath, got %v", err)
	}
}

func TestWriteFile_RejectsParentTraversal(t *testing.T) {
	n, id := newTestNFS(t)
	cases := []string{"../escape.txt", "sub/../../escape.txt", "..", "sub/../.."}
	for _, p := range cases {
		t.Run(p, func(t *testing.T) {
			err := n.WriteFile(id, p, strings.NewReader("x"))
			if !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("expected ErrInvalidPath for %q, got %v", p, err)
			}
		})
	}
}

func TestReadFile_RejectsDirectory(t *testing.T) {
	n, id := newTestNFS(t)
	if err := n.WriteFile(id, "sub/file.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	_, err := n.ReadFile(id, "sub")
	if !errors.Is(err, ErrIsDirectory) {
		t.Fatalf("expected ErrIsDirectory, got %v", err)
	}
}

func TestReadFile_NotFound(t *testing.T) {
	n, id := newTestNFS(t)
	_, err := n.ReadFile(id, "missing.txt")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestWriteFile_OverwritesExisting(t *testing.T) {
	n, id := newTestNFS(t)
	if err := n.WriteFile(id, "file.txt", strings.NewReader("first")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := n.WriteFile(id, "file.txt", strings.NewReader("second")); err != nil {
		t.Fatalf("WriteFile (overwrite): %v", err)
	}
	f, err := n.ReadFile(id, "file.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	defer f.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, f); err != nil {
		t.Fatalf("read: %v", err)
	}
	if buf.String() != "second" {
		t.Fatalf("expected 'second', got %q", buf.String())
	}
}

func TestRemoveSandboxDir(t *testing.T) {
	n, id := newTestNFS(t)
	if err := n.WriteFile(id, "file.txt", strings.NewReader("x")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := n.RemoveSandboxDir(id); err != nil {
		t.Fatalf("RemoveSandboxDir: %v", err)
	}
	if _, err := n.ReadFile(id, "file.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected file gone after RemoveSandboxDir, got %v", err)
	}
}
