package proxy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/tidwall/gjson"
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

// productionFakeThinkingTexts 是 CPA 侧线上真实使用的按序文案。YAML 双引号里的
// `\n` 会被解析成真实换行符，所以这里直接用带真实换行的字符串复现线上取值。
var productionFakeThinkingTexts = []string{
	"\n云翻译处于灰测中",
	"",
	"\n再耐心等等...",
	"\n这有点超出预计耗时了",
}

// reasoningOf 从假思考帧里取出 reasoning_content，便于断言换行等不可见字符。
func reasoningOf(t *testing.T, payload string) string {
	t.Helper()
	idx := strings.Index(payload, "data: ")
	if idx < 0 {
		t.Fatalf("payload carries no data frame: %q", payload)
	}
	raw := payload[idx+len("data: "):]
	if end := strings.Index(raw, "\n\n"); end >= 0 {
		raw = raw[:end]
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
		t.Fatalf("frame is not valid JSON: %v (%q)", err, raw)
	}
	if len(chunk.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(chunk.Choices))
	}
	return chunk.Choices[0].Delta.ReasoningContent
}

// TestFakeThinkingPreservesProductionLineBreaks 用线上真实文案核对按序取用与换行保留。
// 重点：文案开头的 `\n` 必须原样进入 reasoning_content，空项只发心跳且下标继续推进。
func TestFakeThinkingPreservesProductionLineBreaks(t *testing.T) {
	state := &fakeThinkingState{
		protocol:  fakeThinkingProtocolChat,
		model:     "gpt-5.6-codex",
		firstText: "开流首帧",
		texts:     productionFakeThinkingTexts,
	}

	if got := reasoningOf(t, state.payload()); got != "开流首帧" {
		t.Fatalf("preemptive frame reasoning = %q, want %q", got, "开流首帧")
	}

	beat := func(index int, want string) {
		t.Helper()
		payload := state.payload()
		if !strings.HasPrefix(payload, continuousRetryKeepaliveComment) {
			t.Fatalf("beat %d must start with the SSE comment, got %q", index, payload)
		}
		if got := reasoningOf(t, payload); got != want {
			t.Fatalf("beat %d reasoning = %q, want %q", index, got, want)
		}
	}
	beat(1, "\n云翻译处于灰测中")

	skipped := state.payload()
	if skipped != continuousRetryKeepaliveComment {
		t.Fatalf("empty entry must emit the comment only, got %q", skipped)
	}

	beat(3, "\n再耐心等等...")
	beat(4, "\n这有点超出预计耗时了")

	if got := state.payload(); got != continuousRetryKeepaliveComment {
		t.Fatalf("exhausted list must fall back to the comment only, got %q", got)
	}
}

// TestNewFakeThinkingStateFallsBackToDefaultFirstText 验证首帧文案留空时回落到内置
// 默认文案（与 CPA buildEarlyThinkingChunk 的兜底一致），保证抢先开流一定发出假帧。
func TestNewFakeThinkingStateFallsBackToDefaultFirstText(t *testing.T) {
	restoreEnabled, restoreText := streamFakeThinkingEnabled, streamFakeThinkingText
	defer func() {
		streamFakeThinkingEnabled = restoreEnabled
		streamFakeThinkingText = restoreText
	}()

	streamFakeThinkingEnabled = true
	streamFakeThinkingText = ""
	state := newFakeThinkingStateForChat("gpt-5.6-codex")
	if state == nil {
		t.Fatal("state must be built when the master switch is on")
	}
	if state.firstText != defaultFakeThinkingText {
		t.Fatalf("first text = %q, want the built-in default", state.firstText)
	}
	if got := reasoningOf(t, state.payload()); got != defaultFakeThinkingText {
		t.Fatalf("preemptive reasoning = %q", got)
	}

	streamFakeThinkingText = "\n自定义首帧"
	state = newFakeThinkingStateForChat("gpt-5.6-codex")
	if got := reasoningOf(t, state.payload()); got != "\n自定义首帧" {
		t.Fatalf("preemptive reasoning = %q, want the configured text", got)
	}
}

// TestStreamFakeThinkingTextsDecodeEscapes 验证 `|` 分隔写法支持转义，让线上配置可以
// 直接从 YAML 平移过来（.env 里写 \n 必须还原成真实换行，而不是字面反斜杠 n）。
func TestStreamFakeThinkingTextsDecodeEscapes(t *testing.T) {
	t.Setenv("STREAM_FAKE_THINKING_TEXTS", `\n云翻译处于灰测中||\n再耐心等等...|\n这有点超出预计耗时了`)
	got := streamFakeThinkingTextsFromEnv()
	if len(got) != len(productionFakeThinkingTexts) {
		t.Fatalf("texts = %#v, want %d entries", got, len(productionFakeThinkingTexts))
	}
	for i, want := range productionFakeThinkingTexts {
		if got[i] != want {
			t.Fatalf("texts[%d] = %q, want %q", i, got[i], want)
		}
	}
}

// TestStreamFakeThinkingTextDecodesEscapes 验证首帧文案同样支持转义。
func TestStreamFakeThinkingTextDecodesEscapes(t *testing.T) {
	t.Setenv("STREAM_FAKE_THINKING_TEXT", `\n云翻译处于灰测中`)
	if got := streamFakeThinkingTextFromEnv(); got != "\n云翻译处于灰测中" {
		t.Fatalf("first text = %q, want a real newline prefix", got)
	}
	t.Setenv("STREAM_FAKE_THINKING_TEXT", `   `)
	if got := streamFakeThinkingTextFromEnv(); got != "" {
		t.Fatalf("blank first text must be treated as unset, got %q", got)
	}
}

// TestStreamFakeThinkingTextsSurviveDotenvParsing 用真实 .env 解析器（godotenv v1.5.1）
// 核对两种写法，保证线上 YAML 配置平移过来后拿到的是真实换行、不是字面反斜杠 n，
// 也不会被重复解码。不加引号时 godotenv 不解码转义（交给本包还原）；加双引号时
// godotenv 已经还原过换行（本包不再重复处理）。
func TestStreamFakeThinkingTextsSurviveDotenvParsing(t *testing.T) {
	for _, tc := range []struct{ name, snippet string }{
		{
			name: "unquoted",
			snippet: `STREAM_FAKE_THINKING_TEXTS=\n云翻译处于灰测中||\n再耐心等等...|\n这有点超出预计耗时了` + "\n" +
				`STREAM_FAKE_THINKING_TEXT=\n云翻译处于灰测中` + "\n",
		},
		{
			name: "double-quoted",
			snippet: `STREAM_FAKE_THINKING_TEXTS="\n云翻译处于灰测中||\n再耐心等等...|\n这有点超出预计耗时了"` + "\n" +
				`STREAM_FAKE_THINKING_TEXT="\n云翻译处于灰测中"` + "\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values, err := godotenv.Parse(strings.NewReader(tc.snippet))
			if err != nil {
				t.Fatalf("parse .env snippet: %v", err)
			}
			if got := parseStreamFakeThinkingText(values["STREAM_FAKE_THINKING_TEXT"]); got != "\n云翻译处于灰测中" {
				t.Fatalf("first text = %q, want a real newline prefix", got)
			}
			got := parseStreamFakeThinkingTexts(values["STREAM_FAKE_THINKING_TEXTS"])
			if len(got) != len(productionFakeThinkingTexts) {
				t.Fatalf("texts = %#v, want %d entries", got, len(productionFakeThinkingTexts))
			}
			for i, want := range productionFakeThinkingTexts {
				if got[i] != want {
					t.Fatalf("texts[%d] = %q, want %q", i, got[i], want)
				}
			}
		})
	}
}

// TestFakeThinkingContentArrivedIgnoresReasoning 验证上游 reasoning 事件不停止假思考。
//
// 这是线上现象的回归：上游是 codex 模型，先流式输出 reasoning（思考摘要）再输出译文。
// reasoning 事件同样带 delta 字段，所以 isFirstTokenResult 会把它判成「已出内容」——
// 一旦拿它当停止条件，后续心跳全部退回纯注释，STREAM_FAKE_THINKING_TEXTS 里的按序
// 文案永远轮不到（实测现象：SSE 里只有首帧文案）。
func TestFakeThinkingContentArrivedIgnoresReasoning(t *testing.T) {
	for _, raw := range []string{
		`{"type":"response.reasoning_summary_text.delta","delta":"让我先理清需求"}`,
		`{"type":"response.reasoning_text.delta","delta":"thinking"}`,
	} {
		parsed := gjson.Parse(raw)
		eventType := parsed.Get("type").String()
		if fakeThinkingContentArrived(eventType, parsed) {
			t.Fatalf("%s 不应被视为答案内容，否则假思考会提前停掉", eventType)
		}
		// 对照：通用的严格首字判定确实把它算作内容。这说明两个判据不能共用——
		// contentTokenSeen 含 reasoning 是对的（上游一旦开始输出就不该换号重放），
		// 但假思考的停止时机必须更严。
		if !isFirstTokenResult(parsed) {
			t.Fatalf("%s 本应被 isFirstTokenResult 判为内容，用例前提不成立", eventType)
		}
	}
}

// TestFakeThinkingContentArrivedOnAnswerText 验证真正的答案正文会停止假思考。
func TestFakeThinkingContentArrivedOnAnswerText(t *testing.T) {
	for _, raw := range []string{
		`{"type":"response.output_text.delta","delta":"译文开头"}`,
		`{"type":"response.output_text.done","text":"译文"}`,
		`{"type":"response.function_call_arguments.delta","delta":"{\"a\""}`,
	} {
		parsed := gjson.Parse(raw)
		if !fakeThinkingContentArrived(parsed.Get("type").String(), parsed) {
			t.Fatalf("%s 应当被视为答案内容", parsed.Get("type").String())
		}
	}
}

// TestFakeThinkingKeepsSequencingAfterReasoning 验证「先来 reasoning、后有心跳」时
// 按序文案照常推进——也就是修复后的完整行为，而不只是单个判据。
func TestFakeThinkingKeepsSequencingAfterReasoning(t *testing.T) {
	state := &fakeThinkingState{
		protocol:  fakeThinkingProtocolChat,
		model:     "gpt-5.6-codex",
		firstText: "开流首帧",
		texts:     productionFakeThinkingTexts,
	}

	// 首帧（抢先开流）。
	if got := reasoningOf(t, state.payload()); got != "开流首帧" {
		t.Fatalf("preemptive reasoning = %q", got)
	}

	// 上游开始输出 reasoning：不得停止假思考。
	reasoning := gjson.Parse(`{"type":"response.reasoning_summary_text.delta","delta":"上游在思考"}`)
	if fakeThinkingContentArrived(reasoning.Get("type").String(), reasoning) {
		t.Fatal("reasoning 不应停止假思考")
	}

	// 心跳继续：按序文案照常发出（修复前这里只会是纯注释）。
	if got := reasoningOf(t, state.payload()); got != "\n云翻译处于灰测中" {
		t.Fatalf("reasoning 之后的首个心跳 = %q，按序文案被吞掉了", got)
	}

	// 译文到达后才停。
	answer := gjson.Parse(`{"type":"response.output_text.delta","delta":"译文"}`)
	if !fakeThinkingContentArrived(answer.Get("type").String(), answer) {
		t.Fatal("答案正文应当停止假思考")
	}
	state.markFirstContentSeen()
	if got := state.payload(); got != continuousRetryKeepaliveComment {
		t.Fatalf("答案到达后心跳应为纯注释，got %q", got)
	}
}
