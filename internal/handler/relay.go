// Package handler 业务处理层
package handler

import (
	"bytes"   // 字节缓冲：把读出来的请求体重建成流
	"io"      // 流操作：读取请求体、复制响应体
	"log"     // 日志：记录转发链路
	"net/http"
	"strings" // 字符串处理：去掉 base_url 末尾多余的 /
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"llm-gateway/internal/model"
)

// RelayHandler AI 网关转发处理器（第 4 步：简单"一键中转"）
//
// 网关的本质就一句话：客户端只认我们一个入口，
// 我们把请求转给背后真正的模型服务商，再把回复带回来。
// 这个 handler 就是"中转站"的第一个版本。
type RelayHandler struct {
	db     *gorm.DB     // 数据库：查渠道配置
	client *http.Client // 转发专用的 HTTP 客户端（带超时，防止上游拖死我们）
}

// NewRelayHandler 构造器：注入 db，创建带超时的转发客户端
func NewRelayHandler(db *gorm.DB) *RelayHandler {
	return &RelayHandler{
		db: db,
		client: &http.Client{
			Timeout: 30 * time.Second, // 30 秒没收到上游响应就放弃
		},
	}
}

// ChatCompletions POST /v1/chat/completions —— 网关统一入口
//
// 完整链路（这是 AI 网关的灵魂，面试常问）：
//
//	客户端 → 我们(8080) → 渠道上游(如 mock 9090) → 大模型
//	             └────── 转发请求 ──────┘
//	             ←────── 带回响应 ──────┘
func (h *RelayHandler) ChatCompletions(c *gin.Context) {
	// ① 选渠道：先取"第一个启用的渠道"（简单版）
	//    ⚠️ 以后会换成"按权重/健康度调度"（W2 的调度器），这里先跑通链路
	var ch model.Channel
	if err := h.db.Where("enabled = ?", true).First(&ch).Error; err != nil {
		// 503 Service Unavailable：服务不可用（不是调用方的问题，是"没渠道可转发"）
		resp(c, 503, "无可用渠道", nil)
		return
	}
	log.Printf("[relay] 选中渠道 id=%d name=%s → %s", ch.ID, ch.Name, ch.BaseURL)

	// ② 读出客户端的请求体
	//    ⚠️ body 是"流"：只能读一次。这里读出来，后面要重建给上游
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		resp(c, 400, "读取请求体失败: "+err.Error(), nil)
		return
	}

	// ③ 构造发给上游的请求
	//    上游地址 = 渠道 base_url + /chat/completions
	//    例：base_url=http://localhost:9090/v1 → http://localhost:9090/v1/chat/completions
	upstreamURL := strings.TrimRight(ch.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequest(http.MethodPost, upstreamURL, bytes.NewReader(body))
	if err != nil {
		resp(c, 500, "构造上游请求失败: "+err.Error(), nil)
		return
	}
	// 透传两个关键头：
	//   - Content-Type：告诉上游"我发的是 JSON"
	//   - Authorization：用渠道的密钥冒充"我就是 DeepSeek 的合法用户"
	req.Header.Set("Content-Type", c.Request.Header.Get("Content-Type"))
	req.Header.Set("Authorization", "Bearer "+ch.APIKey)

	// ④ 真正发出请求（阻塞等上游响应）
	upResp, err := h.client.Do(req)
	if err != nil {
		// 502 Bad Gateway：网关连不上上游（mock 没启动？密钥错？）
		resp(c, 502, "上游不可达: "+err.Error(), nil)
		return
	}
	defer upResp.Body.Close()
	log.Printf("[relay] 上游返回 %d", upResp.StatusCode)

	// ⑤ 把上游的响应"原样"搬回给客户端：
	//    状态码透传（如 200） + Content-Type 透传 + 响应体逐字节复制
	c.Status(upResp.StatusCode)
	c.Header("Content-Type", upResp.Header.Get("Content-Type"))
	io.Copy(c.Writer, upResp.Body) // 流式复制：边收边发，不用等全部读完
}
