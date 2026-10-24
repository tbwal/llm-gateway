// Package handler 业务处理层
package handler

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"llm-gateway/config"
	"llm-gateway/internal/model"
)

// ============================================================
// 系统状态（/api/system/status）
//
// 管理台"系统状态"页的数据源：把网关运行时的可观测信息聚合到一个接口
//   - 服务：启动时间、uptime（进程级可观测）
//   - 限流：当前限流配置（enabled / store / limit / redis 地址）
//   - 安全：AES 密钥是否已显式配置（只给布尔，绝不暴露密钥本身）
//   - 缓存：渠道快照状态（启用数 / 总数 / 最近刷新时刻）
//   - 熔断：每个渠道的熔断状态快照（closed / open / half_open + 冷却剩余）
//   - AccessKey：已签发数量（凭证体系规模）
//
// 面试讲点：这类"状态聚合接口"是运维可观测性的最小闭环——
// 所有子系统（缓存/熔断/限流）都暴露只读快照方法，一个 handler 拼装，
// 页面一次请求看到全貌；不改动任何子系统内部实现。
// ============================================================

// SystemStatusHandler 系统状态接口处理器
type SystemStatusHandler struct {
	db        *gorm.DB
	cache     *channelCache
	breakers  *circuitBreakers
	cfg       config.Config
	startedAt time.Time // 进程启动时刻（uptime 基准）
}

// NewSystemStatusHandler 构造器：注入 db、缓存、熔断器、配置
func NewSystemStatusHandler(db *gorm.DB, cache *channelCache, breakers *circuitBreakers, cfg config.Config) *SystemStatusHandler {
	return &SystemStatusHandler{
		db:        db,
		cache:     cache,
		breakers:  breakers,
		cfg:       cfg,
		startedAt: time.Now(),
	}
}

// Status GET /api/system/status —— 聚合全部运行时状态
func (h *SystemStatusHandler) Status(c *gin.Context) {
	now := time.Now()

	// 渠道统计：启用数来自内存快照（转发侧真实口径），总数来自库
	enabledChannels := len(h.cache.List())
	var totalChannels int64
	if err := h.db.Model(&model.Channel{}).Count(&totalChannels).Error; err != nil {
		totalChannels = -1 // 查询失败显示 -1，避免 0 误导
	}

	// AccessKey 规模
	var akCount int64
	if err := h.db.Model(&model.AccessKey{}).Count(&akCount).Error; err != nil {
		akCount = -1
	}

	// 熔断器状态列表（合并渠道名，补齐未请求过的启用渠道）
	// ⚠️ 熔断器注册表是"懒创建"的：只有被路由过的渠道才有记录。
	//    直接输出会漏掉"从未被请求"的渠道（用户看到 3/3 启用却只列出 2 个会困惑），
	//    也会残留已删除渠道的记录。所以这里以渠道表为准做一次合并：
	//      - 熔断记录存在 → 补上渠道名
	//      - 渠道已被删除 → 过滤掉熔断残留
	//      - 启用渠道无熔断记录 → 补一条"正常、失败计数 0"
	var allChannels []model.Channel
	if err := h.db.Find(&allChannels).Error; err != nil {
		allChannels = nil
	}
	nameByID := make(map[uint]string)
	enabledIDs := make(map[uint]bool)
	for _, ch := range allChannels {
		nameByID[ch.ID] = ch.Name
		if ch.Enabled {
			enabledIDs[ch.ID] = true
		}
	}
	breakers := h.breakers.snapshotAll(now)
	merged := make([]gin.H, 0, len(breakers)+len(enabledIDs))
	seen := make(map[uint]bool)
	for _, b := range breakers {
		name, ok := nameByID[b.ChannelID]
		if !ok {
			continue // 渠道已删除：丢弃熔断残留记录
		}
		seen[b.ChannelID] = true
		merged = append(merged, gin.H{
			"channel_id":             b.ChannelID,
			"channel_name":           name,
			"state":                  b.State,
			"failures":               b.Failures,
			"opened_at":              b.OpenedAt,
			"cooldown_remaining_sec": b.CooldownRem,
		})
	}
	for _, id := range allChannels {
		if !enabledIDs[id.ID] || seen[id.ID] {
			continue
		}
		merged = append(merged, gin.H{
			"channel_id":   id.ID,
			"channel_name": id.Name,
			"state":        "closed", // 未请求过：没有熔断记录，视为正常
			"failures":     0,
		})
	}

	resp(c, http.StatusOK, "ok", gin.H{
		"server": gin.H{
			"started_at": h.startedAt.Format(time.RFC3339),
			"uptime":     now.Sub(h.startedAt).Round(time.Second).String(),
		},
		"rate_limit": gin.H{
			"enabled":       h.cfg.RateLimitEnabled,
			"store":         h.cfg.RateLimitStore,
			"limit_per_min": h.cfg.RateLimitPerMin,
			"redis_addr":    h.cfg.RedisAddr,
		},
		"security": gin.H{
			// 只暴露"是否已显式配置"，密钥内容绝不外泄
			"secret_key_set": h.cfg.SecretKey != "",
		},
		"cache": gin.H{
			"enabled_channels": enabledChannels,
			"total_channels":   totalChannels,
			"last_refresh":     h.cache.LastRefresh().Format(time.RFC3339),
		},
		"access_keys": akCount,
		"breakers":    merged,
	})
}
