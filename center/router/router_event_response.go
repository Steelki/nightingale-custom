package router

import (
	"errors"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ginx"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"net/http"
)

// eventResponse 是处理进展的单一API：GET读取状态/日志/通知记录，POST执行带版本号的动作。
// 状态关联使用告警周期参数；权限和事务规则仍由 ChangeEventResponse 兜底，不能只依赖前端按钮。
func (rt *Router) eventResponse(c *gin.Context) {
	actor := c.MustGet("user").(*models.User)
	if actor.IsDisabled() {
		ginx.Bomb(http.StatusForbidden, "账号已停用")
	}
	var query struct {
		Hash  string `form:"hash" binding:"required"`
		First int64  `form:"first_trigger_time" binding:"required"`
	}
	if c.ShouldBindQuery(&query) != nil || len(query.Hash) > 128 || query.First <= 0 {
		ginx.Bomb(400, "告警关联参数无效")
	}
	db := models.DB(rt.Ctx)
	calls := []models.TeamsVoiceCall{}
	if err := db.Where("episode=?", models.ResponseEpisode(query.Hash, query.First)).Order("created_at DESC").Limit(30).Find(&calls).Error; err != nil {
		dutyHTTPError(c, err)
		return
	}
	var row models.EventResponse
	err := db.Where("episode=?", models.ResponseEpisode(query.Hash, query.First)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// Lazy initialization only for a verified current episode, including pre-upgrade alerts.
		var event models.AlertCurEvent
		if db.Where("hash=? AND first_trigger_time=?", query.Hash, query.First).First(&event).Error != nil {
			if c.Request.Method == "GET" {
				var count int64
				if db.Model(&models.AlertHisEvent{}).Where("hash=? AND first_trigger_time=?", query.Hash, query.First).Count(&count).Error == nil && count > 0 {
					ginx.NewRender(c).Data(gin.H{"teams_voice_calls": calls, "state": nil, "logs": []models.EventResponseLog{}, "unavailable_reason": "此历史周期未建立处理记录"}, nil)
					return
				}
			}
			dutyHTTPError(c, gorm.ErrRecordNotFound)
			return
		}
		if c.Request.Method == "GET" {
			ginx.NewRender(c).Data(gin.H{"teams_voice_calls": calls, "state": models.EventResponse{Hash: query.Hash, FirstTriggerTime: query.First}, "logs": []models.EventResponseLog{}}, nil)
			return
		}
		if err = models.ObserveEventResponse(db, &event); err == nil {
			err = db.Where("episode=?", models.ResponseEpisode(query.Hash, query.First)).First(&row).Error
		}
	}
	if err != nil {
		dutyHTTPError(c, err)
		return
	}
	if c.Request.Method == "POST" {
		var input struct {
			Version int64  `json:"version"`
			Action  string `json:"action"`
			Content string `json:"content"`
			Target  int64  `json:"target_user_id"`
		}
		if c.ShouldBindJSON(&input) != nil {
			ginx.Bomb(400, "请求格式错误")
		}
		// A pre-upgrade lazy state is exposed as version zero until first persisted.
		if input.Version == 0 && row.Version == 1 && row.OwnerID == 0 {
			input.Version = 1
		}
		if err = models.ChangeEventResponse(db, &row, input.Version, actor, input.Action, input.Content, input.Target); err != nil {
			dutyHTTPError(c, err)
			return
		}
	}
	logs := []models.EventResponseLog{}
	if err = db.Where("response_id=?", row.ID).Order("id DESC").Limit(100).Find(&logs).Error; err != nil {
		dutyHTTPError(c, err)
		return
	}
	notices := []models.ClaimNotice{}
	if err = db.Where("response_id=?", row.ID).Order("id DESC").Limit(20).Find(&notices).Error; err != nil {
		dutyHTTPError(c, err)
		return
	}
	jobs := []models.DutyNotice{}
	if err = db.Where("response_id=?", row.ID).Order("id DESC").Limit(30).Find(&jobs).Error; err != nil {
		dutyHTTPError(c, err)
		return
	}
	ginx.NewRender(c).Data(gin.H{"teams_voice_calls": calls, "duty_notices": jobs, "state": row, "logs": logs, "claim_notices": notices}, nil)
}
