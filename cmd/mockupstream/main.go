// Mock 上游：本地开发用的"假 LLM 服务器"
//
// 作用：模拟真实的 DeepSeek/OpenAI 接口，让网关有真实目标可以转发
// 用法：go run ./cmd/mockupstream → 监听 :9090
// 然后建渠道时 base_url 填 http://localhost:9090/v1
//
// 支持两种模式（由请求体里的 "stream" 字段决定）：
//   stream=false/缺省 → 一次性返回完整 JSON
//   stream=true       → SSE 流式：逐块推送，模拟 ChatGPT 打字机效果
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync/atomic"
	"time"
)

// mockFail 全局故障开关：true 时所有接口返回 503（用于验证网关熔断与健康探测）
var mockFail atomic.Bool

func main() {
	// 注册处理器：OpenAI 兼容的聊天补全接口 + Anthropic Messages API
	http.HandleFunc("/v1/chat/completions", handleChat)
	http.HandleFunc("/v1/messages", handleMessages) // 模拟 Claude：协议转换的本地验证目标
	http.HandleFunc("/health", handleHealth)        // 健康探测端点（网关熔断恢复用）
	http.HandleFunc("/debug/fail", handleFailToggle) // 故障开关控制（验证熔断用）
	log.Println("mock 上游已启动: http://localhost:9090")
	log.Fatal(http.ListenAndServe(":9090", nil)) // 阻塞监听，Ctrl+C 退出
}

// handleHealth 健康探测端点：网关的后台探测器定期 GET 这里，确认渠道上游是否存活
// 正常 → 200；故障模式 → 503（让探测失败，渠道保持熔断）
func handleHealth(w http.ResponseWriter, r *http.Request) {
	if mockFail.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("mock fail mode\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}

// handleFailToggle 故障开关控制端点：POST {"on":true/false}
// 验证熔断的用法：开 → 网关所有渠道连续失败 → 熔断摘除 → 关 → 探活恢复
func handleFailToggle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		On bool `json:"on"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	mockFail.Store(req.On)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "{\"mock_fail\":%v}\n", req.On)
}

// handleChat 统一入口：先看客户端要流式还是普通返回，再分发
func handleChat(w http.ResponseWriter, r *http.Request) {
	// 故障模式检查：开启时全部返回 503（模拟渠道大面积故障，触发网关熔断）
	if mockFail.Load() {
		http.Error(w, "mock 故障模式", http.StatusServiceUnavailable)
		return
	}
	// ① 读取请求体（真实上游会在这里解析消息并调用大模型）
	body, _ := io.ReadAll(r.Body)
	log.Printf("mock 收到请求: %s", body)

	// ② 解析请求体，取出 stream 字段（客户端要不要流式）
	var req struct {
		Stream bool `json:"stream"` // 结构体标签：从 JSON 里取 stream 字段
	}
	_ = json.Unmarshal(body, &req)

	// ③ 按模式分发
	if req.Stream {
		handleStream(w) // 流式：打字机模式
		return
	}
	handleOnce(w) // 普通：一次性返回
}

// handleOnce 普通模式：一次性返回 OpenAI 格式的完整 JSON
func handleOnce(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":      "chatcmpl-mock-001",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   "mock-upstream",
		"choices": []map[string]any{
			{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "我是 mock 上游的回复：收到你的请求了！",
				},
				"finish_reason": "stop",
			},
		},
	})
}

// handleStream 流式模式（SSE）：逐块推送，模拟大模型"一个字一个字生成"
//
// SSE（Server-Sent Events）协议要点：
//   - 响应头 Content-Type 必须是 text/event-stream
//   - 每个数据块格式：data: <内容> 然后一个空行（\n\n 是块分隔符）
//   - 块与块之间用 Flush() 强制立即推给客户端（打字机效果的来源）
func handleStream(w http.ResponseWriter) {
	// ① SSE 必备的三个响应头
	w.Header().Set("Content-Type", "text/event-stream") // 声明这是 SSE 流
	w.Header().Set("Cache-Control", "no-cache")         // 禁止缓存（流式必须实时）
	w.Header().Set("Connection", "keep-alive")          // 保持长连接

	// ② 检查底层连接是否支持"强制推送"（Flusher）
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "当前连接不支持流式", http.StatusInternalServerError)
		return
	}

	// ③ 逐个词推送，模拟大模型逐字生成
	tokens := []string{"你好", "，", "我", "是", "流式", "回复", "，", "打字机", "效果", "！"}
	for _, token := range tokens {
		// SSE 数据块：data: {"choices":[{"delta":{"content":"..."}}]}
		// delta 表示"增量"——OpenAI 流式协议里每次只传新增的那一小段
		data := fmt.Sprintf("data: %s\n\n", `{"choices":[{"delta":{"content":"`+token+`"},"index":0}]}`)
		fmt.Fprint(w, data)   // 写入响应（进底层连接）
		flusher.Flush()       // 关键动作：把缓冲内容立刻推到客户端（不等攒满）
		time.Sleep(200 * time.Millisecond) // 模拟大模型"思考+生成"的耗时
	}

	// ④ 流结束前，OpenAI 真实上游会先发两条元数据：
	//    - finish_reason：本轮生成结束的原因（stop / length / tool_calls）
	//    - usage：用量统计（部分上游放在最后一条事件）
	//    网关的 SSE 增量转换要能透传它们（relay.go 的 parseSSEChunk/render）
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"index\":0,\"finish_reason\":\"stop\"}]}\n\n")
	flusher.Flush()
	fmt.Fprint(w, "data: {\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":10,\"total_tokens\":22}}\n\n")
	flusher.Flush()

	// ⑤ SSE 结束标记（OpenAI 约定的 [DONE]）
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// ============================================================
// Anthropic Messages API（模拟 Claude）
//
// 为什么加它：网关要做 Anthropic ↔ OpenAI 协议转换（W2），
// 本地必须有一个"Anthropic 格式的上游"用来闭环验证。
// 客户端侧（网关）永远只发 OpenAI 格式，这里模拟的是"真实 Claude"
// 会返回的格式——包括请求路径 /v1/messages 和流式事件序列。
//
// Anthropic 流式和 OpenAI 流式的本质区别：
//   - OpenAI：每块 data: {"choices":[{"delta":{"content":"..."}}]}，靠 [DONE] 收尾
//   - Anthropic：用 event: 行区分事件类型，一个会话有 6 种事件：
//     message_start → content_block_start → content_block_delta × N
//     → content_block_stop → message_delta → message_stop
// ============================================================

// handleMessages Anthropic Messages API 统一入口：按 stream 字段分发
func handleMessages(w http.ResponseWriter, r *http.Request) {
	// 故障模式检查：与 OpenAI 端点一致，统一模拟上游故障
	if mockFail.Load() {
		http.Error(w, "mock 故障模式", http.StatusServiceUnavailable)
		return
	}
	// ① 读取请求体（真实 Claude 在这里解析消息并调用大模型）
	body, _ := io.ReadAll(r.Body)
	log.Printf("mock(Anthropic) 收到请求: %s", body)

	// ② 解析 stream 字段（Anthropic 也支持 stream:true）
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &req)

	// ③ 按模式分发
	if req.Stream {
		handleMessagesStream(w) // 流式：Anthropic 事件序列
		return
	}
	handleMessagesOnce(w) // 普通：一次性返回
}

// handleMessagesOnce Anthropic 非流式响应
// 和 OpenAI 的关键差异：content 是数组（元素带 type），结束原因叫 stop_reason
func handleMessagesOnce(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id":            "msg_mock_001",
		"type":          "message",
		"role":          "assistant",
		"content": []map[string]any{
			{"type": "text", "text": "我是 mock 上游的回复：收到你的请求了！（Anthropic 格式）"},
		},
		"model":         "claude-3-5-sonnet",
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": 12, "output_tokens": 10},
	})
}

// handleMessagesStream Anthropic 流式响应（SSE）
//
// 事件序列（与真实 Claude 一致）：
//   message_start        会话开始，带消息元信息（id/model/usage 预估）
//   content_block_start  第一个内容块开始
//   content_block_delta  增量文本（打字机本体，OpenAI 的 delta.content 对应这里的 delta.text）
//   content_block_stop   内容块结束
//   message_delta        结束原因（stop_reason）+ 输出用量
//   message_stop         会话结束（对应 OpenAI 的 [DONE]）
func handleMessagesStream(w http.ResponseWriter) {
	// ① SSE 必备响应头
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// ② Flusher 检查
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "当前连接不支持流式", http.StatusInternalServerError)
		return
	}

	// sseEvent 推送一条 Anthropic 事件：event: 行 + data: 行 + 空行
	sseEvent := func(event, data string) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	}

	// ③ message_start：会话开始（带预估用量）
	sseEvent("message_start",
		`{"type":"message_start","message":{"id":"msg_mock_001","type":"message","role":"assistant","content":[],"model":"claude-3-5-sonnet","usage":{"input_tokens":12,"output_tokens":1}}}`)

	// ④ content_block_start：第一个文本块开始
	sseEvent("content_block_start",
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)

	// ⑤ content_block_delta × N：逐字增量（打字机本体）
	tokens := []string{"你好", "，", "我", "是", "流式", "回复", "，", "打字机", "效果", "！"}
	for _, token := range tokens {
		sseEvent("content_block_delta",
			fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, token))
		time.Sleep(200 * time.Millisecond) // 模拟生成耗时
	}

	// ⑥ content_block_stop：内容块结束
	sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`)

	// ⑦ message_delta：结束原因 + 输出用量
	sseEvent("message_delta",
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":10}}`)

	// ⑧ message_stop：会话结束（Anthropic 没有 [DONE]，这就是它的收尾标记）
	sseEvent("message_stop", `{"type":"message_stop"}`)
}
