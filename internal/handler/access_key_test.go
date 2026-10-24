package handler

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"llm-gateway/internal/model"
)

// TestGenAccessKey 签发格式与唯一性
func TestGenAccessKey(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 50; i++ {
		plain, hash, err := genAccessKey()
		if err != nil {
			t.Fatalf("genAccessKey() error = %v", err)
		}
		if len(plain) != len("sk_lgw_")+64 {
			t.Fatalf("明文长度 = %d, want %d", len(plain), len("sk_lgw_")+64)
		}
		if seen[plain] {
			t.Fatalf("重复明文: %s", plain)
		}
		seen[plain] = true
		if hash != hashAccessKey(plain) {
			t.Fatalf("哈希与明文不匹配")
		}
		if len(hash) != 64 {
			t.Fatalf("哈希长度 = %d, want 64", len(hash))
		}
	}
}

// TestAccessKeyAuth 中间件（模型级鉴权）：
//   被启用 key 的 scopes 覆盖的模型 → 必须带合法 Bearer；
//   未被覆盖的模型 → 无凭证也放行（匿名）；带凭证的一律完整校验
func TestAccessKeyAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	if err := db.AutoMigrate(&model.AccessKey{}); err != nil {
		t.Fatalf("AutoMigrate(AccessKey) error = %v", err)
	}

	// 造两把 key：一把启用且只保护 gpt-4o；一把停用
	// ⚠️ GORM 经典陷阱复现：Enabled 带 default:true 标签，Create 时零值(false)会被
	//    默认值覆盖成 true（和渠道 enabled 一样的坑）→ 停用的那把必须 Create 后再
	//    显式 Update("enabled", false)，否则测试里它实际是启用的
	enablePlain, enableHash, _ := genAccessKey()
	disablePlain, disableHash, _ := genAccessKey()
	if err := db.Create(&model.AccessKey{Name: "ok", KeyHash: enableHash, Scopes: "gpt-4o", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	off := model.AccessKey{Name: "off", KeyHash: disableHash, Scopes: "gpt-4o"}
	if err := db.Create(&off).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&off).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		body   string // 请求体（含 model）
		auth   string
		want   int // http 状态码
	}{
		// ── 受保护模型（gpt-4o）──
		{"受保护+启用 key 放行", `{"model":"gpt-4o"}`, "Bearer " + enablePlain, http.StatusOK},
		{"受保护+停用 key 拒绝", `{"model":"gpt-4o"}`, "Bearer " + disablePlain, http.StatusUnauthorized},
		{"受保护+不存在的 key 拒绝", `{"model":"gpt-4o"}`, "Bearer sk_lgw_ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", http.StatusUnauthorized},
		{"受保护+缺 Bearer 前缀", `{"model":"gpt-4o"}`, "sk_lgw_xxx", http.StatusUnauthorized},
		{"受保护+无凭证拒绝", `{"model":"gpt-4o"}`, "", http.StatusUnauthorized},
		// ── 未受保护模型（claude-3-5-sonnet 不在任何启用 key 的 scope）──
		{"未受保护+无凭证放行", `{"model":"claude-3-5-sonnet"}`, "", http.StatusOK},
		{"未受保护+启用 key 放行", `{"model":"claude-3-5-sonnet"}`, "Bearer " + enablePlain, http.StatusOK},
		{"未受保护+无效凭证仍拒绝", `{"model":"claude-3-5-sonnet"}`, "Bearer sk_lgw_fake", http.StatusUnauthorized},
	}

	// 用完整 gin 引擎走中间件链路（能真实执行 Abort/Next）
	r := gin.New()
	r.POST("/v1/chat/completions", AccessKeyAuth(db), func(c *gin.Context) {
		akID, scopes := AccessKeyFromCtx(c)
		// 放行的匿名请求：context 不注入身份（akID==0）
		// 带凭证放行：身份必须是 enable 那把（id=1, scopes=gpt-4o）
		if akID != 0 {
			if akID != 1 {
				t.Fatalf("context 身份 id = %d, want 1", akID)
			}
			if scopes != "gpt-4o" {
				t.Fatalf("scopes = %q, want gpt-4o", scopes)
			}
		}
		c.JSON(http.StatusOK, gin.H{"ak": akID})
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(tc.body))
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			r.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// TestAccessKeyAllScope 一把 scopes 为空（=全部）的启用 key → 所有模型都受保护，无凭证一律 401
func TestAccessKeyAllScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	if err := db.AutoMigrate(&model.AccessKey{}); err != nil {
		t.Fatal(err)
	}
	plain, hash, _ := genAccessKey()
	if err := db.Create(&model.AccessKey{Name: "all", KeyHash: hash, Enabled: true}).Error; err != nil { // scopes 留空
		t.Fatal(err)
	}

	r := gin.New()
	r.POST("/v1", AccessKeyAuth(db), func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{}) })

	for _, m := range []string{"gpt-4o", "claude-3-5-sonnet", "anything-else"} {
		w := httptest.NewRecorder()
		body := bytes.NewBufferString(`{"model":"` + m + `"}`)
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1", body))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("model=%s 无凭证 status = %d, want 401", m, w.Code)
		}
	}
	// 带合法 key 则放行
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1", bytes.NewBufferString(`{"model":"anything-else"}`))
	req.Header.Set("Authorization", "Bearer "+plain)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("合法 key status = %d, want 200", w.Code)
	}
}

// TestAccessKeyCRUD 管理接口：创建返回明文一次 → 列表无明文 → 更新启停 → 删除
func TestAccessKeyCRUD(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	if err := db.AutoMigrate(&model.AccessKey{}); err != nil {
		t.Fatal(err)
	}
	h := NewAccessKeyHandler(db)
	r := gin.New()
	r.GET("/api/access-keys", h.List)
	r.POST("/api/access-keys", h.Create)
	r.PUT("/api/access-keys/:id", h.Update)
	r.DELETE("/api/access-keys/:id", h.Delete)

	// ① 创建 → 返回明文 + id
	w := httptest.NewRecorder()
	body := bytes.NewBufferString(`{"name":"测试","scopes":"gpt-5,claude-3-5-sonnet"}`)
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/access-keys", body))
	if w.Code != http.StatusOK {
		t.Fatalf("创建 status = %d, body=%s", w.Code, w.Body.String())
	}
	var created struct {
		Data struct {
			ID   uint   `json:"id"`
			Key  string `json:"key"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Data.Key == "" {
		t.Fatalf("创建响应缺少明文 key")
	}
	plain := created.Data.Key

	// ② 列表 → 有记录且不含 key / key_hash 字段
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/access-keys", nil))
	var listed struct {
		Data []gin.H `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Data) != 1 {
		t.Fatalf("列表条数 = %d, want 1", len(listed.Data))
	}
	if _, ok := listed.Data[0]["key"]; ok {
		t.Fatalf("列表泄露了明文 key")
	}
	if _, ok := listed.Data[0]["key_hash"]; ok {
		t.Fatalf("列表泄露了 key_hash")
	}

	// ③ 更新启停 → 中间件立即 401
	var disable = struct {
		Enabled bool `json:"enabled"`
	}{Enabled: false}
	bb, _ := json.Marshal(disable)
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPut, "/api/access-keys/1", bytes.NewReader(bb)))
	if w.Code != http.StatusOK {
		t.Fatalf("停用 status = %d", w.Code)
	}
	authR := gin.New()
	authR.POST("/v1", AccessKeyAuth(db), func(c *gin.Context) { c.JSON(200, gin.H{}) })
	w2 := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	authR.ServeHTTP(w2, req)
	if w2.Code != http.StatusUnauthorized {
		t.Fatalf("停用后调用 status = %d, want 401", w2.Code)
	}

	// ④ 删除 → 表空
	w = httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodDelete, "/api/access-keys/1", nil))
	var count int64
	db.Model(&model.AccessKey{}).Count(&count)
	if count != 0 {
		t.Fatalf("删除后剩余 %d 条", count)
	}
}

// TestAccessKeyFailureAudit 鉴权失败也落库：
//   受保护模型缺凭证 → 401 的同时 usage_logs 里出现 status=401 审计记录
func TestAccessKeyFailureAudit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	if err := db.AutoMigrate(&model.AccessKey{}, &model.UsageLog{}); err != nil {
		t.Fatalf("AutoMigrate error = %v", err)
	}

	// 一把启用 key、scopes 空 = 保护所有模型
	plain, hash, _ := genAccessKey()
	if err := db.Create(&model.AccessKey{Name: "audit", KeyHash: hash, Scopes: "", Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	r.POST("/v1", AccessKeyAuth(db), func(c *gin.Context) { c.JSON(200, gin.H{}) })

	// ① 受保护模型 + 缺凭证 → 401 落库
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1", bytes.NewBufferString(`{"model":"gpt-5"}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("缺凭证 status = %d, want 401", w.Code)
	}
	var entry model.UsageLog
	if err := db.Where("status = ?", http.StatusUnauthorized).Order("id desc").First(&entry).Error; err != nil {
		t.Fatalf("查不到 401 审计记录: %v", err)
	}
	if entry.Model != "gpt-5" {
		t.Fatalf("审计记录 model = %q, want gpt-5（中间件阶段也应带出模型）", entry.Model)
	}
	if entry.Error != "缺少 Bearer 凭证" {
		t.Fatalf("审计记录 error = %q", entry.Error)
	}

	// ② 无效凭证 → 401 落库（错误文案：凭证无效或已停用）
	req := httptest.NewRequest(http.MethodPost, "/v1", bytes.NewBufferString(`{"model":"gpt-5"}`))
	req.Header.Set("Authorization", "Bearer sk_lgw_invalid_key")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("无效凭证 status = %d, want 401", w.Code)
	}
	var count int64
	db.Model(&model.UsageLog{}).Where("status = ? AND error = ?", http.StatusUnauthorized, "凭证无效或已停用").Count(&count)
	if count != 1 {
		t.Fatalf("无效凭证审计记录 = %d, want 1", count)
	}

	// ③ 合法凭证 → 放行，不产生失败记录
	w = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1", bytes.NewBufferString(`{"model":"gpt-5"}`))
	req.Header.Set("Authorization", "Bearer "+plain)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("合法凭证 status = %d, want 200", w.Code)
	}
	var failCount int64
	db.Model(&model.UsageLog{}).Count(&failCount)
	if failCount != 2 {
		t.Fatalf("总审计记录 = %d, want 2（仅两条 401，成功不落 usage）", failCount)
	}
}

// 确保 *gorm.DB 被引用（newTestDB 返回值类型）
var _ = gorm.ErrRecordNotFound
