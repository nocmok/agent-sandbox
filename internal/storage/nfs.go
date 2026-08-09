// Package storage implements sandbox.Storage plus the path-safe file
// operations backing the GET/POST /files endpoints. It talks NFSv4 directly
// over the network via a pure-Go client (github.com/aurorasolar/go-nfs-client,
// served by the Cyberax/go-nfs-client fork per the replace directive in
// go.mod) rather than relying on the host kernel to mount the export -
// sandboxd needs no privileged volume mount of its own to read and write
// sandbox files.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"

	nfs4 "github.com/aurorasolar/go-nfs-client/nfs4"
	"github.com/google/uuid"
)

var (
	ErrInvalidPath = errors.New("invalid path")
	ErrIsDirectory = errors.New("path refers to a directory")
	ErrNotFound    = errors.New("file not found")
)

// nfsPort is NFSv4's well-known port (see the nfs-server Dockerfile's EXPOSE).
const nfsPort = "2049"

type NFS struct {
	addr string
	auth nfs4.AuthParams

	// The client holds a single TCP connection and is not safe for
	// concurrent use (RPC replies are read back synchronously, matched to
	// their request only by ordering, not by request id) - every access
	// goes through withClient, which serializes calls behind mu. That makes
	// a large upload/download block unrelated sandboxes' file operations for
	// its duration; acceptable for this POC's traffic (files are only
	// touched while a sandbox is stopped), not for high concurrency.
	mu     sync.Mutex
	client nfs4.NfsInterface
}

// New constructs an NFS storage client that dials host (e.g. "nfs-server")
// on the standard NFS port. The connection is established lazily, on first
// use, not here.
func New(host string) *NFS {
	return &NFS{
		addr: host + ":" + nfsPort,
		auth: nfs4.AuthParams{
			// Matches the sandbox container's fixed uid:gid from the
			// security profile in internal/dockerengine, so files sandboxd
			// writes land with permissions the container can actually use.
			Uid:         1000,
			Gid:         1000,
			MachineName: "sandboxd",
		},
	}
}

// newWithClient is used by tests to inject a fake nfs4.NfsInterface,
// bypassing the real dial.
func newWithClient(c nfs4.NfsInterface) *NFS {
	return &NFS{client: c}
}

// withClient runs fn against the live client, dialing on first use. If fn's
// error isn't an *nfs4.NfsError (i.e. the server rejected a well-formed
// request), the connection is assumed broken and dropped so the next call
// redials instead of reusing a dead socket.
func (n *NFS) withClient(fn func(nfs4.NfsInterface) error) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.client == nil {
		c, err := nfs4.NewNfsClient(context.Background(), n.addr, n.auth)
		if err != nil {
			return fmt.Errorf("dialing NFS server %s: %w", n.addr, err)
		}
		n.client = c
	}

	err := fn(n.client)

	var nfsErr *nfs4.NfsError
	if err != nil && !errors.As(err, &nfsErr) {
		n.client.Close()
		n.client = nil
	}
	return err
}

// sandboxDir is the sandbox's location on the NFS export - derived
// deterministically from the sandbox id (e.g. "{nfs_root}/{id}"), per the
// doc's Data Model section; it's not stored anywhere.
func (n *NFS) sandboxDir(id uuid.UUID) string {
	return id.String()
}

// resolvePath validates a user-supplied relative path and returns the
// corresponding path on the NFS export, guaranteed to stay within the
// sandbox's directory. Rejects absolute paths and any path that escapes the
// sandbox root via ".." segments.
func (n *NFS) resolvePath(id uuid.UUID, relPath string) (string, error) {
	if path.IsAbs(relPath) {
		return "", fmt.Errorf("%w: path must be relative", ErrInvalidPath)
	}
	clean := path.Clean(relPath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: path escapes sandbox volume", ErrInvalidPath)
	}
	return n.sandboxDir(id) + "/" + clean, nil
}

func (n *NFS) AllocateSandboxDir(id uuid.UUID) error {
	dir := n.sandboxDir(id)
	return n.withClient(func(c nfs4.NfsInterface) error {
		return c.MakePath(dir)
	})
}

func (n *NFS) RemoveSandboxDir(id uuid.UUID) error {
	dir := n.sandboxDir(id)
	return n.withClient(func(c nfs4.NfsInterface) error {
		return nfs4.RemoveRecursive(c, dir)
	})
}

// ReadFile opens relPath for reading within the sandbox's volume. The caller
// must Close the returned reader.
//
// Note: unlike a real POSIX filesystem, the NFSv4 client here has no way to
// report whether a path is a symlink (nfs4.FileInfo only exposes IsDir) - a
// symlink at the leaf is read as if it were the underlying file rather than
// rejected the way the old local-mount implementation rejected symlinks.
func (n *NFS) ReadFile(id uuid.UUID, relPath string) (io.ReadCloser, error) {
	abs, err := n.resolvePath(id, relPath)
	if err != nil {
		return nil, err
	}

	var info nfs4.FileInfo
	err = n.withClient(func(c nfs4.NfsInterface) error {
		var ferr error
		info, ferr = c.GetFileInfo(abs)
		return ferr
	})
	if nfs4.IsNfsError(err, nfs4.ERROR_NOENT) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if info.IsDir {
		return nil, ErrIsDirectory
	}

	pr, pw := io.Pipe()
	go func() {
		err := n.withClient(func(c nfs4.NfsInterface) error {
			_, ferr := c.ReadFileAll(abs, pw)
			return ferr
		})
		pw.CloseWithError(err)
	}()
	return pr, nil
}

// WriteFile writes r to relPath within the sandbox's volume, creating any
// missing parent directories. Overwrites the file if it already exists.
//
// Note: unlike the old local-mount implementation, this doesn't write
// through a temp file and rename - the underlying library has no rename
// operation, so a write that fails partway leaves a partially-written file
// rather than the previous content. Acceptable for this POC (files are only
// touched while the sandbox is stopped, so there's no concurrent reader to
// observe the partial state).
func (n *NFS) WriteFile(id uuid.UUID, relPath string, r io.Reader) error {
	abs, err := n.resolvePath(id, relPath)
	if err != nil {
		return err
	}
	dir := path.Dir(abs)

	return n.withClient(func(c nfs4.NfsInterface) error {
		info, ferr := c.GetFileInfo(abs)
		if ferr == nil && info.IsDir {
			return ErrIsDirectory
		}
		if ferr != nil && !nfs4.IsNfsError(ferr, nfs4.ERROR_NOENT) {
			return ferr
		}
		if ferr := c.MakePath(dir); ferr != nil {
			return ferr
		}
		_, ferr = c.ReWriteFile(abs, r)
		return ferr
	})
}
