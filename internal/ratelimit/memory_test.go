package ratelimit

import (
	"testing"
	"time"
)

// TestMemoryLimiterWindow 窗口内限流：放行 limit 次，第 limit+1 次拒绝
func TestMemoryLimiterWindow(t *testing.T) {
	lim := newMemoryLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		ok, err := lim.Allow("user-1")
		if err != nil {
			t.Fatalf("Allow() error = %v", err)
		}
		if !ok {
			t.Fatalf("第 %d 次请求应放行，却被拒绝", i+1)
		}
	}

	// 第 4 次：窗口内已满 3 个 → 拒绝
	ok, err := lim.Allow("user-1")
	if err != nil {
		t.Fatalf("Allow() error = %v", err)
	}
	if ok {
		t.Fatal("窗口内第 4 次请求应被拒绝，却被放行")
	}

	// 不同 key 互不影响
	ok, _ = lim.Allow("user-2")
	if !ok {
		t.Fatal("不同 key 应独立计数，user-2 应放行")
	}
}

// TestMemoryLimiterSlide 滑动窗口：旧请求滑出窗口后恢复放行
func TestMemoryLimiterSlide(t *testing.T) {
	// 窗口 100ms，limit 1：第一个请求在窗口内，第二个在窗口外
	lim := newMemoryLimiter(1, 100*time.Millisecond)

	ok, _ := lim.Allow("k")
	if !ok {
		t.Fatal("第一次应放行")
	}
	ok, _ = lim.Allow("k")
	if ok {
		t.Fatal("窗口内第二次应拒绝")
	}

	// 等窗口滑过（超过 100ms 后旧请求过期）
	time.Sleep(120 * time.Millisecond)
	ok, _ = lim.Allow("k")
	if !ok {
		t.Fatal("窗口滑动后应恢复放行")
	}
}
