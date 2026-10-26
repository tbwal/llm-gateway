package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRequestLogFile 请求级日志文件：一条 requestID 一个文件，内容可回读
func TestRequestLogFile(t *testing.T) {
	// 用临时目录隔离（包级 requestLogDir 可覆盖；测试结束恢复）
	old := requestLogDir
	requestLogDir = t.TempDir()
	defer func() { requestLogDir = old }()

	rl := newRequestLogger("req_test_123")
	if rl == nil {
		t.Fatal("newRequestLogger 返回 nil，应成功创建")
	}
	defer rl.Close()

	rl.Printf("开始转发 model=%s", "gpt-5")
	rl.Printf("上游返回 %d", 200)

	// 回读文件：{tmp}/{date}/req_test_123.log
	entries, err := os.ReadDir(requestLogDir)
	if err != nil {
		t.Fatalf("读日志目录失败: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("日志目录应有 1 个日期子目录，got %d", len(entries))
	}
	path := filepath.Join(requestLogDir, entries[0].Name(), "req_test_123.log")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读请求日志文件失败: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, "开始转发 model=gpt-5") || !strings.Contains(content, "上游返回 200") {
		t.Fatalf("日志内容不完整:\n%s", content)
	}
}

// TestRequestLogRejectUnsafeID 非法 requestID（路径穿越尝试）不应创建文件
func TestRequestLogRejectUnsafeID(t *testing.T) {
	old := requestLogDir
	requestLogDir = t.TempDir()
	defer func() { requestLogDir = old }()

	for _, bad := range []string{"", "../evil", "a/b", "req 1", "req..x"} {
		if rl := newRequestLogger(bad); rl != nil {
			t.Fatalf("非法 requestID %q 不应创建日志文件", bad)
		}
	}
}
