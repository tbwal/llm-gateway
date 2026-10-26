// 请求级日志落盘：一条 requestID 一个日志文件
//
// 背景：现有 log.Printf 全打到一个终端/全局输出，排障时要把一条请求的
// 转发轨迹从全局日志里"人肉挑出来"。这里把请求链路里的打印信息按
// requestID 归档成独立文件：{REQUEST_LOG_DIR}/{date}/{requestID}.log。
//
// 用法（接入点见 AccessKeyAuth 入口）：
//   c.Set(requestLogKey, rl)   // 中间件入口创建并绑定
//   defer rl.Close()           // 请求处理完（c.Next() 返回）关闭
//   链路内统一用 RequestLogf(c, ...) 写文件；没有请求上下文时
//   （管理接口/后台任务）自动 fallback 到全局 log，不丢日志。
package handler

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"
)

// requestLogKey context 里存 *RequestLogger 的键
const requestLogKey = "reqLogger"

// requestLogDir 请求日志根目录（env REQUEST_LOG_DIR 可覆盖，默认 data/request-logs）
// 每条请求一个文件：{root}/{date}/{requestID}.log —— 按天分目录避免单目录文件海量
var requestLogDir = func() string {
	if d := os.Getenv("REQUEST_LOG_DIR"); d != "" {
		return d
	}
	return filepath.Join("data", "request-logs")
}()

// safeReqID 只允许我们生成的 req_xxx 形态（字母数字下划线横线）——
// 防止恶意/异常 requestID 做路径穿越（../ 之类必须拦在写文件之前）
var safeReqID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// RequestLogger 单请求日志文件：log.Logger 输出到独立文件
type RequestLogger struct {
	file *os.File
	lg   *log.Logger
}

// newRequestLogger 为一条请求创建日志文件 {root}/{date}/{requestID}.log
// requestID 不合法或创建失败时返回 nil（调用方 fallback 到全局日志）
func newRequestLogger(requestID string) *RequestLogger {
	if requestID == "" || !safeReqID.MatchString(requestID) {
		return nil
	}
	dir := filepath.Join(requestLogDir, time.Now().Format("2006-01-02"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, requestID+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	return &RequestLogger{file: f, lg: log.New(f, "", log.LstdFlags|log.Lmicroseconds)}
}

// Printf 写一条到本请求日志文件
func (r *RequestLogger) Printf(format string, args ...any) {
	if r != nil && r.lg != nil {
		r.lg.Printf(format, args...)
	}
}

// Close 请求处理完关闭文件（defer 调用；重复调用安全）
func (r *RequestLogger) Close() {
	if r != nil && r.file != nil {
		r.file.Close()
		r.file = nil
	}
}

// RequestLogf 请求链路的统一日志入口：
//   1. 上下文里有本请求的日志文件 → 写文件（带毫秒时间戳）
//   2. 否则（管理接口/后台任务/文件创建失败）→ fallback 全局 log，不丢日志
func RequestLogf(c *gin.Context, format string, args ...any) {
	if c != nil {
		if v, ok := c.Get(requestLogKey); ok {
			if rl, ok := v.(*RequestLogger); ok && rl != nil {
				rl.Printf(format, args...)
				return
			}
		}
	}
	log.Printf(format, args...)
}
