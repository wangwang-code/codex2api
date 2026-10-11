package security

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestParseHostsOverrideForms(t *testing.T) {
	raw := "api.openai.com=1.2.3.4\n" +
		"chatgpt.com 5.6.7.8  # hosts 文件风格 + 注释\n" +
		"Example.COM=9.9.9.9;\n" +
		"trailing.dot.=10.0.0.1,"
	h, err := ParseHostsOverride(raw)
	if err != nil {
		t.Fatalf("ParseHostsOverride() error = %v", err)
	}
	if got := h.Len(); got != 4 {
		t.Fatalf("Len() = %d, want 4", got)
	}
	cases := map[string]string{
		"api.openai.com": "1.2.3.4",
		"chatgpt.com":    "5.6.7.8",
		"example.com":    "9.9.9.9",
		"trailing.dot":   "10.0.0.1",
	}
	for host, want := range cases {
		got, ok := h.Lookup(host)
		if !ok || got != want {
			t.Fatalf("Lookup(%q) = %q,%v want %q", host, got, ok, want)
		}
	}
	// 大小写不敏感
	if ip, ok := h.Lookup("API.OpenAI.Com"); !ok || ip != "1.2.3.4" {
		t.Fatalf("case-insensitive lookup failed: %q,%v", ip, ok)
	}
}

func TestParseHostsOverrideEmptyIsDisabled(t *testing.T) {
	for _, raw := range []string{"", "   ", "\n", "# only comment\n"} {
		h, err := ParseHostsOverride(raw)
		if err != nil {
			t.Fatalf("ParseHostsOverride(%q) error = %v", raw, err)
		}
		if h.Enabled() {
			t.Fatalf("ParseHostsOverride(%q) should be disabled", raw)
		}
	}
}

func TestParseHostsOverrideRejectsInvalid(t *testing.T) {
	cases := []string{
		"api.openai.com",                 // 缺 IP
		"api.openai.com=not-an-ip",       // IP 非法
		"=1.2.3.4",                       // 缺域名
		"api.openai.com=1.2.3.4=5.6.7.8", // 多余等号 -> ip 非法
		"a b c",                          // 三个字段
	}
	for _, raw := range cases {
		if _, err := ParseHostsOverride(raw); err == nil {
			t.Fatalf("ParseHostsOverride(%q) expected error", raw)
		}
	}
}

func TestHostsOverrideRewriteHostPort(t *testing.T) {
	h, err := ParseHostsOverride("api.openai.com=1.2.3.4,ipv6.test=2001:db8::1")
	if err != nil {
		t.Fatalf("ParseHostsOverride() error = %v", err)
	}

	cases := []struct {
		in   string
		want string
	}{
		{"api.openai.com:443", "1.2.3.4:443"},
		{"api.openai.com", "1.2.3.4"},
		{"other.example:443", "other.example:443"}, // 未命中保持原样
		{"1.1.1.1:443", "1.1.1.1:443"},             // 已是 IP
		{"[2001:db8::2]:443", "[2001:db8::2]:443"},
		{"ipv6.test:8443", "[2001:db8::1]:8443"}, // IPv6 需要加方括号
		{"", ""},
	}
	for _, tc := range cases {
		if got := h.RewriteHostPort(tc.in); got != tc.want {
			t.Fatalf("RewriteHostPort(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGlobalHostsOverrideInstallAndClear(t *testing.T) {
	defer SetHostsOverride("")

	if err := SetHostsOverride("api.openai.com=1.2.3.4"); err != nil {
		t.Fatalf("SetHostsOverride() error = %v", err)
	}
	if !HostsOverrideEnabled() {
		t.Fatal("HostsOverrideEnabled() = false, want true")
	}
	if got := RewriteHostPort("api.openai.com:443"); got != "1.2.3.4:443" {
		t.Fatalf("RewriteHostPort() = %q", got)
	}
	if err := SetHostsOverride(""); err != nil {
		t.Fatalf("clear error = %v", err)
	}
	if HostsOverrideEnabled() {
		t.Fatal("HostsOverrideEnabled() = true after clear")
	}
	if got := RewriteHostPort("api.openai.com:443"); got != "api.openai.com:443" {
		t.Fatalf("RewriteHostPort() after clear = %q", got)
	}
	if err := SetHostsOverride("bad=not-an-ip"); err == nil {
		t.Fatal("invalid override should error")
	}
}

func TestWrapDialContextRewritesAddress(t *testing.T) {
	defer SetHostsOverride("")
	if err := SetHostsOverride("api.openai.com=10.11.12.13"); err != nil {
		t.Fatalf("SetHostsOverride() error = %v", err)
	}

	var got string
	stop := errors.New("stop")
	base := func(_ context.Context, _, address string) (net.Conn, error) {
		got = address
		return nil, stop
	}
	if _, err := WrapDialContext(base)(context.Background(), "tcp", "api.openai.com:443"); !errors.Is(err, stop) {
		t.Fatalf("err = %v, want stop", err)
	}
	if got != "10.11.12.13:443" {
		t.Fatalf("dialed address = %q, want 10.11.12.13:443", got)
	}
	if WrapDialContext(nil) != nil {
		t.Fatal("WrapDialContext(nil) should be nil")
	}
}

func TestTargetRewriteDialerRewritesTarget(t *testing.T) {
	defer SetHostsOverride("")
	if err := SetHostsOverride("api.openai.com=10.11.12.13"); err != nil {
		t.Fatalf("SetHostsOverride() error = %v", err)
	}

	var got string
	stop := errors.New("stop")
	inner := DialContextFunc(func(_ context.Context, _, address string) (net.Conn, error) {
		got = address
		return nil, stop
	})
	d := TargetRewriteDialer{Inner: inner}
	if _, err := d.DialContext(context.Background(), "tcp", "api.openai.com:443"); !errors.Is(err, stop) {
		t.Fatalf("err = %v, want stop", err)
	}
	if got != "10.11.12.13:443" {
		t.Fatalf("target = %q, want 10.11.12.13:443", got)
	}
}

// TestHostsDialerForcesDirectDial 端到端验证：直连拨号被强制成覆盖 IP。
func TestHostsDialerForcesDirectDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	accepted := make(chan struct{}, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		accepted <- struct{}{}
		conn.Close()
	}()

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	defer SetHostsOverride("")
	if err := SetHostsOverride("direct.test=127.0.0.1"); err != nil {
		t.Fatalf("SetHostsOverride() error = %v", err)
	}

	d := NewDialer(2*time.Second, 0)
	conn, err := d.Dial("tcp", net.JoinHostPort("direct.test", port))
	if err != nil {
		t.Fatalf("Dial() error = %v", err)
	}
	defer conn.Close()

	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("override dial never reached the listener")
	}
}

// TestCONNECTTunnelDialerForcesIP 端到端验证：代理端点域名→IP，且 CONNECT 目标
// 也被强制成覆盖 IP（这正是绕开代理侧远端解析的关键）。
func TestCONNECTTunnelDialerForcesIP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	type captured struct {
		method string
		target string
	}
	got := make(chan captured, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			got <- captured{}
			return
		}
		got <- captured{method: req.Method, target: req.URL.Host}
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		_, _ = io.Copy(io.Discard, br)
	}()

	_, port, _ := net.SplitHostPort(ln.Addr().String())
	defer SetHostsOverride("")
	if err := SetHostsOverride("proxy.test=127.0.0.1,api.openai.com=10.11.12.13"); err != nil {
		t.Fatalf("SetHostsOverride() error = %v", err)
	}

	d := &CONNECTTunnelDialer{ProxyAddr: net.JoinHostPort("proxy.test", port)}
	conn, err := d.DialContext(context.Background(), "tcp", "api.openai.com:443")
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	defer conn.Close()

	select {
	case c := <-got:
		if c.method != http.MethodConnect {
			t.Fatalf("method = %q, want CONNECT", c.method)
		}
		if c.target != "10.11.12.13:443" {
			t.Fatalf("CONNECT target = %q, want 10.11.12.13:443", c.target)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fake proxy never received the CONNECT request")
	}
}

// TestCONNECTTunnelDialerWithoutOverrideKeepsHost 验证未命中覆盖时目标保持域名。
func TestCONNECTTunnelDialerWithoutOverrideKeepsHost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			got <- ""
			return
		}
		got <- req.URL.Host
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		_, _ = io.Copy(io.Discard, br)
	}()

	defer SetHostsOverride("")
	d := &CONNECTTunnelDialer{ProxyAddr: ln.Addr().String()}
	conn, err := d.DialContext(context.Background(), "tcp", "api.openai.com:443")
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	defer conn.Close()

	select {
	case target := <-got:
		if target != "api.openai.com:443" {
			t.Fatalf("CONNECT target = %q, want api.openai.com:443", target)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("fake proxy never received the CONNECT request")
	}
}
