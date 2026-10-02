package proxy

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withStreamStallTimeout 临时安装全局停顿超时（包级变量，规则未配置时回落到它）。
func withStreamStallTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	previous := streamStallTimeout
	streamStallTimeout = timeout
	t.Cleanup(func() { streamStallTimeout = previous })
}

// waitForStreamStallFire 等待看门狗触发，避免用固定 sleep 制造偶发失败。
func waitForStreamStallFire(t *testing.T, guard *streamStallGuard) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if guard.Fired() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("停顿看门狗在 3s 内没有触发")
}

// TestStreamStallGuardWaitsForFirstUpstreamEvent 验证起算点是首个上游事件：
// 上游还没开口时不得计时，那一段归 first_token_timeout_seconds 管。
func TestStreamStallGuardWaitsForFirstUpstreamEvent(t *testing.T) {
	var canceled atomic.Bool
	guard := newStreamStallGuard(40*time.Millisecond, func() { canceled.Store(true) })
	t.Cleanup(guard.Stop)

	time.Sleep(150 * time.Millisecond)
	if guard.Fired() || canceled.Load() {
		t.Fatal("首个上游事件之前不应起算")
	}

	guard.MarkEvent()
	waitForStreamStallFire(t, guard)
	if !canceled.Load() {
		t.Fatal("触发后必须 cancel 上游上下文，否则读流不会返回")
	}
}

// TestStreamStallGuardResetsOnEveryUpstreamEvent 验证持续产出不会被判为停顿——
// 这是防误杀的核心语义：看门狗管「空闲」，不管「总时长」。
func TestStreamStallGuardResetsOnEveryUpstreamEvent(t *testing.T) {
	guard := newStreamStallGuard(200*time.Millisecond, func() {})
	t.Cleanup(guard.Stop)

	for i := 0; i < 6; i++ {
		guard.MarkEvent()
		time.Sleep(50 * time.Millisecond)
	}
	if guard.Fired() {
		t.Fatal("每个上游事件都应重置空闲计时，持续有事件不应被判为停顿")
	}
}

// TestStreamStallGuardStopPreventsLateCancel 验证 attempt 收尾后残留定时器不再动手：
// 否则它会 cancel 掉下一轮 attempt 的上游上下文（换号重试时会串台）。
func TestStreamStallGuardStopPreventsLateCancel(t *testing.T) {
	var canceled atomic.Bool
	guard := newStreamStallGuard(40*time.Millisecond, func() { canceled.Store(true) })
	guard.MarkEvent()
	guard.Stop()

	time.Sleep(150 * time.Millisecond)
	if guard.Fired() || canceled.Load() {
		t.Fatal("Stop 之后不得再触发或 cancel")
	}
}

// TestStreamStallGuardDisabledIsNilAndSafe 验证默认关闭路径零开销且 nil 安全。
func TestStreamStallGuardDisabledIsNilAndSafe(t *testing.T) {
	if guard := newStreamStallGuard(0, func() {}); guard != nil {
		t.Fatal("timeout <= 0 时应关闭看门狗")
	}
	if guard := newStreamStallGuard(time.Second, nil); guard != nil {
		t.Fatal("缺少 cancel 时不应安装看门狗")
	}

	var guard *streamStallGuard
	guard.MarkEvent()
	guard.Stop()
	if guard.Fired() {
		t.Fatal("nil 看门狗不应报告已触发")
	}
	if guard.Timeout() != 0 {
		t.Fatal("nil 看门狗的超时应为 0")
	}
}

// TestStreamStallTimeoutForRequestPrefersMatchedRule 验证规则内 stall-timeout 能精确
// 圈定某条线路（key+模型），未命中时回落全局值——这是「只给翻译线布防」的能力。
func TestStreamStallTimeoutForRequestPrefersMatchedRule(t *testing.T) {
	withStreamStallTimeout(t, 9*time.Second)
	withStreamLimitRule(t, streamLimitRule{
		Name:         "translation",
		APIKeys:      []string{"sk-app"},
		Models:       []string{"gpt-5.6-*"},
		StallTimeout: "12s",
	})

	cases := []struct {
		name  string
		key   string
		model string
		want  time.Duration
	}{
		{"key+模型都命中 → 用规则值", "sk-app", "gpt-5.6-luna", 12 * time.Second},
		{"key 不命中 → 回落全局", "sk-other", "gpt-5.6-luna", 9 * time.Second},
		{"模型不命中 → 回落全局", "sk-app", "gpt-5.5", 9 * time.Second},
	}
	for _, tc := range cases {
		if got := streamStallTimeoutForRequest(tc.key, tc.model); got != tc.want {
			t.Fatalf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestStreamStallTimeoutForRequestFallsBackWhenRuleSilent 验证规则没配（或配了非法值）时
// 严格回落全局，不会被空串吞掉整个布防。
func TestStreamStallTimeoutForRequestFallsBackWhenRuleSilent(t *testing.T) {
	withStreamStallTimeout(t, 9*time.Second)
	withStreamLimitRule(t, streamLimitRule{
		Name: "translation", APIKeys: []string{"sk-app"}, BaseChars: 20, CharsPerInputChar: 1,
	})
	if got := streamStallTimeoutForRequest("sk-app", "gpt-5.6-luna"); got != 9*time.Second {
		t.Fatalf("规则未配 stall-timeout 时应回落全局，got %s", got)
	}
}

// TestStreamStallTimeoutForRequestIgnoresInvalidRuleValue 验证非法/非正值不会覆盖全局。
func TestStreamStallTimeoutForRequestIgnoresInvalidRuleValue(t *testing.T) {
	withStreamStallTimeout(t, 9*time.Second)
	withStreamLimitRule(t, streamLimitRule{
		Name: "translation", APIKeys: []string{"sk-app"}, StallTimeout: "12 秒",
	})
	if got := streamStallTimeoutForRequest("sk-app", "gpt-5.6-luna"); got != 9*time.Second {
		t.Fatalf("非法 stall-timeout 应回落全局，got %s", got)
	}
}

// TestStreamStallOutcomeIsRetryableUpstreamBreak 钉住终态语义：598（对外 502 或
// 流内断流事件）+ penalize + 非 terminalLocal，这样零写入时能换号重试，
// 已写正文时由既有出口补流内断流事件。
func TestStreamStallOutcomeIsRetryableUpstreamBreak(t *testing.T) {
	outcome := streamStallOutcome(12 * time.Second)
	if outcome.failureKind != "stream_stall" {
		t.Fatalf("failureKind = %q, want stream_stall", outcome.failureKind)
	}
	if outcome.logStatusCode != logStatusUpstreamStreamBreak {
		t.Fatalf("logStatusCode = %d, want %d", outcome.logStatusCode, logStatusUpstreamStreamBreak)
	}
	if !outcome.penalize {
		t.Fatal("上游挂死应按上游故障处理（可冷却/换号）")
	}
	if outcome.terminalLocal {
		t.Fatal("停顿中止不是本地请求级失败，不应阻止换号重试")
	}
	if !strings.Contains(outcome.failureMessage, "12s") {
		t.Fatalf("failureMessage 应带上生效超时便于事后标定: %q", outcome.failureMessage)
	}
	if !isStreamStallOutcome(outcome) {
		t.Fatal("isStreamStallOutcome 应识别停顿终态（非流式出口依赖它把半截正文改成报错）")
	}
}
