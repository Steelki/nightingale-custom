// Package teamsvoice implements service-hosted Teams calls. It never infers an
// acknowledgement of the incident from a connected call or completed prompt.
package teamsvoice

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/ccfos/nightingale/v6/models"
)

type Client struct{ HTTP *http.Client }

func (c Client) request(ctx context.Context, method, u string, body []byte, headers map[string]string) ([]byte, int, error) {
	req, e := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if e != nil {
		return nil, 0, errors.New("请求地址无效")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	h := c.HTTP
	if h == nil {
		h = &http.Client{Timeout: 20e9}
	}
	// 禁止重定向转发Bearer或Speech凭据到另一主机。
	copy := *h
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, e := copy.Do(req)
	if e != nil {
		return nil, 0, errors.New("供应商请求未取得响应，结果未知")
	}
	defer res.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(res.Body, 4*1024*1024+1))
	if e != nil || len(raw) > 4*1024*1024 {
		return nil, res.StatusCode, errors.New("供应商响应无法读取或过大")
	}
	if res.StatusCode/100 != 2 {
		return nil, res.StatusCode, fmt.Errorf("供应商返回HTTP %d", res.StatusCode)
	}
	return raw, res.StatusCode, nil
}
func (c Client) token(ctx context.Context, cfg models.TeamsVoiceConfig) (string, error) {
	secret := os.Getenv(cfg.SecretEnv)
	if secret == "" {
		return "", errors.New("Teams应用密钥环境变量未配置")
	}
	form := url.Values{"client_id": {cfg.AppID}, "client_secret": {secret}, "scope": {"https://graph.microsoft.com/.default"}, "grant_type": {"client_credentials"}}
	raw, _, e := c.request(ctx, "POST", "https://login.microsoftonline.com/"+cfg.TenantID+"/oauth2/v2.0/token", []byte(form.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if e != nil {
		return "", e
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(raw, &out) != nil || out.AccessToken == "" {
		return "", errors.New("Teams未返回应用令牌")
	}
	return out.AccessToken, nil
}
func (c Client) graph(ctx context.Context, cfg models.TeamsVoiceConfig, method, path string, body any) ([]byte, int, error) {
	token, e := c.token(ctx, cfg)
	if e != nil {
		return nil, -1, e
	}
	raw, e := json.Marshal(body)
	if e != nil {
		return nil, 0, e
	}
	return c.request(ctx, method, "https://graph.microsoft.com/v1.0/communications/calls"+path, raw, map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"})
}

// SpeechText produces plain, bounded text; the title is never interpreted as SSML.
func SpeechText(title string) string {
	title = regexp.MustCompile(`</?[a-zA-Z][^>]*>`).ReplaceAllString(title, " ")
	title = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, title)
	title = strings.Join(strings.Fields(title), " ")
	r := []rune(title)
	if len(r) > 160 {
		title = string(r[:160]) + "，标题已截短"
	}
	if title == "" {
		title = "有一条新的告警"
	}
	return "告警提醒：" + title + "。请查看群消息并及时认领。"
}
func (c Client) synthesize(ctx context.Context, cfg models.TeamsVoiceConfig, title string) ([]byte, error) {
	key := os.Getenv(cfg.SpeechKeyEnv)
	if key == "" {
		return nil, errors.New("Azure Speech密钥环境变量未配置")
	}
	var escaped bytes.Buffer
	xml.EscapeText(&escaped, []byte(SpeechText(title)))
	ssml := `<speak version="1.0" xml:lang="zh-CN"><voice name="` + cfg.Voice + `">` + escaped.String() + `</voice></speak>`
	raw, _, e := c.request(ctx, "POST", "https://"+cfg.SpeechRegion+".tts.speech.microsoft.com/cognitiveservices/v1", []byte(ssml), map[string]string{"Ocp-Apim-Subscription-Key": key, "Content-Type": "application/ssml+xml", "X-Microsoft-OutputFormat": "riff-16khz-16bit-mono-pcm", "User-Agent": "nightingale-teams-voice"})
	if e != nil {
		return nil, e
	}
	if len(raw) < 44 || string(raw[:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, errors.New("Azure Speech未返回WAV音频")
	}
	return raw, nil
}
