package models

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

const RequestTypeTeamsVoice = "teams_voice"

// 密钥仅引用服务端环境变量，绝不保存在媒介JSON或浏览器中。
// 映射以本媒介的租户为作用域，源为夜莺账号邮箱，目标为Entra用户Object ID。
type TeamsVoiceConfig struct {
	TenantID     string              `json:"tenant_id"`
	AppID        string              `json:"app_id"`
	SecretEnv    string              `json:"secret_env"`
	PublicURL    string              `json:"public_url"`
	SpeechRegion string              `json:"speech_region"`
	SpeechKeyEnv string              `json:"speech_key_env"`
	Voice        string              `json:"voice"`
	Mappings     []TeamsVoiceMapping `json:"mappings"`
}
type TeamsVoiceMapping struct {
	Email  string `json:"email"`
	UserID string `json:"user_id"`
}

func (c *TeamsVoiceConfig) Validate() error {
	if c == nil {
		return errors.New("请配置Teams应用内语音")
	}
	if _, e := uuid.Parse(c.TenantID); e != nil || len(c.TenantID) != 36 {
		return errors.New("Teams租户ID须为UUID")
	}
	if _, e := uuid.Parse(c.AppID); e != nil || len(c.AppID) != 36 {
		return errors.New("Teams应用ID须为UUID")
	}
	c.TenantID = strings.ToLower(c.TenantID)
	c.AppID = strings.ToLower(c.AppID)
	env := regexp.MustCompile(`^N9E_TEAMS_[A-Z0-9_]{1,80}$`)
	if !env.MatchString(c.SecretEnv) || !env.MatchString(c.SpeechKeyEnv) {
		return errors.New("密钥环境变量名须以N9E_TEAMS_开头，仅大写字母、数字和下划线")
	}
	u, e := url.Parse(c.PublicURL)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("公网地址须为无认证/查询参数的HTTPS站点地址")
	}
	if !regexp.MustCompile(`^[a-z0-9]{2,40}$`).MatchString(c.SpeechRegion) {
		return errors.New("Azure Speech区域格式无效")
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9-]{3,100}$`).MatchString(c.Voice) {
		return errors.New("Azure Speech音色格式无效")
	}
	if len(c.Mappings) == 0 || len(c.Mappings) > 500 {
		return errors.New("请配置1至500条Teams账号映射")
	}
	seen := map[string]bool{}
	for _, m := range c.Mappings {
		key := strings.ToLower(strings.TrimSpace(m.Email))
		if key == "" || seen[key] {
			return errors.New("Teams账号映射邮箱为空或重复")
		}
		seen[key] = true
		if _, e := uuid.Parse(m.UserID); e != nil || len(m.UserID) != 36 {
			return errors.New("Teams用户Object ID须为UUID")
		}
	}
	return nil
}
func (c TeamsVoiceConfig) Recipient(email string) (string, error) {
	for _, m := range c.Mappings {
		if strings.EqualFold(strings.TrimSpace(m.Email), strings.TrimSpace(email)) {
			return m.UserID, nil
		}
	}
	return "", errors.New("接收账号未配置本租户Teams用户映射")
}

// 呼叫事实与认领完全分离。State只描述调用生命周期；AnsweredAt一旦写入就不回退。
// Payload为加密事件/配置快照；AudioToken不返回给管理API。
type TeamsVoiceCall struct {
	ID             string               `json:"id" gorm:"primaryKey;size:36"`
	Dedup          string               `json:"-" gorm:"size:64;uniqueIndex"`
	Episode        string               `json:"-" gorm:"size:160;index"`
	ChannelID      int64                `json:"channel_id"`
	RuleID         int64                `json:"rule_id"`
	GroupID        int64                `json:"group_id"`
	Recipient      string               `json:"recipient" gorm:"size:320"`
	Title          string               `json:"title" gorm:"type:text"`
	State          string               `json:"state" gorm:"size:24;index"`
	CallID         string               `json:"call_id" gorm:"size:128"`
	AnsweredAt     int64                `json:"answered_at"`
	PlayState      string               `json:"play_state" gorm:"size:24"`
	EndedAt        int64                `json:"ended_at"`
	Reason         string               `json:"reason" gorm:"type:text"`
	CreatedAt      int64                `json:"created_at"`
	ExpiresAt      int64                `json:"expires_at" gorm:"index"`
	FallbackQueued bool                 `json:"-"`
	CloseRequested bool                 `json:"-"`
	AudioToken     string               `json:"-" gorm:"size:64"`
	Payload        TeamsVoiceSealedText `json:"-" gorm:"type:text"`
}

func (TeamsVoiceCall) TableName() string { return "response_teams_voice_call" }

type TeamsVoicePayload struct {
	Config TeamsVoiceConfig
	Event  AlertCurEvent
	Target string
}

func (c TeamsVoiceCall) Decode() (TeamsVoicePayload, error) {
	var p TeamsVoicePayload
	e := openDuty(string(c.Payload), &p)
	return p, e
}
func EnqueueTeamsVoice(db *gorm.DB, config *NotifyChannelConfig, ruleID, groupID int64, events []*AlertCurEvent, recipients []string) ([]string, error) {
	if db == nil {
		return nil, errors.New("Teams语音需要连接中心数据库，不能使用无持久化的测试发送")
	}
	if config == nil || config.RequestConfig == nil {
		return nil, errors.New("Teams配置缺失")
	}
	cfg := config.RequestConfig.TeamsVoiceConfig
	if e := cfg.Validate(); e != nil {
		return nil, e
	}
	if len(events) == 0 || len(recipients) == 0 {
		return nil, errors.New("Teams语音缺少告警或接收人")
	}
	ids := []string{}
	err := db.Transaction(func(tx *gorm.DB) error {
		for _, evt := range events {
			if evt == nil || evt.Hash == "" || evt.FirstTriggerTime <= 0 {
				return errors.New("告警无效")
			}
			if evt.IsRecovered {
				continue
			}
			for _, email := range recipients {
				target, e := cfg.Recipient(email)
				if e != nil {
					return e
				}
				payload, e := sealDuty(TeamsVoicePayload{Config: *cfg, Event: *evt, Target: target})
				if e != nil {
					return e
				}
				raw, _ := json.Marshal([]interface{}{config.ID, ruleID, groupID, evt.Hash, evt.FirstTriggerTime, evt.TriggerTime, strings.ToLower(email)})
				hash := sha256.Sum256(raw)
				now := time.Now().Unix()
				row := TeamsVoiceCall{ID: uuid.NewString(), Dedup: hex.EncodeToString(hash[:]), Episode: ResponseEpisode(evt.Hash, evt.FirstTriggerTime), ChannelID: config.ID, RuleID: ruleID, GroupID: groupID, Recipient: email, Title: evt.RuleName, State: "pending", CreatedAt: now, ExpiresAt: now + 900, AudioToken: uuid.NewString(), Payload: TeamsVoiceSealedText(payload)}
				if e = tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "dedup"}}, DoNothing: true}).Create(&row).Error; e != nil {
					return e
				}
				var saved TeamsVoiceCall
				if e = tx.Where("dedup=?", row.Dedup).First(&saved).Error; e != nil {
					return e
				}
				ids = append(ids, saved.ID)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("Teams语音入队失败: %w", err)
	}
	return ids, nil
}

// 大事件快照在MySQL中不能使用仅64KB的TEXT。
type TeamsVoiceSealedText string

func (TeamsVoiceSealedText) GormDataType() string { return "string" }
func (TeamsVoiceSealedText) GormDBDataType(db *gorm.DB, _ *schema.Field) string {
	if db.Dialector.Name() == "mysql" {
		return "longtext"
	}
	return "text"
}
