// Package model 数据模型层：定义业务数据的"结构图纸"
// GORM 会把下面的结构体自动映射成数据库表 channels
package model

import "time"

// Channel 渠道：一个渠道 = 一家上游服务（如 DeepSeek、OpenAI、Claude）
//
// 字段后的两个标签分别控制两层：
//   gorm:"..."  → 数据库层（列名、约束、默认值）
//   json:"..."  → HTTP 层（接口返回时的字段名）
type Channel struct {
	ID        uint      `gorm:"primaryKey" json:"id"`                 // 主键，自增
	Name      string    `gorm:"size:100;not null" json:"name"`        // 渠道名（如 "DeepSeek 主"）
	Provider  string    `gorm:"size:50;not null" json:"provider"`     // 上游类型：openai / anthropic / deepseek
	BaseURL   string    `gorm:"size:255" json:"base_url"`             // 上游地址（如 https://api.deepseek.com/v1）
	APIKey    string    `gorm:"size:255" json:"-"`                    // 上游密钥
	// ⚠️ json:"-" 表示：接口返回时永远隐藏这个字段（密钥绝不外泄）
	// 第 3 步会把它升级成 AES-GCM 加密后再落库
	Weight    int       `gorm:"default:1" json:"weight"`              // 调度权重（权重越高被选中概率越大）
	Enabled   bool      `gorm:"default:true" json:"enabled"`          // 是否启用（停用后调度不选它）
	CreatedAt time.Time `json:"created_at"`                           // 创建时间（GORM 自动填）
}

// 表名规则：结构体 Channel → 表 channels（Go 会自动复数化）
// 如果想自定义表名，可以加一个方法：
//   func (Channel) TableName() string { return "channels" }
