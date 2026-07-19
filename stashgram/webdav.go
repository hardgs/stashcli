package stashgram

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"
)

// ============================================================================
// WebDAV server
// ============================================================================
//
// This lets stashcli be mounted as a normal-looking drive (Finder, Windows
// Explorer, rclone, davfs2, etc.) instead of using the CLI directly.
//
// Memory note: reads go through FileSystem.ReadAt, which serves from the
// on-disk chunk cache whenever possible and otherwise pulls one chunk at a
// time — safe as-is. Writes are the trickier case: a naive implementation
// would buffer the whole uploaded file before handing it to Upload(), which
// doesn't scale for very large files on a low-RAM box. davFile buffers
// writes to a temp file on disk instead (also required for Windows'
// WebDAV client, which sends partial PUT requests with Content-Range
// headers), then uploads the complete file — in parallel, fixed-size
// chunks — when Close() is called.
//
// Note on notifications: a WebDAV client's PUT only gets its response once
// Close() returns below, and Close() blocks on fs.Upload() — which only
// returns once every chunk has landed on Telegram (or failed). So clients
// already only see "upload finished" after the *entire* file is done, never
// partway through. Stat()/OpenFile() additionally make sure nothing else
// browsing the same mount mid-upload can see or open the file early.
//
// Note: this handler has no built-in auth beyond the optional basic-auth
// wrapper below. Don't bind it to a public interface without at least that;
// binding to 127.0.0.1 and reaching it over an SSH tunnel, or putting a
// reverse proxy with real auth/TLS in front, is the safer default.

// ServeWebDAV starts a WebDAV server backed by fs. If user/pass are
// non-empty, requests must present matching HTTP Basic credentials.
func ServeWebDAV(fs *FileSystem, addr, user, pass string) error {
	_, sessKey, err := fs.getClient()
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	handler := &webdav.Handler{
		FileSystem: &davFS{fs: fs, sessKey: sessKey},
		LockSystem: webdav.NewMemLS(),
		Logger: func(r *http.Request, err error) {
			if err != nil {
				log.Printf("webdav %s %s: %v", r.Method, r.URL.Path, err)
			}
		},
	}

	var h http.Handler = handler
	if user != "" || pass != "" {
		h = basicAuth(handler, user, pass)
	}
	return http.ListenAndServe(addr, h)
}

func basicAuth(next http.Handler, user, pass string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != pass {
			w.Header().Set("WWW-Authenticate", `Basic realm="stashcli"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ============================================================================
// webdav.FileSystem implementation
// ============================================================================

type davFS struct {
	fs      *FileSystem
	sessKey string
}

func normalizeDavName(name string) string {
	name = strings.TrimPrefix(name, "/")
	name = path.Clean(name)
	if name == "." {
		return ""
	}
	return name
}

func (d *davFS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	return d.fs.Mkdir(normalizeDavName(name))
}

// RemoveAll deletes name and, if it's a folder, everything under it.
// Storage.RemoveFile now does that recursion itself in a single locked
// pass + single disk save (previously this only removed the folder's own
// marker entry, silently leaving every file that had been inside it as an
// orphaned entry nobody could see or reach again).
func (d *davFS) RemoveAll(ctx context.Context, name string) error {
	return d.fs.Delete(normalizeDavName(name))
}

func (d *davFS) Rename(ctx context.Context, oldName, newName string) error {
	return d.fs.Rename(normalizeDavName(oldName), normalizeDavName(newName))
}

// Stat implements per-path metadata lookups (WebDAV PROPFIND on a single
// item). A file that's still mid-upload (Incomplete) is reported as not
// existing — same treatment as ShowList/`ls` — so a client can never see a
// partially-uploaded file appear early or report the wrong (partial,
// still-growing) size while the upload is still running.
func (d *davFS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	name = normalizeDavName(name)
	if name == "" {
		return davFileInfo{name: "/", isDir: true, modTime: time.Now()}, nil
	}
	entry, ok := d.fs.Storage.GetEntry(name, d.sessKey)
	if !ok || entry.Incomplete {
		return nil, os.ErrNotExist
	}
	return davFileInfo{
		name:    path.Base(name),
		isDir:   entry.IsFolder,
		size:    davEntrySize(entry),
		modTime: time.Unix(entry.Date, 0),
	}, nil
}

func (d *davFS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	name = normalizeDavName(name)
	writing := flag&(os.O_WRONLY|os.O_RDWR) != 0 || flag&os.O_CREATE != 0

	if name == "" {
		return &davFile{fs: d.fs, sessKey: d.sessKey, name: "", isDir: true}, nil
	}

	entry, exists := d.fs.Storage.GetEntry(name, d.sessKey)

	if writing && (!exists || !entry.IsFolder) {
		return newDavWriteFile(d.fs, name), nil
	}

	if !exists {
		return nil, os.ErrNotExist
	}
	if entry.Incomplete {
		// Still uploading — treat exactly like "doesn't exist yet" so a
		// client can't open/read a partial file mid-transfer. It becomes
		// visible in one step, the instant the whole upload finishes.
		return nil, os.ErrNotExist
	}
	if entry.IsFolder {
		return &davFile{fs: d.fs, sessKey: d.sessKey, name: name, isDir: true}, nil
	}
	return &davFile{fs: d.fs, sessKey: d.sessKey, name: name, size: davEntrySize(entry)}, nil
}

// davEntrySize sums an entry's chunk sizes; 0 for folders.
func davEntrySize(entry *FileEntry) int64 {
	if entry == nil || entry.IsFolder {
		return 0
	}
	var size int64
	for _, c := range entry.Chunks {
		size += int64(c.Size)
	}
	return size
}

// ============================================================================
// webdav.File implementation
// ============================================================================

type davFileInfo struct {
	name    string
	isDir   bool
	size    int64
	modTime time.Time
}

func (fi davFileInfo) Name() string { return fi.name }
func (fi davFileInfo) Size() int64  { return fi.size }
func (fi davFileInfo) Mode() os.FileMode {
	if fi.isDir {
		return os.ModeDir | 0755
	}
	return 0644
}
func (fi davFileInfo) ModTime() time.Time { return fi.modTime }
func (fi davFileInfo) IsDir() bool        { return fi.isDir }
func (fi davFileInfo) Sys() interface{}   { return nil }

type davFile struct {
	fs      *FileSystem
	sessKey string
	name    string
	isDir   bool
	size    int64
	off     int64

	// For writing: buffer to temp file, then upload on Close()
	tempFile *os.File
	mu       sync.Mutex
	closed   bool
}

// newDavWriteFile creates a temp file for buffering writes, then uploads
// the complete file when Close() is called. This handles Windows WebDAV
// client's partial PUT requests properly.
func newDavWriteFile(fs *FileSystem, name string) *davFile {
	tmpFile, err := os.CreateTemp("", "stashcli-webdav-*")
	if err != nil {
		log.Printf("failed to create temp file for %s: %v", name, err)
		return &davFile{fs: fs, name: name, closed: true} // mark closed to fail writes
	}

	return &davFile{
		fs:       fs,
		name:     name,
		tempFile: tmpFile,
	}
}

func (f *davFile) Read(p []byte) (int, error) {
	if f.isDir {
		return 0, io.EOF
	}
	n, err := f.fs.ReadAt(f.name, p, f.off)
	f.off += int64(n)
	return n, err
}

func (f *davFile) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return 0, fmt.Errorf("file closed")
	}
	if f.tempFile == nil {
		return 0, fmt.Errorf("file not open for writing")
	}

	n, err := f.tempFile.Write(p)
	if err != nil {
		return n, err
	}
	return n, nil
}

func (f *davFile) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		f.off = offset
	case io.SeekCurrent:
		f.off += offset
	case io.SeekEnd:
		f.off = f.size + offset
	}
	return f.off, nil
}

func (f *davFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return nil
	}
	f.closed = true

	if f.tempFile == nil {
		return nil // read-only file, nothing to close
	}

	tmpPath := f.tempFile.Name()
	defer os.Remove(tmpPath) // clean up temp file

	// Close temp file to flush all data
	if err := f.tempFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	// Check if file has any content
	stat, err := os.Stat(tmpPath)
	if err != nil {
		return fmt.Errorf("stat temp file: %w", err)
	}
	if stat.Size() == 0 {
		return fmt.Errorf("uploaded file is empty")
	}

	// Now upload the complete file using the normal Upload() method — 0
	// means "use the fixed chunk size from settings.json", and Upload
	// splits/sends chunks in parallel (fs.UploadConcurrency workers).
	log.Printf("Uploading %s (%d bytes) from temp file...", f.name, stat.Size())
	if err := f.fs.Upload(tmpPath, f.name, 0); err != nil {
		return fmt.Errorf("upload %s: %w", f.name, err)
	}

	log.Printf("Upload complete: %s", f.name)
	return nil
}

// Readdir populates real size + modtime for every entry (files AND
// folders) by looking each one up in storage — previously this only set
// name/isDir, so every listed item showed size 0 and an empty/zero
// modtime in WebDAV clients (Finder, Explorer, rclone, ...), unlike the
// FTP server which already had this. Now both match. Items still
// mid-upload never reach here (fs.List/ShowList already excludes them);
// the Incomplete check is a defensive second layer.
func (f *davFile) Readdir(count int) ([]os.FileInfo, error) {
	items := f.fs.List(f.name)
	infos := make([]os.FileInfo, 0, len(items))
	for _, it := range items {
		full := path.Join(f.name, it.Name)
		modTime := time.Now()
		var size int64
		if entry, ok := f.fs.Storage.GetEntry(full, f.sessKey); ok && !entry.Incomplete {
			modTime = time.Unix(entry.Date, 0)
			size = davEntrySize(entry)
		}
		infos = append(infos, davFileInfo{name: it.Name, isDir: it.IsFolder, size: size, modTime: modTime})
	}
	return infos, nil
}

func (f *davFile) Stat() (os.FileInfo, error) {
	if f.isDir {
		return davFileInfo{name: path.Base(f.name), isDir: true}, nil
	}
	return davFileInfo{name: path.Base(f.name), size: f.size}, nil
}
