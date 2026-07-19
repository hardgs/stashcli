package stashgram

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Data model
// ============================================================================

// ChunkFile describes one encrypted, uploaded piece of a file.
type ChunkFile struct {
	// Telegram document identifiers, populated from the SendMedia response
	// so a chunk can, in principle, be fetched directly by file location
	// instead of by searching — fewer round trips, which matters on
	// flaky/censored links. See extractDocumentInfo in filesystem.go.
	FileID        int64  `json:"file_id"`
	AccessHash    int64  `json:"access_hash"`
	FileReference string `json:"file_reference"`

	ChatID    int64  `json:"chat_id"`
	MessageID int    `json:"message_id"`
	Size      int32  `json:"file_size"` // plaintext size, used for output-file offsets
	Password  string `json:"password"`
	Hash      string `json:"hash"`
}

// FileEntry is one stored file, OR a virtual folder marker (IsFolder=true,
// no chunks). Incomplete marks a file whose upload was interrupted, so a
// retry can resume from the last successfully persisted chunk instead of
// re-uploading the whole thing.
type FileEntry struct {
	Chunks     []ChunkFile `json:"chunks,omitempty"`
	Date       int64       `json:"date"`
	FileHash   string      `json:"hash"`
	IsFolder   bool        `json:"is_folder,omitempty"`
	Incomplete bool        `json:"incomplete,omitempty"`
}

type Session struct {
	ChatIds []int64               `json:"chat_ids"`
	Files   map[string]*FileEntry `json:"files"`
}

type StorageData struct {
	Sessions map[string]*Session `json:"sessions"`
}

// FileItem is a single directory-listing entry.
type FileItem struct {
	Name     string
	IsFolder bool
}

// Storage is the on-disk JSON database of sessions/files.
type Storage struct {
	Path  string
	Files StorageData
	mu    sync.Mutex

	lastLoad time.Time // mtime of Path as of the last successful Load, used by LoadIfChanged

	asyncOnce sync.Once
	dirty     bool
	asyncErr  error
}

// ============================================================================
// Load / Save
// ============================================================================

func (s *Storage) Load(p string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Path = p

	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	var fresh StorageData
	if err := json.Unmarshal(data, &fresh); err != nil {
		return err
	}
	if fresh.Sessions == nil {
		fresh.Sessions = make(map[string]*Session)
	}
	for _, sess := range fresh.Sessions {
		if sess.Files == nil {
			sess.Files = make(map[string]*FileEntry)
		}
	}
	s.Files = fresh
	if info, err := os.Stat(p); err == nil {
		s.lastLoad = info.ModTime()
	} else {
		s.lastLoad = time.Now()
	}
	return nil
}

// LoadIfChanged reloads storage from disk only if the file's mtime has
// moved on since the last successful Load/Save by this Storage instance.
// Every FileSystem operation (upload, download, list, ...) used to call
// Load() unconditionally, which meant re-reading and re-parsing the whole
// storage.json from disk on every single call — wasteful, and it adds up
// fast for things like directory listings over FTP/WebDAV, which can fire
// many times a second. This makes those calls a no-op in the common case
// where nothing else has touched the file since we last saw it.
func (s *Storage) LoadIfChanged(p string) error {
	info, err := os.Stat(p)
	if err != nil {
		return err
	}

	s.mu.Lock()
	samePath := s.Path == p
	upToDate := samePath && !s.lastLoad.IsZero() && !info.ModTime().After(s.lastLoad)
	s.mu.Unlock()

	if upToDate {
		return nil
	}
	return s.Load(p)
}

// Save writes storage back to disk atomically (write to a temp file, then
// rename over the original) so a crash or power loss mid-write can't leave
// storage.json half-written and unparseable.
func (s *Storage) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveLocked()
}

func (s *Storage) saveLocked() error {
	if s.Path == "" {
		return fmt.Errorf("storage path not set")
	}
	data, err := json.MarshalIndent(s.Files, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.Path); err != nil {
		return err
	}
	s.lastLoad = time.Now()
	s.dirty = false
	return nil
}

// ensureAsyncSaver starts (once, lazily) a background goroutine that
// flushes pending changes to disk shortly after they happen, instead of
// every AddFileAsync call blocking its caller on a synchronous disk write.
// This is what makes parallel chunk uploads "full async": each worker
// finishes its chunk and moves on immediately; a save happens in the
// background, debounced, without becoming a bottleneck shared across every
// goroutine.
func (s *Storage) ensureAsyncSaver() {
	s.asyncOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for range ticker.C {
				s.mu.Lock()
				if s.dirty {
					if err := s.saveLocked(); err != nil {
						s.asyncErr = err
					}
				}
				s.mu.Unlock()
			}
		}()
	})
}

// ============================================================================
// Path helpers / virtual folders
// ============================================================================

func normalizePath(p string) string {
	p = strings.TrimPrefix(p, "/")
	p = path.Clean(p)
	if p == "." {
		return ""
	}
	return p
}

// ensureFolders makes every ancestor directory of remotePath exist as a
// virtual folder marker, so `ls` on an intermediate directory works even if
// the user never ran `mkdir` on it explicitly.
func ensureFolders(sess *Session, remotePath string) {
	dir := path.Dir(remotePath)
	for dir != "." && dir != "/" && dir != "" {
		if _, ok := sess.Files[dir]; !ok {
			sess.Files[dir] = &FileEntry{IsFolder: true, Date: time.Now().Unix()}
		}
		dir = path.Dir(dir)
	}
}

// Mkdir creates a virtual folder — just a metadata marker, no Telegram
// upload involved.
func (s *Storage) Mkdir(remotePath, sessKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.Files.Sessions[sessKey]
	if !ok {
		return fmt.Errorf("session '%s' not found", sessKey)
	}
	remotePath = normalizePath(remotePath)
	if remotePath == "" {
		return nil
	}
	if existing, exists := sess.Files[remotePath]; !exists || !existing.IsFolder {
		sess.Files[remotePath] = &FileEntry{IsFolder: true, Date: time.Now().Unix()}
	}
	ensureFolders(sess, remotePath)
	return s.saveLocked()
}

// ============================================================================
// File CRUD
// ============================================================================

// AddFile stores (or overwrites) entry at remotePath, creating any parent
// virtual folders needed, and saves synchronously. Used for the final,
// authoritative write of a file entry (upload init and completion), where
// we want to be sure it actually hit disk before returning.
func (s *Storage) AddFile(entry *FileEntry, remotePath, sessKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.Files.Sessions[sessKey]
	if !ok {
		return fmt.Errorf("session '%s' not found", sessKey)
	}
	remotePath = normalizePath(remotePath)
	sess.Files[remotePath] = entry
	ensureFolders(sess, remotePath)
	return s.saveLocked()
}

// AddFileAsync behaves like AddFile but returns immediately, leaving a
// background goroutine (see ensureAsyncSaver) to flush the change to disk
// shortly after. Used for the frequent "chunk N/M uploaded" progress
// updates fired by parallel upload workers, so those don't serialize on a
// disk write each. The very last write for a given file always goes
// through the synchronous AddFile, so completion is never silently lost.
func (s *Storage) AddFileAsync(entry *FileEntry, remotePath, sessKey string) error {
	s.mu.Lock()
	sess, ok := s.Files.Sessions[sessKey]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("session '%s' not found", sessKey)
	}
	remotePath = normalizePath(remotePath)
	sess.Files[remotePath] = entry
	ensureFolders(sess, remotePath)
	s.dirty = true
	s.mu.Unlock()

	s.ensureAsyncSaver()
	return nil
}

// GetEntry looks up a single path within a specific session (used to detect
// resumable/incomplete uploads, and by the WebDAV/FTP Stat implementations).
func (s *Storage) GetEntry(remotePath, sessKey string) (*FileEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.Files.Sessions[sessKey]
	if !ok {
		return nil, false
	}
	e, ok := sess.Files[normalizePath(remotePath)]
	return e, ok
}

// RemoveFile deletes remotePath. If it's a folder, every descendant path is
// removed too, in the same locked pass and a single disk save — previously
// only the folder's own marker entry was removed, leaving every file that
// had been inside it as an orphaned, inaccessible-but-still-stored entry
// (WebDAV's RemoveAll hit this; see webdav.go). Callers that walked the
// tree themselves and called this once per child (FTP's RemoveAll used to)
// can now just call it once on the root and let this handle recursion.
func (s *Storage) RemoveFile(remotePath, sessKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.Files.Sessions[sessKey]
	if !ok {
		return fmt.Errorf("session '%s' not found", sessKey)
	}
	remotePath = normalizePath(remotePath)
	entry, ok := sess.Files[remotePath]
	if !ok {
		return fmt.Errorf("not found: %s", remotePath)
	}

	delete(sess.Files, remotePath)
	if entry.IsFolder {
		prefix := remotePath + "/"
		for p := range sess.Files {
			if strings.HasPrefix(p, prefix) {
				delete(sess.Files, p)
			}
		}
	}
	return s.saveLocked()
	// Note: this only removes metadata. The underlying Telegram messages
	// are left in place (they're already random-named + encrypted, so
	// leaving them is harmless) — add a DeleteMessages call in
	// FileSystem.Delete if you want a full remote purge too.
}

// RenameFile moves metadata from oldPath to newPath. If oldPath is a
// folder, every descendant path is moved along with it, in the same locked
// pass and a single disk save — previously a folder rename only moved the
// folder's own marker, silently leaving every file inside it behind under
// the old (now-orphaned) path. This is still a rename in the virtual
// namespace only: the underlying Telegram messages/chunks are untouched,
// so it's instant regardless of file size or how many files are inside a
// renamed folder.
func (s *Storage) RenameFile(oldPath, newPath, sessKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.Files.Sessions[sessKey]
	if !ok {
		return fmt.Errorf("session '%s' not found", sessKey)
	}
	oldPath = normalizePath(oldPath)
	newPath = normalizePath(newPath)
	entry, ok := sess.Files[oldPath]
	if !ok {
		return fmt.Errorf("not found: %s", oldPath)
	}

	moves := map[string]string{oldPath: newPath}
	if entry.IsFolder {
		prefix := oldPath + "/"
		for p := range sess.Files {
			if strings.HasPrefix(p, prefix) {
				moves[p] = newPath + "/" + strings.TrimPrefix(p, prefix)
			}
		}
	}
	for from, to := range moves {
		sess.Files[to] = sess.Files[from]
		delete(sess.Files, from)
	}
	ensureFolders(sess, newPath)
	return s.saveLocked()
}

// ============================================================================
// Listing
// ============================================================================

// ShowList lists the immediate children (files and virtual folders) of
// remotePath, across all sessions. Entries still mid-upload (Incomplete)
// are hidden so partial files don't show up as if they were ready.
func (s *Storage) ShowList(remotePath string) []FileItem {
	s.mu.Lock()
	defer s.mu.Unlock()

	remotePath = normalizePath(remotePath)
	seen := map[string]FileItem{}
	for _, sess := range s.Files.Sessions {
		for p, entry := range sess.Files {
			if entry.Incomplete {
				continue
			}
			dir := path.Dir(p)
			if dir == "." {
				dir = ""
			}
			if dir != remotePath {
				continue
			}
			name := path.Base(p)
			seen[name] = FileItem{Name: name, IsFolder: entry.IsFolder}
		}
	}
	items := make([]FileItem, 0, len(seen))
	for _, it := range seen {
		items = append(items, it)
	}
	return items
}

// CountChildren returns how many files and how many folders live anywhere
// underneath remotePath — at ANY depth, not just immediate children — across
// every session. remotePath == "" counts everything in the whole store
// (used for `stashcli info` on the root). This is what backs the "how many
// files inside this folder" metadata: previously there was no way to answer
// that without listing every level by hand. Entries still mid-upload
// (Incomplete) are not counted, matching ShowList.
func (s *Storage) CountChildren(remotePath string) (files int, folders int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	remotePath = normalizePath(remotePath)
	prefix := remotePath + "/"
	if remotePath == "" {
		prefix = ""
	}

	for _, sess := range s.Files.Sessions {
		for p, entry := range sess.Files {
			if entry.Incomplete {
				continue
			}
			if p == remotePath {
				continue // the folder itself, not one of its children
			}
			if prefix != "" && !strings.HasPrefix(p, prefix) {
				continue
			}
			if entry.IsFolder {
				folders++
			} else {
				files++
			}
		}
	}
	return
}
