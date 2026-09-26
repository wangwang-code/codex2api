package admin

import (
	"encoding/json"
	"testing"
)

// TestParseAccountSchedulerUpdateActiveWindow 验证生效时间窗口的解析与校验。
//
// 关键约定：两端必须同时提供；同时留空 = 清除窗口（回到全天可用）；跨午夜合法
// （start > end）；起止相同非法（账号会一天都没有生效时刻）。
func TestParseAccountSchedulerUpdateActiveWindow(t *testing.T) {
	t.Run("两端齐全", func(t *testing.T) {
		update, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{
			ActiveWindowStart: json.RawMessage(`"09:00"`),
			ActiveWindowEnd:   json.RawMessage(`"18:00"`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !update.ActiveWindowStart.Set || update.ActiveWindowStart.Value != "09:00" {
			t.Fatalf("ActiveWindowStart = %#v", update.ActiveWindowStart)
		}
		if got := update.CredentialUpdates["active_window_start"]; got != "09:00" {
			t.Fatalf("credentialUpdates[active_window_start] = %#v", got)
		}
		if got := update.CredentialUpdates["active_window_end"]; got != "18:00" {
			t.Fatalf("credentialUpdates[active_window_end] = %#v", got)
		}
		if !update.hasChanges() {
			t.Fatal("时间窗口必须算作一次变更")
		}
	})

	t.Run("跨午夜合法", func(t *testing.T) {
		if _, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{
			ActiveWindowStart: json.RawMessage(`"22:00"`),
			ActiveWindowEnd:   json.RawMessage(`"06:00"`),
		}); err != nil {
			t.Fatalf("22:00-06:00 应当合法: %v", err)
		}
	})

	t.Run("两端留空清除窗口", func(t *testing.T) {
		update, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{
			ActiveWindowStart: json.RawMessage(`""`),
			ActiveWindowEnd:   json.RawMessage(`""`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := update.CredentialUpdates["active_window_start"]; got != "" {
			t.Fatalf("清除窗口应写入空串，实际 %#v", got)
		}
		if got := update.CredentialUpdates["active_window_end"]; got != "" {
			t.Fatalf("清除窗口应写入空串，实际 %#v", got)
		}
	})

	t.Run("只给一端被拒绝", func(t *testing.T) {
		if _, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{
			ActiveWindowStart: json.RawMessage(`"09:00"`),
		}); err == nil {
			t.Fatal("只给起始时刻必须报错")
		}
		if _, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{
			ActiveWindowEnd: json.RawMessage(`"18:00"`),
		}); err == nil {
			t.Fatal("只给结束时刻必须报错")
		}
		if _, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{
			ActiveWindowStart: json.RawMessage(`"09:00"`),
			ActiveWindowEnd:   json.RawMessage(`""`),
		}); err == nil {
			t.Fatal("一端为空必须报错（半配置语义含糊）")
		}
	})

	t.Run("非补零写法被接受", func(t *testing.T) {
		// "9:00" 与 "09:00" 语义相同（都归一到 540 分钟），后端宽容接受；
		// 前端 <input type="time"> 始终产出补零格式，所以这里只是 API 友好性。
		update, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{
			ActiveWindowStart: json.RawMessage(`"9:00"`),
			ActiveWindowEnd:   json.RawMessage(`"18:00"`),
		})
		if err != nil {
			t.Fatalf("非补零写法应当被接受: %v", err)
		}
		if got := update.CredentialUpdates["active_window_start"]; got != "9:00" {
			t.Fatalf("credentialUpdates[active_window_start] = %#v", got)
		}
	})

	t.Run("非法写法与等值被拒绝", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			start, end string
		}{
			{name: "小时越界", start: "24:00", end: "18:00"},
			{name: "分钟越界", start: "09:00", end: "09:60"},
			{name: "缺冒号", start: "0900", end: "18:00"},
			{name: "非数字", start: "上午", end: "18:00"},
			{name: "起止相同", start: "09:00", end: "09:00"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{
					ActiveWindowStart: json.RawMessage(`"` + tc.start + `"`),
					ActiveWindowEnd:   json.RawMessage(`"` + tc.end + `"`),
				})
				if err == nil {
					t.Fatalf("start=%q end=%q 必须报错", tc.start, tc.end)
				}
			})
		}
	})
}
