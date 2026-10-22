// Package handler 业务处理层
package handler

import (
	"bytes"      // 字节缓冲：把读出来的请求体重建成流
	"io"         // 流操作：读取请求体、复制响应体
	"log"        // 日志：记录转发链路
	"math/rand/v2" // 随机数：加权随机选渠道（rand.IntN 是 Go 1.22+ 推荐写法）
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

// pickChannel 按权重加权随机选一个启用的渠道（W2：调度器雏形）
//
// 算法（前缀区间法）：
//   权重数组 [1, 2, 3] → 总权重 6 → 想象一条分成 1:2:3 三段的刻度尺
//   生成随机数 n ∈ [0, 6)，落在哪段就选谁：
//     n=0       → 渠道A（概率 1/6）
//     n=1,2     → 渠道B（概率 2/6）
//     n=3,4,5   → 渠道C（概率 3/6）
//
// 为什么用加权随机而不是轮询？真实网关里渠道有配额差异（贵的少调、便宜的多调），
// 权重就是"配额比例"。轮询是"均等分配"，加权随机才是"按比例分配"。
func (h *RelayHandler) pickChannel() (model.Channel, error) {
	// ① 查出所有启用的渠道（enabled = true）
	var list []model.Channel
	if err := h.db.Where("enabled = ?", true).Find(&list).Error; err != nil {
		return model.Channel{}, err
	}
	if len(list) == 0 {
		return model.Channel{}, gorm.ErrRecordNotFound // 复用"找不到记录"的错误语义
	}

	// ② 计算总权重（weight < 1 按 1 算，防止 0 权重渠道永远不会被选中）
	total := 0
	for _, ch := range list {
		if ch.Weight < 1 {
			ch.Weight = 1
		}
		total += ch.Weight
	}

	// ③ 生成 [0, total) 的随机数，逐段扣减定位区间
	n := rand.IntN(total) // Go 1.22+ 的推荐写法（旧版是 rand.Intn，已废弃）
	for _, ch := range list {
		n -= ch.Weight
		if n < 0 { // 掉进这个渠道的区间 → 选中它
			return ch, nil
		}
	}
	return list[len(list)-1], nil // 兜底：理论上不会走到这
}

// ChatCompletions POST /v1/chat/completions —— 网关统一入口
//
// 完整链路（这是 AI 网关的灵魂，面试常问）：
//
//	客户端 → 我们(8080) → 渠道上游(如 mock 9090) → 大模型
//	             └────── 转发请求 ──────┘
//	             ←────── 带回响应 ──────┘
func (h *RelayHandler) ChatCompletions(c *gin.Context) {
	// ① 选渠道（W2 升级）：按权重加权随机选一个启用渠道
	//    W3 会进一步叠加"健康度"（故障转移：挂了的上游自动降权/剔除）
	var ch model.Channel
	ch, err := h.pickChannel()
	if err != nil {
		// 503 Service Unavailable：服务不可用（不是调用方的问题，是"没渠道可转发"）
		resp(c, 503, "无可用渠道", nil)
		return
	}
	log.Printf("[relay] 加权选中渠道 id=%d name=%s weight=%d → %s", ch.ID, ch.Name, ch.Weight, ch.BaseURL)

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
	//    状态码透传（如 200）+ Content-Type 透传 + 响应体逐字节流式复制
	//
	//    ⚠️ 顺序坑（SSE 场景尤其致命）：
	//    必须先 Header 再 Status！因为 c.Status() 会立即把响应头发出去，
	//    一旦发出，后面的 Header 设置全部无效（HTTP 头在身体之前、且只发一次）
	//    —— 否则 Content-Type: text/event-stream 会丢失，客户端拿到普通文本，
	//    流式效果直接坏掉
	c.Header("Content-Type", upResp.Header.Get("Content-Type")) // 先设头
	c.Status(upResp.StatusCode)                                 // 再写状态码（头随之发出）

	// 流式复制：边收边发，不用等全部读完
	// SSE 场景：上游每来一块（Flush 一次），io.Copy 就把这一块立即写给客户端
	io.Copy(c.Writer, upResp.Body)

	// SSE 场景收尾：手动 Flush 一次，确保最后一截数据也立即送达客户端
	// （gin 的 Writer 通常直接写底层连接，但对某些中间件/代理，最后一块可能还卡在缓冲里）
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
	}
}
