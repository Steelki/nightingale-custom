package models

import (
	"errors"
	"reflect"
	"sort"
	"time"

	"gorm.io/gorm"
)

// RotationChange is a command, never persisted as part of the group.
type RotationChange struct {
	Mode         string  `json:"mode"` // immediate, next_shift, date, cancel
	Date         string  `json:"date,omitempty"`
	RotationDays int     `json:"rotation_days"`
	UserIDs      []int64 `json:"user_ids"`
	StartUserID  int64   `json:"start_user_id"`
}

func dutyPlanStart(p DutyPlan, loc *time.Location) time.Time {
	if p.EffectiveAt > 0 {
		return time.Unix(p.EffectiveAt, 0).In(loc)
	}
	t, _ := time.ParseInLocation("2006-01-02", p.EffectiveDate, loc)
	return t
}
func activeDutyPlan(c DutyConfig, at time.Time) *DutyPlan {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil
	}
	var chosen *DutyPlan
	for i := range c.Plans {
		p := &c.Plans[i]
		start := dutyPlanStart(*p, loc)
		if !start.After(at) && (chosen == nil || start.After(dutyPlanStart(*chosen, loc))) {
			chosen = p
		}
	}
	return chosen
}
func nextDutyShift(c DutyConfig, now time.Time) time.Time {
	p := activeDutyPlan(c, now)
	if p == nil || p.RotationDays < 1 {
		return time.Time{}
	}
	loc, _ := time.LoadLocation(c.Timezone)
	anchor, _ := time.Parse("2006-01-02", p.EffectiveDate)
	day, _ := time.Parse("2006-01-02", now.In(loc).Format("2006-01-02"))
	days := int(day.Sub(anchor).Hours() / 24)
	next := anchor.AddDate(0, 0, (days/p.RotationDays+1)*p.RotationDays)
	result, _ := time.ParseInLocation("2006-01-02", next.Format("2006-01-02"), loc)
	return result
}

// PrepareDutyRotation preserves every effective version, even against stale/legacy clients.
func PrepareDutyRotation(g *DutyGroup, old DutyGroup, now time.Time) error {
	loc, err := time.LoadLocation(g.Config.Timezone)
	if err != nil {
		return errors.New("时区无效")
	}
	if old.ID == 0 {
		if g.RotationChange != nil {
			return errors.New("新建值班组请直接配置初始轮班规则")
		}
		if len(g.Config.Plans) != 1 {
			return errors.New("新建值班组只能配置一条当前轮班规则")
		}
		if dutyPlanStart(g.Config.Plans[0], loc).After(now) {
			return errors.New("初始轮班规则须现在已生效，不能留下排班空档")
		}
		return nil
	}
	if g.Config.Timezone != old.Config.Timezone {
		return errors.New("已有轮班规则的时区不能修改，以免改变历史排班")
	}
	if !reflect.DeepEqual(g.Config.Plans, old.Config.Plans) {
		return errors.New("不能直接覆盖轮班历史，请使用修改轮班规则")
	}
	ch := g.RotationChange
	if ch == nil {
		return nil
	}
	plans := []DutyPlan{}
	for _, p := range old.Config.Plans {
		if !dutyPlanStart(p, loc).After(now) {
			plans = append(plans, p)
		}
	}
	if len(plans) == 0 {
		return errors.New("当前没有生效的轮班规则，请先修复初始安排")
	}
	if ch.Mode == "cancel" {
		g.Config.Plans = plans
		return nil
	}
	if ch.RotationDays < 1 || ch.RotationDays > 365 || len(ch.UserIDs) == 0 || !uniqueDutyIDs(ch.UserIDs) {
		return errors.New("轮班须配置1至365天及不重复的参与人员")
	}
	index := -1
	for i, id := range ch.UserIDs {
		if id == ch.StartUserID {
			index = i
		}
	}
	if index < 0 {
		return errors.New("请选择新规则的起始值班人员")
	}
	var at time.Time
	switch ch.Mode {
	case "immediate":
		at = now.Truncate(time.Second)
	case "next_shift":
		at = nextDutyShift(old.Config, now)
	case "date":
		at, err = time.ParseInLocation("2006-01-02", ch.Date, loc)
		if err != nil || !at.After(now) {
			return errors.New("指定生效日期须晚于今天（值班组时区）")
		}
	default:
		return errors.New("轮班生效方式无效")
	}
	if at.IsZero() {
		return errors.New("无法确定下次交班时间")
	}
	for _, p := range plans {
		if !at.After(dutyPlanStart(p, loc)) {
			return errors.New("轮班刚刚调整，请稍后重试")
		}
	}
	ids := append([]int64{}, ch.UserIDs[index:]...)
	ids = append(ids, ch.UserIDs[:index]...)
	plans = append(plans, DutyPlan{EffectiveDate: at.In(loc).Format("2006-01-02"), EffectiveAt: at.Unix(), RotationDays: ch.RotationDays, UserIDs: ids})
	g.Config.Plans = plans
	return nil
}

type DutyRotationPreview struct {
	Current   DutyResolution   `json:"current"`
	NextShift string           `json:"next_shift"`
	Plans     []DutyPlan       `json:"plans"`
	Rows      []DutyResolution `json:"rows"`
}

// DutyCalendarRows includes intra-day changes, instead of hiding immediate changes behind midnight samples.
func DutyCalendarRows(db *gorm.DB, g DutyGroup, start time.Time, days int) ([]DutyResolution, error) {
	loc, err := time.LoadLocation(g.Config.Timezone)
	if err != nil {
		return nil, err
	}
	end := start.AddDate(0, 0, days)
	times := []time.Time{}
	for i := 0; i < days; i++ {
		times = append(times, start.AddDate(0, 0, i))
	}
	for _, p := range g.Config.Plans {
		t := dutyPlanStart(p, loc)
		if t.After(start) && t.Before(end) {
			times = append(times, t)
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	rows := []DutyResolution{}
	var last time.Time
	for _, t := range times {
		if t.Equal(last) {
			continue
		}
		last = t
		r, e := ResolveDuty(db, g, t)
		if e != nil {
			return nil, e
		}
		rows = append(rows, r)
	}
	return rows, nil
}
func PreviewDutyRotation(db *gorm.DB, g DutyGroup, now time.Time) (DutyRotationPreview, error) {
	var result DutyRotationPreview
	var old DutyGroup
	if g.ID > 0 {
		if err := db.First(&old, g.ID).Error; err != nil {
			return result, err
		}
		if old.Version != g.Version {
			return result, ErrDutyConflict
		}
	}
	if err := PrepareDutyRotation(&g, old, now); err != nil {
		return result, err
	}
	// Preview validates rotation and membership, without processing encrypted delivery settings.
	g.Config.HourlyReminder = false
	g.Config.Endpoints = nil
	g.Config.Fallback = DutyFallback{Kind: "inherit"}
	g.Config.Steps = nil
	if err := g.Validate(db); err != nil {
		return result, err
	}
	result.Plans = g.Config.Plans
	var err error
	result.Current, err = ResolveDuty(db, g, now)
	if err != nil {
		return result, err
	}
	next := nextDutyShift(g.Config, now)
	loc, _ := time.LoadLocation(g.Config.Timezone)
	for _, p := range g.Config.Plans {
		t := dutyPlanStart(p, loc)
		if t.After(now) && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	if !next.IsZero() {
		result.NextShift = next.Format(time.RFC3339)
	}
	// Show the new rule even when the scheduled change is more than two weeks away.
	start := now.In(loc)
	if g.RotationChange != nil && g.RotationChange.Mode != "cancel" {
		start = dutyPlanStart(g.Config.Plans[len(g.Config.Plans)-1], loc)
	}
	day, _ := time.ParseInLocation("2006-01-02", start.Format("2006-01-02"), loc)
	result.Rows, err = DutyCalendarRows(db, g, day, 14)
	return result, err
}
