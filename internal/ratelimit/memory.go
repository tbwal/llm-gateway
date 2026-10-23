package ratelimit

import (
	"sync"
	"time"
)

// memoryLimiter 内存版滑动窗口限流（本地开发用，单实例）
//
// 数据结构：map[key][]time.Time —— 每个 key 记录"滑动窗口内"的请求时间戳
// 每次 Allow：
//   1. 把窗口外（now - window 之前）的时间戳清掉
//   2. 看剩余个数是否已达上限：达 → 拒绝；未达 → 追加当前时间戳 + 放行
type memoryLimiter struct {
	mu      sync.Mutex
	buckets map[string][]time.Time
	limit   int
	window  time.Duration
}

// newMemoryLimiter 构造内存限流器
func newMemoryLimiter(limit int, window time.Duration) *memoryLimiter {
	return &memoryLimiter{
		buckets: make(map[string][]time.Time),
		limit:   limit,
		window:  window,
	}
}

// Allow 判断 key 是否放行（滑动窗口语义）
func (m *memoryLimiter) Allow(key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-m.window) // 窗口起点：now 往前推一个窗口

	// ① 清理窗口外的旧时间戳（滑动窗口的核心：旧请求"滑出"窗口）
	hits := m.buckets[key]
	kept := hits[:0] // 复用底层数组，避免每次分配（零拷贝过滤）
	for _, t := range hits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	// ② 判断是否已达上限
	if len(kept) >= m.limit {
		m.buckets[key] = kept
		return false, nil // 窗口内已满 → 拒绝
	}

	// ③ 记录本次请求 + 放行
	kept = append(kept, now)
	m.buckets[key] = kept
	return true, nil
}
