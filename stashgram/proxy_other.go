//go:build !windows

package stashgram

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// detectSystemSocks5Proxy mirrors proxy_windows.go for Linux/macOS: it reads
// the same "system proxy" setting your desktop's browser would use, so
// "system" in settings.json's "proxy" field works the same way it does on
// Windows instead of requiring you to hunt down env vars manually.
//
// Order tried:
//  1. GNOME/Unity/Cinnamon, via `gsettings get org.gnome.system.proxy ...`
//     — covers Ubuntu, Fedora Workstation, Mint, Pop!_OS, etc.
//  2. KDE Plasma, via ~/.config/kioslaverc's [Proxy Settings] section —
//     covers Kubuntu, KDE Neon, openSUSE Plasma, etc. Read directly from
//     the config file rather than over D-Bus, so no extra dependency.
//  3. Nothing usable found -> caller (FileSystem.buildProxy) falls back to
//     HTTP_PROXY/HTTPS_PROXY/SOCKS5_PROXY/MT_PROXY env vars, same as
//     before.
//
// Only a SOCKS result is ever returned, for the same reason as Windows:
// Telegram's MTProto connection is raw TCP and can't be tunneled through a
// plain HTTP/HTTPS proxy — if your desktop only has an HTTP proxy
// configured, set an explicit "socks5" entry in settings.json instead
// (point it at your proxy tool's local SOCKS5 port — v2rayN/Clash/Xray/
// Shadowsocks all expose one, commonly 127.0.0.1:1080).
func detectSystemSocks5Proxy() (string, bool) {
	if addr, ok := detectGnomeSocks5Proxy(); ok {
		return addr, true
	}
	if addr, ok := detectKDESocks5Proxy(); ok {
		return addr, true
	}
	return "", false
}

// runGsettings runs `gsettings get <schema> <key>` and returns the trimmed,
// quote-stripped value. Returns ok=false if gsettings isn't installed (e.g.
// non-GNOME desktop, headless server, macOS) or the call fails.
func runGsettings(schema, key string) (string, bool) {
	bin, err := exec.LookPath("gsettings")
	if err != nil {
		return "", false
	}
	out, err := exec.Command(bin, "get", schema, key).Output()
	if err != nil {
		return "", false
	}
	return strings.Trim(strings.TrimSpace(string(out)), "'\""), true
}

func detectGnomeSocks5Proxy() (string, bool) {
	mode, ok := runGsettings("org.gnome.system.proxy", "mode")
	if !ok || strings.ToLower(mode) != "manual" {
		return "", false
	}

	host, ok := runGsettings("org.gnome.system.proxy.socks", "host")
	if !ok || host == "" {
		return "", false
	}
	port, ok := runGsettings("org.gnome.system.proxy.socks", "port")
	if !ok || port == "" || port == "0" {
		return "", false
	}
	return fmt.Sprintf("socks5://%s:%s", host, port), true
}

// detectKDESocks5Proxy reads KDE's proxy config file directly. Only handles
// ProxyType=1 (KDE's "Manually specify the proxy settings" mode), which is
// what "system" is meant to auto-detect here.
func detectKDESocks5Proxy() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	f, err := os.Open(filepath.Join(home, ".config", "kioslaverc"))
	if err != nil {
		return "", false
	}
	defer f.Close()

	values := map[string]string{}
	inProxySection := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			inProxySection = line == "[Proxy Settings]"
			continue
		}
		if !inProxySection {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		values[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	if err := scanner.Err(); err != nil {
		return "", false
	}

	if values["ProxyType"] != "1" {
		return "", false
	}
	// KDE stores this as "host port", e.g. "127.0.0.1 1080".
	socksProxy := values["socksProxy"]
	if socksProxy == "" {
		return "", false
	}
	fields := strings.Fields(socksProxy)
	if len(fields) < 2 {
		return "", false
	}
	return fmt.Sprintf("socks5://%s:%s", fields[0], fields[1]), true
}
