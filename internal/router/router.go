// Package router 路由层：把"HTTP 请求路径"映射到"处理函数"
// 目前注册：/health、/api/channels（创建+列表）；后续加：
//   /v1/chat/completions  AI 网关统一入口
package router

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"llm-gateway/internal/handler"
)

// New 创建 gin 引擎并注册全部路由
// 参数 db：数据库句柄，路由层把它注入给业务处理器（依赖往下传递）
func New(db *gorm.DB) *gin.Engine {
	// gin.Default()：自带日志中间件 + panic 恢复中间件
	r := gin.Default()

	// 健康检查：部署后用来探测"服务是否存活"
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 渠道管理（第 3 步：完整 CRUD）
	ch := handler.NewChannelHandler(db) // 依赖注入：把 db 传给渠道处理器
	api := r.Group("/api/channels")     // 路由分组：组内路径自动带 /api/channels 前缀
	{
		// 裸花括号：只用于视觉分组，不影响逻辑
		api.GET("", ch.List)          // 列表：GET /api/channels
		api.POST("", ch.Create)       // 创建：POST /api/channels
		api.GET("/:id", ch.Get)       // 详情：GET /api/channels/3（:id 是路径参数占位符）
		api.PUT("/:id", ch.Update)    // 更新：PUT /api/channels/3
		api.DELETE("/:id", ch.Delete) // 删除：DELETE /api/channels/3
	}

	// AI 网关统一入口（第 4 步：转发）
	// 客户端只调这一个接口，背后转发到哪个渠道由网关决定
	relay := handler.NewRelayHandler(db)
	r.POST("/v1/chat/completions", relay.ChatCompletions)

	return r
}
