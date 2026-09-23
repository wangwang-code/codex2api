package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
)

// 事件通知（最小可用版）：把两类运维关心的事件推到一个 webhook。
//
//   - 账号 401：告诉运维是哪个号被上游拒了；
//   - codex 池整体不可用：告诉运维服务已降级（流量切到中转兜底）。
//
// 通过 NOTIFY_WEBHOOK_URL 配置；不配则整个模块关闭（零开销、不发任何请求）。
// 发送一律异步 + 有超时，失败只记日志，绝不影响请求链路。

const (
	notifyEventAccountUnauthorized = "account_unauthorized"
	notifyEventCodexPoolDegraded   = "codex_pool_degraded"
	notifyEventCodexPoolRecovered  = "codex_pool_recovered"

	defaultNotifyTimeout = 5 * time.Second
)

var (
	notifyWebhookURL    = strings.TrimSpace(os.Getenv("NOTIFY_WEBHOOK_URL"))
	notifyWebhookFormat = notifyWebhookFormatFromEnv()
	notifyHTTPClient    = &http.Client{Timeout: defaultNotifyTimeout}
)

// notifyWebhookFormatFromEnv 读取 webhook 载荷格式，决定 body 形状：
//
//	json（默认）: {"text": "..."}
//	text       : 纯文本 body
//	wecom      : 企业微信群机器人 {"msgtype":"text","text":{"content":"..."}}
//	feishu     : 飞书自定义机器人 {"msg_type":"text","content":{"text":"..."}}
//	dingtalk   : 钉钉自定义机器人 {"msgtype":"text","text":{"content":"..."}}
func notifyWebhookFormatFromEnv() string {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv("NOTIFY_WEBHOOK_FORMAT")))
	switch raw {
	case "", "json":
		return "json"
	case "text", "wecom", "feishu", "dingtalk":
		return raw
	default:
		log.Printf("[Config] NOTIFY_WEBHOOK_FORMAT=%q 非法，沿用默认 json", raw)
		return "json"
	}
}

// ConfigureNotifyFromEnv 在 config.Load 读取 .env 后刷新通知配置。
func ConfigureNotifyFromEnv() {
	notifyWebhookURL = strings.TrimSpace(os.Getenv("NOTIFY_WEBHOOK_URL"))
	notifyWebhookFormat = notifyWebhookFormatFromEnv()
}

func notifyEnabled() bool {
	return notifyWebhookURL != ""
}

// notifyPayload 按配置的格式生成 webhook 请求体。
func notifyPayload(message string) []byte {
	var body []byte
	switch notifyWebhookFormat {
	case "text":
		return []byte(message)
	case "wecom", "dingtalk":
		body, _ = json.Marshal(map[string]any{"msgtype": "text", "text": map[string]string{"content": message}})
	case "feishu":
		body, _ = json.Marshal(map[string]any{"msg_type": "text", "content": map[string]string{"text": message}})
	default:
		body, _ = json.Marshal(map[string]string{"text": message})
	}
	return body
}

// sendNotification 异步投递一条通知。未配置 webhook 时直接返回。
func sendNotification(event, message string) {
	if !notifyEnabled() {
		return
	}
	url := notifyWebhookURL
	payload := notifyPayload(message)
	// 放到 goroutine 里，webhook 慢或挂都不会拖住请求。
	go func() {
		response, err := notifyHTTPClient.Post(url, "application/json", bytes.NewReader(payload))
		if err != nil {
			log.Printf("[Notify] %s 投递失败: %v", event, err)
			return
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			log.Printf("[Notify] %s 投递返回 %d", event, response.StatusCode)
			return
		}
		log.Printf("[Notify] %s 已投递", event)
	}()
}

// codexPoolNotifier 记录上一次上报的 codex 池状态，只在状态翻转时通知，
// 避免每个 401 都刷一条「服务降级」。
//
// 初值是「未降级」：判定是事件驱动的（401 或池空时触发），第一次调用本身就是真实事件，
// 不能当作启动误报吞掉——否则「最后一个号被打空」这一次恰好会被静默。
var codexPoolNotifier = struct {
	mu       sync.Mutex
	degraded bool
}{}

// isCodexPoolAccount 报告账号是否属于「codex 官方号池」：codex 渠道下、非中转、
// 非 Grok / Antigravity / Claude。中转（OpenAI Responses API）是兜底渠道，不算在内
// ——它的存在正是「降级」后的去处。
func isCodexPoolAccount(account *auth.Account) bool {
	if account == nil {
		return false
	}
	if account.IsGrokAPI() || account.IsAntigravityAPI() || account.IsClaudeOAuth() {
		return false
	}
	return !account.IsOpenAIResponsesAPI()
}

// refreshCodexPoolState 重新判定 codex 池是否还有可用账号，状态翻转时发通知。
// reason 只进日志，便于回查是哪次事件触发的判定。
func (h *Handler) refreshCodexPoolState(reason string) {
	if !notifyEnabled() || h == nil || h.store == nil {
		return
	}
	available := h.store.CountDispatchableAccounts(func(account *auth.Account) bool {
		return isCodexPoolAccount(account)
	})
	degraded := available == 0

	codexPoolNotifier.mu.Lock()
	previous := codexPoolNotifier.degraded
	codexPoolNotifier.degraded = degraded
	codexPoolNotifier.mu.Unlock()

	if degraded == previous {
		return
	}
	if degraded {
		log.Printf("[Notify] codex 池已打空（reason=%s），上报服务降级", reason)
		sendNotification(notifyEventCodexPoolDegraded, fmt.Sprintf(
			"[codex2api] 服务降级\n全部 codex 账号不可用，流量已切到中转兜底\n可用 codex 账号: 0\n触发: %s\n时间: %s",
			reason, time.Now().Format(time.RFC3339)))
		return
	}
	log.Printf("[Notify] codex 池恢复可用 %d 个（reason=%s）", available, reason)
	sendNotification(notifyEventCodexPoolRecovered, fmt.Sprintf(
		"[codex2api] 服务恢复\ncodex 账号重新可用\n可用 codex 账号: %d\n触发: %s\n时间: %s",
		available, reason, time.Now().Format(time.RFC3339)))
}

// notifyAccountUnauthorized 上报「某个号收到 401」。detail 是上游错误摘要，
// 会截断后带在消息里，便于直接看出是 token 过期还是被撤销。
func (h *Handler) notifyAccountUnauthorized(account *auth.Account, detail string) {
	if !notifyEnabled() || account == nil {
		return
	}
	detail = strings.TrimSpace(detail)
	if len(detail) > 300 {
		detail = detail[:300] + "…"
	}
	identity := fmt.Sprintf("id=%d", account.ID())
	if email := strings.TrimSpace(account.Email); email != "" {
		identity += " email=" + email
	}
	if plan := strings.TrimSpace(account.GetPlanType()); plan != "" {
		identity += " plan=" + plan
	}
	message := fmt.Sprintf("[codex2api] 账号 401\n账号: %s\n已退出调度\n时间: %s", identity, time.Now().Format(time.RFC3339))
	if detail != "" {
		message += "\n详情: " + detail
	}
	sendNotification(notifyEventAccountUnauthorized, message)
}
