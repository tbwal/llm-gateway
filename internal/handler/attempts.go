package handler

import (
	"encoding/json"

	"github.com/gin-gonic/gin"

	"llm-gateway/internal/model"
)

// relayAttemptsKey 故障转移尝试链存 context 的 key（与 relayStartKey 等并列）
const relayAttemptsKey = "relayAttempts"

// initAttempts 在 ChatCompletions 入口初始化尝试链（挂 context，后续循环 append）
func initAttempts(c *gin.Context) {
	attempts := make([]model.Attempt, 0, 2) // 预分配 2：单次成功容量 1，故障转移通常 2~3 次
	c.Set(relayAttemptsKey, &attempts)
}

// appendAttempt 往 context 里的尝试链追加一跳（循环内换渠道/成功时调用）
func appendAttempt(c *gin.Context, ch model.Channel, statusCode int, durationMs int64, errMsg string, willRetry bool) {
	v, ok := c.Get(relayAttemptsKey)
	if !ok {
		return
	}
	ptr, ok := v.(*[]model.Attempt)
	if !ok {
		return
	}
	seq := len(*ptr) + 1 // 第几次尝试（从 1 开始）
	summary := errMsg
	if len(summary) > 200 {
		summary = summary[:200] + "…"
	}
	*ptr = append(*ptr, model.Attempt{
		Sequence:    seq,
		ChannelID:   ch.ID,
		ChannelName: ch.Name,
		Provider:    ch.Provider,
		StatusCode:  statusCode,
		DurationMs:  durationMs,
		Error:       summary,
		WillRetry:   willRetry,
	})
}

// attemptsFromCtx 取尝试链 JSON 序列化串（recordUsage/recordUsageFailure 落库时用）
// 没有尝试链（非 relay 链路，如 AccessKeyAuth 直接 401）返回空串
func attemptsFromCtx(c *gin.Context) string {
	v, ok := c.Get(relayAttemptsKey)
	if !ok {
		return ""
	}
	ptr, ok := v.(*[]model.Attempt)
	if !ok || ptr == nil || len(*ptr) == 0 {
		return ""
	}
	b, err := json.Marshal(*ptr)
	if err != nil {
		return ""
	}
	return string(b)
}
