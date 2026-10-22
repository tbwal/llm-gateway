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
	"time"
)

func main() {
	// 注册处理器：OpenAI 兼容的聊天补全接口
	http.HandleFunc("/v1/chat/completions", handleChat)
	log.Println("mock 上游已启动: http://localhost:9090")
	log.Fatal(http.ListenAndServe(":9090", nil)) // 阻塞监听，Ctrl+C 退出
}

// handleChat 统一入口：先看客户端要流式还是普通返回，再分发
func handleChat(w http.ResponseWriter, r *http.Request) {
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

	// ④ SSE 结束标记（OpenAI 约定的 [DONE]）
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}
