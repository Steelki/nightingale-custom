package provider

import (
	"context"
	"net/http"

	"github.com/ccfos/nightingale/v6/models"
)

// NotifyChannelProvider 通知媒介提供者接口
// 单次发送媒介通过此接口接入并注册；有异步回执的语音媒介还需持久任务、回调认证及状态机。
// 外接IM语音的设计见仓库根目录 VOICE_INTEGRATION_DESIGN_zh.md，尚未实现该扩展。
type NotifyChannelProvider interface {
	// Ident 返回媒介标识，与 notify_channel 表的 ident 字段对应
	// 如 "dingtalk", "wecom", "email", "flashduty", "http"
	Ident() string

	// Check 校验通知媒介配置是否合法 (保存前调用)
	Check(config *models.NotifyChannelConfig) error

	// Notify 发送通知
	Notify(ctx context.Context, req *NotifyRequest) *NotifyResult
}

type NotifyRequest struct {
	NotifyRuleId         int64
	Config               *models.NotifyChannelConfig
	Events               []*models.AlertCurEvent
	TplContent           map[string]interface{}
	FlashDutyChannelIDs  []int64
	PagerDutyRoutingKeys []string
	CustomParams         map[string]string
	Sendtos              []string
	ImGroupIDs           []string                  // 飞书群/钉钉群ID
	ImGroupRobotCodes    map[string]string         // 钉钉群 openConversationId -> robotCode, 由 BuildNotifyContext 预取
	HttpClient           *http.Client              // 由 cache 层提供
	SmtpChan             chan *models.EmailContext // 由 cache 层提供 (仅 smtp 类型)
	SiteUrl              string
}

// NotifyResult 描述本次接口调用结果；Err == nil不能统一解释为已送达、已接听或已播放。
// 未来语音结果需保存平台呼叫ID并等待认证回执，不能仅靠Response文本判断最终通话状态。
type NotifyResult struct {
	Target   string // 发送目标 (用于 NotifyRecord)
	Response string // 响应内容
	Err      error
}
