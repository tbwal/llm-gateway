package handler

// ============================================================
// 后台健康探测（W2：熔断的"主动恢复"通道）
//
// 熔断渠道不会自己康复——需要有人定期去"戳一下"确认上游活了没。
// 这个 goroutine 每隔 interval 扫描所有 OPEN 渠道，GET {base_url}/health：
//   2xx/3xx/4xx → probeOK：直接恢复 CLOSED，重新参与调度
//   5xx / 网络错误 → 保持 OPEN，下个周期再探
//
// 面试讲点：熔断是"被动失败摘除"，健康探测是"主动探活恢复"，
// 两者配合构成渠道健康管理的闭环；探测状态在进程内，多实例要共享需 Redis。
// ============================================================

import (
	"log"
	"net/http"
	"strings"
	"time"

	"gorm.io/gorm"

	"llm-gateway/internal/model"
)

// healthProber 后台健康探测器
type healthProber struct {
	db       *gorm.DB        // 查渠道配置（base_url）
	breakers *circuitBreakers // 熔断器注册表（扫描 OPEN 渠道）
	client   *http.Client    // 探测专用客户端（短超时，不拖慢探测循环）
	interval time.Duration   // 探测周期
	stop     chan struct{}   // 停止信号
}

// NewHealthProber 构造器
func NewHealthProber(db *gorm.DB, breakers *circuitBreakers, interval time.Duration) *healthProber {
	return &healthProber{
		db:       db,
		breakers: breakers,
		client:   &http.Client{Timeout: 3 * time.Second},
		interval: interval,
		stop:     make(chan struct{}),
	}
}

// Start 启动后台探测循环（独立 goroutine）
func (p *healthProber) Start() {
	go func() {
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				p.probeOnce()
			}
		}
	}()
}

// Stop 停止探测（服务器退出时调用）
func (p *healthProber) Stop() {
	close(p.stop)
}

// probeOnce 一轮探测：扫描全部熔断中的渠道，逐个探活
func (p *healthProber) probeOnce() {
	for _, id := range p.breakers.openIDs() {
		var ch model.Channel
		if err := p.db.First(&ch, id).Error; err != nil {
			// 渠道已被删除：清掉熔断状态即可（下轮不再扫）
			p.breakers.recordSuccess(id)
			continue
		}
		url := strings.TrimRight(ch.BaseURL, "/") + "/health"
		resp, err := p.client.Get(url)
		if err != nil {
			log.Printf("[probe] 渠道 id=%d 探活失败(网络): %v", id, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode < 500 {
			log.Printf("[probe] 渠道 id=%d 探活成功(status=%d) → 恢复调度", id, resp.StatusCode)
			p.breakers.probeOK(id)
		} else {
			log.Printf("[probe] 渠道 id=%d 探活失败(status=%d)，保持熔断", id, resp.StatusCode)
		}
	}
}
