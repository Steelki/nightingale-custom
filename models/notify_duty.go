package models

import (
	"errors"
	"fmt"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
)

// DutyChannelKind returns the shared endpoint required by a supported channel.
// Teams语音复用email取得夜莺账号，再由专用provider按媒介租户映射Entra用户。
// email/phone字段本身不表示支持语音；飞书open_id等仍需独立适配。
func DutyChannelKind(channel *NotifyChannelConfig) (string, error) {
	if channel.RequestType == "http" {
		switch channel.Ident {
		case Dingtalk:
			return "dingtalk", nil
		case Feishu, FeishuCard:
			return "feishu", nil
		case "callback":
			return "webhook", nil
		}
	}
	if channel.ParamConfig != nil && channel.ParamConfig.UserInfo != nil {
		key := channel.ParamConfig.UserInfo.ContactKey
		if key == "email" || key == "phone" {
			return "", nil
		}
	}
	return "", errors.New("该媒介暂不支持值班组接收，请使用邮件、电话、钉钉、飞书或Webhook媒介")
}

// VerifyDuty保证值班模式只有一个接收来源：校验组/渠道并清除旧人员、团队、机器人参数。
// 新建和修改都须经过该校验，不能只在前端隐藏字段。
func (r *NotifyRule) VerifyDuty(c *ctx.Context) error {
	if r.ReceiverMode == "" {
		r.ReceiverMode = "original"
	}
	if r.ReceiverMode == "original" {
		r.DutyGroupID = 0
		return nil
	}
	if r.ReceiverMode != "duty_group" || r.DutyGroupID <= 0 {
		return errors.New("请选择有效的通知接收方式和值班组")
	}
	var group DutyGroup
	if err := DB(c).First(&group, r.DutyGroupID).Error; err != nil {
		return errors.New("值班组不存在或不可用")
	}
	for i := range r.NotifyConfigs {
		config := &r.NotifyConfigs[i]
		var channel NotifyChannelConfig
		if err := DB(c).First(&channel, config.ChannelID).Error; err != nil {
			return errors.New("通知媒介不存在")
		}
		kind, err := DutyChannelKind(&channel)
		if err != nil {
			return err
		}
		if kind != "" {
			found := false
			for _, endpoint := range group.Config.Endpoints {
				if endpoint.Kind == kind && endpoint.Configured {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("值班组尚未配置%s公共渠道", kind)
			}
		}
		// Duty mode has a single source of recipients and shared destinations.
		for _, key := range []string{"user_ids", "user_group_ids", "im_group_ids", "ids", "pagerduty_integration_keys", "pagerduty_integration_ids", "access_token", "callback_url", "bot_name"} {
			delete(config.Params, key)
		}
	}
	return nil
}
