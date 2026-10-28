package handler

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"llm-gateway/internal/model"
	"llm-gateway/internal/usage"
)

// ModelPricingHandler 模型单价管理 CRUD
type ModelPricingHandler struct {
	db      *gorm.DB
	pricing *usage.PricingCache
}

func NewModelPricingHandler(db *gorm.DB, pricing *usage.PricingCache) *ModelPricingHandler {
	return &ModelPricingHandler{db: db, pricing: pricing}
}

// List GET /api/model-pricing
func (h *ModelPricingHandler) List(c *gin.Context) {
	var rows []model.ModelPricing
	if err := h.db.Order("model_name asc").Find(&rows).Error; err != nil {
		resp(c, http.StatusInternalServerError, "查询失败", nil)
		return
	}
	resp(c, http.StatusOK, "ok", rows)
}

// Create POST /api/model-pricing
func (h *ModelPricingHandler) Create(c *gin.Context) {
	var row model.ModelPricing
	if err := c.ShouldBindJSON(&row); err != nil {
		resp(c, http.StatusBadRequest, "参数错误", nil)
		return
	}
	if row.ModelName == "" {
		resp(c, http.StatusBadRequest, "模型名不能为空", nil)
		return
	}
	if err := h.db.Create(&row).Error; err != nil {
		resp(c, http.StatusInternalServerError, "创建失败（模型名可能重复）", nil)
		return
	}
	h.pricing.Refresh()
	resp(c, http.StatusOK, "ok", row)
}

// Update PUT /api/model-pricing/:id
func (h *ModelPricingHandler) Update(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	var row model.ModelPricing
	if err := h.db.First(&row, id).Error; err != nil {
		resp(c, http.StatusNotFound, "记录不存在", nil)
		return
	}
	var in model.ModelPricing
	if err := c.ShouldBindJSON(&in); err != nil {
		resp(c, http.StatusBadRequest, "参数错误", nil)
		return
	}
	row.InputPrice = in.InputPrice
	row.OutputPrice = in.OutputPrice
	row.Enabled = in.Enabled
	h.db.Save(&row)
	h.pricing.Refresh()
	resp(c, http.StatusOK, "ok", row)
}

// Delete DELETE /api/model-pricing/:id
func (h *ModelPricingHandler) Delete(c *gin.Context) {
	id, _ := strconv.Atoi(c.Param("id"))
	h.db.Delete(&model.ModelPricing{}, id)
	h.pricing.Refresh()
	resp(c, http.StatusOK, "ok", nil)
}
