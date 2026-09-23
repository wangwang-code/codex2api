package proxy

import (
	"context"
	"log"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// 请求阶段跟踪 + 卡顿看门狗。
//
// # 为什么需要
//
// 保活（含抢先假思考首帧、SSE 心跳）只在「等待循环」里才会写下游：等上游响应头、
// 读上游流、等账号（调度心跳）、重试退避。也就是说**保活安装之前的那段预工作期间，
// 下游一个字节都不会有**——读请求体、模型映射与校验、请求翻译、额度准入、账号过滤链
// （含多次 DB 读）。实测出现过「客户端连接一直活着、但完全没有 SSE」的请求，就是卡在
// 这个窗口里，而 `/v1/chat/completions` 此前没有任何分段耗时日志，事后无法判断卡在哪。
//
// 这里在每个可能阻塞的边界记一个阶段名，并在请求迟迟没有任何下游产出时把当前阶段
// 打进日志。正常运行只做几次原子写，没有额外开销。

// requestStallWatchpoints 是看门狗的检查点：请求超过这些时长仍没有任何下游产出时打印快照。
// 取 10s 起是为了和默认首字超时量级对齐——正常请求不会在这里被误报。
var requestStallWatchpoints = []time.Duration{10 * time.Second, 30 * time.Second, 60 * time.Second}

// 预工作阶段的名称，仅用于日志定位。
const (
	phaseReadBody      = "read_body"
	phaseValidate      = "validate"
	phaseTranslate     = "translate"
	phaseQuota         = "quota_admission"
	phaseAccountFilter = "account_filter"
	// phaseWaitingUpstream 是保活安装并激活之后。注意它**不等于**下游已有字节：
	// 之后还有代理解析、亲和绑定、账号选择等步骤同样不写下游，真正的分界是第一个
	// 下游字节写出（见 markOutput）。
	phaseWaitingUpstream = "waiting_upstream"
)

type requestPhaseTracker struct {
	phase  atomic.Value // string
	model  atomic.Value // string
	stream atomic.Bool  // 是否流式请求：非流式没有 SSE 保活，不参与布防
	output atomic.Bool  // 下游是否已经写出过字节
}

func newRequestPhaseTracker() *requestPhaseTracker {
	tracker := &requestPhaseTracker{}
	tracker.set(phaseReadBody)
	return tracker
}

func (t *requestPhaseTracker) set(phase string) {
	if t == nil {
		return
	}
	t.phase.Store(phase)
}

func (t *requestPhaseTracker) setModel(model string) {
	if t == nil {
		return
	}
	t.model.Store(model)
}

// setStream 标记是否流式请求。只有流式请求才可能「连接活着但零 SSE」——非流式本来
// 就要等完整响应，长时间没有字节是正常的，不参与布防。
func (t *requestPhaseTracker) setStream(stream bool) {
	if t == nil {
		return
	}
	t.stream.Store(stream)
}

// markOutput 标记下游已经产出过字节。由保活写出与上游事件回调调用。
func (t *requestPhaseTracker) markOutput() {
	if t == nil {
		return
	}
	t.output.Store(true)
}

func (t *requestPhaseTracker) current() string {
	if t == nil {
		return "unknown"
	}
	if value := t.phase.Load(); value != nil {
		if phase, ok := value.(string); ok {
			return phase
		}
	}
	return "unknown"
}

func (t *requestPhaseTracker) currentModel() string {
	if t == nil {
		return ""
	}
	if value := t.model.Load(); value != nil {
		if model, ok := value.(string); ok {
			return model
		}
	}
	return ""
}

// armed 报告请求是否还处在「流式请求、但下游一个字节都没有」的状态。
//
// 刻意用「是否已有下游字节」而不是「是否已过某个阶段」来判断：保活安装之后到第一次
// 心跳之间（代理解析、亲和绑定、账号选择）同样不写下游，用阶段判断会漏掉这段。
func (t *requestPhaseTracker) armed() bool {
	if t == nil {
		return false
	}
	return t.stream.Load() && !t.output.Load()
}

// watchRequestStall 在请求迟迟没有任何下游产出时把当前阶段打进日志，请求结束即退出。
//
// 刻意不去读 c.Writer.Written()：那会和写入方产生数据竞争（仓库 CI 跑 -race）。
// 改用阶段名判断——进入 waiting_upstream 之后保活必然已经写出首帧，看门狗收工。
func watchRequestStall(ctx context.Context, endpoint string, tracker *requestPhaseTracker, startedAt time.Time) {
	if ctx == nil || tracker == nil || len(requestStallWatchpoints) == 0 {
		return
	}
	go func() {
		for _, after := range requestStallWatchpoints {
			delay := time.Until(startedAt.Add(after))
			if delay < 0 {
				delay = 0
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if !tracker.armed() {
				return
			}
			log.Printf("[STALL] %s 已 %.0fs 未产出任何下游字节，当前阶段=%s（model=%s）；"+
				"该请求是流式，正常应当在保活首次心跳（含抢先假思考首帧）时就写出字节，"+
				"所以这里要定位的是「当前阶段为何没有返回」",
				endpoint, time.Since(startedAt).Seconds(), tracker.current(), tracker.currentModel())
		}
	}()
}

// watchRequestStallFor 是给 handler 用的便捷入口：内部记录起始时间。
func watchRequestStallFor(c *gin.Context, endpoint string, tracker *requestPhaseTracker) {
	if c == nil || c.Request == nil {
		return
	}
	watchRequestStall(c.Request.Context(), endpoint, tracker, time.Now())
}
