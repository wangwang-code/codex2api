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

// TestRequestPhaseTrackerArmedWindow 验证看门狗只在「下游完全没有字节」的预工作窗口内生效。
func TestRequestPhaseTrackerArmedWindow(t *testing.T) {
	tracker := newRequestPhaseTracker()
	if tracker.current() != phaseReadBody || !tracker.armed() {
		t.Fatalf("初始阶段 = %q armed=%t，应处于读体阶段且已布防", tracker.current(), tracker.armed())
	}
	for _, phase := range []string{phaseValidate, phaseTranslate, phaseQuota, phaseAccountFilter} {
		tracker.set(phase)
		if !tracker.armed() {
			t.Fatalf("阶段 %q 仍属预工作窗口，应当布防", phase)
		}
	}
	// 保活生效之后下游必然有字节（首帧或心跳），看门狗必须收工，否则长流会被误报。
	tracker.set(phaseWaitingUpstream)
	if tracker.armed() {
		t.Fatalf("阶段 %q 之后不应当布防", phaseWaitingUpstream)
	}
	tracker.setModel("gpt-5.6-codex")
	if got := tracker.currentModel(); got != "gpt-5.6-codex" {
		t.Fatalf("model = %q", got)
	}
}

// TestRequestStallWatchdogLogsPreOutputPhase 验证请求卡在预工作阶段时会打出阶段快照。
func TestRequestStallWatchdogLogsPreOutputPhase(t *testing.T) {
	withStallWatchpoints(t, 10*time.Millisecond)
	readLog := captureLog(t)

	tracker := newRequestPhaseTracker()
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
		t.Fatalf("卡在预工作阶段时应当打出 STALL 快照，实际日志: %q", output)
	}
	if !strings.Contains(output, phaseQuota) || !strings.Contains(output, "gpt-5.6-codex") {
		t.Fatalf("STALL 快照应带阶段名与模型名，实际: %q", output)
	}
}

// TestRequestStallWatchdogSilentAfterKeepaliveActive 验证保活生效后看门狗不再误报，
// 否则长流（几十秒的正常流式输出）会被当成卡顿刷日志。
func TestRequestStallWatchdogSilentAfterKeepaliveActive(t *testing.T) {
	withStallWatchpoints(t, 10*time.Millisecond)
	readLog := captureLog(t)

	tracker := newRequestPhaseTracker()
	tracker.set(phaseWaitingUpstream)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watchRequestStall(ctx, "/v1/chat/completions", tracker, time.Now())

	time.Sleep(150 * time.Millisecond)
	if output := readLog(); strings.Contains(output, "[STALL]") {
		t.Fatalf("保活生效后不应打 STALL: %q", output)
	}
}

// TestRequestStallWatchdogStopsOnContextCancel 验证请求结束后看门狗退出（不泄漏 goroutine）。
func TestRequestStallWatchdogStopsOnContextCancel(t *testing.T) {
	withStallWatchpoints(t, 50*time.Millisecond)
	readLog := captureLog(t)

	tracker := newRequestPhaseTracker()
	ctx, cancel := context.WithCancel(context.Background())
	watchRequestStall(ctx, "/v1/chat/completions", tracker, time.Now())
	cancel()

	time.Sleep(200 * time.Millisecond)
	if output := readLog(); strings.Contains(output, "[STALL]") {
		t.Fatalf("请求已结束，不应再打 STALL: %q", output)
	}
}
