package teamsvoice

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/toolkits/pkg/logger"
	"gorm.io/gorm"
)

type Worker struct {
	DB       *gorm.DB
	Client   Client
	AudioDir string
}

func Start(db *gorm.DB) func() {
	if os.Getenv("N9E_DISABLE_DUTY_DELIVERY") == "1" || os.Getenv("N9E_DISABLE_TEAMS_VOICE") == "1" {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	w := Worker{DB: db}
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if e := w.RunOnce(ctx, now); e != nil {
					logger.Errorf("Teams voice worker: %v", e)
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}
func (w Worker) dir() string {
	if w.AudioDir != "" {
		return w.AudioDir
	}
	return "data/teams-voice"
}
func (w Worker) update(id string, values map[string]interface{}) error {
	return w.DB.Model(&models.TeamsVoiceCall{}).Where("id=?", id).Updates(values).Error
}
func (w Worker) fail(row models.TeamsVoiceCall, state, reason string) error {
	return w.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&models.TeamsVoiceCall{}).Where("id=? AND state NOT IN ?", row.ID, []string{"failed", "ended", "skipped"}).Updates(map[string]interface{}{"state": state, "reason": reason})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 || state != "failed" {
			return nil
		}
		return reportFailure(tx, row, reason)
	})
}
func (w Worker) RunOnce(ctx context.Context, now time.Time) error {
	// 不重拨进程中断/HTTP超时的呼叫；平台可能已经接受请求。
	if e := w.DB.Model(&models.TeamsVoiceCall{}).Where("state IN ? AND created_at<?", []string{"preparing", "calling"}, now.Add(-3*time.Minute).Unix()).Updates(map[string]interface{}{"state": "unknown", "reason": "任务中断或超时，结果未知，未自动重拨"}).Error; e != nil {
		return e
	}
	var rows []models.TeamsVoiceCall
	if e := w.DB.Where("state=? OR (call_id<>? AND ended_at=0 AND close_requested=?)", "pending", "", false).Order("created_at").Limit(100).Find(&rows).Error; e != nil {
		return e
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return nil
		}
		if row.State == "pending" {
			lock := w.DB.Model(&models.TeamsVoiceCall{}).Where("id=? AND state=?", row.ID, "pending").Update("state", "preparing")
			if lock.Error != nil {
				return lock.Error
			}
			if lock.RowsAffected == 0 {
				continue
			}
			if e := w.create(ctx, row, now); e != nil {
				return e
			}
			continue
		}
		if row.AnsweredAt > 0 && row.PlayState == "" && row.State != "ended" && now.Unix() < row.ExpiresAt {
			lock := w.DB.Model(&models.TeamsVoiceCall{}).Where("id=? AND play_state=? AND ended_at=0", row.ID, "").Update("play_state", "sending")
			if lock.Error != nil {
				return lock.Error
			}
			if lock.RowsAffected == 0 {
				continue
			}
			p, e := row.Decode()
			if e != nil {
				return e
			}
			body := map[string]interface{}{"clientContext": row.ID, "prompts": []interface{}{map[string]interface{}{"@odata.type": "#microsoft.graph.mediaPrompt", "mediaInfo": map[string]string{"@odata.type": "#microsoft.graph.mediaInfo", "uri": strings.TrimRight(p.Config.PublicURL, "/") + "/api/n9e/teams-voice/audio/" + row.ID + "?token=" + row.AudioToken, "resourceId": row.ID}}}}
			_, _, e = w.Client.graph(ctx, p.Config, "POST", "/"+row.CallID+"/playPrompt", body)
			state := "accepted"
			reason := ""
			if e != nil {
				state = "unknown"
				reason = "播放请求失败或结果未知；未重复播放"
			}
			if e = w.DB.Model(&models.TeamsVoiceCall{}).Where("id=? AND play_state=?", row.ID, "sending").Updates(map[string]interface{}{"play_state": state, "reason": reason}).Error; e != nil {
				return e
			}
		}
		if row.PlayState == "completed" || row.PlayState == "failed" || now.Unix() > row.CreatedAt+120 {
			lock := w.DB.Model(&models.TeamsVoiceCall{}).Where("id=? AND close_requested=?", row.ID, false).Update("close_requested", true)
			if lock.Error != nil {
				return lock.Error
			}
			if lock.RowsAffected == 0 {
				continue
			}
			p, e := row.Decode()
			if e != nil {
				return e
			}
			_, _, e = w.Client.graph(ctx, p.Config, "DELETE", "/"+row.CallID, nil)
			if e != nil {
				if e = w.update(row.ID, map[string]interface{}{"reason": "结束呼叫请求结果未知，请以平台结束回执为准"}); e != nil {
					return e
				}
			}
		}
	}
	var expired []models.TeamsVoiceCall
	if e := w.DB.Where("expires_at<? AND audio_token<>?", now.Unix(), "").Limit(100).Find(&expired).Error; e != nil {
		return e
	}
	for _, row := range expired {
		if e := os.Remove(filepath.Join(w.dir(), row.ID+".wav")); e != nil && !os.IsNotExist(e) {
			return e
		}
		values := map[string]interface{}{"audio_token": ""}
		if row.EndedAt == 0 && row.State != "failed" && row.State != "skipped" {
			values["state"] = "unknown"
			values["reason"] = "回执等待超时，最终结果未知"
		}
		if e := w.update(row.ID, values); e != nil {
			return e
		}
	}
	return nil
}
func (w Worker) create(ctx context.Context, row models.TeamsVoiceCall, now time.Time) error {
	if now.Unix() >= row.ExpiresAt {
		return w.fail(row, "skipped", "任务已过期")
	}
	p, e := row.Decode()
	if e != nil {
		return w.fail(row, "failed", "语音配置解密失败")
	}
	if e = p.Config.Validate(); e != nil {
		return w.fail(row, "failed", e.Error())
	}
	// 排队期间认领/恢复后不再拨出；邮箱映射同时要求当前夜莺账号有效。
	var response models.EventResponse
	e = w.DB.Where("episode=?", row.Episode).First(&response).Error
	if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
		return e
	}
	if response.OwnerID > 0 || response.Recovered {
		return w.fail(row, "skipped", "告警已认领或恢复")
	}
	var channel models.NotifyChannelConfig
	if e = w.DB.First(&channel, row.ChannelID).Error; e != nil || !channel.Enable {
		return w.fail(row, "skipped", "通知媒介已停用或删除")
	}
	if row.RuleID > 0 {
		var rule models.NotifyRule
		if e = w.DB.First(&rule, row.RuleID).Error; e != nil || !rule.Enable {
			return w.fail(row, "skipped", "通知规则已停用或删除")
		}
		if row.GroupID > 0 && (rule.ReceiverMode != "duty_group" || rule.DutyGroupID != row.GroupID) {
			return w.fail(row, "skipped", "通知接收组已变更")
		}
	}
	var user models.User
	if e = w.DB.Where("email=?", row.Recipient).First(&user).Error; row.GroupID == 0 && (e != nil || user.IsDisabled()) {
		return w.fail(row, "failed", "接收账号不存在或已停用")
	}
	if row.GroupID > 0 {
		var group models.DutyGroup
		if e = w.DB.First(&group, row.GroupID).Error; e != nil {
			return w.fail(row, "failed", "值班组不存在")
		}
		current, e := models.ResolveDutyRecipient(w.DB, group, &p.Event, 0)
		if e != nil {
			return w.fail(row, "failed", "当前值班人员不可用")
		}
		target, e := p.Config.Recipient(current.Email)
		if e != nil {
			return w.fail(row, "failed", e.Error())
		}
		p.Target = target
		row.Recipient = current.Email
		if e = w.update(row.ID, map[string]interface{}{"recipient": current.Email}); e != nil {
			return e
		}
	}
	audio, e := w.Client.synthesize(ctx, p.Config, row.Title)
	if e != nil {
		return w.fail(row, "failed", e.Error())
	}
	if e = os.MkdirAll(w.dir(), 0700); e != nil {
		return w.fail(row, "failed", "音频目录不可写")
	}
	if e = os.WriteFile(filepath.Join(w.dir(), row.ID+".wav"), audio, 0600); e != nil {
		return w.fail(row, "failed", "音频保存失败")
	}
	// TTS可能耗时，拨号前再次核对处理状态。
	var latest models.EventResponse
	e = w.DB.Where("episode=?", row.Episode).First(&latest).Error
	if e != nil && !errors.Is(e, gorm.ErrRecordNotFound) {
		return e
	}
	if latest.OwnerID > 0 || latest.Recovered {
		return w.fail(row, "skipped", "告警已认领或恢复")
	}
	if e = w.update(row.ID, map[string]interface{}{"state": "calling"}); e != nil {
		return e
	}
	body := map[string]interface{}{"@odata.type": "#microsoft.graph.call", "callbackUri": strings.TrimRight(p.Config.PublicURL, "/") + "/api/n9e/teams-voice/callback/" + row.ID, "targets": []interface{}{map[string]interface{}{"@odata.type": "#microsoft.graph.invitationParticipantInfo", "identity": map[string]interface{}{"@odata.type": "#microsoft.graph.identitySet", "user": map[string]string{"@odata.type": "#microsoft.graph.identity", "id": p.Target}}}}, "requestedModalities": []string{"audio"}, "mediaConfig": map[string]string{"@odata.type": "#microsoft.graph.serviceHostedMediaConfig"}, "tenantId": p.Config.TenantID}
	raw, code, e := w.Client.graph(ctx, p.Config, "POST", "", body)
	if e != nil {
		state := "unknown"
		if code == -1 || (code >= 400 && code < 500) {
			state = "failed"
		}
		return w.fail(row, state, e.Error())
	}
	var call struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &call) != nil || !validCallID(call.ID) {
		return w.fail(row, "unknown", "Teams未返回有效呼叫ID，未自动重拨")
	}
	if e := bindCall(w.DB, row.ID, call.ID); e != nil {
		return e
	}
	return w.DB.Model(&models.TeamsVoiceCall{}).Where("id=? AND state=?", row.ID, "calling").Update("state", "accepted").Error
}

func reportFailure(db *gorm.DB, row models.TeamsVoiceCall, reason string) error {
	res := db.Model(&models.TeamsVoiceCall{}).Where("id=? AND fallback_queued=?", row.ID, false).Update("fallback_queued", true)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return nil
	}
	p, e := row.Decode()
	if e != nil {
		return e
	}
	return models.EnqueueDutyGroupFailure(db, &p.Event, row.RuleID, row.GroupID, "Teams应用内语音", row.Recipient, errors.New(reason))
}
