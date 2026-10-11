package auth

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/codex2api/security"
)

// 启用全局强制 hosts 覆盖后，HTTP 代理必须改走自建 CONNECT 隧道——只有自建隧道
// 才能把 CONNECT 目标也强制成覆盖 IP（标准库 transport.Proxy 没有改写钩子）。
func TestConfigureTransportProxyHTTPTunnelsWhenHostsOverrideEnabled(t *testing.T) {
	if err := security.SetHostsOverride("api.openai.com=10.11.12.13"); err != nil {
		t.Fatalf("SetHostsOverride() error = %v", err)
	}
	defer security.SetHostsOverride("")

	transport := &http.Transport{}
	if err := ConfigureTransportProxy(transport, "http://127.0.0.1:8080", nil); err != nil {
		t.Fatalf("ConfigureTransportProxy() error = %v", err)
	}
	if transport.Proxy != nil {
		t.Fatal("hosts override should replace transport.Proxy with a self-built tunnel")
	}
	if transport.DialContext == nil {
		t.Fatal("expected a CONNECT tunnel DialContext")
	}
}

// 未启用覆盖时保持既有行为（transport.Proxy 生效）。
func TestConfigureTransportProxyHTTPKeepsProxyWithoutOverride(t *testing.T) {
	if err := security.SetHostsOverride(""); err != nil {
		t.Fatalf("SetHostsOverride() error = %v", err)
	}

	transport := &http.Transport{}
	if err := ConfigureTransportProxy(transport, "http://127.0.0.1:8080", nil); err != nil {
		t.Fatalf("ConfigureTransportProxy() error = %v", err)
	}
	if transport.Proxy == nil {
		t.Fatal("expected HTTP proxy handler to be configured")
	}
}

// 端到端：假 SOCKS5 代理 + 覆盖，断言 SOCKS5 请求里下发的是 IP 而不是域名。
func TestConfigureTransportProxySOCKS5ForcesIPWhenOverrideEnabled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	targetCh := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		target, err := fakeSOCKS5Handshake(conn)
		if err != nil {
			targetCh <- "handshake-error: " + err.Error()
			return
		}
		targetCh <- target
		_, _ = io.Copy(io.Discard, conn)
	}()

	if err := security.SetHostsOverride("api.openai.com=10.11.12.13"); err != nil {
		t.Fatalf("SetHostsOverride() error = %v", err)
	}
	defer security.SetHostsOverride("")

	transport := &http.Transport{}
	if err := ConfigureTransportProxy(transport, "socks5h://"+ln.Addr().String(), nil); err != nil {
		t.Fatalf("ConfigureTransportProxy() error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := transport.DialContext(ctx, "tcp", "api.openai.com:443")
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	defer conn.Close()

	select {
	case target := <-targetCh:
		if target != "10.11.12.13:443" {
			t.Fatalf("SOCKS5 target = %q, want 10.11.12.13:443", target)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fake SOCKS5 proxy never received the CONNECT request")
	}
}

// fakeSOCKS5Handshake 完成最小 SOCKS5 握手并返回客户端请求的目标 host:port。
func fakeSOCKS5Handshake(conn net.Conn) (string, error) {
	br := bufio.NewReader(conn)

	greeting := make([]byte, 2)
	if _, err := io.ReadFull(br, greeting); err != nil {
		return "", err
	}
	if greeting[0] != 0x05 {
		return "", fmt.Errorf("unexpected socks version %d", greeting[0])
	}
	methods := make([]byte, int(greeting[1]))
	if _, err := io.ReadFull(br, methods); err != nil {
		return "", err
	}
	if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
		return "", err
	}

	head := make([]byte, 4)
	if _, err := io.ReadFull(br, head); err != nil {
		return "", err
	}
	if head[0] != 0x05 || head[1] != 0x01 {
		return "", fmt.Errorf("unexpected socks request %v", head)
	}

	var host string
	switch head[3] {
	case 0x01:
		buf := make([]byte, 4)
		if _, err := io.ReadFull(br, buf); err != nil {
			return "", err
		}
		host = net.IP(buf).String()
	case 0x04:
		buf := make([]byte, 16)
		if _, err := io.ReadFull(br, buf); err != nil {
			return "", err
		}
		host = net.IP(buf).String()
	case 0x03:
		length := make([]byte, 1)
		if _, err := io.ReadFull(br, length); err != nil {
			return "", err
		}
		buf := make([]byte, int(length[0]))
		if _, err := io.ReadFull(br, buf); err != nil {
			return "", err
		}
		host = string(buf)
	default:
		return "", fmt.Errorf("unexpected socks address type %d", head[3])
	}

	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(br, portBytes); err != nil {
		return "", err
	}
	port := int(portBytes[0])<<8 | int(portBytes[1])

	if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}
