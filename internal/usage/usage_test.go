package usage

import "testing"

// TestEstimateTokens 估算：ASCII 4 字符≈1 token，中文 1 字≈1 token
func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		text string
		want int
	}{
		{"hello world", 2},            // 11 个 ASCII 字符 → 11/4 = 2
		{"你好世界", 4},                  // 4 个中文 → 4
		{"", 0},                        // 空串
		{"你好 hello 世界", 4 + 7/4}, // 中文4 + " hello "7个ASCII/4=1 → 5
	}
	for _, c := range cases {
		if got := EstimateTokens(c.text); got != c.want {
			t.Fatalf("EstimateTokens(%q) = %d, want %d", c.text, got, c.want)
		}
	}
}

// TestCalcCost 已知模型单价计算
func TestCalcCost(t *testing.T) {
	// mock-upstream: input 0.001 元/1K, output 0.002 元/1K
	// 1000 input + 2000 output → 0.001*1 + 0.002*2 = 0.005
	cost := CalcCost("mock-upstream", 1000, 2000)
	if cost < 0.0049 || cost > 0.0051 {
		t.Fatalf("CalcCost = %f, want ≈0.005", cost)
	}

	// 未知模型走默认价（0.002/0.002 元/1K）
	cost2 := CalcCost("unknown-model-xyz", 1000, 1000)
	if cost2 < 0.0039 || cost2 > 0.0041 {
		t.Fatalf("CalcCost(unknown) = %f, want ≈0.004", cost2)
	}
}

// TestLookupPricePrefix 前缀匹配：新模型不至于掉到默认价
func TestLookupPricePrefix(t *testing.T) {
	// claude-3-7-sonnet 不存在精确表 → 前缀 claude- → 用 claude-3-5-sonnet 的价
	p := lookupPrice("claude-3-7-sonnet")
	if p.input != prices["claude-3-5-sonnet"].input {
		t.Fatalf("前缀匹配失败: %+v", p)
	}
}
