package models

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// ClaimNotice is a durable, single-attempt delivery. Destination secrets are encrypted.
// A unique key ties one destination to one successful claim transition.
type ClaimNotice struct {
	GroupID     int64  `json:"group_id"`
	ID          int64  `json:"id" gorm:"primaryKey"`
	DedupKey    string `json:"-" gorm:"size:64;uniqueIndex"`
	ResponseID  int64  `json:"response_id" gorm:"index"`
	EventID     int64  `json:"event_id"`
	GroupName   string `json:"group_name"`
	Title       string `json:"title"`
	Claimant    string `json:"claimant"`
	ClaimedAt   int64  `json:"claimed_at"`
	Timezone    string `json:"timezone"`
	Destination string `json:"-" gorm:"type:text"`
	Status      string `json:"status" gorm:"size:20;index"`
	StartedAt   int64  `json:"started_at"`
	FinishedAt  int64  `json:"finished_at"`
	Reason      string `json:"reason"`
}

func (ClaimNotice) TableName() string { return "response_claim_notice" }
func (n ClaimNotice) Endpoint() (DutyEndpoint, error) {
	var endpoint DutyEndpoint
	err := openDuty(n.Destination, &endpoint)
	if err != nil {
		return endpoint, errors.New("钉钉群配置解密失败")
	}
	if endpoint.Kind != "dingtalk" || !validDutyURL(endpoint.URL) {
		return endpoint, errors.New("钉钉群地址无效")
	}
	return endpoint, nil
}

func claimNoticeLog(tx *gorm.DB, id int64, action, content string) error {
	return tx.Create(&EventResponseLog{ResponseID: id, Actor: "system", Action: action, Content: content, CreatedAt: time.Now().Unix()}).Error
}

// Called inside the same transaction as the claim and its audit record.
func enqueueClaimNotices(tx *gorm.DB, response *EventResponse, actor *User) error {
	var event AlertHisEvent
	err := tx.Table("alert_cur_event").Where("hash=? AND first_trigger_time=?", response.Hash, response.FirstTriggerTime).Take(&event).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = tx.Where("hash=? AND first_trigger_time=?", response.Hash, response.FirstTriggerTime).Order("id DESC").Take(&event).Error
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return claimNoticeLog(tx, response.ID, "claim_notice_skipped", "未发送认领通知：找不到关联告警快照")
	}
	if err != nil {
		return err
	}
	if len(event.NotifyRuleIds) == 0 {
		return claimNoticeLog(tx, response.ID, "claim_notice_skipped", "未发送认领通知：告警未关联值班组通知规则")
	}
	var rules []NotifyRule
	if err = tx.Where("id IN ? AND receiver_mode=? AND enable=?", event.NotifyRuleIds, "duty_group", true).Find(&rules).Error; err != nil {
		return err
	}
	groups := map[int64]bool{}
	destinations := map[string]bool{}
	count := 0
	for _, rule := range rules {
		if groups[rule.DutyGroupID] {
			continue
		}
		groups[rule.DutyGroupID] = true
		var group DutyGroup
		if err = tx.First(&group, rule.DutyGroupID).Error; err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if err = claimNoticeLog(tx, response.ID, "claim_notice_failed", "认领成功，但关联值班组不存在，无法发送钉钉通知"); err != nil {
				return err
			}
			continue
		}
		private, e := group.PrivateConfig()
		if e != nil {
			if err = claimNoticeLog(tx, response.ID, "claim_notice_failed", fmt.Sprintf("值班组%s：钉钉群配置解密失败", group.Name)); err != nil {
				return err
			}
			continue
		}
		found := false
		for _, endpoint := range private.Endpoints {
			if endpoint.Kind != "dingtalk" || strings.TrimSpace(endpoint.URL) == "" {
				continue
			}
			found = true
			destHash := sha256.Sum256([]byte(endpoint.URL))
			destKey := hex.EncodeToString(destHash[:])
			if destinations[destKey] {
				continue
			}
			destinations[destKey] = true
			encrypted, e := sealDuty(endpoint)
			if e != nil {
				if err = claimNoticeLog(tx, response.ID, "claim_notice_failed", "认领成功，但无法准备钉钉群配置"); err != nil {
					return err
				}
				continue
			}
			dedup := sha256.Sum256([]byte(fmt.Sprintf("%d:%d:%s", response.ID, response.Version, destKey)))
			claimant := actor.Username
			if actor.Nickname != "" && actor.Nickname != actor.Username {
				claimant = actor.Nickname + "（" + actor.Username + "）"
			}
			job := ClaimNotice{GroupID: group.ID, DedupKey: hex.EncodeToString(dedup[:]), ResponseID: response.ID, EventID: event.Id, GroupName: group.Name, Title: event.RuleName, Claimant: claimant, ClaimedAt: response.UpdatedAt, Timezone: group.Config.Timezone, Destination: encrypted, Status: "pending"}
			if err = tx.Create(&job).Error; err != nil {
				return err
			}
			count++
		}
		if !found {
			if err = claimNoticeLog(tx, response.ID, "claim_notice_skipped", fmt.Sprintf("值班组%s未配置钉钉群，未发送认领通知", group.Name)); err != nil {
				return err
			}
		}
	}
	if len(rules) == 0 {
		return claimNoticeLog(tx, response.ID, "claim_notice_skipped", "未发送认领通知：未找到启用的值班组通知规则")
	}
	if count > 0 {
		return claimNoticeLog(tx, response.ID, "claim_notice_pending", fmt.Sprintf("认领通知已排队，将发送到%d个钉钉群", count))
	}
	return nil
}

func FinishClaimNotice(db *gorm.DB, id int64, status, reason string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var job ClaimNotice
		if err := tx.First(&job, id).Error; err != nil {
			return err
		}
		result := tx.Model(&ClaimNotice{}).Where("id=? AND status=?", id, "sending").Updates(map[string]interface{}{"status": status, "reason": reason, "finished_at": time.Now().Unix()})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		return claimNoticeLog(tx, job.ResponseID, "claim_notice_"+status, fmt.Sprintf("值班组%s：%s", job.GroupName, reason))
	})
}
