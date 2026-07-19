//go:build windows

package stashgram

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// detectSystemSocks5Proxy reads Windows' "Settings > Network & Internet >
// Proxy" configuration (the same one browsers use) and returns a socks5://
// URL if — and only if — a SOCKS proxy is configured there.
//
// Why only SOCKS: Telegram's connection (via gogram) is raw TCP (MTProto),
// which can only be tunneled through a SOCKS5 or MTProto proxy. A plain
// HTTP/HTTPS proxy (the most common Windows system proxy setup) cannot
// carry it, so we deliberately don't try to reinterpret an HTTP proxy entry
// as SOCKS5 — that would silently fail to connect instead of giving a clear
// error. If Windows only has an HTTP proxy configured, set an explicit
// "socks5" entry in settings.json's "proxy" field instead (point it at your
// proxy tool's local SOCKS5 port — v2rayN/Clash/Xray/Shadowsocks all expose
// one, commonly 127.0.0.1:1080).
func detectSystemSocks5Proxy() (string, bool) {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`,
		registry.QUERY_VALUE)
	if err != nil {
		return "", false
	}
	defer k.Close()

	enabled, _, err := k.GetIntegerValue("ProxyEnable")
	if err != nil || enabled == 0 {
		return "", false
	}

	server, _, err := k.GetStringValue("ProxyServer")
	if err != nil || server == "" {
		return "", false
	}

	// ProxyServer is either a single "host:port" (applies to http only in
	// this simple form) or a per-protocol list like
	// "http=host:port;https=host:port;socks=host:port".
	if strings.Contains(server, "=") {
		for _, part := range strings.Split(server, ";") {
			part = strings.TrimSpace(part)
			if strings.HasPrefix(part, "socks=") {
				return "socks5://" + strings.TrimPrefix(part, "socks="), true
			}
		}
		return "", false // only http/https entries present — can't be used
	}

	// A bare "host:port" with no "=" is Windows' HTTP-only shorthand, not
	// SOCKS — don't guess, just report nothing usable.
	return "", false
}
