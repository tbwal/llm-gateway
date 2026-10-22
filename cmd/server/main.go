// Package main 程序入口：AI 网关从这里启动
package main

import (
	"log"

	"llm-gateway/config"
	"llm-gateway/internal/router"
	"llm-gateway/internal/storage"
)

// main 是唯一入口，运行 `go run ./cmd/server` 时从这里开始
func main() {
	// ① 加载配置（端口 + 数据库路径）
	cfg := config.Load()

	// ② 初始化数据库：打开 SQLite + 自动建表
	// 第 3 步起：db 句柄交给路由层 → 注入业务处理器使用
	db, err := storage.InitDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("数据库初始化失败: %v", err)
	}

	// ③ 创建 gin 引擎并注册路由（把 db 传给路由层做依赖注入）
	r := router.New(db)

	// ④ 启动 HTTP 服务并监听端口（阻塞运行）
	log.Printf("AI 网关启动: http://localhost:%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
