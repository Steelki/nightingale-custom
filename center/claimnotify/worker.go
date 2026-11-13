package claimnotify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/alert/sender/provider"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/toolkits/pkg/logger"
	"gorm.io/gorm"
)

// Start returns a shutdown function. Delivery results are recorded before it exits.
func Start(db *gorm.DB, siteURL func() string) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := RunOnce(ctx, db, siteURL(), nil); err != nil {
					logger.Errorf("claim notice worker: %v", err)
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}

func RunOnce(ctx context.Context, db *gorm.DB, siteURL string, client *http.Client) error {
	var expired []models.ClaimNotice
	if err := db.Where("status=? AND started_at<?", "sending", time.Now().Add(-2*time.Minute).Unix()).Limit(100).Find(&expired).Error; err != nil {
		return err
	}
	for _, job := range expired {
		// After a crash the remote outcome is unknowable; never redial/repost automatically.
		if err := models.FinishClaimNotice(db, job.ID, "unknown", "发送进程中断，送达结果未知；为避免重复通知，未自动重发"); err != nil {
			return err
		}
	}
	var jobs []models.ClaimNotice
	if err := db.Where("status=?", "pending").Order("id").Limit(20).Find(&jobs).Error; err != nil {
		return err
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return nil
		}
		lock := db.Model(&models.ClaimNotice{}).Where("id=? AND status=?", job.ID, "pending").Updates(map[string]interface{}{"status": "sending", "started_at": time.Now().Unix()})
		if lock.Error != nil {
			return lock.Error
		}
		if lock.RowsAffected != 1 {
			continue
		}
		status, reason := send(ctx, job, siteURL, client)
		if err := models.FinishClaimNotice(db, job.ID, status, reason); err != nil {
			return err
		}
		if status == "failed" && job.GroupID > 0 {
			var row models.EventResponse
			if db.First(&row, job.ResponseID).Error == nil {
				if event, e := models.LoadResponseEvent(db, &row); e == nil {
					if e = models.EnqueueDutyGroupFailure(db, &event, 0, job.GroupID, "认领钉钉通知", job.Claimant, errors.New(reason)); e != nil {
						logger.Errorf("persist claim notification fallback: %v", e)
					}
				}
			}
		}

	}
	return nil
}

func send(ctx context.Context, job models.ClaimNotice, siteURL string, client *http.Client) (string, string) {
	endpoint, err := job.Endpoint()
	if err != nil {
		return "failed", err.Error()
	}
	u, err := url.Parse(siteURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return "failed", "系统站点地址未配置或无效，无法生成告警详情链接"
	}
	u.Path = strings.TrimRight(u.Path, "/") + fmt.Sprintf("/alert-his-events/%d", job.EventID)
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	loc, err := time.LoadLocation(job.Timezone)
	if err != nil {
		loc = time.UTC
	}
	content := fmt.Sprintf("【告警已认领】\n告警：%s\n认领人：%s\n认领时间：%s\n查看告警详情：%s", job.Title, job.Claimant, time.Unix(job.ClaimedAt, 0).In(loc).Format("2006-01-02 15:04:05 MST"), u.String())
	payload := map[string]interface{}{"msgtype": "text", "text": map[string]string{"content": content}}
	channel := &models.NotifyChannelConfig{RequestType: "http", RequestConfig: &models.RequestConfig{HTTPRequestConfig: &models.HTTPRequestConfig{Timeout: 10000, Request: models.RequestDetail{Body: `{{jsonMarshal $tpl.payload}}`}}}}
	p := provider.DutyWebhookProvider{Endpoint: endpoint}
	result := p.Notify(ctx, &provider.NotifyRequest{Config: channel, Events: []*models.AlertCurEvent{{Id: job.EventID}}, TplContent: map[string]interface{}{"payload": payload}, HttpClient: client})
	if result.Err != nil {
		return "failed", result.Err.Error()
	}
	return "sent", "认领通知已发送到钉钉群"
}
