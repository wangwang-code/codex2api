package proxy

import (
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// 上游错误消息改写（迁移自 CPA 的 error-rewrite 补丁）。
//
// 目的：上游 429/502/503 等错误默认会把上游原始 error body、上游身份（如
// "via openai"、请求 id）透给客户端；这里在「执行错误转成客户端响应」之前，
// 按配置的状态码把 message 换成自定义文案，并且不再转发上游原始 body。
//
// 与 CPA 的对应关系：
//   - CPA `error-rewrite.enabled`           -> UPSTREAM_ERROR_REWRITE_ENABLED
//   - CPA `error-rewrite.default-message`   -> UPSTREAM_ERROR_REWRITE_DEFAULT_MESSAGE
//   - CPA `error-rewrite.status-messages`   -> UPSTREAM_ERROR_REWRITE_STATUS_MESSAGES
//
// 与 CPA 的差异（有意为之）：
//   - 保留网关自有的 `type` / `code` 字段，只替换 `message`。这两个字段由网关常量
//     拼装、不含上游身份，下游常靠 `code` 做重试判定，丢掉会破坏兼容性。
//   - Responses WebSocket 路径继续由已有的 `CodexWSHideErrors` 运行时开关负责
//     （默认开启、固定文案），本改写不与之叠加。

// 配置变量形式与保活/伪装思考一致：包初始化时读进程环境变量，
// config.Load 读取 .env 后再刷新一次。
var (
	upstreamErrorRewriteEnabled     = upstreamErrorRewriteEnabledFromEnv()
	upstreamErrorRewriteDefault     = upstreamErrorRewriteDefaultFromEnv()
	upstreamErrorRewriteStatusTexts = upstreamErrorRewriteStatusTextsFromEnv()
)

func upstreamErrorRewriteEnabledFromEnv() bool {
	return boolFromEnv("UPSTREAM_ERROR_REWRITE_ENABLED", false)
}

func upstreamErrorRewriteDefaultFromEnv() string {
	return strings.TrimSpace(decodeEnvEscapes(os.Getenv("UPSTREAM_ERROR_REWRITE_DEFAULT_MESSAGE")))
}

// upstreamErrorRewriteStatusTextsFromEnv 读取状态码 -> 文案的映射。
// 支持两种写法：
//   - JSON 对象：{"429":"[云翻译]被上游限流，请稍微再试","502":"..."}
//   - `|` 分隔的 `状态码=文案`：429=[云翻译]被上游限流，请稍微再试|502=[云翻译]上游服务暂时不可用
//
// `|` 写法逐项还原转义；文案本身含 `|` 时请改用 JSON 写法。
func upstreamErrorRewriteStatusTextsFromEnv() map[int]string {
	return parseUpstreamErrorRewriteStatusTexts(os.Getenv("UPSTREAM_ERROR_REWRITE_STATUS_MESSAGES"))
}

func parseUpstreamErrorRewriteStatusTexts(raw string) map[int]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "{") {
		decoded := map[string]string{}
		if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
			log.Printf("[Config] UPSTREAM_ERROR_REWRITE_STATUS_MESSAGES 不是合法 JSON 对象，已忽略: %v", err)
			return nil
		}
		out := make(map[int]string, len(decoded))
		for key, value := range decoded {
			status, err := strconv.Atoi(strings.TrimSpace(key))
			if err != nil {
				log.Printf("[Config] UPSTREAM_ERROR_REWRITE_STATUS_MESSAGES 状态码 %q 非法，已跳过", key)
				continue
			}
			if message := strings.TrimSpace(decodeEnvEscapes(value)); message != "" {
				out[status] = message
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	out := map[int]string{}
	for _, entry := range strings.Split(raw, "|") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		sep := strings.Index(entry, "=")
		if sep <= 0 {
			log.Printf("[Config] UPSTREAM_ERROR_REWRITE_STATUS_MESSAGES 条目 %q 缺少 `=`, 已跳过", entry)
			continue
		}
		status, err := strconv.Atoi(strings.TrimSpace(entry[:sep]))
		if err != nil {
			log.Printf("[Config] UPSTREAM_ERROR_REWRITE_STATUS_MESSAGES 状态码 %q 非法，已跳过", entry[:sep])
			continue
		}
		message := strings.TrimSpace(decodeEnvEscapes(entry[sep+1:]))
		if message == "" {
			continue
		}
		out[status] = message
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ConfigureUpstreamErrorRewriteFromEnv 在 config.Load 读取 .env 后刷新改写配置。
func ConfigureUpstreamErrorRewriteFromEnv() {
	upstreamErrorRewriteEnabled = upstreamErrorRewriteEnabledFromEnv()
	upstreamErrorRewriteDefault = upstreamErrorRewriteDefaultFromEnv()
	upstreamErrorRewriteStatusTexts = upstreamErrorRewriteStatusTextsFromEnv()
}

// upstreamErrorRewriteMessage 返回 status 对应的改写文案。
// 未启用、状态码非法、或既无专属条目又无默认文案时返回 ok=false（即原样透出）。
func upstreamErrorRewriteMessage(status int) (string, bool) {
	if !upstreamErrorRewriteEnabled {
		return "", false
	}
	if status > 0 && upstreamErrorRewriteStatusTexts != nil {
		if message := upstreamErrorRewriteStatusTexts[status]; message != "" {
			return message, true
		}
	}
	if upstreamErrorRewriteDefault != "" {
		return upstreamErrorRewriteDefault, true
	}
	return "", false
}

// rewriteUpstreamErrorText 命中配置时返回改写文案，否则原样返回 message。
func rewriteUpstreamErrorText(status int, message string) string {
	if rewritten, ok := upstreamErrorRewriteMessage(status); ok {
		return rewritten
	}
	return message
}

// upstreamErrorRewrittenHeader 标记本次响应里的错误文案是配置话术、不是上游原话。
const upstreamErrorRewrittenHeader = "X-Upstream-Error-Rewritten"

// markRewrittenClientError 在响应头标注「这条错误文案是按配置改写的」。
// 供没有 streamOutcome 的调用点使用（如 /v1/messages 路径）。
func markRewrittenClientError(c *gin.Context, status int) {
	if c == nil {
		return
	}
	if _, ok := upstreamErrorRewriteMessage(status); ok {
		c.Header(upstreamErrorRewrittenHeader, "1")
	}
}

// clientFailureText 返回发给下游的失败文案，以及它是否被改写。
//
// failureMessage 与它的分工是这次修复的重点：failureMessage 永远保留真实原因，
// 用量日志、诊断日志和过载熔断都依赖它；对外文案在这里单独派生。之前是把
// rewriteUpstreamErrorText 的返回值写回 failureMessage 本身，于是日志跟着被改写，
// 线上表现是：下游取消（499）、网关合成的断流（598）、流预算中止、首字超时
// 全部记成「[云翻译]上游服务暂时不可用」——用量页再也分不清到底是哪一种，
// 而过载熔断靠 error_message 里找 server_is_overloaded，字面量被盖掉后永不触发。
//
// 客户端侧行为不变：仍然是命中配置就给配置文案，别让上游的 error body 漏出去。
//
// 改写过的文案必须能被认出来。下游拿到「[云翻译]上游服务暂时不可用」时无从判断
// 这是上游真实原因、还是网关为了不漏上游身份而换掉的说法——两者的处置完全不同：
// 前者该退避重试，后者可能只是本地预算中止或下游自己取消。所以调用方要把这个
// bool 显式标给下游（响应头 + error.rewritten），而不是让固定话术冒充上游原话。
func (o streamOutcome) clientFailureText() (string, bool) {
	if rewritten, ok := upstreamErrorRewriteMessage(o.logStatusCode); ok {
		return rewritten, true
	}
	return o.failureMessage, false
}

// upstreamClientErrorMessage 把上游错误体转成可直接发给客户端的文案，并套用改写。
//
// 与 usageLogErrorMessage 的分工：后者服务于用量日志，运维需要看到真实上游原因
// （issue #524），因此不能被改写；本函数专供客户端响应出口使用，命中配置时只暴露
// 配置文案。改写未命中或未启用时两者结果完全一致。
func upstreamClientErrorMessage(status int, body []byte) string {
	return rewriteUpstreamErrorText(status, usageLogErrorMessage(status, body))
}
