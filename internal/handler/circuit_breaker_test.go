package handler

import (
	"testing"
	"time"
)

// 熔断状态机单测：CLOSED → OPEN → HALF_OPEN → CLOSED / OPEN 全链路
// 不依赖服务，纯逻辑验证（time.Now 直接传入，可控）

// 连续失败 3 次触发熔断（OPEN），冷却中调度跳过
func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	b := &channelBreaker{state: stateClosed}
	now := time.Now()

	// 前 2 次失败：还没到阈值，保持 CLOSED
	b.recordFailure(now)
	if b.state != stateClosed {
		t.Fatalf("第1次失败后 state=%v, want closed", b.state)
	}
	b.recordFailure(now)
	if b.state != stateClosed {
		t.Fatalf("第2次失败后 state=%v, want closed", b.state)
	}
	if !b.allow(now) {
		t.Fatal("CLOSED 状态 allow 应放行")
	}

	// 第 3 次失败：达阈值 → OPEN
	b.recordFailure(now)
	if b.state != stateOpen {
		t.Fatalf("第3次失败后 state=%v, want open", b.state)
	}
	// 冷却中：allow 拒绝
	if b.allow(now) {
		t.Fatal("OPEN 冷却中 allow 应拒绝")
	}
	// 冷却还没到：仍然拒绝
	if b.allow(now.Add(breakerCooldown - time.Second)) {
		t.Fatal("冷却未满 allow 应拒绝")
	}
}

// 冷却窗口过后：转半开放行 1 个探测，成功即恢复 CLOSED
func TestBreakerHalfOpenRecoversOnSuccess(t *testing.T) {
	b := &channelBreaker{state: stateClosed}
	now := time.Now()

	// 打满失败进 OPEN
	for i := 0; i < maxBreakerFailures; i++ {
		b.recordFailure(now)
	}

	// 冷却过后第一次 allow → 转 HALF_OPEN 并放行
	if !b.allow(now.Add(breakerCooldown + time.Second)) {
		t.Fatal("冷却后首次 allow 应放行（转半开探测）")
	}
	if b.state != stateHalfOpen {
		t.Fatalf("冷却后状态=%v, want half_open", b.state)
	}

	// 半开期间只放 1 个：第二个并发请求被拒绝
	if b.allow(now.Add(breakerCooldown + 2*time.Second)) {
		t.Fatal("HALF_OPEN 第二个请求应拒绝（只放 1 个探测）")
	}

	// 探测成功 → CLOSED
	b.recordSuccess()
	if b.state != stateClosed {
		t.Fatalf("半开探测成功后 state=%v, want closed", b.state)
	}
	// 恢复后 allow 放行
	if !b.allow(now.Add(breakerCooldown + 3*time.Second)) {
		t.Fatal("CLOSED 状态 allow 应放行")
	}
}

// 半开探测失败：立即回 OPEN，不等待凑满阈值
func TestBreakerHalfOpenFailsBackToOpen(t *testing.T) {
	b := &channelBreaker{state: stateClosed}
	now := time.Now()
	for i := 0; i < maxBreakerFailures; i++ {
		b.recordFailure(now)
	}

	// 冷却后放行探测
	b.allow(now.Add(breakerCooldown + time.Second))
	// 探测失败：1 次就回 OPEN（半开不按阈值）
	b.recordFailure(now.Add(breakerCooldown + time.Second))
	if b.state != stateOpen {
		t.Fatalf("半开探测失败后 state=%v, want open", b.state)
	}
	// 重新冷却：立刻再问还是拒绝
	if b.allow(now.Add(breakerCooldown + 2*time.Second)) {
		t.Fatal("重新熔断后冷却未满应拒绝")
	}
}

// 成功请求清除失败计数：失败 2 次后成功，再失败不会因旧计数误熔断
func TestBreakerSuccessResetsFailureCount(t *testing.T) {
	b := &channelBreaker{state: stateClosed}
	now := time.Now()

	b.recordFailure(now)
	b.recordFailure(now)
	b.recordSuccess() // 成功清零

	b.recordFailure(now) // 失败 1 次（旧计数已清）
	if b.state != stateClosed {
		t.Fatalf("清零后再失败1次 state=%v, want closed（还差2次才熔断）", b.state)
	}
}

// 健康探测成功：OPEN 直接恢复 CLOSED（跳过半开等待）
func TestBreakerProbeOKRecoversDirectly(t *testing.T) {
	b := &channelBreaker{state: stateClosed}
	now := time.Now()
	for i := 0; i < maxBreakerFailures; i++ {
		b.recordFailure(now)
	}
	if b.state != stateOpen {
		t.Fatal("前置：应已熔断")
	}

	b.probeOK() // 主动探活成功
	if b.state != stateClosed {
		t.Fatalf("probeOK 后 state=%v, want closed", b.state)
	}
	if !b.allow(now) {
		t.Fatal("恢复后 allow 应放行")
	}
}

// 注册表按渠道 ID 隔离：渠道 A 熔断不影响渠道 B
func TestCircuitBreakersPerChannelIsolation(t *testing.T) {
	cb := newCircuitBreakers()
	now := time.Now()

	// 渠道 1 打满失败熔断
	for i := 0; i < maxBreakerFailures; i++ {
		cb.recordFailure(1, now)
	}
	if cb.allow(1, now) {
		t.Fatal("渠道1 冷却中应拒绝")
	}
	// 渠道 2 从未失败：正常放行
	if !cb.allow(2, now) {
		t.Fatal("渠道2 无失败记录应放行")
	}
	// 渠道 1 健康探测恢复
	cb.probeOK(1)
	if !cb.allow(1, now) {
		t.Fatal("渠道1 探活恢复后应放行")
	}
}
