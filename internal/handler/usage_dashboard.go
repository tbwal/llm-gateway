// 用量看板 API（W3-③）：基于 usage_logs 流水表查询时聚合
//
// 设计原则（与 W3-② 计费落库一脉相承）：
//   流水表 + 查询时聚合。落库侧只 INSERT（无锁竞争），
//   看板侧 GROUP BY + SUM（读多写少），不维护冗余聚合表。
//
// 三个端点覆盖看板三种视图：
//   GET /api/usage/summary   总览卡片：请求数 / prompt / completion / total / 费用
//   GET /api/usage/breakdown 分组排行：?group=model|channel，按费用降序
//   GET /api/usage/daily     按天趋势：时间序列，供折线图
// 共同参数：?days=N（默认 7，范围 1~90）
package handler

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"llm-gateway/internal/model"
)

// UsageDashboard 用量看板处理器
type UsageDashboard struct {
	db *gorm.DB
}

func NewUsageDashboard(db *gorm.DB) *UsageDashboard {
	return &UsageDashboard{db: db}
}

// ---------- 聚合行结构（summary / breakdown / daily 复用） ----------

// usageAggRow 一次聚合的明细行
type usageAggRow struct {
	Requests         int64   `json:"requests"`          // 请求数（流水条数）
	PromptTokens     int64   `json:"prompt_tokens"`     // 输入 tokens 合计
	CompletionTokens int64   `json:"completion_tokens"` // 输出 tokens 合计
	TotalTokens      int64   `json:"total_tokens"`      // 总 tokens
	Cost             float64 `json:"cost"`              // 费用合计（元）
}

// ---------- Summary：总览卡片 ----------

// Summary GET /api/usage/summary?days=7
func (d *UsageDashboard) Summary(c *gin.Context) {
	days := parseDays(c)
	var row usageAggRow
	// coalesce：空表时 SUM 返回 NULL，coalesce 兜底成 0，避免 Scan 报错
	err := d.db.Model(&model.UsageLog{}).
		Select("count(*) as requests, coalesce(sum(prompt_tokens),0) as prompt_tokens, " +
			"coalesce(sum(completion_tokens),0) as completion_tokens, coalesce(sum(total_tokens),0) as total_tokens, " +
			"coalesce(sum(cost),0) as cost").
		Where("created_at >= ?", since(days)).
		Scan(&row).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"days":    days,
		"summary": row,
	})
}

// ---------- Breakdown：分组排行 ----------

// breakdownItem 分组聚合的一行（平铺字段：GORM Scan 内嵌结构体时
// 聚合列映射不可靠，必须平铺才能正确绑定 count/sum 结果）
type breakdownItem struct {
	Key              string  `json:"key"` // 分组键：模型名 或 渠道名
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Cost             float64 `json:"cost"`
}

// Breakdown GET /api/usage/breakdown?group=model|channel&days=7
// group 参数：model（默认，按模型）/ channel（按渠道名，流水冗余字段免 join）
func (d *UsageDashboard) Breakdown(c *gin.Context) {
	days := parseDays(c)
	// group 白名单：只允许 model / channel_name 两列，禁止用户输入直拼 SQL
	group := c.DefaultQuery("group", "model")
	col, label := "model", "model"
	switch group {
	case "channel":
		col, label = "channel_name", "channel_name"
	case "model":
		// 默认
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "group 只支持 model 或 channel"})
		return
	}

	var rows []breakdownItem
	err := d.db.Model(&model.UsageLog{}).
		Select(col+" as key, count(*) as requests, "+
			"coalesce(sum(prompt_tokens),0) as prompt_tokens, coalesce(sum(completion_tokens),0) as completion_tokens, "+
			"coalesce(sum(total_tokens),0) as total_tokens, coalesce(sum(cost),0) as cost").
		Where("created_at >= ?", since(days)).
		Group(col).
		Order("cost desc, requests desc").
		Scan(&rows).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// 空表时 rows 是 nil slice → JSON 会序列化成 null 而非 []（前端 .length 直接崩）。
	// 契约：列表字段空必须返回 []，统一在这里兜底。
	if rows == nil {
		rows = []breakdownItem{}
	}
	c.JSON(http.StatusOK, gin.H{"days": days, "group": label, "rows": rows})
}

// ---------- Daily：按天趋势 ----------

// dailyItem 按天聚合的一行（同样平铺：避免 GORM Scan 内嵌结构体映射失效）
type dailyItem struct {
	Date             string  `json:"date"` // YYYY-MM-DD
	Requests         int64   `json:"requests"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	TotalTokens      int64   `json:"total_tokens"`
	Cost             float64 `json:"cost"`
}

// Daily GET /api/usage/daily?days=7
// strftime 是 SQLite 日期格式化函数（glebarez/sqlite 就是 SQLite 方言）
func (d *UsageDashboard) Daily(c *gin.Context) {
	days := parseDays(c)
	var rows []dailyItem
	err := d.db.Model(&model.UsageLog{}).
		Select("strftime('%Y-%m-%d', created_at) as date, count(*) as requests, "+
			"coalesce(sum(prompt_tokens),0) as prompt_tokens, coalesce(sum(completion_tokens),0) as completion_tokens, "+
			"coalesce(sum(total_tokens),0) as total_tokens, coalesce(sum(cost),0) as cost").
		Where("created_at >= ?", since(days)).
		Group("date").
		Order("date").
		Scan(&rows).Error
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// 同 Breakdown：空表返回 [] 而非 null（前端 daily.length 依赖数组）
	if rows == nil {
		rows = []dailyItem{}
	}
	c.JSON(http.StatusOK, gin.H{"days": days, "rows": rows})
}

// ---------- 参数解析 ----------

// parseDays 解析 days 参数：默认 7 天，范围 1~90（非法值回退默认）
func parseDays(c *gin.Context) int {
	days := 7
	if v := c.Query("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 90 {
			days = n
		}
	}
	return days
}

// since 返回 days 天前的时刻（看板的时间下界）
func since(days int) time.Time {
	return time.Now().AddDate(0, 0, -days)
}

// ---------- Logs：请求日志（流水明细） ----------

// Logs GET /api/usage/logs?page=1&page_size=20&model=&channel_id=&provider=&request_id=&days=
// 请求日志 = 流水表原样分页，不做聚合——每行一条真实请求记录，可审计可追溯。
// 参数：
//   page/page_size  分页（默认 1 / 20，page_size 上限 100）
//   model/channel_id/provider  精确筛选（逗号分隔的 model 可传多个？保持单值，够用）
//   request_id      按链路 ID 精确筛选（排障：输入 req_xxx 拉出该请求的全部记录）
//   access_key_name 按调用方名称精确筛选（如 audit-user；匿名请留空 = 不筛）
//   client_ip       按客户端 IP 精确筛选（如 ::1 / 127.0.0.1）
//   days            时间范围（不传 = 全部历史；1~365）
func (d *UsageDashboard) Logs(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if pageSize < 1 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}

	q := d.db.Model(&model.UsageLog{})
	if v := c.Query("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 365 {
			q = q.Where("created_at >= ?", since(n))
		}
	}
	if m := strings.TrimSpace(c.Query("model")); m != "" {
		q = q.Where("model = ?", m)
	}
	if rid := strings.TrimSpace(c.Query("request_id")); rid != "" {
		q = q.Where("request_id = ?", rid)
	}
	if ak := strings.TrimSpace(c.Query("access_key_name")); ak != "" {
		q = q.Where("access_key_name = ?", ak)
	}
	if ip := strings.TrimSpace(c.Query("client_ip")); ip != "" {
		q = q.Where("client_ip = ?", ip)
	}
	if ch := strings.TrimSpace(c.Query("channel_id")); ch != "" {
		if n, err := strconv.Atoi(ch); err == nil {
			q = q.Where("channel_id = ?", n)
		}
	}
	if p := strings.TrimSpace(c.Query("provider")); p != "" {
		q = q.Where("provider = ?", p)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var items []model.UsageLog
	if err := q.Order("created_at desc, id desc").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&items).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"items":     items,
	})
}
