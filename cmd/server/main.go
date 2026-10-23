// Package main 程序入口：AI 网关从这里启动
package main

import (
	"log"

	"llm-gateway/config"
	"llm-gateway/internal/crypto"
	"llm-gateway/internal/router"
	"llm-gateway/internal/storage"
)

// main 是唯一入口，运行 `go run ./cmd/server` 时从这里开始
func main() {
	// ① 加载配置（端口 + 数据库路径 + 加密密钥）
	cfg := config.Load()

	// ② 初始化数据库：打开 SQLite + 自动建表
	// 第 3 步起：db 句柄交给路由层 → 注入业务处理器使用
	db, err := storage.InitDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("数据库初始化失败: %v", err)
	}

	// ③ 初始化加密服务：渠道 API Key 落库前加密、转发前解密
	// 密钥来自环境变量 LLM_GATEWAY_SECRET_KEY（64 位 hex）；
	// 未设置则显式回退到开发默认密钥（仅限本地开发，生产必须显式配置）
	//
	// ⚠️ 回退逻辑放在这里而不是 crypto.NewService 里，原因：
	//    crypto 包保持"空密钥一律拒绝"的安全语义——任何调用点误传空串都会立刻报错，
	//    而不是悄悄用默认密钥；开发便利由入口层（main）负责，并打印警告。
	secretKey := cfg.SecretKey
	if secretKey == "" {
		secretKey = crypto.DefaultSecretKey
		log.Println("⚠️ 未设置 LLM_GATEWAY_SECRET_KEY，正在使用开发默认密钥（仅限本地开发，生产环境请务必设置）")
	}
	enc, err := crypto.NewService(secretKey)
	if err != nil {
		log.Fatalf("加密服务初始化失败: %v", err)
	}

	// ④ 创建 gin 引擎并注册路由（把 db、加密服务、配置传给路由层做依赖注入）
	r := router.New(db, enc, cfg)

	// ⑤ 启动 HTTP 服务并监听端口（阻塞运行）
	log.Printf("AI 网关启动: http://localhost:%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
