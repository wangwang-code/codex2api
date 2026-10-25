package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

// The middle layer reads is_r18 off the block response; both the header and the
// details field have to carry it, and both must stay absent when the audit model
// did not answer.
func TestPromptBlockedAPIErrorCarriesIsR18(t *testing.T) {
	cfg := promptfilter.Config{}
	yes := true

	apiErr := promptBlockedAPIError(cfg, promptfilter.Verdict{
		Action: promptfilter.ActionBlock, ReviewFlagged: true,
		ReviewReason: "explicit adult content", ReviewIsR18: &yes,
	})
	if apiErr.Code != "prompt_blocked" {
		t.Fatalf("code = %q, want prompt_blocked", apiErr.Code)
	}
	details, ok := apiErr.Details.(map[string]any)
	if !ok {
		t.Fatalf("Details = %#v, want map with is_r18", apiErr.Details)
	}
	if got, ok := details["is_r18"]; !ok || got != true {
		t.Fatalf("details.is_r18 = %v (present=%v), want true", got, ok)
	}

	// Unknown must not appear at all: a caller that never saw the field before
	// must see exactly the same envelope as before this change.
	apiErr = promptBlockedAPIError(cfg, promptfilter.Verdict{
		Action: promptfilter.ActionBlock, ReviewFlagged: true, ReviewReason: "injection",
	})
	if apiErr.Details != nil {
		t.Fatalf("Details = %#v, want nil when is_r18 is unknown", apiErr.Details)
	}
	raw, err := json.Marshal(apiErr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := probe["details"]; present {
		t.Fatalf("details must be omitted when is_r18 is unknown: %s", raw)
	}
}

func TestSetPromptFilterR18Header(t *testing.T) {
	gin.SetMode(gin.TestMode)

	yes := true
	no := false
	cases := []struct {
		name string
		flag *bool
		want string
	}{
		{"adult", &yes, "true"},
		{"not adult", &no, "false"},
		{"unknown", nil, ""},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		setPromptFilterR18Header(c, promptfilter.Verdict{ReviewIsR18: tc.flag})
		if got := w.Header().Get(promptFilterR18Header); got != tc.want {
			t.Fatalf("%s: header = %q, want %q", tc.name, got, tc.want)
		}
	}
}
