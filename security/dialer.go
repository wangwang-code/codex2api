package security

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ==================== 覆盖感知的拨号器 ====================
//
// 这里提供几类小工具，供各出站链路组合使用，把「强制 hosts 覆盖」落到实际拨号上：
//
//   - Dialer / HostsDialer / DialContextFunc：直连（含代理端点）拨号，自动改写地址；
//   - CONNECTTunnelDialer：http(s) 代理，自建 CONNECT 隧道，CONNECT 目标可用 IP；
//   - TargetRewriteDialer：包一层任意 Dialer，把「目标地址」也改写掉（socks5 用）。
//
// 全部仅依赖标准库，避免 auth / proxy 之间出现循环依赖。

// Dialer 同时提供 Dial 与 DialContext，与 golang.org/x/net/proxy.Dialer 兼容。
type Dialer interface {
	Dial(network, address string) (net.Conn, error)
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// DialContextFunc 把函数适配成 Dialer。
type DialContextFunc func(ctx context.Context, network, address string) (net.Conn, error)

// DialContext 实现 Dialer。
func (f DialContextFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

// Dial 实现 Dialer（无 ctx 版本）。
func (f DialContextFunc) Dial(network, address string) (net.Conn, error) {
	return f(context.Background(), network, address)
}

// HostsDialer 是 net.Dialer 的覆盖感知版本：每次拨号前把地址里的域名换成强制 IP。
// 它同时满足 net.Dialer 的常用调用方式与 golang.org/x/net/proxy.Dialer。
type HostsDialer struct {
	d net.Dialer
}

// NewDialer 创建覆盖感知拨号器；timeout/keepAlive 语义与 net.Dialer 一致。
func NewDialer(timeout, keepAlive time.Duration) *HostsDialer {
	return &HostsDialer{d: net.Dialer{Timeout: timeout, KeepAlive: keepAlive}}
}

// DialContext 实现 Dialer。
func (h *HostsDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return h.d.DialContext(ctx, network, RewriteHostPort(address))
}

// Dial 实现 Dialer。
func (h *HostsDialer) Dial(network, address string) (net.Conn, error) {
	return h.d.Dial(network, RewriteHostPort(address))
}

// Underlying 返回内部 *net.Dialer，供需要原生类型的少量场景使用。
func (h *HostsDialer) Underlying() *net.Dialer { return &h.d }

// WrapDialContext 包一层拨号函数，使每次拨号都经过全局 hosts 覆盖。
// base 为 nil 时返回 nil。
func WrapDialContext(base func(ctx context.Context, network, address string) (net.Conn, error)) func(ctx context.Context, network, address string) (net.Conn, error) {
	if base == nil {
		return nil
	}
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		return base(ctx, network, RewriteHostPort(address))
	}
}

// ApplyHostsOverrideToTransport 让 transport 的拨号（直连与代理端点）经过全局
// 强制 hosts 覆盖。未启用覆盖时是空操作，保证默认行为零变化。
//
// 对同一 transport 重复调用是安全的：地址被改写后已是 IP，再改写不会命中覆盖。
func ApplyHostsOverrideToTransport(transport *http.Transport) {
	if transport == nil || !HostsOverrideEnabled() {
		return
	}
	switch {
	case transport.DialContext != nil:
		transport.DialContext = WrapDialContext(transport.DialContext)
	case transport.Dial != nil:
		base := transport.Dial
		transport.Dial = func(network, address string) (net.Conn, error) {
			return base(network, RewriteHostPort(address))
		}
	default:
		// 与 net/http 默认拨号器对齐的超时/保活设置。
		transport.DialContext = WrapDialContext(
			(&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		)
	}
}

// TargetRewriteDialer 把 Inner 的目标地址先按覆盖改写，再交给 Inner 处理。
// 用于 socks5：x/net/proxy 会把目标地址原样下发给代理，包一层即可让代理收到 IP。
type TargetRewriteDialer struct {
	Inner Dialer
}

// Dial 实现 Dialer。
func (t TargetRewriteDialer) Dial(network, address string) (net.Conn, error) {
	return t.Inner.Dial(network, RewriteHostPort(address))
}

// DialContext 实现 Dialer。
func (t TargetRewriteDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return t.Inner.DialContext(ctx, network, RewriteHostPort(address))
}

// CONNECTTunnelDialer 通过 http(s) 代理的 CONNECT 方法建立隧道。
//
// 之所以自建隧道而不依赖标准库的 http.Transport.Proxy：标准库的 CONNECT 目标
// 固定取自请求 URL，无法改写，也就无法把「远端解析」换成显式 IP。自建后，
// CONNECT 目标地址同样经过强制 hosts 覆盖。
type CONNECTTunnelDialer struct {
	// ProxyAddr 是代理服务器地址（host:port）；其中的域名会被覆盖改写。
	ProxyAddr string
	// AuthHeader 是可选 Proxy-Authorization 头值（如 "Basic xxx"）。
	AuthHeader string
	// ProxyTLS 为 true 表示与代理之间走 TLS（https:// 代理）。
	ProxyTLS bool
	// ProxyServerName 是与代理 TLS 握手用的 SNI；为空则取 ProxyAddr 的 host 部分。
	ProxyServerName string
	// Base 是拨代理端点用的底层拨号器；为空时使用默认 10s/30s 的覆盖感知拨号器。
	Base Dialer
	// OnProxyConnect 在成功建立到代理的 TCP 连接后回调（用于观测/健康统计）。
	OnProxyConnect func()
}

// Dial 实现 Dialer。
func (d *CONNECTTunnelDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

// DialContext 实现 Dialer。
func (d *CONNECTTunnelDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	base := d.Base
	if base == nil {
		base = NewDialer(10*time.Second, 30*time.Second)
	}

	conn, err := base.DialContext(ctx, "tcp", RewriteHostPort(d.ProxyAddr))
	if err != nil {
		return nil, fmt.Errorf("连接代理服务器失败: %w", err)
	}
	if d.OnProxyConnect != nil {
		d.OnProxyConnect()
	}

	// CONNECT 握手阶段用 ctx 的截止时间兜底，握手完成后立刻清掉，
	// 避免隧道建立后仍被这个 deadline 掐断长连接。
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if d.ProxyTLS {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName: d.proxyServerName(),
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("代理 TLS 握手失败: %w", err)
		}
		conn = tlsConn
	}

	target := RewriteHostPort(address)
	if _, err := conn.Write([]byte(buildCONNECTRequest(target, d.AuthHeader))); err != nil {
		conn.Close()
		return nil, fmt.Errorf("发送 CONNECT 请求失败: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("读取代理响应失败: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("代理 CONNECT 失败 (status %d)", resp.StatusCode)
	}

	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, reader: br}, nil
	}
	return conn, nil
}

func (d *CONNECTTunnelDialer) proxyServerName() string {
	if d.ProxyServerName != "" {
		return d.ProxyServerName
	}
	if host, _, err := net.SplitHostPort(d.ProxyAddr); err == nil {
		return host
	}
	return d.ProxyAddr
}

func buildCONNECTRequest(target, authHeader string) string {
	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if authHeader != "" {
		req += fmt.Sprintf("Proxy-Authorization: %s\r\n", authHeader)
	}
	return req + "\r\n"
}

// bufferedConn 保证 bufio.Reader 里已缓冲的字节不丢失。
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(b []byte) (int, error) {
	return c.reader.Read(b)
}
