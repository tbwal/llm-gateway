package usage

import (
	"sync"
	"time"

	"gorm.io/gorm"

	"llm-gateway/internal/model"
)

// ============================================================
// 模型单价缓存（管理台可配置，热更新）
//
// 为什么要缓存：每请求 CalcCost 都查库太慢——
// 单价是低频变更数据，用内存快照 + 管理台主动刷新（30s 轮询兜底）
// 跟 channelCache 同一个模式。
// ============================================================

// defaultPrice 未匹配到单价表的模型兜底（统一低价）
var defaultPrice = price{input: 0.002, output: 0.002}

// PricingCache 单价缓存：无锁快照读
type PricingCache struct {
	db          *gorm.DB
	mu          sync.RWMutex
	snapshot    map[string]price // 模型名 → 单价
	lastRefresh time.Time
}

// NewPricingCache 构造
func NewPricingCache(db *gorm.DB) *PricingCache {
	c := &PricingCache{db: db, snapshot: map[string]price{}}
	c.Refresh()
	return c
}

// Refresh 从数据库全量加载（管理台 CRUD 后主动调）
func (c *PricingCache) Refresh() {
	var rows []model.ModelPricing
	if err := c.db.Where("enabled = ?", true).Find(&rows).Error; err != nil {
		return
	}
	m := make(map[string]price, len(rows))
	for _, r := range rows {
		m[r.ModelName] = price{input: r.InputPrice, output: r.OutputPrice}
	}
	c.mu.Lock()
	c.snapshot = m
	c.lastRefresh = time.Now()
	c.mu.Unlock()
}

// LastRefresh 最近刷新时刻
func (c *PricingCache) LastRefresh() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lastRefresh
}

// Lookup 查单价：精确匹配 → 默认兜底
// 不再做前缀匹配——新模型没配单价就走 defaultPrice，管理台显式添加更可控
func (c *PricingCache) Lookup(modelName string) price {
	c.mu.RLock()
	snap := c.snapshot
	c.mu.RUnlock()

	// ① 精确匹配
	if p, ok := snap[modelName]; ok {
		return p
	}
	// ② 未配置 → 默认兜底
	return defaultPrice
}

// SeedDefaults 首次启动时写入默认单价（如果表为空）
func SeedDefaults(db *gorm.DB) error {
	var count int64
	db.Model(&model.ModelPricing{}).Count(&count)
	if count > 0 {
		return nil
	}
	defaults := []model.ModelPricing{
		{ModelName: "deepseek-v1-flash", InputPrice: 0.14, OutputPrice: 0.28, Enabled: true},
		{ModelName: "gpt-5", InputPrice: 0.018, OutputPrice: 0.072, Enabled: true},
		{ModelName: "claude-3-5-sonnet", InputPrice: 0.022, OutputPrice: 0.088, Enabled: true},
	}
	return db.Create(&defaults).Error
}
