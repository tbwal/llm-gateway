// Package handler 业务处理层
package handler

import (
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"llm-gateway/config"
	"llm-gateway/internal/model"
	"llm-gateway/internal/ratelimit"
)

// ============================================================
// 限流设置（/api/system/ratelimit）
//
// 管理台"系统状态"页的数据源/控制口：
//   - GET  /api/system/ratelimit → 当前生效的限流配置（+ 持久化值）
//   - PUT  /api/system/ratelimit → 运行时改窗口上限（立即生效）+ 落库持久化
//
// 三层优先级（面试讲点）：
//   默认配置（env/默认 60）→ settings 表（Web 设置，重启保留，覆盖默认）→ 运行时 SetLimit
// 也就是说：在 Web 上改过一次后，重启也按 Web 的值走；env 只在 settings
// 从未设置时才生效（env 仍是"首次启动引导"，Web 是"运维持续设置"）。
// ============================================================

const settingRateLimitPerMin = "ratelimit_per_min"

// RateLimitHandler 限流设置接口处理器
type RateLimitHandler struct {
	db      *gorm.DB
	limiter ratelimit.Limiter // 可能为 nil：限流未开启（enabled=false 或初始化失败）
	cfg     config.Config
}

// NewRateLimitHandler 构造器：注入 db、限流器、配置
func NewRateLimitHandler(db *gorm.DB, limiter ratelimit.Limiter, cfg config.Config) *RateLimitHandler {
	return &RateLimitHandler{db: db, limiter: limiter, cfg: cfg}
}

// Get GET /api/system/ratelimit —— 当前限流配置 + 持久化值
func (h *RateLimitHandler) Get(c *gin.Context) {
	limit := h.cfg.RateLimitPerMin
	if h.limiter != nil {
		limit = h.limiter.Limit() // 运行时真实生效值（可能被 Web 改过）
	}
	persisted := ""
	if v, ok := h.loadSetting(c, settingRateLimitPerMin); ok {
		persisted = v
	}
	resp(c, http.StatusOK, "ok", gin.H{
		"enabled":        h.limiter != nil,
		"store":          h.cfg.RateLimitStore,
		"limit_per_min":  limit,
		"persisted":      persisted, // "" = 未持久化过（用默认）
		"redis_addr":     h.cfg.RedisAddr,
	})
}

// Update PUT /api/system/ratelimit —— 运行时改上限 + 落库
//
// body: {"per_min": 120}
// 校验：1 ≤ per_min ≤ 100000（防 0 值把限流整成"全拒"，防天文数字把限流整成"没限"）
func (h *RateLimitHandler) Update(c *gin.Context) {
	var req struct {
		PerMin int `json:"per_min"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		// 业务参数错误：用真 HTTP 400（resp() 恒 200，不适合"客户端参数错"语义）
		c.JSON(http.StatusBadRequest, gin.H{"error": "请求体格式错误: " + err.Error()})
		return
	}
	if req.PerMin < 1 || req.PerMin > 100000 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "per_min 必须在 1~100000 之间"})
		return
	}

	// ① 落库持久化（重启保留）
	if err := h.saveSetting(c, settingRateLimitPerMin, strconv.Itoa(req.PerMin)); err != nil {
		resp(c, http.StatusInternalServerError, "保存限流配置失败: "+err.Error(), nil)
		return
	}

	// ② 运行时生效（限流未开启时仅持久化，下次启动生效）
	note := "ok"
	if h.limiter != nil {
		h.limiter.SetLimit(req.PerMin)
		note = "已生效"
	} else {
		note = "已保存（限流当前未开启，重启后生效）"
	}
	log.Printf("[ratelimit] 限流设置更新: per_min=%d store=%s（%s）", req.PerMin, h.cfg.RateLimitStore, note)
	resp(c, http.StatusOK, note, gin.H{
		"enabled":       h.limiter != nil,
		"store":         h.cfg.RateLimitStore,
		"limit_per_min": req.PerMin,
	})
}

// loadSetting 读一个设置项；不存在返回 ("", false)
func (h *RateLimitHandler) loadSetting(c *gin.Context, key string) (string, bool) {
	var s model.Setting
	if err := h.db.Where("key = ?", key).First(&s).Error; err != nil {
		return "", false
	}
	return s.Value, true
}

// saveSetting upsert 一个设置项（GORM 主键存在则更新）
func (h *RateLimitHandler) saveSetting(c *gin.Context, key, value string) error {
	s := model.Setting{Key: key, Value: value}
	// OnConflict 语义：key 已存在 → 更新 value 与 updated_at
	return h.db.Save(&s).Error
}
