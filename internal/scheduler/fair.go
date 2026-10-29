// Package scheduler 公平调度器（Fair Scheduler）
//
// 为什么做（面试讲点）：
//
//	原来渠道调度是"加权随机"——rand.IntN 决定，短窗口内可能连续 N 次打中
//	同一个渠道（随机抖动），其他渠道空转；而且无状态，谁被饿死不感知。
//	公平调度 = 给每个渠道一个"欠账本"，永远选欠得最多的，负反馈自动均衡：
//	  - 确定性（无随机）→ 可复现、可测试、可推理
//	  - 比例锁死（weight 大 → share 涨得慢 → 被选更频繁）
//	  - 无饿死（欠账最多的永远优先被补）
//	思想同源：Linux CFS 调度器的 vruntime——"选最落后、按权重记账"。
package scheduler

import (
	"sync"
	"time"
)

const (
	// maxConsecutive 防霸屏阈值：同一渠道连续被选这么多次且还有别的候选 → 强制让位
	maxConsecutive = 8
	// defaultCooldown 默认模型冷却时长：某渠道对某模型失败后，期内不再选它服务该模型
	defaultCooldown = 10 * time.Second
)

// Candidate 调度候选渠道。
// 刻意只暴露 ID + Weight：scheduler 包不依赖业务 model，便于独立测试与复用。
type Candidate struct {
	ID     uint
	Weight int
}

// member 调度账本里"一个渠道"的一行
type member struct {
	ChannelID    uint
	Weight       int
	Progress     uint64 // 累计被选次数（share = Progress/Weight）
	LastSelected uint64 // 上次被选时的全局序号（并列时的 tie-break：更久没被选的先）
	Consecutive  int    // 连续被选次数（防霸屏用）
}

// FairScheduler 进程内公平调度器（单实例，全局一把锁——选择是微秒级操作，锁竞争可忽略）
type FairScheduler struct {
	mu            sync.Mutex
	members       map[uint]*member
	seq           uint64                        // 全局单调序号（LastSelected 赋值来源）
	watermark     uint64                        // 全体成员最大 Progress（新渠道入场校准基线）
	lastChannelID uint                          // 上次选中的渠道（Consecutive 累计用）
	cooldowns     map[uint]map[string]time.Time // channelID → model → 冷却到期时间
	cooldown      time.Duration
}

// New 创建公平调度器；cooldown ≤ 0 时用默认 10s
func New(cooldown time.Duration) *FairScheduler {
	if cooldown <= 0 {
		cooldown = defaultCooldown
	}
	return &FairScheduler{
		members:   make(map[uint]*member),
		cooldowns: make(map[uint]map[string]time.Time),
		cooldown:  cooldown,
	}
}

// Select 从候选里公平选一个渠道，返回其在 candidates 里的下标。
// modelName：候选里正在冷却（该渠道×该模型失败过）的会被跳过。
// 没有可用候选时返回 (-1, false)。调用方保持 candidates 顺序不变。
func (s *FairScheduler) Select(candidates []Candidate, modelName string, now time.Time) (int, bool) {
	if len(candidates) == 0 {
		return -1, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// ① 冷却过滤：该渠道正在冷却此模型的，踢出候选（存原下标，返回时索引语义不变）
	eligible := make([]int, 0, len(candidates))
	for i, cand := range candidates {
		if s.inCooldownLocked(cand.ID, modelName, now) {
			continue
		}
		eligible = append(eligible, i)
	}
	if len(eligible) == 0 {
		return -1, false
	}

	// ② 入场校准：新渠道初始 Progress = watermark（当前最高进度）
	//    防止"新渠道 Progress=0 → share 最小 → 一上线就被打爆"
	for _, i := range eligible {
		cand := candidates[i]
		if _, ok := s.members[cand.ID]; !ok {
			s.members[cand.ID] = &member{
				ChannelID: cand.ID,
				Weight:    weightAtLeast1(cand.Weight),
				Progress:  s.watermark,
			}
		}
	}

	// ③ 选 share 最小（欠得最多的）；并列 → LastSelected 更早；再并列 → ID 更小
	best := eligible[0]
	for _, i := range eligible[1:] {
		if s.less(candidates[i], candidates[best]) {
			best = i
		}
	}

	// ④ 防霸屏：同一个渠道连续被选 ≥ 阈值且还有别的候选 → 强制选次优的
	if m := s.members[candidates[best].ID]; m.Consecutive >= maxConsecutive && len(eligible) > 1 {
		second := -1
		for _, i := range eligible {
			if i == best {
				continue
			}
			if second == -1 || s.less(candidates[i], candidates[second]) {
				second = i
			}
		}
		if second >= 0 {
			best = second
		}
	}

	// ⑤ 记账：Progress+1、seq++、LastSelected=seq、Consecutive 累计/重置、watermark 抬升
	m := s.members[candidates[best].ID]
	m.Progress++
	s.seq++
	m.LastSelected = s.seq
	if m.ChannelID == s.lastChannelID {
		m.Consecutive++
	} else {
		m.Consecutive = 1
	}
	s.lastChannelID = m.ChannelID
	if m.Progress > s.watermark {
		s.watermark = m.Progress
	}
	return best, true
}

// RecordFailure 记录一次失败：该渠道对 modelName 进入冷却期（期内 Select 不再选它服务此模型）
// 与熔断的区别（面试讲点）：熔断是"渠道级"（整个渠道挂掉才摘除），
// 这是"渠道×模型级"——A 对 gpt-5 连续失败，只摘 (A, gpt-5)，A 对 deepseek-chat 照常转发。
func (s *FairScheduler) RecordFailure(chID uint, modelName string, now time.Time) {
	if modelName == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cooldowns[chID] == nil {
		s.cooldowns[chID] = make(map[string]time.Time)
	}
	s.cooldowns[chID][modelName] = now.Add(s.cooldown)
}

// less 判断 a 是否比 b 更"落后"（share 更小）
// share = Progress / Weight，交叉乘法避免浮点误差：
//
//	aP/aW < bP/bW  ⟺  aP*bW < bP*aW
func (s *FairScheduler) less(a, b Candidate) bool {
	ma, mb := s.members[a.ID], s.members[b.ID]
	aShare := ma.Progress * uint64(mb.Weight)
	bShare := mb.Progress * uint64(ma.Weight)
	if aShare != bShare {
		return aShare < bShare
	}
	if ma.LastSelected != mb.LastSelected {
		return ma.LastSelected < mb.LastSelected
	}
	return ma.ChannelID < mb.ChannelID
}

func (s *FairScheduler) inCooldownLocked(chID uint, modelName string, now time.Time) bool {
	if modelName == "" {
		return false
	}
	m, ok := s.cooldowns[chID]
	if !ok {
		return false
	}
	until, ok := m[modelName]
	return ok && now.Before(until)
}

func weightAtLeast1(w int) int {
	if w < 1 {
		return 1
	}
	return w
}
