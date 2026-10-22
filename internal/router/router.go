// Package router 路由层：把"HTTP 请求路径"映射到"处理函数"
// 第 1 步只有健康检查；后续在这里注册：
//   /api/channels         渠道管理
//   /v1/chat/completions  AI 网关统一入口
package router

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// New 创建 gin 引擎并注册全部路由
func New() *gin.Engine {
	// gin.Default()：自带日志中间件 + panic 恢复中间件
	r := gin.Default()

	// 健康检查：部署后用来探测"服务是否存活"
	// 浏览器直接访问 http://localhost:8080/health 即可看到结果
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	return r
}
