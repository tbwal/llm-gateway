package handler

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestSafeLogFilePath 路径穿越防护：只放行根目录内的 .log 相对路径
func TestSafeLogFilePath(t *testing.T) {
	root := filepath.Join("data", "request-logs")

	// 合法：正常日期目录 + 请求文件
	if full, ok := safeLogFilePath(root, "2026-10-25/req_abc123.log"); !ok || !strings.HasSuffix(full, "req_abc123.log") {
		t.Fatalf("合法路径应放行，got ok=%v full=%s", ok, full)
	}

	// 非法：路径穿越（../ 跳出根目录）、越层、非日志后缀、绝对路径
	for _, bad := range []string{
		"../evil.log",            // 直接跳出
		"2026-10-25/../../x.log", // 深层穿越
		"a/b/c/d.log",            // 超过两层
		"x.txt",                  // 非 .log 后缀
		"/abs/path.log",          // 绝对路径
		"req_1.log/../req_2.log", // 内部穿越
	} {
		if _, ok := safeLogFilePath(root, bad); ok {
			t.Fatalf("非法路径 %q 不应放行", bad)
		}
	}
}

// TestLogFileListEmpty 空目录列表不报错（首次启动、无任何请求日志）
func TestLogFileListEmpty(t *testing.T) {
	old := requestLogDir
	requestLogDir = t.TempDir() // 空目录
	defer func() { requestLogDir = old }()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewLogFileHandler()
	r.GET("/api/logs/files", h.List)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/logs/files", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("空目录列表应返回 200，got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"files":[]`) || !strings.Contains(body, `"global":null`) {
		t.Fatalf("空列表返回体不符合预期: %s", body)
	}
}

// TestLogFileDownloadTraversal 下载接口：路径穿越一律 400（端到端走 HTTP）
func TestLogFileDownloadTraversal(t *testing.T) {
	old := requestLogDir
	requestLogDir = t.TempDir() // 空目录（即使有文件也不该被穿越读到）
	defer func() { requestLogDir = old }()

	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewLogFileHandler()
	r.GET("/api/logs/files/download", h.Download)

	for _, bad := range []string{"../evil.log", "2026-10-25/../../secret.log", "x.txt", "/abs/path.log", ""} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/logs/files/download?file="+bad, nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("穿越路径 %q 应返回 400，got %d", bad, w.Code)
		}
	}
}
