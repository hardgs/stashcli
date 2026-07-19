# stashcli — Cloud Storage With Telegram
# [فارسی](README_fa.md) | [عربي](README_ar.md) | [EN](README.md)
Store, sync, and stream your files using your own Telegram account as the
storage backend. Mount it as an FTP or WebDAV drive, or just use the
command line — files are split into encrypted chunks and uploaded as
Telegram messages, then reassembled on download.

- **Big files stay in as few pieces as possible.** By default every file is
  uploaded in fixed **450MB chunks** — an 80MB movie goes up as **one**
  message, not dozens of tiny ones.
- **Fast by default.** Uploads and downloads use **4 chunks in parallel**
  out of the box — no config needed to get real speed.
- **Resumable.** A dropped connection or a killed process doesn't cost you
  the whole transfer — re-run the same command and it picks up where it
  left off.
- **A file only appears once it's fully uploaded.** No partial/incomplete
  entries showing up in `ls`, FTP, or WebDAV mid-transfer.
- **Cross-platform.** Runs on Windows, Linux, and macOS from a single
  static binary.

---

## 1. Install

You need [Go 1.21+](https://go.dev/dl/) installed. That's the only build
requirement.

```bash
git clone https://github.com/yourname/stashcli.git
cd stashcli
go build -o stashcli .
```

That's it — you now have a `stashcli` (or `stashcli.exe` on Windows)
binary in the current folder. No Docker, no external services, nothing
else to install.

### Building for a different OS (cross-compiling)

Go can build binaries for other platforms without needing them installed:

```bash
# Windows (from Linux/macOS)
GOOS=windows GOARCH=amd64 go build -o stashcli.exe .

# Linux (from Windows/macOS)
GOOS=linux GOARCH=amd64 go build -o stashcli .

# macOS (Apple Silicon)
GOOS=darwin GOARCH=arm64 go build -o stashcli .
```

---

## 2. Get a Telegram API ID/hash

1. Go to <https://my.telegram.org>, log in with your phone number.
2. Open **API development tools** and create an app (any name is fine).
3. Copy the **api_id** and **api_hash** shown — you'll need them next.

---

## 3. Configure

Copy the example settings file and fill in your API ID/hash:

```bash
cp settings.example.json settings.json
```

```jsonc
{
  "uploadchunksize": 471859200,   // 450MB — leave as-is, or lower it on a flaky connection (see below)
  "api_id": 123456,               // from my.telegram.org
  "api_hash": "your_api_hash",    // from my.telegram.org
  "parral_download": 4,           // chunks downloaded at once
  "parral_upload": 4,             // chunks uploaded at once
  "cache_max_size_mb": 200,
  "cache_expire_days": 7
}
```

You don't have to set `uploadchunksize`/`parral_upload`/`parral_download`
at all — if you leave them out (or at `0`), stashcli already defaults to
450MB chunks and 4-way parallelism on its own.

> **On a slow or unstable connection?** Set `uploadchunksize` to something
> smaller, like `16777216` (16MB) or `67108864` (64MB). Bigger chunks are
> faster on a good connection, but a dropped chunk means re-sending that
> whole chunk, so smaller is safer when your connection isn't reliable.

Then create the (empty) storage database and log in once:

```bash
./stashcli storage gen storage.json
./stashcli storage add
```

`storage add` will ask for your phone number (to log into Telegram) and
one or more **chat IDs** — these are where your file chunks get sent (a
private group or "Saved Messages" both work fine). You can get a chat ID
from any Telegram bot that reports it, or by forwarding a message from
that chat to `@userinfobot`/`@getidsbot`.

> `storage.json` contains your Telegram session — treat it like a
> password. Never commit it or share it.

---

## 4. Use it

### Straight from the command line

```bash
# upload
./stashcli upload ./movie.mkv /movies/movie.mkv

# download
./stashcli download /movies/movie.mkv ./movie.mkv

# list a folder
./stashcli ls /movies

# see metadata: size / upload date for a file, or item counts for a folder
./stashcli info /movies/movie.mkv
./stashcli info /movies

# make a folder, remove a file/folder
./stashcli mkdir /movies
./stashcli rm /movies/movie.mkv
```

### Mount it as a drive (WebDAV)

```bash
./stashcli webdav --port 8080 --user alice --pass secret
```

Then connect from:
- **Windows:** File Explorer → *This PC* → *Map network drive* → `http://127.0.0.1:8080`
- **macOS:** Finder → *Go* → *Connect to Server* → `http://127.0.0.1:8080`
- **Linux:** any WebDAV client, or mount with `davfs2`/`rclone mount`

### Mount it as FTP

```bash
./stashcli ftp --port 21 --user alice --pass secret
```

Connect with any FTP client (FileZilla, WinSCP, the `ftp` command, your
OS's built-in one, etc).

### Stream a file directly (no download needed)

```bash
./stashcli stream --port 8081
```

Then open `http://127.0.0.1:8081/movies/movie.mkv` in a browser or a
media player like VLC/mpv — it supports seeking/scrubbing without
downloading the whole file first.

---

## 5. Running it as a background service

### Linux (systemd)

Create `/etc/systemd/system/stashcli-webdav.service`:

```ini
[Unit]
Description=stashcli WebDAV server
After=network.target

[Service]
WorkingDirectory=/opt/stashcli
ExecStart=/opt/stashcli/stashcli webdav --port 8080 --user alice --pass secret
Restart=on-failure
User=youruser

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable --now stashcli-webdav
```

### Windows

Run it from a `.bat` file, or use [NSSM](https://nssm.cc/) to install it
as a proper Windows service so it survives reboots.

---

## Security notes

- Every chunk is encrypted (AES-256-GCM) with a random per-chunk password
  before it's uploaded — Telegram never sees your file content in the
  clear.
- FTP/WebDAV/streaming servers have **no built-in transport encryption**.
  Keep them on `127.0.0.1` and use an SSH tunnel, or put a TLS reverse
  proxy in front, if you need remote access. Always set `--user`/`--pass`
  if you bind to anything other than `127.0.0.1`.
- `storage.json` holds your live Telegram session string — anyone with
  that file has full access to your account. Back it up somewhere private,
  never commit it to git.

## Behind a censored/filtered network?

stashcli can route its Telegram connection through a SOCKS5 or MTProto
proxy — see the `proxy` field documented in `configs.go`, or set
`"proxy": {"type": "system"}` in `settings.json` to auto-detect your OS's
configured SOCKS proxy (Windows and Linux/GNOME/KDE are supported).

## License

Add your license of choice here (MIT is a common, permissive pick for
personal-use CLI tools).