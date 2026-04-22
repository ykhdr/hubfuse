package agent

import (
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
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

// Fileread — implements sftp.FileReader.
func (h *aclHandlers) Fileread(r *sftp.Request) (io.ReaderAt, error) {
	alias, _, ok := splitVirtual(r.Filepath)
	if !ok {
		return nil, denied()
	}
	acl, dec, found := h.findShare(alias)
	if !found || !dec.Allow {
		return nil, denied()
	}
	real, err := ResolveSharePath(acl.Path, r.Filepath, alias)
	if err != nil {
		return nil, denied()
	}
	f, err := os.Open(real)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Filewrite — implements sftp.FileWriter.
func (h *aclHandlers) Filewrite(r *sftp.Request) (io.WriterAt, error) {
	alias, _, ok := splitVirtual(r.Filepath)
	if !ok {
		return nil, denied()
	}
	acl, dec, found := h.findShare(alias)
	if !found || !dec.Allow || dec.ReadOnly {
		return nil, denied()
	}
	real, err := ResolveSharePath(acl.Path, r.Filepath, alias)
	if err != nil {
		return nil, denied()
	}
	f, err := os.OpenFile(real, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Filecmd — implements sftp.FileCmder. Every method routed here is write-class.
func (h *aclHandlers) Filecmd(r *sftp.Request) error {
	alias, _, ok := splitVirtual(r.Filepath)
	if !ok {
		return denied()
	}
	acl, dec, found := h.findShare(alias)
	if !found || !dec.Allow || dec.ReadOnly {
		return denied()
	}
	real, err := ResolveSharePath(acl.Path, r.Filepath, alias)
	if err != nil {
		return denied()
	}
	switch r.Method {
	case "Setstat":
		if attrs := r.Attributes(); attrs != nil && attrs.FileMode() != 0 {
			return os.Chmod(real, attrs.FileMode())
		}
		return nil
	case "Rename":
		targetAlias, _, tok := splitVirtual(r.Target)
		if !tok || targetAlias != alias {
			return denied()
		}
		targetReal, err := ResolveSharePath(acl.Path, r.Target, alias)
		if err != nil {
			return denied()
		}
		return os.Rename(real, targetReal)
	case "Rmdir":
		return os.Remove(real)
	case "Remove":
		return os.Remove(real)
	case "Mkdir":
		return os.Mkdir(real, 0o755)
	case "Link":
		targetAlias, _, tok := splitVirtual(r.Target)
		if !tok || targetAlias != alias {
			return denied()
		}
		targetReal, err := ResolveSharePath(acl.Path, r.Target, alias)
		if err != nil {
			return denied()
		}
		return os.Link(real, targetReal)
	case "Symlink":
		return os.Symlink(r.Target, real)
	}
	return denied()
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

	alias, _, ok := splitVirtual(r.Filepath)
	if !ok {
		return nil, denied()
	}
	acl, dec, found := h.findShare(alias)
	if !found || !dec.Allow {
		return nil, denied()
	}
	real, err := ResolveSharePath(acl.Path, r.Filepath, alias)
	if err != nil {
		return nil, denied()
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
	return nil, denied()
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
