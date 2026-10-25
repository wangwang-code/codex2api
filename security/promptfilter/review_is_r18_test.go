package promptfilter

import (
	"encoding/json"
	"testing"
)

// is_r18 is caller-facing metadata, so the parse has to survive every shape the
// upstream audit model has been seen to emit, and must never invent a value.
func TestParseReviewOptionalBoolAcceptsBoolAndString(t *testing.T) {
	cases := []struct {
		raw  string
		want *bool
	}{
		{`true`, boolPtr(true)},
		{`false`, boolPtr(false)},
		{`"true"`, boolPtr(true)},
		{`"false"`, boolPtr(false)},
		{`"TRUE"`, boolPtr(true)},
		{``, nil},
		{`null`, nil},
		{`"yes"`, nil},
		{`1`, nil},
		{`{}`, nil},
	}
	for _, tc := range cases {
		got := parseReviewOptionalBool(json.RawMessage(tc.raw))
		if tc.want == nil {
			if got != nil {
				t.Fatalf("parseReviewOptionalBool(%q) = %v, want nil", tc.raw, *got)
			}
			continue
		}
		if got == nil || *got != *tc.want {
			t.Fatalf("parseReviewOptionalBool(%q) = %v, want %v", tc.raw, got, *tc.want)
		}
	}
}

// The audit model's new field must reach the caller through the normal decode
// path, and its absence must stay "unknown" rather than defaulting to false.
func TestDecodeChatReviewResponseCarriesIsR18(t *testing.T) {
	cfg := NormalizeReviewConfig(DefaultReviewConfig())

	withR18 := `{"choices":[{"message":{"content":"{\"confidence\":0.95,\"reason\":\"explicit sexual content\",\"is_r18\":true}"}}]}`
	outcome, err := decodeChatReviewResponse([]byte(withR18), cfg)
	if err != nil {
		t.Fatalf("decode with is_r18: %v", err)
	}
	if outcome.IsR18 == nil || !*outcome.IsR18 {
		t.Fatalf("IsR18 = %v, want true", outcome.IsR18)
	}

	without := `{"choices":[{"message":{"content":"{\"confidence\":0.95,\"reason\":\"credential theft\"}"}}]}`
	outcome, err = decodeChatReviewResponse([]byte(without), cfg)
	if err != nil {
		t.Fatalf("decode without is_r18: %v", err)
	}
	if outcome.IsR18 != nil {
		t.Fatalf("IsR18 = %v, want nil when the model omitted the field", *outcome.IsR18)
	}
}

// ApplyReviewOutcome is the single mapping from outcome to verdict; the flag has
// to survive it or the proxy layer has nothing to emit.
func TestApplyReviewOutcomeCarriesIsR18ToVerdict(t *testing.T) {
	cfg := NormalizeReviewConfig(DefaultReviewConfig())
	verdict := Verdict{Enabled: true, Action: ActionBlock}
	yes := true
	got := ApplyReviewOutcome(verdict, ReviewOutcome{
		Flagged: true, Confidence: 0.99, Reason: "adult content", IsR18: &yes,
	}, nil, cfg)
	if got.ReviewIsR18 == nil || !*got.ReviewIsR18 {
		t.Fatalf("ReviewIsR18 = %v, want true", got.ReviewIsR18)
	}

	got = ApplyReviewOutcome(verdict, ReviewOutcome{Flagged: true, Confidence: 0.99, Reason: "injection"}, nil, cfg)
	if got.ReviewIsR18 != nil {
		t.Fatalf("ReviewIsR18 = %v, want nil", *got.ReviewIsR18)
	}
}

func boolPtr(v bool) *bool { return &v }
