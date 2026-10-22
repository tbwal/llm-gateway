// Package handler 业务处理层：接收 HTTP 请求 → 操作数据库 → 返回统一 JSON
package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"llm-gateway/internal/model"
)

// ChannelHandler 渠道接口处理器
//
// 关键设计：把数据库句柄 db 存进结构体 —— 这叫"依赖注入"的雏形
//   - 不用全局变量，处理器需要什么就通过构造器传入什么
//   - 好处：可测试（换一个 mock 库）、可替换、职责清晰
type ChannelHandler struct {
	db *gorm.DB
}

// NewChannelHandler 构造器：把数据库依赖注入处理器
func NewChannelHandler(db *gorm.DB) *ChannelHandler {
	return &ChannelHandler{db: db}
}

// resp 统一响应函数：所有接口同一格式 {code, message, data}
func resp(c *gin.Context, code int, msg string, data any) {
	c.JSON(http.StatusOK, gin.H{"code": code, "message": msg, "data": data})
}

// channelView 渠道"对外视图"：手动挑选字段返回，密钥（api_key）绝不外泄
// 为什么要多此一举？
//
//	之前用 json:"-" 想隐藏密钥，结果它"双向屏蔽"——连请求体解析也挡住了，
//	导致密钥存不进去。所以现在：模型层允许 api_key 正常进出（存储层要它），
//	响应层在这里手动过滤（对外层不暴露）。这叫"模型与视图分离"。
func channelView(ch model.Channel) gin.H {
	return gin.H{
		"id":         ch.ID,
		"name":       ch.Name,
		"provider":   ch.Provider,
		"base_url":   ch.BaseURL,
		"weight":     ch.Weight,
		"enabled":    ch.Enabled,
		"created_at": ch.CreatedAt,
		// 没有 api_key —— 对外永远看不到密钥
	}
}

// Create POST /api/channels —— 创建渠道
// 流程：解析请求体 JSON → GORM 写库 → 返回新渠道（含自增 ID，不含密钥）
func (h *ChannelHandler) Create(c *gin.Context) {
	var ch model.Channel
	// ① 把请求体 JSON 解析进结构体
	// 请求示例：{"name":"DeepSeek 主","provider":"deepseek","base_url":"https://api.deepseek.com/v1","api_key":"sk-xxx","weight":1}
	if err := c.ShouldBindJSON(&ch); err != nil {
		resp(c, 400, "参数错误: "+err.Error(), nil)
		return
	}

	// ② GORM 写库：INSERT INTO channels (...) VALUES (...)
	//    - ID（自增）和 CreatedAt（当前时间）由数据库/GORM 自动填充
	if err := h.db.Create(&ch).Error; err != nil {
		resp(c, 500, "创建失败: "+err.Error(), nil)
		return
	}

	// ③ 返回创建成功的渠道（走 channelView，密钥不返回）
	resp(c, 0, "ok", channelView(ch))
}

// List GET /api/channels —— 渠道列表
// 流程：GORM 全表查询 → 逐条转对外视图 → 返回
func (h *ChannelHandler) List(c *gin.Context) {
	var list []model.Channel
	// GORM 查询：SELECT * FROM channels，结果自动填入 list 切片
	// Find 的约定：传入 &切片 → 查询所有行；传入 &结构体 + Where → 查单行
	if err := h.db.Find(&list).Error; err != nil {
		resp(c, 500, "查询失败: "+err.Error(), nil)
		return
	}
	// 逐条转换成对外视图（隐藏密钥）
	views := make([]gin.H, 0, len(list))
	for _, ch := range list {
		views = append(views, channelView(ch))
	}
	resp(c, 0, "ok", views)
}
