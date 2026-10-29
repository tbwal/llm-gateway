package scheduler

import (
	"sync"
	"testing"
	"time"
)

// cands 便捷构造候选列表
func cands(ids ...uint) []Candidate {
	out := make([]Candidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, Candidate{ID: id, Weight: 1})
	}
	return out
}

// TestFairUniform 均匀性：5 个同权重渠道选 10000 次，每个渠道被选次数 ≈ 2000（±5%）
func TestFairUniform(t *testing.T) {
	s := New(0)
	now := time.Now()
	counts := map[uint]int{}
	c := cands(1, 2, 3, 4, 5)
	for i := 0; i < 10000; i++ {
		idx, ok := s.Select(c, "", now)
		if !ok {
			t.Fatal("Select 返回 false")
		}
		counts[c[idx].ID]++
	}
	for id, n := range counts {
		if n < 1900 || n > 2100 {
			t.Errorf("渠道 %d 被选 %d 次，期望在 [1900,2100]", id, n)
		}
	}
}

// TestFairWeighted 权重比例：weight 1:2:1 选 12000 次 → 比例 ≈ 1:2:1（±5%）
func TestFairWeighted(t *testing.T) {
	s := New(0)
	now := time.Now()
	c := []Candidate{{ID: 1, Weight: 1}, {ID: 2, Weight: 2}, {ID: 3, Weight: 1}}
	counts := map[uint]int{}
	for i := 0; i < 12000; i++ {
		idx, _ := s.Select(c, "", now)
		counts[c[idx].ID]++
	}
	// 期望 A=3000、B=6000、C=3000，容差 ±5%
	expect := map[uint]int{1: 3000, 2: 6000, 3: 3000}
	for id, want := range expect {
		got := counts[id]
		if got < want-want/20 || got > want+want/20 {
			t.Errorf("渠道 %d 被选 %d 次，期望 ≈ %d", id, got, want)
		}
	}
}

// TestShortWindowNoJitter 短窗口防抖：同权重 5 渠道连选 20 次，单渠道出现次数 ≤ 6
// （加权随机在 20 次里可能连续 8+ 次打中同一个；公平调度轮流，理论上每渠道 4 次）
func TestShortWindowNoJitter(t *testing.T) {
	s := New(0)
	now := time.Now()
	c := cands(1, 2, 3, 4, 5)
	counts := map[uint]int{}
	for i := 0; i < 20; i++ {
		idx, _ := s.Select(c, "", now)
		counts[c[idx].ID]++
	}
	for id, n := range counts {
		if n > 6 {
			t.Errorf("渠道 %d 短窗口内被选 %d 次（>6，有抖动/霸屏嫌疑）", id, n)
		}
	}
}

// TestCooldown 模型冷却：渠道 1 对 gpt-5 失败 → 冷却期内选不到；对 deepseek-chat 不受影响；过期后恢复
func TestCooldown(t *testing.T) {
	s := New(10 * time.Second)
	now := time.Now()
	c := []Candidate{{ID: 1, Weight: 1}, {ID: 2, Weight: 1}}

	// 渠道 1 对 gpt-5 失败
	s.RecordFailure(1, "gpt-5", now)

	// 冷却期内：选 gpt-5 → 只剩渠道 2
	idx, ok := s.Select(c, "gpt-5", now)
	if !ok || c[idx].ID != 2 {
		t.Fatalf("冷却期内选 gpt-5 应选到渠道 2，got idx=%d id=%d ok=%v", idx, c[idx].ID, ok)
	}
	// 冷却期内：选 deepseek-chat → 渠道 1 仍可用
	idx2, ok2 := s.Select(c, "deepseek-chat", now)
	if !ok2 || c[idx2].ID != 1 {
		t.Fatalf("冷却只摘 gpt-5，(渠道1,deepseek-chat) 应仍可选，got id=%d ok=%v", c[idx2].ID, ok2)
	}
	// 冷却期后：gpt-5 恢复可再选渠道 1
	later := now.Add(10*time.Second + time.Second)
	got := map[uint]int{}
	for i := 0; i < 100; i++ {
		idx3, _ := s.Select(c, "gpt-5", later)
		got[c[idx3].ID]++
	}
	if got[1] == 0 {
		t.Error("冷却过期后渠道 1 应恢复参与 gpt-5 调度")
	}
}

// TestConcurrent 并发安全：8 协程并发 Select，-race 下不应有数据竞争
func TestConcurrent(t *testing.T) {
	s := New(0)
	now := time.Now()
	c := cands(1, 2, 3, 4, 5, 6)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				s.Select(c, "", now)
			}
		}()
	}
	wg.Wait()
}

// TestSelectEmpty 空候选 / 全冷却：返回 false
func TestSelectEmpty(t *testing.T) {
	s := New(0)
	now := time.Now()
	if _, ok := s.Select(nil, "", now); ok {
		t.Error("空候选应返回 false")
	}
	s.RecordFailure(1, "gpt-5", now)
	if _, ok := s.Select([]Candidate{{ID: 1, Weight: 1}}, "gpt-5", now); ok {
		t.Error("唯一候选在冷却中应返回 false")
	}
}
