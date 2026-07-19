package stashgram

import (
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
// Media streaming server
// ============================================================================
//
// Exposes stored files directly over plain HTTP with Range support
// (via the standard library's http.ServeContent), so a video/audio file
// can be opened straight in a browser or handed to a media player (VLC,
// mpv, etc.) as a plain URL and scrubbed/seeked without downloading the
// whole thing first.
//
// This is the same underlying capability FTP (REST) and WebDAV (Range,
// via davFile's Seek) already have through FileSystem.ReadAt — this just
// exposes it as a single flat URL instead of a mount, which is what most
// media players/browsers expect for "paste a link and play". Every read
// goes through FileSystem.ReadAt, so it benefits from the same on-disk
// chunk cache as everything else: scrubbing back and forth in a player
// re-reads mostly-cached data instead of re-hitting Telegram, and a
// dropped/resumed stream picks up from cached chunks too.

// streamReadSeeker adapts FileSystem to io.ReadSeeker (what
// http.ServeContent needs) for a single remote file.
type streamReadSeeker struct {
	fs     *FileSystem
	path   string
	size   int64
	offset int64
}

func (s *streamReadSeeker) Read(p []byte) (int, error) {
	n, err := s.fs.ReadAt(s.path, p, s.offset)
	s.offset += int64(n)
	return n, err
}

func (s *streamReadSeeker) Seek(offset int64, whence int) (int64, error) {
	var newOff int64
	switch whence {
	case io.SeekStart:
		newOff = offset
	case io.SeekCurrent:
		newOff = s.offset + offset
	case io.SeekEnd:
		newOff = s.size + offset
	default:
		return 0, fmt.Errorf("invalid whence")
	}
	if newOff < 0 {
		return 0, fmt.Errorf("negative seek position")
	}
	s.offset = newOff
	return s.offset, nil
}

// ServeStream starts a plain HTTP server that streams any stored file at
// its remote path, e.g. http://host:port/movies/film.mkv. Range requests
// (seeking/scrubbing in a player) are handled by the standard library. If
// user/pass are non-empty, requests must present matching HTTP Basic
// credentials — same caveat as ServeWebDAV: this doesn't add transport
// encryption itself, so keep it on 127.0.0.1 + a tunnel, or behind a TLS
// reverse proxy, if it needs to be reachable beyond your LAN.
func ServeStream(fs *FileSystem, addr, user, pass string) error {
	if _, _, err := fs.getClient(); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		remotePath := normalizeStreamPath(r.URL.Path)
		if remotePath == "" {
			http.Error(w, "specify a file path, e.g. /movies/film.mkv", http.StatusBadRequest)
			return
		}

		entry, _, err := fs.lookupFile(remotePath)
		if err != nil || entry.IsFolder {
			http.NotFound(w, r)
			return
		}
		if entry.Incomplete {
			http.Error(w, "file is incomplete (upload in progress)", http.StatusServiceUnavailable)
			return
		}

		var size int64
		for _, c := range entry.Chunks {
			size += int64(c.Size)
		}

		if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(remotePath))); ct != "" {
			w.Header().Set("Content-Type", ct)
		}

		rs := &streamReadSeeker{fs: fs, path: remotePath, size: size}
		http.ServeContent(w, r, path.Base(remotePath), time.Unix(entry.Date, 0), rs)
	})

	var h http.Handler = mux
	if user != "" || pass != "" {
		h = basicAuth(mux, user, pass)
	}
	log.Printf("Streaming server ready — open a file with e.g. http://%s/path/to/file.mkv in a browser or media player", addr)
	return http.ListenAndServe(addr, h)
}

func normalizeStreamPath(p string) string {
	p = strings.TrimPrefix(p, "/")
	return normalizePath(p)
}
