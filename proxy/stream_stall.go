package proxy

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// 上游流停顿看门狗（移植 CPA 补丁 cliproxy-thinking-mask 的 `streaming.stall-timeout`）。
//
// # 为什么需要
//
// codex2api 原有的两道关卡都不管「流已经开始、随后彻底不说话」这段：
//
//   - 首字看门狗（firstTokenTimeoutGuard）：首个非生命周期事件一到就 Stop()，之后完全撒手；
//   - 流预算（streamLimitState）：判据是**内容字符 / 上游原始字节 / 墙钟**，而且都只在
//     **上游事件到达时**才判定（observe 由读流回调驱动）。上游一旦静默，就没有事件，
//     连 max-stream-duration 这条墙钟上限也不会触发——它根本没有自己的定时器。
//
// 于是出现 CPA 早就修掉的洞：上游发完 response.created（或吐了几个 delta）后半开挂死，
// 下游一个字节都等不到，只能干等客户端自己超时；运维事后从用量日志里看到的是
// 「duration 55s 被流预算掐掉」这种**掐晚了**的终态，而不是「上游挂了」。
//
// CPA 侧对应实现见 _cpa-overlay/sdk/api/handlers/stream_forwarder.go 的 `<-stallC` 分支：
// 每个上游 chunk 重置计时器，超时即写终态错误并 cancel 上游。语义是「**空闲**」而不是
// 「总时长」，与 max-stream-duration（绝对墙钟）互补：一个抓挂死，一个抓慢速失控。
//
// # 与 CPA 的差异（有意为之）
//
//  1. 起算点是**首个上游事件**（包含 response.created / response.in_progress 握手帧），
//     所以「握手后挂死」也在覆盖内；首事件之前的等待仍归 first_token_timeout_seconds 管
//     （注意该项线上当前为 0，即关闭）。
//  2. 计时器由**上游事件**重置，而不是「下游写完一帧」重置：下游写得慢不会伪装成上游
//     有进展。代价是下游单帧写出耗时超过本超时时，也会被判为停顿（见 configure 日志）。
//  3. 只看不改协议：超时只 cancel 上游上下文，终态文案与错误码仍由 handler 既有出口决定
//     （零写入时按真实错误码返回 JSON，已写入正文时补流内断流事件），不新增写路径。
const (
	// defaultStreamStallTimeout 默认关闭。CPA 侧 streaming.stall-timeout 默认同样是关的：
	// 「多长的空窗算异常」与模型、思考档位、上游线路强相关，必须由运维按线路标定，
	// 内置一个默认值会替所有租户做决定。
	defaultStreamStallTimeout = time.Duration(0)

	// streamStallLooseThreshold 超过该值时启动日志提醒：空窗放得太大，停顿看门狗就只剩
	// 「彻底挂死」这一种命中场景，慢速失控那一类仍然要靠内容预算去掐。
	streamStallLooseThreshold = 30 * time.Second
)

// 变量形式与 STREAM_LIMITS_* / 伪装思考一致：包初始化读进程环境变量，
// config.Load 读取 .env 后由 ConfigureFromEnv 再刷新一次。
var streamStallTimeout = streamStallTimeoutFromEnv()

func streamStallTimeoutFromEnv() time.Duration {
	return durationFromEnv("STREAM_STALL_TIMEOUT", defaultStreamStallTimeout)
}

// ConfigureStreamStallFromEnv 在 config.Load 读取 .env 后刷新停顿看门狗配置。
func ConfigureStreamStallFromEnv() {
	streamStallTimeout = streamStallTimeoutFromEnv()
	logStreamStallConfig()
}

// logStreamStallConfig 打印停顿看门狗的实际生效配置。
//
// 这一项最容易配错的地方是**语义**：它管的是「空闲」不是「总耗时」。运维想治「总耗时太长」
// 时很容易把它当成第二个 max-stream-duration 来用，结果发现匀速慢吐的流一条都没被掐——
// 所以启动时把语义和两个可调位置（全局 / 规则）都打出来。
func logStreamStallConfig() {
	if streamStallTimeout <= 0 {
		return
	}
	log.Printf("[Config] 上游流停顿看门狗已启用（全局 STREAM_STALL_TIMEOUT=%s）：首个上游事件起算，"+
		"超过该时长没再收到任何上游事件即中止该流（终态 stream_stall，零写入时会换号重试）。"+
		"它管的是**空闲**不是**总时长**——匀速慢吐的失控流不会命中，那一段仍由 max-stream-duration / 内容预算兜底",
		streamStallTimeout)
	if streamStallTimeout >= streamStallLooseThreshold {
		log.Printf("[Config] 注意：%s 偏大，停顿看门狗基本只在「上游彻底挂死」时生效；"+
			"要抓慢慢吐的失控请调小它，或收紧 STREAM_LIMITS_RULES 的内容预算", streamStallTimeout)
	}
	log.Printf("[Config] 停顿看门狗按流生效：下游单帧写出耗时超过 %s 时也会被判为停顿（读流回调与下游写出在同一循环里）。"+
		"若某条线路有长思考请求，请用规则内的 stall-timeout 精确圈定，而不是调大全局值", streamStallTimeout)
}

// streamStallTimeoutForRequest 解析本次请求生效的停顿超时。
//
// 规则内的 stall-timeout 优先（第一条命中的规则为准，能精确圈到某条线路的 key+模型），
// 未命中或未配置时回落到全局 STREAM_STALL_TIMEOUT。这样「翻译线开 12s、其余租户不开」
// 不需要把全局值调到所有流量都受影响——长思考请求（codex CLI high/xhigh、Claude thinking）
// 首次产出前的空窗天然可能超过 10s 量级。
func streamStallTimeoutForRequest(apiKey, model string) time.Duration {
	if streamLimitsEnabled {
		for _, rule := range streamLimitRules {
			if !streamLimitMatchAny(rule.APIKeys, strings.TrimSpace(apiKey)) {
				continue
			}
			if !streamLimitMatchModels(rule.Models, strings.TrimSpace(model)) {
				continue
			}
			if timeout := parseStreamStallTimeout(rule.StallTimeout); timeout > 0 {
				return timeout
			}
			// 第一条命中的规则没有配 stall-timeout：按规则「第一条命中即生效」的既有语义
			// 不再往下看，直接回落全局值。
			break
		}
	}
	return streamStallTimeout
}

// parseStreamStallTimeout 解析规则里的 stall-timeout（Go duration 字面量）。
// 空值/非法/非正值一律视为未配置（0），不覆盖全局。
func parseStreamStallTimeout(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout <= 0 {
		return 0
	}
	return timeout
}

// streamStallGuard 是单个上游尝试（attempt）的停顿看门狗。
//
// 生命周期：newStreamStallGuard（每次 attempt 一个，绑定该 attempt 的 upstreamCancel）
// → 首个上游事件 MarkEvent() 起算 → 之后每个事件重置 → 超时 fire 时 cancel 上游上下文
// → 该 attempt 收尾时 Stop()。
//
// 与 firstTokenTimeoutGuard 的关系是接力而不是重叠：首字看门狗在首个事件就 Stop，
// 停顿看门狗在首个事件才起算，两者拼起来正好覆盖「请求入口 → 首事件 → 流结束」全程
// （前提是 first_token_timeout_seconds 也配了）。
type streamStallGuard struct {
	timeout time.Duration
	cancel  context.CancelFunc

	mu      sync.Mutex
	timer   *time.Timer
	fired   bool
	stopped bool
}

// newStreamStallGuard 构造看门狗；timeout <= 0 或缺少 cancel 时返回 nil（零开销）。
func newStreamStallGuard(timeout time.Duration, cancel context.CancelFunc) *streamStallGuard {
	if timeout <= 0 || cancel == nil {
		return nil
	}
	return &streamStallGuard{timeout: timeout, cancel: cancel}
}

// MarkEvent 记录一次上游事件：首个事件激活看门狗，此后每个事件把空闲计时重置。
// 任何上游事件（含握手帧、推理帧、心跳帧）都证明「上游还活着」，所以不做事件类型过滤。
func (g *streamStallGuard) MarkEvent() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped || g.fired {
		return
	}
	if g.timer == nil {
		g.timer = time.AfterFunc(g.timeout, g.fire)
		return
	}
	g.timer.Reset(g.timeout)
}

func (g *streamStallGuard) fire() {
	g.mu.Lock()
	if g.stopped || g.fired {
		g.mu.Unlock()
		return
	}
	g.fired = true
	timeout := g.timeout
	g.mu.Unlock()
	log.Printf("[Stream] 上游流停顿超时：首个事件后 %s 未再收到上游事件，中止上游读取"+
		"（该 attempt 按 stream_stall 收尾；零写入时交回重试链换号）", timeout)
	g.cancel()
}

// Fired 报告看门狗是否已经因停顿触发。
func (g *streamStallGuard) Fired() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fired
}

// Timeout 返回本次生效的停顿超时，供终态文案与日志使用。
func (g *streamStallGuard) Timeout() time.Duration {
	if g == nil {
		return 0
	}
	return g.timeout
}

// Stop 结束看门狗。attempt 收尾时必须调用，否则残留定时器会在下一次 attempt 里
// cancel 掉上一轮的上游上下文（幂等，可重复调用）。
func (g *streamStallGuard) Stop() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = true
	if g.timer != nil {
		g.timer.Stop()
	}
}

// streamStallOutcome 是停顿中止的终态。
//
// 与首字超时（firstTokenTimeoutOutcome）保持同一套语义：598 内部状态码（对外 502 或
// 流内断流事件）＋ penalize，让重试链按上游故障处理——零写入时可以换号重试，
// 已经写出正文时由既有出口补流内断流事件。区别只在 failureKind 与文案，便于用量页
// 一眼区分「上游挂了」与「上游输出失控」。
func streamStallOutcome(timeout time.Duration) streamOutcome {
	return streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "stream_stall",
		failureMessage: fmt.Sprintf("上游流停顿超时：首个事件后 %s 未再收到上游事件，已中止", timeout.Round(time.Millisecond)),
		penalize:       true,
	}
}

// isStreamStallOutcome 报告终态是否由停顿看门狗判定。非流式出口需要它来把
// 「半截正文」从 200 改成真实错误（见 handler 的非流式写出分支）。
func isStreamStallOutcome(outcome streamOutcome) bool {
	return strings.EqualFold(strings.TrimSpace(outcome.failureKind), "stream_stall")
}
