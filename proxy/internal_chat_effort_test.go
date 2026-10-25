package proxy

import (
	"context"
	"testing"

	"github.com/tidwall/gjson"
)

// 内部调用的档位必须在重建上游报文之后才写进去——入站的 reasoning 到不了这里。
func TestApplyInternalReasoningEffort(t *testing.T) {
	ctxWith := func(effort string) context.Context {
		return context.WithValue(context.Background(), internalReasoningEffortKey{}, effort)
	}
	body := []byte(`{"model":"codex-auto-review","instructions":"x"}`)

	// 没有声明就不动报文。
	if got := ApplyInternalReasoningEffort(context.Background(), body); gjson.GetBytes(got, "reasoning.effort").String() != "" {
		t.Fatalf("unexpected effort without declaration: %s", got)
	}

	// 声明了就要落到最终报文体上。
	got := ApplyInternalReasoningEffort(ctxWith("none"), body)
	if v := gjson.GetBytes(got, "reasoning.effort").String(); v != "none" {
		t.Fatalf("reasoning.effort = %q, want none; body=%s", v, got)
	}
	// 原有字段不能被动到。
	if v := gjson.GetBytes(got, "instructions").String(); v != "x" {
		t.Fatalf("instructions = %q, want unchanged", v)
	}

	// payload 规则或调用方已经显式指定时不覆盖——更具体的指令优先。
	explicit := []byte(`{"reasoning":{"effort":"high"}}`)
	if got := ApplyInternalReasoningEffort(ctxWith("none"), explicit); gjson.GetBytes(got, "reasoning.effort").String() != "high" {
		t.Fatalf("explicit effort was overwritten: %s", got)
	}

	// 非法值直接忽略，不能把拼错的配置发上游。
	bad := ApplyInternalReasoningEffort(ctxWith("highest"), body)
	if v := gjson.GetBytes(bad, "reasoning.effort").String(); v != "" {
		t.Fatalf("invalid effort leaked through: %q", v)
	}

	// 空 body 不 panic。
	if got := ApplyInternalReasoningEffort(ctxWith("none"), nil); got != nil {
		t.Fatalf("nil body changed: %v", got)
	}
}

func TestNormalizeInternalReasoningEffort(t *testing.T) {
	for _, ok := range []string{"none", "NONE", " low ", "minimal", "medium", "high"} {
		if normalizeInternalReasoningEffort(ok) == "" {
			t.Fatalf("rejected valid effort %q", ok)
		}
	}
	for _, bad := range []string{"", "highest", "off", "0", "思考"} {
		if got := normalizeInternalReasoningEffort(bad); got != "" {
			t.Fatalf("accepted invalid effort %q -> %q", bad, got)
		}
	}
}
