package usage

import "strings"

// price 模型单价（输入/输出，元 / 1K token）
// 简化配置表（示例价，生产放数据库/配置中心，按渠道可覆盖）
type price struct {
	input  float64
	output float64
}

// prices 精确模型名 → 单价
// 说明：价格为示例值（接近各厂商公开价的量级），面试时可说明"单价表应配置化"
var prices = map[string]price{
	"deepseek-chat":       {input: 0.14, output: 0.28},
	"deepseek-reasoner":   {input: 0.55, output: 1.19},
	"gpt-4o":              {input: 0.018, output: 0.072},
	"gpt-4o-mini":         {input: 0.0011, output: 0.0044},
	"claude-3-5-sonnet":   {input: 0.022, output: 0.088},
	"claude-3-haiku":      {input: 0.0018, output: 0.009},
	"mock-upstream":       {input: 0.001, output: 0.002}, // 本地 mock：象征性单价
}

// defaultPrice 未匹配到单价表的模型兜底（统一低价）
var defaultPrice = price{input: 0.002, output: 0.002}

// lookupPrice 按模型名查单价：精确匹配 → 前缀匹配 → 默认兜底
// 前缀匹配：如 "claude-3-7-sonnet" 能命中 "claude-3-5-sonnet"？不——前缀匹配用 provider 前缀
// 简化：按模型名前缀粗匹配常见厂商，保证新模型不至于掉到默认价
func lookupPrice(model string) price {
	if p, ok := prices[model]; ok {
		return p
	}
	// 前缀匹配：claude-* → claude 系；gpt-* → openai 系；deepseek-* → deepseek 系
	switch {
	case strings.HasPrefix(model, "claude-"):
		return prices["claude-3-5-sonnet"]
	case strings.HasPrefix(model, "gpt-"):
		return prices["gpt-4o-mini"]
	case strings.HasPrefix(model, "deepseek-"):
		return prices["deepseek-chat"]
	default:
		return defaultPrice
	}
}

// CalcCost 按模型单价计算费用（元）
//   prompt 费 = 输入单价 × promptTokens/1000
//   completion 费 = 输出单价 × completionTokens/1000
func CalcCost(model string, promptTokens, completionTokens int) float64 {
	p := lookupPrice(model)
	return p.input*float64(promptTokens)/1000 + p.output*float64(completionTokens)/1000
}
