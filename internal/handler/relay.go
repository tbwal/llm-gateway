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
	"net"          // 连接层：Dialer 超时（连接/KeepAlive 分层超时）
	"net/http"
	"strings" // 字符串处理：去掉 base_url 末尾多余的 /、判断 Content-Type
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"llm-gateway/internal/crypto"
	"llm-gateway/internal/model"
	"llm-gateway/internal/ratelimit"
)

// RelayHandler AI 网关转发处理器（第 4 步：简单"一键中转"）
//
// 网关的本质就一句话：客户端只认我们一个入口，
// 我们把请求转给背后真正的模型服务商，再把回复带回来。
// 这个 handler 就是"中转站"的第一个版本。
type RelayHandler struct {
	db     *gorm.DB     // 数据库：查渠道配置
	cache  *channelCache // 渠道配置缓存（无锁快照读，替代每请求查库）
	client *http.Client // 转发专用的 HTTP 客户端（连接池 + 分层超时）
	enc    crypto.Service // 加密服务：渠道 API Key 从库里取出来是密文，转发前先解密
	breakers *circuitBreakers // 熔断器注册表：跨请求统计渠道连续失败，自动摘除/恢复
	limiter  ratelimit.Limiter // W3 限流器：滑动窗口（memory 本地 / redis 分布式）
}

// NewRelayHandler 构造器：注入 db、加密服务、渠道缓存、限流器；创建连接池调优的转发客户端和熔断器
func NewRelayHandler(db *gorm.DB, enc crypto.Service, cache *channelCache, limiter ratelimit.Limiter) *RelayHandler {
	// 连接池调优（W2 连接池/热更新：连接侧）
	// 默认 http.DefaultTransport 的 MaxIdleConnsPerHost=2——
	// 高并发转发时并发连接无法复用，每请求重建 TCP 连接（慢 + 耗资源）。
	// 这里显式调大空闲连接数，配合分层超时：
	//   - DialContext.Timeout          连接超时（10s 建不上连接就放弃）
	//   - ResponseHeaderTimeout        响应头超时（30s 上游还没开始吐就放弃）
	//   - Client.Timeout=0             不设整体超时（SSE 流式是"无限长"的，
	//                                   整体超时会掐断已开始的流——上游思考 40s 就断）
	transport := &http.Transport{
		MaxIdleConns:          100,               // 全局最大空闲连接
		MaxIdleConnsPerHost:   10,                // 单上游最大空闲连接（默认 2 → 10）
		IdleConnTimeout:       90 * time.Second,  // 空闲连接超过 90s 回收
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,  // 等响应头上限（流开始后不限制）
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second, // 连接超时
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	return &RelayHandler{
		db:   db,
		cache: cache,
		client: &http.Client{
			Transport: transport,
			Timeout:   0, // 禁用整体超时（SSE 长流不被掐断）；分层超时已兜住"上游不理我们"
		},
		enc:      enc,
		breakers: newCircuitBreakers(),
		limiter:  limiter,
	}
}

// Breakers 暴露熔断器注册表（供后台健康探测扫描 OPEN 渠道）
func (h *RelayHandler) Breakers() *circuitBreakers {
	return h.breakers
}

// rateLimitKey 提取限流的"调用方标识"
// 优先级：Authorization: Bearer <key> → X-User-Key → 客户端 IP（兜底）
// 真实网关里这里是 AccessKey/团队 ID（鉴权后取），本网关暂未做鉴权，
// 用调用方自报的标识做限流维度（面试讲：接入鉴权后替换为可信身份即可）
func (h *RelayHandler) rateLimitKey(c *gin.Context) string {
	if auth := c.GetHeader("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return "user:" + strings.TrimPrefix(auth, "Bearer ")
	}
	if k := c.GetHeader("X-User-Key"); k != "" {
		return "user:" + k
	}
	return "ip:" + c.ClientIP()
}

// pickChannel 按权重加权随机选一个启用的渠道（W2 调度 + W3 排除已试）
//
// excluded：已经试过且失败的渠道 ID 集合（故障转移的核心：失败的不再选）
// modelName：客户端请求里的模型名（路由层：渠道声明支持该模型才进候选池）
//
// 算法（前缀区间法）：
//   权重数组 [1, 2, 3] → 总权重 6 → 想象一条分成 1:2:3 三段的刻度尺
//   生成随机数 n ∈ [0, 6)，落在哪段就选谁：
//     n=0       → 渠道A（概率 1/6）
//     n=1,2     → 渠道B（概率 2/6）
//     n=3,4,5   → 渠道C（概率 3/6）
func (h *RelayHandler) pickChannel(excluded map[uint]struct{}, modelName string) (model.Channel, error) {
	// ① 从内存快照读启用渠道（无锁；替代每请求查库——配置热更新的读路径）
	list := h.cache.List()

	// ② 三层过滤：已试失败排除 + 按模型路由 + 熔断状态
	//    （熔断：连续失败 N 次的渠道被摘除，冷却期/探活恢复后才重新入池）
	candidates := make([]model.Channel, 0, len(list))
	for _, ch := range list {
		if _, tried := excluded[ch.ID]; tried { // 这个渠道已经试过且失败 → 排除
			continue
		}
		// 路由层：渠道声明了 models 列表，但客户端请求的模型不在其中 → 不参与调度
		// （渠道 models 为空 = 支持所有模型，兼容旧渠道无需迁移）
		if modelName != "" && ch.Models != "" && !containsModel(ch.Models, modelName) {
			continue
		}
		// 熔断层：OPEN（冷却中）或 HALF_OPEN（探测已放行）→ 跳过
		if !h.breakers.allow(ch.ID, time.Now()) {
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

// containsModel 判断渠道的 models 列表是否包含目标模型
// 匹配规则：逗号分隔逐项精确匹配（允许多余空白）；"*" 表示支持所有模型
func containsModel(models, target string) bool {
	for _, m := range strings.Split(models, ",") {
		m = strings.TrimSpace(m)
		if m == "*" || m == target {
			return true
		}
	}
	return false
}

// isAnthropicProvider 判断渠道上游是不是 Anthropic（Claude）
// provider 字段取值约定：openai / deepseek / anthropic（历史也叫 claude）
func isAnthropicProvider(provider string) bool {
	return provider == "anthropic" || provider == "claude"
}

// convertToAnthropic 请求侧转换：客户端 OpenAI 请求 → Anthropic 请求
//
// 转换点（和 Anthropic 协议的差异一一对应）：
//   - system 消息从 messages 数组拆出，变成顶层 system 字段
//   - max_tokens 补齐（Anthropic 必填，客户端没传给默认值 1024）
//   - messages 只保留 user / assistant 角色
//   - 模型名、stream 原样透传
func convertToAnthropic(body []byte) ([]byte, error) {
	var openai struct {
		Model     string `json:"model"`
		Stream    bool   `json:"stream"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &openai); err != nil {
		return nil, err
	}

	// ① system 拆出（Anthropic 的 system 是顶层字段，messages 里没有）
	var systemParts []string
	messages := make([]map[string]any, 0, len(openai.Messages))
	for _, m := range openai.Messages {
		if m.Role == "system" {
			systemParts = append(systemParts, m.Content)
			continue
		}
		messages = append(messages, map[string]any{"role": m.Role, "content": m.Content})
	}

	// ② max_tokens 补齐（Anthropic 必填）
	maxTokens := openai.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 1024
	}

	// ③ 组装 Anthropic 请求
	anthropic := map[string]any{
		"model":      openai.Model,
		"max_tokens": maxTokens,
		"messages":   messages,
		"stream":     openai.Stream,
	}
	if len(systemParts) > 0 {
		anthropic["system"] = strings.Join(systemParts, "\n")
	}
	return json.Marshal(anthropic)
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
//
// 双协议解析（W2-② 协议转换的核心）：
//   - OpenAI 格式：{"choices":[{"delta":{"content":...},"finish_reason":...}]}
//   - Anthropic 格式：{"type":"content_block_delta","delta":{"type":"text_delta","text":...}}
//                    {"type":"message_delta","delta":{"stop_reason":"end_turn"}}
// 先探测 type 字段判断协议，再走各自的提取逻辑；输出统一是 sseChunk（语义层）
func parseSSEChunk(data string) (*sseChunk, error) {
	// ① 先看有没有 type 字段：有 = Anthropic 事件；没有 = OpenAI 格式
	var probe struct {
		Type  string          `json:"type"`
		Delta json.RawMessage `json:"delta"`
		Index int             `json:"index"`
	}
	if err := json.Unmarshal([]byte(data), &probe); err != nil {
		return nil, err
	}

	chunk := &sseChunk{index: probe.Index}

	// ② Anthropic 事件分支
	if probe.Type != "" {
		switch probe.Type {
		case "content_block_delta":
			// delta: {"type":"text_delta","text":"你好"}
			// 只认文本增量；tool_use 等其他类型暂无增量（将来扩展）
			var d struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(probe.Delta, &d); err != nil {
				return nil, err
			}
			if d.Type == "text_delta" {
				chunk.content = d.Text
			}
		case "message_delta":
			// delta: {"stop_reason":"end_turn","stop_sequence":null}
			var d struct {
				StopReason string `json:"stop_reason"`
			}
			if err := json.Unmarshal(probe.Delta, &d); err != nil {
				return nil, err
			}
			chunk.finishReason = mapAnthropicStopReason(d.StopReason)
		}
		// message_start / content_block_start / content_block_stop / message_stop：
		// 不产生增量，返回空 chunk（由 handleSSEEvent 跳过，不写回客户端）
		return chunk, nil
	}

	// ③ OpenAI 格式分支
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

// mapAnthropicStopReason Anthropic 结束原因 → OpenAI 语义
//   end_turn → stop（正常结束）；max_tokens → length（截断）；
//   tool_use → tool_calls（工具调用）；stop_sequence → stop
func mapAnthropicStopReason(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default: // end_turn / stop_sequence / 空
		return "stop"
	}
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
// handleSSEEvent 处理一条完整 SSE 事件（已攒到空行）
// 返回值 done=true 表示"已向客户端发出 [DONE] 收尾标记"
// （forwardSSE 靠它判断要不要在 EOF 时补发 [DONE]：Anthropic 上游没有 [DONE]）
func handleSSEEvent(c *gin.Context, event []byte) (bool, error) {
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
		return false, nil // 纯注释/心跳行，忽略
	}
	data := strings.Join(datas, "\n")

	// ② 结束标记 [DONE]：原样透传，这是 OpenAI 流结束的约定
	if data == "[DONE]" {
		if _, err := io.WriteString(c.Writer, "data: [DONE]\n\n"); err != nil {
			return false, err
		}
		flushWriter(c)
		return true, nil
	}

	// ③ 解析成结构化增量；解析失败（非 JSON 的 data）→ 容错原样透传，不打断流
	chunk, err := parseSSEChunk(data)
	if err != nil {
		if _, werr := c.Writer.Write(event); werr != nil {
			return false, werr
		}
		flushWriter(c)
		return false, nil
	}

	// ④ 无增量事件直接跳过：Anthropic 的 message_start / content_block_start /
	//    content_block_stop / message_stop 不产生任何内容，不透传给客户端
	if chunk.content == "" && chunk.finishReason == "" && chunk.usage == nil {
		return false, nil
	}

	// ⑤ 增量转换：按 OpenAI 格式重组写回 + 立即 Flush（保打字机效果）
	if _, err := c.Writer.Write(chunk.render()); err != nil {
		return false, err // 客户端断连会在这里报错 → 上游 context 随之取消
	}
	flushWriter(c)
	return false, nil
}

// forwardSSE 逐事件消费上游 SSE 流，边收边转边推
//
// 核心：按行读、按空行切事件、攒够一条处理一条。
// ReadBytes('\n') 天然处理"一条事件被拆成多个网络包"——没读到 \n 就一直攒着。
func forwardSSE(c *gin.Context, upResp *http.Response) error {
	reader := bufio.NewReader(upResp.Body)
	var event []byte // 当前事件的缓冲（跨块攒）
	sentDone := false // 是否已向客户端发过 [DONE]（OpenAI 上游自带；Anthropic 上游没有，EOF 时补）
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			event = append(event, line...)
			if len(bytes.TrimSpace(line)) == 0 { // 空行 = SSE 事件分隔符
				done, herr := handleSSEEvent(c, event)
				if herr != nil {
					return herr
				}
				sentDone = sentDone || done
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
		done, herr := handleSSEEvent(c, event)
		if herr != nil {
			return herr
		}
		sentDone = sentDone || done
	}
	// 协议补齐：Anthropic 流用 message_stop 结束、没有 [DONE]，
	// 客户端（OpenAI SDK）等的是 data: [DONE] 收尾 → EOF 时补发一个
	if !sentDone {
		if _, err := io.WriteString(c.Writer, "data: [DONE]\n\n"); err != nil {
			return err
		}
		flushWriter(c)
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
	// ⓪ W3 限流：在解析 body 之前先掐住（省得为被拒绝的请求白做 JSON 解析）
	// key 取调用方标识（Authorization Bearer / X-User-Key），兜底客户端 IP
	if h.limiter != nil {
		key := h.rateLimitKey(c)
		ok, err := h.limiter.Allow(key)
		if err != nil {
			// fail-open：限流器自身故障（如 Redis 连不上）时放行，
			// 保证"限流组件挂了"不会拖垮整个网关（可用性优先），但记日志可观测
			log.Printf("[ratelimit] 限流器故障，fail-open 放行（key=%s）: %v", key, err)
		} else if !ok {
			resp(c, 429, "请求过于频繁，请稍后再试（滑动窗口内已达上限）", nil)
			return
		}
	}

	// ① 读出客户端的请求体（只读一次，各次重试复用同一份 body）
	//    ⚠️ body 是"流"：只能读一次。这里读出来，后面要重建给每个候选上游
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		resp(c, 400, "读取请求体失败: "+err.Error(), nil)
		return
	}

	// 解析客户端请求里的 model（路由层依据：按模型匹配候选渠道）
	// 例：{"model":"claude-3-5-sonnet",...} → 只有声明支持该模型的渠道才进候选池
	var reqMeta struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &reqMeta)

	// ② 故障转移循环（W3 核心）：
	//    选渠道 → 转发 → 失败（网络错误/5xx）→ 记录已试 → 换下一个
	//    ⚠️ 关键边界（面试必讲）：
	//    只能在"拿到上游响应之前"切换渠道！
	//    一旦拿到响应（哪怕只是响应头），客户端就开始消费数据了——
	//    此时再换渠道重试，客户端会收到"半截回复 + 另一个渠道的回复"，比失败更糟
	//    所以：成功拿到响应 → 立即锁定并透传，绝不再切换（尤其 SSE 流式）
	tried := make(map[uint]struct{}) // 已尝试过的渠道集合（去重，防止无限循环）
	for {
		ch, err := h.pickChannel(tried, reqMeta.Model) // 路由层：按客户端 model 过滤候选渠道
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
		// 上游地址 = 渠道 base_url + 协议路径：
		//   OpenAI 系 → /chat/completions；Anthropic → /messages
		// 例：base_url=http://localhost:9090/v1 → http://localhost:9090/v1/chat/completions
		upstreamURL := strings.TrimRight(ch.BaseURL, "/")
		anthropic := isAnthropicProvider(ch.Provider)
		if anthropic {
			upstreamURL += "/messages"
		} else {
			upstreamURL += "/chat/completions"
		}

		// ⚠️ 密钥解密（第 5 步：AES-GCM 落库加密配套动作）：
		// 库里存的 api_key 是密文，必须解成明文才能冒充"上游的合法用户"。
		// DecryptOrRaw 兼容旧明文数据：老库里未加密的 sk-xxx 会原样返回。
		// 解密失败（密钥不匹配/密文损坏）→ 记入已试，换下一个渠道，
		// 绝不把密文当密钥发给上游（那会 401，还暴露内部格式）。
		apiKey, err := h.enc.DecryptOrRaw(ch.APIKey)
		if err != nil {
			log.Printf("[relay] 渠道 id=%d 密钥解密失败: %v，尝试下一个", ch.ID, err)
			tried[ch.ID] = struct{}{}
			h.breakers.recordFailure(ch.ID, time.Now()) // 密钥坏了也算渠道故障，累计熔断
			continue
		}

		// 请求体：claude 渠道需要把 OpenAI 请求"翻译"成 Anthropic 格式
		// （system 拆出、max_tokens 补齐、messages 角色过滤），其余渠道原样透传
		upBody := body
		if anthropic {
			upBody, err = convertToAnthropic(body)
			if err != nil {
				log.Printf("[relay] 渠道 id=%d 请求转换失败: %v，尝试下一个", ch.ID, err)
				tried[ch.ID] = struct{}{}
				continue
			}
		}

		req, err := http.NewRequest(http.MethodPost, upstreamURL, bytes.NewReader(upBody))
		if err != nil {
			resp(c, 500, "构造上游请求失败: "+err.Error(), nil)
			return
		}
		// 透传关键头：
		//   - Content-Type：告诉上游"我发的是 JSON"
		//   - 鉴权头按协议分流：OpenAI 系用 Authorization: Bearer；
		//     Anthropic 用 x-api-key + anthropic-version（缺了会 401）
		req.Header.Set("Content-Type", c.Request.Header.Get("Content-Type"))
		if anthropic {
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		} else {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}

		// 发出请求（阻塞等上游响应）
		upResp, err := h.client.Do(req)
		if err != nil {
			// 网络层失败（连不上/超时）→ 记入已试 + 熔断计数，换下一个渠道
			log.Printf("[relay] 渠道 id=%d 网络失败: %v，尝试下一个", ch.ID, err)
			tried[ch.ID] = struct{}{}
			h.breakers.recordFailure(ch.ID, time.Now())
			continue
		}

		// 上游给了明确响应，按状态码分流：
		if upResp.StatusCode >= 500 {
			// 5xx = 上游服务器故障 → 值得换渠道重试；同时累计熔断计数
			log.Printf("[relay] 渠道 id=%d 返回 %d，尝试下一个", ch.ID, upResp.StatusCode)
			upResp.Body.Close() // 显式关闭，释放连接（不用 defer——循环里会累积）
			tried[ch.ID] = struct{}{}
			h.breakers.recordFailure(ch.ID, time.Now())
			continue
		}

		// 4xx/3xx/2xx：请求本身的结果（成功/参数错/限流），直接透传，不再重试
		//   为什么 4xx 不重试？400 是请求体坏、404 是路径错、401 是密钥错——
		//   换任何渠道结果都一样，重试纯属浪费
		//   ⚠️ 从此处起渠道已锁定：响应开始流向客户端，故障转移到此为止
		defer upResp.Body.Close()
		h.breakers.recordSuccess(ch.ID) // 拿到响应 = 渠道是通的，记成功清失败计数
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
		} else if anthropic {
			// 非流式 + Anthropic 渠道：响应是 Anthropic 格式（content 数组 + stop_reason），
			// 客户端（OpenAI SDK）不认——必须转成 OpenAI 格式再写回
			log.Printf("[relay] 渠道 id=%d 非流式响应，Anthropic → OpenAI 转换", ch.ID)
			if err := convertAnthropicOnce(c, upResp); err != nil {
				log.Printf("[relay] 渠道 id=%d 非流式转换失败: %v", ch.ID, err)
			}
		} else {
			// 普通响应：边收边发，不用等全部读完
			io.Copy(c.Writer, upResp.Body)
			flushWriter(c) // 收尾 Flush，确保最后一截也立即送达
		}
		return
	}
}

// convertAnthropicOnce 非流式响应转换：Anthropic JSON → OpenAI 格式
//
// 非流式时 Anthropic 返回的是"完整消息对象"（非流式的结构与流式 6 种事件拼出的结果一致）：
//   Anthropic: {"id":"msg_xxx","type":"message","content":[{"type":"text","text":"..."}],
//               "stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":10}}
//   OpenAI:    {"id":"...","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"..."},
//               "finish_reason":"stop"}],"usage":{...}}
func convertAnthropicOnce(c *gin.Context, upResp *http.Response) error {
	var anth struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(upResp.Body).Decode(&anth); err != nil {
		return err
	}

	// ① 文本块拼接（Anthropic 的 content 是数组，可能多个 text 块）
	var text string
	for _, blk := range anth.Content {
		if blk.Type == "text" {
			text += blk.Text
		}
	}

	// ② 组装 OpenAI 非流式响应（id 复用上游的，保留可追溯性）
	openai := map[string]any{
		"id":      anth.ID,
		"object":  "chat.completion",
		"model":   anth.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": mapAnthropicStopReason(anth.StopReason),
		}},
	}
	if anth.Usage.InputTokens > 0 || anth.Usage.OutputTokens > 0 {
		openai["usage"] = map[string]any{
			"prompt_tokens":     anth.Usage.InputTokens,
			"completion_tokens": anth.Usage.OutputTokens,
			"total_tokens":      anth.Usage.InputTokens + anth.Usage.OutputTokens,
		}
	}
	return json.NewEncoder(c.Writer).Encode(openai)
}
