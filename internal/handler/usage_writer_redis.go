package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"

	"llm-gateway/internal/model"
)

// ============================================================
// Redis Stream 版异步落库（对比内存 channel 版）
//
// 为什么用 Redis Stream：
//   1. 队列在 Redis 里——网关进程挂了，队列不丢（Redis 持久化）
//   2. 多网关实例可以往同一个 stream 塞，统一消费
//   3. 消费组 + ACK：worker 处理完才 ACK，没 ACK 的重启后还能再读
//
// 架构：
//   网关 A/B/C → XADD usage_logs "*" payload=...
//                              ↓
//                     Redis Stream "usage_logs"
//                              ↓
//                     消费组 "logs-worker"
//                     ┌────┴────┐
//                     │consumer1│  ← 批量写 SQLite
//                     │consumer2│  ← 批量写 SQLite
//                     └─────────┘
// ============================================================

const (
	RedisStreamKey   = "usage_logs"       // stream 名（导出：router 日志用）
	RedisStreamGroup = "logs-worker"      // 消费组名（导出：router 日志用）
	redisBatchSize   = 32                 // 一次读多少条
	redisReadBlockMS = 500               // XREADGROUP 阻塞多久（毫秒）
)

// RedisStreamUsageWriter Redis Stream 版异步落库器
type RedisStreamUsageWriter struct {
	client   *redis.Client
	db       *gorm.DB
	consumer string // consumer-id（主机名-pid，保证多实例不重名）

	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    chan struct{}

	// 统计（原子读写）
	EnqueuedTotal     uint64
	PersistedTotal    uint64
	DroppedTotal      uint64
	WriteFailureTotal uint64
}

// NewRedisStreamUsageWriter 构造：建消费组（不存在才建），启动后台 worker
func NewRedisStreamUsageWriter(rdb *redis.Client, db *gorm.DB) (*RedisStreamUsageWriter, error) {
	w := &RedisStreamUsageWriter{
		client:   rdb,
		db:       db,
		consumer: fmt.Sprintf("%s-%d", hostname(), os.Getpid()),
		closed:   make(chan struct{}),
	}

	// ① 建消费组：MKSTREAM 表示 stream 不存在时自动创建；$ 表示从最新消息开始读
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rdb.XGroupCreateMkStream(ctx, RedisStreamKey, RedisStreamGroup, "$").Err(); err != nil {
		// BUSYGROUP = 消费组已存在，不算错
		if err.Error() != "BUSYGROUP Consumer Group name already exists" {
			return nil, fmt.Errorf("创建消费组失败: %w", err)
		}
	}

	// ② 启动后台 worker
	w.wg.Add(1)
	go w.run()
	return w, nil
}

// Enqueue 非阻塞入队：XADD 到 Redis Stream；Redis 挂了丢弃计数
func (w *RedisStreamUsageWriter) Enqueue(entry model.UsageLog) {
	payload, err := json.Marshal(entry)
	if err != nil {
		atomic.AddUint64(&w.DroppedTotal, 1)
		return
	}
	atomic.AddUint64(&w.EnqueuedTotal, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// XADD key "*" "payload" <json>
	// "*" 表示自动生成 ID（时间戳-序号）
	if err := w.client.XAdd(ctx, &redis.XAddArgs{
		Stream: RedisStreamKey,
		Values: map[string]any{"payload": string(payload)},
	}).Err(); err != nil {
		atomic.AddUint64(&w.DroppedTotal, 1)
	}
}

// run worker 主循环：XREADGROUP 读 → 批量写库 → XACK
func (w *RedisStreamUsageWriter) run() {
	defer w.wg.Done()

	for {
		select {
		case <-w.closed:
			w.drain()
			return
		default:
		}

		// XREADGROUP：阻塞读，最多 32 条，等 500ms
		// ">" 表示读新消息（不读 pending 列表里没 ACK 的旧消息）
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(redisReadBlockMS+100)*time.Millisecond)
		res, err := w.client.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group:    RedisStreamGroup,
			Consumer: w.consumer,
			Count:    redisBatchSize,
			Block:    time.Duration(redisReadBlockMS) * time.Millisecond,
			Streams:  []string{RedisStreamKey, ">"},
		}).Result()
		cancel()

		if err != nil {
			// redis.Nil = 超时没消息，正常情况，继续循环
			if err != redis.Nil {
				atomic.AddUint64(&w.WriteFailureTotal, 1)
				log.Printf("[usage-writer] XREADGROUP 失败: %v", err)
				time.Sleep(time.Second) // 出错退避，别狂刷
			}
			continue
		}

		// 解析消息 → 批量写库 → ACK
		for _, stream := range res {
			rows := make([]model.UsageLog, 0, len(stream.Messages))
			ids := make([]string, 0, len(stream.Messages))
			for _, msg := range stream.Messages {
				ids = append(ids, msg.ID)
				payload, ok := msg.Values["payload"].(string)
				if !ok {
					continue
				}
				var entry model.UsageLog
				if err := json.Unmarshal([]byte(payload), &entry); err != nil {
					continue
				}
				rows = append(rows, entry)
			}

			if len(rows) > 0 {
				if err := w.db.CreateInBatches(rows, redisBatchSize).Error; err != nil {
					atomic.AddUint64(&w.WriteFailureTotal, uint64(len(rows)))
					log.Printf("[usage-writer] 批量落库失败 %d 条: %v", len(rows), err)
					// 不 ACK：这批消息留在 pending 列表，下次重启还能再读
					continue
				}
				atomic.AddUint64(&w.PersistedTotal, uint64(len(rows)))
			}

			// ACK：告诉 Redis 这批处理完了，可以从 pending 列表删掉
			if len(ids) > 0 {
				ackCtx, ackCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
				if err := w.client.XAck(ackCtx, RedisStreamKey, RedisStreamGroup, ids...).Err(); err != nil {
					log.Printf("[usage-writer] XACK 失败: %v", err)
				}
				ackCancel()
			}
		}
	}
}

// drain 关闭时尽量把 pending 里的旧消息也处理掉（简单版：直接返回）
func (w *RedisStreamUsageWriter) drain() {
	// 生产环境可以 XPENDING 查 pending 列表，XCLAIM 接管后处理
	// 这里简化：等 XREADGROUP 的 Block 超时（500ms）自然返回，然后退出
}

// Close 优雅关闭
func (w *RedisStreamUsageWriter) Close() {
	w.closeOnce.Do(func() {
		close(w.closed)
		w.wg.Wait()
	})
}

// Stats 返回统计
func (w *RedisStreamUsageWriter) Stats() (enqueued, persisted, dropped, failed uint64) {
	return atomic.LoadUint64(&w.EnqueuedTotal),
		atomic.LoadUint64(&w.PersistedTotal),
		atomic.LoadUint64(&w.DroppedTotal),
		atomic.LoadUint64(&w.WriteFailureTotal)
}

// hostname 取主机名（consumer-id 用，失败返回 unknown）
func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}
