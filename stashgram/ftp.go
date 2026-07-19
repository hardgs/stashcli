package stashgram

import (
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	ftpserver "github.com/fclairamb/ftpserverlib"
	"github.com/spf13/afero"
)

// ============================================================================
// FTP server
// ============================================================================
//
// Built on github.com/fclairamb/ftpserverlib: actively maintained, small
// memory footprint, and supports:
//   - MLSD (machine-readable listing) — many modern/Windows FTP clients
//     fall back to fragile LIST-text parsing without it, which is a common
//     cause of directories looking "broken" or empty in some clients.
//   - A real passive-port-range + public-IP setting, so passive mode works
//     from behind a home router/NAT.
//   - Upload-abort detection (FileTransferError), so an interrupted upload
//     doesn't get pushed to Telegram at all.

// ServeFTP starts an FTP server bound to addr:port, with optional auth.
// publicIP should be your public/router IP if the server is reachable from
// outside your LAN (needed for passive mode to work through NAT) — leave it
// empty for local-only use (127.0.0.1 or same-LAN clients).
// passivePorts is a "min-max" range, e.g. "30000-30050"; leave empty to let
// the OS pick a random port per transfer (fine for LAN/local use, but you
// should set it and forward that range on your router for WAN access).
func ServeFTP(fs *FileSystem, addr string, port int, user, pass string, passivePorts string, publicIP string) error {
	_, sessKey, err := fs.getClient()
	if err != nil {
		return err
	}

	settings := &ftpserver.Settings{
		ListenAddr:          fmt.Sprintf("%s:%d", addr, port),
		PublicHost:          publicIP,
		IdleTimeout:         15 * 60, // 15 min — avoid piling up dead half-open connections
		ConnectionTimeout:   30,
		DefaultTransferType: ftpserver.TransferTypeBinary,
		DisableActiveMode:   false,
	}

	if passivePorts != "" {
		start, end, err := parsePortRange(passivePorts)
		if err != nil {
			return fmt.Errorf("invalid --passive-ports %q: %w", passivePorts, err)
		}
		settings.PassiveTransferPortRange = &ftpserver.PortRange{Start: start, End: end}
	}

	if publicIP == "" {
		log.Println("FTP: no --public-ip set. If clients connect from outside your LAN, passive-mode transfers will likely fail (they'll see LIST/control working but uploads/downloads hang). Set --public-ip to your router's public IP and forward --passive-ports on it.")
	}

	driver := &ftpMainDriver{
		fs:       fs,
		sessKey:  sessKey,
		user:     user,
		pass:     pass,
		settings: settings,
	}
	if user == "" && pass == "" {
		log.Println("FTP server: no --user/--pass set — accepting any credentials.")
	}

	server := ftpserver.NewFtpServer(driver)
	return server.ListenAndServe()
}

func parsePortRange(s string) (int, int, error) {
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expected format min-max")
	}
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return 0, 0, err
	}
	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return 0, 0, err
	}
	if end < start {
		return 0, 0, fmt.Errorf("end must be >= start")
	}
	return start, end, nil
}

// ----------------------------------------------------------------------------
// MainDriver — authentication and per-connection setup
// ----------------------------------------------------------------------------

type ftpMainDriver struct {
	fs       *FileSystem
	sessKey  string
	user     string
	pass     string
	settings *ftpserver.Settings
}

func (d *ftpMainDriver) GetSettings() (*ftpserver.Settings, error) {
	return d.settings, nil
}

func (d *ftpMainDriver) ClientConnected(cc ftpserver.ClientContext) (string, error) {
	return "stashcli FTP — Telegram-backed storage", nil
}

func (d *ftpMainDriver) ClientDisconnected(cc ftpserver.ClientContext) {}

func (d *ftpMainDriver) AuthUser(cc ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	if d.user != "" || d.pass != "" {
		if user != d.user || pass != d.pass {
			return nil, fmt.Errorf("invalid username or password")
		}
	}
	return &ftpClientDriver{fs: d.fs, sessKey: d.sessKey}, nil
}

func (d *ftpMainDriver) GetTLSConfig() (*tls.Config, error) {
	return nil, nil // no FTPS/AUTH TLS support configured
}

// ----------------------------------------------------------------------------
// ClientDriver (afero.Fs) — path/metadata operations
// ----------------------------------------------------------------------------
//
// Actual file transfers go through GetHandle (ClientDriverExtentionFileTransfer)
// and directory listings go through ReadDir (ClientDriverExtensionFileList),
// both below — those are what the library actually calls in normal use. The
// afero.Fs methods (Open/OpenFile/Create) still have to exist to satisfy the
// interface at compile time, but are never reached in practice.

type ftpClientDriver struct {
	fs      *FileSystem
	sessKey string
}

func (d *ftpClientDriver) Name() string { return "stashgram" }

func normalizeFTPPath(p string) string {
	if p == "" || p == "/" {
		return ""
	}
	p = path.Clean(p)
	if p == "." || p == "/" {
		return ""
	}
	return strings.TrimPrefix(p, "/")
}

// entrySize sums an entry's chunk sizes; 0 for folders.
func entrySize(entry *FileEntry) int64 {
	if entry == nil || entry.IsFolder {
		return 0
	}
	var size int64
	for _, c := range entry.Chunks {
		size += int64(c.Size)
	}
	return size
}

// Stat implements STAT/single-file lookups. A file that's still mid-upload
// (Incomplete) is reported as not existing — same treatment as ReadDir/`ls`
// — so an FTP client can never see a partially-uploaded file appear early
// or report the wrong (partial, still-growing) size while the upload is
// still in progress. It becomes visible in one step, the instant the whole
// file finishes uploading.
func (d *ftpClientDriver) Stat(name string) (os.FileInfo, error) {
	name = normalizeFTPPath(name)
	if name == "" {
		return &ftpFileInfo{name: "/", isDir: true, modTime: time.Now()}, nil
	}
	entry, ok := d.fs.Storage.GetEntry(name, d.sessKey)
	if !ok || entry.Incomplete {
		return nil, os.ErrNotExist
	}
	return &ftpFileInfo{
		name:    path.Base(name),
		isDir:   entry.IsFolder,
		size:    entrySize(entry),
		modTime: time.Unix(entry.Date, 0),
	}, nil
}

// ReadDir implements ClientDriverExtensionFileList — used for LIST/MLSD.
// Populates real size + modtime for every entry (files AND folders) by
// looking each one up in storage, rather than defaulting folders to
// time.Now() — this is what makes `ls` show accurate metadata instead of
// just names. Items still mid-upload never reach here in the first place
// (fs.List/ShowList already excludes them), but the Incomplete check is
// kept as a defensive second layer in case that ever changes.
func (d *ftpClientDriver) ReadDir(name string) ([]os.FileInfo, error) {
	name = normalizeFTPPath(name)
	items := d.fs.List(name)
	infos := make([]os.FileInfo, 0, len(items))
	for _, it := range items {
		full := normalizeFTPPath(path.Join(name, it.Name))
		modTime := time.Now()
		var size int64
		if entry, ok := d.fs.Storage.GetEntry(full, d.sessKey); ok && !entry.Incomplete {
			modTime = time.Unix(entry.Date, 0)
			size = entrySize(entry)
		}
		infos = append(infos, &ftpFileInfo{name: it.Name, isDir: it.IsFolder, size: size, modTime: modTime})
	}
	return infos, nil
}

// GetHandle implements ClientDriverExtentionFileTransfer — used for
// RETR/STOR/APPE/REST, no need to implement a full afero.File for transfers.
func (d *ftpClientDriver) GetHandle(name string, flags int, offset int64) (ftpserver.FileTransfer, error) {
	name = normalizeFTPPath(name)

	if flags&(os.O_WRONLY|os.O_RDWR) != 0 {
		if flags&os.O_APPEND != 0 {
			return nil, fmt.Errorf("resuming/appending an existing upload is not supported; re-upload the whole file")
		}
		return newFTPUploadHandle(d.fs, name)
	}

	entry, ok := d.fs.Storage.GetEntry(name, d.sessKey)
	if !ok {
		return nil, os.ErrNotExist
	}
	if entry.IsFolder {
		return nil, fmt.Errorf("'%s' is a folder", name)
	}
	if entry.Incomplete {
		return nil, fmt.Errorf("file is incomplete (upload in progress)")
	}
	size := entrySize(entry)
	if offset > size {
		return nil, os.ErrInvalid
	}
	return &ftpDownloadHandle{fs: d.fs, path: name, off: offset, size: size}, nil
}

// Remove handles DELE (file delete only — see RemoveDir for RMD).
func (d *ftpClientDriver) Remove(name string) error {
	name = normalizeFTPPath(name)
	entry, ok := d.fs.Storage.GetEntry(name, d.sessKey)
	if !ok || entry.IsFolder {
		return os.ErrNotExist
	}
	return d.fs.Delete(name)
}

// RemoveDir implements ClientDriverExtensionRemoveDir, handling RMD
// separately from DELE so we can refuse to remove non-empty folders — the
// conventional, safer FTP RMD behavior (use RemoveAll/a recursive client
// command to force-remove a non-empty folder).
func (d *ftpClientDriver) RemoveDir(name string) error {
	name = normalizeFTPPath(name)
	entry, ok := d.fs.Storage.GetEntry(name, d.sessKey)
	if !ok || !entry.IsFolder {
		return os.ErrNotExist
	}
	if items := d.fs.List(name); len(items) > 0 {
		return fmt.Errorf("directory not empty")
	}
	return d.fs.Delete(name)
}

// RemoveAll is required by afero.Fs; recursively deletes name and
// everything under it. Previously this walked the tree itself and called
// fs.Delete once per child — one disk save per file. Storage.RemoveFile now
// handles the recursion itself in a single locked pass + single disk save,
// so this is just one call regardless of how many files are inside.
func (d *ftpClientDriver) RemoveAll(name string) error {
	name = normalizeFTPPath(name)
	return d.fs.Delete(name)
}

func (d *ftpClientDriver) Rename(oldname, newname string) error {
	return d.fs.Rename(normalizeFTPPath(oldname), normalizeFTPPath(newname))
}

func (d *ftpClientDriver) Mkdir(name string, perm os.FileMode) error {
	return d.fs.Mkdir(normalizeFTPPath(name))
}

func (d *ftpClientDriver) MkdirAll(name string, perm os.FileMode) error {
	return d.Mkdir(name, perm) // Storage.Mkdir already creates ancestor folders
}

func (d *ftpClientDriver) Chmod(name string, mode os.FileMode) error { return nil } // no permission model
func (d *ftpClientDriver) Chown(name string, uid, gid int) error     { return nil } // no ownership model
func (d *ftpClientDriver) Chtimes(name string, atime, mtime time.Time) error {
	return nil // mtimes come from Telegram message dates, not user-settable
}

// Open/OpenFile/Create exist only to satisfy afero.Fs at compile time — see
// the comment above the type. GetHandle/ReadDir handle everything the
// library actually calls.
func (d *ftpClientDriver) Open(name string) (afero.File, error) {
	return nil, fmt.Errorf("not supported; use RETR/LIST")
}
func (d *ftpClientDriver) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	return nil, fmt.Errorf("not supported; use RETR/STOR")
}
func (d *ftpClientDriver) Create(name string) (afero.File, error) {
	return nil, fmt.Errorf("not supported; use STOR")
}

// ----------------------------------------------------------------------------
// FileTransfer implementations
// ----------------------------------------------------------------------------

// ftpDownloadHandle streams a remote file via FileSystem.ReadAt, which
// serves from the on-disk chunk cache whenever possible.
type ftpDownloadHandle struct {
	fs   *FileSystem
	path string
	off  int64
	size int64
}

func (h *ftpDownloadHandle) Read(p []byte) (int, error) {
	n, err := h.fs.ReadAt(h.path, p, h.off)
	h.off += int64(n)
	return n, err
}

func (h *ftpDownloadHandle) Write(p []byte) (int, error) {
	return 0, fmt.Errorf("file opened read-only")
}

func (h *ftpDownloadHandle) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		h.off = offset
	case io.SeekCurrent:
		h.off += offset
	case io.SeekEnd:
		h.off = h.size + offset
	}
	return h.off, nil
}

func (h *ftpDownloadHandle) Close() error { return nil }

// ftpUploadHandle buffers the incoming STOR to a temp file on disk (not in
// RAM), then uploads the complete file on Close via the parallel, resumable
// upload path in filesystem.go. If the transfer is interrupted
// (TransferError is called by the library before Close whenever that
// happens — dropped connection, ABOR, I/O error), we skip the Telegram
// upload entirely instead of paying to push a file nobody will ever finish
// downloading.
//
// Note on notifications: the FTP STOR command's success reply is only sent
// back to the client once Close() returns — and Close() blocks on
// fs.Upload(), which only returns once every chunk has landed on Telegram
// (or failed). So the client's "upload complete" notification already only
// fires after the *entire* file is uploaded, never partway through; the
// Stat() fix above additionally makes sure nothing else polling the same
// path mid-upload sees it either.
type ftpUploadHandle struct {
	fs      *FileSystem
	path    string
	tmpFile *os.File
	tmpPath string
	aborted bool
}

func newFTPUploadHandle(fs *FileSystem, remotePath string) (*ftpUploadHandle, error) {
	f, err := os.CreateTemp("", "stashcli-ftp-*")
	if err != nil {
		return nil, err
	}
	return &ftpUploadHandle{fs: fs, path: remotePath, tmpFile: f, tmpPath: f.Name()}, nil
}

func (h *ftpUploadHandle) Read(p []byte) (int, error) {
	return 0, fmt.Errorf("file opened write-only")
}

func (h *ftpUploadHandle) Write(p []byte) (int, error) {
	return h.tmpFile.Write(p)
}

func (h *ftpUploadHandle) Seek(offset int64, whence int) (int64, error) {
	return h.tmpFile.Seek(offset, whence)
}

// TransferError implements ftpserver.FileTransferError.
func (h *ftpUploadHandle) TransferError(err error) {
	h.aborted = true
}

func (h *ftpUploadHandle) Close() error {
	tmpPath := h.tmpPath
	defer os.Remove(tmpPath)

	if err := h.tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if h.aborted {
		log.Printf("upload of %s was interrupted — discarding, nothing sent to Telegram", h.path)
		return nil
	}

	stat, err := os.Stat(tmpPath)
	if err != nil {
		return fmt.Errorf("stat temp file: %w", err)
	}
	if stat.Size() == 0 {
		return fmt.Errorf("uploaded file is empty")
	}

	if err := h.fs.Upload(tmpPath, h.path, 0); err != nil {
		return fmt.Errorf("upload %s: %w", h.path, err)
	}
	return nil
}

// ----------------------------------------------------------------------------
// FileInfo implementation
// ----------------------------------------------------------------------------

type ftpFileInfo struct {
	name    string
	size    int64
	isDir   bool
	modTime time.Time
}

func (fi *ftpFileInfo) Name() string       { return fi.name }
func (fi *ftpFileInfo) Size() int64        { return fi.size }
func (fi *ftpFileInfo) ModTime() time.Time { return fi.modTime }
func (fi *ftpFileInfo) IsDir() bool        { return fi.isDir }
func (fi *ftpFileInfo) Sys() interface{}   { return nil }
func (fi *ftpFileInfo) Mode() os.FileMode {
	if fi.isDir {
		return os.ModeDir | 0755
	}
	return 0644
}
