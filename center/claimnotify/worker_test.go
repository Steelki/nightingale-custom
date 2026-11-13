package claimnotify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type mockTransport func(*http.Request) (*http.Response, error)

func (f mockTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixture(t *testing.T, linked bool) (*gorm.DB, *models.EventResponse, *models.User) {
	t.Helper()
	dir := t.TempDir()
	db, err := gorm.Open(sqlite.Open(filepath.Join(dir, "test.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	for _, query := range []string{
		"CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, nickname TEXT, disabled INTEGER)",
		"CREATE TABLE alert_cur_event (id INTEGER PRIMARY KEY, hash TEXT, first_trigger_time INTEGER, rule_name TEXT, notify_rule_ids TEXT)",
	} {
		if err = db.Exec(query).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err = models.InitDutySchema(db, filepath.Join(dir, "key")); err != nil {
		t.Fatal(err)
	}
	if err = db.AutoMigrate(&models.NotifyRule{}); err != nil {
		t.Fatal(err)
	}
	actor := &models.User{Id: 1, Username: "zhangsan", Nickname: "张三"}
	if err = db.Exec("INSERT INTO users VALUES (1,'zhangsan','张三',0)").Error; err != nil {
		t.Fatal(err)
	}
	group := models.DutyGroup{Name: "测试值班组", Config: models.DutyConfig{MemberIDs: []int64{1}, Timezone: "Asia/Shanghai", Plans: []models.DutyPlan{{EffectiveDate: "2026-11-01", RotationDays: 1, UserIDs: []int64{1}}}, Endpoints: []models.DutyEndpoint{{Kind: "dingtalk", URL: "https://oapi.dingtalk.com/robot/send?access_token=private-token", Secret: "private-signing-key"}}, Fallback: models.DutyFallback{Kind: "inherit"}}}
	if err = models.SaveDutyGroup(db, &group, "test"); err != nil {
		t.Fatal(err)
	}
	rule := models.NotifyRule{Name: "test", Enable: true, ReceiverMode: "duty_group", DutyGroupID: group.ID}
	if err = db.Create(&rule).Error; err != nil {
		t.Fatal(err)
	}
	rule2 := rule
	rule2.ID = 0
	rule2.Name = "same group"
	if err = db.Create(&rule2).Error; err != nil {
		t.Fatal(err)
	}
	ids := "[]"
	if linked {
		b, _ := json.Marshal([]int64{rule.ID, rule2.ID})
		ids = string(b)
	}
	if err = db.Exec("INSERT INTO alert_cur_event VALUES (1,?,100,?,?)", "hash", "CPU > 90% {{ not_a_template }}", ids).Error; err != nil {
		t.Fatal(err)
	}
	event := models.AlertCurEvent{Hash: "hash", FirstTriggerTime: 100}
	if err = models.ObserveEventResponse(db, &event); err != nil {
		t.Fatal(err)
	}
	var row models.EventResponse
	if err = db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	return db, &row, actor
}

func TestClaimNoticeSuccessAndDedup(t *testing.T) {
	db, row, actor := fixture(t, true)
	if err := models.ChangeEventResponse(db, row, row.Version, actor, "claim", "", 0); err != nil {
		t.Fatal(err)
	}
	var jobs []models.ClaimNotice
	db.Find(&jobs)
	if len(jobs) != 1 {
		t.Fatalf("want one job for shared group, got %d", len(jobs))
	}
	if strings.Contains(jobs[0].Destination, "private-") {
		t.Fatal("plaintext credentials persisted")
	}
	if err := models.ChangeEventResponse(db, row, row.Version, actor, "claim", "", 0); err == nil {
		t.Fatal("duplicate claim accepted")
	}
	calls := 0
	client := &http.Client{Transport: mockTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Query().Get("sign") == "" || r.URL.Query().Get("timestamp") == "" {
			t.Error("missing signature")
		}
		var body struct {
			Msgtype string `json:"msgtype"`
			Text    struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{"告警已认领", "张三（zhangsan）", "CPU > 90% {{ not_a_template }}", "认领时间", "https://example.test/n9e/alert-his-events/1"} {
			if !strings.Contains(body.Text.Content, text) {
				t.Errorf("message missing %q: %s", text, body.Text.Content)
			}
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"errcode":0}`)), Header: make(http.Header)}, nil
	})}
	for i := 0; i < 2; i++ {
		if err := RunOnce(context.Background(), db, "https://example.test/n9e", client); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("sent %d times", calls)
	}
	var job models.ClaimNotice
	db.First(&job)
	if job.Status != "sent" {
		t.Fatalf("%s %s", job.Status, job.Reason)
	}
}

func TestClaimNoticeFailureDoesNotUndoClaim(t *testing.T) {
	db, row, actor := fixture(t, true)
	if err := models.ChangeEventResponse(db, row, row.Version, actor, "claim", "", 0); err != nil {
		t.Fatal(err)
	}
	calls := 0
	client := &http.Client{Transport: mockTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"errcode":310000,"errmsg":"private-token private-signing-key"}`))}, nil
	})}
	if err := RunOnce(context.Background(), db, "https://example.test", client); err != nil {
		t.Fatal(err)
	}
	var job models.ClaimNotice
	db.First(&job)
	db.First(row, row.ID)
	if row.OwnerID != actor.Id || job.Status != "failed" {
		t.Fatalf("claim undone or failure missing: %+v %s", row, job.Status)
	}
	if strings.Contains(job.Reason, "private-") || !strings.Contains(job.Reason, "310000") {
		t.Fatal("unsafe or missing failure reason")
	}
	if err := RunOnce(context.Background(), db, "https://example.test", client); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("failure automatically retried")
	}
}
func TestUnlinkedDemoClaimDoesNotSend(t *testing.T) {
	db, row, actor := fixture(t, false)
	if err := models.ChangeEventResponse(db, row, row.Version, actor, "claim", "", 0); err != nil {
		t.Fatal(err)
	}
	var count int64
	db.Model(&models.ClaimNotice{}).Count(&count)
	if count != 0 {
		t.Fatal("unlinked event queued")
	}
	var logs []models.EventResponseLog
	db.Where("action=?", "claim_notice_skipped").Find(&logs)
	if len(logs) != 1 {
		t.Fatal("skip explanation missing")
	}
}
func TestInterruptedDeliveryIsNotReposted(t *testing.T) {
	db, row, actor := fixture(t, true)
	if err := models.ChangeEventResponse(db, row, row.Version, actor, "claim", "", 0); err != nil {
		t.Fatal(err)
	}
	db.Model(&models.ClaimNotice{}).Where("status=?", "pending").Updates(map[string]interface{}{"status": "sending", "started_at": time.Now().Add(-3 * time.Minute).Unix()})
	client := &http.Client{Transport: mockTransport(func(r *http.Request) (*http.Response, error) { t.Fatal("unexpected repost"); return nil, nil })}
	if err := RunOnce(context.Background(), db, "https://example.test", client); err != nil {
		t.Fatal(err)
	}
	var job models.ClaimNotice
	db.First(&job)
	if job.Status != "unknown" {
		t.Fatal(job.Status)
	}
}

func TestClaimAndNoticeRollBackTogether(t *testing.T) {
	db, row, actor := fixture(t, true)
	// Simulate storage failure after the ownership update; no half-committed claim/log/job.
	if err := db.Exec("CREATE TRIGGER reject_claim_notice BEFORE INSERT ON response_claim_notice BEGIN SELECT RAISE(ABORT, 'simulated storage failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := models.ChangeEventResponse(db, row, row.Version, actor, "claim", "", 0); err == nil {
		t.Fatal("expected transaction error")
	}
	var actual models.EventResponse
	db.First(&actual, row.ID)
	if actual.OwnerID != 0 || actual.Version != 1 {
		t.Fatalf("claim partially persisted: %+v", actual)
	}
	var count int64
	db.Model(&models.EventResponseLog{}).Where("response_id=?", row.ID).Count(&count)
	if count != 0 {
		t.Fatal("audit partially persisted")
	}
}
