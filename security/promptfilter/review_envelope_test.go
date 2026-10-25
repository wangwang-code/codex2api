package promptfilter

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestReviewPromptTextStripsTransportEnvelope(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"单个字符串", `["你好啊"]`, "你好啊"},
		{"多个字符串", `["a","b"]`, "a\nb"},
		{"带空白", `  [" x "]  `, " x "},
		{"元素里有 JSON 对象文本", `["{\"a\":1}"]`, `{"a":1}`},
		{"不是纯字符串数组", `["a",{"b":1}]`, `["a",{"b":1}]`},
		{"是对象不是数组", `{"a":1}`, `{"a":1}`},
		{"空数组", `[]`, `[]`},
		{"不是 JSON", `今天天气真好`, `今天天气真好`},
		{"只有一半括号", `[abc`, `[abc`},
		{"嵌套数组", `[["a"],"b"]`, `[["a"],"b"]`},
	}
	for _, tc := range cases {
		if got := reviewPromptText(tc.in); got != tc.want {
			t.Fatalf("%s: reviewPromptText(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// 送进审核模型的必须是剥掉信封之后的文本：最外层方括号是我们自己的传输格式，
// 不该被「包含完整 JSON 报文」这条规则命中。
func TestReviewSendsUnwrappedTextToExecutor(t *testing.T) {
	fake := &fakeInternalExecutor{
		handles:  "127.0.0.1:8317",
		status:   http.StatusOK,
		respBody: chatCompletionBody(`{"confidence":0.1,"reason":"ok","is_r18":false}`),
	}
	RegisterInternalExecutor(fake)
	defer RegisterInternalExecutor(nil)

	cfg := internalReviewConfig("http://127.0.0.1:8317/v1")
	cfg.Adapter.ReasoningEffort = "none"

	if _, err := DefaultReviewClient.ReviewTextDetailed(context.Background(), `["你好啊","再见"]`, cfg); err != nil {
		t.Fatalf("ReviewTextDetailed: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("calls = %d, want 1", fake.calls)
	}

	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(fake.lastBody, &body); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	joined := ""
	for _, m := range body.Messages {
		joined += m.Content
	}
	if strings.Contains(joined, `["你好啊"`) {
		t.Fatalf("outer JSON array still present in the review prompt: %s", joined)
	}
	if !strings.Contains(joined, "你好啊") || !strings.Contains(joined, "再见") {
		t.Fatalf("review prompt lost the actual content: %s", joined)
	}

	// 档位必须一并交给执行器，由它在重建上游报文之后落地。
	if fake.lastEffort != "none" {
		t.Fatalf("reasoning effort handed to executor = %q, want none", fake.lastEffort)
	}
}
