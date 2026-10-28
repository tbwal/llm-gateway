package model

import "time"

// ModelPricing 模型单价表（管理台可配置）
// 单位：元 / 1K token（input 输入价 / output 输出价）
type ModelPricing struct {
	ID         uint      `json:"id" gorm:"primaryKey"`
	ModelName  string    `json:"model_name" gorm:"uniqueIndex;size:100;not null"` // 模型名（唯一）
	InputPrice float64   `json:"input_price"` // 输入单价（元/1K token）
	OutputPrice float64  `json:"output_price"` // 输出单价（元/1K token）
	Enabled    bool      `json:"enabled" gorm:"default:true"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (ModelPricing) TableName() string { return "model_pricing" }
