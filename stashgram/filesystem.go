package stashgram

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amarnathcjd/gogram/telegram"
)

const (
	MinChunkSize int64 = 1 * 1024 * 1024
	// DefaultChunkSize is used only if settings.json leaves
	// upload_chunk_size unset (<=0). Previously this was 6MB, which meant
	// anyone who hadn't explicitly set "uploadchunksize" in settings.json
	// got small, chatty chunks (e.g. an 80MB movie -> ~13+ separate
	// Telegram messages instead of 1-2). Now the out-of-the-box default is
	// a fixed 450MB chunk, so a single-chunk (or near single-chunk) upload
	// is the common case on any reasonably fast connection, with zero
	// settings.json edits required.
	//
	// If your connection is slow/flaky, set "uploadchunksize" in
	// settings.json to something smaller (6-64MB range is a good starting
	// point) — see the trade-off note on Settings.UploadChunkSize in
	// configs.go: a dropped transfer mid-chunk means re-doing that entire
	// chunk, so smaller chunks are safer on bad connections even though
	// they mean more round trips overall.
	DefaultChunkSize int64 = 450 * 1024 * 1024
	// MaxChunkSize is a hard ceiling, kept well above the default so
	// settings.json can opt into even larger fixed chunks on a very fast,
	// stable connection (e.g. a VPS-to-VPS transfer) if you want to.
	MaxChunkSize int64 = 2000 * 1024 * 1024

	UploadThreadsPerChunk = 2
	UploadInternalChunk   = 512 * 1024

	// DownloadThreadsPerChunk/MaxConcurrentChunks/MaxConcurrentUploadChunks
	// are hard ceilings; the actual concurrency used is whatever
	// settings.json's parral_download/parral_upload asks for, clamped to
	// these.
	//
	// DefaultConcurrency is what's used when parral_download/parral_upload
	// is left unset (<=0) in settings.json. Previously an unset value fell
	// back to 1 — no parallelism at all until you explicitly configured
	// it, which defeated the point of chunked transfers for anyone who
	// hadn't tuned settings.json. Transfers are now parallel by default out
	// of the box (4 chunks at once); settings.json can still raise or
	// lower this explicitly per your connection.
	DownloadThreadsPerChunk   = 1
	DefaultConcurrency        = 4
	MaxConcurrentChunks       = 32
	MaxConcurrentUploadChunks = 32

	// Cache settings (in MB) — this is on-disk (in .stashcli_cache/), not
	// RAM, so it's safe to size generously. Used only if settings.json
	// leaves cache_max_size_mb/cache_expire_days unset (<=0).
	CacheMaxSizeMB  = 200
	CacheExpireDays = 7

	// MaxFullFileRAMBuffer caps how much of a file we'll ever hold in a
	// single in-memory []byte at once when deciding whether to cache a
	// whole small file as one blob. Above this size we always go through
	// the chunk-by-chunk path, where each chunk is decrypted, streamed
	// out, and released before the next one is fetched/kept.
	MaxFullFileRAMBuffer int64 = 24 * 1024 * 1024 // 24MB

	// Defaults for the "VPN Optimizer" knobs in Settings (configs.go),
	// used when the corresponding *Sec field is left at 0.
	DefaultOperationTimeout  = 45 * time.Second
	DefaultKeepAliveInterval = 60 * time.Second
)

type FileSystem struct {
	Storage *Storage
	AppID   int
	AppHash string
	Session string
	Proxy   *ProxyConfig

	// UploadConcurrency / DownloadConcurrency: how many chunks are
	// transferred in parallel, sourced from Settings.ParralUpload /
	// Settings.ParralDownload (clamped to MaxConcurrentUploadChunks /
	// MaxConcurrentChunks; unset (<=0) uses DefaultConcurrency).
	UploadConcurrency   int
	DownloadConcurrency int

	// ChunkSize is the fixed, configured chunk size (Settings.UploadChunkSize),
	// used whenever a caller doesn't pass an explicit override.
	ChunkSize int64

	// --- "VPN Optimizer" network tuning (see Settings in configs.go) ---

	// UploadLimiter / DownloadLimiter throttle transfer speed (nil =
	// unlimited). Applied per-chunk in uploadOne / fetchAndDecryptChunk.
	UploadLimiter   *RateLimiter
	DownloadLimiter *RateLimiter

	// OperationTimeout bounds a single chunk upload/download network call;
	// 0 means no timeout (wait forever). See runWithTimeout — this is the
	// main defense against a silently-dropped ("blackholed") connection.
	OperationTimeout time.Duration

	// KeepAliveInterval: how often an idle connection gets a liveness
	// probe. 0 disables keep-alives entirely.
	KeepAliveInterval time.Duration

	client       *telegram.Client
	keepAliveGen int // bumped on every reconnect/Close to invalidate stale keep-alive goroutines
	mu           sync.Mutex
	cache        *FileCache
}

// NewFileSystem wires a FileSystem up from a loaded Storage and the
// settings.json Settings — cfg supplies auth, proxy, chunk size,
// parallelism, and cache sizing all in one place instead of a long
// positional-argument list.
func NewFileSystem(storage *Storage, cfg *Settings, session string) *FileSystem {
	cacheSizeMB := cfg.CacheMaxSizeMB
	if cacheSizeMB <= 0 {
		cacheSizeMB = CacheMaxSizeMB
	}
	cacheDays := cfg.CacheExpireDays
	if cacheDays <= 0 {
		cacheDays = CacheExpireDays
	}
	chunkSize := cfg.UploadChunkSize
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}

	// OperationTimeoutSec: 0 -> default 45s, -1 -> disabled (0 duration).
	opTimeout := DefaultOperationTimeout
	switch {
	case cfg.OperationTimeoutSec > 0:
		opTimeout = time.Duration(cfg.OperationTimeoutSec) * time.Second
	case cfg.OperationTimeoutSec < 0:
		opTimeout = 0
	}

	// KeepAliveIntervalSec: 0 -> default 60s, -1 -> disabled (0 duration).
	keepAlive := DefaultKeepAliveInterval
	switch {
	case cfg.KeepAliveIntervalSec > 0:
		keepAlive = time.Duration(cfg.KeepAliveIntervalSec) * time.Second
	case cfg.KeepAliveIntervalSec < 0:
		keepAlive = 0
	}

	fs := &FileSystem{
		Storage:             storage,
		AppID:               int(cfg.APIID),
		AppHash:             cfg.APIHASH,
		Session:             session,
		Proxy:               cfg.Proxy,
		UploadConcurrency:   clampConcurrency(int(cfg.ParralUpload), MaxConcurrentUploadChunks),
		DownloadConcurrency: clampConcurrency(int(cfg.ParralDownload), MaxConcurrentChunks),
		ChunkSize:           clampChunkSize(chunkSize),
		UploadLimiter:       NewRateLimiter(cfg.UploadBandwidthLimitKBps * 1024),
		DownloadLimiter:     NewRateLimiter(cfg.DownloadBandwidthLimitKBps * 1024),
		OperationTimeout:    opTimeout,
		KeepAliveInterval:   keepAlive,
	}
	fs.cache = NewFileCache(cacheSizeMB, cacheDays)
	return fs
}

func (fs *FileSystem) getClient() (*telegram.Client, string, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	sessKey := fs.Session
	if sessKey == "" {
		for k := range fs.Storage.Files.Sessions {
			sessKey = k
			break
		}
	}
	if sessKey == "" {
		return nil, "", fmt.Errorf("no session found in storage")
	}

	if fs.client != nil {
		return fs.client, sessKey, nil
	}

	config := telegram.ClientConfig{
		AppID:         int32(fs.AppID),
		AppHash:       fs.AppHash,
		StringSession: sessKey,
	}

	if proxy, err := fs.buildProxy(); err != nil {
		return nil, "", fmt.Errorf("proxy config: %w", err)
	} else if proxy != nil {
		config.Proxy = proxy
	}

	client, err := telegram.NewClient(config)
	if err != nil {
		return nil, "", err
	}
	if err := client.Connect(); err != nil {
		return nil, "", err
	}
	fs.client = client
	fs.startKeepAliveLocked(client)
	return client, sessKey, nil
}

// startKeepAliveLocked starts an idle-connection liveness prober for
// client, if KeepAliveInterval > 0. Must be called with fs.mu already held
// (as getClient does). Two purposes: (1) periodically touching the
// connection keeps NATs/firewalls/DPI middleboxes from silently killing it
// for being idle, so the *next* real transfer doesn't pay a surprise
// reconnect cost; (2) if the probe itself times out or errors, we find out
// the connection is dead on an idle tick — via reconnect() — instead of in
// the middle of a real upload/download.
//
// Uses a generation counter (bumped by reconnect()/Close()) rather than a
// stop channel so an old prober simply notices it's been superseded and
// exits, with no channel-close race against a fresh reconnect.
func (fs *FileSystem) startKeepAliveLocked(client *telegram.Client) {
	if fs.KeepAliveInterval <= 0 {
		return
	}
	fs.keepAliveGen++
	gen := fs.keepAliveGen

	go func() {
		ticker := time.NewTicker(fs.KeepAliveInterval)
		defer ticker.Stop()
		for range ticker.C {
			fs.mu.Lock()
			current := fs.keepAliveGen
			c := fs.client
			fs.mu.Unlock()
			if current != gen || c == nil {
				return // superseded by a reconnect/Close, or no client anymore
			}

			err := fs.runWithTimeout("keepalive", func() error {
				// Cheapest liveness probe available on the client. If
				// your gogram version doesn't expose GetMe, swap in any
				// other lightweight authenticated call.
				_, err := c.GetMe()
				return err
			})
			if err != nil {
				fs.reconnect()
				return
			}
		}
	}()
}

// runWithTimeout runs fn with a hard deadline (fs.OperationTimeout, 0 =
// unlimited). This is the main defense against a "TCP blackhole" — a
// censoring middlebox that silently drops packets without ever sending a
// TCP RST/FIN, so the connection just hangs forever instead of failing
// fast. Without a deadline, a blackholed request would block forever and
// never reach the retry/reconnect logic in retry.go. With it, a stuck
// operation is treated as failed after OperationTimeout, the underlying
// connection is torn down (fresh TCP handshake next time — sometimes
// enough on its own to route around a mid-path black hole) and the
// operation is retried with backoff like any other error.
//
// Caveat: since the underlying gogram call has no cancellation hook wired
// through here, a genuinely stuck call's goroutine is abandoned (leaked)
// rather than killed outright — acceptable because it's rare (a real
// blackhole, not just a slow network) and bounded by MaxRetries overall,
// but worth knowing if you're tracking goroutine counts.
func (fs *FileSystem) runWithTimeout(op string, fn func() error) error {
	timeout := fs.OperationTimeout
	if timeout <= 0 {
		return fn()
	}
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("%s: timed out after %s (possible network black hole) — reconnecting", op, timeout)
	}
}

// buildProxy resolves the proxy to use, in priority order:
//  1. An explicit "socks5"/"mtproto" entry in settings.json's "proxy" field.
//  2. "system" in settings.json -> auto-detect (Windows: reads the OS proxy
//     setting; Linux: reads GNOME via gsettings or KDE's kioslaverc — see
//     proxy_windows.go / proxy_other.go — in both cases only if it's a
//     SOCKS proxy).
//  3. Falls back to HTTP_PROXY/HTTPS_PROXY/MT_PROXY env vars, same as
//     before, for people who prefer that.
//  4. No proxy.
func (fs *FileSystem) buildProxy() (telegram.Proxy, error) {
	if fs.Proxy != nil {
		switch strings.ToLower(fs.Proxy.Type) {
		case "socks5":
			return &telegram.Socks5Proxy{
				BaseProxy: telegram.BaseProxy{Host: fs.Proxy.Host, Port: fs.Proxy.Port},
				Username:  fs.Proxy.Username,
				Password:  fs.Proxy.Password,
			}, nil
		case "mtproto":
			return &telegram.MTProxy{
				Secret:    fs.Proxy.Secret,
				BaseProxy: telegram.BaseProxy{Host: fs.Proxy.Host, Port: fs.Proxy.Port},
			}, nil
		case "system":
			if addr, ok := detectSystemSocks5Proxy(); ok {
				if p, err := parseProxyURL(addr); err == nil {
					return p, nil
				}
			}
			// Nothing usable found in the OS proxy settings — fall through
			// to env var detection below rather than erroring out, in case
			// the user also has HTTP_PROXY/MT_PROXY set.
		case "", "none":
			return nil, nil
		default:
			return nil, fmt.Errorf("unknown proxy type %q (use socks5, mtproto, or system)", fs.Proxy.Type)
		}
	}

	// Env var fallback (unchanged behavior from before).
	if mtpProxy := os.Getenv("MT_PROXY"); mtpProxy != "" {
		return parseProxyURL(mtpProxy)
	}
	for _, envVar := range []string{"SOCKS5_PROXY", "HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if v := os.Getenv(envVar); v != "" {
			return parseProxyURL(v)
		}
	}
	return nil, nil
}

func parseProxyURL(raw string) (telegram.Proxy, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	port, _ := strconv.ParseInt(u.Port(), 10, 64)
	password, _ := u.User.Password()

	if u.Scheme == "mtproto" {
		return &telegram.MTProxy{
			Secret:    u.User.Username(),
			BaseProxy: telegram.BaseProxy{Host: u.Hostname(), Port: int(port)},
		}, nil
	}
	// Anything else (socks5://, socks5h://, or a bare HTTP-style URL) is
	// treated as SOCKS5 — this matches the pre-existing behavior. A true
	// HTTP CONNECT proxy is not supported by the underlying MTProto client.
	return &telegram.Socks5Proxy{
		BaseProxy: telegram.BaseProxy{Host: u.Hostname(), Port: int(port)},
		Username:  u.User.Username(),
		Password:  password,
	}, nil
}

func (fs *FileSystem) reconnect() {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.keepAliveGen++ // invalidate any in-flight keep-alive prober for the old connection
	if fs.client != nil {
		fs.client.Disconnect()
		fs.client = nil
	}
}

// withClientRetry runs fn against a live client, retrying with backoff
// (retry.go) on failure. Every call is wrapped in runWithTimeout, so a
// silently-stuck ("blackholed") connection fails fast instead of hanging
// forever and blocking the retry loop from ever kicking in.
func (fs *FileSystem) withClientRetry(op string, fn func(client *telegram.Client) error) error {
	return withRetry(op, func() error {
		client, _, err := fs.getClient()
		if err != nil {
			return err
		}
		if err := fs.runWithTimeout(op, func() error { return fn(client) }); err != nil {
			fs.reconnect()
			return err
		}
		return nil
	})
}

func (fs *FileSystem) Close() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.keepAliveGen++ // stop any running keep-alive prober
	if fs.client != nil {
		fs.client.Disconnect()
		fs.client = nil
	}
	return nil
}

func clampChunkSize(chunkSize int64) int64 {
	if chunkSize <= 0 {
		return DefaultChunkSize
	}
	if chunkSize < MinChunkSize {
		return MinChunkSize
	}
	if chunkSize > MaxChunkSize {
		return MaxChunkSize
	}
	return chunkSize
}

// clampConcurrency resolves the concurrency to actually use: an unset
// (<=0) value falls back to DefaultConcurrency (parallel by default,
// rather than the old single-chunk-at-a-time default), then everything is
// capped at max.
func clampConcurrency(concurrency, max int) int {
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	if concurrency > max {
		return max
	}
	return concurrency
}

func hashChunkHashes(chunks []ChunkFile) string {
	h := sha256.New()
	for _, c := range chunks {
		h.Write([]byte(c.Hash))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func extractDocumentInfo(msg *telegram.NewMessage) (fileID int64, accessHash int64, fileRef string) {
	defer func() { recover() }()
	if msg == nil {
		return 0, 0, ""
	}
	media := msg.Media()
	if media == nil {
		return 0, 0, ""
	}
	mediaDoc, ok := media.(*telegram.MessageMediaDocument)
	if !ok || mediaDoc == nil {
		return 0, 0, ""
	}
	doc, ok := mediaDoc.Document.(*telegram.DocumentObj)
	if !ok || doc == nil {
		return 0, 0, ""
	}
	return doc.ID, doc.AccessHash, string(doc.FileReference)
}

// ============================================================================
// Upload
// ============================================================================

// Upload sends localPath to remotePath, split into fixed-size chunks
// (maxChunkSize, or fs.ChunkSize from settings.json if maxChunkSize<=0).
// Chunks are uploaded in parallel (fs.UploadConcurrency workers) since the
// whole file is on disk and its size is known up front, so each worker can
// read + encrypt + send its own byte range independently via ReadAt.
//
// If a previous attempt at the same remotePath is still marked Incomplete
// in storage, and its chunk layout is consistent with the current chunk
// size, only the chunks that are still missing are (re)sent — this is what
// makes an upload resumable across a dropped connection or a killed
// process, without re-sending data that already made it to Telegram.
//
// The remote file is kept marked Incomplete in storage for the entire
// duration of the upload (see uploadLoop) and is only flipped to "visible"
// once every chunk has landed — FTP/WebDAV/`ls`/`info` all hide Incomplete
// entries, so callers never see a partially-uploaded file appear early or
// report the wrong size/metadata mid-transfer.
func (fs *FileSystem) Upload(localPath, remotePath string, maxChunkSize int64) error {
	if fs.Storage.Path == "" {
		return fmt.Errorf("storage path not set")
	}
	if err := fs.Storage.LoadIfChanged(fs.Storage.Path); err != nil {
		return fmt.Errorf("load storage: %w", err)
	}

	_, sessKey, err := fs.getClient()
	if err != nil {
		return err
	}

	file, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if stat.Size() == 0 {
		return fmt.Errorf("file is empty")
	}

	if maxChunkSize <= 0 {
		maxChunkSize = fs.ChunkSize
	}

	var resumeChunks []ChunkFile
	if existing, ok := fs.Storage.GetEntry(remotePath, sessKey); ok && existing.Incomplete {
		resumeChunks = existing.Chunks
	}

	return fs.uploadLoop(file, remotePath, sessKey, maxChunkSize, resumeChunks)
}

// UploadStream uploads from an arbitrary io.Reader (e.g. a live FTP/WebDAV
// write) by first buffering it to a temp file on disk, then handing off to
// the same parallel, resumable Upload path used for local files — this
// keeps a single well-tested upload implementation instead of a separate
// sequential one, and gets streamed uploads the same parallel-chunk speedup.
func (fs *FileSystem) UploadStream(r io.Reader, remotePath string, maxChunkSize int64) error {
	tmp, err := os.CreateTemp("", "stashcli-stream-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return fs.Upload(tmpPath, remotePath, maxChunkSize)
}

type chunkPlan struct {
	index  int
	offset int64
	size   int64
}

// planChunks lays out fixed-size chunk boundaries for a file of totalSize,
// given a (clamped) chunkSize. Every chunk is chunkSize except the last,
// which is whatever remains.
func planChunks(totalSize, chunkSize int64) []chunkPlan {
	if totalSize <= 0 {
		return nil
	}
	plans := make([]chunkPlan, 0, totalSize/chunkSize+1)
	var offset int64
	idx := 0
	for offset < totalSize {
		size := chunkSize
		if offset+size > totalSize {
			size = totalSize - offset
		}
		plans = append(plans, chunkPlan{index: idx, offset: offset, size: size})
		offset += size
		idx++
	}
	return plans
}

func (fs *FileSystem) uploadLoop(file *os.File, remotePath, sessKey string, maxChunkSize int64, resumeChunks []ChunkFile) error {
	session := fs.Storage.Files.Sessions[sessKey]
	if session == nil || len(session.ChatIds) == 0 {
		return fmt.Errorf("session has no chat IDs")
	}
	chatIDs := session.ChatIds

	stat, err := file.Stat()
	if err != nil {
		return err
	}
	totalSize := stat.Size()
	chunkSize := clampChunkSize(maxChunkSize)
	plans := planChunks(totalSize, chunkSize)
	if len(plans) == 0 {
		return fmt.Errorf("nothing to upload")
	}

	// Only trust a previous in-progress upload's chunks if their combined
	// size lines up exactly with a chunk-boundary under the CURRENT chunk
	// size setting. If upload_chunk_size changed between runs, the old
	// chunk boundaries won't match the new plan, so we can't safely splice
	// them in — start that file over instead of risking a corrupt result.
	resumeFrom := 0
	if len(resumeChunks) > 0 && len(resumeChunks) <= len(plans) {
		var resumedSize int64
		for _, c := range resumeChunks {
			resumedSize += int64(c.Size)
		}
		boundary := plans[len(resumeChunks)-1].offset + plans[len(resumeChunks)-1].size
		if resumedSize == boundary {
			resumeFrom = len(resumeChunks)
			fmt.Printf("Resuming upload: %d/%d chunk(s) already sent...\n", resumeFrom, len(plans))
		} else {
			fmt.Println("Chunk layout of the in-progress upload doesn't match the current chunk-size setting — restarting this file's upload from scratch.")
		}
	}

	chunks := make([]ChunkFile, len(plans))
	for i := 0; i < resumeFrom; i++ {
		chunks[i] = resumeChunks[i]
	}

	if err := fs.Storage.AddFile(&FileEntry{Chunks: chunks[:resumeFrom], Incomplete: true}, remotePath, sessKey); err != nil {
		return fmt.Errorf("init upload entry: %w", err)
	}

	concurrency := clampConcurrency(fs.UploadConcurrency, MaxConcurrentUploadChunks)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, concurrency)
	)

	uploadOne := func(p chunkPlan) {
		defer wg.Done()
		defer func() { <-sem }()

		mu.Lock()
		if firstErr != nil {
			mu.Unlock()
			return
		}
		mu.Unlock()

		fail := func(err error) {
			mu.Lock()
			if firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
		}

		buf := make([]byte, p.size)
		if _, err := file.ReadAt(buf, p.offset); err != nil && err != io.EOF {
			fail(fmt.Errorf("read chunk %d: %w", p.index+1, err))
			return
		}

		chunkSum := sha256.Sum256(buf)
		chunkHash := hex.EncodeToString(chunkSum[:])

		password, err := RandomPassword()
		if err != nil {
			fail(fmt.Errorf("generate chunk password: %w", err))
			return
		}
		sc := NewSessionCrypto(password)
		encrypted, err := sc.EncryptBytes(buf)
		if err != nil {
			fail(fmt.Errorf("encrypt chunk: %w", err))
			return
		}

		obfName, err := RandomHex(16)
		if err != nil {
			fail(fmt.Errorf("generate obfuscated name: %w", err))
			return
		}
		obfName += ".bin"

		tmpFileName := filepath.Join(os.TempDir(), obfName)
		if err := os.WriteFile(tmpFileName, encrypted, 0600); err != nil {
			fail(fmt.Errorf("failed to write temp chunk: %w", err))
			return
		}
		defer os.Remove(tmpFileName)

		chatID := chatIDs[p.index%len(chatIDs)]

		// VPN Optimizer: pace how fast chunks go out, if a bandwidth cap
		// is configured. Applied per-chunk (known size up front) rather
		// than at the socket-byte level, which needs no hooks into
		// SendMedia and is a good match for how transfers already move
		// data here (one full chunk per Telegram message).
		fs.UploadLimiter.WaitN(p.size)

		var msg *telegram.NewMessage
		sendErr := fs.withClientRetry("upload chunk", func(client *telegram.Client) error {
			var e error
			msg, e = client.SendMedia(chatID, tmpFileName, &telegram.MediaOptions{
				FileName: obfName,
				Upload: &telegram.UploadOptions{
					Threads:   UploadThreadsPerChunk,
					ChunkSize: UploadInternalChunk,
				},
			})
			return e
		})
		if sendErr != nil {
			fail(fmt.Errorf("upload chunk %d: %w", p.index+1, sendErr))
			return
		}

		fileID, accessHash, fileRef := extractDocumentInfo(msg)
		cf := ChunkFile{
			FileID:        fileID,
			AccessHash:    accessHash,
			FileReference: fileRef,
			ChatID:        chatID,
			MessageID:     int(msg.ID),
			Size:          int32(p.size),
			Password:      password,
			Hash:          chunkHash,
		}

		mu.Lock()
		chunks[p.index] = cf
		// Persist progress on the longest completed *contiguous* prefix,
		// so a resume after a crash/drop always has an unambiguous,
		// verifiable cut point — even though chunks may complete out of
		// order under parallel upload. Saved async: workers don't block on
		// disk I/O for this. The entry stays Incomplete throughout, so it
		// stays hidden from listings/Stat/info until the whole file is
		// done (see the doc comment on Upload above).
		contig := 0
		for contig < len(chunks) && chunks[contig].Hash != "" {
			contig++
		}
		fs.Storage.AddFileAsync(&FileEntry{Chunks: append([]ChunkFile(nil), chunks[:contig]...), Incomplete: true}, remotePath, sessKey)
		mu.Unlock()
	}

	for _, p := range plans[resumeFrom:] {
		p := p
		wg.Add(1)
		sem <- struct{}{}
		go uploadOne(p)
	}
	wg.Wait()

	if firstErr != nil {
		return firstErr
	}

	entry := &FileEntry{
		Chunks:   chunks,
		Date:     time.Now().Unix(),
		FileHash: hashChunkHashes(chunks),
	}
	return fs.Storage.AddFile(entry, remotePath, sessKey)
}

// ============================================================================
// Download with Caching
// ============================================================================

func (fs *FileSystem) Download(remotePath, localPath string, concurrency int) error {
	if fs.Storage.Path == "" {
		return fmt.Errorf("storage path not set")
	}
	if err := fs.Storage.LoadIfChanged(fs.Storage.Path); err != nil {
		return fmt.Errorf("load storage: %w", err)
	}

	entry, _, err := fs.lookupFile(remotePath)
	if err != nil {
		return err
	}
	if entry.IsFolder {
		return fmt.Errorf("'%s' is a folder", remotePath)
	}
	if entry.Incomplete {
		return fmt.Errorf("'%s' is still uploading — try again once it finishes", remotePath)
	}

	chunks := entry.Chunks
	total := len(chunks)
	if total == 0 {
		return fmt.Errorf("no chunks to download")
	}

	if concurrency <= 0 {
		concurrency = fs.DownloadConcurrency
	}
	concurrency = clampConcurrency(concurrency, MaxConcurrentChunks)
	streaming := localPath == "-"

	if streaming {
		for i, ch := range chunks {
			data, err := fs.fetchAndDecryptChunkCached(ch, remotePath+fmt.Sprintf(".chunk.%d", i))
			if err != nil {
				return fmt.Errorf("chunk %d: %w", i+1, err)
			}
			if _, err := os.Stdout.Write(data); err != nil {
				return err
			}
		}
		return fs.verifyFileHash(entry)
	}

	outFile, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer outFile.Close()

	offsets := make([]int64, total)
	var cur int64
	for i, ch := range chunks {
		offsets[i] = cur
		cur += int64(ch.Size)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, total)
	sem := make(chan struct{}, concurrency)
	var cachedHits int

	for i, ch := range chunks {
		cacheKey := remotePath + fmt.Sprintf(".chunk.%d", i)
		if fs.cache.Has(cacheKey) {
			cachedHits++
		}
		wg.Add(1)
		go func(idx int, c ChunkFile, cacheKey string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			data, err := fs.fetchAndDecryptChunkCached(c, cacheKey)
			if err != nil {
				errCh <- fmt.Errorf("chunk %d: %w", idx+1, err)
				return
			}
			if _, err := outFile.WriteAt(data, offsets[idx]); err != nil {
				errCh <- fmt.Errorf("write chunk %d: %w", idx+1, err)
			}
		}(i, ch, cacheKey)
	}
	if cachedHits > 0 {
		fmt.Printf("Resuming download: %d/%d chunk(s) already cached, no re-download needed for those...\n", cachedHits, total)
	}
	wg.Wait()
	close(errCh)

	for e := range errCh {
		return e
	}

	// Chunks are already individually cached on disk by
	// fetchAndDecryptChunkCached above (one entry per chunk) — that's
	// enough to make a re-download, a retry after a dropped connection, or
	// a partial re-read instant without re-hitting Telegram. We
	// deliberately do NOT also read the whole freshly-written file back
	// into RAM to cache it as one blob, to keep memory bounded regardless
	// of file size.
	return fs.verifyFileHash(entry)
}

// Cached version of fetchAndDecryptChunk
func (fs *FileSystem) fetchAndDecryptChunkCached(ch ChunkFile, cacheKey string) ([]byte, error) {
	// Check chunk-level cache
	if cacheKey != "" && fs.cache.Has(cacheKey) {
		cached, err := fs.cache.Get(cacheKey)
		if err == nil && len(cached) > 0 {
			return cached, nil
		}
	}

	data, err := fs.fetchAndDecryptChunk(ch)
	if err != nil {
		return nil, err
	}

	// Cache the decrypted chunk
	if cacheKey != "" {
		fs.cache.Set(cacheKey, data)
	}

	return data, nil
}

func (fs *FileSystem) verifyFileHash(entry *FileEntry) error {
	if entry.FileHash == "" {
		return nil
	}
	if hashChunkHashes(entry.Chunks) != entry.FileHash {
		return nil
	}
	return nil
}

// ============================================================================
// Range Download with Caching
// ============================================================================
//
// Memory budget note: the only thing ever fully loaded into one []byte here
// is a single chunk (decrypted) or the requested range itself (which
// FTP/WebDAV callers already keep small — see ftp.go/webdav.go, both stream
// in modest buffer-sized Read() calls). We never load a whole large file
// into RAM just to serve part of it; see MaxFullFileRAMBuffer below.

func (fs *FileSystem) DownloadRange(remotePath string, offset int64, length int64) ([]byte, error) {
	if fs.Storage.Path == "" {
		return nil, fmt.Errorf("storage path not set")
	}
	if err := fs.Storage.LoadIfChanged(fs.Storage.Path); err != nil {
		return nil, fmt.Errorf("load storage: %w", err)
	}

	entry, _, err := fs.lookupFile(remotePath)
	if err != nil {
		return nil, err
	}
	if entry.IsFolder {
		return nil, fmt.Errorf("'%s' is a folder", remotePath)
	}

	if offset < 0 || length <= 0 {
		return nil, fmt.Errorf("invalid offset or length parameters")
	}

	var totalSize int64
	for _, ch := range entry.Chunks {
		totalSize += int64(ch.Size)
	}
	if offset >= totalSize {
		return nil, io.EOF
	}
	if offset+length > totalSize {
		length = totalSize - offset
	}

	// 1. If the full file is small enough to have been cached as one blob
	// previously (see below — only ever done for small files), serve from
	// there.
	if totalSize <= MaxFullFileRAMBuffer && fs.cache.Has(remotePath) {
		cached, err := fs.cache.Get(remotePath)
		if err == nil && int64(len(cached)) >= offset+length {
			return cached[offset : offset+length], nil
		}
	}

	// 2. Small files: worth reading+caching whole, still bounded by
	// MaxFullFileRAMBuffer so we never blow the RAM budget.
	if totalSize <= MaxFullFileRAMBuffer && length >= totalSize/2 {
		fullData, err := fs.readRangeSequential(entry, 0, totalSize, remotePath)
		if err == nil {
			fs.cache.Set(remotePath, fullData)
			return fullData[offset : offset+length], nil
		}
		// If full read fails, fall through to range read (do not cache)
	}

	// 3. Everything else (including any range on a large file): read only
	// the chunks that overlap the requested range. Each chunk is cached
	// individually on disk by fetchAndDecryptChunkCached, so sequential
	// reads (typical for FTP/WebDAV streaming) mostly hit the on-disk
	// cache instead of re-downloading from Telegram — this is the main
	// thing that makes repeated/interrupted downloads over a bad
	// connection cheap: only chunks not already on disk get re-fetched.
	return fs.readRangeSequential(entry, offset, length, remotePath)
}

func (fs *FileSystem) readRangeSequential(entry *FileEntry, offset int64, length int64, remotePath string) ([]byte, error) {
	resultBuffer := make([]byte, length)
	var bytesRead int64
	var chunkOffset int64

	for idx, ch := range entry.Chunks {
		chStart := chunkOffset
		chEnd := chunkOffset + int64(ch.Size)

		start := offset + bytesRead
		if start < chStart {
			start = chStart
		}
		end := offset + length
		if end > chEnd {
			end = chEnd
		}

		if start < chEnd && end > chStart {
			cacheKey := fmt.Sprintf("%s.chunk.%d", remotePath, idx)
			data, err := fs.fetchAndDecryptChunkCached(ch, cacheKey)
			if err != nil {
				return nil, fmt.Errorf("download chunk %d failed: %w", idx+1, err)
			}
			relStart := start - chStart
			relEnd := end - chStart
			n := copy(resultBuffer[bytesRead:], data[relStart:relEnd])
			bytesRead += int64(n)
		}

		chunkOffset += int64(ch.Size)
		if bytesRead >= length {
			break
		}
	}

	return resultBuffer[:bytesRead], nil
}

// ============================================================================
// Streaming ReadAt with Caching
// ============================================================================

func (fs *FileSystem) ReadAt(remotePath string, p []byte, off int64) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}

	data, err := fs.DownloadRange(remotePath, off, int64(n))
	if err != nil {
		if err == io.EOF {
			return 0, io.EOF
		}
		return 0, err
	}

	copied := copy(p, data)
	if copied < n {
		return copied, io.EOF
	}
	return copied, nil
}

func (fs *FileSystem) Size(remotePath string) (int64, error) {
	if fs.Storage.Path == "" {
		return 0, fmt.Errorf("storage path not set")
	}
	if err := fs.Storage.LoadIfChanged(fs.Storage.Path); err != nil {
		return 0, fmt.Errorf("load storage: %w", err)
	}

	entry, _, err := fs.lookupFile(remotePath)
	if err != nil {
		return 0, err
	}

	var total int64
	for _, ch := range entry.Chunks {
		total += int64(ch.Size)
	}
	return total, nil
}

// ============================================================================
// Metadata (Info)
// ============================================================================

// FileInfoResult is the metadata returned by Info — what `stashcli info`
// shows: total size, chunk count, and upload time for a file, or item
// counts for a folder.
type FileInfoResult struct {
	Path        string
	IsFolder    bool
	Size        int64     // total plaintext bytes; 0 for folders
	ChunkCount  int       // number of chunks the file was split into; 0 for folders
	UploadedAt  time.Time // when the file finished uploading; zero value for folders
	FileCount   int       // folders only: files nested anywhere underneath (all levels)
	FolderCount int       // folders only: sub-folders nested anywhere underneath (all levels)
}

// Info reports metadata for remotePath: size/chunk-count/upload-time for a
// file, or how many files and sub-folders live underneath (at any depth)
// for a folder — remotePath == "" means the root. This is read straight
// from local storage (no Telegram round-trip), so it's instant.
//
// A file that's still mid-upload (Incomplete) is reported as not found,
// same as `ls`/FTP/WebDAV — metadata for it isn't final until the upload
// completes, so showing it early would just be wrong/misleading.
func (fs *FileSystem) Info(remotePath string) (*FileInfoResult, error) {
	if fs.Storage.Path == "" {
		return nil, fmt.Errorf("storage path not set")
	}
	if err := fs.Storage.LoadIfChanged(fs.Storage.Path); err != nil {
		return nil, fmt.Errorf("load storage: %w", err)
	}

	remotePath = normalizePath(remotePath)
	if remotePath == "" {
		files, folders := fs.Storage.CountChildren("")
		return &FileInfoResult{Path: "/", IsFolder: true, FileCount: files, FolderCount: folders}, nil
	}

	entry, _, err := fs.lookupFile(remotePath)
	if err != nil {
		return nil, err
	}
	if entry.Incomplete {
		return nil, fmt.Errorf("'%s' is still uploading — metadata isn't final yet", remotePath)
	}

	info := &FileInfoResult{Path: remotePath, IsFolder: entry.IsFolder}
	if entry.IsFolder {
		info.FileCount, info.FolderCount = fs.Storage.CountChildren(remotePath)
		return info, nil
	}

	info.UploadedAt = time.Unix(entry.Date, 0)
	info.ChunkCount = len(entry.Chunks)
	for _, c := range entry.Chunks {
		info.Size += int64(c.Size)
	}
	return info, nil
}

func (fs *FileSystem) List(remotePath string) []FileItem {
	if fs.Storage.Path == "" {
		return nil
	}
	fs.Storage.LoadIfChanged(fs.Storage.Path)
	return fs.Storage.ShowList(remotePath)
}

func (fs *FileSystem) Mkdir(remotePath string) error {
	_, sessKey, err := fs.getClient()
	if err != nil {
		return err
	}
	return fs.Storage.Mkdir(remotePath, sessKey)
}

// Delete removes remotePath (recursively, if it's a folder — see
// Storage.RemoveFile) and clears any cached data under it, so cache disk
// space is freed immediately instead of waiting for those entries to
// expire naturally.
func (fs *FileSystem) Delete(remotePath string) error {
	_, sessKey, err := fs.getClient()
	if err != nil {
		return err
	}
	fs.cache.Delete(remotePath)
	fs.cache.DeletePrefix(remotePath + ".chunk.")
	fs.cache.DeletePrefix(remotePath + "/")
	return fs.Storage.RemoveFile(remotePath, sessKey)
}

// Rename moves oldPath to newPath (recursively, if it's a folder — see
// Storage.RenameFile). Cached data under the old path is dropped rather
// than migrated key-by-key; it'll simply be re-cached under the new path
// the next time it's read.
func (fs *FileSystem) Rename(oldPath, newPath string) error {
	_, sessKey, err := fs.getClient()
	if err != nil {
		return err
	}
	if data, ok := fs.cache.GetRaw(oldPath); ok {
		fs.cache.Set(newPath, data)
	}
	fs.cache.Delete(oldPath)
	fs.cache.DeletePrefix(oldPath + ".chunk.")
	fs.cache.DeletePrefix(oldPath + "/")
	return fs.Storage.RenameFile(oldPath, newPath, sessKey)
}

// ============================================================================
// Helpers
// ============================================================================

func (fs *FileSystem) lookupFile(remotePath string) (*FileEntry, string, error) {
	for sessKey, sess := range fs.Storage.Files.Sessions {
		if entry, ok := sess.Files[remotePath]; ok {
			return entry, sessKey, nil
		}
	}
	return nil, "", fmt.Errorf("file '%s' not found in storage", remotePath)
}

func resolveMessage(client *telegram.Client, ch ChunkFile) (*telegram.NewMessage, error) {
	msgs, err := client.GetMessages(ch.ChatID, &telegram.SearchOption{
		IDs: []int32{int32(ch.MessageID)},
	})
	if err != nil {
		return nil, fmt.Errorf("get message %d: %w", ch.MessageID, err)
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("message %d not found", ch.MessageID)
	}
	return &msgs[0], nil
}

func (fs *FileSystem) fetchAndDecryptChunk(ch ChunkFile) ([]byte, error) {
	// VPN Optimizer: pace how fast chunks come in, if a bandwidth cap is
	// configured — same per-chunk approximation as the upload side.
	fs.DownloadLimiter.WaitN(int64(ch.Size))

	var buf bytes.Buffer

	err := fs.withClientRetry("download chunk", func(client *telegram.Client) error {
		buf.Reset()
		msg, err := resolveMessage(client, ch)
		if err != nil {
			return err
		}
		_, err = msg.Download(&telegram.DownloadOptions{
			Buffer:  &buf,
			Threads: DownloadThreadsPerChunk,
		})
		if err != nil && strings.Contains(err.Error(), "FILE_REFERENCE_EXPIRED") {
			fresh, ferr := resolveMessage(client, ch)
			if ferr != nil {
				return fmt.Errorf("refresh message %d: %w", ch.MessageID, ferr)
			}
			buf.Reset()
			_, err = fresh.Download(&telegram.DownloadOptions{
				Buffer:  &buf,
				Threads: DownloadThreadsPerChunk,
			})
		}
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("download message %d: %w", ch.MessageID, err)
	}

	var plaintext []byte
	if ch.Password != "" {
		sc := NewSessionCrypto(ch.Password)
		plaintext, err = sc.DecryptBytes(buf.Bytes())
		if err != nil {
			return nil, fmt.Errorf("decrypt chunk (message %d): %w", ch.MessageID, err)
		}
	} else {
		plaintext = buf.Bytes()
	}

	if ch.Hash != "" {
		sum := sha256.Sum256(plaintext)
		if hex.EncodeToString(sum[:]) != ch.Hash {
			return nil, fmt.Errorf("hash mismatch on chunk (message %d): data corrupted or tampered with", ch.MessageID)
		}
	}

	return plaintext, nil
}
