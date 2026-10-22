// Package handler 业务处理层
package handler

import (
	"bytes"         // 字节缓冲：把读出来的请求体重建成流
	"io"            // 流操作：读取请求体、复制响应体
	"log"           // 日志：记录转发链路
	"math/rand/v2"  // 随机数：加权随机选渠道（rand.IntN 是 Go 1.22+ 推荐写法）
	"net/http"
	"strings" // 字符串处理：去掉 base_url 末尾多余的 /
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"llm-gateway/internal/crypto"
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
	enc    crypto.Service // 加密服务：渠道 API Key 从库里取出来是密文，转发前先解密
}

// NewRelayHandler 构造器：注入 db 和加密服务，创建带超时的转发客户端
func NewRelayHandler(db *gorm.DB, enc crypto.Service) *RelayHandler {
	return &RelayHandler{
		db: db,
		client: &http.Client{
			Timeout: 30 * time.Second, // 30 秒没收到上游响应就放弃
		},
		enc: enc,
	}
}

// pickChannel 按权重加权随机选一个启用的渠道（W2 调度 + W3 排除已试）
//
// excluded：已经试过且失败的渠道 ID 集合（故障转移的核心：失败的不再选）
//
// 算法（前缀区间法）：
//   权重数组 [1, 2, 3] → 总权重 6 → 想象一条分成 1:2:3 三段的刻度尺
//   生成随机数 n ∈ [0, 6)，落在哪段就选谁：
//     n=0       → 渠道A（概率 1/6）
//     n=1,2     → 渠道B（概率 2/6）
//     n=3,4,5   → 渠道C（概率 3/6）
func (h *RelayHandler) pickChannel(excluded map[uint]struct{}) (model.Channel, error) {
	// ① 查出所有启用的渠道（enabled = true）
	var list []model.Channel
	if err := h.db.Where("enabled = ?", true).Find(&list).Error; err != nil {
		return model.Channel{}, err
	}

	// ② 过滤：跳过已试失败的渠道 + 权重保底（<1 按 1 算）
	candidates := make([]model.Channel, 0, len(list))
	for _, ch := range list {
		if _, tried := excluded[ch.ID]; tried { // 这个渠道已经试过且失败 → 排除
			continue
		}
		if ch.Weight < 1 { // 防止 0 权重渠道永远不会被选中
			ch.Weight = 1
		}
		candidates = append(candidates, ch)
	}
	if len(candidates) == 0 {
		return model.Channel{}, gorm.ErrRecordNotFound // 没有可用的了（全试完/无启用渠道）
	}

	// ③ 计算总权重，生成 [0, total) 随机数，逐段扣减定位区间
	total := 0
	for _, ch := range candidates {
		total += ch.Weight
	}
	n := rand.IntN(total)
	for _, ch := range candidates {
		n -= ch.Weight
		if n < 0 { // 掉进这个渠道的区间 → 选中它
			return ch, nil
		}
	}
	return candidates[len(candidates)-1], nil // 兜底：理论上不会走到这
}

// ChatCompletions POST /v1/chat/completions —— 网关统一入口
//
// 完整链路（这是 AI 网关的灵魂，面试常问）：
//
//	客户端 → 我们(8080) → 渠道上游(如 mock 9090) → 大模型
//	             └────── 转发请求 ──────┘
//	             ←────── 带回响应 ──────┘
func (h *RelayHandler) ChatCompletions(c *gin.Context) {
	// ① 读出客户端的请求体（只读一次，各次重试复用同一份 body）
	//    ⚠️ body 是"流"：只能读一次。这里读出来，后面要重建给每个候选上游
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		resp(c, 400, "读取请求体失败: "+err.Error(), nil)
		return
	}

	// ② 故障转移循环（W3 核心）：
	//    选渠道 → 转发 → 失败（网络错误/5xx）→ 记录已试 → 换下一个
	//    ⚠️ 关键边界（面试必讲）：
	//    只能在"拿到上游响应之前"切换渠道！
	//    一旦拿到响应（哪怕只是响应头），客户端就开始消费数据了——
	//    此时再换渠道重试，客户端会收到"半截回复 + 另一个渠道的回复"，比失败更糟
	//    所以：成功拿到响应 → 立即锁定并透传，绝不再切换（尤其 SSE 流式）
	tried := make(map[uint]struct{}) // 已尝试过的渠道集合（去重，防止无限循环）
	for {
		ch, err := h.pickChannel(tried)
		if err != nil {
			// 没有候选渠道了：区分"从来没渠道"和"全试完都失败"
			if len(tried) > 0 {
				resp(c, 502, "所有渠道均不可用", nil) // 网关尽力了，全是上游的锅
			} else {
				resp(c, 503, "无可用渠道", nil)
			}
			return
		}
		log.Printf("[relay] 尝试渠道 id=%d name=%s weight=%d → %s", ch.ID, ch.Name, ch.Weight, ch.BaseURL)

		// 构造发给上游的请求
		// 上游地址 = 渠道 base_url + /chat/completions
		// 例：base_url=http://localhost:9090/v1 → http://localhost:9090/v1/chat/completions
		upstreamURL := strings.TrimRight(ch.BaseURL, "/") + "/chat/completions"

		// ⚠️ 密钥解密（第 5 步：AES-GCM 落库加密配套动作）：
		// 库里存的 api_key 是密文，必须解成明文才能冒充"上游的合法用户"。
		// DecryptOrRaw 兼容旧明文数据：老库里未加密的 sk-xxx 会原样返回。
		// 解密失败（密钥不匹配/密文损坏）→ 记入已试，换下一个渠道，
		// 绝不把密文当密钥发给上游（那会 401，还暴露内部格式）。
		apiKey, err := h.enc.DecryptOrRaw(ch.APIKey)
		if err != nil {
			log.Printf("[relay] 渠道 id=%d 密钥解密失败: %v，尝试下一个", ch.ID, err)
			tried[ch.ID] = struct{}{}
			continue
		}

		req, err := http.NewRequest(http.MethodPost, upstreamURL, bytes.NewReader(body))
		if err != nil {
			resp(c, 500, "构造上游请求失败: "+err.Error(), nil)
			return
		}
		// 透传两个关键头：
		//   - Content-Type：告诉上游"我发的是 JSON"
		//   - Authorization：用解密后的渠道密钥冒充"我就是 DeepSeek 的合法用户"
		req.Header.Set("Content-Type", c.Request.Header.Get("Content-Type"))
		req.Header.Set("Authorization", "Bearer "+apiKey)

		// 发出请求（阻塞等上游响应）
		upResp, err := h.client.Do(req)
		if err != nil {
			// 网络层失败（连不上/超时）→ 记入已试，换下一个渠道
			log.Printf("[relay] 渠道 id=%d 网络失败: %v，尝试下一个", ch.ID, err)
			tried[ch.ID] = struct{}{}
			continue
		}

		// 上游给了明确响应，按状态码分流：
		if upResp.StatusCode >= 500 {
			// 5xx = 上游服务器故障 → 值得换渠道重试
			log.Printf("[relay] 渠道 id=%d 返回 %d，尝试下一个", ch.ID, upResp.StatusCode)
			upResp.Body.Close() // 显式关闭，释放连接（不用 defer——循环里会累积）
			tried[ch.ID] = struct{}{}
			continue
		}

		// 4xx/3xx/2xx：请求本身的结果（成功/参数错/限流），直接透传，不再重试
		//   为什么 4xx 不重试？400 是请求体坏、404 是路径错、401 是密钥错——
		//   换任何渠道结果都一样，重试纯属浪费
		//   ⚠️ 从此处起渠道已锁定：响应开始流向客户端，故障转移到此为止
		defer upResp.Body.Close()
		log.Printf("[relay] 渠道 id=%d 返回 %d，锁定并透传", ch.ID, upResp.StatusCode)

		// 把上游的响应"原样"搬回给客户端：
		//   状态码透传 + Content-Type 透传 + 响应体逐字节流式复制
		//
		//   ⚠️ 顺序坑（SSE 场景尤其致命）：
		//   必须先 Header 再 Status！因为 c.Status() 会立即把响应头发出去，
		//   一旦发出，后面的 Header 设置全部无效（HTTP 头在身体之前、且只发一次）
		//   —— 否则 Content-Type: text/event-stream 会丢失，客户端拿到普通文本
		c.Header("Content-Type", upResp.Header.Get("Content-Type")) // 先设头
		c.Status(upResp.StatusCode)                                 // 再写状态码（头随之发出）

		// 流式复制：边收边发，不用等全部读完
		// SSE 场景：上游每来一块（Flush 一次），io.Copy 就把这一块立即写给客户端
		io.Copy(c.Writer, upResp.Body)

		// SSE 场景收尾：手动 Flush 一次，确保最后一截数据也立即送达客户端
		if f, ok := c.Writer.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
}
