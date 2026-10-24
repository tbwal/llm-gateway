// Package handler 业务处理层
package handler

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"llm-gateway/internal/model"
)

// ============================================================
// AccessKey 管理（调用方凭证）
//
// 背景：网关入口 /v1/chat/completions 原先谁都能调（限流 key 靠客户端自报），
// 面试官一问"怎么保证调用方身份可信"就卡住。这里补上完整凭证体系：
//
//   签发：POST /api/access-keys → 生成 sk_lgw_xxx，明文只返回这一次，库里存 SHA-256
//   鉴权：中间件校验 Authorization: Bearer sk_lgw_xxx → 命中 + enabled → 放行
//   范围：Scopes 限定允许调用的模型；请求 model 不在范围 → 403
//   限流：通过鉴权后，限流维度从"客户端自报"换成"akID（可信身份）"
//
// 面试讲点（为什么要哈希存而不是 AES 加密存）：
//   - AES 加密可解密回显，但 key 一旦可回显 = 泄露风险面大
//   - 哈希不可逆：就算数据库整个泄露，攻击者拿到的也是哈希，无法冒充调用
//   - 明文只在签发那一刻出现一次（GitHub token / OpenAI key 同款实践）
// ============================================================

const (
	akPrefix = "sk_lgw_" // 前缀：一眼认出是网关签发的 key（也可按前缀做存量清理）
	akLen    = 32        // 随机部分字节数 → 64 位 hex
)

// AccessKeyHandler AccessKey 管理接口处理器
type AccessKeyHandler struct {
	db *gorm.DB
}

// NewAccessKeyHandler 构造器
func NewAccessKeyHandler(db *gorm.DB) *AccessKeyHandler {
	return &AccessKeyHandler{db: db}
}

// accessKeyView 对外视图：key_hash 永不暴露，只给业务字段
func accessKeyView(ak model.AccessKey) gin.H {
	return gin.H{
		"id":          ak.ID,
		"name":        ak.Name,
		"scopes":      ak.Scopes,
		"enabled":     ak.Enabled,
		"last_used_at": ak.LastUsedAt,
		"created_at":  ak.CreatedAt,
	}
}

// genAccessKey 生成明文 key + 哈希
// 明文格式：sk_lgw_ + 32 随机字节的 hex（共 64 位）→ 信息熵 256bit，暴力枚举不可行
func genAccessKey() (plain string, hash string, err error) {
	buf := make([]byte, akLen)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	plain = akPrefix + hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plain))
	return plain, hex.EncodeToString(sum[:]), nil
}

// hashAccessKey 校验时对请求里的 key 做同样哈希，去库里比对
func hashAccessKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// accessKeyReq 创建/更新请求体
type accessKeyReq struct {
	Name    string `json:"name"`
	Scopes  string `json:"scopes"`  // 逗号分隔模型名；空 = 全部
	Enabled *bool  `json:"enabled"` // 指针：区分"没传"和"传 false"
}

// List GET /api/access-keys —— 列表（不含哈希，永远看不到明文）
func (h *AccessKeyHandler) List(c *gin.Context) {
	var keys []model.AccessKey
	if err := h.db.Order("created_at desc").Find(&keys).Error; err != nil {
		resp(c, 500, "查询 AccessKey 失败: "+err.Error(), nil)
		return
	}
	out := make([]gin.H, 0, len(keys))
	for _, k := range keys {
		out = append(out, accessKeyView(k))
	}
	resp(c, 0, "ok", out)
}

// Create POST /api/access-keys —— 签发新 key（明文只返回这一次）
func (h *AccessKeyHandler) Create(c *gin.Context) {
	var req accessKeyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		resp(c, 400, "请求体格式错误: "+err.Error(), nil)
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		resp(c, 400, "name 必填", nil)
		return
	}

	plain, hash, err := genAccessKey()
	if err != nil {
		resp(c, 500, "生成 AccessKey 失败: "+err.Error(), nil)
		return
	}
	ak := model.AccessKey{
		Name:    strings.TrimSpace(req.Name),
		KeyHash: hash,
		Scopes:  strings.TrimSpace(req.Scopes),
		Enabled: true,
	}
	if err := h.db.Create(&ak).Error; err != nil {
		resp(c, 500, "保存 AccessKey 失败: "+err.Error(), nil)
		return
	}
	log.Printf("[accesskey] 签发新 key id=%d name=%s scopes=%q", ak.ID, ak.Name, ak.Scopes)

	// 返回视图 + 明文（仅此一次；后续 List/Get 都不会再有 key 字段）
	out := accessKeyView(ak)
	out["key"] = plain
	resp(c, 0, "ok（密钥仅显示这一次，请立即保存）", out)
}

// Update PUT /api/access-keys/:id —— 改备注名 / 范围 / 启停（不换 key）
// 设计：启停是"吊销/恢复"的软开关；停用后该 key 立即 401，无需重新签发
func (h *AccessKeyHandler) Update(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		resp(c, 400, "id 非法", nil)
		return
	}
	var req accessKeyReq
	if err := c.ShouldBindJSON(&req); err != nil {
		resp(c, 400, "请求体格式错误: "+err.Error(), nil)
		return
	}

	var ak model.AccessKey
	if err := h.db.First(&ak, id).Error; err != nil {
		resp(c, 404, "AccessKey 不存在", nil)
		return
	}
	if req.Name != "" {
		ak.Name = req.Name
	}
	if req.Scopes != "" || req.Enabled != nil {
		if req.Scopes != "" {
			ak.Scopes = req.Scopes
		}
		if req.Enabled != nil {
			ak.Enabled = *req.Enabled
		}
	}
	if err := h.db.Save(&ak).Error; err != nil {
		resp(c, 500, "更新失败: "+err.Error(), nil)
		return
	}
	resp(c, 0, "ok", accessKeyView(ak))
}

// Delete DELETE /api/access-keys/:id —— 删除（彻底吊销）
func (h *AccessKeyHandler) Delete(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil {
		resp(c, 400, "id 非法", nil)
		return
	}
	if err := h.db.Delete(&model.AccessKey{}, id).Error; err != nil {
		resp(c, 500, "删除失败: "+err.Error(), nil)
		return
	}
	resp(c, 0, "ok", nil)
}

// contextKey AccessKey 信息在 gin.Context 里的 key（限流/计费从 context 取可信身份）
const contextKey = "accessKey"

// AccessKeyAuth 鉴权中间件：校验 Authorization: Bearer sk_lgw_xxx
//
// 用法：r.POST("/v1/chat/completions", handler.AccessKeyAuth(db), relay.ChatCompletions)
//
// 校验流程：
//  1. 取 Authorization → 不是 Bearer 开头 → 401
//  2. 对 key 做 SHA-256 → 查 access_keys
//  3. 命中且 enabled → 把 {id, name, scopes} 塞进 context → 放行
//  4. 未命中 / 已停用 → 401（停用 = 即时吊销）
//
// ⚠️ 与业务接口不同：这里必须用真正的 HTTP 401（AbortWithStatusJSON），
// 不用项目统一的 resp()（它 HTTP 恒 200）——客户端 SDK 靠 HTTP 状态码
// 识别"凭证无效要换 key"，HTTP 200 会被误判为成功。
// 失败文案统一且不泄露"是 key 不存在还是停用"（避免枚举探测）。
func AccessKeyAuth(db *gorm.DB) gin.HandlerFunc {
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": http.StatusUnauthorized, "message": "缺少 Bearer 凭证", "data": nil})
			return
		}
		plain := strings.TrimPrefix(auth, "Bearer ")

		var ak model.AccessKey
		if err := db.Where("key_hash = ?", hashAccessKey(plain)).First(&ak).Error; err != nil || !ak.Enabled {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": http.StatusUnauthorized, "message": "凭证无效或已停用", "data": nil})
			return
		}

		// 校验通过：更新 LastUsedAt（可审计；失败不计，避免无效 key 刷脏数据）
		if ak.LastUsedAt == nil || time.Since(*ak.LastUsedAt) > time.Minute {
			now := time.Now()
			ak.LastUsedAt = &now
			db.Model(&ak).Update("last_used_at", now)
		}

		c.Set(contextKey, gin.H{"id": ak.ID, "name": ak.Name, "scopes": ak.Scopes})
		c.Next()
	}
}

// AccessKeyFromCtx 从 context 取当前调用方身份（限流/计费维度）
// 返回 (akID, scopes)；没走鉴权的请求返回 (0, "")——兼容老链路，不 panic
func AccessKeyFromCtx(c *gin.Context) (uint, string) {
	if v, ok := c.Get(contextKey); ok {
		if m, ok := v.(gin.H); ok {
			id, _ := m["id"].(uint)
			scopes, _ := m["scopes"].(string)
			return id, scopes
		}
	}
	return 0, ""
}
