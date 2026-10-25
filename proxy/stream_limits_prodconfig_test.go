package proxy

import (
	"os"
	"testing"
)

// 用线上那条规则的真实 JSON 验证：按用户发的密钥（shaft-<uid>）确实命中预算，
// 而且命中的是「按名称」这条路，不是靠写死的那把明文密钥。
//
// 这不是回归测试，是一次性核对——线上配置变化后它自然会失效，故用 env 跳过。
func TestProdStreamLimitRulesMatchPerUIDKeys(t *testing.T) {
	raw := os.Getenv("PROD_STREAM_LIMITS_RULES")
	if raw == "" {
		t.Skip("PROD_STREAM_LIMITS_RULES 未设置")
	}
	t.Setenv("STREAM_LIMITS_RULES", raw)

	restoreEnabled, restoreRules := streamLimitsEnabled, streamLimitRules
	t.Cleanup(func() { streamLimitsEnabled, streamLimitRules = restoreEnabled, restoreRules })

	streamLimitsEnabled = true
	streamLimitRules = streamLimitRulesFromEnv()
	if len(streamLimitRules) == 0 {
		t.Fatal("规则没解析出来")
	}

	cases := []struct {
		key, name, model string
		want             bool
		why              string
	}{
		{"sk-derived-aaaa", "shaft-80107868", "gpt-6-luna", true, "按用户发的密钥应命中"},
		{"sk-derived-bbbb", "shaft-1", "gpt-6-luna", true, "最短的 uid 也应命中"},
		{"sk-derived-cccc", "shaft-999999999999", "gpt-6-luna", true, "测试 uid"},
		{"sk-FbXYxnIadgzl63ceH9IBzzq8wcLBv3MLIQBJ1RgwdsWReO3P", "shaft", "gpt-6-luna", true, "旧共享密钥仍命中"},
		{"sk-derived-dddd", "astrbot", "gpt-6-luna", false, "别的密钥名不应命中"},
		{"sk-derived-eeee", "review", "gpt-6-luna", false, "审核密钥不应命中"},
		{"sk-derived-ffff", "shaft-1", "claude-sonnet", false, "模型不匹配不应命中"},
	}

	for _, tc := range cases {
		budget, ok := resolveStreamLimitBudget(tc.key, tc.name, tc.model, 1000)
		if ok != tc.want {
			t.Errorf("%s：key=%q name=%q model=%q → 命中=%v，期望 %v", tc.why, tc.key, tc.name, tc.model, ok, tc.want)
			continue
		}
		if ok {
			// 输入 1000 字符：200 + 1000×4 = 4200
			if budget.maxContentChars != 4200 {
				t.Errorf("%s：预算 = %d，期望 4200", tc.why, budget.maxContentChars)
			}
			if budget.ruleName != "translation" {
				t.Errorf("%s：命中的规则是 %q", tc.why, budget.ruleName)
			}
		}
	}

	// 关键一条：把明文密钥那条从配置里拿掉，名称匹配必须仍然成立。
	streamLimitRules = []streamLimitRule{{Name: "name-only", APIKeys: []string{"shaft*"}, Models: []string{"gpt-6-*"}, BaseChars: 100, MinChars: 100}}
	if _, ok := resolveStreamLimitBudget("sk-any", "shaft-80107868", "gpt-6-luna", 10); !ok {
		t.Fatal("只剩名称模式时仍应命中——否则说明是明文密钥在起作用，名称匹配其实没生效")
	}
}
