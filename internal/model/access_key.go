// Package model 数据模型层
package model

import "time"

// AccessKey 客户端调用网关的凭证（AccessKey 管理）
//
// 设计要点（面试讲点）：
//   - 库里只存 KeyHash（SHA-256），绝不存明文——数据库泄露也拿不到可用凭证
//   - 明文只在创建时返回一次，之后不可再查（和 GitHub token / OpenAI API key 同款做法）
//   - Scopes：该 key 允许调用的模型范围（逗号分隔模型名，""/"*" = 全部）
//   - 中间件校验通过后把 akID 注入 context，限流维度随之换成"可信身份"而不是客户端自报
type AccessKey struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	Name      string     `gorm:"size:100" json:"name"`         // 备注名（谁在用这把 key）
	KeyHash   string     `gorm:"size:64;uniqueIndex" json:"-"` // sha256(key) 十六进制；对外永不暴露
	Scopes    string     `gorm:"size:255" json:"scopes"`       // 允许的模型，逗号分隔；空 = 全部
	Enabled   bool       `gorm:"default:true" json:"enabled"`  // 软启停（不停用也能吊销：停用即 401）
	LastUsedAt *time.Time `json:"last_used_at"`                // 最近一次调用时间（可审计、可清理僵尸 key）
	CreatedAt time.Time  `json:"created_at"`
}
