package proxy

import (
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
)

// withErrorRewrite 临时打开改写配置，跑完还原，避免污染同包其他用例。
func withErrorRewrite(t *testing.T, defaultText string, statusTexts map[int]string) {
	t.Helper()
	prevEnabled, prevDefault, prevTexts := upstreamErrorRewriteEnabled, upstreamErrorRewriteDefault, upstreamErrorRewriteStatusTexts
	upstreamErrorRewriteEnabled = true
	upstreamErrorRewriteDefault = defaultText
	upstreamErrorRewriteStatusTexts = statusTexts
	t.Cleanup(func() {
		upstreamErrorRewriteEnabled, upstreamErrorRewriteDefault, upstreamErrorRewriteStatusTexts = prevEnabled, prevDefault, prevTexts
	})
}

// 改写只作用于「发给下游」的文案，用量日志必须留着真实原因。
//
// 之前是把 rewriteUpstreamErrorText 的返回值写回 outcome.failureMessage 本身，
// 于是日志跟着被改写：下游取消（499）、网关合成的断流（598）、流预算中止、
// 首字超时全部记成「上游服务暂时不可用」，用量页再也分不清是哪一种。
func TestUsageLogKeepsRealFailureReason(t *testing.T) {
	withErrorRewrite(t, "[云翻译]上游服务暂时不可用，请稍后重试", map[int]string{
		502: "[云翻译]上游服务暂时不可用，请稍后重试",
	})

	cases := []struct {
		name    string
		outcome streamOutcome
		wantLog string
	}{
		{
			name:    "下游取消",
			outcome: streamOutcome{logStatusCode: logStatusClientClosed, failureMessage: "下游请求上下文超时"},
			wantLog: "下游请求上下文超时",
		},
		{
			name:    "首字超时",
			outcome: firstTokenTimeoutOutcome(8 * time.Second),
			wantLog: "上游首字超时",
		},
		{
			name:    "流预算中止",
			outcome: streamLimitOutcome(&streamLimitState{reason: "content_chars"}),
			wantLog: "上游输出超出流预算（content_chars）",
		},
		{
			name:    "上游过载",
			outcome: streamOutcome{logStatusCode: 500, failureKind: "server", failureMessage: "server_is_overloaded · service_unavailable_error · Our servers are currently overloaded"},
			wantLog: "server_is_overloaded",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.outcome.failureMessage == "" {
				t.Fatal("用例没设置 failureMessage")
			}
			// 日志侧：真实原因原样保留
			logged := usageLogFailureMessage(tc.outcome.logStatusCode, tc.outcome.failureMessage)
			if !strings.Contains(logged, tc.wantLog) {
				t.Fatalf("用量日志丢了真实原因：%q（想要包含 %q）", logged, tc.wantLog)
			}
			if strings.Contains(logged, "上游服务暂时不可用") {
				t.Fatalf("用量日志被改写成配置话术：%q", logged)
			}
			// 客户端侧：仍然是配置话术，而且明确标出「改写过了」
			clientText, rewritten := tc.outcome.clientFailureText()
			if !rewritten {
				t.Fatalf("客户端文案应当标记为已改写")
			}
			if clientText != "[云翻译]上游服务暂时不可用，请稍后重试" {
				t.Fatalf("客户端文案 = %q", clientText)
			}
			// 原始字段没有被污染
			if strings.Contains(tc.outcome.failureMessage, "上游服务暂时不可用") {
				t.Fatalf("failureMessage 被就地改写了：%q", tc.outcome.failureMessage)
			}
		})
	}
}

// 没开改写时，客户端拿到的就是真实原因，且不能标记成「已改写」。
func TestClientFailureTextNotMarkedWhenRewriteDisabled(t *testing.T) {
	prevEnabled := upstreamErrorRewriteEnabled
	upstreamErrorRewriteEnabled = false
	t.Cleanup(func() { upstreamErrorRewriteEnabled = prevEnabled })

	outcome := streamOutcome{logStatusCode: 500, failureMessage: "upstream exploded"}
	text, rewritten := outcome.clientFailureText()
	if rewritten {
		t.Fatal("未启用改写却标记了 rewritten")
	}
	if text != "upstream exploded" {
		t.Fatalf("文案 = %q", text)
	}
}

// 过载熔断靠 error_message 里找 server_is_overloaded。改写把它盖掉过，
// 于是熔断永远不触发——这条用例锁住「字面量能活到日志里」。
func TestOverloadFailureSurvivesToUsageLog(t *testing.T) {
	withErrorRewrite(t, "[云翻译]上游服务暂时不可用，请稍后重试", nil)

	payload := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"Our servers are currently overloaded. Please try again later."}}}`)
	outcome := classifyResponseFailedOutcome(payload)
	if !strings.Contains(outcome.failureMessage, overloadErrorCode) {
		t.Fatalf("failureMessage 里没有过载码：%q", outcome.failureMessage)
	}

	logged := usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage)
	input := &database.UsageLogInput{StatusCode: outcome.logStatusCode, ErrorMessage: logged}
	if !isOverloadedUsageError(input) {
		t.Fatalf("过载熔断认不出这次失败：%q", logged)
	}

	// 客户端仍然只看到配置话术，且被标记为改写。
	clientText, rewritten := outcome.clientFailureText()
	if !rewritten || strings.Contains(clientText, overloadErrorCode) {
		t.Fatalf("客户端文案处理不对：%q rewritten=%v", clientText, rewritten)
	}
}

// 流预算中止是本地的请求级失败，不能算成上游过载。
func TestStreamBudgetIsNotCountedAsOverload(t *testing.T) {
	outcome := streamLimitOutcome(&streamLimitState{reason: "duration"})
	if outcome.penalize {
		t.Fatal("流预算中止不该惩罚账号")
	}
	if !outcome.terminalLocal {
		t.Fatal("流预算中止应当是本地终态（不换号重试）")
	}
	logged := usageLogFailureMessage(outcome.logStatusCode, outcome.failureMessage)
	if isOverloadedUsageError(&database.UsageLogInput{StatusCode: outcome.logStatusCode, ErrorMessage: logged}) {
		t.Fatalf("流预算中止被当成上游过载：%q", logged)
	}
}
