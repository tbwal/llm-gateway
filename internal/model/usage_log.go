// Package model 数据模型层
package model

import "time"

// UsageLog token 用量流水表（W3-② 计费落库）
//
// 设计原则：**流水表 + 查询时聚合**，而不是"每请求 UPDATE 一行计数"
//   - 每次请求 INSERT 一条流水（只增不减，天然可审计、可追溯）
//   - 聚合（SUM/按渠道/按模型/按天）在看板查询时做
//   - 面试讲点：为什么不维护"每渠道一个计数行、每请求 UPDATE"？
//     热点行锁——所有请求都抢同一行的写锁，并发越高越慢；
//     流水表只 INSERT，无锁竞争，聚合交给读侧（读多写少天然适合）
type UsageLog struct {
	ID               uint      `gorm:"primaryKey" json:"id"`
	ChannelID        uint      `json:"channel_id"`          // 走了哪个渠道（id）
	ChannelName      string    `gorm:"size:100" json:"channel_name"` // 渠道名（冗余，看板免 join）
	Provider         string    `gorm:"size:50" json:"provider"`      // openai / anthropic / deepseek
	Model            string    `gorm:"size:100" json:"model"`        // 客户端请求的模型名
	PromptTokens     int       `json:"prompt_tokens"`                // 输入 token
	CompletionTokens int       `json:"completion_tokens"`            // 输出 token
	TotalTokens      int       `json:"total_tokens"`                 // 合计
	// Cost 费用（元）：单价 × token 数/1000
	// 单价表在 internal/usage（简化配置；生产放 DB/配置中心）
	Cost float64 `json:"cost"`
	// Status 返回给客户端的 HTTP 状态码（成功=200；限流=429、未授权=403、无渠道=503…）
	// 失败请求同样落库：日志从"计费流水"升级为"请求审计"（面试讲点）
	Status int `gorm:"index" json:"status,omitempty"`
	// Error 失败时的错误摘要（成功为空）；上限 500 字防撑爆表
	Error     string    `gorm:"size:500" json:"error,omitempty"`
	CreatedAt time.Time `json:"created_at"` // GORM 自动填
}
