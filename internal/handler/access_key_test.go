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

// TestAccessKeyAuth 中间件：
//   合法 key → 放行 + context 注入身份；非法 key → 401；停用 key → 401
func TestAccessKeyAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestDB(t)
	if err := db.AutoMigrate(&model.AccessKey{}); err != nil {
		t.Fatalf("AutoMigrate(AccessKey) error = %v", err)
	}

	// 造两把 key：一把启用、一把停用
	// ⚠️ GORM 经典陷阱复现：Enabled 带 default:true 标签，Create 时零值(false)会被
	//    默认值覆盖成 true（和渠道 enabled 一样的坑）→ 停用的那把必须 Create 后再
	//    显式 Update("enabled", false)，否则测试里它实际是启用的
	enablePlain, enableHash, _ := genAccessKey()
	disablePlain, disableHash, _ := genAccessKey()
	if err := db.Create(&model.AccessKey{Name: "ok", KeyHash: enableHash, Enabled: true}).Error; err != nil {
		t.Fatal(err)
	}
	off := model.AccessKey{Name: "off", KeyHash: disableHash}
	if err := db.Create(&off).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&off).Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		auth   string
		want   int // http 状态码
	}{
		{"启用 key 放行", "Bearer " + enablePlain, http.StatusOK},
		{"停用 key 拒绝", "Bearer " + disablePlain, http.StatusUnauthorized},
		{"不存在的 key 拒绝", "Bearer sk_lgw_ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", http.StatusUnauthorized},
		{"缺 Bearer 前缀", "sk_lgw_xxx", http.StatusUnauthorized},
		{"无 Authorization", "", http.StatusUnauthorized},
	}

	// 用完整 gin 引擎走中间件链路（能真实执行 Abort/Next）
	r := gin.New()
	r.POST("/v1/chat/completions", AccessKeyAuth(db), func(c *gin.Context) {
		akID, scopes := AccessKeyFromCtx(c)
		if akID == 0 {
			t.Fatalf("context 未注入身份")
		}
		if scopes != "" {
			t.Fatalf("scopes = %q, want 空", scopes)
		}
		c.JSON(http.StatusOK, gin.H{"ak": akID})
	})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
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

// 确保 *gorm.DB 被引用（newTestDB 返回值类型）
var _ = gorm.ErrRecordNotFound
