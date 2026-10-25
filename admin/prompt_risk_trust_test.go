package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestPromptRiskTrustAdminGrantDetailAndRevoke(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "admin-risk-trust.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	if err := db.InsertPromptFilterLog(t.Context(), &database.PromptFilterLogInput{
		Source: "local_filter", Action: "allow", ReviewModel: "review-model", ReviewFlagged: false,
		NewAPIPolicyStatus: "verified", NewAPIPlatform: "gateway-a", NewAPIUserID: "trusted-admin-user",
	}); err != nil {
		t.Fatalf("InsertPromptFilterLog: %v", err)
	}
	profiles, total, err := db.ListPromptRiskProfiles(t.Context(), database.PromptRiskProfileQuery{Page: 1, PageSize: 10, SubjectType: database.PromptRiskSubjectNewAPIUser})
	if err != nil || total != 1 || len(profiles) != 1 {
		t.Fatalf("profiles=%#v total=%d err=%v", profiles, total, err)
	}
	profile := profiles[0]
	h := &Handler{db: db}
	router := gin.New()
	router.PUT("/api/admin/prompt-policy/risk-profiles/:subject_type/:subject_key/trust", h.UpsertPromptRiskTrustPolicy)
	router.GET("/api/admin/prompt-policy/risk-profiles/:subject_type/:subject_key", h.GetPromptRiskProfile)
	router.DELETE("/api/admin/prompt-policy/risk-profiles/:subject_type/:subject_key/trust", h.RevokePromptRiskTrustPolicy)
	path := "/api/admin/prompt-policy/risk-profiles/newapi_user/" + profile.SubjectKey + "/trust"
	grant := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(`{"duration_hours":24,"risk_threshold":35,"reason":"paid user first-token optimization"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(grant, req)
	if grant.Code != http.StatusOK || !strings.Contains(grant.Body.String(), `"status":"active"`) {
		t.Fatalf("grant status=%d body=%s", grant.Code, grant.Body.String())
	}
	detail := httptest.NewRecorder()
	router.ServeHTTP(detail, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/risk-profiles/newapi_user/"+profile.SubjectKey, nil))
	if detail.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detail.Code, detail.Body.String())
	}
	var response struct {
		Profile struct {
			TrustPolicy *database.PromptRiskTrustPolicy `json:"trust_policy"`
		} `json:"profile"`
		TrustEvents []database.PromptRiskTrustEvent `json:"trust_events"`
	}
	if err := json.Unmarshal(detail.Body.Bytes(), &response); err != nil || response.Profile.TrustPolicy == nil || len(response.TrustEvents) != 1 {
		t.Fatalf("detail response=%s err=%v", detail.Body.String(), err)
	}
	revoke := httptest.NewRecorder()
	router.ServeHTTP(revoke, httptest.NewRequest(http.MethodDelete, path, nil))
	if revoke.Code != http.StatusOK || !strings.Contains(revoke.Body.String(), `"status":"revoked"`) {
		t.Fatalf("revoke status=%d body=%s", revoke.Code, revoke.Body.String())
	}
}

// 中间层给付费用户授信走 api_key 维度：一把 shaft-{uid} 就是一个 uid。
// 这条路径必须能过接口；出口 IP 维度仍然被拒。
func TestPromptRiskTrustAdminGrantsAPIKeySubject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "admin-risk-trust-apikey.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	confidence := 0.1
	if err := db.InsertPromptFilterLog(t.Context(), &database.PromptFilterLogInput{
		Source: "local_filter", Action: "allow", ReviewModel: "review-model", ReviewConfidence: &confidence,
		APIKeyID: 37, APIKeyName: "shaft-65550035", ClientIP: "203.0.113.9",
	}); err != nil {
		t.Fatalf("InsertPromptFilterLog: %v", err)
	}
	h := &Handler{db: db}
	router := gin.New()
	router.PUT("/api/admin/prompt-policy/risk-profiles/:subject_type/:subject_key/trust", h.UpsertPromptRiskTrustPolicy)
	router.DELETE("/api/admin/prompt-policy/risk-profiles/:subject_type/:subject_key/trust", h.RevokePromptRiskTrustPolicy)

	grant := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/admin/prompt-policy/risk-profiles/api_key/37/trust",
		strings.NewReader(`{"duration_hours":24,"risk_threshold":35,"reason":"pixshaft 付费用户连续 6 次干净翻译"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(grant, req)
	if grant.Code != http.StatusOK || !strings.Contains(grant.Body.String(), `"status":"active"`) {
		t.Fatalf("api_key grant status=%d body=%s", grant.Code, grant.Body.String())
	}

	// 出口 IP 维度必须仍然被拒——它后面可能是几十个互不相关的人。
	ipGrant := httptest.NewRecorder()
	ipReq := httptest.NewRequest(http.MethodPut, "/api/admin/prompt-policy/risk-profiles/client_ip/deadbeef/trust",
		strings.NewReader(`{"duration_hours":24,"risk_threshold":35,"reason":"should be rejected"}`))
	ipReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(ipGrant, ipReq)
	if ipGrant.Code != http.StatusBadRequest {
		t.Fatalf("client_ip grant status=%d body=%s（应当被拒）", ipGrant.Code, ipGrant.Body.String())
	}

	revoke := httptest.NewRecorder()
	router.ServeHTTP(revoke, httptest.NewRequest(http.MethodDelete, "/api/admin/prompt-policy/risk-profiles/api_key/37/trust", nil))
	if revoke.Code != http.StatusOK || !strings.Contains(revoke.Body.String(), `"status":"revoked"`) {
		t.Fatalf("api_key revoke status=%d body=%s", revoke.Code, revoke.Body.String())
	}
}
