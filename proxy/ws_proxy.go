package proxy

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"

	"github.com/codex2api/security"
)

// ApplyWebsocketProxy 把代理应用到 gorilla WebSocket dialer。
//
// 默认沿用 http.ProxyURL（目标域名交由代理解析，与既有行为一致；gorilla 不接受
// curl 风格的 socks5h 别名，这里归一成 socks5）。
//
// 当启用全局强制 hosts 覆盖时，改为自建隧道：gorilla 会把「目标地址」直接交给
// 代理拨号器，而标准库 http.ProxyURL 无法改写 CONNECT / SOCKS5 的目标，只有自建
// 隧道才能把目标也强制成显式 IP。此时 dialer.Proxy 置空、NetDialContext 换成隧道
// 拨号器；wss 的 TLS 仍由 gorilla 按原域名握手，SNI / Host 不受影响。
func ApplyWebsocketProxy(dialer *websocket.Dialer, rawProxyURL string) error {
	if dialer == nil {
		return fmt.Errorf("websocket dialer is nil")
	}
	u, err := security.ParseProxyURL(rawProxyURL)
	if err != nil {
		return fmt.Errorf("parse proxy URL failed: %w", err)
	}

	if !security.HostsOverrideEnabled() {
		parsed := *u
		parsed.Scheme = strings.ToLower(strings.TrimSpace(parsed.Scheme))
		if parsed.Scheme == "socks5h" {
			parsed.Scheme = "socks5"
		}
		dialer.Proxy = http.ProxyURL(&parsed)
		return nil
	}

	tunnel, err := buildProxyTunnelDialer(u)
	if err != nil {
		return err
	}
	ctxDialer, ok := tunnel.(security.Dialer)
	if !ok {
		return fmt.Errorf("proxy tunnel dialer for scheme %q does not support DialContext", u.Scheme)
	}
	dialer.Proxy = nil
	dialer.NetDialContext = ctxDialer.DialContext
	return nil
}

// applyProxyURLIfSet 是 ApplyWebsocketProxy 的便捷包装：空代理直接跳过。
func applyProxyURLIfSet(dialer *websocket.Dialer, rawProxyURL string) error {
	if strings.TrimSpace(rawProxyURL) == "" {
		return nil
	}
	return ApplyWebsocketProxy(dialer, rawProxyURL)
}
