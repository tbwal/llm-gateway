// Package model 数据模型层
package model

import "time"

// Setting 运行时配置键值表（管理台可改、重启保留的持久化设置）
//
// 用途：限流次数等"运维可在 Web 上调整"的配置。
// 设计：KV 表，启动时优先读这里覆盖默认值（env 仍是第一优先），
// 运行时改动落这里，重启后不丢。
type Setting struct {
	Key       string    `gorm:"primaryKey;size:50" json:"key"`
	Value     string    `gorm:"size:200" json:"value"`
	UpdatedAt time.Time `json:"updated_at"` // GORM 自动填
}
