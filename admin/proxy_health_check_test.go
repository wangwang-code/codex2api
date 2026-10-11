package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNormalizeProxyHealthCheckURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "empty disables", raw: "   ", want: ""},
		{name: "https trace", raw: "https://auth.openai.com/cdn-cgi/trace", want: "https://auth.openai.com/cdn-cgi/trace"},
		{name: "http with port and path", raw: " http://127.0.0.1:8080/health ", want: "http://127.0.0.1:8080/health"},
		{name: "rejects socks scheme", raw: "socks5://127.0.0.1:1080", wantErr: true},
		{name: "rejects missing scheme", raw: "auth.openai.com/cdn-cgi/trace", wantErr: true},
		{name: "rejects missing host", raw: "https://", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeProxyHealthCheckURL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeProxyHealthCheckURL(%q) expected error, got %q", tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeProxyHealthCheckURL(%q) error = %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("normalizeProxyHealthCheckURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// 配置了健康检查 URL 时，代理检测必须连接这个真实业务目标（而不是内置的 ip-api），
// 并尽力从响应里取出出口 IP。
func TestProbeProxyUsesConfiguredHealthCheckURL(t *testing.T) {
	const healthURL = "http://health.test/cdn-cgi/trace"
	var sawHealth, sawLegacy atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.String(), "health.test") {
			sawHealth.Add(1)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("fl=abc\nh=health.test\nip=203.0.113.9\nts=1\nloc=US\n"))
			return
		}
		sawLegacy.Add(1)
		http.Error(w, "legacy probe target must not be used", http.StatusBadGateway)
	}))
	t.Cleanup(proxyServer.Close)

	oldURLFn := proxyProbeHealthCheckURLFn
	oldGeo := proxyProbeLookupGeoFn
	proxyProbeHealthCheckURLFn = func() string { return healthURL }
	proxyProbeLookupGeoFn = func(_ context.Context, ip, lang string) (string, string, string, string) {
		if ip != "203.0.113.9" || lang != "zh-CN" {
			t.Fatalf("geo lookup ip=%q lang=%q, want 203.0.113.9/zh-CN", ip, lang)
		}
		return "美国", "加州", "洛杉矶", "Example ISP"
	}
	t.Cleanup(func() {
		proxyProbeHealthCheckURLFn = oldURLFn
		proxyProbeLookupGeoFn = oldGeo
	})

	result := probeProxy(context.Background(), proxyServer.URL, "zh-CN")
	if !result.Success || !result.Conclusive {
		t.Fatalf("probe result = %#v, want conclusive success", result)
	}
	if result.IP != "203.0.113.9" {
		t.Fatalf("probe IP = %q, want 203.0.113.9", result.IP)
	}
	if result.Location != "美国·加州·洛杉矶" || result.ISP != "Example ISP" {
		t.Fatalf("probe geo = %q / %q, want 美国·加州·洛杉矶 / Example ISP", result.Location, result.ISP)
	}
	if sawHealth.Load() == 0 {
		t.Fatal("configured health check URL was never requested")
	}
	if sawLegacy.Load() != 0 {
		t.Fatalf("legacy probe target hits = %d, want 0", sawLegacy.Load())
	}
}

// 业务目标返回非 2xx 说明这个代理用不了，必须判为确定失败，且不再做地理查询。
func TestProbeProxyHealthCheckNonSuccessIsConclusive(t *testing.T) {
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(proxyServer.Close)

	oldURLFn := proxyProbeHealthCheckURLFn
	oldGeo := proxyProbeLookupGeoFn
	proxyProbeHealthCheckURLFn = func() string { return "http://health.test/cdn-cgi/trace" }
	proxyProbeLookupGeoFn = func(context.Context, string, string) (string, string, string, string) {
		t.Fatal("geo lookup must not run when the health check failed")
		return "", "", "", ""
	}
	t.Cleanup(func() {
		proxyProbeHealthCheckURLFn = oldURLFn
		proxyProbeLookupGeoFn = oldGeo
	})

	result := probeProxy(context.Background(), proxyServer.URL, "en")
	if result.Success || !result.Conclusive {
		t.Fatalf("probe result = %#v, want conclusive failure", result)
	}
	if !strings.Contains(result.Error, "403") {
		t.Fatalf("probe error = %q, want HTTP 403 note", result.Error)
	}
}

// 未配置（空）时必须回落到内置检测目标，保持既有行为。
func TestProbeProxyWithoutHealthCheckURLKeepsBuiltInTarget(t *testing.T) {
	var sawLegacy atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !strings.Contains(req.URL.String(), "ip-api.com") {
			http.Error(w, "unexpected target", http.StatusBadGateway)
			return
		}
		sawLegacy.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","query":"1.2.3.4","country":"US","regionName":"CA","city":"LA","isp":"Example"}`))
	}))
	t.Cleanup(proxyServer.Close)

	oldURLFn := proxyProbeHealthCheckURLFn
	proxyProbeHealthCheckURLFn = func() string { return "" }
	t.Cleanup(func() { proxyProbeHealthCheckURLFn = oldURLFn })

	result := probeProxy(context.Background(), proxyServer.URL, "en")
	if !result.Success || result.IP != "1.2.3.4" {
		t.Fatalf("probe result = %#v, want built-in ip-api success", result)
	}
	if sawLegacy.Load() == 0 {
		t.Fatal("built-in probe target was never requested")
	}
}
