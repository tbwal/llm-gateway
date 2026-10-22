// Package config 配置层：从环境变量加载服务配置
// 第 1 步只管理端口，后续在这里扩展数据库/Redis/AES 密钥等配置
package config

import "os"

// Config 服务配置结构体
type Config struct {
	Port   string // 监听端口（默认 8080）
	DBPath string // SQLite 数据库文件路径（默认 data/llm-gateway.db）
	// SecretKey 渠道 API Key 的加密密钥（环境变量 LLM_GATEWAY_SECRET_KEY，64 位 hex）
	// 未设置时返回空串，由 crypto.NewService 回退到开发默认密钥（仅限本地开发）
	SecretKey string
}

// Load 从环境变量读取配置，未设置时使用默认值
func Load() Config {
	// os.Getenv("PORT")：从"环境变量"里读端口号
	//   - 环境变量 = 操作系统级的配置项（key=value 形式），如 PORT=18080
	//   - 没设置时返回空字符串 ""（不会报错）
	//   - 这样改端口只需设置环境变量，不用改代码、不用重新编译

	//命令行指定：
	//$env:PORT = 18080    # 设置环境变量
	//go run ./cmd/server  # 代码一行没改，服务就跑到 18080
	port := os.Getenv("PORT")
	if port == "" { // 未设置或为空 → 用兜底默认值
		port = "8080"
	}

	// DB_PATH 同理：SQLite 数据库文件路径，默认 data/llm-gateway.db
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		dbPath = "data/llm-gateway.db"
	}

	// LLM_GATEWAY_SECRET_KEY：渠道 API Key 的 AES 加密密钥
	// 生产环境必须设置；留空则由 crypto.NewService 使用开发默认密钥（并打警告日志）
	secretKey := os.Getenv("LLM_GATEWAY_SECRET_KEY")
	return Config{Port: port, DBPath: dbPath, SecretKey: secretKey}
}
