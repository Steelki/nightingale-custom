package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ccfos/nightingale/v6/alert/sender/provider"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
)

// prepareDutyContext 是值班模式接入原生通知链路的适配层。
// 当班人/恢复负责人由模型解析；公共 IM 地址来自组配置，个人渠道使用用户联系方式。
// 返回发送专用副本，不把动态接收人写回共享的规则/渠道缓存；此函数只组装，不发送。
func prepareDutyContext(c *ctx.Context, groupID int64, channel *models.NotifyChannelConfig, config *models.NotifyConfig, events ...*models.AlertCurEvent) (*models.NotifyChannelConfig, *models.NotifyConfig, []string, provider.NotifyChannelProvider, error) {
	if c == nil || c.DB == nil {
		return nil, nil, nil, nil, errors.New("值班接收须由连接中心数据库的通知节点执行")
	}
	db := models.DB(c)
	var group models.DutyGroup
	if err := db.First(&group, groupID).Error; err != nil {
		return nil, nil, nil, nil, errors.New("值班组不可用")
	}
	var event *models.AlertCurEvent
	if len(events) > 0 {
		event = events[0]
	}
	user, err := models.ResolveDutyRecipient(db, group, event, config.DutyUserID)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("通知接收人员不可用：%w", err)
	}
	for _, evt := range events {
		evt.DutyRecipientName = models.UserDisplay(user)
	}
	kind, err := models.DutyChannelKind(channel)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	raw, err := json.Marshal(channel)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	var channelCopy models.NotifyChannelConfig
	if err = json.Unmarshal(raw, &channelCopy); err != nil {
		return nil, nil, nil, nil, err
	}
	configCopy := *config
	configCopy.Params = map[string]interface{}{}
	// Retain only non-recipient custom parameters. Group endpoints have their own destinations.
	for key, value := range config.Params {
		switch key {
		case "user_ids", "user_group_ids", "im_group_ids", "ids", "pagerduty_integration_keys", "pagerduty_integration_ids", "access_token", "callback_url", "bot_name":
			continue
		}
		configCopy.Params[key] = value
	}
	sendtos := []string{}
	var chosen provider.NotifyChannelProvider
	if kind != "" {
		private, err := group.PrivateConfig()
		if err != nil {
			return nil, nil, nil, nil, errors.New("公共通知配置无法解密")
		}
		for _, endpoint := range private.Endpoints {
			if endpoint.Kind == kind && endpoint.URL != "" {
				chosen = &provider.DutyWebhookProvider{Endpoint: endpoint}
				break
			}
		}
		if chosen == nil {
			return nil, nil, nil, nil, errors.New("值班组公共渠道未配置")
		}
		if kind == "dingtalk" {
			if user.Phone == "" {
				return nil, nil, nil, nil, fmt.Errorf("值班人员%s缺少手机号，无法@", user.Username)
			}
			sendtos = []string{user.Phone}
		}
	} else {
		contact := ""
		if channel.ParamConfig.UserInfo.ContactKey == "email" {
			contact = user.Email
		} else {
			contact = user.Phone
		}
		if contact == "" {
			return nil, nil, nil, nil, fmt.Errorf("值班人员%s缺少%s联系方式", user.Username, channel.ParamConfig.UserInfo.ContactKey)
		}
		sendtos = []string{contact}
	}
	return &channelCopy, &configCopy, sendtos, chosen, nil
}
