// Package handler 业务处理层
package handler

import (
	"bufio"        // 逐行读 SSE 流（ReadBytes('\n') 天然处理"一条事件被拆成多个 TCP 包"）
	"bytes"        // 字节缓冲：把读出来的请求体重建成流
	"encoding/json" // JSON：解析/重组 SSE 事件
	"errors"       // 错误判断：识别 io.EOF 结束流
	"io"           // 流操作：读取请求体、复制响应体
	"log"          // 日志：记录转发链路
	"math/rand/v2" // 随机数：加权随机选渠道（rand.IntN 是 Go 1.22+ 推荐写法）
	"net/http"
	"strings" // 字符串处理：去掉 base_url 末尾多余的 /、判断 Content-Type
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

// ============================================================
// SSE 增量转换（W2 核心难点）
//
// 为什么不能 io.Copy 纯透传？
//   - 上游（Anthropic/Gemini）的流式格式和 OpenAI 不一样，纯透传 = 客户端看到别的格式
//   - 需要"边收边转"：每个 chunk 解析出增量字段，重组成 OpenAI 格式再写给客户端
//   - 打字的连贯性靠"来一块、转一块、Flush 一块"保住
//
// 这条链路的三个技术点（面试必讲）：
//   1. 拆包缓冲：TCP 流可能把一条 SSE 事件（data: ...\n\n）拆成两段到达，
//      所以不能按"读到的字节"直接转发，必须攒到空行（\n\n）才算一条完整事件
//   2. 增量映射：解析 data: 里的 JSON，只搬 delta.content / finish_reason / usage
//   3. 半途断连：客户端断开 → 写入报错 → 停止读取，上游的 context 随之取消
// ============================================================

// sseUsage OpenAI 流式响应的用量块（流末尾一般带一条）
type sseUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// sseChunk 一条 SSE data 事件解析出的"增量"（OpenAI 流式最小单元）
type sseChunk struct {
	index        int       // choices[0].index
	content      string    // delta.content（这一块新出现的字）
	finishReason string    // "" = 未结束；"stop" 等 = 流结束
	usage        *sseUsage // 可能为 nil（只有末尾一条带）
}

// parseSSEChunk 解析一条 data: 的 JSON 内容（data: 前缀已去掉）
func parseSSEChunk(data string) (*sseChunk, error) {
	var raw struct {
		Choices []struct {
			Index        int    `json:"index"`
			Delta        struct {
				Content string `json:"content"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *sseUsage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return nil, err
	}
	chunk := &sseChunk{}
	if len(raw.Choices) > 0 {
		chunk.index = raw.Choices[0].Index
		chunk.content = raw.Choices[0].Delta.Content
		if raw.Choices[0].FinishReason != nil {
			chunk.finishReason = *raw.Choices[0].FinishReason
		}
	}
	chunk.usage = raw.Usage
	return chunk, nil
}

// render 把解析出的增量重组成 OpenAI 格式的 SSE 事件字节
// （后续做 Anthropic → OpenAI 转换时，只需改这里的映射，框架不动）
func (c *sseChunk) render() []byte {
	// OpenAI 规范：增量放 delta.content；流结束时 delta 保持空对象 {}
	delta := map[string]any{}
	if c.content != "" {
		delta["content"] = c.content
	}
	choice := map[string]any{
		"index": c.index,
		"delta": delta,
	}
	if c.finishReason != "" { // finish_reason 只在流结束时出现，平时不写，保持事件精简
		choice["finish_reason"] = c.finishReason
	}
	payload := map[string]any{"choices": []any{choice}}
	if c.usage != nil {
		payload["usage"] = c.usage
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return []byte{} // 理论上不会失败（全是基础类型）
	}
	// SSE 事件格式：data: <json>\n\n
	return append([]byte("data: "), append(b, '\n', '\n')...)
}

// handleSSEEvent 处理一条完整 SSE 事件（已按空行切好）：
// 提取 data 行 → [DONE] 透传 → 解析 → 增量重组 → 写回 + Flush
func handleSSEEvent(c *gin.Context, event []byte) error {
	// ① 提取事件里所有 data: 行（SSE 规范：多条 data 用 \n 连接；忽略 event:/id:/retry:）
	lines := strings.Split(string(event), "\n")
	var datas []string
	for _, ln := range lines {
		ln = strings.TrimSuffix(ln, "\r") // 兼容 \r\n 换行的 SSE
		if strings.HasPrefix(ln, "data:") {
			datas = append(datas, strings.TrimSpace(strings.TrimPrefix(ln, "data:")))
		}
	}
	if len(datas) == 0 {
		return nil // 纯注释/心跳行，忽略
	}
	data := strings.Join(datas, "\n")

	// ② 结束标记 [DONE]：原样透传，这是 OpenAI 流结束的约定
	if data == "[DONE]" {
		if _, err := io.WriteString(c.Writer, "data: [DONE]\n\n"); err != nil {
			return err
		}
		flushWriter(c)
		return nil
	}

	// ③ 解析成结构化增量；解析失败（非 JSON 的 data）→ 容错原样透传，不打断流
	chunk, err := parseSSEChunk(data)
	if err != nil {
		if _, werr := c.Writer.Write(event); werr != nil {
			return werr
		}
		flushWriter(c)
		return nil
	}

	// ④ 增量转换：按 OpenAI 格式重组写回 + 立即 Flush（保打字机效果）
	if _, err := c.Writer.Write(chunk.render()); err != nil {
		return err // 客户端断连会在这里报错 → 上游 context 随之取消
	}
	flushWriter(c)
	return nil
}

// forwardSSE 逐事件消费上游 SSE 流，边收边转边推
//
// 核心：按行读、按空行切事件、攒够一条处理一条。
// ReadBytes('\n') 天然处理"一条事件被拆成多个网络包"——没读到 \n 就一直攒着。
func forwardSSE(c *gin.Context, upResp *http.Response) error {
	reader := bufio.NewReader(upResp.Body)
	var event []byte // 当前事件的缓冲（跨块攒）
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			event = append(event, line...)
			if len(bytes.TrimSpace(line)) == 0 { // 空行 = SSE 事件分隔符
				if err := handleSSEEvent(c, event); err != nil {
					return err
				}
				event = event[:0] // 清空缓冲，等下一条
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break // 上游流正常结束
			}
			return err
		}
	}
	// 流被上游截断时，最后可能剩一条没闭合的事件，也处理掉
	if len(bytes.TrimSpace(event)) > 0 {
		return handleSSEEvent(c, event)
	}
	return nil
}

// flushWriter 把缓冲内容立即推给客户端（打字机效果的来源）
func flushWriter(c *gin.Context) {
	if f, ok := c.Writer.(http.Flusher); ok {
		f.Flush()
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

		// 把上游的响应搬回给客户端，按类型分流：
		//   - SSE（text/event-stream）→ 增量转换：逐事件解析→重组→Flush
		//   - 其他（普通 JSON）→ 原样流式复制
		//
		//   ⚠️ 顺序坑（SSE 场景尤其致命）：
		//   必须先 Header 再 Status！因为 c.Status() 会立即把响应头发出去，
		//   一旦发出，后面的 Header 设置全部无效（HTTP 头在身体之前、且只发一次）
		//   —— 否则 Content-Type: text/event-stream 会丢失，客户端拿到普通文本
		c.Header("Content-Type", upResp.Header.Get("Content-Type")) // 先设头
		c.Status(upResp.StatusCode)                                 // 再写状态码（头随之发出）

		if strings.Contains(upResp.Header.Get("Content-Type"), "text/event-stream") {
			log.Printf("[relay] 渠道 id=%d SSE 流式响应，进入增量转换", ch.ID)
			if err := forwardSSE(c, upResp); err != nil {
				// 客户端断连 / 上游异常中断：记录但不再切换渠道（响应已锁定）
				log.Printf("[relay] 渠道 id=%d SSE 转发中断: %v", ch.ID, err)
			}
		} else {
			// 普通响应：边收边发，不用等全部读完
			io.Copy(c.Writer, upResp.Body)
			flushWriter(c) // 收尾 Flush，确保最后一截也立即送达
		}
		return
	}
}
