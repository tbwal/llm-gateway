// Package config 配置层：从环境变量加载服务配置
// 第 1 步只管理端口，后续在这里扩展数据库/Redis/AES 密钥等配置
package config

import (
	"os"
	"strconv"
)

// Config 服务配置结构体
type Config struct {
	Port   string // 监听端口（默认 8080）
	DBPath string // SQLite 数据库文件路径（默认 data/llm-gateway.db）
	// SecretKey 渠道 API Key 的加密密钥（环境变量 LLM_GATEWAY_SECRET_KEY，64 位 hex）
	// 未设置时返回空串，由 crypto.NewService 回退到开发默认密钥（仅限本地开发）
	SecretKey string

	// ── W3 限流配置 ──
	// RateLimitEnabled 是否开启限流（默认开：本地用 memory，生产切 redis）
	// RateLimitPerMin 每调用方每分钟最大请求数（滑动窗口；调小可验证 429）
	// RateLimitStore  限流存储：memory（默认，单机）| redis（多实例共享）
	// RedisAddr       Redis 地址（store=redis 时使用）
	RateLimitEnabled bool
	RateLimitPerMin  int
	RateLimitStore   string
	RedisAddr        string

	// ── 异步日志落库后端 ──
	// UsageLogBackend 异步写 usage_logs 的队列后端：
	//   memory（默认，单进程、重启丢队列里剩余消息）
	//   redis（Redis Stream，多实例共享队列、重启不丢、ACK 确认）
	// 与限流的 RateLimitStore 是独立决策：限流可以用 memory，日志可以用 redis
	UsageLogBackend string
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

	// ── W3 限流配置（RATE_LIMIT_* 环境变量）──
	// 默认开启 + memory 存储：本地零依赖跑通全流程；
	// 生产设置 RATE_LIMIT_STORE=redis + REDIS_ADDR 即切换为分布式限流
	rateLimitEnabled := os.Getenv("RATE_LIMIT_ENABLED") != "false" // 默认开，显式 "false" 才关
	rateLimitPerMin := 60                                          // 默认每调用方 60 次/分钟
	if v := os.Getenv("RATE_LIMIT_PER_MIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			rateLimitPerMin = n
		}
	}
	rateLimitStore := os.Getenv("RATE_LIMIT_STORE")
	if rateLimitStore == "" {
		rateLimitStore = "memory"
	}
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	// 异步日志后端：默认 memory；显式设 USAGE_LOG_BACKEND=redis 才切 Redis Stream
	usageLogBackend := os.Getenv("USAGE_LOG_BACKEND")
	if usageLogBackend == "" {
		usageLogBackend = "memory"
	}

	return Config{
		Port:              port,
		DBPath:            dbPath,
		SecretKey:         secretKey,
		RateLimitEnabled:  rateLimitEnabled,
		RateLimitPerMin:   rateLimitPerMin,
		RateLimitStore:    rateLimitStore,
		RedisAddr:         redisAddr,
		UsageLogBackend:   usageLogBackend,
	}
}
