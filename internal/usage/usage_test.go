package usage

import (
	"testing"

	"llm-gateway/internal/model"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

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

// newTestPricing 测试用内存数据库构造 PricingCache
func newTestPricing(t *testing.T) *PricingCache {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.ModelPricing{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db.Create(&model.ModelPricing{
		ModelName: "mock-upstream", InputPrice: 0.001, OutputPrice: 0.002, Enabled: true,
	})
	db.Create(&model.ModelPricing{
		ModelName: "claude-3-5-sonnet", InputPrice: 0.022, OutputPrice: 0.088, Enabled: true,
	})
	c := NewPricingCache(db)
	SetPricingCache(c)
	return c
}

// TestCalcCost 已知模型单价计算
func TestCalcCost(t *testing.T) {
	newTestPricing(t)

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

// TestLookupPrice 精确匹配 + 默认兜底
func TestLookupPricePrefix(t *testing.T) {
	c := newTestPricing(t)

	// 精确匹配
	p := c.Lookup("mock-upstream")
	if p.input != 0.001 {
		t.Fatalf("精确匹配失败: input=%f, want 0.001", p.input)
	}

	// 未配置的模型 → 默认兜底 0.002
	p2 := c.Lookup("unknown-model")
	if p2.input != 0.002 {
		t.Fatalf("默认兜底失败: input=%f, want 0.002", p2.input)
	}
}
