package stashgram

// ProxyConfig lets you tell stashcli how to reach Telegram when your network
// is censored/filtered or you already run a local proxy (v2rayN, Clash,
// Shadowsocks, Xray, etc — all of these expose a local SOCKS5 port).
//
// Type can be:
//   "socks5"  - use Host/Port (+ optional Username/Password) as a SOCKS5 proxy
//   "mtproto" - use Host/Port + Secret as a Telegram MTProto proxy
//   "system"  - auto-detect from the OS.
//                 * Windows: reads "Settings > Network & Internet > Proxy"
//                   (registry: Internet Settings\ProxyServer) — same as before.
//                 * Linux: reads GNOME/Unity/Cinnamon's proxy setting via
//                   `gsettings` (org.gnome.system.proxy), falling back to
//                   KDE Plasma's ~/.config/kioslaverc if gsettings isn't
//                   present. This is the same "system proxy" your browser
//                   uses on those desktops.
//               Only the SOCKS portion of any of these can be used, because
//               Telegram's MTProto connection is raw TCP and can only be
//               tunneled through a SOCKS5 or MTProto proxy, never a plain
//               HTTP proxy. If your system proxy is HTTP-only, set an
//               explicit "socks5" entry below instead (point it at your
//               proxy tool's local SOCKS5 port, commonly 127.0.0.1:1080).
//   ""        - no proxy (default). HTTP_PROXY/SOCKS5_PROXY env vars are
//               still honored as a fallback on any OS.
type ProxyConfig struct {
	Type     string `json:"type"`               // "socks5", "mtproto", "system", or "" for none
	Host     string `json:"host"`               // e.g. "127.0.0.1"
	Port     int    `json:"port"`               // e.g. 1080
	Username string `json:"username,omitempty"` // socks5 auth (optional)
	Password string `json:"password,omitempty"` // socks5 auth (optional)
	Secret   string `json:"secret,omitempty"`   // mtproto secret
}

// Settings is the root of settings.json. See settings.example.json in the
// repo root for a ready-to-copy starting point.
//
// UploadChunkSize: bytes per chunk, FIXED — every chunk (except the last)
// is exactly this size. Previously each chunk's size was randomized
// (between a min and this value) as a light obfuscation measure; that's
// been removed because it actively worked against resumability: a
// random-length chunk can't be safely compared against a fresh chunk plan
// when resuming an interrupted upload, and it made chunk-cache keys harder
// to reason about. Now the value you set here is exactly what gets used,
// every time.
//
//   Leave this at 0/unset and stashcli now defaults to a fixed 450MB chunk
//   (471859200 bytes) out of the box — e.g. an 80MB movie goes up as ONE
//   chunk / one Telegram message instead of many small ones. You only need
//   to set this yourself if you want something different.
//
//   Trade-off to know about: bigger chunks mean fewer, larger uploads/
//   downloads. That's fine on a fast, stable connection, but on a bad/
//   flaky connection a dropped transfer mid-chunk means re-doing that
//   *entire* chunk (retried automatically, see retry.go) rather than just
//   a few MB. If your internet is unreliable, prefer something in the
//   6-64MB range instead; 450MB-class chunks are best suited to fast,
//   stable connections (e.g. a home fiber line or a VPS). Either way,
//   per-chunk on-disk caching (see cache.go) means a chunk is only ever
//   downloaded once — a retry or a later re-download of the same file
//   reuses the cached copy instead of hitting Telegram again.
//
// ParralUpload / ParralDownload: how many chunks are transferred at once,
// in parallel. Wired directly into FileSystem — no more hardcoded caps
// buried in code. Leave at 0/unset and stashcli now defaults to 4-way
// parallelism out of the box (previously an unset value meant 1 chunk at a
// time — no parallelism until you configured it yourself).
//
// CacheMaxSizeMB / CacheExpireDays: on-disk chunk-cache size/retention
// (see cache.go). Left at 0 to use the built-in defaults.
type Settings struct {
	UploadChunkSize int64        `json:"uploadchunksize"` // bytes per chunk, fixed (no randomization); 0/unset = 450MB default
	APIID           int32        `json:"api_id"`
	APIHASH         string       `json:"api_hash"`
	ParralDownload  int8         `json:"parral_download"` // 0/unset = 4-way parallel by default
	ParralUpload    int8         `json:"parral_upload"`   // 0/unset = 4-way parallel by default
	Proxy           *ProxyConfig `json:"proxy,omitempty"`
	CacheMaxSizeMB  int          `json:"cache_max_size_mb,omitempty"`
	CacheExpireDays int          `json:"cache_expire_days,omitempty"`

	// ------------------------------------------------------------------
	// "VPN Optimizer" — network tuning for bad/high-latency/censored
	// connections. Every field here is optional; 0 means "use the
	// built-in default" unless noted otherwise. All of this is read once
	// in NewFileSystem and lives on FileSystem from then on — see
	// FileSystem.UploadLimiter/DownloadLimiter/OperationTimeout/
	// KeepAliveInterval in filesystem.go.
	// ------------------------------------------------------------------

	// UploadBandwidthLimitKBps / DownloadBandwidthLimitKBps cap transfer
	// speed in KB/s (0 = unlimited). Useful if stashcli is competing with
	// other traffic on a slow/shared link and you'd rather it go slower
	// and steady than saturate the connection and cause everything else
	// (including stashcli's own control traffic) to time out.
	UploadBandwidthLimitKBps   int64 `json:"upload_bandwidth_limit_kbps,omitempty"`
	DownloadBandwidthLimitKBps int64 `json:"download_bandwidth_limit_kbps,omitempty"`

	// OperationTimeoutSec bounds how long a single chunk upload/download
	// network call is allowed to hang before it's treated as failed and
	// retried on a fresh connection. This is the main defense against a
	// "TCP blackhole" (a censoring middlebox that silently drops packets
	// without ever sending RST/FIN, so a plain socket read/write just
	// hangs forever instead of erroring) — without a deadline, that kind
	// of stall would never trigger the existing retry/backoff logic.
	//   0  -> default (45s)
	//   -1 -> disabled (wait forever, old behavior)
	OperationTimeoutSec int `json:"operation_timeout_sec,omitempty"`

	// KeepAliveIntervalSec: how often an idle connection sends a cheap
	// liveness probe. Two purposes: (1) many NATs/firewalls/DPI boxes
	// silently kill idle connections after a while, so a periodic probe
	// keeps the path "warm" and avoids paying reconnect cost right when a
	// download/upload is about to start; (2) if the probe itself times
	// out, we find out the connection is dead on an idle tick and
	// reconnect proactively, instead of discovering it mid-transfer.
	//   0  -> default (60s)
	//   -1 -> disabled
	KeepAliveIntervalSec int `json:"keepalive_interval_sec,omitempty"`

	// ------------------------------------------------------------------
	// Media streaming server (`stashcli stream`) — serves files directly
	// over plain HTTP with Range support, so a browser or media player
	// (VLC, mpv, ...) can play/scrub a file without downloading it first.
	// All optional; flags on the `stream` command override these.
	// ------------------------------------------------------------------
	StreamAddr string `json:"stream_addr,omitempty"` // default "127.0.0.1"
	StreamPort int    `json:"stream_port,omitempty"` // default 8081
	StreamUser string `json:"stream_user,omitempty"` // optional HTTP Basic Auth
	StreamPass string `json:"stream_pass,omitempty"`
}
