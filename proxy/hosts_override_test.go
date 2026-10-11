package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/codex2api/security"
)

// fakeConnectProxy 启动一个只回 200 的假 CONNECT 代理，返回监听地址与
// 收到的 CONNECT 目标通道。
func fakeConnectProxy(t *testing.T) (addr string, targetCh chan string, cleanup func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	targetCh = make(chan string, 1)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				targetCh <- req.URL.Host
				_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
				_, _ = io.Copy(io.Discard, br)
			}()
		}
	}()
	return ln.Addr().String(), targetCh, func() { ln.Close() }
}

func TestApplyWebsocketProxyDefaultUsesProxyURL(t *testing.T) {
	defer security.SetHostsOverride("")
	if err := security.SetHostsOverride(""); err != nil {
		t.Fatalf("SetHostsOverride() error = %v", err)
	}

	dialer := &websocket.Dialer{}
	if err := ApplyWebsocketProxy(dialer, "SOCKS5H://user:pass@proxy.example:1080"); err != nil {
		t.Fatalf("ApplyWebsocketProxy() error = %v", err)
	}
	if dialer.Proxy == nil {
		t.Fatal("expected http.ProxyURL to be configured")
	}
	u, err := dialer.Proxy(nil)
	if err != nil {
		t.Fatalf("Proxy() error = %v", err)
	}
	if u.Scheme != "socks5" {
		t.Fatalf("scheme = %q, want socks5 (socks5h alias normalized)", u.Scheme)
	}
	if dialer.NetDialContext != nil {
		t.Fatal("tunnel dialer should not be installed without hosts override")
	}
}

func TestApplyWebsocketProxyRejectsInvalidURL(t *testing.T) {
	dialer := &websocket.Dialer{}
	if err := ApplyWebsocketProxy(dialer, "socks5h://"); err == nil {
		t.Fatal("expected error for malformed proxy URL")
	}
	if dialer.Proxy != nil || dialer.NetDialContext != nil {
		t.Fatal("invalid proxy URL must not configure the dialer")
	}
}

// 覆盖启用时 WS 拨号改走自建隧道，目标地址被强制成覆盖 IP。
func TestApplyWebsocketProxyTunnelsAndForcesIP(t *testing.T) {
	addr, targetCh, cleanup := fakeConnectProxy(t)
	defer cleanup()

	defer security.SetHostsOverride("")
	if err := security.SetHostsOverride("api.openai.com=10.11.12.13"); err != nil {
		t.Fatalf("SetHostsOverride() error = %v", err)
	}

	dialer := &websocket.Dialer{}
	if err := ApplyWebsocketProxy(dialer, "http://"+addr); err != nil {
		t.Fatalf("ApplyWebsocketProxy() error = %v", err)
	}
	if dialer.Proxy != nil {
		t.Fatal("hosts override should replace Proxy with a self-built tunnel")
	}
	if dialer.NetDialContext == nil {
		t.Fatal("expected tunnel NetDialContext")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := dialer.NetDialContext(ctx, "tcp", "api.openai.com:443")
	if err != nil {
		t.Fatalf("NetDialContext() error = %v", err)
	}
	defer conn.Close()

	select {
	case target := <-targetCh:
		if target != "10.11.12.13:443" {
			t.Fatalf("CONNECT target = %q, want 10.11.12.13:443", target)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fake proxy never received the CONNECT request")
	}
}

// uTLS 通道的代理拨号同样把代理端点与 CONNECT 目标强制成覆盖 IP。
func TestBuildProxyDialerForcesIPForHTTPProxy(t *testing.T) {
	addr, targetCh, cleanup := fakeConnectProxy(t)
	defer cleanup()

	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}

	defer security.SetHostsOverride("")
	if err := security.SetHostsOverride("proxy.test=127.0.0.1,api.openai.com=10.11.12.13"); err != nil {
		t.Fatalf("SetHostsOverride() error = %v", err)
	}

	dialer, err := buildProxyDialer("http://" + net.JoinHostPort("proxy.test", port))
	if err != nil {
		t.Fatalf("buildProxyDialer() error = %v", err)
	}

	done := make(chan error, 1)
	go func() {
		conn, err := dialer.Dial("tcp", "api.openai.com:443")
		if err != nil {
			done <- err
			return
		}
		conn.Close()
		done <- nil
	}()

	select {
	case target := <-targetCh:
		if target != "10.11.12.13:443" {
			t.Fatalf("CONNECT target = %q, want 10.11.12.13:443", target)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fake proxy never received the CONNECT request")
	}
	if err := <-done; err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
}
