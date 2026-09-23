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

// 预工作阶段的名称。走完这些阶段后保活就生效了，客户端会开始收到字节，看门狗随即收工。
const (
	phaseReadBody      = "read_body"
	phaseValidate      = "validate"
	phaseTranslate     = "translate"
	phaseQuota         = "quota_admission"
	phaseAccountFilter = "account_filter"
	// phaseWaitingUpstream 是保活安装并激活之后：此后下游至少会收到抢先假思考首帧或
	// 心跳，不再是「完全没有字节」的场景，看门狗停止。
	phaseWaitingUpstream = "waiting_upstream"
)

type requestPhaseTracker struct {
	phase atomic.Value // string
	model atomic.Value // string
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

// armed 报告请求是否还处在「下游完全没有字节」的预工作窗口内。
func (t *requestPhaseTracker) armed() bool {
	switch t.current() {
	case phaseReadBody, phaseValidate, phaseTranslate, phaseQuota, phaseAccountFilter:
		return true
	}
	return false
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
				"该阶段在保活生效之前，所以既没有心跳也没有抢先假思考，属预期表现而非故障——"+
				"要定位的是这一阶段为何没有返回",
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
