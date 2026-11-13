package router

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ginx"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func (rt *Router) registerDutyRoutes(p *gin.RouterGroup) {
	p.GET("/event-response", rt.auth(), rt.user(), rt.eventResponse)
	p.POST("/event-response", rt.auth(), rt.user(), rt.eventResponse)
	p.GET("/duty-groups", rt.auth(), rt.user(), rt.dutyGroups)
	p.GET("/duty-groups/:id", rt.auth(), rt.user(), rt.dutyGroup)
	p.POST("/duty-groups/preview", rt.auth(), rt.user(), rt.admin(), rt.dutyRotationPreview)
	p.POST("/duty-groups", rt.auth(), rt.user(), rt.admin(), rt.dutyGroupSave)
	p.PUT("/duty-groups/:id", rt.auth(), rt.user(), rt.admin(), rt.dutyGroupSave)
	p.GET("/duty-groups/:id/calendar", rt.auth(), rt.user(), rt.dutyCalendar)
	p.GET("/duty-groups/:id/notices", rt.auth(), rt.user(), rt.dutyNotices)
	p.GET("/duty-groups/:id/audit", rt.auth(), rt.user(), rt.admin(), rt.dutyAudit)
	p.GET("/duty-system-settings", rt.auth(), rt.user(), rt.admin(), rt.dutySettings)
	p.PUT("/duty-system-settings", rt.auth(), rt.user(), rt.admin(), rt.dutySettings)
}
func dutyHTTPError(c *gin.Context, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, models.ErrDutyConflict) {
		status = http.StatusConflict
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		status = http.StatusNotFound
	}
	ginx.NewRender(c, status).Message(err)
}
func (rt *Router) dutyGroups(c *gin.Context) {
	var rows []models.DutyGroup
	db := models.DB(rt.Ctx).Order("name,id")
	q := c.Query("query")
	if len(q) > 200 {
		ginx.Bomb(400, "query too long")
	}
	if q != "" {
		db = db.Where("name LIKE ?", "%"+q+"%")
	}
	ginx.Dangerous(db.Limit(500).Find(&rows).Error)
	if rows == nil {
		rows = []models.DutyGroup{}
	}
	ginx.NewRender(c).Data(rows, nil)
}
func (rt *Router) loadDuty(c *gin.Context) (models.DutyGroup, error) {
	var g models.DutyGroup
	id, e := strconv.ParseInt(c.Param("id"), 10, 64)
	if e != nil || id < 1 {
		return g, errors.New("值班组ID无效")
	}
	e = models.DB(rt.Ctx).First(&g, id).Error
	return g, e
}
func (rt *Router) dutyGroup(c *gin.Context) {
	g, e := rt.loadDuty(c)
	if e != nil {
		dutyHTTPError(c, e)
		return
	}
	ginx.NewRender(c).Data(g, nil)
}
func (rt *Router) dutyGroupSave(c *gin.Context) {
	var g models.DutyGroup
	if err := c.ShouldBindJSON(&g); err != nil {
		dutyHTTPError(c, errors.New("请求格式错误"))
		return
	}
	g.ID = 0
	if c.Request.Method == "PUT" {
		old, e := rt.loadDuty(c)
		if e != nil {
			dutyHTTPError(c, e)
			return
		}
		g.ID = old.ID
	}
	if e := models.SaveDutyGroup(models.DB(rt.Ctx), &g, c.MustGet("username").(string)); e != nil {
		dutyHTTPError(c, e)
		return
	}
	ginx.NewRender(c).Data(g, nil)
}
func (rt *Router) dutyCalendar(c *gin.Context) {
	g, e := rt.loadDuty(c)
	if e != nil {
		dutyHTTPError(c, e)
		return
	}
	loc, e := time.LoadLocation(g.Config.Timezone)
	if e != nil {
		dutyHTTPError(c, e)
		return
	}
	start := c.DefaultQuery("start", time.Now().In(loc).Format("2006-01-02"))
	day, e := time.ParseInLocation("2006-01-02", start, loc)
	if e != nil {
		dutyHTTPError(c, e)
		return
	}
	days, e := strconv.Atoi(c.DefaultQuery("days", "14"))
	if e != nil || days < 1 || days > 93 {
		dutyHTTPError(c, errors.New("日历范围须1至93天"))
		return
	}
	rows, e := models.DutyCalendarRows(models.DB(rt.Ctx), g, day, days)
	if e != nil {
		dutyHTTPError(c, e)
		return
	}
	ginx.NewRender(c).Data(rows, nil)
}
func (rt *Router) dutyAudit(c *gin.Context) {
	g, e := rt.loadDuty(c)
	if e != nil {
		dutyHTTPError(c, e)
		return
	}
	rows := []models.DutyAudit{}
	ginx.Dangerous(models.DB(rt.Ctx).Where("group_id=?", g.ID).Order("id DESC").Limit(100).Find(&rows).Error)
	ginx.NewRender(c).Data(rows, nil)
}
func (rt *Router) dutySettings(c *gin.Context) {
	db := models.DB(rt.Ctx)
	var s models.DutySystemSettings
	if c.Request.Method == "GET" {
		e := db.First(&s, 1).Error
		if errors.Is(e, gorm.ErrRecordNotFound) {
			s = models.DutySystemSettings{ID: 1}
		} else if e != nil {
			dutyHTTPError(c, e)
			return
		}
	} else {
		if e := c.ShouldBindJSON(&s); e != nil {
			dutyHTTPError(c, errors.New("请求格式错误"))
			return
		}
		if e := models.SaveDutySettings(db, &s, c.MustGet("username").(string)); e != nil {
			dutyHTTPError(c, e)
			return
		}
	}
	ginx.NewRender(c).Data(s, nil)
}

func (rt *Router) dutyRotationPreview(c *gin.Context) {
	var g models.DutyGroup
	if e := c.ShouldBindJSON(&g); e != nil {
		dutyHTTPError(c, errors.New("请求格式错误"))
		return
	}
	result, e := models.PreviewDutyRotation(models.DB(rt.Ctx), g, time.Now())
	if e != nil {
		dutyHTTPError(c, e)
		return
	}
	ginx.NewRender(c).Data(result, nil)
}

func (rt *Router) dutyNotices(c *gin.Context) {
	g, e := rt.loadDuty(c)
	if e != nil {
		dutyHTTPError(c, e)
		return
	}
	rows := []models.DutyNotice{}
	if e = models.DB(rt.Ctx).Where("group_id=?", g.ID).Order("id DESC").Limit(100).Find(&rows).Error; e != nil {
		dutyHTTPError(c, e)
		return
	}
	ginx.NewRender(c).Data(rows, nil)
}
