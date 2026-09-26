import type { AccountRow } from "../types";

/**
 * 账号每日生效时间窗口（24 小时制）的编辑草稿。
 *
 * 与后端约定的语义：
 *   - 两端必须同时提供；同时留空 = 清除窗口，回到全天可用。
 *   - start > end 表示跨午夜（例如 22:00-06:00）。
 *   - 起止相同是非法配置（账号会一天都没有生效时刻），提交前拦掉。
 *   - 窗口是否生效、当前是否在窗口内，一律以**服务端**结论为准（in_active_window）：
 *     时区取 .env 的 TZ，前端按浏览器时区算会与调度判定不一致。
 */
export interface ActiveWindowDraft {
  enabled: boolean;
  /** "HH:MM"，24 小时制 */
  start: string;
  /** "HH:MM"，24 小时制 */
  end: string;
}

/** 关闭窗口时的默认时刻：给一个常见工作时段，方便直接打开开关。 */
export const DEFAULT_ACTIVE_WINDOW: ActiveWindowDraft = {
  enabled: false,
  start: "09:00",
  end: "18:00",
};

const CLOCK_PATTERN = /^([01]\d|2[0-3]):([0-5]\d)$/;

/** "HH:MM" → 当天分钟数；非法返回 -1。 */
export function clockToMinutes(value: string): number {
  if (!isValidClock(value)) return -1;
  const [hour, minute] = value.trim().split(":").map(Number);
  return hour * 60 + minute;
}

/** 校验 24 小时制 "HH:MM"。 */
export function isValidClock(value: string): boolean {
  return CLOCK_PATTERN.test(value.trim());
}

/** 是否跨午夜（结束时刻早于起始时刻）。 */
export function isOvernightWindow(start: string, end: string): boolean {
  const startMinutes = clockToMinutes(start);
  const endMinutes = clockToMinutes(end);
  if (startMinutes < 0 || endMinutes < 0) return false;
  return endMinutes < startMinutes;
}

/** 从账号数据还原编辑草稿；未配置或半配置都按「未启用」处理。 */
export function activeWindowDraftFromAccount(
  account: Pick<AccountRow, "active_window_start" | "active_window_end">,
): ActiveWindowDraft {
  const start = (account.active_window_start ?? "").trim();
  const end = (account.active_window_end ?? "").trim();
  if (!isValidClock(start) || !isValidClock(end)) {
    return { ...DEFAULT_ACTIVE_WINDOW };
  }
  return { enabled: true, start, end };
}

/**
 * 草稿校验结果。返回错误码而不是文案，由调用方映射到 i18n 文案。
 */
export type ActiveWindowValidationError =
  | "invalid_start"
  | "invalid_end"
  | "same_clock"
  | null;

/** 校验草稿；返回错误码，通过时返回 null。 */
export function validateActiveWindowDraft(
  draft: ActiveWindowDraft,
): ActiveWindowValidationError {
  if (!draft.enabled) return null;
  if (!isValidClock(draft.start)) return "invalid_start";
  if (!isValidClock(draft.end)) return "invalid_end";
  if (draft.start.trim() === draft.end.trim()) return "same_clock";
  return null;
}

/**
 * 构造提交载荷。
 * 关闭窗口时两端都提交空串——后端据此清除窗口（只提交一端会被拒绝）。
 */
export function buildActiveWindowPayload(draft: ActiveWindowDraft): {
  active_window_start: string;
  active_window_end: string;
} {
  if (!draft.enabled) {
    return { active_window_start: "", active_window_end: "" };
  }
  return {
    active_window_start: draft.start.trim(),
    active_window_end: draft.end.trim(),
  };
}

/**
 * 是否显示「窗口外」标记。
 * 只认服务端结论：未配置窗口时服务端不返回该字段（undefined），不得自行推断为窗口外。
 */
export function isOutsideActiveWindow(
  account: Pick<AccountRow, "in_active_window">,
): boolean {
  return account.in_active_window === false;
}

/** 窗口的展示文案，例如 "09:00-18:00"；跨午夜时标注「次日」。 */
export function formatActiveWindowLabel(start: string, end: string): string {
  if (!isValidClock(start) || !isValidClock(end)) return "";
  const suffix = isOvernightWindow(start, end) ? "（次日）" : "";
  return `${start.trim()}-${end.trim()}${suffix}`;
}
