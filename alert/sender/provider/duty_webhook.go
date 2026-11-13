package provider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ccfos/nightingale/v6/models"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// DutyWebhookProvider keeps encrypted group destinations out of generic URL logs.
// It reuses the configured message body/template, but owns destination and signing.
type DutyWebhookProvider struct{ Endpoint models.DutyEndpoint }

func (p *DutyWebhookProvider) Ident() string { return "duty_webhook" }
func (p *DutyWebhookProvider) Check(c *models.NotifyChannelConfig) error {
	return c.ValidateHTTPRequestConfig()
}
func (p *DutyWebhookProvider) Notify(ctx context.Context, req *NotifyRequest) *NotifyResult {
	result := &NotifyResult{Target: "值班组公共" + p.Endpoint.Kind}
	fail := func(err error) *NotifyResult { result.Err = err; return result }
	if req.Config.RequestConfig == nil || req.Config.RequestConfig.HTTPRequestConfig == nil {
		return fail(errors.New("公共渠道缺少HTTP配置"))
	}
	cfg := req.Config.RequestConfig.HTTPRequestConfig
	body, err := parseRequestBody(cfg, buildNotifyTplData(req.Events, req.TplContent, req.CustomParams, req.Sendtos))
	if err != nil {
		return fail(errors.New("公共渠道消息模板渲染失败"))
	}
	target, err := url.Parse(p.Endpoint.URL)
	if err != nil {
		return fail(errors.New("公共渠道地址无效"))
	}
	if p.Endpoint.Secret != "" {
		if p.Endpoint.Kind == "dingtalk" {
			timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
			mac := hmac.New(sha256.New, []byte(p.Endpoint.Secret))
			mac.Write([]byte(timestamp + "\n" + p.Endpoint.Secret))
			query := target.Query()
			query.Set("timestamp", timestamp)
			query.Set("sign", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
			target.RawQuery = query.Encode()
		} else if p.Endpoint.Kind == "feishu" {
			timestamp := strconv.FormatInt(time.Now().Unix(), 10)
			mac := hmac.New(sha256.New, []byte(timestamp+"\n"+p.Endpoint.Secret))
			var payload map[string]interface{}
			if json.Unmarshal(body, &payload) != nil || payload == nil {
				return fail(errors.New("飞书消息须为JSON对象"))
			}
			payload["timestamp"] = timestamp
			payload["sign"] = base64.StdEncoding.EncodeToString(mac.Sum(nil))
			body, err = json.Marshal(payload)
			if err != nil {
				return fail(errors.New("飞书签名消息编码失败"))
			}
		}
	}
	timeout := time.Duration(cfg.Timeout) * time.Millisecond
	if timeout <= 0 || timeout > 60*time.Second {
		timeout = 10 * time.Second
	}
	sendCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(sendCtx, "POST", target.String(), bytes.NewReader(body))
	if err != nil {
		return fail(errors.New("公共渠道请求构建失败"))
	}
	request.Header.Set("Content-Type", "application/json")
	if p.Endpoint.Kind == "webhook" && p.Endpoint.Secret != "" {
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		mac := hmac.New(sha256.New, []byte(p.Endpoint.Secret))
		mac.Write([]byte(timestamp + "."))
		mac.Write(body)
		request.Header.Set("X-N9E-Timestamp", timestamp)
		request.Header.Set("X-N9E-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	}
	client := &http.Client{Timeout: timeout}
	if req.HttpClient != nil {
		*client = *req.HttpClient
	}
	// Never forward robot credentials to a redirect target.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return fail(errors.New("公共渠道网络请求失败或超时"))
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024))
	if err != nil {
		return fail(errors.New("读取公共渠道响应失败"))
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fail(fmt.Errorf("公共渠道返回HTTP %d", response.StatusCode))
	}
	if p.Endpoint.Kind == "dingtalk" || p.Endpoint.Kind == "feishu" {
		var reply map[string]json.RawMessage
		if json.Unmarshal(raw, &reply) != nil {
			return fail(errors.New("公共渠道返回无效JSON"))
		}
		keys := []string{"errcode"}
		if p.Endpoint.Kind == "feishu" {
			keys = []string{"code", "StatusCode"}
		}
		found := false
		for _, key := range keys {
			if value, ok := reply[key]; ok {
				var code int
				if json.Unmarshal(value, &code) != nil {
					return fail(errors.New("公共渠道返回无效状态码"))
				}
				found = true
				if code != 0 {
					return fail(fmt.Errorf("公共渠道供应商错误码 %d", code))
				}
			}
		}
		if !found {
			return fail(errors.New("公共渠道响应缺少业务状态码"))
		}
	}
	result.Response = "发送成功"
	return result
}
