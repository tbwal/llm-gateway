package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"llm-gateway/internal/model"
)

// newUsageTestEnv 内存 SQLite + usage_logs 表 + 三条样本流水 + 看板路由
func newUsageTestEnv(t *testing.T) *gin.Engine {
	t.Helper()
	db := newTestDB(t) // 复用：内存库 + channels 表
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatalf("AutoMigrate(UsageLog) error = %v", err)
	}
	// 样本流水：2 条 deepseek-chat（同一渠道不同时间） + 1 条 gpt-4o
	now := time.Now()
	samples := []model.UsageLog{
		{ChannelID: 1, ChannelName: "DeepSeek 主", Provider: "deepseek", Model: "deepseek-chat",
			PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, Cost: 0.0500, CreatedAt: now},
		{ChannelID: 1, ChannelName: "DeepSeek 主", Provider: "deepseek", Model: "deepseek-chat",
			PromptTokens: 200, CompletionTokens: 100, TotalTokens: 300, Cost: 0.1000, CreatedAt: now.Add(-24 * time.Hour)},
		{ChannelID: 5, ChannelName: "OpenAI 备用", Provider: "openai", Model: "gpt-4o",
			PromptTokens: 300, CompletionTokens: 150, TotalTokens: 450, Cost: 0.0135, CreatedAt: now},
	}
	for i, s := range samples {
		if err := db.Create(&s).Error; err != nil {
			t.Fatalf("插入样本 %d error = %v", i, err)
		}
	}

	r := gin.New()
	dash := NewUsageDashboard(db)
	ug := r.Group("/api/usage")
	{
		ug.GET("/summary", dash.Summary)
		ug.GET("/breakdown", dash.Breakdown)
		ug.GET("/daily", dash.Daily)
	}
	return r
}

// getJSON 发 GET 请求并解析响应 JSON（通用断言辅助）
func getJSON(t *testing.T, r *gin.Engine, path string, out any) int {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s 状态码 = %d, body = %s", path, w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		t.Fatalf("解析响应 %s error = %v: %s", path, err, w.Body.String())
	}
	return w.Code
}

// TestUsageSummary 总览：3 条流水 → 请求数 3、tokens 合计、费用合计
func TestUsageSummary(t *testing.T) {
	r := newUsageTestEnv(t)
	var resp struct {
		Days    int `json:"days"`
		Summary struct {
			Requests         int64   `json:"requests"`
			PromptTokens     int64   `json:"prompt_tokens"`
			CompletionTokens int64   `json:"completion_tokens"`
			TotalTokens      int64   `json:"total_tokens"`
			Cost             float64 `json:"cost"`
		} `json:"summary"`
	}
	getJSON(t, r, "/api/usage/summary?days=7", &resp)
	if resp.Days != 7 {
		t.Fatalf("days = %d, want 7", resp.Days)
	}
	if resp.Summary.Requests != 3 {
		t.Fatalf("requests = %d, want 3", resp.Summary.Requests)
	}
	if resp.Summary.PromptTokens != 600 || resp.Summary.CompletionTokens != 300 || resp.Summary.TotalTokens != 900 {
		t.Fatalf("tokens = %d+%d/%d, want 600+300/900",
			resp.Summary.PromptTokens, resp.Summary.CompletionTokens, resp.Summary.TotalTokens)
	}
	// 0.05 + 0.10 + 0.0135 = 0.1635
	if resp.Summary.Cost < 0.1634 || resp.Summary.Cost > 0.1636 {
		t.Fatalf("cost = %f, want ≈0.1635", resp.Summary.Cost)
	}
}

// TestUsageBreakdownByModel 按模型分组：deepseek-chat 2 条 / gpt-4o 1 条，按费用降序
func TestUsageBreakdownByModel(t *testing.T) {
	r := newUsageTestEnv(t)
	var resp struct {
		Rows []struct {
			Key        string `json:"key"`
			Requests   int64  `json:"requests"`
			TotalTokens int64 `json:"total_tokens"`
		} `json:"rows"`
	}
	getJSON(t, r, "/api/usage/breakdown?group=model&days=7", &resp)
	if len(resp.Rows) != 2 {
		t.Fatalf("分组行数 = %d, want 2: %+v", len(resp.Rows), resp.Rows)
	}
	if resp.Rows[0].Key != "deepseek-chat" || resp.Rows[0].Requests != 2 || resp.Rows[0].TotalTokens != 450 {
		t.Fatalf("rows[0] = %+v, want deepseek-chat 2条/450 tokens（费用高排前）", resp.Rows[0])
	}
	if resp.Rows[1].Key != "gpt-4o" || resp.Rows[1].Requests != 1 {
		t.Fatalf("rows[1] = %+v, want gpt-4o 1条", resp.Rows[1])
	}
}

// TestUsageBreakdownByChannel 按渠道分组：DeepSeek 主 2 条 / OpenAI 备用 1 条
func TestUsageBreakdownByChannel(t *testing.T) {
	r := newUsageTestEnv(t)
	var resp struct {
		Rows []struct {
			Key      string `json:"key"`
			Requests int64  `json:"requests"`
		} `json:"rows"`
	}
	getJSON(t, r, "/api/usage/breakdown?group=channel&days=7", &resp)
	if len(resp.Rows) != 2 {
		t.Fatalf("分组行数 = %d, want 2", len(resp.Rows))
	}
	if resp.Rows[0].Key != "DeepSeek 主" || resp.Rows[0].Requests != 2 {
		t.Fatalf("rows[0] = %+v, want DeepSeek 主 2条", resp.Rows[0])
	}
}

// TestUsageDaily 按天聚合：样本覆盖今天和昨天 → 2 行（时间序列升序）
func TestUsageDaily(t *testing.T) {
	r := newUsageTestEnv(t)
	var resp struct {
		Rows []struct {
			Date   string `json:"date"`
			Requests int64 `json:"requests"`
		} `json:"rows"`
	}
	getJSON(t, r, "/api/usage/daily?days=7", &resp)
	if len(resp.Rows) != 2 {
		t.Fatalf("按天行数 = %d, want 2: %+v", len(resp.Rows), resp.Rows)
	}
	// 升序：昨天在前、今天在后
	if resp.Rows[0].Requests != 1 || resp.Rows[1].Requests != 2 {
		t.Fatalf("rows = %+v, want 昨天1条、今天2条", resp.Rows)
	}
}

// TestUsageEmpty 空表：summary 应返回全 0 而不是报错（coalesce 兜底验证）
func TestUsageEmpty(t *testing.T) {
	db := newTestDB(t)
	if err := db.AutoMigrate(&model.UsageLog{}); err != nil {
		t.Fatalf("AutoMigrate error = %v", err)
	}
	r := gin.New()
	dash := NewUsageDashboard(db)
	r.GET("/api/usage/summary", dash.Summary)
	var resp struct {
		Summary struct {
			Requests int64   `json:"requests"`
			TotalTokens int64 `json:"total_tokens"`
			Cost     float64 `json:"cost"`
		} `json:"summary"`
	}
	getJSON(t, r, "/api/usage/summary?days=7", &resp)
	if resp.Summary.Requests != 0 || resp.Summary.TotalTokens != 0 || resp.Summary.Cost != 0 {
		t.Fatalf("空表 summary = %+v, want 全 0", resp.Summary)
	}
}

// TestUsageBreakdownBadGroup 非法 group 参数 → 400
func TestUsageBreakdownBadGroup(t *testing.T) {
	r := newUsageTestEnv(t)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/usage/breakdown?group=sql_inject", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("非法 group 状态码 = %d, want 400", w.Code)
	}
}
