package auth

import (
	"testing"
	"time"

	"github.com/codex2api/database"
)

// TestParseActiveWindowMinute 验证 "HH:MM" 解析：合法值、边界与各类非法输入。
func TestParseActiveWindowMinute(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		want  int
		valid bool
	}{
		{name: "midnight", raw: "00:00", want: 0, valid: true},
		{name: "nine-thirty", raw: "09:30", want: 570, valid: true},
		{name: "last-minute", raw: "23:59", want: 1439, valid: true},
		{name: "surrounding-space", raw: " 9:05 ", want: 545, valid: true},
		{name: "empty", raw: "", valid: false},
		{name: "blank", raw: "   ", valid: false},
		{name: "no-colon", raw: "9", valid: false},
		{name: "hour-out-of-range", raw: "24:00", valid: false},
		{name: "minute-out-of-range", raw: "23:60", valid: false},
		{name: "non-numeric", raw: "abc", valid: false},
		{name: "partial-non-numeric", raw: "09:xx", valid: false},
		{name: "negative-hour", raw: "-1:00", valid: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseActiveWindowMinute(tc.raw)
			if ok != tc.valid {
				t.Fatalf("ParseActiveWindowMinute(%q) ok = %v, want %v", tc.raw, ok, tc.valid)
			}
			if ok && got != tc.want {
				t.Fatalf("ParseActiveWindowMinute(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

// TestFormatActiveWindowMinute 验证分钟数回写为 "HH:MM"，越界返回空串。
func TestFormatActiveWindowMinute(t *testing.T) {
	for _, tc := range []struct {
		minute int
		want   string
	}{
		{minute: 0, want: "00:00"},
		{minute: 570, want: "09:30"},
		{minute: 1439, want: "23:59"},
		{minute: -1, want: ""},
		{minute: 1440, want: ""},
	} {
		if got := FormatActiveWindowMinute(tc.minute); got != tc.want {
			t.Fatalf("FormatActiveWindowMinute(%d) = %q, want %q", tc.minute, got, tc.want)
		}
	}
}

// TestInActiveWindowAt 验证窗口判定：左闭右开、跨午夜、未配置与无效配置。
func TestInActiveWindowAt(t *testing.T) {
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 9, 26, hour, minute, 0, 0, time.Local)
	}
	cases := []struct {
		name       string
		start, end int
		now        time.Time
		want       bool
	}{
		{name: "未配置-全天可用", start: ActiveWindowUnset, end: ActiveWindowUnset, now: at(3, 0), want: true},
		{name: "只配一半-按未配置处理", start: 540, end: ActiveWindowUnset, now: at(3, 0), want: true},
		{name: "start等于end-视为无效-全天可用", start: 540, end: 540, now: at(3, 0), want: true},

		{name: "普通区间-起点含", start: 540, end: 1080, now: at(9, 0), want: true},
		{name: "普通区间-终点不含", start: 540, end: 1080, now: at(18, 0), want: false},
		{name: "普通区间-区间内", start: 540, end: 1080, now: at(12, 30), want: true},
		{name: "普通区间-起点前", start: 540, end: 1080, now: at(8, 59), want: false},
		{name: "普通区间-终点前一分钟", start: 540, end: 1080, now: at(17, 59), want: true},

		{name: "跨午夜-起点含", start: 1320, end: 360, now: at(22, 0), want: true},
		{name: "跨午夜-次日凌晨", start: 1320, end: 360, now: at(2, 0), want: true},
		{name: "跨午夜-终点不含", start: 1320, end: 360, now: at(6, 0), want: false},
		{name: "跨午夜-白天不在窗口", start: 1320, end: 360, now: at(12, 0), want: false},
		{name: "跨午夜-午夜整点", start: 1320, end: 360, now: at(0, 0), want: true},

		{name: "全天窗口", start: 0, end: 1439, now: at(23, 58), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := InActiveWindowAt(tc.start, tc.end, tc.now); got != tc.want {
				t.Fatalf("InActiveWindowAt(%d, %d, %s) = %v, want %v",
					tc.start, tc.end, tc.now.Format("15:04"), got, tc.want)
			}
		})
	}
}

// TestSetActiveWindowClearsInvalidValues 验证越界或等值配置被清除（回到全天可用），
// 避免在管理端配出「永不生效」的账号。
func TestSetActiveWindowClearsInvalidValues(t *testing.T) {
	account := &Account{AccessToken: "token"}
	account.SetActiveWindow(540, 1080)
	if _, _, ok := account.ActiveWindow(); !ok {
		t.Fatal("合法窗口应当生效")
	}

	for _, tc := range []struct {
		name       string
		start, end int
	}{
		{name: "起点越界", start: 1440, end: 1080},
		{name: "终点越界", start: 540, end: -2},
		{name: "等值", start: 540, end: 540},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account.SetActiveWindow(tc.start, tc.end)
			if start, end, ok := account.ActiveWindow(); ok {
				t.Fatalf("非法配置应被清除，实际 start=%d end=%d", start, end)
			}
			if !account.InActiveWindow(time.Now()) {
				t.Fatal("清除窗口后应当全天可用")
			}
		})
	}
}

// TestActiveWindowIsolatesEverySelectionPath 是本功能的核心回归：窗口外的账号必须
// 在**所有**选号路径上被隔离。
//
// 四条路径各自独立判定，漏掉任何一条隔离就会被绕过：
//   - isAvailableLocked         普通调度（dispatchableForPolicy → IsAvailable）
//   - lazySelectableLocked      懒加载待探测模式（不走 isAvailableLocked）
//   - sparkDispatchEligibleLocked Spark 策略（dispatchableForPolicy 对 spark 走这里）
//   - usageLimitContinuationEligibleLocked 用量限制下的续链兜底
func TestActiveWindowIsolatesEverySelectionPath(t *testing.T) {
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 9, 26, hour, minute, 0, 0, time.Local)
	}
	outside := at(3, 0)
	inside := at(12, 0)

	account := &Account{AccessToken: "token", RefreshToken: "refresh"}
	account.SetActiveWindow(540, 1080) // 09:00-18:00

	account.mu.RLock()
	defer account.mu.RUnlock()

	if account.isAvailableLocked(outside) {
		t.Fatal("窗口外不应通过普通调度可用性判定")
	}
	if account.lazySelectableLocked(outside) {
		t.Fatal("窗口外不应通过懒加载可选性判定")
	}
	if account.sparkDispatchEligibleLocked(outside) {
		t.Fatal("窗口外不应通过 Spark 可用性判定")
	}
	if account.usageLimitContinuationEligibleLocked(outside) {
		t.Fatal("窗口外不应通过用量限制续链判定")
	}

	// 窗口内恢复正常（续链判定另有前置条件，不在此断言）。
	if !account.isAvailableLocked(inside) {
		t.Fatal("窗口内应通过普通调度可用性判定")
	}
	if !account.lazySelectableLocked(inside) {
		t.Fatal("窗口内应通过懒加载可选性判定")
	}
	if !account.sparkDispatchEligibleLocked(inside) {
		t.Fatal("窗口内应通过 Spark 可用性判定")
	}
}

// TestLoadActiveWindowFromCredentials 验证从 credentials 装载窗口，
// 以及只配一个端点时按未配置处理（fail-open）。
func TestLoadActiveWindowFromCredentials(t *testing.T) {
	newRow := func(credentials map[string]interface{}) *database.AccountRow {
		return &database.AccountRow{Credentials: credentials}
	}

	t.Run("两个端点齐全", func(t *testing.T) {
		account := &Account{}
		account.loadActiveWindowFromCredentials(newRow(map[string]interface{}{
			"active_window_start": "09:00",
			"active_window_end":   "18:00",
		}))
		start, end, ok := account.ActiveWindow()
		if !ok || start != 540 || end != 1080 {
			t.Fatalf("装载结果 start=%d end=%d ok=%v", start, end, ok)
		}
	})

	t.Run("跨午夜", func(t *testing.T) {
		account := &Account{}
		account.loadActiveWindowFromCredentials(newRow(map[string]interface{}{
			"active_window_start": "22:00",
			"active_window_end":   "06:00",
		}))
		if !account.InActiveWindow(time.Date(2026, 9, 26, 23, 30, 0, 0, time.Local)) {
			t.Fatal("22:00-06:00 应当覆盖 23:30")
		}
		if account.InActiveWindow(time.Date(2026, 9, 26, 12, 0, 0, 0, time.Local)) {
			t.Fatal("22:00-06:00 不应覆盖 12:00")
		}
	})

	t.Run("只配一个端点按未配置处理", func(t *testing.T) {
		for _, credentials := range []map[string]interface{}{
			{"active_window_start": "09:00"},
			{"active_window_end": "18:00"},
			{"active_window_start": "09:00", "active_window_end": ""},
			{"active_window_start": "bad", "active_window_end": "18:00"},
		} {
			account := &Account{}
			account.loadActiveWindowFromCredentials(newRow(credentials))
			if _, _, ok := account.ActiveWindow(); ok {
				t.Fatalf("credentials=%v 应按未配置处理", credentials)
			}
		}
	})

	t.Run("空凭据与nil行不 panic", func(t *testing.T) {
		account := &Account{}
		account.loadActiveWindowFromCredentials(newRow(map[string]interface{}{}))
		account.loadActiveWindowFromCredentials(nil)
		if _, _, ok := account.ActiveWindow(); ok {
			t.Fatal("没有窗口配置时不应生效")
		}
	})
}

// TestActiveWindowExcludesAccountFromPool 验证窗口隔离在真实 Store 上生效：
// 窗口外的账号既不算「可服务」，也不构成「结构性候选」——全部账号都在窗口外时，
// 池级判定为「没有归属」，请求会走快速失败而不是空等（与无账号场景同一路径）。
func TestActiveWindowExcludesAccountFromPool(t *testing.T) {
	store := newSchedulerWaitTestStore(t, 2)

	// 未配置窗口（默认）：两个账号都可服务。
	if got := store.CountServiceableAccounts(nil); got != 2 {
		t.Fatalf("未配置窗口时可服务账号 = %d, want 2", got)
	}

	// 把两个账号都设成「当前不在窗口内」：取当前分钟之后的 1 分钟窗口。
	now := time.Now()
	minute := now.Hour()*60 + now.Minute()
	start, end := (minute+2)%1440, (minute+3)%1440
	for _, account := range store.accounts {
		account.SetActiveWindow(start, end)
	}
	if store.accounts[0].InActiveWindow(time.Now()) {
		t.Skip("测试跨越了分钟边界，跳过（窗口与当前时刻重叠）")
	}

	if got := store.CountServiceableAccounts(nil); got != 0 {
		t.Fatalf("全部账号在窗口外时可服务账号 = %d, want 0", got)
	}
	if store.hasStaticCandidateWithDispatch(0, nil, nil, DispatchPolicyStandard) {
		t.Fatal("全部账号在窗口外时不应有结构性候选（应当快速失败而不是空等）")
	}

	// 放开一个账号（清除窗口）→ 立即恢复。
	store.accounts[0].SetActiveWindow(ActiveWindowUnset, ActiveWindowUnset)
	if got := store.CountServiceableAccounts(nil); got != 1 {
		t.Fatalf("放开一个账号后可服务账号 = %d, want 1", got)
	}
	if !store.hasStaticCandidateWithDispatch(0, nil, nil, DispatchPolicyStandard) {
		t.Fatal("放开一个账号后应当重新有结构性候选")
	}
}

// activeWindowOutsideNow 返回一个「保证不含当前时刻」的窗口（当前分钟之后 1 分钟）。
// 第二个返回值为 false 表示测试刚好跨越分钟边界，调用方应当跳过。
func activeWindowOutsideNow() (int, int, bool) {
	now := time.Now()
	minute := now.Hour()*60 + now.Minute()
	return (minute + 2) % 1440, (minute + 3) % 1440, true
}

// TestActiveWindowExcludesAccountFromFastScheduler 验证索引调度引擎不会选中窗口外账号。
//
// 这是线上现象的回归：fastSchedulerSnapshotWithUsageOverride 复刻了 isAvailableLocked 的
// 状态/档位/冷却/配额判定，但漏了窗口——而索引桶扫描与候选检查都拿它的 available 当最终
// 判据，于是窗口外账号照常被选中。实测现象是：优先级 P+97 的账号 A 处于窗口外，
// 第一次请求仍被 A 接走（在途 20s+ 后切到 B），第二次请求更是直接由 A 完成。
func TestActiveWindowExcludesAccountFromFastScheduler(t *testing.T) {
	start, end, ok := activeWindowOutsideNow()
	if !ok {
		t.Skip("测试跨越了分钟边界，跳过")
	}

	account := newFastSchedulerTestAccount(1, HealthTierHealthy, 90, 1)
	account.SetActiveWindow(start, end)
	if account.InActiveWindow(time.Now()) {
		t.Skip("测试跨越了分钟边界，跳过（窗口与当前时刻重叠）")
	}

	scheduler := NewFastScheduler(1, "round_robin")
	scheduler.Rebuild([]*Account{account})

	// 阴影检查不占槽位，只回答「有没有可用候选」。
	if scheduler.HasAvailableWithDispatch(0, nil, nil, DispatchPolicyStandard) {
		t.Fatal("窗口外账号不应被算作可用候选")
	}
	if got := scheduler.Acquire(); got != nil {
		t.Fatalf("窗口外账号不应被索引引擎选中，实际选中 account %d", got.DBID)
	}

	// 清除窗口 → 立即恢复。快照是实时计算的，不需要重建索引。
	account.SetActiveWindow(ActiveWindowUnset, ActiveWindowUnset)
	if !scheduler.HasAvailableWithDispatch(0, nil, nil, DispatchPolicyStandard) {
		t.Fatal("清除窗口后应当重新有可用候选")
	}
	if got := scheduler.Acquire(); got == nil {
		t.Fatal("清除窗口后应当能选中该账号")
	}
}

// TestActiveWindowExcludesAccountFromStoreNext 用真实 Store 的 Next() 覆盖同一条路径
// （Next → 索引引擎取号），也就是用户实际请求走的入口。
func TestActiveWindowExcludesAccountFromStoreNext(t *testing.T) {
	store := newSchedulerWaitTestStore(t, 2)

	start, end, _ := activeWindowOutsideNow()
	for _, account := range store.accounts {
		account.SetActiveWindow(start, end)
	}
	if store.accounts[0].InActiveWindow(time.Now()) {
		t.Skip("测试跨越了分钟边界，跳过（窗口与当前时刻重叠）")
	}

	if got := store.Next(); got != nil {
		store.Release(got)
		t.Fatalf("窗口外账号不应被 Next() 选中: account %d", got.DBID)
	}

	// 放开一个账号 → 立即可选。
	store.accounts[0].SetActiveWindow(ActiveWindowUnset, ActiveWindowUnset)
	got := store.Next()
	if got == nil {
		t.Fatal("清除窗口后 Next() 应当能取到账号")
	}
	store.Release(got)
}
