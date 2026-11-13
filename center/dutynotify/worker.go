package dutynotify

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/alert/dispatch"
	"github.com/ccfos/nightingale/v6/alert/sender/provider"
	"github.com/ccfos/nightingale/v6/models"
	nctx "github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/toolkits/pkg/logger"
	"gorm.io/gorm"
)

type Worker struct {
	DB         *gorm.DB
	SiteURL    func() string
	HTTPClient *http.Client
	// Injection point permits isolated tests to exercise outcome handling without sending mail.
	Email func(context.Context, *models.NotifyChannelConfig, []string, map[string]interface{}) error
}

func Start(db *gorm.DB, siteURL func() string) func() {
	if os.Getenv("N9E_DISABLE_DUTY_DELIVERY") == "1" {
		logger.Info("duty delivery disabled for isolated verification")
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	w := Worker{DB: db, SiteURL: siteURL}
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := w.RunOnce(ctx, time.Now()); err != nil {
					logger.Errorf("duty notice worker: %v", err)
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}

// RunOnce 先生成到期提醒，再以条件更新抢占 pending 任务，发送后记录最终状态。
// sending 超过两分钟记为 unknown 而不自动重发：网络超时/进程退出不等于对方没收到。
// 本实现不承诺跨网络的 exactly-once，也没有电话回执与逐级升级执行器。
// 当前unknown策略适用于本worker的单次通知结果，不是完整的语音通话状态机。
// 外接IM呼叫还需关联供应商ID、处理重复/乱序回执，并独立判断升级条件；参见语音设计文档。
func (w *Worker) RunOnce(ctx context.Context, now time.Time) error {
	if err := models.EnqueueDueDutyReminders(w.DB, now); err != nil {
		return err
	}
	var expired []models.DutyNotice
	if e := w.DB.Where("status=? AND started_at<?", "sending", now.Add(-2*time.Minute).Unix()).Limit(100).Find(&expired).Error; e != nil {
		return e
	}
	for _, n := range expired {
		if e := models.FinishDutyNotice(w.DB, n.ID, "unknown", "发送进程中断，结果未知；未自动重发"); e != nil {
			return e
		}
	}
	var jobs []models.DutyNotice
	if e := w.DB.Where("status=?", "pending").Order("id").Limit(30).Find(&jobs).Error; e != nil {
		return e
	}
	for _, n := range jobs {
		if ctx.Err() != nil {
			return nil
		}
		lock := w.DB.Model(&models.DutyNotice{}).Where("id=? AND status=?", n.ID, "pending").Updates(map[string]interface{}{"status": "sending", "started_at": now.Unix()})
		if lock.Error != nil {
			return lock.Error
		}
		if lock.RowsAffected == 0 {
			continue
		}
		status, reason := w.send(ctx, n, now)
		if e := models.FinishDutyNotice(w.DB, n.ID, status, reason); e != nil {
			return e
		}
		if status == "failed" && (n.Kind == "transfer" || n.Kind == "unclaim") {
			p, e := n.Decode()
			if e == nil {
				var latest models.DutyNotice
				if w.DB.First(&latest, n.ID).Error == nil {
					n.Recipient = latest.Recipient
				}
				p.Event.DutyRecipientName = n.Recipient
				if e = models.EnqueueNotificationFailure(w.DB, &p.Event, n.RuleID, fmt.Sprintf("处理通知媒介%d", n.ChannelID), n.Recipient, errors.New(reason)); e != nil {
					logger.Errorf("persist lifecycle fallback: %v", e)
				}
			}
		}
	}
	return nil
}
func detailURL(site string, eventID int64) (string, error) {
	u, err := url.Parse(site)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "", errors.New("系统站点地址无效，无法生成告警详情链接")
	}
	u.Path = strings.TrimRight(u.Path, "/") + fmt.Sprintf("/alert-his-events/%d", eventID)
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}
func eventSummary(e models.AlertCurEvent) string {
	severity := map[int]string{1: "Critical", 2: "Warning", 3: "Info"}[e.Severity]
	if severity == "" {
		severity = fmt.Sprint(e.Severity)
	}
	tags := strings.Join(e.TagsJSON, ", ")
	if tags == "" {
		tags = e.Tags
	}
	annotations := e.Annotations
	if len(e.AnnotationsJSON) > 0 {
		annotations = ""
		for k, v := range e.AnnotationsJSON {
			annotations += k + ": " + v + "\n"
		}
	}
	return fmt.Sprintf("告警：%s\n级别：%s\n对象：%s\n触发时间：%s\n触发值：%s\n标签：%s\n说明：%s\n%s", e.RuleName, severity, e.TargetIdent, time.Unix(e.TriggerTime, 0).Format(time.RFC3339), e.TriggerValue, tags, e.RuleNote, annotations)
}
func (w *Worker) send(ctx context.Context, n models.DutyNotice, now time.Time) (string, string) {
	p, err := n.Decode()
	if err != nil {
		return "failed", "通知配置解密失败"
	}
	if n.Kind == "duty_reminder" {
		return w.reminder(ctx, n, p, now)
	}
	link, err := detailURL(w.SiteURL(), p.Event.Id)
	if err != nil {
		return "failed", err.Error()
	}
	if n.Kind == "fallback" {
		current, e := models.EffectiveDutyFallback(w.DB, n.GroupID)
		if e != nil {
			return "skipped", "兜底配置已不可用，取消旧通知"
		}
		if current.Kind != p.Fallback.Kind || current.Target != p.Fallback.Target || current.Secret != p.Fallback.Secret || current.SMTPChannelID != p.Fallback.SMTPChannelID {
			return "skipped", "失败兜底配置已修改，取消旧地址通知"
		}
		text := fmt.Sprintf("【告警通知发送失败】\n%s\n原接收人：%s\n失败渠道：%s\n失败时间：%s\n失败原因：%s\n查看告警详情：%s", eventSummary(p.Event), n.Recipient, p.FailureChannel, time.Unix(p.FailedAt, 0).Format(time.RFC3339), p.FailureReason, link)
		if p.Fallback.Kind == "email" {
			channel, e := w.emailChannel(p.Fallback.SMTPChannelID)
			if e != nil {
				return "failed", e.Error()
			}
			if e = w.email(ctx, channel, []string{p.Fallback.Target}, "告警通知发送失败："+p.Event.RuleName, text); e != nil {
				return "failed", e.Error()
			}
		} else {
			if e := w.webhook(ctx, models.DutyEndpoint{Kind: p.Fallback.Kind, URL: p.Fallback.Target, Secret: p.Fallback.Secret}, text, nil, &p.Event); e != nil {
				return "failed", e.Error()
			}
		}
		return "sent", "已发送原告警及失败原因到备用通知渠道"
	}
	return w.lifecycle(ctx, n, p, link)
}
func (w *Worker) emailChannel(id int64) (*models.NotifyChannelConfig, error) {
	var channels []models.NotifyChannelConfig
	q := w.DB.Where("request_type=? AND enable=?", "smtp", true)
	if id > 0 {
		q = q.Where("id=?", id)
	}
	if e := q.Limit(2).Find(&channels).Error; e != nil {
		return nil, e
	}
	if len(channels) == 0 {
		return nil, errors.New("未找到启用的邮件媒介，请先配置SMTP")
	}
	if len(channels) > 1 {
		return nil, errors.New("存在多个邮件媒介，请在兜底配置中指定一个")
	}
	return &channels[0], nil
}
func (w *Worker) email(ctx context.Context, c *models.NotifyChannelConfig, to []string, title, text string) error {
	f := w.Email
	if f == nil {
		f = provider.SendDutyEmail
	}
	return f(ctx, c, to, map[string]interface{}{"subject": title, "content": "<pre>" + html.EscapeString(text) + "</pre>"})
}
func (w *Worker) webhook(ctx context.Context, endpoint models.DutyEndpoint, text string, phones []string, event *models.AlertCurEvent) error {
	if endpoint.Kind != "dingtalk" && endpoint.Kind != "feishu" && endpoint.Kind != "webhook" {
		return errors.New("备用或公共通知渠道类型无效")
	}
	var payload map[string]interface{}
	switch endpoint.Kind {
	case "dingtalk":
		payload = map[string]interface{}{"msgtype": "text", "text": map[string]string{"content": text}, "at": map[string]interface{}{"atMobiles": phones, "isAtAll": false}}
	case "feishu":
		payload = map[string]interface{}{"msg_type": "text", "content": map[string]string{"text": text}}
	default:
		payload = map[string]interface{}{"text": text, "event": event}
	}
	config := &models.NotifyChannelConfig{RequestType: "http", RequestConfig: &models.RequestConfig{HTTPRequestConfig: &models.HTTPRequestConfig{Timeout: 10000, Request: models.RequestDetail{Body: `{{jsonMarshal $tpl.payload}}`}}}}
	req := &provider.NotifyRequest{Config: config, Events: []*models.AlertCurEvent{event}, TplContent: map[string]interface{}{"payload": payload}, HttpClient: w.HTTPClient}
	result := (&provider.DutyWebhookProvider{Endpoint: endpoint}).Notify(ctx, req)
	if result == nil {
		return errors.New("通知渠道未返回结果")
	}
	return result.Err
}
func (w *Worker) reminder(ctx context.Context, n models.DutyNotice, p models.DutyNoticePayload, now time.Time) (string, string) {
	var g models.DutyGroup
	if e := w.DB.First(&g, n.GroupID).Error; e != nil {
		return "skipped", "值班组已不存在"
	}
	if !g.Config.HourlyReminder {
		return "skipped", "值班提醒已关闭"
	}
	if now.Unix() >= n.ExpiresAt {
		return "skipped", "提醒已过期，不补发历史提醒"
	}
	private, e := g.PrivateConfig()
	if e != nil {
		return "failed", "公共渠道解密失败"
	}
	valid := false
	for _, ep := range private.Endpoints {
		if ep.Kind == p.Endpoint.Kind && ep.URL == p.Endpoint.URL && ep.Secret == p.Endpoint.Secret {
			valid = true
		}
	}
	if !valid {
		return "skipped", "组公共通知渠道已修改，取消旧地址提醒"
	}
	resolution, e := models.ResolveDuty(w.DB, g, now)
	if e != nil || !resolution.Available {
		return "failed", "当前排班无人可用，请检查值班账号和排班"
	}
	var user models.User
	if e = w.DB.First(&user, resolution.UserID).Error; e != nil || user.IsDisabled() {
		return "failed", "当前值班账号不可用"
	}
	phone := user.Phone
	if phone == "" {
		phone = "未填写，请补充联系方式"
	}
	loc, _ := time.LoadLocation(g.Config.Timezone)
	text := fmt.Sprintf("【当前值班人员】\n值班组：%s\n本次值班人员：%s\n联系电话：%s\n时间：%s", g.Name, models.UserDisplay(user), phone, now.In(loc).Format("2006-01-02 15:04:05 MST"))
	w.DB.Model(&models.DutyNotice{}).Where("id=?", n.ID).Update("recipient", models.UserDisplay(user))
	if e = w.webhook(ctx, p.Endpoint, text, nil, nil); e != nil {
		return "failed", e.Error()
	}
	return "sent", "当前值班人员及联系电话已发送到" + p.Endpoint.Kind
}

// lifecycle 发送转交/取消认领通知前复查归属版本、恢复状态及当前通知规则。
// 已过时任务跳过；这里使用显式接收人构建上下文，不能走会拦截已认领事件的普通提醒入口。
func (w *Worker) lifecycle(ctx context.Context, n models.DutyNotice, p models.DutyNoticePayload, link string) (string, string) {
	var row models.EventResponse
	if e := w.DB.First(&row, n.ResponseID).Error; e != nil {
		return "skipped", "处理周期已不存在"
	}
	if row.Recovered || row.AssignmentVersion != n.AssignmentVersion {
		return "skipped", "告警已恢复或负责人已变更，取消过时通知"
	}
	var rule models.NotifyRule
	if e := w.DB.First(&rule, n.RuleID).Error; e != nil || !rule.Enable || rule.ReceiverMode != "duty_group" || rule.DutyGroupID != n.GroupID {
		return "skipped", "通知规则已停用或接收方式已变更"
	}
	if n.ConfigIndex >= len(rule.NotifyConfigs) || rule.NotifyConfigs[n.ConfigIndex].ChannelID != n.ChannelID {
		return "skipped", "通知渠道配置已变更"
	}
	cfg := rule.NotifyConfigs[n.ConfigIndex]
	if e := dispatch.NotifyRuleMatchCheck(&cfg, &p.Event); e != nil {
		return "skipped", "告警不匹配该渠道的级别、标签或通知时段"
	}
	var channel models.NotifyChannelConfig
	if e := w.DB.First(&channel, n.ChannelID).Error; e != nil || !channel.Enable {
		return "skipped", "通知媒介已停用或不存在"
	}
	var group models.DutyGroup
	if e := w.DB.First(&group, n.GroupID).Error; e != nil {
		return "failed", "值班组不存在"
	}
	explicit := int64(0)
	if n.Kind == "transfer" {
		explicit = row.OwnerID
	} else if row.OwnerID != 0 {
		return "skipped", "告警已重新认领"
	}
	user, e := models.ResolveDutyRecipient(w.DB, group, &p.Event, explicit)
	if e != nil {
		return "failed", e.Error()
	}
	title := "告警已转交"
	if n.Kind == "unclaim" {
		title = "告警已取消认领，请当前值班人接手"
	}
	recipient := models.UserDisplay(user)
	w.DB.Model(&models.DutyNotice{}).Where("id=?", n.ID).Update("recipient", recipient)
	text := fmt.Sprintf("【%s】\n操作人：%s\n接收人：%s\n%s\n查看告警详情：%s", title, p.Actor, recipient, eventSummary(p.Event), link)
	kind, e := models.DutyChannelKind(&channel)
	if e != nil {
		return "failed", e.Error()
	}
	if kind != "" {
		c, e := group.PrivateConfig()
		if e != nil {
			return "failed", "公共渠道解密失败"
		}
		for _, endpoint := range c.Endpoints {
			if endpoint.Kind == kind && endpoint.URL != "" {
				if e = w.webhook(ctx, endpoint, text, []string{user.Phone}, &p.Event); e != nil {
					return "failed", e.Error()
				}
				return "sent", title + "通知已发送给" + recipient
			}
		}
		return "failed", "值班组公共渠道未配置"
	}
	if channel.RequestType == "smtp" {
		if user.Email == "" {
			return "failed", "接收人缺少邮箱"
		}
		if e = w.email(ctx, &channel, []string{user.Email}, title+"："+p.Event.RuleName, text); e != nil {
			return "failed", e.Error()
		}
		return "sent", title + "邮件已发送给" + recipient
	}
	// Existing phone/user-info providers continue to use their configured templates.
	event := p.Event
	event.RuleName = "【" + title + "，接收人：" + recipient + "】" + event.RuleName
	event.RuleNote = text
	var tpl models.MessageTemplate
	if e = w.DB.First(&tpl, cfg.TemplateID).Error; e != nil {
		return "failed", "通知模板不存在"
	}
	cfg.DutyGroupID = group.ID
	cfg.DutyUserID = user.Id
	nc, e := dispatch.BuildNotifyContext(nctx.NewContext(ctx, w.DB, true), nil, nil, []*models.AlertCurEvent{&event}, rule.ID, &cfg, &channel, tpl.RenderEventForChannel(channel.RequestType, []*models.AlertCurEvent{&event}, w.SiteURL()), w.HTTPClient, w.SiteURL())
	if e != nil {
		return "failed", e.Error()
	}
	result := nc.Provider.Notify(ctx, nc.Request)
	if result == nil {
		return "failed", "通知媒介未返回结果"
	}
	if result.Err != nil {
		return "failed", models.SafeNoticeText(result.Err.Error())
	}
	return "sent", title + "通知已发送给" + recipient
}
