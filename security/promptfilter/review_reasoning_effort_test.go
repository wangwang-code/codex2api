package promptfilter

import (
	"encoding/json"
	"testing"
)

func reasoningEffortOf(t *testing.T, payload []byte) (string, bool) {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	reasoning, ok := body["reasoning"].(map[string]any)
	if !ok {
		return "", false
	}
	effort, _ := reasoning["effort"].(string)
	return effort, true
}

// 审核是一次分类，不是解题。档位必须能显式钉住，否则上游默认档位会让模型
// 过度思考——这正是把普通数值当成 JSON 结构误判的那条路径。
func TestReviewPayloadCarriesReasoningEffort(t *testing.T) {
	cfg := DefaultReviewConfig()
	cfg.Enabled = true
	cfg.Adapter.RequestMode = ReviewRequestModeChatCompletions
	cfg.Adapter.ReasoningEffort = "none"

	payload, err := buildReviewPayload("137*29=___", cfg)
	if err != nil {
		t.Fatalf("buildReviewPayload: %v", err)
	}
	effort, present := reasoningEffortOf(t, payload)
	if !present || effort != "none" {
		t.Fatalf("reasoning.effort = %q (present=%v), want none", effort, present)
	}
}

// 不配就不干预报文：既有部署的请求体必须逐字不变。
func TestReviewPayloadOmitsReasoningWhenUnset(t *testing.T) {
	cfg := DefaultReviewConfig()
	cfg.Enabled = true
	cfg.Adapter.RequestMode = ReviewRequestModeChatCompletions
	cfg.Adapter.ReasoningEffort = ""

	payload, err := buildReviewPayload("text", cfg)
	if err != nil {
		t.Fatalf("buildReviewPayload: %v", err)
	}
	if _, present := reasoningEffortOf(t, payload); present {
		t.Fatal("reasoning field must be absent when no effort is configured")
	}
}

// 自定义 payload_template 里已经写了 reasoning 时，配置项不能覆盖它——
// 模板是更具体的意图。
func TestReviewPayloadTemplateReasoningWins(t *testing.T) {
	cfg := DefaultReviewConfig()
	cfg.Enabled = true
	cfg.Adapter.RequestMode = ReviewRequestModeChatCompletions
	cfg.Adapter.ReasoningEffort = "none"
	cfg.Adapter.PayloadTemplate = `{"model":"{{model}}","messages":[{"role":"system","content":"{{system_prompt}}"},{"role":"user","content":"{{user_prompt}}"}],"reasoning":{"effort":"high"}}`

	payload, err := buildReviewPayload("text", cfg)
	if err != nil {
		t.Fatalf("buildReviewPayload: %v", err)
	}
	effort, present := reasoningEffortOf(t, payload)
	if !present || effort != "high" {
		t.Fatalf("reasoning.effort = %q, want the template's high", effort)
	}
}

// 模板没写 reasoning 时，配置项负责补上。
func TestReviewPayloadTemplateGetsConfiguredEffort(t *testing.T) {
	cfg := DefaultReviewConfig()
	cfg.Enabled = true
	cfg.Adapter.RequestMode = ReviewRequestModeChatCompletions
	cfg.Adapter.ReasoningEffort = "minimal"
	cfg.Adapter.PayloadTemplate = `{"model":"{{model}}","messages":[{"role":"system","content":"{{system_prompt}}"},{"role":"user","content":"{{user_prompt}}"}]}`

	payload, err := buildReviewPayload("text", cfg)
	if err != nil {
		t.Fatalf("buildReviewPayload: %v", err)
	}
	effort, present := reasoningEffortOf(t, payload)
	if !present || effort != "minimal" {
		t.Fatalf("reasoning.effort = %q (present=%v), want minimal", effort, present)
	}
}

// 拼错的档位不能被悄悄发出去。
func TestNormalizeReviewReasoningEffort(t *testing.T) {
	accept := map[string]string{
		"none": "none", "NONE": "none", " low ": "low",
		"minimal": "minimal", "medium": "medium", "high": "high",
	}
	for raw, want := range accept {
		if got := normalizeReviewReasoningEffort(raw); got != want {
			t.Fatalf("normalize(%q) = %q, want %q", raw, got, want)
		}
	}
	for _, raw := range []string{"", "highest", "off", "0", "none; drop table", "思考"} {
		if got := normalizeReviewReasoningEffort(raw); got != "" {
			t.Fatalf("normalize(%q) = %q, want empty", raw, got)
		}
	}
}
