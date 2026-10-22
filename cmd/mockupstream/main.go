// Mock 上游：本地开发用的"假 LLM 服务器"
//
// 作用：模拟真实的 DeepSeek/OpenAI 接口，让网关有真实目标可以转发
// 用法：go run ./cmd/mockupstream → 监听 :9090
// 然后建渠道时 base_url 填 http://localhost:9090/v1
package main

import (
	"encoding/json"
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

// handleChat 处理聊天补全请求，返回 OpenAI 格式的假响应
func handleChat(w http.ResponseWriter, r *http.Request) {
	// ① 读取请求体（假装解析用户消息；真实上游会在这里调用大模型）
	body, _ := io.ReadAll(r.Body)
	log.Printf("mock 收到请求: %s", body)

	// ② 返回 OpenAI 兼容格式的假响应
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
