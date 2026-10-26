package promptfilter

import (
	"strings"
	"testing"
)

// 2xx 但零字节必须报成「空响应」，不能报成「坏 JSON」。
//
// 这两件事的处理完全不同：坏 JSON 说明模型没按格式回答（重试同一模型没意义，
// 该看提示词）；空响应说明这次复核根本没拿到结果（超时、上下文取消、上游断流
// 后重试耗尽），是链路问题。混成一句话，运维只能看到一堆「invalid JSON」，
// 却不知道审核其实一直在超时。
func TestDecodeReviewResponseReportsEmptyBody(t *testing.T) {
	cfg := NormalizeReviewConfig(ReviewConfig{Model: "codex-auto-review"})

	for _, body := range [][]byte{nil, {}, []byte("   \n\t")} {
		_, err := decodeReviewResponse(200, "application/json", body, cfg)
		if err == nil {
			t.Fatalf("空响应应当报错")
		}
		message := err.Error()
		if !strings.Contains(message, "empty") {
			t.Fatalf("空响应应当明确说 empty，得到 %q", message)
		}
		if strings.Contains(message, "invalid review model JSON") {
			t.Fatalf("空响应被误报成 JSON 解析失败: %q", message)
		}
		if !strings.Contains(message, "status 200") {
			t.Fatalf("应当带上状态码，得到 %q", message)
		}
	}
}

// 真的解析失败时，要把模型说的话带进错误里。
//
// 只报「unexpected end of JSON input」，没人分得清是空回复、被截断、还是夹了
// 自然语言——而这三种的处置完全不同。
func TestDecodeChatReviewResponseIncludesContentInError(t *testing.T) {
	cfg := NormalizeReviewConfig(ReviewConfig{Model: "codex-auto-review"})
	// 截断：有开头没有结尾。
	body := []byte(`{"choices":[{"message":{"content":"{\"flagged\": false, \"confidence\": 0."}}]}`)
	_, err := decodeReviewResponse(200, "application/json", body, cfg)
	if err == nil {
		t.Fatal("截断的 JSON 应当报错")
	}
	message := err.Error()
	if !strings.Contains(message, "invalid review model JSON") {
		t.Fatalf("应当是解析错误，得到 %q", message)
	}
	if !strings.Contains(message, "content") || !strings.Contains(message, "flagged") {
		t.Fatalf("错误里应当带上模型原文，得到 %q", message)
	}

	// 正常回复不受影响。
	ok := []byte(`{"choices":[{"message":{"content":"{\"flagged\": false, \"confidence\": 0.1}"}}]}`)
	outcome, err := decodeReviewResponse(200, "application/json", ok, cfg)
	if err != nil {
		t.Fatalf("正常回复被拒: %v", err)
	}
	if outcome.Flagged || outcome.Confidence != 0.1 {
		t.Fatalf("解析结果不对: %+v", outcome)
	}
}
