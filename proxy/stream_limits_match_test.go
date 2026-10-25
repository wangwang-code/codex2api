package proxy

import "testing"

// TestStreamLimitMatchesKeyName 验证 api-keys 可以写密钥**名称**。
//
// 按用户发密钥的部署里，规则不可能列出每一把密钥，只能按名字前缀匹配。
func TestStreamLimitMatchesKeyName(t *testing.T) {
	restoreEnabled, restoreRules := streamLimitsEnabled, streamLimitRules
	t.Cleanup(func() { streamLimitsEnabled, streamLimitRules = restoreEnabled, restoreRules })

	streamLimitsEnabled = true
	streamLimitRules = []streamLimitRule{
		{Name: "translation", APIKeys: []string{"shaft-*"}, Models: []string{"gpt-6-*"}, BaseChars: 100, MinChars: 100},
	}

	budget, ok := resolveStreamLimitBudget("sk-whatever-derived", "shaft-12345", "gpt-6-luna", 10)
	if !ok {
		t.Fatal("密钥名 shaft-12345 应命中 shaft-* 规则")
	}
	if budget.ruleName != "translation" {
		t.Fatalf("ruleName = %q", budget.ruleName)
	}

	// 名字不匹配就不命中 —— 否则这条规则等于没写。
	if _, ok := resolveStreamLimitBudget("sk-x", "review", "gpt-6-luna", 10); ok {
		t.Fatal("密钥名不匹配不应命中")
	}
	// 只有名字能匹配、没有密钥串时也要生效（内部调用路径没有密钥串）。
	if _, ok := resolveStreamLimitBudget("", "shaft-9", "gpt-6-luna", 10); !ok {
		t.Fatal("缺少密钥串时应按密钥名匹配")
	}
}

// TestStreamLimitMatchesExactKeyStillWorks 验证历史配置（写死明文密钥）行为不变。
func TestStreamLimitMatchesExactKeyStillWorks(t *testing.T) {
	restoreEnabled, restoreRules := streamLimitsEnabled, streamLimitRules
	t.Cleanup(func() { streamLimitsEnabled, streamLimitRules = restoreEnabled, restoreRules })

	streamLimitsEnabled = true
	streamLimitRules = []streamLimitRule{
		{Name: "legacy", APIKeys: []string{"sk-FbXYxnIadgzl63ceH9IBzzq8wcLBv3MLIQBJ1RgwdsWReO3P"}, BaseChars: 100, MinChars: 100},
	}

	if _, ok := resolveStreamLimitBudget("sk-FbXYxnIadgzl63ceH9IBzzq8wcLBv3MLIQBJ1RgwdsWReO3P", "shaft-1", "gpt-6-luna", 10); !ok {
		t.Fatal("写死明文密钥的旧配置必须继续生效")
	}
	if _, ok := resolveStreamLimitBudget("sk-other", "shaft-1", "gpt-6-luna", 10); ok {
		t.Fatal("明文密钥不匹配时不应命中")
	}
}

// TestStreamLimitGlobMatch 固化通配语义：只有 `*` 是元字符。
func TestStreamLimitGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"*", "anything", true},
		{"*", "", true}, // 空串由 matchAny 跳过，不在 glob 层处理
		{"shaft-*", "shaft-1", true},
		{"shaft-*", "shaft-", true},
		{"shaft-*", "review", false},
		{"shaft-1*", "shaft-12345", true},
		{"*12345", "shaft-12345", true},
		{"*12345", "shaft-123456", false},
		{"shaft-*-x", "shaft-abc-x", true},
		{"shaft-*-x", "shaft-abc-y", false},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
		{"exact", "exact", true},
		{"exact", "exact2", false},
		// `?` 与 `[` 是字面量，不是元字符 —— 写规则的人只会想到 `*`。
		{"sk-??", "sk-ab", false},
		{"sk-[a]", "sk-[a]", true},
	}
	for _, tc := range cases {
		if got := streamLimitGlobMatch(tc.pattern, tc.value); got != tc.want {
			t.Errorf("streamLimitGlobMatch(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}

// TestStreamLimitMatchAnySkipsEmptyCandidate 验证空候选值不参与匹配：
// 内部调用没有密钥串，不能被 `*` 规则顺手算成命中。
func TestStreamLimitMatchAnySkipsEmptyCandidate(t *testing.T) {
	if streamLimitMatchAny([]string{"*"}, "") {
		t.Fatal("空候选值不应命中 * 规则")
	}
	if !streamLimitMatchAny([]string{"*"}, "", "shaft-7") {
		t.Fatal("有一个非空候选值命中即可")
	}
	if !streamLimitMatchAny(nil, "") {
		t.Fatal("空模式列表应匹配任意请求")
	}
}
