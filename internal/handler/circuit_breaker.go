package handler

// ============================================================
// 渠道熔断器（W2：故障转移的升级）
//
// 已有能力：单次请求内的故障转移（tried 集合换渠道重试）
// 本文件：跨请求的熔断——连续失败 N 次的渠道自动摘除，冷却后恢复
//
// 状态机（三态）：
//   CLOSED（关闭=正常）──连续失败≥阈值──▶ OPEN（熔断=摘除）
//   OPEN（冷却窗口过后）──────────▶ HALF_OPEN（半开）
//   HALF_OPEN 探测成功 ──▶ CLOSED（恢复调度）
//   HALF_OPEN 探测失败 ──▶ OPEN（继续熔断，重记冷却）
//
// 恢复路径有两条（面试讲点）：
//   1. 被动：冷却期后转半开，放行 1 个真实请求"试水"，成功即恢复
//   2. 主动：后台健康探测 goroutine 定期 GET /health，成功直接恢复
// ============================================================

import (
	"sync"
	"time"
)

// breakerState 熔断状态
type breakerState int

const (
	stateClosed   breakerState = iota // 正常：参与调度
	stateOpen                         // 熔断：调度跳过
	stateHalfOpen                     // 半开：放行有限探测请求
)

// 熔断参数（演示用常量；生产可做成配置）
const (
	maxBreakerFailures = 3                // 连续失败几次触发熔断
	breakerCooldown    = 30 * time.Second // 熔断冷却窗口（OPEN 多久后允许试水）
)

// channelBreaker 单个渠道的熔断状态
// 并发安全：多个 goroutine 同时转发，状态读写必须加锁
type channelBreaker struct {
	mu       sync.Mutex
	state    breakerState
	failures int       // CLOSED 下连续失败次数（成功清零）
	openedAt time.Time // 进入 OPEN 的时刻（算冷却窗口）
	probed   bool      // HALF_OPEN 下是否已放行过探测请求（只放 1 个）
}

// allow 调度前的熔断决策：这个渠道现在能不能被选中？
func (b *channelBreaker) allow(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case stateOpen:
		// 冷却窗口过了吗？过了 → 转半开，这次放行就是"探测请求"
		// （立即占掉探测名额，后续并发请求被拒，等探测结果定生死）
		if now.Sub(b.openedAt) >= breakerCooldown {
			b.state = stateHalfOpen
			b.probed = true
			return true
		}
		return false // 冷却中：调度跳过该渠道
	case stateHalfOpen:
		// 半开只放行 1 个探测，其余拒绝（避免恢复瞬间被并发请求打爆）
		if b.probed {
			return false
		}
		b.probed = true
		return true
	default: // stateClosed
		return true
	}
}

// recordSuccess 一次转发成功（拿到上游响应）：清除失败计数；半开 → 关闭
func (b *channelBreaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	if b.state == stateHalfOpen {
		b.state = stateClosed
	}
}

// recordFailure 一次转发失败（网络错误/5xx/密钥解密失败）：连续失败累计，达阈值 → 熔断
func (b *channelBreaker) recordFailure(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.probed = false // 无论哪个状态，探测名额重置
	b.failures++
	// 半开状态下探测失败：立即回熔断（不用等凑满阈值）
	if b.state == stateHalfOpen || b.failures >= maxBreakerFailures {
		b.state = stateOpen
		b.openedAt = now
	}
}

// probeOK 健康探测成功（主动探活）：直接恢复 CLOSED，跳过半开等待
func (b *channelBreaker) probeOK() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = stateClosed
	b.failures = 0
	b.probed = false
}

// circuitBreakers 全渠道熔断器注册表（按渠道 ID 索引，懒创建）
type circuitBreakers struct {
	mu    sync.Mutex
	items map[uint]*channelBreaker
}

// newCircuitBreakers 构造器
func newCircuitBreakers() *circuitBreakers {
	return &circuitBreakers{items: make(map[uint]*channelBreaker)}
}

// get 取（或懒创建）某渠道的熔断器
func (cb *circuitBreakers) get(id uint) *channelBreaker {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	b, ok := cb.items[id]
	if !ok {
		b = &channelBreaker{state: stateClosed}
		cb.items[id] = b
	}
	return b
}

// allow 熔断决策入口（供 pickChannel 调用）
func (cb *circuitBreakers) allow(id uint, now time.Time) bool {
	return cb.get(id).allow(now)
}

// recordSuccess 转发成功回写入口
func (cb *circuitBreakers) recordSuccess(id uint) {
	cb.get(id).recordSuccess()
}

// recordFailure 转发失败回写入口
func (cb *circuitBreakers) recordFailure(id uint, now time.Time) {
	cb.get(id).recordFailure(now)
}

// probeOK 健康探测成功回写入口
func (cb *circuitBreakers) probeOK(id uint) {
	cb.get(id).probeOK()
}

// openIDs 当前处于 OPEN 状态的渠道 ID（供健康探测扫描）
func (cb *circuitBreakers) openIDs() []uint {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	var ids []uint
	for id, b := range cb.items {
		b.mu.Lock()
		if b.state == stateOpen {
			ids = append(ids, id)
		}
		b.mu.Unlock()
	}
	return ids
}

// String 熔断状态的可读名（系统状态页展示用）
func (s breakerState) String() string {
	switch s {
	case stateOpen:
		return "open"
	case stateHalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// breakerSnapshot 单个渠道熔断状态快照（系统状态页 / 面试演示用）
type breakerSnapshot struct {
	ChannelID   uint      `json:"channel_id"`
	State       string    `json:"state"`         // closed / open / half_open
	Failures    int       `json:"failures"`      // 累计连续失败（成功清零）
	OpenedAt    *time.Time `json:"opened_at,omitempty"` // 进入 OPEN 的时刻；nil = 从未熔断过（closed 不输出，避免零值 0001 年泄漏）
	CooldownRem float64   `json:"cooldown_remaining_sec"` // OPEN 剩余冷却秒（0 = 不在冷却中）
}

// snapshotAll 全渠道熔断状态快照（遍历注册表，双锁：外层注册表锁 + 每个熔断器自己的锁）
func (cb *circuitBreakers) snapshotAll(now time.Time) []breakerSnapshot {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	out := make([]breakerSnapshot, 0, len(cb.items))
	for id, b := range cb.items {
		b.mu.Lock()
		s := breakerSnapshot{ChannelID: id, State: b.state.String(), Failures: b.failures}
		if b.state == stateOpen {
			t := b.openedAt
			s.OpenedAt = &t // 只在 OPEN 时输出熔断时刻；closed/half_open 不输出
			remain := breakerCooldown - now.Sub(b.openedAt)
			if remain < 0 {
				remain = 0
			}
			s.CooldownRem = remain.Seconds()
		}
		out = append(out, s)
		b.mu.Unlock()
	}
	return out
}
