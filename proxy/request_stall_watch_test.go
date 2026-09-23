package proxy

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// withStallWatchpoints 临时缩短看门狗检查点，测试结束后恢复。
func withStallWatchpoints(t *testing.T, points ...time.Duration) {
	t.Helper()
	previous := requestStallWatchpoints
	requestStallWatchpoints = points
	t.Cleanup(func() { requestStallWatchpoints = previous })
}

// captureLog 捕获默认 logger 的输出，返回读取函数。
func captureLog(t *testing.T) func() string {
	t.Helper()
	var mu sync.Mutex
	var buffer bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&writerFunc{mu: &mu, buffer: &buffer})
	t.Cleanup(func() { log.SetOutput(previous) })
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return buffer.String()
	}
}

type writerFunc struct {
	mu     *sync.Mutex
	buffer *bytes.Buffer
}

func (w *writerFunc) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffer.Write(p)
}

// TestRequestPhaseTrackerArmedWindow 验证看门狗只在「流式请求 + 下游一个字节都没有」
// 时布防。
//
// 关键点：布防边界是「是否已有下游字节」而不是「是否已过某个阶段」——保活安装之后到
// 第一次心跳之间（代理解析、亲和绑定、账号选择）同样不写下游，用阶段判断会漏掉这段。
func TestRequestPhaseTrackerArmedWindow(t *testing.T) {
	tracker := newRequestPhaseTracker()
	if tracker.armed() {
		t.Fatal("未标记是否流式前不应布防")
	}
	tracker.setStream(false)
	if tracker.armed() {
		t.Fatal("非流式请求本来就要等完整响应，不参与布防")
	}
	tracker.setStream(true)
	if !tracker.armed() {
		t.Fatal("流式请求在没有任何下游字节时应当布防")
	}
	for _, phase := range []string{phaseValidate, phaseTranslate, phaseQuota, phaseAccountFilter, phaseWaitingUpstream} {
		tracker.set(phase)
		if !tracker.armed() {
			t.Fatalf("阶段 %q 时仍未有下游字节，应当继续布防", phase)
		}
	}
	tracker.markOutput()
	if tracker.armed() {
		t.Fatal("下游已有字节后不应再布防，否则几十秒的正常长流会被误报")
	}
	tracker.setModel("gpt-5.6-codex")
	if got := tracker.currentModel(); got != "gpt-5.6-codex" {
		t.Fatalf("model = %q", got)
	}
}

// TestRequestStallWatchdogLogsCurrentPhase 验证请求卡在零字节状态时会打出阶段快照。
func TestRequestStallWatchdogLogsCurrentPhase(t *testing.T) {
	withStallWatchpoints(t, 10*time.Millisecond)
	readLog := captureLog(t)

	tracker := newRequestPhaseTracker()
	tracker.setStream(true)
	tracker.set(phaseQuota)
	tracker.setModel("gpt-5.6-codex")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watchRequestStall(ctx, "/v1/chat/completions", tracker, time.Now())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(readLog(), "[STALL]") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	output := readLog()
	if !strings.Contains(output, "[STALL]") {
		t.Fatalf("零字节状态应当打出 STALL 快照，实际日志: %q", output)
	}
	if !strings.Contains(output, phaseQuota) || !strings.Contains(output, "gpt-5.6-codex") {
		t.Fatalf("STALL 快照应带阶段名与模型名，实际: %q", output)
	}
}

// TestRequestStallWatchdogSilentAfterOutput 验证下游一旦有字节（抢先假思考首帧或心跳）
// 看门狗就收工，否则长流会被当成卡顿刷日志。
func TestRequestStallWatchdogSilentAfterOutput(t *testing.T) {
	withStallWatchpoints(t, 10*time.Millisecond)
	readLog := captureLog(t)

	tracker := newRequestPhaseTracker()
	tracker.setStream(true)
	tracker.markOutput()
	tracker.set(phaseWaitingUpstream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watchRequestStall(ctx, "/v1/chat/completions", tracker, time.Now())

	time.Sleep(150 * time.Millisecond)
	if output := readLog(); strings.Contains(output, "[STALL]") {
		t.Fatalf("已有下游字节后不应打 STALL: %q", output)
	}
}

// TestRequestStallWatchdogSilentForNonStream 验证非流式请求不参与布防：
// 它本来就要等完整响应，长时间没有字节是正常的。
func TestRequestStallWatchdogSilentForNonStream(t *testing.T) {
	withStallWatchpoints(t, 10*time.Millisecond)
	readLog := captureLog(t)

	tracker := newRequestPhaseTracker()
	tracker.setStream(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watchRequestStall(ctx, "/v1/chat/completions", tracker, time.Now())

	time.Sleep(150 * time.Millisecond)
	if output := readLog(); strings.Contains(output, "[STALL]") {
		t.Fatalf("非流式请求不应打 STALL: %q", output)
	}
}

// TestRequestStallWatchdogStopsOnContextCancel 验证请求结束后看门狗退出（不泄漏 goroutine）。
func TestRequestStallWatchdogStopsOnContextCancel(t *testing.T) {
	withStallWatchpoints(t, 50*time.Millisecond)
	readLog := captureLog(t)

	tracker := newRequestPhaseTracker()
	tracker.setStream(true)
	ctx, cancel := context.WithCancel(context.Background())
	watchRequestStall(ctx, "/v1/chat/completions", tracker, time.Now())
	cancel()

	time.Sleep(200 * time.Millisecond)
	if output := readLog(); strings.Contains(output, "[STALL]") {
		t.Fatalf("请求已结束，不应再打 STALL: %q", output)
	}
}
