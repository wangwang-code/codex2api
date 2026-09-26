package proxy

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// 本文件把 CPA 补丁（cliproxy-thinking-mask 的 _cpa-overlay）里的
// “抢先思考 + 按序 Keepalive 假思考”迁移到 codex2api 的下游保活链路上。
//
// 与 CPA 的对应关系：
//   - CPA `streaming.keepalive-seconds`        -> DOWNSTREAM_HTTP_KEEPALIVE_INTERVAL（已有）
//   - CPA `streaming.fake-thinking-text`       -> STREAM_FAKE_THINKING_TEXT
//   - CPA `streaming.fake-thinking-texts`      -> STREAM_FAKE_THINKING_TEXTS
//   - CPA “keepalive>0 即抢先开流”              -> STREAM_FAKE_THINKING_IMMEDIATE
//   - CPA 的开关语义（默认关闭，显式打开才伪装） -> STREAM_FAKE_THINKING_ENABLED
//
// 与 CPA 的差异（有意为之）：
//   - 只在 `/v1/chat/completions` 的下游协议里注入 `chat.completion.chunk` 假帧；
//     Responses / Messages / Gemini 等协议保持原有的纯注释心跳，避免把非本协议的
//     帧混进客户端事件流。
//   - 首个上游真实内容 token 一到就停止注入，之后退回纯注释心跳（CPA 的
//     “first token still pending” 语义）。

const (
	// defaultFakeThinkingText 与 CPA 内置默认文案保持一致。
	defaultFakeThinkingText = "Let me think through this step by step before giving the final answer."

	// fakeThinkingProtocolChat 是当前唯一支持注入假思考帧的下游协议。
	fakeThinkingProtocolChat = "chat"

	// defaultStreamFakeThinkingInterval 是假思考在「等上游首个内容」空窗里的心跳节奏。
	// 与 CPA 的 streaming.keepalive-seconds 对齐：CPA 线上用的是 1s，示例配置 15s。
	// 全局保活间隔（DOWNSTREAM_HTTP_KEEPALIVE_INTERVAL）默认 30s，按那个节奏走的话
	// 空窗内连第二次心跳都等不到，按序文案只会停在开流首帧。
	defaultStreamFakeThinkingInterval = time.Second
)

// 配置变量形式与 downstreamSSEKeepaliveInterval 一致：包初始化时读取进程环境变量，
// config.Load 读取 .env 后由 ConfigureFromEnv 再刷新一次。
var (
	streamFakeThinkingEnabled   = streamFakeThinkingEnabledFromEnv()
	streamFakeThinkingImmediate = streamFakeThinkingImmediateFromEnv()
	streamFakeThinkingText      = streamFakeThinkingTextFromEnv()
	streamFakeThinkingTexts     = streamFakeThinkingTextsFromEnv()
	streamFakeThinkingInterval  = streamFakeThinkingIntervalFromEnv()
)

// streamFakeThinkingIntervalFromEnv 读取假思考在空窗里的心跳节奏。
// 默认 1s（对齐 CPA 线上）；0 表示跟随全局保活间隔。
func streamFakeThinkingIntervalFromEnv() time.Duration {
	return durationFromEnv("STREAM_FAKE_THINKING_INTERVAL", defaultStreamFakeThinkingInterval)
}

// streamFakeThinkingEnabledFromEnv 读取伪装思考总开关，默认关闭。
// 关闭时本文件所有逻辑都不生效，下游保活行为与改动前完全一致。
func streamFakeThinkingEnabledFromEnv() bool {
	return boolFromEnv("STREAM_FAKE_THINKING_ENABLED", false)
}

// streamFakeThinkingImmediateFromEnv 读取“抢先开流”开关，默认开启（但受总开关约束）。
// 开启时首个心跳在第一次等待循环里立即落地，也就是请求一进入等待上游的阶段就提交
// SSE 200 并发出首帧假思考；关闭时沿用原有节奏，等满一个保活周期才发第一个心跳。
func streamFakeThinkingImmediateFromEnv() bool {
	return boolFromEnv("STREAM_FAKE_THINKING_IMMEDIATE", true)
}

// streamFakeThinkingTextFromEnv 读取开流首帧的假思考文案；留空用内置默认文案。
func streamFakeThinkingTextFromEnv() string {
	return parseStreamFakeThinkingText(os.Getenv("STREAM_FAKE_THINKING_TEXT"))
}

// parseStreamFakeThinkingText 解析首帧文案原始值。
// 值与 CPA 的 YAML 双引号语义对齐：`\n` 等转义会还原成真实字符，首尾空白原样保留
// （线上文案靠开头的 `\n` 换行，不能被裁掉）；纯空白视为未配置。
func parseStreamFakeThinkingText(raw string) string {
	value := decodeEnvEscapes(raw)
	if strings.TrimSpace(value) == "" {
		return ""
	}
	return value
}

// streamFakeThinkingTextsFromEnv 读取按序 Keepalive 文案列表。
func streamFakeThinkingTextsFromEnv() []string {
	return parseStreamFakeThinkingTexts(os.Getenv("STREAM_FAKE_THINKING_TEXTS"))
}

// parseStreamFakeThinkingTexts 解析按序文案原始值，支持两种写法：
//   - JSON 数组（转义由 JSON 解码器负责）
//   - `|` 分隔的字符串（逐项做转义还原，保留空项：空项=该次只发心跳，下标继续推进）
//
// 分隔符先切再解码，所以文案本身可以含有解码后才出现的换行。
// 注意 .env 两种写法的差异：不加引号时值里是字面 `\n`（由本函数还原）；加了双引号时
// godotenv 已经还原过换行，本函数看不到反斜杠，因此不会重复解码。
func parseStreamFakeThinkingTexts(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	// 只对「是否为空」和「是不是 JSON」做去空白判断，真正切分用原值：
	// 双引号写法下 godotenv 不做裁剪，首项开头的换行必须保留；不加引号时
	// godotenv 已裁掉首尾空格，这里无需再裁。
	if trimmed := strings.TrimSpace(raw); strings.HasPrefix(trimmed, "[") {
		var list []string
		if err := json.Unmarshal([]byte(trimmed), &list); err != nil {
			log.Printf("[Config] STREAM_FAKE_THINKING_TEXTS 不是合法 JSON 数组，已忽略: %v", err)
			return nil
		}
		return list
	}
	parts := strings.Split(raw, "|")
	for i := range parts {
		parts[i] = decodeEnvEscapes(parts[i])
	}
	return parts
}

// ConfigureStreamFakeThinkingFromEnv 在 config.Load 读取 .env 后刷新伪装思考配置。
func ConfigureStreamFakeThinkingFromEnv() {
	streamFakeThinkingEnabled = streamFakeThinkingEnabledFromEnv()
	streamFakeThinkingImmediate = streamFakeThinkingImmediateFromEnv()
	streamFakeThinkingText = streamFakeThinkingTextFromEnv()
	streamFakeThinkingTexts = streamFakeThinkingTextsFromEnv()
	streamFakeThinkingInterval = streamFakeThinkingIntervalFromEnv()
	logStreamFakeThinkingConfig()
}

// logStreamFakeThinkingConfig 打印伪装思考的实际生效配置。
//
// 文案列表是这一块最容易配错又最难自查的一项：变量名写错、`|` 写成别的符号、跨行书写
// 都会静默变成空列表，表现为「下游只有首帧假思考，后续心跳全是纯注释」，从日志里完全
// 看不出原因。所以启动时把解析结果显式打出来。
func logStreamFakeThinkingConfig() {
	if !streamFakeThinkingEnabled {
		return
	}
	first, source := streamFakeThinkingText, "STREAM_FAKE_THINKING_TEXT"
	if strings.TrimSpace(first) == "" {
		first, source = defaultFakeThinkingText, "内置默认（STREAM_FAKE_THINKING_TEXT 未配置或为空）"
	}
	log.Printf("[Config] 伪装思考已启用：抢先开流=%v，首帧文案来自 %s（%q）", streamFakeThinkingImmediate, source, first)
	if len(streamFakeThinkingTexts) == 0 {
		log.Printf("[Config] STREAM_FAKE_THINKING_TEXTS 解析出 0 条：后续心跳只会发纯注释，不再有假思考文案。" +
			"请检查变量名与写法——需要单行、以 `|` 分隔（写 \\n 会还原成换行），或用 JSON 数组写法；" +
			".env 里跨行书写会导致整个文件解析失败")
		return
	}
	parts := make([]string, 0, len(streamFakeThinkingTexts))
	for i, text := range streamFakeThinkingTexts {
		if strings.TrimSpace(text) == "" {
			parts = append(parts, fmt.Sprintf("[%d]空项(该次只发心跳)", i))
			continue
		}
		parts = append(parts, fmt.Sprintf("[%d]%q", i, text))
	}
	log.Printf("[Config] STREAM_FAKE_THINKING_TEXTS 解析出 %d 条：%s", len(streamFakeThinkingTexts), strings.Join(parts, " "))
	// 按序文案每次心跳只推进一条，而上游首个内容事件一到假思考就停止，所以能发出几条
	// 取决于「等上游首个内容」的空窗里塞得下几个心跳。空窗通常只有几秒，节奏偏慢时
	// 客户端只会看到开流首帧——这是配置看起来「没生效」的最常见原因。
	if continuousRetryKeepaliveInterval <= 0 {
		log.Printf("[Config] 注意：DOWNSTREAM_HTTP_KEEPALIVE_INTERVAL=0 已关闭下游保活，伪装思考没有心跳载体，不会下发假思考帧")
		return
	}
	rhythm, source := streamFakeThinkingInterval, "STREAM_FAKE_THINKING_INTERVAL"
	if rhythm <= 0 {
		rhythm, source = continuousRetryKeepaliveInterval, "跟随 DOWNSTREAM_HTTP_KEEPALIVE_INTERVAL"
	}
	log.Printf("[Config] 按序文案节奏=%s（%s），上游首个内容到达即停止注入，空窗内最多发出「空窗时长 ÷ 节奏」条", rhythm, source)
	if rhythm > 5*time.Second {
		log.Printf("[Config] 注意：节奏 %s 偏慢，而空窗通常只有几秒，很可能只发得出开流首帧。"+
			"CPA 侧对应项 streaming.keepalive-seconds 线上用 1s，需要文案尽快出现请调小 STREAM_FAKE_THINKING_INTERVAL", rhythm)
	}
}

// boolFromEnv 与 decodeEnvEscapes 见 env_config.go（与上游错误改写共用同一套语义）。

// fakeThinkingState 在一次流式请求内由 handler 与保活写入方共享。
// handler 在首个真实内容落地时置位 firstContentSeen，保活写入方据此决定继续
// 发假思考帧还是退回纯注释心跳。
type fakeThinkingState struct {
	mu               sync.Mutex
	protocol         string
	model            string
	firstText        string
	texts            []string
	index            int
	preempted        bool
	firstContentSeen bool
	primeImmediately bool
	// interval 是假思考期望的心跳节奏，只在「等上游首个内容」的空窗里生效；
	// 上游一旦开始产出（firstContentSeen）就回落全局保活间隔。0 表示跟随全局。
	interval time.Duration
}

// newFakeThinkingStateForChat 在总开关打开时为 chat 协议构造共享状态，否则返回 nil。
// 首帧文案留空时回落到内置默认文案，与 CPA buildEarlyThinkingChunk 的兜底一致，
// 保证「抢先开流」永远能发出一帧假思考。
func newFakeThinkingStateForChat(responseModel string) *fakeThinkingState {
	if !streamFakeThinkingEnabled {
		return nil
	}
	firstText := streamFakeThinkingText
	if strings.TrimSpace(firstText) == "" {
		firstText = defaultFakeThinkingText
	}
	return &fakeThinkingState{
		protocol:         fakeThinkingProtocolChat,
		model:            strings.TrimSpace(responseModel),
		firstText:        firstText,
		texts:            streamFakeThinkingTexts,
		primeImmediately: streamFakeThinkingImmediate,
		interval:         streamFakeThinkingInterval,
	}
}

// intervalOverride 返回假思考期望的心跳间隔；不在注入阶段（非 chat 协议或上游已开始
// 产出）时返回 0，表示沿用全局保活间隔——所以只有「等上游首个内容」的空窗会被调密。
func (s *fakeThinkingState) intervalOverride() time.Duration {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.protocol != fakeThinkingProtocolChat || s.firstContentSeen {
		return 0
	}
	return s.interval
}

// markFirstContentSeen 标记上游首个真实内容已到达，此后不再注入假思考帧。
func (s *fakeThinkingState) markFirstContentSeen() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.firstContentSeen = true
	s.mu.Unlock()
}

// shouldPrimeFirstBeat 报告首个心跳是否应当立即落地（抢先开流）。
func (s *fakeThinkingState) shouldPrimeFirstBeat() bool {
	return s != nil && s.primeImmediately
}

// payload 返回本次心跳要写出的完整 SSE 载荷。
// 首次心跳发开流假思考帧；后续心跳先发标准注释再按序附加一条假思考帧；
// 列表耗尽、文案为空、或真实内容已到达时退回纯注释心跳。
func (s *fakeThinkingState) payload() string {
	if s == nil {
		return continuousRetryKeepaliveComment
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.protocol != fakeThinkingProtocolChat || s.firstContentSeen {
		return continuousRetryKeepaliveComment
	}
	if !s.preempted {
		s.preempted = true
		if frame := buildFakeThinkingFrame(s.model, s.firstText); frame != "" {
			return frame
		}
		return continuousRetryKeepaliveComment
	}
	if s.index >= len(s.texts) {
		return continuousRetryKeepaliveComment
	}
	text := s.texts[s.index]
	s.index++
	if strings.TrimSpace(text) == "" {
		return continuousRetryKeepaliveComment
	}
	frame := buildFakeThinkingFrame(s.model, text)
	if frame == "" {
		return continuousRetryKeepaliveComment
	}
	return continuousRetryKeepaliveComment + frame
}

type fakeThinkingChunk struct {
	ID      string               `json:"id"`
	Object  string               `json:"object"`
	Created int64                `json:"created"`
	Model   string               `json:"model"`
	Choices []fakeThinkingChoice `json:"choices"`
}

type fakeThinkingChoice struct {
	Index        int               `json:"index"`
	Delta        fakeThinkingDelta `json:"delta"`
	FinishReason *string           `json:"finish_reason"`
}

type fakeThinkingDelta struct {
	Role             string `json:"role"`
	ReasoningContent string `json:"reasoning_content"`
}

// buildFakeThinkingFrame 构造一个只有 reasoning_content、没有 content 的
// chat.completion.chunk 数据帧（含 `data: ` 前缀与空行结尾）。
// 文案按原样写入，不做裁剪——线上文案开头的 `\n` 必须保留；纯空白视为空文案。
func buildFakeThinkingFrame(model, text string) string {
	if strings.TrimSpace(text) == "" {
		return ""
	}
	chunk := fakeThinkingChunk{
		ID:      fmt.Sprintf("chatcmpl-thinking-%d", time.Now().UnixNano()),
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   strings.TrimSpace(model),
		Choices: []fakeThinkingChoice{{
			Index:        0,
			Delta:        fakeThinkingDelta{Role: "assistant", ReasoningContent: text},
			FinishReason: nil,
		}},
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		log.Printf("[Stream] 构造假思考帧失败: %v", err)
		return ""
	}
	return fmt.Sprintf("data: %s\n\n", string(raw))
}

// installStreamFakeThinkingKeepalive 安装带假思考载荷的下游保活。
// state 为 nil（伪装思考关闭）时退回原有的纯注释心跳，行为不变。
// onWrite 在每次心跳写出后调用（可为 nil），供请求卡顿看门狗判定下游已有字节——
// 伪装思考关闭时也必须接上，否则长等待期间的心跳不会被看门狗看见。
func installStreamFakeThinkingKeepalive(c *gin.Context, stream bool, state *fakeThinkingState, onWrite func()) func() {
	if !stream {
		return installContinuousRetrySSEKeepalive(c, stream, "text/event-stream")
	}
	options := continuousRetrySSEKeepaliveOptions{
		contentType: "text/event-stream",
		payload:     continuousRetryKeepaliveComment,
		onWrite:     onWrite,
	}
	if state != nil {
		options.payloadFunc = state.payload
		options.primeFirstBeat = state.shouldPrimeFirstBeat()
		// 空窗内按假思考自己的节奏推进（默认 1s，对齐 CPA 的 keepalive-seconds）；
		// 上游开始产出后 intervalOverride 返回 0，自动回落全局保活间隔。
		options.intervalFunc = state.intervalOverride
	}
	return installContinuousRetrySSEKeepaliveWithOptions(c, stream, options)
}
