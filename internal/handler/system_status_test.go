package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"llm-gateway/config"
	"llm-gateway/internal/model"
)

// TestSystemStatus 系统状态接口：聚合字段齐全、熔断快照可读、安全字段只给布尔
func TestSystemStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	if err := db.AutoMigrate(&model.Channel{}, &model.AccessKey{}); err != nil {
		t.Fatal(err)
	}

	// 造 2 个启用渠道 + 1 个 AccessKey
	for i := 0; i < 2; i++ {
		ch := model.Channel{Name: "ch", Provider: "openai", BaseURL: "http://localhost:9090/v1", Enabled: true, Models: "*", Weight: 1}
		if err := db.Create(&ch).Error; err != nil {
			t.Fatal(err)
		}
	}
	plain, hash, _ := genAccessKey()
	_ = plain
	if err := db.Create(&model.AccessKey{Name: "ak", KeyHash: hash}).Error; err != nil {
		t.Fatal(err)
	}

	cache := NewChannelCache(db, 0) // 启动即载入快照（2 个启用渠道）
	breakers := newCircuitBreakers()

	// 造一个熔断渠道：渠道 1 连续失败 3 次 → OPEN
	for i := 0; i < 3; i++ {
		breakers.recordFailure(1, time.Now())
	}

	h := NewSystemStatusHandler(db, cache, breakers, config.Config{
		RateLimitEnabled: true,
		RateLimitPerMin:  60,
		RateLimitStore:   "memory",
		RedisAddr:        "localhost:6379",
		SecretKey:        "", // 未配置 → secret_key_set 应为 false
	})
	r := gin.New()
	r.GET("/api/system/status", h.Status)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/system/status", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}

	var body struct {
		Data struct {
			Server struct {
				StartedAt string `json:"started_at"`
				Uptime    string `json:"uptime"`
			} `json:"server"`
			RateLimit struct {
				Enabled     bool   `json:"enabled"`
				Store       string `json:"store"`
				LimitPerMin int    `json:"limit_per_min"`
			} `json:"rate_limit"`
			Security struct {
				SecretKeySet bool `json:"secret_key_set"`
			} `json:"security"`
			Cache struct {
				EnabledChannels int `json:"enabled_channels"`
				TotalChannels   int `json:"total_channels"`
			} `json:"cache"`
			AccessKeys int `json:"access_keys"`
			Breakers   []struct {
				ChannelID uint    `json:"channel_id"`
				Name      string  `json:"channel_name"`
				State     string  `json:"state"`
				Failures  int     `json:"failures"`
				Cooldown  float64 `json:"cooldown_remaining_sec"`
			} `json:"breakers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	d := body.Data
	if d.Server.StartedAt == "" || d.Server.Uptime == "" {
		t.Fatalf("server 字段缺失: %+v", d.Server)
	}
	if !d.RateLimit.Enabled || d.RateLimit.Store != "memory" || d.RateLimit.LimitPerMin != 60 {
		t.Fatalf("rate_limit 字段不对: %+v", d.RateLimit)
	}
	if d.Security.SecretKeySet {
		t.Fatalf("未配置密钥却显示已配置")
	}
	if d.Cache.EnabledChannels != 2 || d.Cache.TotalChannels != 2 {
		t.Fatalf("cache 字段不对: %+v", d.Cache)
	}
	if d.AccessKeys != 1 {
		t.Fatalf("access_keys = %d, want 1", d.AccessKeys)
	}
	// 熔断列表：2 个启用渠道都应出现（渠道 1 熔断 + 渠道 2 补齐正常），且都带渠道名
	if len(d.Breakers) != 2 {
		t.Fatalf("breakers 条数 = %d, want 2: %+v", len(d.Breakers), d.Breakers)
	}
	foundOpen := false
	foundClosed := false
	for _, b := range d.Breakers {
		if b.Name == "" {
			t.Fatalf("breakers 缺少渠道名: %+v", b)
		}
		switch b.State {
		case "open":
			foundOpen = true
			if b.ChannelID != 1 {
				t.Fatalf("熔断渠道 id = %d, want 1", b.ChannelID)
			}
			if b.Failures != 3 {
				t.Fatalf("熔断渠道 failures = %d, want 3", b.Failures)
			}
			if b.Cooldown <= 0 {
				t.Fatalf("熔断渠道冷却剩余 = %f, want > 0", b.Cooldown)
			}
		case "closed":
			foundClosed = true
		}
	}
	if !foundOpen || !foundClosed {
		t.Fatalf("熔断列表应同时含 open 与 closed 补齐项: %+v", d.Breakers)
	}
}
