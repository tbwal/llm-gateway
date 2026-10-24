// Package ratelimit W3 分布式限流：滑动窗口算法
//
// 为什么用"滑动窗口"而不是"固定窗口 + INCR"：
//   固定窗口（Redis INCR + EXPIRE）在窗口边界有突刺——
//   59:59 秒用满 100 次，60:00 窗口重置又可用满 100 次，
//   一瞬间实际放行 200 次（两个窗口各打满）。
//   滑动窗口按"每个请求的真实时间戳"统计最近 window 内的次数，
//   任何时刻看到的都是"过去 N 秒真实发生的请求数"，没有突刺。
//
// 两种实现：
//   - memoryLimiter：单机内存版（本地开发，无外部依赖）
//   - redisLimiter：Redis ZSET + Lua 版（生产多实例共享状态，原子）
package ratelimit

import "time"

// Limiter 限流器接口
//
// Allow 判断 key 是否放行：
//   - 返回 (true, nil)   → 放行（并记录这次请求）
//   - 返回 (false, nil)  → 拒绝（滑动窗口内已满）
//   - 返回 (_, err)      → 限流器自身故障（如 Redis 连不上）
//                         调用方决定降级策略（网关通常 fail-open：放行 + 打日志，
//                         避免"限流组件挂了导致整个网关不可用"）
type Limiter interface {
	Allow(key string) (bool, error)
	// SetLimit 运行时调整窗口内上限（管理台"限流设置"用，立即生效，无需重启）
	SetLimit(limit int)
	// Limit 返回当前生效的窗口内上限（系统状态页展示实时值）
	Limit() int
}

// New 按配置创建限流器
//
//	store:     "memory"（默认，本地开发）| "redis"（生产多实例）
//	limit:     窗口内最大请求数
//	window:    滑动窗口时长（如 time.Minute）
//	redisAddr: store=redis 时的连接地址（如 "localhost:6379"）
func New(store string, limit int, window time.Duration, redisAddr string) (Limiter, error) {
	if store == "redis" {
		return newRedisLimiter(redisAddr, limit, window)
	}
	return newMemoryLimiter(limit, window), nil
}
