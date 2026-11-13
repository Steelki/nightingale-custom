package provider

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"gopkg.in/gomail.v2"
)

// Bounded synchronous delivery: callers record the final outcome, never "queued" as success.
func SendDutyEmail(ctx context.Context, channel *models.NotifyChannelConfig, to []string, tpl map[string]interface{}) error {
	if channel == nil || channel.RequestConfig == nil || channel.RequestConfig.SMTPRequestConfig == nil {
		return errors.New("邮件媒介配置缺失")
	}
	conf := channel.RequestConfig.SMTPRequestConfig
	if conf.Host == "" || conf.Port < 1 || conf.Port > 65535 || len(to) == 0 {
		return errors.New("邮件服务器或收件人配置无效")
	}
	from, err := mail.ParseAddress(conf.From)
	if err != nil {
		return errors.New("邮件发件地址无效")
	}
	for _, addr := range to {
		if _, e := mail.ParseAddress(addr); e != nil {
			return errors.New("邮件收件地址无效")
		}
	}
	sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var conn net.Conn
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	tlsConfig := &tls.Config{ServerName: conf.Host, MinVersion: tls.VersionTLS12, InsecureSkipVerify: conf.InsecureSkipVerify}
	address := net.JoinHostPort(conf.Host, strconv.Itoa(conf.Port))
	if conf.Port == 465 {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(sendCtx, "tcp", address)
	} else {
		conn, err = dialer.DialContext(sendCtx, "tcp", address)
	}
	if err != nil {
		return fmt.Errorf("连接邮件服务器失败：%s", models.SafeNoticeText(err.Error()))
	}
	defer conn.Close()
	deadline, _ := sendCtx.Deadline()
	conn.SetDeadline(deadline)
	stop := context.AfterFunc(sendCtx, func() { conn.Close() })
	defer stop()
	client, err := smtp.NewClient(conn, conf.Host)
	if err != nil {
		return fmt.Errorf("邮件服务器握手失败：%s", models.SafeNoticeText(err.Error()))
	}
	defer client.Close()
	if conf.Port != 465 {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err = client.StartTLS(tlsConfig); err != nil {
				return errors.New("邮件TLS协商失败")
			}
		}
	}
	if conf.Username != "" {
		if err = client.Auth(smtp.PlainAuth("", conf.Username, conf.Password, conf.Host)); err != nil {
			return errors.New("邮件服务器认证失败，请检查账号和凭据")
		}
	}
	if err = client.Mail(from.Address); err != nil {
		return fmt.Errorf("邮件服务器拒绝发件人：%s", models.SafeNoticeText(err.Error()))
	}
	for _, addr := range to {
		a, _ := mail.ParseAddress(addr)
		if err = client.Rcpt(a.Address); err != nil {
			return fmt.Errorf("邮件服务器拒绝收件人：%s", models.SafeNoticeText(err.Error()))
		}
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("邮件服务器拒绝消息：%s", models.SafeNoticeText(err.Error()))
	}
	m := gomail.NewMessage()
	m.SetHeader("From", conf.From)
	m.SetHeader("To", to...)
	m.SetHeader("Subject", getMapString(tpl, "subject"))
	m.SetBody("text/html", getMapString(tpl, "content"))
	if _, err = m.WriteTo(w); err != nil {
		return fmt.Errorf("发送邮件内容失败：%s", models.SafeNoticeText(err.Error()))
	}
	if err = w.Close(); err != nil {
		return fmt.Errorf("邮件提交失败：%s", models.SafeNoticeText(err.Error()))
	}
	// A successful DATA response is authoritative; a failed QUIT must not cause duplicate mail.
	_ = client.Quit()
	return nil
}
