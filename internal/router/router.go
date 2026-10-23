// Package router 路由层：把"HTTP 请求路径"映射到"处理函数"
// 目前注册：/health、/api/channels（创建+列表）；后续加：
//
//	/v1/chat/completions  AI 网关统一入口
package router

import (
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"llm-gateway/config"
	"llm-gateway/internal/crypto"
	"llm-gateway/internal/handler"
	"llm-gateway/internal/ratelimit"
)

// New 创建 gin 引擎并注册全部路由
// 参数 db：数据库句柄；enc：加密服务（API Key 落库加密/转发解密）；cfg：服务配置
// 路由层把依赖注入给业务处理器（依赖往下传递）
func New(db *gorm.DB, enc crypto.Service, cfg config.Config) *gin.Engine {
	// gin.Default()：自带日志中间件 + panic 恢复中间件
	r := gin.Default()

	// 健康检查：部署后用来探测"服务是否存活"
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	// 渠道配置缓存（W2 热更新）：原子快照无锁读；CRUD 主动刷新 + 30s 轮询兜底
	cache := handler.NewChannelCache(db, 30*time.Second)
	cache.Start()

	// 渠道管理（第 3 步：完整 CRUD）
	ch := handler.NewChannelHandler(db, enc, cache) // 依赖注入：把 db、加密服务、缓存传给渠道处理器
	api := r.Group("/api/channels")                 // 路由分组：组内路径自动带 /api/channels 前缀
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
	var limiter ratelimit.Limiter
	if cfg.RateLimitEnabled {
		l, err := ratelimit.New(cfg.RateLimitStore, cfg.RateLimitPerMin, time.Minute, cfg.RedisAddr)
		if err != nil {
			log.Printf("⚠️ 限流器初始化失败，本次启动限流关闭: %v", err)
		} else {
			limiter = l
			log.Printf("限流已开启: store=%s limit=%d/分钟", cfg.RateLimitStore, cfg.RateLimitPerMin)
		}
	}
	relay := handler.NewRelayHandler(db, enc, cache, limiter)
	r.POST("/v1/chat/completions", relay.ChatCompletions)

	// 用量看板 API（W3-③）：基于 usage_logs 流水表查询时聚合
	//   GET /api/usage/summary   总览卡片
	//   GET /api/usage/breakdown 分组排行（?group=model|channel）
	//   GET /api/usage/daily     按天趋势
	dash := handler.NewUsageDashboard(db)
	ug := r.Group("/api/usage")
	{
		ug.GET("/summary", dash.Summary)
		ug.GET("/breakdown", dash.Breakdown)
		ug.GET("/daily", dash.Daily)
	}

	// 后台健康探测（W2 熔断配套）：每 15 秒扫描熔断渠道，探活成功自动恢复
	prober := handler.NewHealthProber(db, relay.Breakers(), 15*time.Second)
	prober.Start()

	return r
}
