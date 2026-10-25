package database

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// 降频的粒度必须是「一个人」。API Key（本部署一 uid 一把）和已签名身份满足，
// 出口 IP 不满足——它后面可能是几十个互不相关的人。
func TestPromptRiskTrustSubjectEligibility(t *testing.T) {
	cases := []struct {
		subjectType string
		want        bool
		why         string
	}{
		{PromptRiskSubjectAPIKey, true, "一把 shaft-{uid} 对应一个 uid"},
		{PromptRiskSubjectNewAPIUser, true, "签名身份，归属证据更强"},
		{PromptRiskSubjectClientIP, false, "同 IP 可能几十个人，误放代价最大"},
		{PromptRiskSubjectSession, false, "一次会话干净说明不了什么"},
		{PromptRiskSubjectUpstreamAccount, false, "上游账号是资源不是人"},
		{"", false, "空类型"},
		{"bogus", false, "未知类型"},
	}
	for _, tc := range cases {
		if got := PromptRiskTrustAdaptiveSubjectEligible(tc.subjectType); got != tc.want {
			t.Errorf("%s：%q 可参与降频 = %v，期望 %v", tc.why, tc.subjectType, got, tc.want)
		}
	}
}

// 策略写入层的门要跟判定一致：api_key 能建，client_ip 不能。
func TestNormalizePromptRiskTrustInputSubjectGate(t *testing.T) {
	base := PromptRiskTrustPolicyInput{
		Source: PromptRiskTrustSourceAutomatic, Reason: "test",
		RiskThreshold: 35, ValidUntil: time.Now().UTC().Add(time.Hour),
	}

	apiKeyInput := base
	apiKeyInput.SubjectType = PromptRiskSubjectAPIKey
	apiKeyInput.SubjectKey = "42"
	if _, err := normalizePromptRiskTrustInput(apiKeyInput); err != nil {
		t.Fatalf("api_key 应可建策略: %v", err)
	}

	personInput := base
	personInput.SubjectType = PromptRiskSubjectNewAPIUser
	personInput.SubjectKey = PromptRiskNewAPIUserSubjectKey("gateway-a", "user-1")
	if _, err := normalizePromptRiskTrustInput(personInput); err != nil {
		t.Fatalf("newapi_user 应可建策略: %v", err)
	}

	for _, subjectType := range []string{PromptRiskSubjectClientIP, PromptRiskSubjectSession, PromptRiskSubjectUpstreamAccount} {
		input := base
		input.SubjectType = subjectType
		input.SubjectKey = "whatever"
		if _, err := normalizePromptRiskTrustInput(input); err == nil {
			t.Fatalf("%s 不该允许建信任策略", subjectType)
		}
	}
}

// 自适应授予：干净的 API Key 会被授信；同一批流量产生的 client_ip 画像不会。
func TestAdaptiveTrustGrantsAPIKeyButNotClientIP(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	confidence := 0.1

	// 10 次干净复核：review_cleared 事件，api_key=42，来源 IP 固定。
	for i := 0; i < 10; i++ {
		input := &PromptFilterLogInput{
			Source: "local_filter", Endpoint: "/v1/chat/completions", Model: "gpt-6-luna",
			Action: "allow", ReviewModel: "codex-auto-review", ReviewConfidence: &confidence,
			ReviewReason: "正常翻译", APIKeyID: 42, APIKeyName: "shaft-777", ClientIP: "203.0.113.9",
			// 生产里这个 hash 由 proxy 算好传进来；不设的话根本不会有 client_ip 画像，
			// 「没被授信」就成了空断言。
			ClientIPHash: promptRiskHash("client-ip", "203.0.113.9"),
			RequestCorrelationID: fmt.Sprintf("corr-%d", i), TextPreview: fmt.Sprintf("text-%d", i),
		}
		if err := db.InsertPromptFilterLog(ctx, input); err != nil {
			t.Fatalf("InsertPromptFilterLog: %v", err)
		}
	}

	// 把事件时间往前挪，越过最小观察窗口（测试里不想等 24 小时）。
	if _, err := db.conn.ExecContext(ctx, `UPDATE prompt_risk_events SET created_at=?`, time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatalf("backdate events: %v", err)
	}

	policies, err := db.ReconcileAdaptivePromptRiskTrustPolicies(ctx, PromptRiskTrustAdaptiveOptions{
		MinCleanReviews: 3, MinObservationHours: 1,
	})
	if err != nil {
		t.Fatalf("ReconcileAdaptivePromptRiskTrustPolicies: %v", err)
	}

	var apiKeyPolicy *PromptRiskTrustPolicy
	for _, policy := range policies {
		if policy.SubjectType == PromptRiskSubjectClientIP {
			t.Fatalf("client_ip 不该被授予信任策略: %+v", policy)
		}
		if policy.SubjectType == PromptRiskSubjectAPIKey && policy.SubjectKey == "42" {
			apiKeyPolicy = policy
		}
	}
	if apiKeyPolicy == nil {
		t.Fatalf("干净的 API Key 应被授予信任策略，实际拿到 %d 条: %+v", len(policies), policies)
	}
	if apiKeyPolicy.Status != PromptRiskTrustStatusActive {
		t.Fatalf("策略状态 = %q", apiKeyPolicy.Status)
	}
	if apiKeyPolicy.Source != PromptRiskTrustSourceAutomatic {
		t.Fatalf("策略来源 = %q", apiKeyPolicy.Source)
	}

	// 确认 client_ip 画像本身是存在的——不然「没被授信」可能只是因为没有画像。
	profiles, _, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 50})
	if err != nil {
		t.Fatalf("ListPromptRiskProfiles: %v", err)
	}
	var hasClientIP, hasAPIKey bool
	for _, profile := range profiles {
		switch profile.SubjectType {
		case PromptRiskSubjectClientIP:
			hasClientIP = true
		case PromptRiskSubjectAPIKey:
			hasAPIKey = true
		}
	}
	if !hasClientIP || !hasAPIKey {
		t.Fatalf("应当同时存在 api_key 与 client_ip 画像：api_key=%v client_ip=%v", hasAPIKey, hasClientIP)
	}
}
