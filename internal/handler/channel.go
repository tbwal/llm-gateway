// Package handler 业务处理层：接收 HTTP 请求 → 操作数据库 → 返回统一 JSON
package handler

import (
	"errors" // 错误判断：errors.Is 识别"记录不存在"
	"net/http"
	"strconv" // 字符串转整数：解析路径里的 :id

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"llm-gateway/internal/crypto"
	"llm-gateway/internal/model"
)

// ChannelHandler 渠道接口处理器
//
// 关键设计：把数据库句柄 db 和加密服务 enc 存进结构体 —— 这叫"依赖注入"的雏形
//   - 不用全局变量，处理器需要什么就通过构造器传入什么
//   - 好处：可测试（换一个 mock 库）、可替换、职责清晰
//   - enc：API Key 落库前必须加密，绝不明文入库
type ChannelHandler struct {
	db    *gorm.DB
	enc   crypto.Service
	cache *channelCache // 渠道配置缓存：CRUD 后主动刷新，转发侧立即感知（热更新写路径）
}

// NewChannelHandler 构造器：把数据库依赖注入处理器
func NewChannelHandler(db *gorm.DB, enc crypto.Service, cache *channelCache) *ChannelHandler {
	return &ChannelHandler{db: db, enc: enc, cache: cache}
}

// resp 统一响应函数：所有接口同一格式 {code, message, data}
func resp(c *gin.Context, code int, msg string, data any) {
	c.JSON(http.StatusOK, gin.H{"code": code, "message": msg, "data": data})
}

// channelView 渠道"对外视图"：手动挑选字段返回，密钥（api_key）绝不外泄
// 为什么要多此一举？
//
//	之前用 json:"-" 想隐藏密钥，结果它"双向屏蔽"——连请求体解析也挡住了，
//	导致密钥存不进去。所以现在：模型层允许 api_key 正常进出（存储层要它），
//	响应层在这里手动过滤（对外层不暴露）。这叫"模型与视图分离"。
func channelView(ch model.Channel) gin.H {
	return gin.H{
		"id":         ch.ID,
		"name":       ch.Name,
		"provider":   ch.Provider,
		"base_url":   ch.BaseURL,
		"weight":     ch.Weight,
		"enabled":    ch.Enabled,
		"models":     ch.Models,
		"created_at": ch.CreatedAt,
		// 没有 api_key —— 对外永远看不到密钥
	}
}

// Create POST /api/channels —— 创建渠道
// 流程：解析请求体 JSON → GORM 写库 → 返回新渠道（含自增 ID，不含密钥）
// channelReq 创建/更新渠道请求体
// ⚠️ Enabled 必须用 *bool（指针）：GORM 对带 default 标签的零值字段（false）在 Create 时
// 会跳过并落数据库默认值 true——用结构体 bool 直接绑，"传 false"会被吞成"没传"。
// 指针能区分三种状态：nil=没传（保持默认）、false=显式禁用、true=显式启用。
type channelReq struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
	Weight   int    `json:"weight"`
	Enabled  *bool  `json:"enabled"`
	Models   string `json:"models"`
}

// Create POST /api/channels —— 创建渠道
// 流程：解析请求体 → 密钥加密 → 写库 → 返回（含渠道 id）
func (h *ChannelHandler) Create(c *gin.Context) {
	// ① 把请求体 JSON 解析进请求结构体
	// 请求示例：{"name":"DeepSeek 主","provider":"deepseek","base_url":"https://api.deepseek.com/v1","api_key":"sk-xxx","weight":1}
	var req channelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		resp(c, 400, "参数错误: "+err.Error(), nil)
		return
	}

	// ② 组装模型：enabled 默认启用，显式传了才覆盖（绕开 GORM 零值吞 false 的陷阱）
	ch := model.Channel{
		Name:     req.Name,
		Provider: req.Provider,
		BaseURL:  req.BaseURL,
		APIKey:   req.APIKey,
		Weight:   req.Weight,
		Enabled:  true, // 与模型 default:true 语义一致
		Models:   req.Models,
	}
	if req.Enabled != nil {
		ch.Enabled = *req.Enabled // 用户显式传了 enabled → 按传的来（false 也能存）
	}

	// ③ 密钥加密：API Key 绝不明文落库
	// 加密后才写库，库里只存 hex 密文（解密动作在转发时由 relay 做）
	if ch.APIKey != "" {
		encKey, err := h.enc.Encrypt(ch.APIKey)
		if err != nil {
			resp(c, 500, "密钥加密失败: "+err.Error(), nil)
			return
		}
		ch.APIKey = encKey // 用密文替换明文，后续 GORM 落库的就是密文
	}

	// ④ GORM 写库：INSERT INTO channels (...) VALUES (...)
	//    - ID（自增）和 CreatedAt（当前时间）由数据库/GORM 自动填充
	if err := h.db.Create(&ch).Error; err != nil {
		resp(c, 500, "创建失败: "+err.Error(), nil)
		return
	}

	// ⑤ 返回创建成功的渠道（走 channelView，密钥不返回）
	h.cache.Refresh() // 热更新：新渠道立即进入转发侧候选池
	resp(c, 0, "ok", channelView(ch))
}

// List GET /api/channels —— 渠道列表
// 流程：GORM 全表查询 → 逐条转对外视图 → 返回
func (h *ChannelHandler) List(c *gin.Context) {
	var list []model.Channel
	// GORM 查询：SELECT * FROM channels，结果自动填入 list 切片
	// Find 的约定：传入 &切片 → 查询所有行；传入 &结构体 + Where → 查单行
	if err := h.db.Find(&list).Error; err != nil {
		resp(c, 500, "查询失败: "+err.Error(), nil)
		return
	}
	// 逐条转换成对外视图（隐藏密钥）
	views := make([]gin.H, 0, len(list))
	for _, ch := range list {
		views = append(views, channelView(ch))
	}
	resp(c, 0, "ok", views)
}

// Get GET /api/channels/:id —— 渠道详情
// 流程：id 转整数 → 按主键查询 → 存在返回 / 不存在 404
func (h *ChannelHandler) Get(c *gin.Context) {
	// ① 路径参数 :id 是字符串，转成整数才能查数据库
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil { // 转换失败（如 "abc"）→ 参数错误
		resp(c, 400, "id 无效", nil)
		return
	}

	var ch model.Channel
	// ② 按主键查询：First(&ch, id) 等价 SELECT * FROM channels WHERE id = ?
	//    ⚠️ 查不到时返回 gorm.ErrRecordNotFound（要单独判断，不能当 500）
	if err := h.db.First(&ch, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) { // 精确识别"记录不存在"
			resp(c, 404, "渠道不存在", nil)
		} else { // 其他数据库错误
			resp(c, 500, "查询失败: "+err.Error(), nil)
		}
		return
	}
	resp(c, 0, "ok", channelView(ch)) // ③ 走对外视图，密钥不返回
}

// Update PUT /api/channels/:id —— 更新渠道
// 流程：id 转整数 → 确认存在 → 解析请求体 → map 更新 → 返回最新数据
func (h *ChannelHandler) Update(c *gin.Context) {
	// ① id 转整数
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp(c, 400, "id 无效", nil)
		return
	}

	// ② 先确认要更新的记录存在（更新不存在的资源 → 404）
	var ch model.Channel
	if err := h.db.First(&ch, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			resp(c, 404, "渠道不存在", nil)
		} else {
			resp(c, 500, "查询失败: "+err.Error(), nil)
		}
		return
	}

	// ③ 解析请求体（传什么更新什么，缺省字段保持原值）
	var req channelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		resp(c, 400, "参数错误: "+err.Error(), nil)
		return
	}

	// ④ 用 map 更新（关键教学点）：
	//    为什么不用 结构体 更新？因为 GORM 用结构体更新会"忽略零值"——
	//    比如 weight 传 0、enabled 传 false，会被当成"没传"而跳过更新
	//    用 map 更新则 0/false 也会真实写入
	//
	//    ⚠️ enabled 特殊处理：只有显式传了才更新（*bool 非 nil）
	//       修复：旧实现恒写 req.Enabled=false，任何不带 enabled 的 PUT 都会把渠道禁用
	//    ⚠️ api_key 特殊处理：留空 = 不更新（保持原密钥）
	//       - 防止"没传密钥"把库里密钥清成空串（原实现的隐患）
	//       - 防止把空串加密成密文写进去
	//       只有传了非空密钥才加密后更新
	updates := map[string]any{
		"name":     req.Name,
		"provider": req.Provider,
		"base_url": req.BaseURL,
		"weight":   req.Weight,
		"models":   req.Models,
	}
	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled // 显式传了才写，缺省保持原值
	}
	if req.APIKey != "" {
		encKey, err := h.enc.Encrypt(req.APIKey)
		if err != nil {
			resp(c, 500, "密钥加密失败: "+err.Error(), nil)
			return
		}
		updates["api_key"] = encKey
	}
	// Model(&ch)：锁定更新目标是 ch 这张表的 id 行；Updates 只更新给定字段
	if err := h.db.Model(&ch).Updates(updates).Error; err != nil {
		resp(c, 500, "更新失败: "+err.Error(), nil)
		return
	}

	// ⑤ 重新查询一次，返回更新后的最新数据（Updates 不会回填结构体）
	h.db.First(&ch, id)
	h.cache.Refresh() // 热更新：改动立即影响转发侧调度
	resp(c, 0, "ok", channelView(ch))
}

// Delete DELETE /api/channels/:id —— 删除渠道
// 流程：id 转整数 → 确认存在 → 删除 → 返回
func (h *ChannelHandler) Delete(c *gin.Context) {
	// ① id 转整数
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp(c, 400, "id 无效", nil)
		return
	}

	// ② 先确认存在（删除不存在的资源 → 404）
	var ch model.Channel
	if err := h.db.First(&ch, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			resp(c, 404, "渠道不存在", nil)
		} else {
			resp(c, 500, "查询失败: "+err.Error(), nil)
		}
		return
	}

	// ③ 删除：DELETE FROM channels WHERE id = ?
	//    （GORM 的 Delete 默认支持软删除——模型里有 DeletedAt 字段才会软删，这里没有 = 物理删除）
	if err := h.db.Delete(&ch).Error; err != nil {
		resp(c, 500, "删除失败: "+err.Error(), nil)
		return
	}
	h.cache.Refresh() // 热更新：删除的渠道立即从候选池移除
	resp(c, 0, "ok", nil) // 删除成功，无数据返回
}
