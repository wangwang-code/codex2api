package proxy

import (
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// TestStreamLimitCountsContentCharsNotFrameBytes 是本实现与 CPA 的关键差异。
//
// 预算数的是模型输出的**内容字符**，不是下游 SSE 帧字节。CPA 把帧字节当预算
// （每帧 JSON 信封约 281 字节却只装 1-3 个答案字符），导致实测消耗是 80-137 字节
// /输入字符，标定脆到换个分帧方式就全部失准。这个用例把该教训钉住：帧很多、字节
// 很大，但内容很少时绝不能中止。
func TestStreamLimitCountsContentCharsNotFrameBytes(t *testing.T) {
	rule := streamLimitRule{BaseChars: 100, CharsPerInputChar: 3, MinChars: 100}
	if got := computeStreamContentChars(rule, 10); got != 130 {
		t.Fatalf("输入 10 字符的预算 = %d, want 130", got)
	}

	state := &streamLimitState{budget: streamLimitBudget{maxContentChars: 130}, startedAt: time.Now()}
	frame := []byte(`data: {"type":"response.output_text.delta","delta":"字"}`)

	// 20 帧、每帧 1 个字符：帧字节合计远超 130，但内容只有 20 字符，不应触发。
	for i := 0; i < 20; i++ {
		if state.observe(len(frame), 1) {
			t.Fatalf("第 %d 帧就中止了：说明预算按帧字节而不是内容字符计数", i+1)
		}
	}
	if state.contentChars != 20 {
		t.Fatalf("内容字符 = %d, want 20", state.contentChars)
	}
	if want := 20 * len(frame); state.upstreamBytes != want {
		t.Fatalf("上游字节 = %d, want %d", state.upstreamBytes, want)
	}

	// 内容累计超过 130 字符才中止，且原因是 content_chars。
	for i := 0; i < 200 && !state.breached; i++ {
		state.observe(0, 1)
	}
	if !state.breached || state.reason != "content_chars" {
		t.Fatalf("应当因内容字符超限中止，state = %+v", state)
	}
}

// TestCountRequestTextCharsCountsRunes 验证输入字符数按 rune 计：中文算 1 个字符
// 而不是 3 个字节，否则中文输入的预算会被放大三倍。
func TestCountRequestTextCharsCountsRunes(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","messages":[{"role":"user","content":"你好世界"}]}`)
	if got := countRequestTextChars(body); got != 4 {
		t.Fatalf("输入字符数 = %d, want 4（中文按字符计，不按字节）", got)
	}
	if got := countRequestTextChars([]byte(`{"model":"gpt-5.6","input":"abcd"}`)); got != 4 {
		t.Fatalf("Responses 形态输入字符数 = %d, want 4", got)
	}
	if got := countRequestTextChars(nil); got != 0 {
		t.Fatalf("空请求体 = %d, want 0", got)
	}
}

// TestResolveStreamLimitBudgetEmptyMatchersMatchAnything 验证 api-keys / models
// 留空即匹配任意：单流量网关不需要像 CPA 那样填具体 Key。
func TestResolveStreamLimitBudgetEmptyMatchersMatchAnything(t *testing.T) {
	restoreEnabled, restoreRules := streamLimitsEnabled, streamLimitRules
	t.Cleanup(func() { streamLimitsEnabled, streamLimitRules = restoreEnabled, restoreRules })

	streamLimitsEnabled = true
	streamLimitRules = []streamLimitRule{{Name: "all", BaseChars: 100, MinChars: 100}}

	for _, tc := range []struct{ apiKey, model string }{
		{"sk-anything", "gpt-5.6-codex"},
		{"", ""},
		{"sk-other", "claude-sonnet-4-5"},
	} {
		if _, ok := resolveStreamLimitBudget(tc.apiKey, tc.model, 10); !ok {
			t.Fatalf("api-keys/models 留空时应匹配任意请求，未命中 %+v", tc)
		}
	}
}

// TestResolveStreamLimitBudget 验证规则匹配与预算推导。
func TestResolveStreamLimitBudget(t *testing.T) {
	restoreEnabled, restoreRules := streamLimitsEnabled, streamLimitRules
	t.Cleanup(func() { streamLimitsEnabled, streamLimitRules = restoreEnabled, restoreRules })

	streamLimitsEnabled = false
	if _, ok := resolveStreamLimitBudget("sk-a", "gpt-5.6", 10); ok {
		t.Fatal("关闭时不应命中")
	}

	streamLimitsEnabled = true
	streamLimitRules = []streamLimitRule{
		{Name: "other-key", APIKeys: []string{"sk-b"}, BaseChars: 10},
		{Name: "translation", APIKeys: []string{"sk-a"}, Models: []string{"gpt-5.6-*"},
			BaseChars: 100, CharsPerInputChar: 3, MinChars: 100, MaxChars: 5000, MaxUpstreamBytes: 1 << 20},
	}

	budget, ok := resolveStreamLimitBudget("sk-a", "gpt-5.6-codex", 100)
	if !ok {
		t.Fatal("应当命中 translation 规则")
	}
	if budget.ruleName != "translation" || budget.maxContentChars != 400 {
		t.Fatalf("budget = %+v", budget)
	}
	if budget.maxUpstreamBytes != 1<<20 {
		t.Fatalf("上游字节上限 = %d", budget.maxUpstreamBytes)
	}
	// API Key 不匹配 → 不命中。
	if _, ok := resolveStreamLimitBudget("sk-c", "gpt-5.6-codex", 100); ok {
		t.Fatal("API Key 不匹配不应命中")
	}
	// 模型通配不匹配 → 不命中。
	if _, ok := resolveStreamLimitBudget("sk-a", "claude-sonnet", 100); ok {
		t.Fatal("模型不匹配不应命中")
	}
}

// TestComputeStreamContentCharsClamps 验证 min/max 钳制。
func TestComputeStreamContentCharsClamps(t *testing.T) {
	rule := streamLimitRule{BaseChars: 10, CharsPerInputChar: 1, MinChars: 100, MaxChars: 200}
	if got := computeStreamContentChars(rule, 0); got != 100 {
		t.Fatalf("下限钳制 = %d, want 100", got)
	}
	if got := computeStreamContentChars(rule, 1000); got != 200 {
		t.Fatalf("上限钳制 = %d, want 200", got)
	}
	// 负输入按 0 处理。
	if got := computeStreamContentChars(streamLimitRule{BaseChars: 50}, -5); got != 50 {
		t.Fatalf("负输入 = %d, want 50", got)
	}
}

// TestStreamLimitObserveDurationAndUpstreamBytes 验证时长与上游字节两个兜底判据。
func TestStreamLimitObserveDurationAndUpstreamBytes(t *testing.T) {
	state := &streamLimitState{
		budget:    streamLimitBudget{maxUpstreamBytes: 10},
		startedAt: time.Now(),
	}
	if state.observe(11, 0) != true || state.reason != "upstream_bytes" {
		t.Fatalf("上游字节超限未触发，state = %+v", state)
	}

	timed := &streamLimitState{
		budget:    streamLimitBudget{maxDuration: time.Nanosecond},
		startedAt: time.Now().Add(-time.Second),
	}
	if timed.observe(1, 0) != true || timed.reason != "duration" {
		t.Fatalf("时长超限未触发，state = %+v", timed)
	}

	// nil 状态是零开销路径。
	var nilState *streamLimitState
	if nilState.observe(1, 1) {
		t.Fatal("nil 状态不应触发")
	}
}

// TestStreamLimitsEnabledFromEnv 验证总开关默认关闭。
func TestStreamLimitsEnabledFromEnv(t *testing.T) {
	t.Setenv("STREAM_LIMITS_ENABLED", "")
	if streamLimitsEnabledFromEnv() {
		t.Fatal("流预算必须默认关闭")
	}
	t.Setenv("STREAM_LIMITS_ENABLED", "true")
	if !streamLimitsEnabledFromEnv() {
		t.Fatal("显式开启应生效")
	}
}

// TestStreamLimitRulesFromEnv 验证规则 JSON 解析与非法输入处理。
func TestStreamLimitRulesFromEnv(t *testing.T) {
	t.Setenv("STREAM_LIMITS_RULES", `[{"name":"t","models":["gpt-5.6-*"],"base-chars":100,"chars-per-input-char":3}]`)
	rules := streamLimitRulesFromEnv()
	if len(rules) != 1 || rules[0].Name != "t" || rules[0].BaseChars != 100 || rules[0].CharsPerInputChar != 3 {
		t.Fatalf("rules = %+v", rules)
	}

	t.Setenv("STREAM_LIMITS_RULES", `{not-json`)
	if got := streamLimitRulesFromEnv(); got != nil {
		t.Fatalf("非法 JSON 应被忽略，got %+v", got)
	}
	t.Setenv("STREAM_LIMITS_RULES", "  ")
	if got := streamLimitRulesFromEnv(); got != nil {
		t.Fatalf("空值应返回 nil，got %+v", got)
	}
}

// TestUpstreamContentRunesIgnoresReasoningAndTools 验证只数答案文本：
// reasoning 与 tool call 参数不进预算（与 CPA 的 delta.content 口径一致）。
func TestUpstreamContentRunesIgnoresReasoningAndTools(t *testing.T) {
	delta := []byte(`{"type":"response.output_text.delta","delta":"答案"}`)
	reasoning := []byte(`{"type":"response.reasoning_summary_text.delta","delta":"思考"}`)
	tool := []byte(`{"type":"response.function_call_arguments.delta","delta":"{\"a\":1}"}`)

	if got := upstreamContentRunes("response.output_text.delta", gjson.ParseBytes(delta)); got != 2 {
		t.Fatalf("答案文本 = %d, want 2", got)
	}
	if got := upstreamContentRunes("response.reasoning_summary_text.delta", gjson.ParseBytes(reasoning)); got != 0 {
		t.Fatalf("reasoning 不应计入预算，got %d", got)
	}
	if got := upstreamContentRunes("response.function_call_arguments.delta", gjson.ParseBytes(tool)); got != 0 {
		t.Fatalf("tool call 不应计入预算，got %d", got)
	}
}

// TestStreamLimitBudgetCurveForShortInput 固化短输入的预算算术。
//
// base-chars / min-chars 共同构成「短输入下限」，输入很小时预算基本就等于它：
// 设成 2000 会让 9 个字符的输入允许输出 2000+ 字符，比例防御直接失效。
func TestStreamLimitBudgetCurveForShortInput(t *testing.T) {
	loose := streamLimitRule{BaseChars: 2000, CharsPerInputChar: 3, MinChars: 2000}
	if got := computeStreamContentChars(loose, 9); got != 2027 {
		t.Fatalf("宽松配置：9 字符输入得 %d，公式应为 2000 + 9×3", got)
	}
	tight := streamLimitRule{BaseChars: 200, CharsPerInputChar: 4, MinChars: 200}
	if got := computeStreamContentChars(tight, 9); got != 236 {
		t.Fatalf("收紧配置：9 字符输入得 %d，公式应为 200 + 9×4", got)
	}
	if got := computeStreamContentChars(tight, 1000); got != 4200 {
		t.Fatalf("收紧配置：1000 字符输入得 %d，公式应为 200 + 1000×4", got)
	}
}
