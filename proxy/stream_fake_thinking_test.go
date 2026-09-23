package proxy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestStreamFakeThinkingEnabledFromEnv 验证总开关的默认值与识别规则。
func TestStreamFakeThinkingEnabledFromEnv(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "default-off", want: false},
		{name: "on", raw: "true", want: true},
		{name: "one", raw: "1", want: true},
		{name: "off", raw: "false", want: false},
		{name: "zero", raw: "0", want: false},
		{name: "invalid", raw: "maybe", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("STREAM_FAKE_THINKING_ENABLED", test.raw)
			if got := streamFakeThinkingEnabledFromEnv(); got != test.want {
				t.Fatalf("enabled = %t, want %t", got, test.want)
			}
		})
	}
}

// TestStreamFakeThinkingImmediateFromEnv 验证抢先开流默认开启。
func TestStreamFakeThinkingImmediateFromEnv(t *testing.T) {
	t.Setenv("STREAM_FAKE_THINKING_IMMEDIATE", "")
	if !streamFakeThinkingImmediateFromEnv() {
		t.Fatal("immediate must default to true")
	}
	t.Setenv("STREAM_FAKE_THINKING_IMMEDIATE", "false")
	if streamFakeThinkingImmediateFromEnv() {
		t.Fatal("immediate must honor an explicit false")
	}
}

// TestStreamFakeThinkingTextsFromEnv 验证按序文案列表的两种写法与空项保留。
func TestStreamFakeThinkingTextsFromEnv(t *testing.T) {
	t.Run("pipe-separated", func(t *testing.T) {
		t.Setenv("STREAM_FAKE_THINKING_TEXTS", "第一次|第二次||第四次")
		got := streamFakeThinkingTextsFromEnv()
		want := []string{"第一次", "第二次", "", "第四次"}
		if len(got) != len(want) {
			t.Fatalf("texts = %#v, want %#v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("texts[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("json-array", func(t *testing.T) {
		t.Setenv("STREAM_FAKE_THINKING_TEXTS", `["a","b",""]`)
		got := streamFakeThinkingTextsFromEnv()
		if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "" {
			t.Fatalf("texts = %#v", got)
		}
	})

	t.Run("invalid-json-ignored", func(t *testing.T) {
		t.Setenv("STREAM_FAKE_THINKING_TEXTS", `[not-json`)
		if got := streamFakeThinkingTextsFromEnv(); got != nil {
			t.Fatalf("invalid JSON must be ignored, got %#v", got)
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Setenv("STREAM_FAKE_THINKING_TEXTS", "  ")
		if got := streamFakeThinkingTextsFromEnv(); got != nil {
			t.Fatalf("empty value must yield nil, got %#v", got)
		}
	})
}

// TestNewFakeThinkingStateForChatRespectsMasterSwitch 验证总开关关闭时不构造状态。
func TestNewFakeThinkingStateForChatRespectsMasterSwitch(t *testing.T) {
	restore := streamFakeThinkingEnabled
	defer func() { streamFakeThinkingEnabled = restore }()

	streamFakeThinkingEnabled = false
	if state := newFakeThinkingStateForChat("gpt-5"); state != nil {
		t.Fatal("disabled master switch must yield a nil state")
	}
	streamFakeThinkingEnabled = true
	if state := newFakeThinkingStateForChat("gpt-5"); state == nil {
		t.Fatal("enabled master switch must yield a state")
	}
}

// TestBuildFakeThinkingFrame 验证假思考帧形状：只有 reasoning_content、没有 content。
func TestBuildFakeThinkingFrame(t *testing.T) {
	frame := buildFakeThinkingFrame("gpt-5.6-codex", "让我先理清需求")
	if !strings.HasPrefix(frame, "data: ") || !strings.HasSuffix(frame, "\n\n") {
		t.Fatalf("frame must be an SSE data event, got %q", frame)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(frame, "data: "), "\n\n")
	root := map[string]any{}
	if err := json.Unmarshal([]byte(payload), &root); err != nil {
		t.Fatalf("frame payload is not valid JSON: %v", err)
	}
	if root["object"] != "chat.completion.chunk" {
		t.Fatalf("object = %v, want chat.completion.chunk", root["object"])
	}
	if root["model"] != "gpt-5.6-codex" {
		t.Fatalf("model = %v", root["model"])
	}
	if !strings.HasPrefix(root["id"].(string), "chatcmpl-thinking-") {
		t.Fatalf("id = %v, want chatcmpl-thinking- prefix", root["id"])
	}
	choices, ok := root["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("choices = %#v", root["choices"])
	}
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != nil {
		t.Fatalf("finish_reason = %v, want null", choice["finish_reason"])
	}
	delta := choice["delta"].(map[string]any)
	if delta["reasoning_content"] != "让我先理清需求" {
		t.Fatalf("reasoning_content = %v", delta["reasoning_content"])
	}
	if _, hasContent := delta["content"]; hasContent {
		t.Fatal("fake thinking frame must not carry delta.content")
	}

	if got := buildFakeThinkingFrame("gpt-5", "   "); got != "" {
		t.Fatalf("blank text must yield an empty frame, got %q", got)
	}
}

// TestFakeThinkingStatePayloadSequence 验证按序取用、空项只发心跳、耗尽与停发。
func TestFakeThinkingStatePayloadSequence(t *testing.T) {
	state := &fakeThinkingState{
		protocol:  fakeThinkingProtocolChat,
		model:     "gpt-5.6-codex",
		firstText: "开流首帧",
		texts:     []string{"第一次", "", "第三次"},
	}

	first := state.payload()
	if !strings.Contains(first, "开流首帧") || strings.Contains(first, continuousRetryKeepaliveComment) {
		t.Fatalf("first beat must be the preemptive frame only, got %q", first)
	}

	second := state.payload()
	if !strings.HasPrefix(second, continuousRetryKeepaliveComment) || !strings.Contains(second, "第一次") {
		t.Fatalf("second beat must be comment + ordered text, got %q", second)
	}

	third := state.payload()
	if third != continuousRetryKeepaliveComment {
		t.Fatalf("blank entry must emit the comment only, got %q", third)
	}

	fourth := state.payload()
	if !strings.HasPrefix(fourth, continuousRetryKeepaliveComment) || !strings.Contains(fourth, "第三次") {
		t.Fatalf("fourth beat must consume the third entry, got %q", fourth)
	}

	fifth := state.payload()
	if fifth != continuousRetryKeepaliveComment {
		t.Fatalf("exhausted list must fall back to the comment only, got %q", fifth)
	}

	state.markFirstContentSeen()
	if got := state.payload(); got != continuousRetryKeepaliveComment {
		t.Fatalf("after real content every beat must be the comment only, got %q", got)
	}
}

// TestFakeThinkingStatePayloadWithoutFirstText 验证首帧文案为空时退回纯注释心跳。
func TestFakeThinkingStatePayloadWithoutFirstText(t *testing.T) {
	state := &fakeThinkingState{protocol: fakeThinkingProtocolChat, firstText: ""}
	if got := state.payload(); got != continuousRetryKeepaliveComment {
		t.Fatalf("blank first text must fall back to the comment, got %q", got)
	}
}

// TestFakeThinkingPayloadIgnoresOtherProtocols 验证非 chat 协议不注入假帧。
func TestFakeThinkingPayloadIgnoresOtherProtocols(t *testing.T) {
	state := &fakeThinkingState{protocol: "responses", firstText: "x", texts: []string{"y"}}
	if got := state.payload(); got != continuousRetryKeepaliveComment {
		t.Fatalf("non-chat protocol must not be injected, got %q", got)
	}
}

// TestPrimeImmediatelyMakesFirstKeepaliveDue 验证抢先模式下首个心跳立即到期。
func TestPrimeImmediatelyMakesFirstKeepaliveDue(t *testing.T) {
	restoreInterval := continuousRetryKeepaliveInterval
	defer func() { continuousRetryKeepaliveInterval = restoreInterval }()
	continuousRetryKeepaliveInterval = 30 * time.Second

	primed := &requestContinuousRetryKeepalive{primeImmediately: true}
	primed.Activate()
	if delay := continuousRetryKeepaliveDelay(primed); delay != 0 {
		t.Fatalf("primed keepalive delay = %s, want 0", delay)
	}

	plain := &requestContinuousRetryKeepalive{}
	plain.Activate()
	if delay := continuousRetryKeepaliveDelay(plain); delay != continuousRetryKeepaliveInterval {
		t.Fatalf("plain keepalive delay = %s, want %s", delay, continuousRetryKeepaliveInterval)
	}
}

// TestFakeThinkingPayloadFuncOverridesStaticPayload 验证动态载荷会覆盖静态心跳文案。
func TestFakeThinkingPayloadFuncOverridesStaticPayload(t *testing.T) {
	state := &fakeThinkingState{protocol: fakeThinkingProtocolChat, firstText: "开流首帧"}
	options := continuousRetrySSEKeepaliveOptions{
		contentType:    "text/event-stream",
		payload:        continuousRetryKeepaliveComment,
		payloadFunc:    state.payload,
		primeFirstBeat: true,
	}
	if options.payloadFunc == nil || !options.primeFirstBeat {
		t.Fatal("options must carry the dynamic payload and the priming flag")
	}
	if got := options.payloadFunc(); !strings.Contains(got, "开流首帧") {
		t.Fatalf("dynamic payload = %q", got)
	}
}
