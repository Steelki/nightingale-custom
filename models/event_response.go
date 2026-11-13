package models

import (
	"errors"
	"fmt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"strings"
	"time"
)

// EventResponse tracks a native alert episode, independent of changing history row IDs.
// Cross-severity episode association is deliberately handled separately.
type EventResponse struct {
	AssignmentVersion int64  `json:"assignment_version"`
	ID                int64  `json:"id" gorm:"primaryKey"`
	Episode           string `json:"episode" gorm:"size:160;uniqueIndex"`
	Hash              string `json:"hash" gorm:"size:128;index"`
	FirstTriggerTime  int64  `json:"first_trigger_time"`
	OwnerID           int64  `json:"owner_id"`
	OwnerName         string `json:"owner_name"`
	Recovered         bool   `json:"recovered"`
	UnclaimedSince    int64  `json:"unclaimed_since"`
	Version           int64  `json:"version"`
	UpdatedAt         int64  `json:"updated_at"`
}

func (EventResponse) TableName() string { return "response_event" }

type EventResponseLog struct {
	ID         int64  `json:"id" gorm:"primaryKey"`
	ResponseID int64  `json:"response_id" gorm:"index"`
	Actor      string `json:"actor"`
	Action     string `json:"action"`
	Content    string `json:"content" gorm:"type:text"`
	CreatedAt  int64  `json:"created_at"`
}

func (EventResponseLog) TableName() string            { return "response_event_log" }
func ResponseEpisode(hash string, first int64) string { return fmt.Sprintf("%s:%d", hash, first) }
func ObserveEventResponse(db *gorm.DB, event *AlertCurEvent) error {
	if db == nil || event.Hash == "" || event.FirstTriggerTime <= 0 {
		return nil
	}
	now := time.Now().Unix()
	row := EventResponse{Episode: ResponseEpisode(event.Hash, event.FirstTriggerTime), Hash: event.Hash, FirstTriggerTime: event.FirstTriggerTime, UnclaimedSince: event.FirstTriggerTime, Version: 1, UpdatedAt: now, Recovered: false}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "episode"}}, DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		if !event.IsRecovered {
			return nil
		}
		var current EventResponse
		if err := tx.Where("episode=?", row.Episode).First(&current).Error; err != nil {
			return err
		}
		result := tx.Model(&EventResponse{}).Where("id=? AND recovered=?", current.ID, false).Updates(map[string]interface{}{"recovered": true, "version": gorm.Expr("version + 1"), "updated_at": now})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected > 0 {
			return tx.Create(&EventResponseLog{ResponseID: current.ID, Actor: "system", Action: "recovered", Content: "告警已恢复", CreatedAt: now}).Error
		}
		return nil
	})
}
func ResponseIsClaimed(db *gorm.DB, event *AlertCurEvent) (bool, error) {
	if db == nil || event.IsRecovered || event.FirstTriggerTime <= 0 {
		return false, nil
	}
	var row EventResponse
	err := db.Where("episode=?", ResponseEpisode(event.Hash, event.FirstTriggerTime)).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return row.OwnerID > 0 && !row.Recovered, err
}

func ChangeEventResponse(db *gorm.DB, row *EventResponse, version int64, actor *User, action, content string, target int64) error {
	if actor == nil || actor.IsDisabled() {
		return errors.New("账号不可用")
	}
	content = strings.TrimSpace(content)
	if len(content) > 12000 {
		return errors.New("处理记录最多12000字节")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.First(row, row.ID).Error; err != nil {
			return err
		}
		if row.Version != version {
			return ErrDutyConflict
		}
		if row.Recovered && action != "comment" {
			return errors.New("告警已恢复，不能变更认领状态")
		}
		switch action {
		case "claim":
			if row.OwnerID != 0 {
				return errors.New("告警已被认领，请刷新")
			}
			row.OwnerID = actor.Id
			row.OwnerName = actor.Username
		case "unclaim", "transfer":
			if row.OwnerID == 0 {
				return errors.New("告警尚未认领")
			}
			if row.OwnerID != actor.Id && !actor.IsAdmin() {
				return errors.New("仅当前负责人或管理员可取消认领或转交")
			}
			if action == "unclaim" {
				row.OwnerID = 0
				row.OwnerName = ""
				row.UnclaimedSince = time.Now().Unix()
			} else {
				var user User
				if tx.First(&user, target).Error != nil || user.IsDisabled() {
					return errors.New("目标账号不存在或已停用")
				}
				if row.OwnerID == target {
					return errors.New("目标人员已是负责人")
				}
				row.OwnerID = user.Id
				row.OwnerName = user.Username
			}
		case "comment":
			if content == "" {
				return errors.New("请填写处理进展")
			}
		default:
			return errors.New("不支持的操作")
		}
		if action != "comment" {
			row.AssignmentVersion++
		}
		row.Version++
		row.UpdatedAt = time.Now().Unix()
		result := tx.Model(&EventResponse{}).Where("id=? AND version=?", row.ID, version).Updates(map[string]interface{}{"assignment_version": row.AssignmentVersion, "owner_id": row.OwnerID, "owner_name": row.OwnerName, "unclaimed_since": row.UnclaimedSince, "version": row.Version, "updated_at": row.UpdatedAt})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrDutyConflict
		}
		if action == "claim" || action == "transfer" {
			content = fmt.Sprintf("负责人：%s\n%s", row.OwnerName, content)
		}
		if err := tx.Create(&EventResponseLog{ResponseID: row.ID, Actor: actor.Username, Action: action, Content: content, CreatedAt: row.UpdatedAt}).Error; err != nil {
			return err
		}
		if action == "claim" {
			return enqueueClaimNotices(tx, row, actor)
		}
		if action == "transfer" || action == "unclaim" {
			return enqueueLifecycleNotices(tx, row, actor, action)
		}
		return nil
	})
}
