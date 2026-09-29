package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cloudsoda/go-smb2"
)

// Backend reads the files of one source.
type Backend interface {
	// Walk calls fn for every video file under the source, with its slash-separated relative path.
	Walk(fn func(rel string, size int64, mod time.Time)) error
	// Open opens a file for streaming; ctx ends remote reads when the request that asked for them goes away.
	Open(ctx context.Context, rel string) (io.ReadSeekCloser, error)
	// LocalPath is the file's path on this machine, or "" when it is not a local file.
	LocalPath(rel string) string
	// ExternalPath is what the operating system can open the file with, or "".
	ExternalPath(rel string) string
	// Close releases any connection the backend holds.
	Close()
}

// newBackend builds the backend for a source.
func newBackend(src Source, secret string) Backend {
	if src.Kind == "smb" {
		return &smbBackend{src: src, secret: secret}
	}
	return &localBackend{root: filepath.Clean(expandHome(src.Path))}
}

// skipName reports names that never hold a library: hidden files and system folders.
func skipName(name string) bool {
	return strings.HasPrefix(name, ".") || strings.HasPrefix(name, "$") || name == "System Volume Information" || name == "@eaDir"
}

var extrasDirs = map[string]bool{
	"sample": true, "samples": true, "trailer": true, "trailers": true, "extras": true, "featurettes": true,
	"behind the scenes": true, "deleted scenes": true,
}

// skipDir reports folders that hold trailers, samples and extras rather than episodes or films.
func skipDir(name string) bool {
	return skipName(name) || extrasDirs[strings.ToLower(name)]
}

// expandHome turns a leading ~ into the home folder.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}

type localBackend struct {
	root string
}

// Walk lists video files under the folder, following symlinks without looping.
func (b *localBackend) Walk(fn func(rel string, size int64, mod time.Time)) error {
	if info, err := os.Stat(b.root); err != nil {
		return err
	} else if !info.IsDir() {
		return errors.New("not a folder")
	}
	visited := map[string]bool{}
	var walk func(dir, rel string)
	walk = func(dir, rel string) {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil || visited[real] {
			return
		}
		visited[real] = true
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			name := entry.Name()
			if skipName(name) {
				continue
			}
			full := filepath.Join(dir, name)
			info, err := os.Stat(full)
			if err != nil {
				continue
			}
			if info.IsDir() {
				if !skipDir(name) {
					walk(full, path.Join(rel, name))
				}
			} else if isVideo(name) && !isSample(name, info.Size()) {
				fn(path.Join(rel, name), info.Size(), info.ModTime())
			}
		}
	}
	walk(b.root, "")
	return nil
}

// Open opens a local file.
func (b *localBackend) Open(_ context.Context, rel string) (io.ReadSeekCloser, error) {
	return os.Open(b.LocalPath(rel))
}

// LocalPath joins the relative path onto the folder.
func (b *localBackend) LocalPath(rel string) string {
	return filepath.Join(b.root, filepath.FromSlash(rel))
}

// ExternalPath is the local path itself.
func (b *localBackend) ExternalPath(rel string) string {
	return b.LocalPath(rel)
}

// Close has nothing to release.
func (b *localBackend) Close() {}

type smbBackend struct {
	src     Source
	secret  string
	mu      sync.Mutex
	session *smb2.Session
	share   *smb2.Share
}

// smbAddress adds the default SMB port to a host that has none.
func smbAddress(host string) string {
	if _, _, err := net.SplitHostPort(host); err == nil {
		return host
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), "445")
}

// dialSMB opens an authenticated SMB session; an empty user logs in as a guest.
func dialSMB(host, user, password, domain string) (*smb2.Session, error) {
	users := []string{user}
	if user == "" {
		users = []string{"", "guest"}
	}
	var lastErr error
	for _, name := range users {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		dialer := &smb2.Dialer{Initiator: &smb2.NTLMInitiator{User: name, Password: password, Domain: domain}}
		session, err := dialer.Dial(ctx, smbAddress(host))
		cancel()
		if err == nil {
			return session, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// closeSMB logs a session off, waiting at most a few seconds for the server to answer; the connection closes either way.
func closeSMB(session *smb2.Session) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = session.WithContext(ctx).Logoff()
}

// remote turns a relative path into the share's backslash path.
func (b *smbBackend) remote(rel string) string {
	joined := strings.Trim(path.Join(strings.ReplaceAll(b.src.Dir, `\`, "/"), rel), "/")
	if joined == "." {
		return ""
	}
	return strings.ReplaceAll(joined, "/", `\`)
}

// connect returns the mounted share, dialing it first when needed.
func (b *smbBackend) connect() (*smb2.Share, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.share != nil {
		return b.share, nil
	}
	session, err := dialSMB(b.src.Host, b.src.User, b.secret, b.src.Domain)
	if err != nil {
		return nil, err
	}
	share, err := session.Mount(b.src.Share)
	if err != nil {
		closeSMB(session)
		return nil, err
	}
	b.session, b.share = session, share
	return share, nil
}

// reset drops the connection so the next call dials again.
func (b *smbBackend) reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.session != nil {
		closeSMB(b.session)
	}
	b.session, b.share = nil, nil
}

// with runs fn on the share, reconnecting once when the connection has gone away.
func (b *smbBackend) with(fn func(*smb2.Share) error) error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var share *smb2.Share
		if share, err = b.connect(); err != nil {
			return err
		}
		if err = fn(share); err == nil || isServerError(err) {
			return err
		}
		b.reset()
	}
	return err
}

// isServerError reports an error the server answered with, as opposed to a broken connection.
func isServerError(err error) bool {
	var response *smb2.ResponseError
	return errors.As(err, &response) || errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission)
}

// Walk lists video files under the share folder; unreadable subfolders are skipped.
func (b *smbBackend) Walk(fn func(rel string, size int64, mod time.Time)) error {
	return b.with(func(share *smb2.Share) error {
		var walk func(dir, rel string, depth int) error
		walk = func(dir, rel string, depth int) error {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			entries, err := share.WithContext(ctx).ReadDir(dir)
			cancel()
			if err != nil {
				return err
			}
			for _, entry := range entries {
				name := entry.Name()
				if skipName(name) {
					continue
				}
				child := name
				if dir != "" {
					child = dir + `\` + name
				}
				if entry.IsDir() {
					if depth < 16 && !skipDir(name) {
						if err := walk(child, path.Join(rel, name), depth+1); err != nil && !isServerError(err) {
							return err
						}
					}
				} else if isVideo(name) && !isSample(name, entry.Size()) {
					fn(path.Join(rel, name), entry.Size(), entry.ModTime())
				}
			}
			return nil
		}
		return walk(b.remote(""), "", 0)
	})
}

// Open opens a file on the share with read-ahead buffering, so streaming needs few round trips.
func (b *smbBackend) Open(ctx context.Context, rel string) (io.ReadSeekCloser, error) {
	var file *smb2.File
	err := b.with(func(share *smb2.Share) error {
		f, err := share.WithContext(ctx).Open(b.remote(rel))
		file = f
		return err
	})
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &readAhead{file: file, size: info.Size(), buf: make([]byte, 1<<20)}, nil
}

// LocalPath is empty: share files are not on this machine.
func (b *smbBackend) LocalPath(string) string { return "" }

// ExternalPath is the UNC path Windows can open directly.
func (b *smbBackend) ExternalPath(rel string) string {
	host := b.src.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	unc := `\\` + host + `\` + b.src.Share
	if r := b.remote(rel); r != "" {
		unc += `\` + r
	}
	return unc
}

// Close logs off the share.
func (b *smbBackend) Close() { b.reset() }

// readAhead serves small reads from one large buffered read, which matters over a network.
type readAhead struct {
	file  *smb2.File
	size  int64
	pos   int64
	buf   []byte
	start int64
	n     int
}

// Read copies from the buffer, refilling it at the current position when needed.
func (r *readAhead) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	if r.pos < r.start || r.pos >= r.start+int64(r.n) {
		n, err := r.file.ReadAt(r.buf, r.pos)
		if n == 0 {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		r.start, r.n = r.pos, n
	}
	copied := copy(p, r.buf[r.pos-r.start:r.n])
	r.pos += int64(copied)
	return copied, nil
}

// Seek moves the read position without touching the network.
func (r *readAhead) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		offset += r.pos
	case io.SeekEnd:
		offset += r.size
	}
	if offset < 0 {
		return 0, errors.New("negative seek position")
	}
	r.pos = offset
	return offset, nil
}

// Close closes the remote file.
func (r *readAhead) Close() error { return r.file.Close() }
