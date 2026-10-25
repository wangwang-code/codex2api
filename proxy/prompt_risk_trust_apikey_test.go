package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

// 降频按 API Key 生效：没有签名身份时，`shaft-{uid}` 那把密钥就是「这个人」。
func TestAdaptiveTrustBypassesByAPIKeyWithoutSignedIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var reviewCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reviewCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "review-model", "choices": []map[string]any{{"message": map[string]any{"content": `{"confidence":0.99,"reason":"high risk"}`}}},
		})
	}))
	defer server.Close()

	cfg := promptGuardTestConfig()
	cfg.Review = promptfilter.ReviewConfig{
		Enabled: true, APIKey: "test-key", BaseURL: server.URL, Model: "review-model", TimeoutSeconds: 2,
		Adapter: promptfilter.ReviewAdapterConfig{
			RequestMode: "chat_completions", SystemPrompt: "review", UserPromptTemplate: "<user_input>{{text}}</user_input>",
			ConfidenceThreshold: 0.7, MaxConcurrent: 4, MaxTextLength: 4096,
		},
	}
	cfg = promptfilter.NormalizeConfig(cfg)
	handler := newPromptGuardTestHandler(cfg)

	// 翻译流量没有 NewAPI 签名身份，只有 API Key。信任策略按密钥 id 存。
	handler.store.ReplacePromptRiskTrustPolicies([]*database.PromptRiskTrustPolicy{{
		ID: 11, SubjectType: database.PromptRiskSubjectAPIKey, SubjectKey: "42",
		Status: database.PromptRiskTrustStatusActive, ValidUntil: time.Now().UTC().Add(time.Hour), RiskThreshold: 35, LastRiskScore: 0,
	}})

	requestContext := func(apiKeyID int64) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		c.Set(contextAPIKeyID, apiKeyID)
		return c
	}
	envelope := promptfilter.RequestEnvelope{
		Endpoint: "/v1/chat/completions", Protocol: promptfilter.ProtocolChat, Transport: promptfilter.TransportHTTP,
		RequestedModel: "gpt-6-luna", ModelFamily: promptfilter.ModelFamilyOpenAI,
		Segments: []promptfilter.Segment{{Origin: promptfilter.OriginCurrentUser, Role: "user", Text: "请整理今天的会议纪要。", Trust: promptfilter.SegmentTrustClientSupplied}},
	}

	// 受信密钥：跳过同步复核。
	clean := handler.evaluatePromptGuardEnvelope(requestContext(42), cfg, envelope, false, "", "")
	if reviewCalls.Load() != 0 {
		t.Fatalf("受信密钥不该触发同步复核，实际调用 %d 次", reviewCalls.Load())
	}
	if clean.Decision.Action != promptfilter.ActionAllow || clean.Decision.ReasonCode != "adaptive_trust_review_bypass" {
		t.Fatalf("decision = %+v", clean.Decision)
	}

	// 未受信的密钥：照常复核。证明策略是按密钥精确命中，不是「有策略就放行」。
	reviewCalls.Store(0)
	handler.evaluatePromptGuardEnvelope(requestContext(43), cfg, envelope, false, "", "")
	if reviewCalls.Load() == 0 {
		t.Fatal("未受信的密钥必须照常复核")
	}
}

// 出口 IP 永远不参与降频：即使库里存在一条 client_ip 的信任策略，也不能放行。
func TestAdaptiveTrustIgnoresClientIPPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var reviewCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reviewCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "review-model", "choices": []map[string]any{{"message": map[string]any{"content": `{"confidence":0.99,"reason":"high risk"}`}}},
		})
	}))
	defer server.Close()

	cfg := promptGuardTestConfig()
	cfg.Review = promptfilter.ReviewConfig{
		Enabled: true, APIKey: "test-key", BaseURL: server.URL, Model: "review-model", TimeoutSeconds: 2,
		Adapter: promptfilter.ReviewAdapterConfig{
			RequestMode: "chat_completions", SystemPrompt: "review", UserPromptTemplate: "<user_input>{{text}}</user_input>",
			ConfidenceThreshold: 0.7, MaxConcurrent: 4, MaxTextLength: 4096,
		},
	}
	cfg = promptfilter.NormalizeConfig(cfg)
	handler := newPromptGuardTestHandler(cfg)

	// 人为塞一条 client_ip 策略，subject_key 用请求来源 IP 的 hash。
	// 写入层的门会拒绝，但这里直接灌进运行时缓存，验证「就算存在也不会被用」。
	handler.store.ReplacePromptRiskTrustPolicies([]*database.PromptRiskTrustPolicy{{
		ID: 13, SubjectType: database.PromptRiskSubjectClientIP, SubjectKey: "203.0.113.9",
		Status: database.PromptRiskTrustStatusActive, ValidUntil: time.Now().UTC().Add(time.Hour), RiskThreshold: 35, LastRiskScore: 0,
	}})

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	c.Set(contextAPIKeyID, int64(99))
	envelope := promptfilter.RequestEnvelope{
		Endpoint: "/v1/chat/completions", Protocol: promptfilter.ProtocolChat, Transport: promptfilter.TransportHTTP,
		RequestedModel: "gpt-6-luna", ModelFamily: promptfilter.ModelFamilyOpenAI,
		Segments: []promptfilter.Segment{{Origin: promptfilter.OriginCurrentUser, Role: "user", Text: "请整理今天的会议纪要。", Trust: promptfilter.SegmentTrustClientSupplied}},
	}

	handler.evaluatePromptGuardEnvelope(c, cfg, envelope, false, "", "")
	if reviewCalls.Load() == 0 {
		t.Fatal("只有 client_ip 策略时绝不能跳过复核")
	}
}
