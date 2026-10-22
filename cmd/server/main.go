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
	// 第 2 步先只确认"数据库能开、表能建"；第 3 步开始把 db 传给业务层使用
	_, err := storage.InitDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("数据库初始化失败: %v", err)
	}

	// ③ 创建 gin 引擎并注册路由（目前只有 /health）
	r := router.New()

	// ④ 启动 HTTP 服务并监听端口（阻塞运行）
	log.Printf("AI 网关启动: http://localhost:%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
