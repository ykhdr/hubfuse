package agent

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

// aclHandlers is a per-connection sftp.Handlers implementation. It captures
// the connecting peer's device_id and reads a fresh []ShareACL snapshot on
// every request so fsnotify-driven config reloads take effect immediately.
type aclHandlers struct {
	deviceID string
	resolver DeviceResolver
	snapshot func() []ShareACL
	logger   *slog.Logger
}

func newACLHandlers(deviceID string, r DeviceResolver, snap func() []ShareACL, logger *slog.Logger) *aclHandlers {
	return &aclHandlers{deviceID: deviceID, resolver: r, snapshot: snap, logger: logger}
}

// ToHandlers produces the sftp.Handlers value the request server needs.
func (h *aclHandlers) ToHandlers() sftp.Handlers {
	return sftp.Handlers{
		FileGet:  h,
		FilePut:  h,
		FileCmd:  h,
		FileList: h,
	}
}

func (h *aclHandlers) findShare(alias string) (ShareACL, AccessDecision, bool) {
	for _, a := range h.snapshot() {
		if a.Alias == alias {
			return a, a.Decide(h.deviceID, h.resolver), true
		}
	}
	return ShareACL{}, AccessDecision{}, false
}

// splitVirtual splits the virtual SFTP path into the alias and the remainder.
// The path is cleaned; returns ok=false for the synthetic root "/".
func splitVirtual(virtual string) (alias, rest string, ok bool) {
	cleaned := path.Clean("/" + strings.TrimPrefix(virtual, "/"))
	if cleaned == "/" {
		return "", "", false
	}
	trimmed := strings.TrimPrefix(cleaned, "/")
	if i := strings.Index(trimmed, "/"); i >= 0 {
		return trimmed[:i], trimmed[i+1:], true
	}
	return trimmed, "", true
}

func denied() error { return sftp.ErrSSHFxPermissionDenied }

// resolveErr maps path-resolution errors to the correct SFTP status code.
//
// Not-exist errors (fs.ErrNotExist, including wrapped syscall.ENOENT from
// filepath.EvalSymlinks) surface as SSH_FX_NO_SUCH_FILE so that SFTP/sshfs
// clients receive a negative lookup: create and mkdir depend on an ENOENT
// response to obtain a negative dentry before issuing the write (issue #46).
//
// All other errors — escape attempts (custom fmt.Errorf from the Rel
// containment check), ACL failures, and unknown-alias paths — stay as
// PERMISSION_DENIED so that share-alias existence and outside-share paths
// do not leak to unauthorized peers.
func resolveErr(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return sftp.ErrSSHFxNoSuchFile
	}
	return denied()
}

// resolveReadReal finds the share for virtualPath, confirms the peer is
// allowed, returns the canonical (symlink-resolved) on-disk path contained
// within the share root. Any escape attempt surfaces as a permission error —
// including symlinks planted inside the share that point outside.
func (h *aclHandlers) resolveReadReal(virtualPath string) (string, error) {
	alias, _, ok := splitVirtual(virtualPath)
	if !ok {
		return "", denied()
	}
	acl, dec, found := h.findShare(alias)
	if !found || !dec.Allow {
		return "", denied()
	}
	lexical, err := ResolveSharePath(acl.Path, virtualPath, alias)
	if err != nil {
		return "", denied()
	}
	real, err := containedReal(acl.Path, lexical)
	if err != nil {
		return "", resolveErr(err)
	}
	return real, nil
}

// resolveWriteReal is the write-side counterpart: performs ACL + read-only
// checks, then verifies the *parent* directory stays inside the share root
// (the leaf may not exist yet), returning the path built from the canonical
// parent and the untouched base name.
func (h *aclHandlers) resolveWriteReal(virtualPath string) (string, error) {
	alias, _, ok := splitVirtual(virtualPath)
	if !ok {
		return "", denied()
	}
	acl, dec, found := h.findShare(alias)
	if !found || !dec.Allow || dec.ReadOnly {
		return "", denied()
	}
	lexical, err := ResolveSharePath(acl.Path, virtualPath, alias)
	if err != nil {
		return "", denied()
	}
	real, err := containedWritePath(acl.Path, lexical)
	if err != nil {
		return "", resolveErr(err)
	}
	return real, nil
}

// openFlagsForRequest maps an SFTP open-request's pflags to os.OpenFile flags.
//
// The client's pflags are honored exactly. pkg/sftp's request server delivers
// every write via io.WriterAt.WriteAt(data, offset); os.File.WriteAt always
// returns an error on O_APPEND handles ("invalid use of WriteAt on file opened
// with O_APPEND", issue #54). For that reason, when the client sets APPEND the
// file is still opened with O_APPEND (kernel-atomic EOF positioning), but
// Filewrite wraps it in an appendOnlyWriter whose WriteAt ignores the offset
// and calls f.Write instead — matching OpenSSH sftp-server semantics.
//
// The implicit O_CREATE|O_TRUNC fallback fires ONLY when neither the Read nor
// the Write pflag is set ("bare Put" minimal clients). Any explicit Read or
// Write pflag means the client is managing creation/truncation itself, so we
// must not add implicit flags — the old unconditional fallback truncated
// existing files on legitimate non-truncating write-opens, producing NUL-prefix
// corruption (issue #45).
func openFlagsForRequest(r *sftp.Request) int {
	p := r.Pflags()
	flags := 0
	switch {
	case p.Read && p.Write:
		flags |= os.O_RDWR
	case p.Write:
		flags |= os.O_WRONLY
	case p.Read:
		// Read-only pflags routed to a write-class method (pathological
		// client, e.g. READ|CREAT without WRITE): the handle must still be
		// writable, but the client did not opt into upload semantics.
		flags |= os.O_WRONLY
	default:
		// Neither Read nor Write is set: fully degenerate "bare Put" client.
		// Apply upload semantics so plain sftp-put still works.
		flags |= os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	if p.Append {
		flags |= os.O_APPEND
	}
	if p.Creat {
		flags |= os.O_CREATE
	}
	if p.Trunc {
		flags |= os.O_TRUNC
	}
	if p.Excl {
		flags |= os.O_EXCL
	}
	return flags
}

// appendOnlyWriter wraps an O_APPEND *os.File for use as an io.WriterAt.
//
// SFTP SSH_FXF_APPEND semantics require every write to land at EOF regardless
// of the offset carried in the WRITE packet — identical to OpenSSH sftp-server
// behavior. pkg/sftp's own client (client.go toPflags) maps os.O_APPEND →
// SSH_FXF_APPEND but tracks file offsets from 0 (no seek-to-EOF on open), so
// library clients depend on the server ignoring the supplied offset. Offset-
// correct clients like sshfs also work correctly because kernel O_APPEND
// positions the write atomically before the fd-level Write.
//
// The mutex is required because pkg/sftp's request server processes packets on
// a concurrent worker pool (request-server.go:189-198); without serialization,
// concurrent WriteAt calls on the same handle would race on the underlying
// os.File.Write.
type appendOnlyWriter struct {
	f  *os.File
	mu sync.Mutex
}

// WriteAt ignores off and appends p to the file at the current EOF position.
// This satisfies both offset-correct clients (sshfs) and offset-from-0 clients
// (pkg/sftp's own client), matching the SFTP spec and OpenSSH sftp-server.
func (a *appendOnlyWriter) WriteAt(p []byte, _ int64) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.f.Write(p)
}

// Close closes the underlying file. pkg/sftp's request server closes handles
// via an io.Closer type assertion (request.go:254 in pkg/sftp v1.13.10).
func (a *appendOnlyWriter) Close() error {
	return a.f.Close()
}

// requestedMode returns the mode bits the client asked for on create, or
// 0644 when none were specified.
func requestedMode(r *sftp.Request) os.FileMode {
	if r.AttrFlags().Permissions {
		if attrs := r.Attributes(); attrs != nil {
			if m := attrs.FileMode(); m != 0 {
				return m & os.ModePerm
			}
		}
	}
	return 0o644
}

// Fileread — implements sftp.FileReader.
func (h *aclHandlers) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	real, err := h.resolveReadReal(r.Filepath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Filewrite — implements sftp.FileWriter.
func (h *aclHandlers) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	real, err := h.resolveWriteReal(r.Filepath)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(real, openFlagsForRequest(r), requestedMode(r))
	if err != nil {
		return nil, err
	}
	if r.Pflags().Append {
		return &appendOnlyWriter{f: f}, nil
	}
	return f, nil
}

// Filecmd — implements sftp.FileCmder. Every method routed here is
// write-class and therefore requires a writable share.
func (h *aclHandlers) Filecmd(r *sftp.Request) error {
	// Symlink creation is unconditionally rejected: the target is untrusted
	// peer-supplied data and a planted symlink combined with a later read
	// would let the peer escape the share.
	if r.Method == "Symlink" {
		return denied()
	}

	real, err := h.resolveWriteReal(r.Filepath)
	if err != nil {
		return err
	}

	switch r.Method {
	case "Setstat":
		return h.applySetstat(r, real)
	case "Rename":
		targetReal, err := h.resolveWriteReal(r.Target)
		if err != nil {
			return err
		}
		return os.Rename(real, targetReal)
	case "Rmdir", "Remove":
		return os.Remove(real)
	case "Mkdir":
		return os.Mkdir(real, 0o755)
	case "Link":
		targetReal, err := h.resolveWriteReal(r.Target)
		if err != nil {
			return err
		}
		return os.Link(real, targetReal)
	}
	return sftp.ErrSSHFxOpUnsupported
}

// applySetstat applies the attributes flagged in r to real. Mode, times and
// truncate are supported; uid/gid changes are explicitly reported as
// unsupported so clients do not silently assume the change took effect.
func (h *aclHandlers) applySetstat(r *sftp.Request, real string) error {
	flags := r.AttrFlags()
	attrs := r.Attributes()
	if attrs == nil {
		return nil
	}
	if flags.Permissions {
		if err := os.Chmod(real, attrs.FileMode()); err != nil {
			return err
		}
	}
	if flags.Acmodtime {
		atime := time.Unix(int64(attrs.Atime), 0)
		mtime := time.Unix(int64(attrs.Mtime), 0)
		if err := os.Chtimes(real, atime, mtime); err != nil {
			return err
		}
	}
	if flags.Size {
		if err := os.Truncate(real, int64(attrs.Size)); err != nil {
			return err
		}
	}
	if flags.UidGid {
		// Deliberately refuse: changing ownership would cross the trust
		// boundary between the peer and the host's Unix user, and silently
		// dropping it (as the old implementation did) misleads clients.
		return sftp.ErrSSHFxOpUnsupported
	}
	return nil
}

// Filelist — implements sftp.FileLister.
func (h *aclHandlers) Filelist(r *sftp.Request) (sftp.ListerAt, error) {
	// Synthetic root: list only allowed shares.
	if r.Filepath == "/" || r.Filepath == "" {
		var infos []os.FileInfo
		for _, a := range h.snapshot() {
			if !a.Decide(h.deviceID, h.resolver).Allow {
				continue
			}
			fi, err := os.Stat(a.Path)
			if err != nil {
				continue
			}
			infos = append(infos, renamedFileInfo{FileInfo: fi, name: a.Alias})
		}
		return listerAt(infos), nil
	}

	real, err := h.resolveReadReal(r.Filepath)
	if err != nil {
		return nil, err
	}

	switch r.Method {
	case "List":
		f, err := os.Open(real)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		entries, err := f.Readdir(-1)
		if err != nil {
			return nil, err
		}
		return listerAt(entries), nil
	case "Stat":
		fi, err := os.Stat(real)
		if err != nil {
			return nil, err
		}
		return listerAt([]os.FileInfo{fi}), nil
	case "Lstat":
		fi, err := os.Lstat(real)
		if err != nil {
			return nil, err
		}
		return listerAt([]os.FileInfo{fi}), nil
	case "Readlink":
		target, err := os.Readlink(real)
		if err != nil {
			return nil, err
		}
		return listerAt([]os.FileInfo{renamedFileInfo{FileInfo: staticLink(target), name: target}}), nil
	}
	return nil, sftp.ErrSSHFxOpUnsupported
}

// listerAt is a trivial sftp.ListerAt over a slice.
type listerAt []os.FileInfo

func (l listerAt) ListAt(p []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(p, l[off:])
	if off+int64(n) >= int64(len(l)) {
		return n, io.EOF
	}
	return n, nil
}

// renamedFileInfo overrides Name() while delegating all other methods.
type renamedFileInfo struct {
	os.FileInfo
	name string
}

func (r renamedFileInfo) Name() string { return r.name }

// staticLink is a minimal os.FileInfo describing a symlink target by name.
type staticLink string

func (s staticLink) Name() string       { return string(s) }
func (s staticLink) Size() int64        { return int64(len(s)) }
func (s staticLink) Mode() os.FileMode  { return os.ModeSymlink | 0o777 }
func (s staticLink) ModTime() time.Time { return time.Time{} }
func (s staticLink) IsDir() bool        { return false }
func (s staticLink) Sys() any           { return nil }
