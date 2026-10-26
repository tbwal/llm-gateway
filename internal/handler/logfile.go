// 管理后台日志文件：列表 + 下载
//
// 覆盖两类日志文件：
//  1. 请求级日志  data/request-logs/{date}/{requestID}.log   —— 每条请求一个文件
//  2. 全局日志    data/logs/global.log                        —— 启动时 tee 落盘（见 cmd/server/main.go）
//
// ⚠️ 安全边界（下载接口的核心）：前端只传"相对路径"（如 2026-10-25/req_xxx.log），
// 服务端 join 到根目录后再做双重校验——Clean 后必须仍在根目录内 + 只允许 .log 后缀，
// 任何 ../、绝对路径、非日志后缀一律 400。路径穿越是文件下载类接口的第一防线。
package handler

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
)

// globalLogPath 全局日志文件路径（main.go tee 落盘；与 requestLogDir 同级）
var globalLogPath = filepath.Join("data", "logs", "global.log")

// LogFileHandler 日志文件列表/下载
type LogFileHandler struct{}

// NewLogFileHandler 构造器
func NewLogFileHandler() *LogFileHandler { return &LogFileHandler{} }

// logFileMeta 单个日志文件的元信息（前端列表展示）
type logFileMeta struct {
	Date    string `json:"date"`     // 日期目录名（请求日志）；全局日志为 "global"
	Name    string `json:"name"`     // 文件名（requestID.log / global.log）
	RelPath string `json:"rel_path"` // 相对 requestLogDir 的路径（请求日志下载参数）；全局日志用固定 "global"
	Size    int64  `json:"size"`     // 字节
	ModTime string `json:"mod_time"` // 修改时间（展示用）
}

// List GET /api/logs/files
// 返回：全局日志 + 请求日志文件列表（按日期倒序、同日期内按文件名倒序）
func (h *LogFileHandler) List(c *gin.Context) {
	// ① 全局日志（存在才返回）
	var global *logFileMeta
	if fi, err := os.Stat(globalLogPath); err == nil && !fi.IsDir() {
		global = &logFileMeta{
			Date:    "global",
			Name:    "global.log",
			RelPath: "global", // 下载接口特殊识别：不在 requestLogDir 内
			Size:    fi.Size(),
			ModTime: fi.ModTime().Format("2006-01-02 15:04:05"),
		}
	}

	// ② 请求日志文件：扫描 {requestLogDir}/{date}/*.log（两层）
	type row struct {
		date string
		fi   os.FileInfo
	}
	var rows []row
	dates, err := os.ReadDir(requestLogDir)
	if err == nil {
		for _, d := range dates {
			if !d.IsDir() || len(d.Name()) != len("2006-01-02") {
				continue
			}
			files, err := os.ReadDir(filepath.Join(requestLogDir, d.Name()))
			if err != nil {
				continue
			}
			for _, f := range files {
				if f.IsDir() || !strings.HasSuffix(f.Name(), ".log") {
					continue
				}
				fi, err := f.Info()
				if err != nil {
					continue
				}
				rows = append(rows, row{date: d.Name(), fi: fi})
			}
		}
	}
	// 排序：日期倒序 → 文件名倒序（新请求在前）
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].date != rows[j].date {
			return rows[i].date > rows[j].date
		}
		return rows[i].fi.Name() > rows[j].fi.Name()
	})
	files := make([]logFileMeta, 0, len(rows))
	for _, r := range rows {
		files = append(files, logFileMeta{
			Date:    r.date,
			Name:    r.fi.Name(),
			RelPath: r.date + "/" + r.fi.Name(),
			Size:    r.fi.Size(),
			ModTime: r.fi.ModTime().Format("2006-01-02 15:04:05"),
		})
	}

	c.JSON(http.StatusOK, gin.H{"global": global, "files": files, "dir": requestLogDir})
}

// Download GET /api/logs/files/download?file={rel_path}
// file 参数两种形态：
//   - "global"          → 全局日志 data/logs/global.log
//   - "2026-10-25/req_x.log" → 请求日志（相对 requestLogDir）
func (h *LogFileHandler) Download(c *gin.Context) {
	rel := c.Query("file")
	if rel == "" {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "缺少 file 参数", "data": nil})
		return
	}

	// ① 全局日志特殊分支（不在 requestLogDir 内）
	if rel == "global" {
		h.serveFile(c, globalLogPath, "global.log")
		return
	}

	// ② 请求日志：安全校验后落盘
	full, ok := safeLogFilePath(requestLogDir, rel)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"code": 400, "message": "非法的日志文件路径", "data": nil})
		return
	}
	h.serveFile(c, full, filepath.Base(rel))
}

// serveFile 校验存在后以附件形式下发（Content-Disposition 触发浏览器下载）
func (h *LogFileHandler) serveFile(c *gin.Context, path, name string) {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		c.JSON(http.StatusNotFound, gin.H{"code": 404, "message": "日志文件不存在", "data": nil})
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.File(path)
}

// safeLogFilePath 双重校验相对路径 → 绝对路径：
//  1. 后缀必须是 .log（防传任意文件）
//  2. Clean 后必须仍是我们约定的两层结构 {date}/{requestID}.log
//  3. Clean 后必须仍在 root 内（防 ../ 路径穿越；filepath.Rel 双保险）
// 平台坑：Windows 的 filepath.IsAbs 只认盘符/UNC，"C:\x" 是绝对、"/x" 不是——
// 所以还要显式拒绝以 / 或 \ 开头的形态，否则 Join 语义会出乎意料。
func safeLogFilePath(root, rel string) (string, bool) {
	if !strings.HasSuffix(rel, ".log") {
		return "", false
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") {
		return "", false
	}
	clean := filepath.Clean(rel)
	// 只放行两层：{date}/{requestID}.log；一层（裸文件名）、深层、穿越形态全拒绝
	if clean == "." || strings.Count(clean, string(filepath.Separator)) != 1 {
		return "", false
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	full := filepath.Join(root, clean)
	rel2, err := filepath.Rel(root, full)
	if err != nil || strings.HasPrefix(rel2, "..") || filepath.IsAbs(rel2) {
		return "", false
	}
	return full, true
}
