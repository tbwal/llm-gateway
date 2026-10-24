package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"llm-gateway/config"
	"llm-gateway/internal/model"
	"llm-gateway/internal/ratelimit"
)

// TestRateLimitAdmin 限流设置接口：
//   GET 返回当前配置；PUT 运行时生效（limiter.Limit 变）+ 落库持久化（settings 表）
func TestRateLimitAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	if err := db.AutoMigrate(&model.Setting{}); err != nil {
		t.Fatal(err)
	}
	limiter, err := ratelimit.New("memory", 60, time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	h := NewRateLimitHandler(db, limiter, config.Config{
		RateLimitEnabled: true,
		RateLimitPerMin:  60,
		RateLimitStore:   "memory",
	})
	r := gin.New()
	r.GET("/api/system/ratelimit", h.Get)
	r.PUT("/api/system/ratelimit", h.Update)

	// ① GET 初始：limit=60（来自构造默认），persisted 空
	var resp struct {
		Data struct {
			Enabled     bool   `json:"enabled"`
			Store       string `json:"store"`
			LimitPerMin int    `json:"limit_per_min"`
			Persisted   string `json:"persisted"`
		} `json:"data"`
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/system/ratelimit", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d", w.Code)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Data.Enabled || resp.Data.LimitPerMin != 60 {
		t.Fatalf("初始配置 = %+v, want enabled=true limit=60", resp.Data)
	}

	// ② PUT {per_min:3} → 200，返回 3
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/system/ratelimit", bytes.NewBufferString(`{"per_min":3}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", w.Code, w.Body.String())
	}
	if limiter.Limit() != 3 {
		t.Fatalf("limiter.Limit() = %d, want 3（运行时立即生效）", limiter.Limit())
	}

	// ③ GET 再查 → limit=3（实时值）
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/system/ratelimit", nil))
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Data.LimitPerMin != 3 {
		t.Fatalf("GET 后 limit = %d, want 3", resp.Data.LimitPerMin)
	}
	if resp.Data.Persisted != "3" {
		t.Fatalf("persisted = %q, want 3（已落库）", resp.Data.Persisted)
	}

	// ④ 持久化落库断言：settings 表有 ratelimit_per_min=3
	var st model.Setting
	if err := db.Where("key = ?", "ratelimit_per_min").First(&st).Error; err != nil {
		t.Fatalf("settings 表无记录: %v", err)
	}
	if st.Value != "3" {
		t.Fatalf("settings value = %q, want 3", st.Value)
	}

	// ⑤ 非法值 0 / 负数 / 超上限 → 400，且不改运行时值
	for _, bad := range []string{`{"per_min":0}`, `{"per_min":-1}`, `{"per_min":100001}`} {
		w = httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/system/ratelimit", bytes.NewBufferString(bad)))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("PUT %s status = %d, want 400", bad, w.Code)
		}
	}
	if limiter.Limit() != 3 {
		t.Fatalf("非法值后 limiter.Limit() = %d, want 3（应保持原值）", limiter.Limit())
	}
}
