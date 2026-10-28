package usage

// price 模型单价（输入/输出，元 / 1K token）
type price struct {
	input  float64
	output float64
}

// globalPricing 全局单价缓存（启动时由 router 注入；nil 时用硬编码兜底）
var globalPricing *PricingCache

// SetPricingCache 注入单价缓存（router 启动时调）
func SetPricingCache(c *PricingCache) {
	globalPricing = c
}

// CalcCost 按模型单价计算费用（元）
//   prompt 费 = 输入单价 × promptTokens/1000
//   completion 费 = 输出单价 × completionTokens/1000
func CalcCost(modelName string, promptTokens, completionTokens int) float64 {
	var p price
	if globalPricing != nil {
		p = globalPricing.Lookup(modelName)
	} else {
		// 缓存未初始化（测试）：用默认兜底
		p = defaultPrice
	}
	return p.input*float64(promptTokens)/1000 + p.output*float64(completionTokens)/1000
}
