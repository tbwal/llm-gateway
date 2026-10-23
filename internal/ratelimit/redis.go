package ratelimit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisLimiter Redis 版滑动窗口限流（生产多实例共享状态）
//
// 为什么用 ZSET + Lua：
//   - ZSET（有序集合）：member = 请求的唯一标识，score = 请求时间戳（毫秒），
//     ZREMRANGEBYSCORE 清窗口外的记录、ZCARD 数窗口内个数——天然表达滑动窗口
//   - Lua 脚本：Redis 单线程执行整个脚本，**原子性**由服务器保证——
//     并发请求不会出现"两个都查到 count=99 都放行"的竞态
type redisLimiter struct {
	client *redis.Client
	limit  int
	window time.Duration
}

// slidingWindowLua 一次调用完成 清过期 → 计数 → 记录 → 设过期
// 一个脚本做完所有事 = 原子（不会中途被其他请求插队）
//
// KEYS[1] = 限流 key（如 ratelimit:user-1）
// ARGV[1] = 当前时间戳（毫秒）      ARGV[2] = 窗口时长（毫秒）
// ARGV[3] = 窗口内上限              ARGV[4] = 本次请求唯一 member（防同毫秒覆盖）
const slidingWindowLua = `
local key = KEYS[1]
local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local limit = tonumber(ARGV[3])
-- ① 清掉窗口外的旧记录（滑动窗口核心：旧请求"滑出"窗口）
redis.call('ZREMRANGEBYSCORE', key, 0, now - window)
-- ② 数窗口内剩余记录
local count = redis.call('ZCARD', key)
if count < limit then
    -- ③ 未满：记录本次请求（member 唯一，防同毫秒 ZADD 覆盖）+ 放行
    redis.call('ZADD', key, now, ARGV[4])
    -- ④ 给 key 设过期（窗口 + 1s 余量），防止 key 永不删除占内存
    redis.call('PEXPIRE', key, window + 1000)
    return 1
end
-- 已满：拒绝（不记录本次请求）
return 0
`

// newRedisLimiter 构造 Redis 限流器并做连接自检
func newRedisLimiter(addr string, limit int, window time.Duration) (*redisLimiter, error) {
	client := redis.NewClient(&redis.Options{Addr: addr})

	// 连接自检：Redis 不可用直接报错（启动时暴露配置错误，而不是运行时才炸）
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		return nil, fmt.Errorf("Redis 连接失败 (%s): %w", addr, err)
	}
	return &redisLimiter{client: client, limit: limit, window: window}, nil
}

// Allow 判断 key 是否放行（Redis 原子滑动窗口）
func (r *redisLimiter) Allow(key string) (bool, error) {
	// 单次限流判定给 500ms 预算：Redis 正常时 Lua 是亚毫秒级的，
	// 只有 Redis 故障/网络异常才会触达超时（此时返回错误 → 调用方 fail-open）
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	now := time.Now().UnixMilli()
	member := fmt.Sprintf("%d:%s", now, randomSuffix()) // 唯一 member：毫秒时间戳 + 随机串
	res, err := r.client.Eval(ctx, slidingWindowLua,
		[]string{"ratelimit:" + key},
		now, r.window.Milliseconds(), r.limit, member,
	).Int()
	if err != nil {
		return false, err
	}
	return res == 1, nil
}

// randomSuffix 生成 8 位 hex 随机串（member 唯一性）
func randomSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano()) // 兜底：极端情况下用纳秒时间戳
	}
	return hex.EncodeToString(b)
}
