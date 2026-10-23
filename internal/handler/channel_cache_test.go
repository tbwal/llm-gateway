package handler

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"llm-gateway/internal/model"
)

// newTestDB 建一个内存 SQLite 库并迁移渠道表（单测用）
func newTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("gorm.Open() error = %v", err)
	}
	if err := db.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("AutoMigrate() error = %v", err)
	}
	return db
}

func mustInsertChannel(t *testing.T, db *gorm.DB, name string, enabled bool) uint {
	t.Helper()
	// 用 map 插入：Enabled 有 default:true 标签，结构体 Create 时零值（false）会被
	// 数据库默认值 true 覆盖（GORM 经典零值陷阱，Select("*") 也压不住）；
	// map 插入直接按值写，能真实写入 enabled=false
	vals := map[string]interface{}{
		"name": name, "provider": "openai", "base_url": "http://localhost:9090/v1",
		"api_key": "sk-test", "weight": 1, "enabled": enabled,
	}
	if err := db.Model(&model.Channel{}).Create(vals).Error; err != nil {
		t.Fatalf("Create(%q) error = %v", name, err)
	}
	var ch model.Channel
	if err := db.Where("name = ?", name).First(&ch).Error; err != nil {
		t.Fatalf("回查渠道(%q) error = %v", name, err)
	}
	return ch.ID
}

// TestChannelCacheRefresh 快照只含启用渠道，且 Refresh 后能看到最新数据
func TestChannelCacheRefresh(t *testing.T) {
	db := newTestDB(t)
	_ = mustInsertChannel(t, db, "A-开", true)
	_ = mustInsertChannel(t, db, "B-关", false)

	cache := NewChannelCache(db, 0) // 不启动轮询，只测手动刷新
	if got := len(cache.List()); got != 1 {
		t.Fatalf("初始快照渠道数 = %d, want 1（禁用渠道不应进快照）", got)
	}

	// 新增一个启用渠道 + 停用原来的 → 刷新后快照应换成新集合
	_ = mustInsertChannel(t, db, "C-开", true)
	if err := db.Model(&model.Channel{}).Where("name = ?", "A-开").Update("enabled", false).Error; err != nil {
		t.Fatalf("Update enabled error = %v", err)
	}
	cache.Refresh()

	got := cache.List()
	if len(got) != 1 {
		t.Fatalf("刷新后快照渠道数 = %d, want 1", len(got))
	}
	if got[0].Name != "C-开" {
		t.Fatalf("刷新后快照渠道 = %q, want %q", got[0].Name, "C-开")
	}
}

// TestChannelCacheListStable 并发读快照是安全的（无锁读的竞态护栏）
func TestChannelCacheListStable(t *testing.T) {
	db := newTestDB(t)
	for i := 0; i < 20; i++ {
		_ = mustInsertChannel(t, db, "开", true)
	}
	cache := NewChannelCache(db, 0)

	done := make(chan struct{})
	go func() { // 后台不停刷新（换指针）
		defer close(done)
		for i := 0; i < 100; i++ {
			cache.Refresh()
		}
	}()
	for i := 0; i < 500; i++ { // 主协程并发读
		if n := len(cache.List()); n != 20 {
			t.Fatalf("并发读快照渠道数 = %d, want 20", n)
		}
	}
	<-done
}
