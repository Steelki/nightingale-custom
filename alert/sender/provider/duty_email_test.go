package provider

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ccfos/nightingale/v6/models"
)

func TestDutyEmailReportsSMTPAcceptanceAndRejection(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(fmt.Sprint(reject), func(t *testing.T) {
			listener, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer listener.Close()
			done := make(chan string, 1)
			go func() {
				c, e := listener.Accept()
				if e != nil {
					done <- ""
					return
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				io.WriteString(c, "220 localhost ESMTP\r\n")
				reader := bufio.NewReader(c)
				var data strings.Builder
				for {
					line, e := reader.ReadString('\n')
					if e != nil {
						break
					}
					switch {
					case strings.HasPrefix(line, "EHLO"):
						io.WriteString(c, "250 localhost\r\n")
					case strings.HasPrefix(line, "MAIL"):
						io.WriteString(c, "250 OK\r\n")
					case strings.HasPrefix(line, "RCPT"):
						if reject {
							io.WriteString(c, "550 recipient blocked\r\n")
						} else {
							io.WriteString(c, "250 OK\r\n")
						}
					case strings.HasPrefix(line, "DATA"):
						io.WriteString(c, "354 end with dot\r\n")
						for {
							l, e := reader.ReadString('\n')
							if e != nil || l == ".\r\n" {
								break
							}
							data.WriteString(l)
						}
						io.WriteString(c, "250 accepted\r\n")
					case strings.HasPrefix(line, "QUIT"):
						io.WriteString(c, "221 bye\r\n")
						done <- data.String()
						return
					default:
						io.WriteString(c, "250 OK\r\n")
					}
				}
				done <- data.String()
			}()
			addr := listener.Addr().(*net.TCPAddr)
			channel := &models.NotifyChannelConfig{RequestConfig: &models.RequestConfig{SMTPRequestConfig: &models.SMTPRequestConfig{Host: "127.0.0.1", Port: addr.Port, From: "sender@example.invalid"}}}
			e = SendDutyEmail(context.Background(), channel, []string{"receiver@example.invalid"}, map[string]interface{}{"subject": "failure report", "content": "original alert and reason"})
			if reject && e == nil {
				t.Fatal("SMTP rejection treated as success")
			}
			if !reject && e != nil {
				t.Fatal(e)
			}
			body := <-done
			if !reject && (!strings.Contains(body, "receiver@example.invalid") || !strings.Contains(body, "original alert and reason")) {
				t.Fatalf("body %q", body)
			}
		})
	}
}
func TestDutyEmailContextStopsStalledSMTP(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		c, e := listener.Accept()
		if e == nil {
			defer c.Close()
			io.Copy(io.Discard, c)
		}
		close(done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	now := time.Now()
	channel := &models.NotifyChannelConfig{RequestConfig: &models.RequestConfig{SMTPRequestConfig: &models.SMTPRequestConfig{Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, From: "sender@example.invalid"}}}
	e = SendDutyEmail(ctx, channel, []string{"receiver@example.invalid"}, map[string]interface{}{})
	if e == nil || time.Since(now) > 2*time.Second {
		t.Fatalf("stalled SMTP not bounded: %v", e)
	}
	<-done
}
