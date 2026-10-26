package promptfilter

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// 熔断器拒绝时复核**没有发生**：没有请求、没有延迟、没有上游参与。
//
// 之前 ApplyReviewOutcome 无条件置 Reviewed=true，于是复核历史里会出现一条
// latency=0 的记录，看着像审过了，其实是整段窗口都没审；同时它还被计成一次
// 「复核失败」，把一次全局跳过算到某条具体请求头上。
func TestCircuitUnavailableIsNotCountedAsReviewed(t *testing.T) {
	cfg := NormalizeReviewConfig(ReviewConfig{
		Enabled: true, Model: "review-model", APIKey: "test-key",
		BaseURL: "http://127.0.0.1:1/v1", TimeoutSeconds: 3, FailClosed: false,
	})

	cases := []struct {
		name string
		err  error
	}{
		{"熔断器开着", &reviewCircuitUnavailableError{retryAfter: 21 * time.Second}},
		{"恢复探针进行中", &reviewCircuitUnavailableError{probe: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ApplyReviewOutcome(Verdict{Action: ActionAllow}, ReviewOutcome{}, tc.err, cfg)
			if got.Reviewed {
				t.Fatal("熔断跳过被标成已复核")
			}
			if got.Action != ActionAllow {
				t.Fatalf("fail-open 下应当放行，得到 %s", got.Action)
			}
			if !strings.Contains(got.Reason, "skipped") {
				t.Fatalf("原因应当写明是跳过而不是失败：%q", got.Reason)
			}
			if !strings.Contains(got.Reason, "circuit open") {
				t.Fatalf("原因应当点出熔断：%q", got.Reason)
			}
			if !IsReviewCircuitUnavailable(tc.err) {
				t.Fatal("IsReviewCircuitUnavailable 没认出来")
			}
		})
	}

	// 真正的复核失败仍然是「已复核」——它确实发出去了、也确实失败了。
	realFailure := errors.New("review request failed with status 500")
	got := ApplyReviewOutcome(Verdict{Action: ActionAllow}, ReviewOutcome{}, realFailure, cfg)
	if !got.Reviewed {
		t.Fatal("复核失败应当仍是已复核")
	}
	if strings.Contains(got.Reason, "skipped") {
		t.Fatalf("复核失败被写成了跳过：%q", got.Reason)
	}
	if IsReviewCircuitUnavailable(realFailure) {
		t.Fatal("普通错误被误判成熔断跳过")
	}
}

// 熔断器连续失败达到阈值后应当拒绝，并且用类型化错误把「没发出去」讲清楚。
func TestCircuitBreakerRejectsWithTypedError(t *testing.T) {
	cfg := NormalizeReviewAdapterConfig(ReviewAdapterConfig{CircuitBreakerFailures: 3, CircuitBreakerSeconds: 30})
	endpoint, model := "http://127.0.0.1:1/v1/chat/completions", "circuit-unit-model"
	key := reviewCircuitKey(endpoint, model)
	reviewCircuitBreakers.Delete(key)
	t.Cleanup(func() { reviewCircuitBreakers.Delete(key) })

	for i := 0; i < 3; i++ {
		lease, err := acquireReviewCircuit(endpoint, model, cfg)
		if err != nil {
			t.Fatalf("第 %d 次不该被拒: %v", i+1, err)
		}
		completeReviewCircuit(lease, errors.New("upstream 500"))
	}

	_, err := acquireReviewCircuit(endpoint, model, cfg)
	if !IsReviewCircuitUnavailable(err) {
		t.Fatalf("熔断应当拒绝，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "retry after") {
		t.Fatalf("拒绝信息应当带重试时间：%q", err.Error())
	}

	value, ok := reviewCircuitBreakers.Load(key)
	if !ok {
		t.Fatal("熔断状态没存下来")
	}
	state := value.(*reviewCircuitBreaker)

	// 恢复窗口到期：第一个请求当探针，其余请求在探针期间必须被拒。
	state.mu.Lock()
	state.openUntil = time.Now().Add(-time.Second)
	state.mu.Unlock()
	probeLease, err := acquireReviewCircuit(endpoint, model, cfg)
	if err != nil {
		t.Fatalf("恢复窗口到期后应当放一个探针: %v", err)
	}
	if !probeLease.probe {
		t.Fatal("这个 lease 应当被标记为探针")
	}
	if _, err := acquireReviewCircuit(endpoint, model, cfg); !IsReviewCircuitUnavailable(err) ||
		!strings.Contains(err.Error(), "half-open") {
		t.Fatalf("探针进行中应当拒绝并说明 half-open，得到 %v", err)
	}

	// 探针成功 → 熔断关闭，后续请求不再是探针。
	completeReviewCircuit(probeLease, nil)
	next, err := acquireReviewCircuit(endpoint, model, cfg)
	if err != nil {
		t.Fatalf("熔断关闭后应当放行: %v", err)
	}
	if next.probe {
		t.Fatal("熔断关闭后不该再当探针")
	}
}
