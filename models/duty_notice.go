package models

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// DutyNotice deliberately never stores destination credentials in public columns.
type DutyNotice struct {
	ID                int64  `json:"id" gorm:"primaryKey"`
	DedupKey          string `json:"-" gorm:"size:64;uniqueIndex"`
	Kind              string `json:"kind"`
	GroupID           int64  `json:"group_id" gorm:"index"`
	ResponseID        int64  `json:"response_id" gorm:"index"`
	AssignmentVersion int64  `json:"-"`
	RuleID            int64  `json:"rule_id"`
	ChannelID         int64  `json:"channel_id"`
	ConfigIndex       int    `json:"-"`
	Recipient         string `json:"recipient"`
	Status            string `json:"status" gorm:"size:20;index"`
	Reason            string `json:"reason"`
	Payload           string `json:"-" gorm:"type:text"`
	CreatedAt         int64  `json:"created_at"`
	StartedAt         int64  `json:"started_at"`
	FinishedAt        int64  `json:"finished_at"`
	ExpiresAt         int64  `json:"-"`
}

func (DutyNotice) TableName() string { return "response_duty_notice" }

type DutyNoticePayload struct {
	Event          AlertCurEvent `json:"event"`
	Fallback       DutyFallback  `json:"fallback"`
	Endpoint       DutyEndpoint  `json:"endpoint"`
	Actor          string        `json:"actor"`
	FailureChannel string        `json:"failure_channel"`
	FailureReason  string        `json:"failure_reason"`
	FailedAt       int64         `json:"failed_at"`
}

func (n DutyNotice) Decode() (DutyNoticePayload, error) {
	var p DutyNoticePayload
	err := openDuty(n.Payload, &p)
	return p, err
}

type DutyReminderClock struct {
	GroupID int64 `gorm:"primaryKey"`
	NextAt  int64
}

func (DutyReminderClock) TableName() string { return "response_duty_reminder_clock" }

var noticeURL = regexp.MustCompile(`https?://[^\s<>"']+`)
var noticeSecret = regexp.MustCompile(`(?i)(password|secret|token|authorization|access[_-]?key)\s*[:=]\s*[^\s,;]+`)

func SafeNoticeText(s string) string {
	s = noticeURL.ReplaceAllString(s, "[地址已隐藏]")
	s = noticeSecret.ReplaceAllString(s, "$1=[已隐藏]")
	r := []rune(s)
	if len(r) > 600 {
		s = string(r[:600]) + "…"
	}
	return s
}
func noticeKey(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func UserDisplay(u User) string {
	if u.Nickname != "" && u.Nickname != u.Username {
		return u.Nickname + "（" + u.Username + "）"
	}
	return u.Username
}
func insertDutyNotice(tx *gorm.DB, n *DutyNotice, p DutyNoticePayload) error {
	encrypted, e := sealDuty(p)
	if e != nil {
		return e
	}
	n.Payload = encrypted
	n.Status = "pending"
	if n.CreatedAt == 0 {
		n.CreatedAt = time.Now().Unix()
	}
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "dedup_key"}}, DoNothing: true}).Create(n).Error
}
func dutyNoticeLog(tx *gorm.DB, n DutyNotice, status, reason string) error {
	if n.ResponseID == 0 {
		return nil
	}
	return claimNoticeLog(tx, n.ResponseID, n.Kind+"_notice_"+status, reason)
}
func FinishDutyNotice(db *gorm.DB, id int64, status, reason string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var n DutyNotice
		if e := tx.First(&n, id).Error; e != nil {
			return e
		}
		reason = SafeNoticeText(reason)
		r := tx.Model(&DutyNotice{}).Where("id=? AND status=?", id, "sending").Updates(map[string]interface{}{"status": status, "reason": reason, "finished_at": time.Now().Unix()})
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected == 0 {
			return nil
		}
		return dutyNoticeLog(tx, n, status, reason)
	})
}
func LoadResponseEvent(db *gorm.DB, row *EventResponse) (AlertCurEvent, error) {
	var event AlertCurEvent
	err := db.Where("hash=? AND first_trigger_time=?", row.Hash, row.FirstTriggerTime).Take(&event).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = db.Table("alert_his_event").Where("hash=? AND first_trigger_time=?", row.Hash, row.FirstTriggerTime).Order("id DESC").Take(&event).Error
	}
	if err == nil {
		if event.Annotations == "" {
			event.Annotations = "{}"
		}
		if event.RuleConfig == "" {
			event.RuleConfig = "{}"
		}
		err = event.DB2FE()
	}
	return event, err
}
func enqueueLifecycleNotices(tx *gorm.DB, row *EventResponse, actor *User, action string) error {
	event, err := LoadResponseEvent(tx, row)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return claimNoticeLog(tx, row.ID, action+"_notice_skipped", "未发送处理通知：找不到告警快照")
	}
	if err != nil {
		return err
	}
	var rules []NotifyRule
	if len(event.NotifyRuleIds) > 0 {
		if err = tx.Where("id IN ? AND receiver_mode=? AND enable=?", event.NotifyRuleIds, "duty_group", true).Find(&rules).Error; err != nil {
			return err
		}
	}
	count := 0
	for _, r := range rules {
		for index, cfg := range r.NotifyConfigs {
			n := DutyNotice{Kind: action, GroupID: r.DutyGroupID, ResponseID: row.ID, AssignmentVersion: row.AssignmentVersion, RuleID: r.ID, ChannelID: cfg.ChannelID, ConfigIndex: index, Recipient: row.OwnerName}
			n.DedupKey = noticeKey(fmt.Sprintf("%s:%d:%d:%d:%d:%d", action, row.ID, row.AssignmentVersion, r.ID, cfg.ChannelID, index))
			if err = insertDutyNotice(tx, &n, DutyNoticePayload{Event: event, Actor: UserDisplay(*actor)}); err != nil {
				return err
			}
			count++
		}
	}
	if count == 0 {
		return claimNoticeLog(tx, row.ID, action+"_notice_skipped", "未发送处理通知：告警未关联启用的值班组通知渠道")
	}
	return claimNoticeLog(tx, row.ID, action+"_notice_pending", fmt.Sprintf("处理通知已排队，将按告警级别和渠道配置通知负责人（%d项）", count))
}
func (s DutySystemSettings) PrivateFallback() (DutyFallback, error) {
	var fallback DutyFallback
	e := openDuty(s.Secrets, &fallback)
	return fallback, e
}
func EffectiveDutyFallback(db *gorm.DB, groupID int64) (DutyFallback, error) {
	f := DutyFallback{Kind: "inherit"}
	if groupID > 0 {
		var g DutyGroup
		if err := db.First(&g, groupID).Error; err != nil {
			return f, err
		}
		c, err := g.PrivateConfig()
		if err != nil {
			return f, err
		}
		f = c.Fallback
	}
	if f.Kind == "inherit" || f.Kind == "" {
		var s DutySystemSettings
		err := db.First(&s, 1).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return DutyFallback{Kind: "none"}, nil
		}
		if err != nil {
			return f, err
		}
		return s.PrivateFallback()
	}
	return f, nil
}

// Only real delivery failures call this; worker failures never recursively enter NotifyRecord.
// EnqueueNotificationFailure 接收原通知失败结果，按规则/值班组查找兜底目的地并持久化任务。
// 这是失败报告，不是上级联系人升级；被工作流主动丢弃的事件不当作传输失败。
func EnqueueNotificationFailure(db *gorm.DB, event *AlertCurEvent, ruleID int64, channel, target string, sendErr error) error {
	if db == nil || sendErr == nil || event == nil || ruleID <= 0 {
		return nil
	}
	if strings.Contains(sendErr.Error(), "drop by pipeline") {
		return nil
	}
	var rule NotifyRule
	if err := db.First(&rule, ruleID).Error; err != nil {
		return err
	}
	groupID := int64(0)
	if rule.ReceiverMode == "duty_group" {
		groupID = rule.DutyGroupID
	}
	return EnqueueDutyGroupFailure(db, event, ruleID, groupID, channel, target, sendErr)
}
func EnqueueDutyGroupFailure(db *gorm.DB, event *AlertCurEvent, ruleID, groupID int64, channel, target string, sendErr error) error {
	if db == nil || event == nil || sendErr == nil {
		return nil
	}
	f, err := EffectiveDutyFallback(db, groupID)
	if err != nil {
		return err
	}
	var row EventResponse
	e := db.Where("episode=?", ResponseEpisode(event.Hash, event.FirstTriggerTime)).First(&row).Error
	if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
		return e
	}
	recipient := event.DutyRecipientName
	if recipient == "" {
		recipient = SafeNoticeText(target)
	}
	if recipient == "" {
		recipient = "未解析到接收人"
	}
	reason := SafeNoticeText(sendErr.Error())
	n := DutyNotice{Kind: "fallback", GroupID: groupID, RuleID: ruleID, ResponseID: row.ID, Recipient: recipient}
	n.DedupKey = noticeKey(fmt.Sprintf("failure:%s:%d:%d:%d:%d:%d:%t:%s:%s:%s", event.Hash, event.FirstTriggerTime, event.Id, event.TriggerTime, ruleID, groupID, event.IsRecovered, channel, target, reason))
	p := DutyNoticePayload{Event: *event, FailureChannel: channel, FailureReason: reason, FailedAt: time.Now().Unix(), Fallback: f}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := insertDutyNotice(tx, &n, p); err != nil {
			return err
		}
		if n.ID == 0 {
			return nil
		}
		if f.Kind == "none" || f.Kind == "" {
			return tx.Model(&DutyNotice{}).Where("id=?", n.ID).Updates(map[string]interface{}{"status": "skipped", "reason": "未配置失败兜底，仅记录原发送失败：" + reason, "finished_at": time.Now().Unix()}).Error
		}
		return dutyNoticeLog(tx, n, "pending", fmt.Sprintf("%s通知发送失败，兜底消息已排队；接收人：%s；原因：%s", channel, recipient, reason))
	})
}

// Legacy configurations retain the original one-hour cadence.
func (c DutyConfig) ReminderDuration() time.Duration {
	if c.ReminderInterval <= 0 {
		return time.Hour
	}
	if c.ReminderUnit == "minute" {
		return time.Duration(c.ReminderInterval) * time.Minute
	}
	return time.Duration(c.ReminderInterval) * time.Hour
}

// SyncDutyReminderClock 只在开关/实际间隔变化时调整计时，普通组配置保存不推迟下一次提醒。
// 新开启即到期；修改间隔从保存时重新计时，并取消旧间隔尚未发送的任务。
func SyncDutyReminderClock(tx *gorm.DB, g, old DutyGroup) error {
	if g.Config.HourlyReminder == old.Config.HourlyReminder && g.ID == old.ID && g.Config.ReminderDuration() == old.Config.ReminderDuration() {
		return nil
	}
	if g.Config.ReminderDuration() != old.Config.ReminderDuration() {
		if err := tx.Model(&DutyNotice{}).Where("group_id=? AND kind=? AND status=?", g.ID, "duty_reminder", "pending").Updates(map[string]interface{}{"status": "skipped", "reason": "提醒间隔已修改，按新间隔重新计时", "finished_at": time.Now().Unix()}).Error; err != nil {
			return err
		}
	}
	next := int64(0)
	if g.Config.HourlyReminder {
		next = time.Now().Unix()
		if old.Config.HourlyReminder {
			next = time.Now().Add(g.Config.ReminderDuration()).Unix()
		}
	}
	return tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "group_id"}}, DoUpdates: clause.Assignments(map[string]interface{}{"next_at": next})}).Create(&DutyReminderClock{GroupID: g.ID, NextAt: next}).Error
}

// EnqueueDueDutyReminders 以 next_at 条件更新抢占到期时钟，再为组公共渠道创建持久任务。
// 从当前时刻安排下一次，不补停机期间每个遗漏时段；具体当班人在 worker 发送时再解析。
func EnqueueDueDutyReminders(db *gorm.DB, now time.Time) error {
	var clocks []DutyReminderClock
	if e := db.Where("next_at>0 AND next_at<=?", now.Unix()).Limit(100).Find(&clocks).Error; e != nil {
		return e
	}
	for _, clock := range clocks {
		if e := db.Transaction(func(tx *gorm.DB) error {
			var g DutyGroup
			if e := tx.First(&g, clock.GroupID).Error; e != nil {
				return e
			}
			interval := g.Config.ReminderDuration()
			r := tx.Model(&DutyReminderClock{}).Where("group_id=? AND next_at=?", clock.GroupID, clock.NextAt).Update("next_at", now.Add(interval).Unix())
			if r.Error != nil {
				return r.Error
			}
			if r.RowsAffected == 0 {
				return nil
			}
			if !g.Config.HourlyReminder {
				return nil
			}
			private, e := g.PrivateConfig()
			if e != nil {
				return e
			}
			for _, endpoint := range private.Endpoints {
				if endpoint.URL == "" {
					continue
				}
				n := DutyNotice{Kind: "duty_reminder", GroupID: g.ID, CreatedAt: now.Unix(), ExpiresAt: now.Add(interval).Unix(), DedupKey: noticeKey(fmt.Sprintf("reminder:%d:%d:%s", g.ID, clock.NextAt, endpoint.Kind))}
				if e = insertDutyNotice(tx, &n, DutyNoticePayload{Endpoint: endpoint}); e != nil {
					return e
				}
			}
			return nil
		}); e != nil {
			return e
		}
	}
	return nil
}

// Explicit ownership wins for recovery and transfer; otherwise resolve the live roster.
// ResolveDutyRecipient 优先级：显式转交目标 → 恢复事件原认领人 → 当前排班。
// 已认领但未恢复的普通重复通知由上层拦截；这里不自动随交班转走已有认领。
func ResolveDutyRecipient(db *gorm.DB, g DutyGroup, event *AlertCurEvent, explicit int64) (User, error) {
	id := explicit
	if id == 0 && event != nil && event.IsRecovered {
		var row EventResponse
		e := db.Where("episode=?", ResponseEpisode(event.Hash, event.FirstTriggerTime)).First(&row).Error
		if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
			return User{}, e
		}
		id = row.OwnerID
	}
	if id == 0 {
		r, e := ResolveDuty(db, g, time.Now())
		if e != nil {
			return User{}, e
		}
		if !r.Available {
			return User{}, errors.New(r.Reason)
		}
		id = r.UserID
	}
	var user User
	if e := db.First(&user, id).Error; e != nil || user.IsDisabled() {
		return User{}, errors.New("接收账号不存在或已停用")
	}
	return user, nil
}

func validateDutyEmailChannel(db *gorm.DB, f DutyFallback) error {
	if f.Kind != "email" {
		return nil
	}
	if f.SMTPChannelID < 0 {
		return errors.New("邮件媒介ID无效")
	}
	q := db.Model(&NotifyChannelConfig{}).Where("request_type=? AND enable=?", "smtp", true)
	if f.SMTPChannelID > 0 {
		q = q.Where("id=?", f.SMTPChannelID)
	}
	var count int64
	if e := q.Count(&count).Error; e != nil {
		return e
	}
	if f.SMTPChannelID > 0 && count != 1 {
		return errors.New("请选择启用的邮件媒介")
	}
	if f.SMTPChannelID == 0 && count > 1 {
		return errors.New("存在多个邮件媒介，请明确选择兜底使用的媒介")
	}
	return nil
}
