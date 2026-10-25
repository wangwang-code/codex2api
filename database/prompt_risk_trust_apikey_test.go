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

// 面板上的「干净复核 N/M」走的是 GetPromptRiskAdaptiveReviewBasis。
// 它曾经带着 is_person=TRUE，于是 api_key 画像永远显示 0/3 —— 而后台自动授信
// 走另一条查询，两边不一致，看起来就像授信没生效。
func TestAdaptiveReviewBasisCountsAPIKeyCleanReviews(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	confidence := 0.1

	for i := 0; i < 4; i++ {
		input := &PromptFilterLogInput{
			Source: "local_filter", Endpoint: "/v1/chat/completions", Model: "gpt-6-luna",
			Action: "allow", ReviewModel: "codex-auto-review", ReviewConfidence: &confidence,
			ReviewReason: "正常翻译", APIKeyID: 37, APIKeyName: "shaft-65550035", ClientIP: "203.0.113.9",
			ClientIPHash: promptRiskHash("client-ip", "203.0.113.9"),
			RequestCorrelationID: fmt.Sprintf("corr-basis-%d", i), TextPreview: fmt.Sprintf("t-%d", i),
		}
		if err := db.InsertPromptFilterLog(ctx, input); err != nil {
			t.Fatalf("InsertPromptFilterLog: %v", err)
		}
	}

	since := time.Now().UTC().Add(-time.Hour)
	basis, err := db.GetPromptRiskAdaptiveReviewBasis(ctx, PromptRiskSubjectAPIKey, "37", since)
	if err != nil {
		t.Fatalf("GetPromptRiskAdaptiveReviewBasis: %v", err)
	}
	if basis.CleanReviewCount != 4 {
		t.Fatalf("api_key 干净复核数 = %d, want 4（is_person 过滤会让它恒为 0）", basis.CleanReviewCount)
	}
	if basis.PositiveEvidenceCount != 0 {
		t.Fatalf("正面证据数 = %d, want 0", basis.PositiveEvidenceCount)
	}
	if basis.FirstCleanAt == nil || basis.LastCleanAt == nil {
		t.Fatalf("起止时间没算出来: %+v", basis)
	}

	// 面板和后台授信必须看到同一个数字——否则又会「后台说该授信、面板说 0/3」。
	summary, err := db.promptRiskTrustEvidenceSummarySince(ctx, PromptRiskSubjectAPIKey, "37", since)
	if err != nil {
		t.Fatalf("promptRiskTrustEvidenceSummarySince: %v", err)
	}
	if summary.CleanCount != basis.CleanReviewCount {
		t.Fatalf("两条查询不一致：面板 %d，授信 %d", basis.CleanReviewCount, summary.CleanCount)
	}
}

// 预授信要能被 codex2api 继续观测并升级：中间层凭付费身份给 24 小时，
// 观测够格后自动换成正式授信（更长时长、source=automatic）。
func TestProvisionalTrustEscalatesToAutomatic(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	confidence := 0.1

	for i := 0; i < 10; i++ {
		input := &PromptFilterLogInput{
			Source: "local_filter", Endpoint: "/v1/chat/completions", Model: "gpt-6-luna",
			Action: "allow", ReviewModel: "codex-auto-review", ReviewConfidence: &confidence,
			ReviewReason: "正常翻译", APIKeyID: 55, APIKeyName: "shaft-55550035", ClientIP: "203.0.113.9",
			ClientIPHash: promptRiskHash("client-ip", "203.0.113.9"),
			RequestCorrelationID: fmt.Sprintf("corr-prov-%d", i), TextPreview: fmt.Sprintf("p-%d", i),
		}
		if err := db.InsertPromptFilterLog(ctx, input); err != nil {
			t.Fatalf("InsertPromptFilterLog: %v", err)
		}
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE prompt_risk_events SET created_at=?`, time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatalf("backdate events: %v", err)
	}

	// 中间层给的预授信：24 小时。
	provisionalUntil := time.Now().UTC().Add(24 * time.Hour)
	if _, err := db.UpsertPromptRiskTrustPolicy(ctx, PromptRiskTrustPolicyInput{
		SubjectType: PromptRiskSubjectAPIKey, SubjectKey: "55", Source: PromptRiskTrustSourceProvisional,
		Reason: "pixshaft 付费用户（pro）连续 6 次干净翻译，中间层授信", RiskThreshold: 35, ValidUntil: provisionalUntil,
	}); err != nil {
		t.Fatalf("预授信写入失败: %v", err)
	}

	// 人工策略不该被自动升级（对照组）。
	if _, err := db.UpsertPromptRiskTrustPolicy(ctx, PromptRiskTrustPolicyInput{
		SubjectType: PromptRiskSubjectAPIKey, SubjectKey: "56", Source: PromptRiskTrustSourceManual,
		Reason: "管理员手工设置", RiskThreshold: 35, ValidUntil: provisionalUntil,
	}); err != nil {
		t.Fatalf("人工策略写入失败: %v", err)
	}

	policies, err := db.ReconcileAdaptivePromptRiskTrustPolicies(ctx, PromptRiskTrustAdaptiveOptions{
		MinCleanReviews: 3, MinObservationHours: 1, TrustDurationHours: 168,
	})
	if err != nil {
		t.Fatalf("ReconcileAdaptivePromptRiskTrustPolicies: %v", err)
	}

	var escalated *PromptRiskTrustPolicy
	for _, policy := range policies {
		if policy.SubjectKey == "55" {
			escalated = policy
		}
		if policy.SubjectKey == "56" && policy.Source != PromptRiskTrustSourceManual {
			t.Fatalf("人工策略被自动改写了: %+v", policy)
		}
	}
	if escalated == nil {
		t.Fatalf("预授信策略不见了: %+v", policies)
	}
	if escalated.Source != PromptRiskTrustSourceAutomatic {
		t.Fatalf("预授信未升级，source = %q", escalated.Source)
	}
	hours := escalated.ValidUntil.Sub(time.Now().UTC()).Hours()
	if hours < 167 || hours > 169 {
		t.Fatalf("升级后时长 = %.1fh，期望约 168h（预授信 24h 应被拉长）", hours)
	}
}
