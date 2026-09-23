package proxy

import (
	"encoding/json"
	"log"
	"math"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 上游流预算（迁移自 CPA 的 stream-limits 补丁）。
//
// 目的：防止上游模型「输出失控」——不按预期翻译、无限输出、或长时间挂着不下线。
//
// # CPA 侧踩过的坑（本实现刻意规避）
//
// CPA 的字节预算数的是「下游已翻译的 SSE 帧字节」（其 stream_forwarder.go 里
// `streamBytes += len(chunk)`），而不是模型实际输出的字符。每个 chat.completion.chunk
// 帧的 JSON 信封本身就有约 281 字节，里面只装 1-3 个答案字符，于是：
//
//   - 实测消耗是 80-137 字节/输入字符，而按「输入字符 × 32 字节」配的预算提前 3~4 倍耗尽；
//   - 再叠上 max-bytes 钳制（256 KiB），任何超过约 1900 字符的源文本都会被误杀。
//
// 也就是说：字节预算实际上退化成了「帧数预算」，与输出长度严重非线性，标定极其脆弱，
// 换一个翻译分帧方式就全部失准。
//
// 因此本实现：
//
//  1. 主判据是**输出内容字符数**（只数答案文本，不数 reasoning / tool call），单位与
//     协议无关，也不受分帧方式影响；
//  2. 按输入字符数线性推导预算，系数直接表达「输出不应超过输入的 N 倍」；
//  3. 字节兜底只数**读到的上游原始字节**（max-upstream-bytes），不数下游帧，这样它是
//     稳定的绝对上限，不会随翻译分帧方式漂移。

// streamLimitAbortPayload 是超限时发给客户端的固定 payload（与 CPA 保持一致，
// 便于下游按 code 编程识别）。
const streamLimitAbortPayload = `{"error":{"message":"模型输出失控，已中止","type":"server_error","param":null,"code":"upstream_response_too_large"}}`

// streamLimitRule 是一条流预算规则；API Keys 与 Models 都为空时匹配任意请求。
type streamLimitRule struct {
	Name string `json:"name"`
	// APIKeys 精确匹配下游 API Key；空=任意。
	APIKeys []string `json:"api-keys"`
	// Models 匹配请求模型，支持 shell 通配（如 gpt-5.6-*）；空=任意。
	Models []string `json:"models"`

	// BaseChars 是每个请求的基础预算（字符）。
	BaseChars int `json:"base-chars"`
	// CharsPerInputChar 让预算随输入文本字符数线性增长：直译场景下它就是
	// 「输出不应超过输入的多少倍」。注意不要用字节口径标定（见文件头注释）。
	CharsPerInputChar float64 `json:"chars-per-input-char"`
	// MinChars / MaxChars 钳制算出来的预算。MaxChars 要留足余量：一旦钳制生效，
	// 预算就不再随输入增长，较长的源文本会被无差别中止。
	MinChars int `json:"min-chars"`
	MaxChars int `json:"max-chars"`

	// MaxStreamDuration 是单次上游流的墙钟上限（Go duration 字面量，如 "55s"）。
	MaxStreamDuration string `json:"max-stream-duration"`
	// MaxUpstreamBytes 是单次上游流读取的原始字节上限（兜底，不数下游帧）。
	MaxUpstreamBytes int `json:"max-upstream-bytes"`
}

// streamLimitBudget 是某个请求解析后的预算；零值表示不限。
type streamLimitBudget struct {
	ruleName         string
	maxContentChars  int
	maxDuration      time.Duration
	maxUpstreamBytes int
}

func (b streamLimitBudget) enabled() bool {
	return b.maxContentChars > 0 || b.maxDuration > 0 || b.maxUpstreamBytes > 0
}

var (
	streamLimitsEnabled = streamLimitsEnabledFromEnv()
	streamLimitRules    = streamLimitRulesFromEnv()
)

func streamLimitsEnabledFromEnv() bool {
	return boolFromEnv("STREAM_LIMITS_ENABLED", false)
}

// streamLimitRulesFromEnv 读取规则列表（JSON 数组，按顺序匹配，第一条命中即生效）。
func streamLimitRulesFromEnv() []streamLimitRule {
	raw := strings.TrimSpace(os.Getenv("STREAM_LIMITS_RULES"))
	if raw == "" {
		return nil
	}
	var rules []streamLimitRule
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		log.Printf("[Config] STREAM_LIMITS_RULES 不是合法 JSON 数组，已忽略: %v", err)
		return nil
	}
	for _, rule := range rules {
		logStreamLimitRuleCurve(rule)
	}
	return rules
}

// logStreamLimitRuleCurve 在配置加载时打印规则的预算曲线，便于一眼看出标定是否合理。
//
// 预算公式是 clamp(base-chars + 输入字符 × chars-per-input-char, min-chars, max-chars)。
// 两个钳制都会让比例项失效：下限主导时短输入拿到固定的大预算（例如输入 9 字符却允许
// 输出 2000 字符），上限主导时长源文本被无差别中止（CPA 踩的就是后者）。把曲线直接
// 打出来，比事后翻文档更快发现问题——启动日志里看一眼就知道标定对不对。
func logStreamLimitRuleCurve(rule streamLimitRule) {
	const (
		shortInput = 10
		midInput   = 1000
		longInput  = 10000
	)
	log.Printf("[Config] 流预算规则 %q：预算曲线 clamp(base=%d + 输入×%.2f, min=%d, max=%d) → 输入 %d 字符得 %d；输入 %d 得 %d；输入 %d 得 %d",
		rule.Name, rule.BaseChars, rule.CharsPerInputChar, rule.MinChars, rule.MaxChars,
		shortInput, computeStreamContentChars(rule, shortInput),
		midInput, computeStreamContentChars(rule, midInput),
		longInput, computeStreamContentChars(rule, longInput))

	if rule.MaxChars <= 0 || rule.CharsPerInputChar <= 0 {
		return
	}
	chars := float64(rule.MaxChars-rule.BaseChars) / rule.CharsPerInputChar
	if chars <= 0 {
		log.Printf("[Config] 流预算规则 %q：max-chars=%d 低于 base-chars=%d，预算恒为上限，等于固定输出长度上限", rule.Name, rule.MaxChars, rule.BaseChars)
		return
	}
	log.Printf("[Config] 流预算规则 %q：max-chars=%d 会在输入约 %.0f 字符处开始钳制，超过该长度的请求不再享受随输入增长的预算，可能被误杀；请确认这是预期", rule.Name, rule.MaxChars, chars)
}

// ConfigureStreamLimitsFromEnv 在 config.Load 读取 .env 后刷新流预算配置。
func ConfigureStreamLimitsFromEnv() {
	streamLimitsEnabled = streamLimitsEnabledFromEnv()
	streamLimitRules = streamLimitRulesFromEnv()
}

// resolveStreamLimitBudget 按 API Key 与模型匹配第一条规则并算出预算。
func resolveStreamLimitBudget(apiKey, model string, inputChars int) (streamLimitBudget, bool) {
	if !streamLimitsEnabled {
		return streamLimitBudget{}, false
	}
	for _, rule := range streamLimitRules {
		if !streamLimitMatchAny(rule.APIKeys, strings.TrimSpace(apiKey)) {
			continue
		}
		if !streamLimitMatchModels(rule.Models, strings.TrimSpace(model)) {
			continue
		}
		budget := streamLimitBudget{
			ruleName:         rule.Name,
			maxContentChars:  computeStreamContentChars(rule, inputChars),
			maxUpstreamBytes: rule.MaxUpstreamBytes,
		}
		if d, err := time.ParseDuration(strings.TrimSpace(rule.MaxStreamDuration)); err == nil && d > 0 {
			budget.maxDuration = d
		}
		if !budget.enabled() {
			return streamLimitBudget{}, false
		}
		return budget, true
	}
	return streamLimitBudget{}, false
}

func streamLimitMatchAny(values []string, value string) bool {
	if len(values) == 0 {
		return true
	}
	for _, candidate := range values {
		if strings.TrimSpace(candidate) == value {
			return true
		}
	}
	return false
}

func streamLimitMatchModels(patterns []string, model string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if ok, err := path.Match(pattern, model); err == nil && ok {
			return true
		}
	}
	return false
}

// computeStreamContentChars 按输入字符数线性推导输出字符预算并钳制。
func computeStreamContentChars(rule streamLimitRule, inputChars int) int {
	if inputChars < 0 {
		inputChars = 0
	}
	value := rule.BaseChars
	if rule.CharsPerInputChar > 0 {
		value += int(math.Ceil(float64(inputChars) * rule.CharsPerInputChar))
	}
	if rule.MinChars > 0 && value < rule.MinChars {
		value = rule.MinChars
	}
	if rule.MaxChars > 0 && value > rule.MaxChars {
		value = rule.MaxChars
	}
	if value < 0 {
		value = 0
	}
	return value
}

// countRequestTextChars 估算请求的输入文本字符数（按 rune 计，中文按 1 个字符算，
// 不按字节算——字节口径会让中文输入的预算放大三倍）。覆盖 OpenAI / Responses /
// Gemini 三种常见形态，命中不到时退化为整段 input 文本。
func countRequestTextChars(rawBody []byte) int {
	if len(rawBody) == 0 {
		return 0
	}
	if messages := gjson.GetBytes(rawBody, "messages"); messages.IsArray() {
		total := 0
		for _, message := range messages.Array() {
			total += countContentTextChars(message.Get("content"))
		}
		return total
	}
	if input := gjson.GetBytes(rawBody, "input"); input.Exists() {
		if input.IsArray() {
			total := 0
			for _, item := range input.Array() {
				total += countContentTextChars(item.Get("content"))
			}
			return total
		}
		return utf8.RuneCountInString(input.String())
	}
	if contents := gjson.GetBytes(rawBody, "contents"); contents.IsArray() {
		total := 0
		for _, content := range contents.Array() {
			parts := content.Get("parts")
			if !parts.IsArray() {
				continue
			}
			for _, part := range parts.Array() {
				total += utf8.RuneCountInString(part.Get("text").String())
			}
		}
		return total
	}
	return 0
}

func countContentTextChars(content gjson.Result) int {
	if !content.Exists() {
		return 0
	}
	if content.Type == gjson.String {
		return utf8.RuneCountInString(content.String())
	}
	if !content.IsArray() {
		return 0
	}
	total := 0
	for _, part := range content.Array() {
		switch strings.ToLower(strings.TrimSpace(part.Get("type").String())) {
		case "text", "input_text", "output_text":
			total += utf8.RuneCountInString(part.Get("text").String())
		}
	}
	return total
}

// streamLimitState 在读取上游流的过程中累计用量并判定是否超限。
type streamLimitState struct {
	budget        streamLimitBudget
	contentChars  int
	upstreamBytes int
	startedAt     time.Time
	breached      bool
	reason        string
}

// newStreamLimitStateForRequest 解析本次请求命中的预算；未命中返回 nil（零开销）。
func newStreamLimitStateForRequest(c *gin.Context, model string, rawBody []byte) *streamLimitState {
	if !streamLimitsEnabled {
		return nil
	}
	budget, ok := resolveStreamLimitBudget(apiKeyFromContext(c), model, countRequestTextChars(rawBody))
	if !ok {
		return nil
	}
	return &streamLimitState{budget: budget, startedAt: time.Now()}
}

func apiKeyFromContext(c *gin.Context) string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.GetString("apiKey"))
}

// observe 累计一次上游事件；返回 true 表示已超限，调用方应立即中止上游流。
func (s *streamLimitState) observe(eventBytes, contentRunes int) bool {
	if s == nil {
		return false
	}
	if s.breached {
		return true
	}
	s.upstreamBytes += eventBytes
	s.contentChars += contentRunes
	switch {
	case s.budget.maxUpstreamBytes > 0 && s.upstreamBytes > s.budget.maxUpstreamBytes:
		s.breached, s.reason = true, "upstream_bytes"
	case s.budget.maxContentChars > 0 && s.contentChars > s.budget.maxContentChars:
		s.breached, s.reason = true, "content_chars"
	case s.budget.maxDuration > 0 && time.Since(s.startedAt) > s.budget.maxDuration:
		s.breached, s.reason = true, "duration"
	}
	return s.breached
}

// upstreamContentRunes 只数客户端可见的答案文本，不数 reasoning 与 tool call
// （与 CPA 的 delta.content 口径一致）。
func upstreamContentRunes(eventType string, parsed gjson.Result) int {
	switch eventType {
	case "response.output_text.delta":
		return utf8.RuneCountInString(parsed.Get("delta").String())
	}
	return 0
}

// streamLimitOutcome 是预算中止的终态：502 + terminalLocal。
// terminalLocal 让重试链把它当作请求级失败，不换号重放（上游模型输出失控与账号无关，
// 换号只会再跑一遍同样失控的输出）。
func streamLimitOutcome(state *streamLimitState) streamOutcome {
	reason := ""
	if state != nil {
		reason = state.reason
	}
	return streamOutcome{
		logStatusCode:  http.StatusBadGateway,
		failureKind:    "stream_budget",
		failureMessage: "上游输出超出流预算（" + reason + "），已中止",
		terminalLocal:  true,
	}
}

// writeStreamLimitAbort 向客户端发出预算中止的固定 payload。
// 已提交流就发 SSE 错误帧，否则按 502 + JSON 返回（与 CPA 一致）。
func writeStreamLimitAbort(c *gin.Context) {
	if c == nil {
		return
	}
	if retryKeepaliveCommitted(c) {
		_, _ = c.Writer.WriteString("data: " + streamLimitAbortPayload + "\n\n")
		if flusher, ok := c.Writer.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}
	if !claimContinuousRetryTerminal(c, continuousRetryProtocolOpenAI) {
		return
	}
	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Status(http.StatusBadGateway)
	_, _ = c.Writer.WriteString(streamLimitAbortPayload)
}
