package handler

import (
	"log"
	"sync"
	"sync/atomic"
	"time"

	"gorm.io/gorm"

	"llm-gateway/internal/model"
)

// UsageWriter 异步落库器接口（内存版 / Redis Stream 版都实现这个接口）
//
// 主链路只依赖这个接口，不关心底层是内存 channel 还是 Redis Stream——
// 切换实现时 relay.go / logUsageFailure 一行代码都不用改。
type UsageWriter interface {
	Enqueue(entry model.UsageLog) // 非阻塞入队
	Close()                        // 优雅关闭（drain 队列剩余）
}

// ============================================================
// 异步落库 worker（参考 gpt-load requestlog/worker.go）
//
// 为什么要异步：
//   现在 recordUsage 是同步写 SQLite——主链路"转发完→写库→回客户端"，
//   写库慢了直接拖慢响应。日志是旁路功能，不该影响主链路。
//
// 机制：
//   1. 带缓冲 channel 队列：Enqueue 非阻塞塞进去，立即返回
//   2. 后台 worker goroutine：从 channel 取，攒够 batchSize 或超时就批量写
//   3. 定时 flush（flushDelay）：低流量时也不能让日志在队列里过夜
//   4. 优雅关闭（Close）：main 退出时 drain 队列里剩余的
//   5. 队列满了丢弃 + 计数（DroppedTotal）：日志丢了不影响主链路
// ============================================================

const (
	usageQueueCapacity = 256                    // 队列容量：攒 256 条不写就开始丢
	usageBatchSize     = 32                     // 攒够 32 条就批量写一次（CreateInBatches）
	usageFlushDelay    = 500 * time.Millisecond // 即使没攒够，0.5s 也 flush 一次
)

// memoryUsageWriter 内存 channel 版异步用量日志落库器（实现 UsageWriter 接口）
type memoryUsageWriter struct {
	db        *gorm.DB
	queue     chan model.UsageLog
	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    chan struct{}

	// 统计（原子读写）
	EnqueuedTotal     uint64 // 入队总数
	PersistedTotal    uint64 // 已落库总数
	DroppedTotal      uint64 // 队列满被丢弃数
	WriteFailureTotal uint64 // 落库失败次数
}

// NewUsageWriter 构造并启动后台 worker（内存版，返回 UsageWriter 接口）
func NewUsageWriter(db *gorm.DB) UsageWriter {
	w := &memoryUsageWriter{
		db:     db,
		queue:  make(chan model.UsageLog, usageQueueCapacity),
		closed: make(chan struct{}),
	}
	w.wg.Add(1)
	go w.run()
	return w
}

// Enqueue 非阻塞入队：队列满就丢弃并计数（不影响主链路）
func (w *memoryUsageWriter) Enqueue(entry model.UsageLog) {
	atomic.AddUint64(&w.EnqueuedTotal, 1)
	select {
	case w.queue <- entry:
		// 入队成功
	default:
		// 队列满：丢弃（计数），日志丢了不影响转发
		atomic.AddUint64(&w.DroppedTotal, 1)
	}
}

// run worker 主循环：攒批 + 定时 flush
func (w *memoryUsageWriter) run() {
	defer w.wg.Done()
	batch := make([]model.UsageLog, 0, usageBatchSize)
	timer := time.NewTimer(usageFlushDelay)
	defer timer.Stop()

	for {
		select {
		case <-w.closed:
			// 关闭：把当前 batch + 队列剩余的全部写完
			w.drain(batch)
			return
		case entry, ok := <-w.queue:
			if !ok {
				w.drain(batch)
				return
			}
			batch = append(batch, entry)
			if len(batch) >= usageBatchSize {
				w.writeBatch(batch)
				batch = batch[:0]
				if !timer.Stop() {
					<-timer.C
				}
				timer.Reset(usageFlushDelay)
			}
		case <-timer.C:
			if len(batch) > 0 {
				w.writeBatch(batch)
				batch = batch[:0]
			}
			timer.Reset(usageFlushDelay)
		}
	}
}

// drain 关闭时把剩余的全部写完
func (w *memoryUsageWriter) drain(batch []model.UsageLog) {
	// 先把当前 batch 写完
	if len(batch) > 0 {
		w.writeBatch(batch)
	}
	// 再把队列里剩余的全部取出来写完
	for {
		select {
		case entry := <-w.queue:
			batch = append(batch, entry)
			if len(batch) >= usageBatchSize {
				w.writeBatch(batch)
				batch = batch[:0]
			}
		default:
			if len(batch) > 0 {
				w.writeBatch(batch)
			}
			return
		}
	}
}

// writeBatch 批量写数据库（CreateInBatches 比逐条 Create 快很多）
func (w *memoryUsageWriter) writeBatch(rows []model.UsageLog) {
	if len(rows) == 0 {
		return
	}
	if err := w.db.CreateInBatches(rows, usageBatchSize).Error; err != nil {
		atomic.AddUint64(&w.WriteFailureTotal, uint64(len(rows)))
		log.Printf("[usage-writer] 批量落库失败 %d 条: %v", len(rows), err)
		return
	}
	atomic.AddUint64(&w.PersistedTotal, uint64(len(rows)))
}

// Close 优雅关闭：等待 worker drain 完队列
func (w *memoryUsageWriter) Close() {
	w.closeOnce.Do(func() {
		close(w.closed)
		w.wg.Wait()
	})
}

// Stats 返回当前统计（系统状态页展示用）
func (w *memoryUsageWriter) Stats() (enqueued, persisted, dropped, failed uint64, queueDepth int) {
	return atomic.LoadUint64(&w.EnqueuedTotal),
		atomic.LoadUint64(&w.PersistedTotal),
		atomic.LoadUint64(&w.DroppedTotal),
		atomic.LoadUint64(&w.WriteFailureTotal),
		len(w.queue)
}
