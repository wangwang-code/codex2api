import assert from "node:assert/strict";
import test from "node:test";

import {
  activeWindowDraftFromAccount,
  buildActiveWindowPayload,
  clockToMinutes,
  formatActiveWindowLabel,
  isOutsideActiveWindow,
  isOvernightWindow,
  isValidClock,
  validateActiveWindowDraft,
} from "./accountActiveWindow.ts";

test("isValidClock 只接受 24 小时制 HH:MM", () => {
  for (const value of ["00:00", "09:30", "23:59", " 09:05 "]) {
    assert.equal(isValidClock(value), true, `${value} 应当合法`);
  }
  for (const value of ["", "24:00", "23:60", "9:00", "09", "abc", "09:5"]) {
    assert.equal(isValidClock(value), false, `${value} 应当非法`);
  }
});

test("clockToMinutes 换算当天分钟数", () => {
  assert.equal(clockToMinutes("00:00"), 0);
  assert.equal(clockToMinutes("09:30"), 570);
  assert.equal(clockToMinutes("23:59"), 1439);
  assert.equal(clockToMinutes("nope"), -1);
});

test("isOvernightWindow 判定跨午夜", () => {
  assert.equal(isOvernightWindow("22:00", "06:00"), true);
  assert.equal(isOvernightWindow("09:00", "18:00"), false);
  // 起止相同不是跨午夜（本身就是非法配置，由校验拦掉）。
  assert.equal(isOvernightWindow("09:00", "09:00"), false);
  assert.equal(isOvernightWindow("bad", "18:00"), false);
});

test("activeWindowDraftFromAccount 还原草稿", () => {
  assert.deepEqual(
    activeWindowDraftFromAccount({
      active_window_start: "09:00",
      active_window_end: "18:00",
    }),
    { enabled: true, start: "09:00", end: "18:00" },
  );

  // 未配置 → 关闭，并使用默认时刻。
  assert.deepEqual(activeWindowDraftFromAccount({}), {
    enabled: false,
    start: "09:00",
    end: "18:00",
  });

  // 半配置 / 非法值 → 也按未启用处理，避免把脏数据带进表单。
  assert.equal(
    activeWindowDraftFromAccount({ active_window_start: "09:00" }).enabled,
    false,
  );
  assert.equal(
    activeWindowDraftFromAccount({
      active_window_start: "bad",
      active_window_end: "18:00",
    }).enabled,
    false,
  );
});

test("validateActiveWindowDraft 拦掉非法草稿", () => {
  assert.equal(
    validateActiveWindowDraft({ enabled: false, start: "", end: "" }),
    null,
  );
  assert.equal(
    validateActiveWindowDraft({ enabled: true, start: "09:00", end: "18:00" }),
    null,
  );
  // 跨午夜是合法配置。
  assert.equal(
    validateActiveWindowDraft({ enabled: true, start: "22:00", end: "06:00" }),
    null,
  );
  assert.equal(
    validateActiveWindowDraft({ enabled: true, start: "9:00", end: "18:00" }),
    "invalid_start",
  );
  assert.equal(
    validateActiveWindowDraft({ enabled: true, start: "09:00", end: "25:00" }),
    "invalid_end",
  );
  assert.equal(
    validateActiveWindowDraft({ enabled: true, start: "09:00", end: "09:00" }),
    "same_clock",
  );
});

test("buildActiveWindowPayload 关闭窗口时提交双空串", () => {
  assert.deepEqual(
    buildActiveWindowPayload({ enabled: true, start: "09:00", end: "18:00" }),
    { active_window_start: "09:00", active_window_end: "18:00" },
  );
  // 只提交一端会被后端拒绝，所以关闭时必须两端都发空串。
  assert.deepEqual(
    buildActiveWindowPayload({ enabled: false, start: "09:00", end: "18:00" }),
    { active_window_start: "", active_window_end: "" },
  );
});

test("isOutsideActiveWindow 只认服务端结论", () => {
  assert.equal(isOutsideActiveWindow({ in_active_window: false }), true);
  assert.equal(isOutsideActiveWindow({ in_active_window: true }), false);
  // 未配置窗口时服务端不返回该字段，不能推断成「窗口外」。
  assert.equal(isOutsideActiveWindow({}), false);
  assert.equal(isOutsideActiveWindow({ in_active_window: undefined }), false);
});

test("formatActiveWindowLabel 标注跨午夜", () => {
  assert.equal(formatActiveWindowLabel("09:00", "18:00"), "09:00-18:00");
  assert.equal(
    formatActiveWindowLabel("22:00", "06:00"),
    "22:00-06:00（次日）",
  );
  assert.equal(formatActiveWindowLabel("bad", "06:00"), "");
});
