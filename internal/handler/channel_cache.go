package handler

// ============================================================
// 渠道配置缓存（W2 连接池/热更新：配置侧）
//
// 之前：每个请求都查一次数据库（pickChannel 里 db.Find）——
//       SQLite 本地没压力，但真实网关渠道表在 MySQL，每请求查库 =
//       每请求一次网络往返 + 数据库压力。
//
// 现在：内存快照 + 原子指针无锁读：
//   1. 启动/轮询时把"启用渠道列表"整份载入内存，构建不可变快照
//   2. 请求只读快照（atomic.Pointer.Load，一条原子读指令）
//   3. 渠道增删改后主动 Refresh() 换新快照；后台 ticker 兜底（防外部改库）
//
// 面试讲点：为什么 atomic.Pointer 而不是 RWMutex？
//   RWMutex：每个请求都要 RLock/RUnlock（锁开销 + 缓存行争用）
//   atomic.Pointer：Load 是原子读指令，无锁、无争用；
//   更新 = 构建新对象 → Store 换指针（Copy-on-Write），
//   读者要么拿到旧快照、要么拿到新快照，永远是一份完整一致的切片
// ============================================================

import (
	"log"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"llm-gateway/internal/model"
)

// channelSnapshot 渠道快照：不可变（只读），更新时整体替换
type channelSnapshot struct {
	channels []model.Channel // 只含启用渠道（enabled = true）
}

// channelCache 渠道配置缓存
type channelCache struct {
	db       *gorm.DB
	interval time.Duration               // 后台轮询周期（0 = 不轮询）
	snap     atomic.Pointer[channelSnapshot] // 原子指针：无锁读、Copy-on-Write 写
	stop     chan struct{}
}

// NewChannelCache 构造器：立即载入一次快照
func NewChannelCache(db *gorm.DB, interval time.Duration) *channelCache {
	c := &channelCache{
		db:       db,
		interval: interval,
		stop:     make(chan struct{}),
	}
	c.Refresh() // 启动先载入
	return c
}

// Start 启动后台轮询（兜底：渠道被外部直接改库时也能感知；CRUD 已主动刷新）
func (c *channelCache) Start() {
	if c.interval <= 0 {
		return
	}
	go func() {
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-ticker.C:
				c.Refresh()
			}
		}
	}()
}

// Stop 停止轮询（服务器退出时调用）
func (c *channelCache) Stop() {
	close(c.stop)
}

// Refresh 重新从数据库加载渠道快照（渠道 CRUD 后主动调用）
// 失败时保留旧快照：缓存宁可"旧"也不"空"，请求不受影响
func (c *channelCache) Refresh() {
	var list []model.Channel
	if err := c.db.Where("enabled = ?", true).Find(&list).Error; err != nil {
		log.Printf("[cache] 渠道快照刷新失败: %v（保留旧快照）", err)
		return
	}
	c.snap.Store(&channelSnapshot{channels: list}) // 换指针，读者无感知切换
}

// List 无锁读取当前快照（请求路径调用，替代每请求查库）
func (c *channelCache) List() []model.Channel {
	snap := c.snap.Load()
	if snap == nil {
		return nil
	}
	return snap.channels
}
