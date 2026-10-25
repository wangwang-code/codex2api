package proxy

import "testing"

// Handles decides whether the reviewer is allowed to short-circuit into this
// process. Getting it wrong in the permissive direction would route an audit
// aimed at some other gateway back into us, so the negatives matter as much as
// the positives.
func TestInternalChatExecutorHandles(t *testing.T) {
	exec := InternalChatExecutor{Handler: &Handler{}, Port: 8317}

	yes := []string{
		"http://127.0.0.1:8317/v1",
		"http://localhost:8317/v1",
		"http://[::1]:8317/v1",
		"http://0.0.0.0:8317/v1",
		"http://127.0.0.2:8317/v1",
		"http://127.0.0.1:8317",
	}
	for _, u := range yes {
		if !exec.Handles(u) {
			t.Fatalf("Handles(%q) = false, want true", u)
		}
	}

	no := []string{
		"http://127.0.0.1:9999/v1",       // 端口不符
		"https://openrouter.ai/api/v1",   // 外部主机
		"http://10.0.0.5:8317/v1",        // 非回环
		"",                               // 空
		"not a url",                      // 解析失败
		"http://127.0.0.1/v1",            // 缺端口 → 默认 80，不符
	}
	for _, u := range no {
		if exec.Handles(u) {
			t.Fatalf("Handles(%q) = true, want false", u)
		}
	}

	// 没有 handler 时一律拒绝：宁可走 HTTP，也不能短路到一个空壳。
	if (InternalChatExecutor{Port: 8317}).Handles("http://127.0.0.1:8317/v1") {
		t.Fatal("executor without a handler claimed the endpoint")
	}
}
