package auth

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"

	"github.com/codex2api/security"
)

type contextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// proxyConnectDialer 是拨号器需要同时具备的能力（*net.Dialer 与
// *security.HostsDialer 均满足）。
type proxyConnectDialer interface {
	Dial(network, address string) (net.Conn, error)
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

type observingProxyDialer struct {
	base      proxyConnectDialer
	onConnect func()
	wrapConn  func(net.Conn) net.Conn
}

func (d *observingProxyDialer) Dial(network, address string) (net.Conn, error) {
	conn, err := d.base.Dial(network, address)
	if err == nil {
		conn = d.observe(conn)
	}
	return conn, err
}

func (d *observingProxyDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := d.base.DialContext(ctx, network, address)
	if err == nil {
		conn = d.observe(conn)
	}
	return conn, err
}

func (d *observingProxyDialer) observe(conn net.Conn) net.Conn {
	if d.onConnect != nil {
		d.onConnect()
	}
	if d.wrapConn != nil {
		return d.wrapConn(conn)
	}
	return conn
}

type observingSOCKSConn struct {
	net.Conn
	onTargetConnectStart func()
	once                 sync.Once
}

func (c *observingSOCKSConn) Write(payload []byte) (int, error) {
	n, err := c.Conn.Write(payload)
	if err == nil && n == len(payload) && isSOCKSConnectRequest(payload) {
		c.once.Do(c.onTargetConnectStart)
	}
	return n, err
}

func isSOCKSConnectRequest(payload []byte) bool {
	if len(payload) < 4 || payload[0] != 0x05 || payload[1] != 0x01 || payload[2] != 0x00 {
		return false
	}
	switch payload[3] {
	case 0x01:
		return len(payload) == 4+net.IPv4len+2
	case 0x03:
		return len(payload) >= 5 && len(payload) == 7+int(payload[4])
	case 0x04:
		return len(payload) == 4+net.IPv6len+2
	default:
		return false
	}
}

// ProxyTransportObserver reports connection milestones while configuring a
// transport proxy.
type ProxyTransportObserver struct {
	OnProxyConnect            func()
	OnSOCKSTargetConnectStart func()
}

// ConfigureTransportProxy applies HTTP(S) or SOCKS5 proxy settings to a transport.
func ConfigureTransportProxy(transport *http.Transport, rawProxyURL string, baseDialer proxyConnectDialer) error {
	return configureTransportProxy(transport, rawProxyURL, baseDialer, ProxyTransportObserver{})
}

// ConfigureTransportProxyWithObserver applies proxy settings and reports
// successful proxy TCP connections and SOCKS target-connect attempts.
func ConfigureTransportProxyWithObserver(
	transport *http.Transport,
	rawProxyURL string,
	baseDialer proxyConnectDialer,
	observer ProxyTransportObserver,
) error {
	return configureTransportProxy(transport, rawProxyURL, baseDialer, observer)
}

func configureTransportProxy(
	transport *http.Transport,
	rawProxyURL string,
	baseDialer proxyConnectDialer,
	observer ProxyTransportObserver,
) error {
	if transport == nil {
		return nil
	}
	if strings.TrimSpace(rawProxyURL) == "" {
		// 无代理时也要让直连拨号经过强制 hosts 覆盖（未启用覆盖时空操作）。
		security.ApplyHostsOverrideToTransport(transport)
		return nil
	}
	if baseDialer == nil {
		baseDialer = security.NewDialer(10*time.Second, 30*time.Second)
	}

	u, err := security.ParseProxyURL(rawProxyURL)
	if err != nil {
		return fmt.Errorf("parse proxy url: %w", err)
	}

	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "http", "https":
		// 启用强制 hosts 覆盖时改走自建 CONNECT 隧道：标准库 http.Transport 的
		// CONNECT 目标固定取自请求 URL，没有改写钩子，只有自建隧道才能把目标也
		// 强制成覆盖 IP（代理端点地址同样在隧道里被改写）。
		if security.HostsOverrideEnabled() {
			tunnel := connectTunnelDialerForProxy(u, baseDialer, observer)
			transport.Proxy = nil
			transport.DialContext = tunnel.DialContext
			return nil
		}
		var proxyDialer proxyConnectDialer = baseDialer
		if observer.OnProxyConnect != nil {
			proxyDialer = &observingProxyDialer{
				base:      baseDialer,
				onConnect: observer.OnProxyConnect,
			}
		}
		transport.Proxy = http.ProxyURL(u)
		transport.DialContext = proxyDialer.DialContext
		return nil
	case "socks5", "socks5h":
		var forward proxyConnectDialer = baseDialer
		if observer.OnProxyConnect != nil || observer.OnSOCKSTargetConnectStart != nil {
			var wrapConn func(net.Conn) net.Conn
			if observer.OnSOCKSTargetConnectStart != nil {
				wrapConn = func(conn net.Conn) net.Conn {
					return &observingSOCKSConn{
						Conn:                 conn,
						onTargetConnectStart: observer.OnSOCKSTargetConnectStart,
					}
				}
			}
			forward = &observingProxyDialer{
				base:      baseDialer,
				onConnect: observer.OnProxyConnect,
				wrapConn:  wrapConn,
			}
		}
		var auth *xproxy.Auth
		if u.User != nil {
			password, _ := u.User.Password()
			auth = &xproxy.Auth{User: u.User.Username(), Password: password}
		}

		dialer, err := xproxy.SOCKS5("tcp", u.Host, auth, forward)
		if err != nil {
			return fmt.Errorf("build socks5 dialer: %w", err)
		}
		// x/net/proxy 会把目标地址原样下发给代理（socks5h 即由代理做远端解析）；
		// 包一层把目标先换成覆盖 IP，代理侧就不再解析域名。
		targetAware := security.TargetRewriteDialer{Inner: socksDialerAdapter{d: dialer}}
		transport.Proxy = nil
		transport.DialContext = targetAware.DialContext
		return nil
	default:
		return fmt.Errorf("unsupported proxy scheme: %s", u.Scheme)
	}
}

// connectTunnelDialerForProxy 依据代理 URL 构造自建 CONNECT 隧道拨号器。
func connectTunnelDialerForProxy(u *url.URL, baseDialer proxyConnectDialer, observer ProxyTransportObserver) *security.CONNECTTunnelDialer {
	addr := u.Host
	if !strings.Contains(addr, ":") {
		if strings.EqualFold(u.Scheme, "https") {
			addr += ":443"
		} else {
			addr += ":80"
		}
	}
	d := &security.CONNECTTunnelDialer{
		ProxyAddr:      addr,
		ProxyTLS:       strings.EqualFold(u.Scheme, "https"),
		Base:           baseDialer,
		OnProxyConnect: observer.OnProxyConnect,
	}
	if u.User != nil {
		password, _ := u.User.Password()
		credentials := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + password))
		d.AuthHeader = "Basic " + credentials
	}
	return d
}

// socksDialerAdapter 把 x/net/proxy 的 Dialer 适配成 security.Dialer。
type socksDialerAdapter struct {
	d xproxy.Dialer
}

func (a socksDialerAdapter) Dial(network, address string) (net.Conn, error) {
	return a.d.Dial(network, address)
}

func (a socksDialerAdapter) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if cd, ok := a.d.(contextDialer); ok {
		return cd.DialContext(ctx, network, address)
	}
	return dialWithContextFallback(ctx, a.d.Dial, network, address)
}

// dialWithContextFallback 给不支持 context 的拨号器补一个可取消的包装。
func dialWithContextFallback(ctx context.Context, dial func(network, address string) (net.Conn, error), network, address string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := dial(network, address)
		done <- result{conn: conn, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case out := <-done:
		return out.conn, out.err
	}
}
