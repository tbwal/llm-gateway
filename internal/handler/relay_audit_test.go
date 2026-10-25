package handler

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"llm-gateway/internal/crypto"
	"llm-gateway/internal/model"
)

// TestRelayAuditFields 成功转发的请求，usage_logs 也应带全审计字段：
//   RequestID（req_ 前缀链路 ID）+ AccessKeyID/Name（可信身份）+ ClientIP
// 端到端验证：AccessKeyAuth 中间件 → 真实 RelayHandler → httptest mock 上游 → 落库
func TestRelayAuditFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	if err := db.AutoMigrate(&model.Channel{}, &model.UsageLog{}, &model.AccessKey{}); err != nil {
		t.Fatalf("AutoMigrate error = %v", err)
	}

	// mock 上游：OpenAI 非流式 JSON，带 usage（能走到计费落库分支）
	// 先发响应头、sleep 20ms 再发 body：制造"响应头先到、内容慢发"——
	// 首响应延迟（mark 于 Do 返回 = 头到达）才有稳定非零值可断言
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(20 * time.Millisecond)
		fmt.Fprintf(w, `{"id":"cmpl-1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	}))
	defer upstream.Close()

	enc, err := crypto.NewService(strings.Repeat("0", 64))
	if err != nil {
		t.Fatalf("crypto.NewService error = %v", err)
	}

	// 启用渠道（api_key 直接存明文，转发 DecryptOrRaw 兼容）
	if err := db.Create(&model.Channel{
		Name: "mock-audit", Provider: "openai",
		BaseURL: upstream.URL + "/v1", APIKey: "sk-mock-1",
		Models: "gpt-5", Weight: 1, Enabled: true,
	}).Error; err != nil {
		t.Fatal(err)
	}
	cache := NewChannelCache(db, time.Hour)
	cache.Refresh()

	// 一把启用 key：scopes 覆盖 gpt-5
	plain, hash, _ := genAccessKey()
	if err := db.Create(&model.AccessKey{Name: "audit-user", KeyHash: hash, Scopes: "gpt-5", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	var ak model.AccessKey
	if err := db.Where("key_hash = ?", hash).First(&ak).Error; err != nil {
		t.Fatal(err)
	}

	relay := NewRelayHandler(db, enc, cache, nil)
	r := gin.New()
	r.POST("/v1/chat/completions", AccessKeyAuth(db), relay.ChatCompletions)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewBufferString(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Authorization", "Bearer "+plain)
	req.RemoteAddr = "10.0.0.7:9999"
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("转发 status = %d, body=%s", w.Code, w.Body.String())
	}

	// 最新一条流水：应带全审计字段
	var entry model.UsageLog
	if err := db.Order("id desc").First(&entry).Error; err != nil {
		t.Fatalf("查不到流水: %v", err)
	}
	if entry.Status != 200 || entry.Model != "gpt-5" {
		t.Fatalf("流水 status/model = %d/%s, want 200/gpt-5", entry.Status, entry.Model)
	}
	if entry.RequestID == "" || !strings.HasPrefix(entry.RequestID, "req_") {
		t.Fatalf("request_id = %q, want req_ 前缀", entry.RequestID)
	}
	if entry.AccessKeyID != ak.ID || entry.AccessKeyName != "audit-user" {
		t.Fatalf("access_key = %d/%s, want %d/audit-user（可信身份应随鉴权带入落库）", entry.AccessKeyID, entry.AccessKeyName, ak.ID)
	}
	if entry.ClientIP != "10.0.0.7" {
		t.Fatalf("client_ip = %q, want 10.0.0.7", entry.ClientIP)
	}
	if entry.PromptTokens != 2 || entry.CompletionTokens != 1 {
		t.Fatalf("tokens = %d+%d, want 2+1（上游 usage 解析）", entry.PromptTokens, entry.CompletionTokens)
	}
	// ③ 耗时字段：成功请求必须有总耗时 + 首响应延迟（非流式 = 开始写回响应体）
	if entry.DurationMs <= 0 {
		t.Fatalf("duration_ms = %d, want > 0", entry.DurationMs)
	}
	if entry.FirstResponseMs <= 0 {
		t.Fatalf("first_response_ms = %d, want > 0（非流式首响应应被 mark）", entry.FirstResponseMs)
	}
}

// TestParseAnthropicSSEUsage Anthropic 流式 usage 解析 + 合并：
// usage 分散在 message_start（input_tokens）与 message_delta（output_tokens）两条事件里，
// parseSSEChunk 各取一段，mergeSSEUsage 拼成完整 {12,10,22}——计费不再走估算兜底
func TestParseAnthropicSSEUsage(t *testing.T) {
	// ① message_start：提取真实输入量（output_tokens 是预估上限，忽略）
	chunk, err := parseSSEChunk(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-3-5-sonnet","usage":{"input_tokens":12,"output_tokens":1}}}`)
	if err != nil {
		t.Fatalf("parseSSEChunk(message_start) err = %v", err)
	}
	if chunk.usage == nil || chunk.usage.PromptTokens != 12 {
		t.Fatalf("message_start usage = %+v, want PromptTokens=12", chunk.usage)
	}
	if chunk.content != "" || chunk.finishReason != "" {
		t.Fatalf("message_start 不应产生增量，got content=%q finish=%q", chunk.content, chunk.finishReason)
	}

	// ② content_block_delta：增量文本，不带 usage
	chunk, err = parseSSEChunk(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好"}}`)
	if err != nil {
		t.Fatalf("parseSSEChunk(delta) err = %v", err)
	}
	if chunk.content != "你好" || chunk.usage != nil {
		t.Fatalf("content_block_delta = content=%q usage=%+v, want 你好/nil", chunk.content, chunk.usage)
	}

	// ③ message_delta：结束原因 + 顶层 usage.output_tokens（最终生成量）
	chunk, err = parseSSEChunk(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":10}}`)
	if err != nil {
		t.Fatalf("parseSSEChunk(message_delta) err = %v", err)
	}
	if chunk.finishReason != "stop" {
		t.Fatalf("message_delta finishReason = %q, want stop", chunk.finishReason)
	}
	if chunk.usage == nil || chunk.usage.CompletionTokens != 10 {
		t.Fatalf("message_delta usage = %+v, want CompletionTokens=10", chunk.usage)
	}

	// ④ 合并：message_start 的 input + message_delta 的 output → 完整用量
	merged := mergeSSEUsage(&sseUsage{PromptTokens: 12, TotalTokens: 12},
		&sseUsage{CompletionTokens: 10, TotalTokens: 10})
	if merged.PromptTokens != 12 || merged.CompletionTokens != 10 || merged.TotalTokens != 22 {
		t.Fatalf("mergeSSEUsage = %+v, want {12,10,22}", merged)
	}

	// ⑤ 非 Anthropic 事件（message_stop）不产生 usage
	chunk, err = parseSSEChunk(`{"type":"message_stop"}`)
	if err != nil {
		t.Fatalf("parseSSEChunk(message_stop) err = %v", err)
	}
	if chunk.usage != nil || chunk.content != "" || chunk.finishReason != "" {
		t.Fatalf("message_stop 不应有任何内容，got %+v", chunk)
	}
}
