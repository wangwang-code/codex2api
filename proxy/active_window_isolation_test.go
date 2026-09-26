package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestActiveWindowOutsideFailsFast 端到端验证账号生效时间窗口的隔离：
// 池里唯一的账号被设成「当前不在窗口内」时，请求应当快速失败（503）而不是空等
// 等待超时。
//
// 这条覆盖的是「隔离真的生效」而不只是判定函数返回 false：它走完整 handler，
// 依赖 hasStaticCandidateWithDispatch（→ structurallyDispatchable）判定池里有没有归属，
// 而后者刻意不看冷却，所以窗口判定必须显式覆盖它（否则会退化成空等 30s）。
//
// 关于「窗口清除后恢复」：这里不断言，因为本测试的账号只存在于内存池（newStreamLimit
// 基建不入库），而 store 的账号对账会把「数据库里不存在的账号」标记为 DispatchPaused
// （auth/store.go 的 ReconcileAccounts），几轮请求后账号会被暂停，与窗口无关。
// 恢复能力由 auth 层的 TestActiveWindowExcludesAccountFromPool 覆盖：清除窗口后
// CountServiceableAccounts 与结构性候选都立即恢复。
func TestActiveWindowOutsideFailsFast(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, modelQuotaSSE)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	h, router := newStreamLimitTestHandler(t, upstream.URL, 0)
	account := h.store.FindByID(1)
	if account == nil {
		t.Fatal("测试账号未进入运行时池")
	}

	// 基线：未设窗口时应当成功，证明失败确实是窗口导致的。
	baseline := performModelQuotaRequest(router, "/v1/chat/completions", chatStreamBody)
	if baseline.Code != http.StatusOK {
		t.Fatalf("基线请求应当成功: status=%d body=%q", baseline.Code, baseline.Body.String())
	}

	// 取当前分钟之后的 1 分钟窗口：保证不含「现在」。
	now := time.Now()
	minute := now.Hour()*60 + now.Minute()
	account.SetActiveWindow((minute+2)%1440, (minute+3)%1440)
	if account.InActiveWindow(time.Now()) {
		t.Skip("测试跨越了分钟边界，跳过（窗口与当前时刻重叠）")
	}
	if account.IsAvailable() {
		t.Fatal("窗口外账号不应可用")
	}

	started := time.Now()
	response := performModelQuotaRequest(router, "/v1/chat/completions", chatStreamBody)
	elapsed := time.Since(started)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("窗口外的唯一账号应当让请求失败: status=%d body=%q", response.Code, response.Body.String())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("窗口外应当快速失败，实际耗时 %s（像是空等了等待超时）", elapsed)
	}
	t.Logf("窗口外请求: status=%d elapsed=%s", response.Code, elapsed)
}
