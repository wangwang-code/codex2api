package auth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// withRefreshEndpoint 把刷新请求指到测试服务器上。
//
// TokenURL 是常量，但 Resin 装饰器是可变钩子，传一个 resinAccountID 就会走它
// 改写过后的 URL —— 这样不用为了测试去改生产代码的常量。
func withRefreshEndpoint(t *testing.T, endpoint string) {
	t.Helper()
	prev := ResinRequestDecorator
	ResinRequestDecorator = func(_, _ string) string { return endpoint }
	t.Cleanup(func() { ResinRequestDecorator = prev })
}

// 连接层 EOF：服务端收到请求就直接断开，一个响应字节都没回。
//
// 这类失败以前被当成「刷新结果未确认」，于是 FailCodexRefresh 把这次刷新挂成
// 未确认，账号从此不再使用这把 RT，要求重新授权并导入凭据。一次网络抖动换来
// 一次人工重授权，代价完全不对等。
func TestRefreshEOFIsTransportFailureNotUncertain(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()
	withRefreshEndpoint(t, srv.URL)

	td, info, err := RefreshAccessToken(context.Background(), "rt-eof", "", "1")
	if err == nil {
		t.Fatal("连接被断开时应当报错")
	}
	if td != nil || info != nil {
		t.Fatalf("失败时不该返回 token: %+v %+v", td, info)
	}
	if isOAuthRefreshUncertain(err) {
		t.Fatalf("EOF 不该被当成结果未确认: %v", err)
	}
	if IsPermanentRefreshFailure(err) {
		t.Fatalf("网络错误不该被判成永久失效: %v", err)
	}
	if strings.Contains(err.Error(), "结果未确认") {
		t.Fatalf("错误文案不该出现「结果未确认」: %v", err)
	}
	if !strings.Contains(err.Error(), "网络") {
		t.Fatalf("错误文案应当点明是网络失败: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("单次调用只该发一次请求，实际 %d", hits)
	}
}

// TLS 握手/证书校验失败同样是网络错误：请求根本没进到应用层。
func TestRefreshTLSErrorIsTransportFailureNotUncertain(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"at"}`))
	}))
	defer srv.Close()
	withRefreshEndpoint(t, srv.URL)

	_, _, err := RefreshAccessToken(context.Background(), "rt-tls", "", "1")
	if err == nil {
		t.Fatal("自签证书应当校验失败")
	}
	if isOAuthRefreshUncertain(err) {
		t.Fatalf("TLS 失败不该被当成结果未确认: %v", err)
	}
	if IsPermanentRefreshFailure(err) {
		t.Fatalf("TLS 失败不该被判成永久失效: %v", err)
	}
}

// 网络失败要重试，而不是一次就放弃 —— 这是「这只是网络错误」的实际含义。
func TestRefreshRetriesTransportFailure(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()
	withRefreshEndpoint(t, srv.URL)

	_, _, err := RefreshWithRetry(context.Background(), "rt-retry", "", "1")
	if err == nil {
		t.Fatal("应当最终失败")
	}
	if got := atomic.LoadInt32(&hits); got != MaxRetries {
		t.Fatalf("应当重试到 %d 次，实际 %d", MaxRetries, got)
	}
	if isOAuthRefreshUncertain(err) {
		t.Fatalf("重试耗尽后仍是网络失败，不该标成未确认: %v", err)
	}
}

// 边界：服务端已经回了 200，只是响应体读不出来/解析不了 —— 那才是**真的**未确认。
// 服务端显然处理过这次刷新，可能已经把 RT 轮换掉了，这时继续用旧 RT 才危险。
func TestRefreshAnsweredButUnusableBodyStaysUncertain(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"缺少 access_token", `{"token_type":"Bearer"}`},
		{"不是 JSON", `not json at all`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			withRefreshEndpoint(t, srv.URL)

			_, _, err := RefreshAccessToken(context.Background(), "rt-body", "", "1")
			if err == nil {
				t.Fatal("应当报错")
			}
			if !isOAuthRefreshUncertain(err) {
				t.Fatalf("服务端已回 200 但响应不可用时应当是未确认: %v", err)
			}
		})
	}
}

// 分类器本身：判据是「有没有收到响应」，不是「错误看起来严不严重」。
func TestIsOAuthRefreshTransportFailure(t *testing.T) {
	transport := []error{
		io.EOF,
		io.ErrUnexpectedEOF,
		&url.Error{Op: "Post", URL: "https://auth.openai.com", Err: io.EOF},
		&url.Error{Op: "Post", URL: "https://auth.openai.com", Err: errors.New("tls: handshake failure")},
		&url.Error{Op: "Post", URL: "https://auth.openai.com", Err: errors.New("x509: certificate signed by unknown authority")},
		&net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")},
		errors.New("read tcp 127.0.0.1:1->127.0.0.1:2: read: connection reset by peer"),
		errors.New("remote error: tls: bad record MAC"),
		context.DeadlineExceeded,
		context.Canceled,
	}
	for _, err := range transport {
		if !isOAuthRefreshTransportFailure(err) {
			t.Fatalf("%v 应当判为传输层失败", err)
		}
	}

	notTransport := []error{
		nil,
		errors.New("刷新响应缺少 access_token"),
		errors.New("invalid_grant"),
	}
	for _, err := range notTransport {
		if isOAuthRefreshTransportFailure(err) {
			t.Fatalf("%v 不该判为传输层失败", err)
		}
	}
}
