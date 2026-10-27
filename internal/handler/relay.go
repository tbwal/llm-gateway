// Package handler 业务处理层
package handler

import (
	"bufio"        // 逐行读 SSE 流（ReadBytes('\n') 天然处理"一条事件被拆成多个 TCP 包"）
	"bytes"        // 字节缓冲：把读出来的请求体重建成流
	"encoding/json" // JSON：解析/重组 SSE 事件
	"errors"       // 错误判断：识别 io.EOF 结束流
	"io"           // 流操作：读取请求体、复制响应体
	"math/rand/v2" // 随机数：加权随机选渠道（rand.IntN 是 Go 1.22+ 推荐写法）
	"net"          // 连接层：Dialer 超时（连接/KeepAlive 分层超时）
	"net/http"
	"strconv" // 字符串转整数：限流 key 拼 akID
	"strings" // 字符串处理：去掉 base_url 末尾多余的 /、判断 Content-Type
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"llm-gateway/internal/crypto"
	"llm-gateway/internal/model"
	"llm-gateway/internal/ratelimit"
	"llm-gateway/internal/usage"
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
// 优先级：可信身份（鉴权中间件注入的 AccessKey id）→ 客户端自报（兼容无鉴权链路）→ 客户端 IP
// ⚠️ 接入 AccessKey 鉴权后，限流维度就是"可信身份"：同一个 key 不管从哪台机器调
//    都算同一个人，真正把"限流"作用到"调用方"而不是"来源 IP"
func (h *RelayHandler) rateLimitKey(c *gin.Context) string {
	if akID, _ := AccessKeyFromCtx(c); akID > 0 {
		return "ak:" + strconv.FormatUint(uint64(akID), 10)
	}
	if auth := c.GetHeader("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return "user:" + strings.TrimPrefix(auth, "Bearer ")
	}
	if k := c.GetHeader("X-User-Key"); k != "" {
		return "user:" + k
	}
	return "ip:" + c.ClientIP()
}

// auditMeta 从 gin.Context 提取审计字段：链路 ID / 调用方 AccessKey / 客户端 IP
// 成功与失败落库共用——日志页能回答"谁（key）从哪（IP）调了什么（model），链路 ID 是多少"
func auditMeta(c *gin.Context) (reqID string, akID uint, akName string, ip string) {
	return RequestIDFromCtx(c), AccessKeyFromCtxID(c), AccessKeyNameFromCtx(c), c.ClientIP()
}

// relayStartKey / relayFirstRespKey 请求计时存 context 的 key
// （relayAttemptsKey 与尝试链辅助函数在 attempts.go）
const (
	relayStartKey     = "relayStart"
	relayFirstRespKey = "relayFirstResp"
)

// markFirstResponse 记录"首响应"时刻（只记一次）
// 流式 = 首个增量 token 写出；非流式 = 上游响应到达（Do 返回）。
// ⚠️ 存的是"入口 → 首 token"的毫秒差值（TTFT），不是时间点——
//    fallback：先掏 start 算 time.Since，存 int64。失败分支不调用（首响应=0）
func markFirstResponse(c *gin.Context) {
	if _, ok := c.Get(relayFirstRespKey); !ok {
		var ttftMs int64
		if v, ok := c.Get(relayStartKey); ok {
			if t, ok := v.(time.Time); ok {
				ttftMs = time.Since(t).Milliseconds()
			}
		}
		c.Set(relayFirstRespKey, ttftMs)
	}
}

// relayTiming 从 context 取请求计时：
//   durationMs   总耗时（入口 → 落库时刻，毫秒）
//   firstRespMs  TTFT（入口 → 首 token/首响应，毫秒）——mark 时已算好存差值
// 两者差 = 首 token 之后到请求结束的时间（后续生成 + 传输），可定位"慢在哪一段"
func relayTiming(c *gin.Context) (durationMs, firstRespMs int64) {
	if v, ok := c.Get(relayStartKey); ok {
		if t, ok := v.(time.Time); ok {
			durationMs = time.Since(t).Milliseconds()
		}
	}
	if v, ok := c.Get(relayFirstRespKey); ok {
		if d, ok := v.(int64); ok {
			firstRespMs = d
		}
	}
	return durationMs, firstRespMs
}

// recordUsage W3-② 计费落库：把一次请求的 token 用量写成流水
//
// 设计原则（面试讲点）：**流水表 + 查询时聚合**
//   - 每次请求 INSERT 一条（只增不减，可审计）
//   - 聚合（SUM / 按渠道 / 按模型 / 按天）由用量看板查询时算
//   - 不维护"每渠道一个计数行 + 每请求 UPDATE"：那是热点行锁，
//     所有请求抢同一行写锁，并发越高越慢；INSERT 流水没有锁竞争
func (h *RelayHandler) recordUsage(c *gin.Context, ch model.Channel, modelName string, promptTokens, completionTokens int) {
	if promptTokens == 0 && completionTokens == 0 {
		return // 什么都拿不到就不记（避免一堆 0 流水）
	}
	cost := usage.CalcCost(modelName, promptTokens, completionTokens)
	reqID, akID, akName, ip := auditMeta(c)
	durationMs, firstRespMs := relayTiming(c)
	logEntry := model.UsageLog{
		RequestID:        reqID,
		ChannelID:        ch.ID,
		ChannelName:      ch.Name,
		Provider:         ch.Provider,
		Model:            modelName,
		AccessKeyID:      akID,
		AccessKeyName:    akName,
		ClientIP:         ip,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
		Cost:             cost,
		Status:           200, // 能走到落账 = 转发成功（HTTP 200 语义）
		DurationMs:       durationMs,
		FirstResponseMs:  firstRespMs,
		AttemptsJSON:     attemptsFromCtx(c), // 故障转移链：单次成功=1条；换过渠道=多条
	}
	if err := h.db.Create(&logEntry).Error; err != nil {
		RequestLogf(c, "[usage] 计费落库失败: %v", err)
		return
	}
	RequestLogf(c, "[usage] req=%s 渠道 id=%d model=%s tokens=%d+%d cost=%.4f元", reqID, ch.ID, modelName, promptTokens, completionTokens, cost)
}

// logUsageFailure 包级失败落库（relay 转发链路与 AccessKeyAuth 中间件共用）
func logUsageFailure(db *gorm.DB, c *gin.Context, ch model.Channel, modelName string, status int, errMsg string) {
	reqID, akID, akName, ip := auditMeta(c)
	durationMs, firstRespMs := relayTiming(c)
	logEntry := model.UsageLog{
		RequestID:        reqID,
		ChannelID:        ch.ID,
		ChannelName:      ch.Name,
		Provider:         ch.Provider,
		Model:            modelName,
		AccessKeyID:      akID,
		AccessKeyName:    akName,
		ClientIP:         ip,
		Status:           status,
		Error:            errMsg,
		DurationMs:       durationMs,
		FirstResponseMs:  firstRespMs, // 失败分支未 mark → 0（语义：没等到任何响应）
		AttemptsJSON:     attemptsFromCtx(c),
	}
	if err := db.Create(&logEntry).Error; err != nil {
		RequestLogf(c, "[usage] 失败落库失败: %v", err)
		return
	}
	RequestLogf(c, "[usage] 失败 req=%s 渠道 id=%d model=%s status=%d err=%s", reqID, ch.ID, modelName, status, errMsg)
}

// recordUsageFailure 失败也落库：把被拒绝/失败/异常的请求写成审计流水
//
// 原则（面试讲点）：计费流水升级为请求审计——
//   成功的记 token/费用，失败的一样留痕（status + error 摘要），
//   排障时能回答"这条请求为什么失败、卡在哪一层"。
// 渠道/模型拿不到的阶段（如限流在解析 body 之前）传空值即可。
//
// ⚠️ 命名区分：与熔断器的 h.breakers.recordFailure（渠道故障计数）不是一回事，
//   这里是"把一次失败请求写进 usage_logs 审计流水"
func (h *RelayHandler) recordUsageFailure(c *gin.Context, ch model.Channel, modelName string, status int, errMsg string) {
	logUsageFailure(h.db, c, ch, modelName, status, errMsg)
}

// upstreamErrorSummary 从上游错误响应体提取一句话错误摘要（排障用）
// 兼容 OpenAI 与 Anthropic 的错误体格式：{"error":{"message":"..."}}
// 提取失败/体为空时返回空串
func upstreamErrorSummary(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.Error.Message == "" {
		return ""
	}
	msg := strings.TrimSpace(parsed.Error.Message)
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
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
//
// 排障日志：每个候选渠道被过滤时都打印原因（已试/模型不匹配/熔断/禁用），
//   否则"无可用渠道"时只能瞎猜是哪个条件挡住的。
func (h *RelayHandler) pickChannel(c *gin.Context, excluded map[uint]struct{}, modelName string) (model.Channel, error) {
	// ① 从内存快照读启用渠道（无锁；替代每请求查库——配置热更新的读路径）
	list := h.cache.List()

	// ② 三层过滤：已试失败排除 + 按模型路由 + 熔断状态
	//    （熔断：连续失败 N 次的渠道被摘除，冷却期/探活恢复后才重新入池）
	candidates := make([]model.Channel, 0, len(list))
	now := time.Now()
	RequestLogf(c, "[pick] 候选池扫描（模型=%s）：共 %d 个启用渠道", modelName, len(list))
	for _, ch := range list {
		// 过滤原因①：已经试过且失败
		if _, tried := excluded[ch.ID]; tried {
			RequestLogf(c, "[pick]   渠道 id=%d %s：已试失败，排除", ch.ID, ch.Name)
			continue
		}
		// 过滤原因②：模型白名单不匹配（models 为空=支持所有，不过滤）
		if modelName != "" && ch.Models != "" && !containsModel(ch.Models, modelName) {
			RequestLogf(c, "[pick]   渠道 id=%d %s：models=%q 不含 %q，跳过", ch.ID, ch.Name, ch.Models, modelName)
			continue
		}
		// 过滤原因③：熔断 OPEN/冷却中
		if !h.breakers.allow(ch.ID, now) {
			RequestLogf(c, "[pick]   渠道 id=%d %s：熔断冷却中，跳过", ch.ID, ch.Name)
			continue
		}
		// 兜底：权重为 0 的渠道不参与调度（防止死循环）
		if ch.Weight < 1 {
			ch.Weight = 1
		}
		candidates = append(candidates, ch)
		RequestLogf(c, "[pick]   渠道 id=%d %s：入候选池（weight=%d）", ch.ID, ch.Name, ch.Weight)
	}
	RequestLogf(c, "[pick] 最终候选 %d 个", len(candidates))
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
		case "message_start":
			// message: {"usage":{"input_tokens":12,"output_tokens":1}}
			// input_tokens 是真实输入量；output_tokens 是"预计上限"（近似 max_tokens），
			// 不是最终生成量 → 只取 input，output 等 message_delta 的真实值
			var m struct {
				Message struct {
					Usage struct {
						InputTokens int `json:"input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal([]byte(data), &m); err != nil {
				return nil, err
			}
			if m.Message.Usage.InputTokens > 0 {
				chunk.usage = &sseUsage{
					PromptTokens: m.Message.Usage.InputTokens,
					TotalTokens:  m.Message.Usage.InputTokens,
				}
			}
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
			// delta: {"stop_reason":"end_turn","stop_sequence":null} + 顶层 usage.output_tokens（最终生成量）
			var d struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(data), &d); err != nil {
				return nil, err
			}
			chunk.finishReason = mapAnthropicStopReason(d.Delta.StopReason)
			if d.Usage.OutputTokens > 0 {
				chunk.usage = &sseUsage{
					CompletionTokens: d.Usage.OutputTokens,
					TotalTokens:      d.Usage.OutputTokens,
				}
			}
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

// handleSSEEvent 处理一条完整 SSE 事件（已攒到空行）
// 返回值：
//   chunk    解析出的增量（W3-② 计费用：累计 content / 捕获 usage；无增量事件为 nil）
//   done     是否已向客户端发出 [DONE] 收尾标记
//            （forwardSSE 靠它判断要不要在 EOF 时补发 [DONE]：Anthropic 上游没有 [DONE]）
func handleSSEEvent(c *gin.Context, event []byte) (*sseChunk, bool, error) {
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
		return nil, false, nil // 纯注释/心跳行，忽略
	}
	data := strings.Join(datas, "\n")

	// ② 结束标记 [DONE]：原样透传，这是 OpenAI 流结束的约定
	if data == "[DONE]" {
		if _, err := io.WriteString(c.Writer, "data: [DONE]\n\n"); err != nil {
			return nil, false, err
		}
		flushWriter(c)
		return nil, true, nil
	}

	// ③ 解析成结构化增量；解析失败（非 JSON 的 data）→ 容错原样透传，不打断流
	chunk, err := parseSSEChunk(data)
	if err != nil {
		if _, werr := c.Writer.Write(event); werr != nil {
			return nil, false, werr
		}
		flushWriter(c)
		return nil, false, nil
	}

	// ④ 无增量事件直接跳过：Anthropic 的 message_start / content_block_start /
	//    content_block_stop / message_stop 不产生任何内容，不透传给客户端
	//    （chunk 仍返回给调用方：usage 等元数据可能藏在无增量事件里）
	if chunk.content == "" && chunk.finishReason == "" && chunk.usage == nil {
		return chunk, false, nil
	}

	// ⑤ 增量转换：按 OpenAI 格式重组写回 + 立即 Flush（保打字机效果）
	//    首个有内容的增量 = 首 token 时刻（TTFT），只记一次
	markFirstResponse(c)
	if _, err := c.Writer.Write(chunk.render()); err != nil {
		return chunk, false, err // 客户端断连会在这里报错 → 上游 context 随之取消
	}
	flushWriter(c)
	return chunk, false, nil
}

// mergeSSEUsage 合并两段 usage：Anthropic 流的用量分散在两条事件里——
// message_start 带 input_tokens、message_delta 带 output_tokens，各取非零段拼成完整用量。
// OpenAI 单条事件自带完整 usage（三字段齐全），直接覆盖语义不受影响（取非零即原值）。
func mergeSSEUsage(dst, src *sseUsage) *sseUsage {
	if dst == nil {
		return src
	}
	if src.PromptTokens > 0 {
		dst.PromptTokens = src.PromptTokens
	}
	if src.CompletionTokens > 0 {
		dst.CompletionTokens = src.CompletionTokens
	}
	dst.TotalTokens = dst.PromptTokens + dst.CompletionTokens
	return dst
}

// forwardSSE 逐事件消费上游 SSE 流，边收边转边推
//
// 核心：按行读、按空行切事件、攒够一条处理一条。
// ReadBytes('\n') 天然处理"一条事件被拆成多个网络包"——没读到 \n 就一直攒着。
//
// 返回值（W3-② 计费用）：
//   content 累计的全部增量文本（usage 缺失时按它估算 completion tokens）
//   usage   流末尾合并后的真实用量（可能为 nil：上游没返回 usage）
//   error   转发过程中的错误
func forwardSSE(c *gin.Context, upResp *http.Response, isAnthropic bool) (string, *sseUsage, error) {
	reader := bufio.NewReader(upResp.Body)
	var event []byte   // 当前事件的缓冲（跨块攒）
	sentDone := false  // 是否已向客户端发过 [DONE]（OpenAI 上游自带；Anthropic 上游没有，EOF 时补）
	var content strings.Builder // 累计增量文本（计费估算兜底）
	var usage *sseUsage         // 合并后的 usage（OpenAI 单条事件覆盖；Anthropic 两段合并）
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			event = append(event, line...)
			if len(bytes.TrimSpace(line)) == 0 { // 空行 = SSE 事件分隔符
				chunk, done, herr := handleSSEEvent(c, event)
				if herr != nil {
					return content.String(), usage, herr
				}
				sentDone = sentDone || done
				if chunk != nil { // 累计增量 + 合并 usage（chunk 可能为空：无增量事件）
					content.WriteString(chunk.content)
					if chunk.usage != nil {
						usage = mergeSSEUsage(usage, chunk.usage)
					}
				}
				event = event[:0] // 清空缓冲，等下一条
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break // 上游流正常结束
			}
			return content.String(), usage, err
		}
	}
	// 流被上游截断时，最后可能剩一条没闭合的事件，也处理掉
	if len(bytes.TrimSpace(event)) > 0 {
		chunk, done, herr := handleSSEEvent(c, event)
		if herr != nil {
			return content.String(), usage, herr
		}
		sentDone = sentDone || done
		if chunk != nil {
			content.WriteString(chunk.content)
			if chunk.usage != nil {
				usage = mergeSSEUsage(usage, chunk.usage)
			}
		}
	}
	// Anthropic 流：usage 分散在 message_start（input）与 message_delta（output）里，
	// 客户端一路上收到的是两条"半对"的 usage 事件 → 流结束时补发一条完整 usage 覆盖
	// （OpenAI 上游自带独立完整 usage 事件，已透传，无需补发）
	if isAnthropic && usage != nil && (usage.PromptTokens > 0 || usage.CompletionTokens > 0) {
		if b, jerr := json.Marshal(map[string]any{"usage": usage}); jerr == nil {
			if _, werr := io.WriteString(c.Writer, "data: "+string(b)+"\n\n"); werr == nil {
				flushWriter(c)
			}
		}
	}
	// 协议补齐：Anthropic 流用 message_stop 结束、没有 [DONE]，
	// 客户端（OpenAI SDK）等的是 data: [DONE] 收尾 → EOF 时补发一个
	if !sentDone {
		if _, err := io.WriteString(c.Writer, "data: [DONE]\n\n"); err != nil {
			return content.String(), usage, err
		}
		flushWriter(c)
	}
	return content.String(), usage, nil
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
	// 计时起点：请求一进来就记（限流/鉴权失败也算总耗时的一部分）
	c.Set(relayStartKey, time.Now())
	// 初始化故障转移尝试链：每换一次渠道 append 一跳，最终随流水落库
	initAttempts(c)

	// ⓪ W3 限流：在解析 body 之前先掐住（省得为被拒绝的请求白做 JSON 解析）
	// key 取调用方标识（Authorization Bearer / X-User-Key），兜底客户端 IP
	if h.limiter != nil {
		key := h.rateLimitKey(c)
		ok, err := h.limiter.Allow(key)
		if err != nil {
			// fail-open：限流器自身故障（如 Redis 连不上）时放行，
			// 保证"限流组件挂了"不会拖垮整个网关（可用性优先），但记日志可观测
			RequestLogf(c, "[ratelimit] 限流器故障，fail-open 放行（key=%s）: %v", key, err)
		} else if !ok {
			// 失败也落库：限流拒绝在解析 body 之前，渠道/模型拿不到 → 传空值
			h.recordUsageFailure(c, model.Channel{}, "", http.StatusTooManyRequests, "滑动窗口内请求超限，限流拒绝")
			resp(c, 429, "请求过于频繁，请稍后再试（滑动窗口内已达上限）", nil)
			return
		}
	}

	// ① 读出客户端的请求体（只读一次，各次重试复用同一份 body）
	//    ⚠️ body 是"流"：只能读一次。这里读出来，后面要重建给每个候选上游
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		h.recordUsageFailure(c, model.Channel{}, "", http.StatusBadRequest, "读取请求体失败: "+err.Error())
		resp(c, 400, "读取请求体失败: "+err.Error(), nil)
		return
	}

	// 解析客户端请求里的 model（路由层依据：按模型匹配候选渠道）
	// 例：{"model":"claude-3-5-sonnet",...} → 只有声明支持该模型的渠道才进候选池
	// 同时提取 messages 文本：W3-② 计费用——上游流式响应没返回 usage 时，
	// 用请求文本估算 prompt tokens 兜底
	var reqMeta struct {
		Model    string `json:"model"`
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &reqMeta)

	// 请求侧估算 prompt tokens（拼所有消息文本；usage 缺失时兜底用）
	promptTokensEst := 0
	if len(reqMeta.Messages) > 0 {
		var sb strings.Builder
		for _, m := range reqMeta.Messages {
			sb.WriteString(m.Content)
		}
		promptTokensEst = usage.EstimateTokens(sb.String())
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
		ch, err := h.pickChannel(c, tried, reqMeta.Model) // 路由层：按客户端 model 过滤候选渠道
		if err != nil {
			// 没有候选渠道了：区分"从来没渠道"和"全试完都失败"
			if len(tried) > 0 {
				// 失败也落库：全试完都失败 → 502（网关尽力了，全是上游的锅）
				h.recordUsageFailure(c, model.Channel{}, reqMeta.Model, http.StatusBadGateway, "所有渠道均不可用")
				resp(c, 502, "所有渠道均不可用", nil) // 网关尽力了，全是上游的锅
			} else {
				h.recordUsageFailure(c, model.Channel{}, reqMeta.Model, http.StatusServiceUnavailable, "无可用渠道")
				resp(c, 503, "无可用渠道", nil)
			}
			return
		}
		RequestLogf(c, "[relay] 尝试渠道 id=%d name=%s weight=%d → %s", ch.ID, ch.Name, ch.Weight, ch.BaseURL)
		attemptStart := time.Now() // 这一跳尝试的计时起点（发出请求前记）

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
			RequestLogf(c, "[relay] 渠道 id=%d 密钥解密失败: %v，尝试下一个", ch.ID, err)
			appendAttempt(c, ch, 0, time.Since(attemptStart).Milliseconds(), "密钥解密失败: "+err.Error(), true)
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
				RequestLogf(c, "[relay] 渠道 id=%d 请求转换失败: %v，尝试下一个", ch.ID, err)
				appendAttempt(c, ch, 0, time.Since(attemptStart).Milliseconds(), "请求转换失败: "+err.Error(), true)
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
			RequestLogf(c, "[relay] 渠道 id=%d 网络失败: %v，尝试下一个", ch.ID, err)
			appendAttempt(c, ch, 0, time.Since(attemptStart).Milliseconds(), "网络失败: "+err.Error(), true)
			tried[ch.ID] = struct{}{}
			h.breakers.recordFailure(ch.ID, time.Now())
			continue
		}

		// 上游给了明确响应，按状态码分流：
		if upResp.StatusCode >= 500 {
			// 5xx = 上游服务器故障 → 值得换渠道重试；同时累计熔断计数
			RequestLogf(c, "[relay] 渠道 id=%d 返回 %d，尝试下一个", ch.ID, upResp.StatusCode)
			appendAttempt(c, ch, upResp.StatusCode, time.Since(attemptStart).Milliseconds(), "", true)
			upResp.Body.Close() // 显式关闭，释放连接（不用 defer——循环里会累积）
			tried[ch.ID] = struct{}{}
			h.breakers.recordFailure(ch.ID, time.Now())
			continue
		}

		// 4xx/3xx/2xx：请求本身的结果（成功/参数错/限流），直接透传，不再重试
		//   为什么 4xx 不重试？400 是请求体坏、404 是路径错、401 是密钥错——
		//   换任何渠道结果都一样，重试纯属浪费
		//   ⚠️ 从此处起渠道已锁定：响应开始流向客户端，故障转移到此为止
		appendAttempt(c, ch, upResp.StatusCode, time.Since(attemptStart).Milliseconds(), "", false)
		defer upResp.Body.Close()
		h.breakers.recordSuccess(ch.ID) // 拿到响应 = 渠道是通的，记成功清失败计数
		RequestLogf(c, "[relay] 渠道 id=%d 返回 %d，锁定并透传", ch.ID, upResp.StatusCode)

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
			RequestLogf(c, "[relay] 渠道 id=%d SSE 流式响应，进入增量转换", ch.ID)
			content, usageInfo, ferr := forwardSSE(c, upResp, isAnthropicProvider(ch.Provider))
			if ferr != nil {
				// 客户端断连 / 上游异常中断：记录但不再切换渠道（响应已锁定）
				// 失败也落库：中断时 usage 往往拿不全，以可审计优先记失败
				RequestLogf(c, "[relay] 渠道 id=%d SSE 转发中断: %v", ch.ID, ferr)
				h.recordUsageFailure(c, ch, reqMeta.Model, upResp.StatusCode, "SSE 转发中断: "+ferr.Error())
			} else {
				// W3-② 计费：优先用上游返回的 usage；缺失时估算兜底
				//   - prompt: 上游 usage 或 请求侧估算
				//   - completion: 上游 usage 或 累计增量文本估算
				pt := 0
				ct := 0
				if usageInfo != nil {
					pt, ct = usageInfo.PromptTokens, usageInfo.CompletionTokens
				}
				if pt == 0 {
					pt = promptTokensEst
				}
				if ct == 0 {
					ct = usage.EstimateTokens(content)
				}
				h.recordUsage(c, ch, reqMeta.Model, pt, ct)
			}
		} else if anthropic {
			// 非流式 + Anthropic 渠道：响应是 Anthropic 格式（content 数组 + stop_reason），
			// 客户端（OpenAI SDK）不认——必须转成 OpenAI 格式再写回
			markFirstResponse(c) // 首响应 = 上游响应已到达（Do 返回，含上游生成时间）
			RequestLogf(c, "[relay] 渠道 id=%d 非流式响应，Anthropic → OpenAI 转换", ch.ID)
			usageInfo, respText, cerr := convertAnthropicOnce(c, upResp)
			if cerr != nil {
				// 失败也落库：上游错误体不是合法 Anthropic JSON（如 4xx 错误页）→ 转换必然失败
				RequestLogf(c, "[relay] 渠道 id=%d 非流式转换失败: %v", ch.ID, cerr)
				h.recordUsageFailure(c, ch, reqMeta.Model, upResp.StatusCode, "Anthropic 响应转换失败: "+cerr.Error())
			} else {
				// W3-② 计费：Anthropic usage 里 input/output tokens → prompt/completion
				// usage 缺失时兜底：prompt 用请求估算、completion 用响应文本估算
				if usageInfo != nil {
					h.recordUsage(c, ch, reqMeta.Model, usageInfo.PromptTokens, usageInfo.CompletionTokens)
				} else {
					h.recordUsage(c, ch, reqMeta.Model, promptTokensEst, usage.EstimateTokens(respText))
				}
			}
		} else {
			// 普通响应：读完 → 原样写回 → 解析 usage 落账（缺失时按响应文本估算）
			// ⚠️ 为什么这里改成"读完再写"而不是 io.Copy 边读边写：
			//    计费需要解析响应体里的 usage；普通 JSON 响应体一般不大，
			//    读进内存的开销可接受（SSE 才必须流式，这里不流式）
			markFirstResponse(c) // 首响应 = 上游响应已到达（Do 返回，含上游生成时间）
			respBody, rerr := io.ReadAll(upResp.Body)
			if rerr != nil {
				RequestLogf(c, "[relay] 渠道 id=%d 读取响应体失败: %v", ch.ID, rerr)
				h.recordUsageFailure(c, ch, reqMeta.Model, upResp.StatusCode, "读取响应体失败: "+rerr.Error())
			} else {
				if _, werr := c.Writer.Write(respBody); werr != nil {
					RequestLogf(c, "[relay] 渠道 id=%d 写回响应失败: %v", ch.ID, werr)
				} else {
					flushWriter(c) // 收尾 Flush，确保最后一截也立即送达
				}
				// 上游 4xx 透传给客户端 = 这次请求"失败"：记失败流水（带上游错误摘要）
				// 200 才走计费落账；错误体摘要从 {"error":{"message":...}} 提取
				if upResp.StatusCode >= 400 {
					h.recordUsageFailure(c, ch, reqMeta.Model, upResp.StatusCode, upstreamErrorSummary(respBody))
					return
				}
				// W3-② 计费：解析 OpenAI usage（prompt_tokens/completion_tokens）
				// usage 缺失时：completion 按 choices[0].message.content 纯输出文本估算
				// ⚠️ 不能按整个响应体估算——JSON 骨架（id/object/role/finish_reason 等）
				//    不是模型输出，会把 completion 高估 2~3 倍；口径必须和流式/Anthropic
				//    分支一致（估"模型输出的文本"，不估"响应的大小"）
				var bodyMeta struct {
					Usage   *sseUsage `json:"usage"`
					Choices []struct {
						Message struct {
							Content string `json:"content"`
						} `json:"message"`
					} `json:"choices"`
				}
				pt, ct := 0, 0
				if err := json.Unmarshal(respBody, &bodyMeta); err == nil {
					if bodyMeta.Usage != nil {
						pt, ct = bodyMeta.Usage.PromptTokens, bodyMeta.Usage.CompletionTokens
					}
					if ct == 0 && len(bodyMeta.Choices) > 0 {
						ct = usage.EstimateTokens(bodyMeta.Choices[0].Message.Content) // 纯输出文本估算
					}
				}
				if pt == 0 {
					pt = promptTokensEst
				}
				h.recordUsage(c, ch, reqMeta.Model, pt, ct)
			}
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
// 返回 *sseUsage（W3-② 计费用：input_tokens/output_tokens → prompt/completion）
// 和完整响应文本（usage 缺失时按文本估算 completion tokens 兜底）
func convertAnthropicOnce(c *gin.Context, upResp *http.Response) (*sseUsage, string, error) {
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
		return nil, "", err
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
	var usage *sseUsage
	if anth.Usage.InputTokens > 0 || anth.Usage.OutputTokens > 0 {
		usage = &sseUsage{
			PromptTokens:     anth.Usage.InputTokens,
			CompletionTokens: anth.Usage.OutputTokens,
			TotalTokens:      anth.Usage.InputTokens + anth.Usage.OutputTokens,
		}
		openai["usage"] = map[string]any{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.TotalTokens,
		}
	}
	return usage, text, json.NewEncoder(c.Writer).Encode(openai)
}
