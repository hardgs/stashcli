package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"stashcli/stashgram"
	"strconv"
	"strings"
	"syscall"

	"github.com/amarnathcjd/gogram/telegram"
	"github.com/spf13/cobra"
)

// Version is stashcli's release version, shown by `stashcli --version`.
const Version = "1.2.0"

var rootCmd = &cobra.Command{
	Use:     "stashcli",
	Short:   "Cloud Storage With Telegram",
	Long:    "Stashgram — Store, Sync, and Stream Your Files On Telegram Cloud",
	Version: Version,
}

// newFileSystem loads storage from disk and wires up a FileSystem against
// it, using settings.json (cfg) for auth, proxy, chunk size, parallelism,
// and cache sizing. session may be "" to auto-pick the first session found
// in storage.
func newFileSystem(storagePath, session string) (*stashgram.FileSystem, error) {
	storage := &stashgram.Storage{}
	if err := storage.Load(storagePath); err != nil {
		return nil, fmt.Errorf("load storage: %w", err)
	}
	if len(storage.Files.Sessions) == 0 {
		return nil, fmt.Errorf("no sessions found in storage; run 'stashcli storage add' first")
	}
	if session != "" {
		if _, ok := storage.Files.Sessions[session]; !ok {
			return nil, fmt.Errorf("session '%s' not found in storage", session)
		}
	}
	return stashgram.NewFileSystem(storage, &cfg, session), nil
}

var uploadCmd = &cobra.Command{
	Use:   "upload [path] [dest path]",
	Short: "upload a file",
	Args:  cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		storagePath, _ := cmd.Root().PersistentFlags().GetString("storage")
		session, _ := cmd.Flags().GetString("session")

		// chunk-size defaults to 0 (unset) unless the user explicitly
		// passed --chunk-size, meaning: use the fixed chunk size from
		// settings.json's upload_chunk_size (see FileSystem.Upload) —
		// which itself defaults to a fixed 450MB if settings.json doesn't
		// set it either.
		var chunkSize int64
		if cmd.Flags().Changed("chunk-size") {
			chunkSize, _ = cmd.Flags().GetInt64("chunk-size")
		}

		fs, err := newFileSystem(storagePath, session)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer fs.Close()

		localPath, destPath := args[0], args[1]
		effectiveChunk := pickChunkSize(chunkSize, fs)
		fmt.Printf("Uploading %s -> %s (chunk size: %s, up to %d chunk(s) in parallel)...\n",
			localPath, destPath, stashgram.HumanSize(effectiveChunk), fs.UploadConcurrency)
		fmt.Println("The file won't show up in `ls`/FTP/WebDAV until the upload fully finishes — no partial/wrong-looking entries in the meantime.")
		if err := fs.Upload(localPath, destPath, chunkSize); err != nil {
			fmt.Println("Upload failed:", err)
			fmt.Println("Progress was saved — re-running the same upload command will resume instead of starting over.")
			os.Exit(1)
		}
		fmt.Println("Upload complete.")
	},
}

func pickChunkSize(explicit int64, fs *stashgram.FileSystem) int64 {
	if explicit > 0 {
		return explicit
	}
	return fs.ChunkSize
}

var downloadCmd = &cobra.Command{
	Use:   "download [path] [to]",
	Short: "download a file",
	Args:  cobra.ExactArgs(2),
	Run: func(cmd *cobra.Command, args []string) {
		storagePath, _ := cmd.Root().PersistentFlags().GetString("storage")
		session, _ := cmd.Flags().GetString("session")

		// concurrency defaults to 0 (unset) unless explicitly passed,
		// meaning: use settings.json's parral_download (see
		// FileSystem.Download) — which itself defaults to running 4
		// chunks in parallel if settings.json doesn't set it either.
		var concurrency int
		if cmd.Flags().Changed("concurrency") {
			concurrency, _ = cmd.Flags().GetInt("concurrency")
		}

		fs, err := newFileSystem(storagePath, session)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer fs.Close()

		remotePath, localPath := args[0], args[1]
		if localPath != "-" {
			fmt.Printf("Downloading %s -> %s ...\n", remotePath, localPath)
		}
		if err := fs.Download(remotePath, localPath, concurrency); err != nil {
			fmt.Println("Download failed:", err)
			fmt.Println("Any chunks that already finished are cached — re-running the same download command will reuse them instead of starting over.")
			os.Exit(1)
		}
		if localPath != "-" {
			fmt.Println("Download complete.")
		}
	},
}

var showListCmd = &cobra.Command{
	Use:   "ls [path]",
	Short: "show list of files on path",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		storagePath, _ := cmd.Root().PersistentFlags().GetString("storage")
		session, _ := cmd.Flags().GetString("session")

		fs, err := newFileSystem(storagePath, session)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer fs.Close()

		items := fs.List(args[0])
		if len(items) == 0 {
			fmt.Println("(empty)")
			return
		}
		for _, item := range items {
			if item.IsFolder {
				fmt.Printf("  %s/\n", item.Name)
				continue
			}
			// Show the file's size next to its name — best-effort: if the
			// lookup fails for some reason, still list the name so `ls`
			// never silently drops an entry.
			fullPath := strings.TrimSuffix(args[0], "/") + "/" + item.Name
			fullPath = strings.TrimPrefix(fullPath, "/")
			if size, err := fs.Size(fullPath); err == nil {
				fmt.Printf("  %-40s %s\n", item.Name, stashgram.HumanSize(size))
			} else {
				fmt.Printf("  %s\n", item.Name)
			}
		}
	},
}

var infoCmd = &cobra.Command{
	Use:   "info [path]",
	Short: "Show metadata: file size/upload time, or how many files/folders are inside a folder",
	Long: "Show metadata for a file or folder.\n\n" +
		"For a file: size, how many chunks it was split into, and when it finished uploading.\n" +
		"For a folder (including the root, with no path given): how many files and\n" +
		"sub-folders live inside it, counting every level underneath.",
	Args: cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		storagePath, _ := cmd.Root().PersistentFlags().GetString("storage")
		session, _ := cmd.Flags().GetString("session")
		target := ""
		if len(args) > 0 {
			target = args[0]
		}

		fs, err := newFileSystem(storagePath, session)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer fs.Close()

		info, err := fs.Info(target)
		if err != nil {
			fmt.Println("info failed:", err)
			os.Exit(1)
		}

		if info.IsFolder {
			fmt.Printf("Folder:            %s\n", info.Path)
			fmt.Printf("  Files inside:    %d (all levels)\n", info.FileCount)
			fmt.Printf("  Folders inside:  %d (all levels)\n", info.FolderCount)
			return
		}

		fmt.Printf("File:              %s\n", info.Path)
		fmt.Printf("  Size:            %s\n", stashgram.HumanSize(info.Size))
		fmt.Printf("  Chunks:          %d\n", info.ChunkCount)
		fmt.Printf("  Uploaded:        %s\n", info.UploadedAt.Format("2006-01-02 15:04:05 MST"))
	},
}

var mkdirCmd = &cobra.Command{
	Use:   "mkdir [path]",
	Short: "Create a virtual folder (metadata only, no upload)",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		storagePath, _ := cmd.Root().PersistentFlags().GetString("storage")
		session, _ := cmd.Flags().GetString("session")

		fs, err := newFileSystem(storagePath, session)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer fs.Close()

		if err := fs.Mkdir(args[0]); err != nil {
			fmt.Println("mkdir failed:", err)
			os.Exit(1)
		}
		fmt.Println("Folder created.")
	},
}

var rmCmd = &cobra.Command{
	Use:   "rm [path]",
	Short: "Remove a file or folder (recursively) — metadata only",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		storagePath, _ := cmd.Root().PersistentFlags().GetString("storage")
		session, _ := cmd.Flags().GetString("session")

		fs, err := newFileSystem(storagePath, session)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer fs.Close()

		if err := fs.Delete(args[0]); err != nil {
			fmt.Println("rm failed:", err)
			os.Exit(1)
		}
		fmt.Println("Removed.")
	},
}

// runServeWithGracefulShutdown runs a blocking Serve* call in the
// background and waits for either it to fail or Ctrl+C/SIGTERM. Either way,
// storage is flushed synchronously before exit — the async debounced saver
// (storage.go) normally catches up within ~200ms on its own, but forcing a
// final Save() here means a Ctrl+C right after the very last chunk still
// can't lose that write.
func runServeWithGracefulShutdown(fs *stashgram.FileSystem, name string, serve func() error) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() { errCh <- serve() }()

	select {
	case err := <-errCh:
		if err != nil {
			fmt.Printf("%s server failed: %v\n", name, err)
			fs.Storage.Save()
			fs.Close()
			os.Exit(1)
		}
	case <-sigCh:
		fmt.Printf("\nShutting down %s server...\n", name)
		fs.Storage.Save()
		fs.Close()
		os.Exit(0)
	}
}

var webdavCmd = &cobra.Command{
	Use:   "webdav",
	Short: "Serve storage over WebDAV (mount with Finder, Explorer, rclone, davfs2, ...)",
	Run: func(cmd *cobra.Command, args []string) {
		storagePath, _ := cmd.Root().PersistentFlags().GetString("storage")
		session, _ := cmd.Flags().GetString("session")
		port, _ := cmd.Flags().GetInt("port")
		addr, _ := cmd.Flags().GetString("addr")
		user, _ := cmd.Flags().GetString("user")
		pass, _ := cmd.Flags().GetString("pass")

		fs, err := newFileSystem(storagePath, session)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer fs.Close()

		listenAddr := fmt.Sprintf("%s:%d", addr, port)
		if user == "" && pass == "" {
			fmt.Println("Warning: no --user/--pass set, this WebDAV server has no auth. Keep it on 127.0.0.1 or behind a tunnel/reverse proxy.")
		}
		fmt.Println("Note for Windows clients: the built-in Windows WebDAV client (Map Network Drive) refuses Basic Auth over plain HTTP by default. If mounting fails with an auth error, either put this behind an HTTPS reverse proxy, or set the registry value BasicAuthLevel=2 under HKLM\\SYSTEM\\CurrentControlSet\\Services\\WebClient\\Parameters (restart the WebClient service after). Third-party clients (rclone, WinSCP, Cyberduck) don't have this restriction.")
		fmt.Printf("WebDAV server listening on http://%s (Ctrl+C to stop)\n", listenAddr)
		runServeWithGracefulShutdown(fs, "WebDAV", func() error {
			return stashgram.ServeWebDAV(fs, listenAddr, user, pass)
		})
	},
}

var ftpCmd = &cobra.Command{
	Use:   "ftp",
	Short: "Serve storage over FTP (mount with any FTP client)",
	Run: func(cmd *cobra.Command, args []string) {
		storagePath, _ := cmd.Root().PersistentFlags().GetString("storage")
		session, _ := cmd.Flags().GetString("session")
		port, _ := cmd.Flags().GetInt("port")
		addr, _ := cmd.Flags().GetString("addr")
		user, _ := cmd.Flags().GetString("user")
		pass, _ := cmd.Flags().GetString("pass")
		passivePorts, _ := cmd.Flags().GetString("passive-ports")
		publicIP, _ := cmd.Flags().GetString("public-ip")

		fs, err := newFileSystem(storagePath, session)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer fs.Close()

		if user == "" && pass == "" {
			fmt.Println("Warning: no --user/--pass set, this FTP server has no auth. Keep it on 127.0.0.1 or behind a firewall.")
		}
		fmt.Printf("FTP server listening on %s:%d (Ctrl+C to stop)\n", addr, port)
		runServeWithGracefulShutdown(fs, "FTP", func() error {
			return stashgram.ServeFTP(fs, addr, port, user, pass, passivePorts, publicIP)
		})
	},
}

var streamCmd = &cobra.Command{
	Use:   "stream",
	Short: "Serve files over plain HTTP with Range support — open a file URL directly in a browser or media player (VLC, mpv, ...) without downloading it first",
	Run: func(cmd *cobra.Command, args []string) {
		storagePath, _ := cmd.Root().PersistentFlags().GetString("storage")
		session, _ := cmd.Flags().GetString("session")
		port, _ := cmd.Flags().GetInt("port")
		addr, _ := cmd.Flags().GetString("addr")
		user, _ := cmd.Flags().GetString("user")
		pass, _ := cmd.Flags().GetString("pass")

		// Fall back to settings.json's stream_* fields for anything not
		// explicitly passed on the command line.
		if !cmd.Flags().Changed("port") && cfg.StreamPort > 0 {
			port = cfg.StreamPort
		}
		if !cmd.Flags().Changed("addr") && cfg.StreamAddr != "" {
			addr = cfg.StreamAddr
		}
		if !cmd.Flags().Changed("user") && cfg.StreamUser != "" {
			user = cfg.StreamUser
		}
		if !cmd.Flags().Changed("pass") && cfg.StreamPass != "" {
			pass = cfg.StreamPass
		}

		fs, err := newFileSystem(storagePath, session)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		defer fs.Close()

		listenAddr := fmt.Sprintf("%s:%d", addr, port)
		if user == "" && pass == "" {
			fmt.Println("Warning: no --user/--pass set, this streaming server has no auth. Keep it on 127.0.0.1 or behind a tunnel/reverse proxy.")
		}
		fmt.Printf("Streaming server listening on http://%s (Ctrl+C to stop)\n", listenAddr)
		fmt.Println("Open e.g. http://" + listenAddr + "/path/to/movie.mkv directly in a browser or media player.")
		runServeWithGracefulShutdown(fs, "streaming", func() error {
			return stashgram.ServeStream(fs, listenAddr, user, pass)
		})
	},
}

/* ********** Storage Group Commands ************ */
var storageGroupCmd = &cobra.Command{
	Use:   "storage",
	Short: "Manage Storage",
	Long:  "Storage Management Commands",
}

var storageGenCmd = &cobra.Command{
	Use:   "gen [path]",
	Short: "Generate Storage File",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		os.WriteFile(args[0], []byte(`{"sessions":{}}`), 0666)
	},
}

var storageAddCmd = &cobra.Command{
	Use:   "add",
	Short: "Add Session To Storage",
	Run: func(cmd *cobra.Command, args []string) {
		storagePath, _ := cmd.Root().PersistentFlags().GetString("storage")
		storage := stashgram.Storage{}
		if err := storage.Load(storagePath); err != nil {
			fmt.Println("Failed to load storage:", err)
			os.Exit(1)
		}

		reader := bufio.NewReader(os.Stdin)

		client, err := telegram.NewClient(telegram.ClientConfig{
			AppID:         cfg.APIID,
			AppHash:       cfg.APIHASH,
			StringSession: "",
			MemorySession: true,
		})
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		if err := client.Connect(); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		fmt.Print("Enter Phone Number (+123456789): ")
		phone, _ := reader.ReadString('\n')
		phone = strings.TrimSpace(phone)

		ok, err := client.Login(phone)
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		if !ok {
			fmt.Println("Login failed")
			os.Exit(1)
		}

		stringSession := client.ExportSession()

		// Ask for chat IDs
		fmt.Print("Enter chat IDs (comma separated, e.g. 123456,789012 — multiple IDs let chunks be spread round-robin across them): ")
		chatIdsInput, _ := reader.ReadString('\n')
		chatIdsInput = strings.TrimSpace(chatIdsInput)
		var chatIDs []int64
		if chatIdsInput != "" {
			parts := strings.Split(chatIdsInput, ",")
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				id, err := strconv.ParseInt(p, 10, 64)
				if err != nil {
					fmt.Printf("Invalid chat ID '%s': %v\n", p, err)
					os.Exit(1)
				}
				chatIDs = append(chatIDs, id)
			}
		}

		// Create or update session
		session, exists := storage.Files.Sessions[stringSession]
		if !exists {
			session = &stashgram.Session{
				ChatIds: chatIDs,
				Files:   make(map[string]*stashgram.FileEntry),
			}
			storage.Files.Sessions[stringSession] = session
		} else {
			if len(chatIDs) > 0 {
				session.ChatIds = chatIDs
			}
		}

		if err := storage.Save(); err != nil {
			fmt.Println("Failed to save storage:", err)
			os.Exit(1)
		}
		fmt.Println("Session added/updated successfully.")
		fmt.Println("Reminder: the session string above grants full account access — keep storage.json private, never share or commit it.")
	},
}

var storageDeleteCmd = &cobra.Command{
	Use:   "delete [session_string]",
	Short: "Delete Session From Storage",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		storagePath, _ := cmd.Root().PersistentFlags().GetString("storage")
		storage := stashgram.Storage{}
		if err := storage.Load(storagePath); err != nil {
			fmt.Println("Failed to load storage:", err)
			os.Exit(1)
		}

		sessionKey := args[0]
		if _, exists := storage.Files.Sessions[sessionKey]; !exists {
			fmt.Println("Session not found.")
			os.Exit(1)
		}
		delete(storage.Files.Sessions, sessionKey)
		if err := storage.Save(); err != nil {
			fmt.Println("Failed to save storage:", err)
			os.Exit(1)
		}
		fmt.Println("Session deleted successfully.")
	},
}

var storageListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show List Of Sessions",
	Run: func(cmd *cobra.Command, args []string) {
		storagePath, _ := cmd.Root().PersistentFlags().GetString("storage")
		storage := stashgram.Storage{}
		if err := storage.Load(storagePath); err != nil {
			fmt.Println("Failed to load storage:", err)
			os.Exit(1)
		}

		if len(storage.Files.Sessions) == 0 {
			fmt.Println("No sessions found.")
			return
		}

		fmt.Println("Sessions:")
		for sess, session := range storage.Files.Sessions {
			fmt.Printf("  Session: %s\n", sess)
			fmt.Printf("    Chat IDs: %v\n", session.ChatIds)
			fmt.Printf("    Files: %d\n", len(session.Files))
		}
	},
}

var storageEditCmd = &cobra.Command{
	Use:   "edit [session_string]",
	Short: "Edit Chat IDs of an existing session",
	Args:  cobra.ExactArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		storagePath, _ := cmd.Root().PersistentFlags().GetString("storage")
		storage := stashgram.Storage{}
		if err := storage.Load(storagePath); err != nil {
			fmt.Println("Failed to load storage:", err)
			os.Exit(1)
		}

		sessionKey := args[0]
		session, exists := storage.Files.Sessions[sessionKey]
		if !exists {
			fmt.Println("Session not found.")
			os.Exit(1)
		}

		reader := bufio.NewReader(os.Stdin)
		fmt.Printf("Current chat IDs: %v\n", session.ChatIds)
		fmt.Print("Enter new chat IDs (comma separated, or leave empty to keep current): ")
		input, _ := reader.ReadString('\n')
		input = strings.TrimSpace(input)

		if input != "" {
			var newChatIDs []int64
			parts := strings.Split(input, ",")
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				id, err := strconv.ParseInt(p, 10, 64)
				if err != nil {
					fmt.Printf("Invalid chat ID '%s': %v\n", p, err)
					os.Exit(1)
				}
				newChatIDs = append(newChatIDs, id)
			}
			session.ChatIds = newChatIDs
			if err := storage.Save(); err != nil {
				fmt.Println("Failed to save storage:", err)
				os.Exit(1)
			}
			fmt.Println("Chat IDs updated successfully.")
		} else {
			fmt.Println("No changes made.")
		}
	},
}

func init() {
	rootCmd.AddCommand(uploadCmd)
	rootCmd.AddCommand(downloadCmd)
	rootCmd.AddCommand(showListCmd)
	rootCmd.AddCommand(infoCmd)
	rootCmd.AddCommand(mkdirCmd)
	rootCmd.AddCommand(rmCmd)
	rootCmd.AddCommand(webdavCmd)
	rootCmd.AddCommand(ftpCmd)
	rootCmd.AddCommand(streamCmd)

	rootCmd.PersistentFlags().StringP("storage", "s", "storage.json", "Path Of Storage File")

	// chunk-size/concurrency default to 0 here; the Run funcs only honor
	// them if the flag was actually passed (cmd.Flags().Changed(...)) —
	// otherwise settings.json's upload_chunk_size/parral_upload/
	// parral_download are used (which themselves default to 450MB chunks /
	// 4-way parallelism if settings.json leaves them unset too). This lets
	// settings.json be the single source of truth while still allowing a
	// one-off CLI override.
	uploadCmd.Flags().Int64("chunk-size", 0, "Bytes per chunk (default: settings.json's upload_chunk_size, or 450MB if that's unset too)")
	uploadCmd.Flags().String("session", "", "Session string to upload under (default: first session in storage)")

	downloadCmd.Flags().Int("concurrency", 0, "Max chunks downloaded in parallel (default: settings.json's parral_download, or 4 if that's unset too)")
	downloadCmd.Flags().String("session", "", "Session string to download from (default: first session in storage)")

	showListCmd.Flags().String("session", "", "Session string to list from (default: first session in storage)")
	infoCmd.Flags().String("session", "", "Session string to look up (default: first session in storage)")
	mkdirCmd.Flags().String("session", "", "Session string to create the folder under (default: first session in storage)")
	rmCmd.Flags().String("session", "", "Session string to remove from (default: first session in storage)")

	webdavCmd.Flags().Int("port", 8080, "Port to listen on")
	webdavCmd.Flags().String("addr", "127.0.0.1", "Address to bind to (use 0.0.0.0 to expose beyond localhost — set --user/--pass if you do)")
	webdavCmd.Flags().String("user", "", "Optional HTTP Basic Auth username")
	webdavCmd.Flags().String("pass", "", "Optional HTTP Basic Auth password")
	webdavCmd.Flags().String("session", "", "Session string to serve (default: first session in storage)")

	// FTP flags
	ftpCmd.Flags().Int("port", 21, "Port to listen on")
	ftpCmd.Flags().String("addr", "0.0.0.0", "Address to bind to")
	ftpCmd.Flags().String("user", "", "FTP username (optional)")
	ftpCmd.Flags().String("pass", "", "FTP password (optional)")
	ftpCmd.Flags().String("passive-ports", "30000-30050", "Passive port range, e.g. '30000-30050' — forward this range on your router for WAN access")
	ftpCmd.Flags().String("public-ip", "", "Your public/router IP (required for passive mode to work when clients connect from outside your LAN)")
	ftpCmd.Flags().String("session", "", "Session string to serve (default: first session in storage)")

	streamCmd.Flags().Int("port", 8081, "Port to listen on (default falls back to settings.json's stream_port)")
	streamCmd.Flags().String("addr", "127.0.0.1", "Address to bind to (use 0.0.0.0 to expose beyond localhost — set --user/--pass if you do; default falls back to settings.json's stream_addr)")
	streamCmd.Flags().String("user", "", "Optional HTTP Basic Auth username (default falls back to settings.json's stream_user)")
	streamCmd.Flags().String("pass", "", "Optional HTTP Basic Auth password (default falls back to settings.json's stream_pass)")
	streamCmd.Flags().String("session", "", "Session string to serve (default: first session in storage)")

	rootCmd.AddCommand(storageGroupCmd)
	storageGroupCmd.AddCommand(storageAddCmd)
	storageGroupCmd.AddCommand(storageDeleteCmd)
	storageGroupCmd.AddCommand(storageGenCmd)
	storageGroupCmd.AddCommand(storageListCmd)
	storageGroupCmd.AddCommand(storageEditCmd)
}
