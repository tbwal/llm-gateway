// Package main 程序入口：AI 网关从这里启动
package main

import (
	"io"
	"log"
	"net/http"
	_ "net/http/pprof" // pprof 分析端点：监听 6060，/debug/pprof/profile 采样 CPU
	"os"
	"path/filepath"

	"llm-gateway/config"
	"llm-gateway/internal/crypto"
	"llm-gateway/internal/handler"
	"llm-gateway/internal/router"
	"llm-gateway/internal/storage"
)

// main 是唯一入口，运行 `go run ./cmd/server` 时从这里开始
func main() {
	// ⓪ 全局日志同时落盘（管理台"请求日志"页可下载）：data/logs/global.log
	// tee 到 stdout（终端可见）+ 文件（可下载、可回溯）；失败静默——日志落盘不阻塞启动
	if f, err := os.OpenFile(filepath.Join("data", "logs", "global.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
		defer f.Close()
		log.SetOutput(io.MultiWriter(os.Stdout, f))
	}

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

	// ④.5 pprof 分析端点：独立起一个 6060 端口挂 DefaultServeMux
	// 用法：压负载时 `curl http://localhost:6060/debug/pprof/profile?seconds=10 -o cpu.prof`
	// 然后 `go tool pprof cpu.prof` 分析 CPU 热点（标准做法，线上排查利器）
	go func() {
		log.Printf("pprof 已启动: http://localhost:6060/debug/pprof/")
		if err := http.ListenAndServe(":6060", nil); err != nil {
			log.Printf("pprof 启动失败: %v", err)
		}
	}()

	// ⑤ 启动 HTTP 服务并监听端口（阻塞运行）
	//    r.Run 返回（Ctrl+C）后 defer 执行：drain 异步日志队列剩余条目
	defer handler.CloseUsageWriter()
	log.Printf("AI 网关启动: http://localhost:%s", cfg.Port)
	if err := r.Run(":" + cfg.Port); err != nil {
		log.Fatal(err)
	}
}
