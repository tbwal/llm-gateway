// Package storage 存储层：负责数据库的初始化与表结构维护
// 第 2 步只有"连接 + 自动建表"；第 3 步开始在这里封装增删改查
package storage

import (
	"log"
	"os"
	"path/filepath"

	"github.com/glebarez/sqlite" // 纯 Go 实现的 SQLite 驱动（免 CGO，Windows 直接可用）
	"gorm.io/gorm"

	"llm-gateway/internal/model"
)

// InitDB 初始化数据库：
//  1. 确保数据目录存在
//  2. 打开（或创建）SQLite 数据库文件
//  3. 自动迁移：把 model 里的结构体变成真实的表
//
// 返回 *gorm.DB（数据库句柄），后续所有读写都通过它
func InitDB(path string) (*gorm.DB, error) {
	// ① 确保数据库文件所在目录存在（data/ 不存在时 sqlite 会报错）
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	// ② 打开数据库：文件不存在会自动创建
	// gorm.Open(驱动, 配置) 是 GORM 的标准入口
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		return nil, err
	}

	// ③ 自动迁移：把结构体 Channel 变成真实表 channels
	// 效果等价于手写 CREATE TABLE IF NOT EXISTS channels (...)
	// 后续每新增一个模型（AccessKey、UsageLog），加一行 AutoMigrate 即可
	if err := db.AutoMigrate(&model.Channel{}, &model.UsageLog{}, &model.AccessKey{}, &model.Setting{}); err != nil {
		return nil, err
	}

	log.Printf("数据库就绪: %s（表 channels、usage_logs、access_keys、settings 已创建）", path)
	return db, nil
}
