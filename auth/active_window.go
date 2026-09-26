package auth

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
)

// 账号生效时间窗口（24 小时制，每天重复）。
//
// 语义：
//   - 未配置窗口 → 全天可用（默认，向后兼容）。
//   - 配置窗口后，不在窗口内的账号被「隔离」：不参与任何选号路径（普通调度、懒加载
//     待探测、Spark 策略、用量限制续链），但**不改变账号自身的状态**（不是冷却、不是
//     禁用、不是封禁）——窗口外只是「还没到点」，时间一到自动恢复，无需人工干预。
//   - 时区取 time.Local，由 .env 的 TZ 决定（config.applyTimezone）。
//   - 区间为左闭右开 [start, end)：配置 09:00-18:00 时，18:00 整已不在窗口内。
//   - start > end 表示跨午夜，例如 22:00-06:00 覆盖 22:00 到次日 05:59。
//
// 存储：credentials 里的 active_window_start / active_window_end，格式 "HH:MM"。
// 两者必须同时存在才视为启用窗口；只配一个按未配置处理（fail-open，避免误隔离）。
const (
	// ActiveWindowUnset 表示账号未配置生效时间窗口（全天可用）。
	ActiveWindowUnset = -1

	// credentialActiveWindowStart / credentialActiveWindowEnd 是 credentials 里的键名。
	credentialActiveWindowStart = "active_window_start"
	credentialActiveWindowEnd   = "active_window_end"
)

// ActiveWindowStartCredentialKey / ActiveWindowEndCredentialKey 是凭据键名的导出版本，
// 供管理端写入与测试引用。
const (
	ActiveWindowStartCredentialKey = credentialActiveWindowStart
	ActiveWindowEndCredentialKey   = credentialActiveWindowEnd
)

// ParseActiveWindowMinute 解析 "HH:MM"（24 小时制）为当天分钟数。
// 空值、格式错误或越界都返回 ok=false。
func ParseActiveWindowMinute(value string) (int, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, false
	}
	hourText, minuteText, found := strings.Cut(trimmed, ":")
	if !found {
		return 0, false
	}
	hour, err := strconv.Atoi(strings.TrimSpace(hourText))
	if err != nil || hour < 0 || hour > 23 {
		return 0, false
	}
	minute, err := strconv.Atoi(strings.TrimSpace(minuteText))
	if err != nil || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

// FormatActiveWindowMinute 把当天分钟数格式化为 "HH:MM"；越界返回空串。
func FormatActiveWindowMinute(minute int) string {
	if minute < 0 || minute > 1439 {
		return ""
	}
	return fmt.Sprintf("%02d:%02d", minute/60, minute%60)
}

// ActiveWindowMinuteInRange 报告分钟数是否是合法的当天时刻。
func ActiveWindowMinuteInRange(minute int) bool {
	return minute >= 0 && minute <= 1439
}

// InActiveWindowAt 报告给定窗口在 now 时刻是否生效，供不经由 Account 的调用方
// （例如管理端列表直接读 credentials 投影）复用同一套判定。
//
// startMinute / endMinute 传 ActiveWindowUnset 表示未配置 → 恒为 true。
// start == end 视为无效配置 → 恒为 true（fail-open，避免配出「永不生效」的账号）。
func InActiveWindowAt(startMinute, endMinute int, now time.Time) bool {
	if !ActiveWindowMinuteInRange(startMinute) || !ActiveWindowMinuteInRange(endMinute) {
		return true
	}
	if startMinute == endMinute {
		return true
	}
	minute := now.Hour()*60 + now.Minute()
	if startMinute < endMinute {
		return minute >= startMinute && minute < endMinute
	}
	// 跨午夜：窗口跨越 00:00，例如 22:00-06:00。
	return minute >= startMinute || minute < endMinute
}

// ActiveWindow 返回账号的生效时间窗口（当天分钟数）。未配置时 ok=false。
func (a *Account) ActiveWindow() (startMinute, endMinute int, ok bool) {
	if a == nil {
		return 0, 0, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.activeWindowSet {
		return 0, 0, false
	}
	return a.activeWindowStart, a.activeWindowEnd, true
}

// SetActiveWindow 设置账号的生效时间窗口（当天分钟数）。
// 任一值越界、或 start == end 时清除窗口（回到全天可用），避免留下「永不生效」的配置。
func (a *Account) SetActiveWindow(startMinute, endMinute int) {
	if a == nil {
		return
	}
	valid := ActiveWindowMinuteInRange(startMinute) &&
		ActiveWindowMinuteInRange(endMinute) &&
		startMinute != endMinute
	a.mu.Lock()
	a.activeWindowSet = valid
	a.activeWindowStart = startMinute
	a.activeWindowEnd = endMinute
	a.mu.Unlock()
}

// InActiveWindow 报告 now 是否落在账号的生效时间窗口内；未配置窗口时恒为 true。
func (a *Account) InActiveWindow(now time.Time) bool {
	if a == nil {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.inActiveWindowLocked(now)
}

// inActiveWindowLocked 与 InActiveWindow 相同，但要求调用方已持有 a.mu。
func (a *Account) inActiveWindowLocked(now time.Time) bool {
	if !a.activeWindowSet {
		return true
	}
	return InActiveWindowAt(a.activeWindowStart, a.activeWindowEnd, now)
}

// loadActiveWindowFromCredentials 从账号行的 credentials 装载时间窗口。
// 只配了一个端点时按未配置处理（fail-open）。
func (a *Account) loadActiveWindowFromCredentials(row *database.AccountRow) {
	if a == nil || row == nil {
		return
	}
	startMinute, hasStart := ParseActiveWindowMinute(row.GetCredential(credentialActiveWindowStart))
	endMinute, hasEnd := ParseActiveWindowMinute(row.GetCredential(credentialActiveWindowEnd))
	if !hasStart || !hasEnd {
		return
	}
	a.SetActiveWindow(startMinute, endMinute)
}

// InActiveWindowForRow 用账号行的 credentials 判断 now 是否落在生效时间窗口内。
// 未配置窗口（或只配了一个端点）时返回 true。
//
// 供不经由 Account 的调用方复用与调度一致的判定——管理端列表直接读投影行，
// 若让前端按浏览器时区自行计算，展示结果会与调度判定不一致。
func InActiveWindowForRow(row *database.AccountRow, now time.Time) bool {
	if row == nil {
		return true
	}
	startMinute, hasStart := ParseActiveWindowMinute(row.GetCredential(credentialActiveWindowStart))
	endMinute, hasEnd := ParseActiveWindowMinute(row.GetCredential(credentialActiveWindowEnd))
	if !hasStart || !hasEnd {
		return true
	}
	return InActiveWindowAt(startMinute, endMinute, now)
}

// ActiveWindowFromRow 返回账号行配置的时间窗口原文（"HH:MM"）；未配置时返回空串。
func ActiveWindowFromRow(row *database.AccountRow) (start, end string) {
	if row == nil {
		return "", ""
	}
	startMinute, hasStart := ParseActiveWindowMinute(row.GetCredential(credentialActiveWindowStart))
	endMinute, hasEnd := ParseActiveWindowMinute(row.GetCredential(credentialActiveWindowEnd))
	if !hasStart || !hasEnd {
		return "", ""
	}
	return FormatActiveWindowMinute(startMinute), FormatActiveWindowMinute(endMinute)
}
