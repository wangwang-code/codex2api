package auth

import (
	"context"
	"testing"
	"time"
)

// waitForDispatchProbe 在给定 store 上做一次调度等待，返回是否拿到账号与实际耗时。
func waitForDispatchProbe(s *Store, filter AccountFilter, timeout, ctxBudget time.Duration) (*Account, time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), ctxBudget)
	defer cancel()
	started := time.Now()
	account, _, _, _ := s.WaitForDispatchAvailable(ctx, "", timeout, 1, nil, filter, false, DispatchPolicyStandard)
	return account, time.Since(started)
}

// TestWaitForDispatchAvailableFailsFastWithoutStaticCandidate 验证请求在池里没有归属
// （过滤器拒绝全部账号）时立即返回，而不是等满调度超时。indexed 引擎原本会一直等到
// 超时，客户端因此被白挂 30 秒。
func TestWaitForDispatchAvailableFailsFastWithoutStaticCandidate(t *testing.T) {
	for _, engine := range []string{"indexed", "legacy"} {
		t.Run(engine, func(t *testing.T) {
			s := newSchedulerWaitTestStore(t, 2)
			s.SetSchedulerEngine(engine)
			rejectAll := func(*Account) bool { return false }

			account, elapsed := waitForDispatchProbe(s, rejectAll, 5*time.Second, 5*time.Second)
			if account != nil {
				t.Fatalf("rejected pool must not grant an account, got %d", account.DBID)
			}
			if elapsed > time.Second {
				t.Fatalf("无归属请求等待了 %s，应当立即返回", elapsed)
			}
		})
	}
}

// TestWaitForDispatchAvailableFailsFastWithEmptyPool 验证池内一个账号都没有时立即返回。
func TestWaitForDispatchAvailableFailsFastWithEmptyPool(t *testing.T) {
	s := newSchedulerWaitTestStore(t, 0)
	account, elapsed := waitForDispatchProbe(s, nil, 5*time.Second, 5*time.Second)
	if account != nil {
		t.Fatalf("empty pool must not grant an account, got %d", account.DBID)
	}
	if elapsed > time.Second {
		t.Fatalf("空池请求等待了 %s，应当立即返回", elapsed)
	}
}

// TestWaitForDispatchAvailableStillWaitsForCoolingCandidate 验证「账号只是冷却」时
// 仍然按调度超时等待：快速失败只针对池里没有归属账号，不能把容量等待一起砍掉。
func TestWaitForDispatchAvailableStillWaitsForCoolingCandidate(t *testing.T) {
	s := newSchedulerWaitTestStore(t, 1)
	s.SetSchedulerEngine("indexed")
	s.MarkCooldown(s.accounts[0], time.Minute, "unit-test-cooldown")

	// 等待预算给到 400ms：冷却中的账号不可能被授予，因此必须等到预算耗尽。
	account, elapsed := waitForDispatchProbe(s, nil, 5*time.Second, 400*time.Millisecond)
	if account != nil {
		t.Fatalf("cooling account must not be granted, got %d", account.DBID)
	}
	if elapsed < 300*time.Millisecond {
		t.Fatalf("冷却中的账号应当继续等待，实际 %s 就返回了", elapsed)
	}
}
