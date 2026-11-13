package provider

import (
	"context"
	"errors"
	"github.com/ccfos/nightingale/v6/models"
	"strings"
)

// TeamsVoiceProvider只持久化任务；Graph受理/接通/播报由worker及认证回调记录。
type TeamsVoiceProvider struct{}

func (*TeamsVoiceProvider) Ident() string { return models.RequestTypeTeamsVoice }
func (*TeamsVoiceProvider) Check(c *models.NotifyChannelConfig) error {
	if c == nil || c.RequestType != models.RequestTypeTeamsVoice || c.RequestConfig == nil {
		return errors.New("Teams语音配置无效")
	}
	if c.ParamConfig == nil || c.ParamConfig.UserInfo == nil || c.ParamConfig.UserInfo.ContactKey != "email" {
		return errors.New("Teams语音使用夜莺账号邮箱映射Entra用户，请保留email接收字段")
	}
	return c.RequestConfig.TeamsVoiceConfig.Validate()
}
func (p *TeamsVoiceProvider) Notify(_ context.Context, r *NotifyRequest) *NotifyResult {
	if r == nil {
		return &NotifyResult{Err: errors.New("Teams语音请求为空")}
	}
	if e := p.Check(r.Config); e != nil {
		return &NotifyResult{Err: e}
	}
	ids, e := models.EnqueueTeamsVoice(r.DB, r.Config, r.NotifyRuleId, r.DutyGroupID, r.Events, r.Sendtos)
	return &NotifyResult{Target: strings.Join(r.Sendtos, ","), Response: "Teams语音任务: " + strings.Join(ids, ","), Err: e}
}
