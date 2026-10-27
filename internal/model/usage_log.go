// Package model 数据模型层
package model

import "time"

// Attempt 一次渠道尝试（故障转移链中的一跳）
//
// 设计目的（面试讲点）：gpt-load 式"请求审计"——
//   原来只记最终一次结果，中间试了哪些渠道、为什么换、每次耗时多少全无痕迹。
//   有了 Attempts[]，排障能回答"这条请求先试了渠道A（500，耗时120ms），
//   又试了渠道B（超时，耗时3000ms），最后渠道C（200成功）"。
//
// 序列化存进 UsageLog.AttemptsJSON（TEXT），查日志页时反序列化展示。
type Attempt struct {
	Sequence    int    `json:"sequence"`      // 第几次尝试（从 1 开始）
	ChannelID   uint   `json:"channel_id"`     // 这次试的渠道 id
	ChannelName string `json:"channel_name"`   // 渠道名（冗余展示，免 join）
	Provider    string `json:"provider"`       // openai / anthropic / deepseek
	StatusCode  int    `json:"status_code"`   // 上游返回状态码；网络错误=0（没拿到响应）
	DurationMs  int64  `json:"duration_ms"`   // 这次尝试的耗时（发出请求 → 拿到响应/报错）
	Error       string `json:"error,omitempty"` // 错误摘要（成功为空；上限 200 字）
	WillRetry   bool   `json:"will_retry"`    // 这次失败后是否换渠道重试（false=最终结果）
}

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
	RequestID        string    `gorm:"size:40;index" json:"request_id"`         // 链路 ID（req_xxx）：同一请求跨日志/排障可关联
	ChannelID        uint      `json:"channel_id"`                              // 最终成功的渠道 id（失败=0 或最后一次）
	ChannelName      string    `gorm:"size:100" json:"channel_name"`            // 渠道名（冗余，看板免 join）
	Provider         string    `gorm:"size:50" json:"provider"`                 // openai / anthropic / deepseek
	Model            string    `gorm:"size:100" json:"model"`                   // 客户端请求的模型名
	AccessKeyID      uint      `gorm:"index" json:"access_key_id"`              // 调用方 AccessKey（0 = 匿名请求）
	AccessKeyName    string    `gorm:"size:100" json:"access_key_name"`         // 调用方备注名（冗余展示，免 join）
	ClientIP         string    `gorm:"size:45" json:"client_ip"`                // 调用方 IP（IPv6 最长 45）
	PromptTokens     int       `json:"prompt_tokens"`                           // 输入 token
	CompletionTokens int       `json:"completion_tokens"`                       // 输出 token
	TotalTokens      int       `json:"total_tokens"`                            // 合计
	// Cost 费用（元）：单价 × token 数/1000
	// 单价表在 internal/usage（简化配置；生产放 DB/配置中心）
	Cost float64 `json:"cost"`
	// Status 返回给客户端的 HTTP 状态码（成功=200；限流=429、未授权=403、无渠道=503…）
	// 失败请求同样落库：日志从"计费流水"升级为"请求审计"（面试讲点）
	Status int `gorm:"index" json:"status,omitempty"`
	// Error 失败时的错误摘要（成功为空）；上限 500 字防撑爆表
	Error     string    `gorm:"size:500" json:"error,omitempty"`
	DurationMs      int64     `json:"duration_ms"`        // 网关总耗时（请求进入 → 落库），毫秒；排障"慢"用
	FirstResponseMs int64     `json:"first_response_ms"`  // 首响应延迟：流式=首个增量 token；非流式=开始写回响应体（≈ TTFT）
	// AttemptsJSON 整组渠道尝试的 JSON 序列化（Attempts[]）。
	// 只在故障转移发生时非空；单次成功 = 数组长度 1。
	// TEXT 存整串，读日志页时反序列化展示——不拆成子表是因为
	//   尝试链随主请求生灭，没有独立查询需求（不会"只看某次尝试"）。
	AttemptsJSON string    `gorm:"type:TEXT" json:"attempts_json,omitempty"`
	CreatedAt time.Time `json:"created_at"` // GORM 自动填
}
