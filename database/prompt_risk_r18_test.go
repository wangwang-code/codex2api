package database

import (
	"context"
	"testing"
)

// R18 命中必须变成独立事件类型：留档、但不带分数也不带指纹。
//
// 这条是整件事的支点——分数和指纹任一漏掉，R18 流量都会通过 weighted_total
// 或 recurrence 把正常用户的画像推高。
func TestPromptRiskSignalExcludesR18Hits(t *testing.T) {
	r18 := true
	logItem := PromptFilterLog{
		ID: 7, Source: "local_filter", Action: "block", StrikeEligible: true,
		ReviewFlagged: true, ReviewModel: "codex-auto-review", ReviewConfidence: floatPtr(0.97),
		ReviewReason: "包含明确的性行为表述", TextPreview: "先生、今日は何の日か…",
		IsR18: &r18, APIKeyID: 9, APIKeyName: "shaft-42", RequestCorrelationID: "corr-7",
	}

	signal, ok := promptRiskSignalForLog(logItem)
	if !ok {
		t.Fatal("R18 记录应产生一条（被排除的）事件，而不是完全丢弃")
	}
	if signal.EventKind != promptRiskEventR18Excluded {
		t.Fatalf("event_kind = %q, want %q", signal.EventKind, promptRiskEventR18Excluded)
	}
	if signal.RequestRiskScore != 0 {
		t.Fatalf("request_risk_score = %d, want 0", signal.RequestRiskScore)
	}
	if signal.EvidenceConfidence != 0 {
		t.Fatalf("evidence_confidence = %d, want 0", signal.EvidenceConfidence)
	}
	if signal.PromptFingerprint != "" {
		t.Fatalf("prompt_fingerprint = %q, want 空（重复指纹会加 recurrence 分）", signal.PromptFingerprint)
	}
	// 归因信息要留着，否则后台看不出这条属于谁。
	if signal.APIKeyName != "shaft-42" || signal.RequestCorrelationID != "corr-7" {
		t.Fatalf("归因信息丢了: %+v", signal)
	}
}

// 明确 false 与「未知」都必须照旧计分。
func TestPromptRiskSignalKeepsNonR18Hits(t *testing.T) {
	falseValue := false
	for _, tc := range []struct {
		name  string
		isR18 *bool
	}{
		{"明确不是 R18", &falseValue},
		{"审核没给 is_r18", nil},
	} {
		logItem := PromptFilterLog{
			ID: 8, Source: "local_filter", Action: "block", StrikeEligible: true,
			ReviewFlagged: true, ReviewModel: "codex-auto-review", TextPreview: "what is 137*29?",
			IsR18: tc.isR18,
		}
		signal, ok := promptRiskSignalForLog(logItem)
		if !ok {
			t.Fatalf("%s：应当产生信号", tc.name)
		}
		if signal.EventKind == promptRiskEventR18Excluded {
			t.Fatalf("%s：不该被当成 R18 排除", tc.name)
		}
		if signal.RequestRiskScore != 40 {
			t.Fatalf("%s：request_risk_score = %d, want 40", tc.name, signal.RequestRiskScore)
		}
		if signal.PromptFingerprint == "" {
			t.Fatalf("%s：非 R18 记录应当保留指纹", tc.name)
		}
	}
}

// 端到端：同样的 block 记录，is_r18=true 的那条不能给画像加分。
func TestPromptRiskProfileIgnoresR18Blocks(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()

	r18 := true
	notR18 := false
	base := func(isR18 *bool, preview string) *PromptFilterLogInput {
		return &PromptFilterLogInput{
			Source: "local_filter", Endpoint: "/v1/chat/completions", Model: "gpt-6-luna",
			Action: "block", Score: 95, AuditScore: 95, ReasonCode: "terminal_category", StrikeEligible: true,
			MatchedPatterns: `[{"name":"terminal","weight":95}]`,
			TextPreview:     preview, APIKeyID: 9, APIKeyName: "shaft-42", APIKeyMasked: "sk-***9",
			IsR18: isR18,
		}
	}

	// 同一个主体先来一条 R18 拦截，再来一条真正的注入拦截。
	if err := db.InsertPromptFilterLog(ctx, base(&r18, "先生、今日は何の日か")); err != nil {
		t.Fatalf("InsertPromptFilterLog(r18): %v", err)
	}
	afterR18, _, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("ListPromptRiskProfiles: %v", err)
	}
	apiKeyAfterR18 := findPromptRiskProfile(afterR18, PromptRiskSubjectAPIKey)
	if apiKeyAfterR18 == nil {
		t.Fatalf("应当有 API Key 画像: %#v", afterR18)
	}
	if apiKeyAfterR18.RiskScore != 0 {
		t.Fatalf("只有 R18 拦截时风险分 = %d, want 0", apiKeyAfterR18.RiskScore)
	}

	if err := db.InsertPromptFilterLog(ctx, base(&notR18, "what is 137*29?")); err != nil {
		t.Fatalf("InsertPromptFilterLog(not r18): %v", err)
	}
	afterInjection, _, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("ListPromptRiskProfiles: %v", err)
	}
	apiKeyAfterInjection := findPromptRiskProfile(afterInjection, PromptRiskSubjectAPIKey)
	if apiKeyAfterInjection == nil {
		t.Fatalf("应当有 API Key 画像: %#v", afterInjection)
	}
	if apiKeyAfterInjection.RiskScore <= apiKeyAfterR18.RiskScore {
		t.Fatalf("非 R18 拦截必须加分：R18 后 = %d，注入后 = %d",
			apiKeyAfterR18.RiskScore, apiKeyAfterInjection.RiskScore)
	}

	// 事件留档，且类型正确——运营要看得出「有 R18 流量，但没算它」。
	events, total, err := db.ListPromptRiskEvents(ctx, PromptRiskSubjectAPIKey, apiKeyAfterInjection.SubjectKey, PromptRiskEventQuery{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("ListPromptRiskEvents: %v", err)
	}
	if total != 2 {
		t.Fatalf("事件数 = %d, want 2（R18 那条也要留档）", total)
	}
	kinds := map[string]int{}
	for _, event := range events {
		kinds[event.EventKind]++
	}
	if kinds[promptRiskEventR18Excluded] != 1 {
		t.Fatalf("R18 事件数 = %d, want 1：%v", kinds[promptRiskEventR18Excluded], kinds)
	}
	for _, event := range events {
		if event.EventKind == promptRiskEventR18Excluded && event.RequestRiskScore != 0 {
			t.Fatalf("R18 事件带了分数 %d", event.RequestRiskScore)
		}
	}
}

func floatPtr(v float64) *float64 { return &v }
