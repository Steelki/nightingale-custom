package dutynotify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ccfos/nightingale/v6/alert/sender"
	"github.com/ccfos/nightingale/v6/models"
	nctx "github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

type transport func(*http.Request) (*http.Response, error)

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) { return t(r) }
func fixture(t *testing.T) (*gorm.DB, models.DutyGroup, models.NotifyRule, models.AlertCurEvent) {
	t.Helper()
	dir := t.TempDir()
	db, e := gorm.Open(sqlite.Open(filepath.Join(dir, "test.db")), &gorm.Config{})
	if e != nil {
		t.Fatal(e)
	}
	sql, _ := db.DB()
	t.Cleanup(func() { sql.Close() })
	if e = db.Exec("CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, nickname TEXT, disabled INTEGER, phone TEXT, email TEXT)").Error; e != nil {
		t.Fatal(e)
	}
	db.Exec("INSERT INTO users VALUES (1,'oncall','张三',0,'13800000001','one@example.invalid'),(2,'owner','李四',0,'13800000002','two@example.invalid')")
	if e = models.InitDutySchema(db, filepath.Join(dir, "key")); e != nil {
		t.Fatal(e)
	}
	if e = db.AutoMigrate(&models.NotifyRule{}, &models.NotifyChannelConfig{}, &models.AlertCurEvent{}); e != nil {
		t.Fatal(e)
	}
	g := models.DutyGroup{Name: "运维值班", Config: models.DutyConfig{Timezone: "Asia/Shanghai", MemberIDs: []int64{1, 2}, Plans: []models.DutyPlan{{EffectiveDate: "2020-01-01", RotationDays: 1, UserIDs: []int64{1}}}, Endpoints: []models.DutyEndpoint{{Kind: "webhook", URL: "https://group.invalid/hook", Secret: "group-secret"}}, Fallback: models.DutyFallback{Kind: "inherit"}}}
	if e = models.SaveDutyGroup(db, &g, "test"); e != nil {
		t.Fatal(e)
	}
	c := models.NotifyChannelConfig{Name: "公共Webhook", Ident: "callback", Enable: true, RequestType: "http"}
	if e = db.Create(&c).Error; e != nil {
		t.Fatal(e)
	}
	r := models.NotifyRule{Name: "test", Enable: true, ReceiverMode: "duty_group", DutyGroupID: g.ID, NotifyConfigs: []models.NotifyConfig{{ChannelID: c.ID, Severities: []int{1, 2, 3}}}}
	if e = db.Create(&r).Error; e != nil {
		t.Fatal(e)
	}
	ev := models.AlertCurEvent{Hash: "episode", FirstTriggerTime: 100, TriggerTime: time.Now().Unix(), Severity: 1, RuleName: "CPU过高", TargetIdent: "host-1", TriggerValue: "99", NotifyRuleIds: []int64{r.ID}, Annotations: "{}", RuleConfig: "{}"}
	if e = db.Create(&ev).Error; e != nil {
		t.Fatal(e)
	}
	if e = models.ObserveEventResponse(db, &ev); e != nil {
		t.Fatal(e)
	}
	return db, g, r, ev
}
func worker(db *gorm.DB, bodies *[]string) *Worker {
	return &Worker{DB: db, SiteURL: func() string { return "http://localhost:8774/base" }, HTTPClient: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(body))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"errcode":0,"code":0}`)), Header: http.Header{}}, nil
	})}}
}
func allJobs(t *testing.T, db *gorm.DB) []models.DutyNotice {
	t.Helper()
	var jobs []models.DutyNotice
	if e := db.Order("id").Find(&jobs).Error; e != nil {
		t.Fatal(e)
	}
	return jobs
}
func state(t *testing.T, db *gorm.DB) *models.EventResponse {
	t.Helper()
	var r models.EventResponse
	if e := db.First(&r).Error; e != nil {
		t.Fatal(e)
	}
	return &r
}
func change(t *testing.T, db *gorm.DB, actorID int64, action string, target int64) {
	t.Helper()
	r := state(t, db)
	var u models.User
	db.First(&u, actorID)
	if e := models.ChangeEventResponse(db, r, r.Version, &u, action, "", target); e != nil {
		t.Fatal(e)
	}
}
func TestDutyFallbackInheritancePayloadDedup(t *testing.T) {
	db, g, r, ev := fixture(t)
	s := models.DutySystemSettings{Fallback: models.DutyFallback{Kind: "webhook", Target: "https://fallback.invalid/hook", Secret: "private-secret"}}
	if e := models.SaveDutySettings(db, &s, "test"); e != nil {
		t.Fatal(e)
	}
	ev.DutyRecipientName = "张三"
	sendErr := errors.New("connection timeout https://secret.invalid/path?token=private-token password=secretpass")
	for i := 0; i < 2; i++ {
		if e := models.EnqueueNotificationFailure(db, &ev, r.ID, "Email", "one@example.invalid", sendErr); e != nil {
			t.Fatal(e)
		}
	}
	jobs := allJobs(t, db)
	if len(jobs) != 1 {
		t.Fatalf("dedup %d", len(jobs))
	}
	raw, _ := json.Marshal(jobs)
	if strings.Contains(string(raw), "private-secret") || strings.Contains(string(raw), "private-token") {
		t.Fatal("secret leaked")
	}
	p, e := jobs[0].Decode()
	if e != nil || p.Fallback.Target != s.Fallback.Target && p.Fallback.Target != "https://fallback.invalid/hook" {
		t.Fatalf("fallback %+v %v", p, e)
	}
	bodies := []string{}
	w := worker(db, &bodies)
	if e = w.RunOnce(context.Background(), time.Now()); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], "CPU过高") || !strings.Contains(bodies[0], "张三") || !strings.Contains(bodies[0], "connection timeout") || strings.Contains(bodies[0], "private-token") {
		t.Fatalf("body %v", bodies)
	}
	if allJobs(t, db)[0].Status != "sent" {
		t.Fatal("not sent")
	}
	if e = w.RunOnce(context.Background(), time.Now()); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 1 {
		t.Fatal("replayed")
	}
	g.Config.Fallback = models.DutyFallback{Kind: "none"}
	if e = models.SaveDutyGroup(db, &g, "test"); e != nil {
		t.Fatal(e)
	}
	ev.TriggerTime++
	if e = models.EnqueueNotificationFailure(db, &ev, r.ID, "Email", "one@example.invalid", sendErr); e != nil {
		t.Fatal(e)
	}
	if allJobs(t, db)[1].Status != "skipped" {
		t.Fatal("none did not override global")
	}
}
func TestDutyFallbackFailureNoRecursionOrRetry(t *testing.T) {
	db, g, r, ev := fixture(t)
	g.Config.Fallback = models.DutyFallback{Kind: "dingtalk", Target: "https://oapi.dingtalk.com/robot/send?access_token=private"}
	if e := models.SaveDutyGroup(db, &g, "test"); e != nil {
		t.Fatal(e)
	}
	if e := models.EnqueueNotificationFailure(db, &ev, r.ID, "phone", "张三", errors.New("无人接听")); e != nil {
		t.Fatal(e)
	}
	calls := 0
	w := worker(db, &[]string{})
	w.HTTPClient = &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"errcode":310000,"errmsg":"secret"}`)), Header: http.Header{}}, nil
	})}
	for i := 0; i < 2; i++ {
		if e := w.RunOnce(context.Background(), time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	jobs := allJobs(t, db)
	if len(jobs) != 1 || jobs[0].Status != "failed" || calls != 1 || strings.Contains(jobs[0].Reason, "secret") {
		t.Fatalf("%+v calls%d", jobs, calls)
	}
}
func TestDutyLifecycleTransferUnclaimAndStale(t *testing.T) {
	db, _, _, _ := fixture(t)
	change(t, db, 1, "claim", 0)
	change(t, db, 1, "transfer", 2)
	bodies := []string{}
	w := worker(db, &bodies)
	if e := w.RunOnce(context.Background(), time.Now()); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], "李四") || !strings.Contains(bodies[0], "已转交") {
		t.Fatalf("%v", bodies)
	}
	change(t, db, 2, "unclaim", 0)
	if e := w.RunOnce(context.Background(), time.Now()); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 2 || !strings.Contains(bodies[1], "张三") || !strings.Contains(bodies[1], "取消认领") {
		t.Fatalf("%v", bodies)
	}
	change(t, db, 1, "claim", 0)
	change(t, db, 1, "transfer", 2)
	change(t, db, 2, "unclaim", 0)
	change(t, db, 1, "claim", 0)
	if e := w.RunOnce(context.Background(), time.Now()); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 2 {
		t.Fatal("stale notifications sent")
	}
	jobs := allJobs(t, db)
	if jobs[2].Status != "skipped" || jobs[3].Status != "skipped" {
		t.Fatalf("%+v", jobs)
	}
}
func TestDutyLifecycleCommentDoesNotCancelNoticeAndRecoveryDoes(t *testing.T) {
	db, _, _, ev := fixture(t)
	change(t, db, 1, "claim", 0)
	change(t, db, 1, "transfer", 2)
	row := state(t, db)
	u := models.User{Id: 2, Username: "owner"}
	if e := models.ChangeEventResponse(db, row, row.Version, &u, "comment", "正在处理", 0); e != nil {
		t.Fatal(e)
	}
	bodies := []string{}
	w := worker(db, &bodies)
	if e := w.RunOnce(context.Background(), time.Now()); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 1 {
		t.Fatal("comment canceled delivery")
	}
	change(t, db, 2, "transfer", 1)
	ev.IsRecovered = true
	if e := models.ObserveEventResponse(db, &ev); e != nil {
		t.Fatal(e)
	}
	if e := w.RunOnce(context.Background(), time.Now()); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 1 {
		t.Fatal("recovered notification not canceled")
	}
}
func TestDutyHourlyLiveRosterCadenceAndDisable(t *testing.T) {
	db, g, _, _ := fixture(t)
	g.Config.HourlyReminder = true
	if e := models.SaveDutyGroup(db, &g, "test"); e != nil {
		t.Fatal(e)
	}
	now := time.Now().Add(time.Second)
	bodies := []string{}
	w := worker(db, &bodies)
	if e := w.RunOnce(context.Background(), now); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], "张三") || !strings.Contains(bodies[0], "13800000001") {
		t.Fatalf("%v", bodies)
	}
	if e := w.RunOnce(context.Background(), now.Add(59*time.Minute)); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 1 {
		t.Fatal("too frequent")
	}
	loc, _ := time.LoadLocation(g.Config.Timezone)
	g.Config.Overrides = []models.DutyOverride{{Date: now.Add(time.Hour).In(loc).Format("2006-01-02"), UserID: 2, Reason: "替班"}}
	if e := models.SaveDutyGroup(db, &g, "test"); e != nil {
		t.Fatal(e)
	}
	if e := w.RunOnce(context.Background(), now.Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 2 || !strings.Contains(bodies[1], "李四") || !strings.Contains(bodies[1], "13800000002") {
		t.Fatalf("%v", bodies)
	}
	// After downtime only one current notice, not one for every missed hour.
	if e := w.RunOnce(context.Background(), now.Add(10*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 3 {
		t.Fatal("backfill storm")
	}
	if e := models.EnqueueDueDutyReminders(db, now.Add(11*time.Hour)); e != nil {
		t.Fatal(e)
	}
	g.Config.HourlyReminder = false
	if e := models.SaveDutyGroup(db, &g, "test"); e != nil {
		t.Fatal(e)
	}
	if e := w.RunOnce(context.Background(), now.Add(11*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 3 {
		t.Fatal("sent after disabled")
	}
	jobs := allJobs(t, db)
	if jobs[len(jobs)-1].Status != "skipped" {
		t.Fatal("pending not canceled")
	}
}
func TestDutyRecoveryRecipientAndUnknownNoReplay(t *testing.T) {
	db, g, r, ev := fixture(t)
	change(t, db, 2, "claim", 0)
	ev.IsRecovered = true
	user, e := models.ResolveDutyRecipient(db, g, &ev, 0)
	if e != nil || user.Id != 2 {
		t.Fatalf("recovery routed to oncall %v %v", user, e)
	}
	g.Config.Fallback = models.DutyFallback{Kind: "webhook", Target: "https://fallback.invalid/hook"}
	if e = models.SaveDutyGroup(db, &g, "test"); e != nil {
		t.Fatal(e)
	}
	if e = models.EnqueueNotificationFailure(db, &ev, r.ID, "Email", "张三", errors.New("failed")); e != nil {
		t.Fatal(e)
	}
	db.Model(&models.DutyNotice{}).Where("kind=?", "fallback").Updates(map[string]interface{}{"status": "sending", "started_at": time.Now().Add(-3 * time.Minute).Unix()})
	bodies := []string{}
	w := worker(db, &bodies)
	if e = w.RunOnce(context.Background(), time.Now()); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 0 || allJobs(t, db)[0].Status != "unknown" {
		t.Fatal("uncertain delivery replayed")
	}
}
func TestDutyFallbackEmailAndOriginalMode(t *testing.T) {
	db, _, r, ev := fixture(t)
	r.ReceiverMode = "original"
	r.DutyGroupID = 0
	db.Model(&models.NotifyRule{}).Where("id=?", r.ID).Updates(map[string]interface{}{"receiver_mode": "original", "duty_group_id": 0})
	c := models.NotifyChannelConfig{Name: "Email", Enable: true, RequestType: "smtp", RequestConfig: &models.RequestConfig{SMTPRequestConfig: &models.SMTPRequestConfig{Host: "smtp.invalid", Port: 587, From: "sender@example.invalid"}}}
	db.Create(&c)
	s := models.DutySystemSettings{Fallback: models.DutyFallback{Kind: "email", Target: "fallback@example.invalid", SMTPChannelID: c.ID}}
	if e := models.SaveDutySettings(db, &s, "test"); e != nil {
		t.Fatal(e)
	}
	if e := models.EnqueueNotificationFailure(db, &ev, r.ID, "Email", "one@example.invalid", errors.New("connection refused")); e != nil {
		t.Fatal(e)
	}
	w := worker(db, &[]string{})
	calls := 0
	w.Email = func(_ context.Context, channel *models.NotifyChannelConfig, to []string, tpl map[string]interface{}) error {
		calls++
		if channel.ID != c.ID || len(to) != 1 || to[0] != "fallback@example.invalid" || !strings.Contains(tpl["content"].(string), "CPU过高") {
			t.Fatal("wrong email")
		}
		return nil
	}
	if e := w.RunOnce(context.Background(), time.Now()); e != nil {
		t.Fatal(e)
	}
	if calls != 1 || allJobs(t, db)[0].Status != "sent" {
		t.Fatal("fallback email not sent")
	}
}

func TestDutyLifecycleRollbackAndHourlyPendingDestinationChange(t *testing.T) {
	db, g, _, _ := fixture(t)
	change(t, db, 1, "claim", 0)
	row := state(t, db)
	version := row.Version
	db.Migrator().DropTable(&models.DutyNotice{})
	actor := models.User{Id: 1, Username: "oncall"}
	if e := models.ChangeEventResponse(db, row, row.Version, &actor, "transfer", "", 2); e == nil {
		t.Fatal("missing outbox accepted transfer")
	}
	if got := state(t, db); got.OwnerID != 1 || got.Version != version {
		t.Fatal("transfer not rolled back")
	}
	db.AutoMigrate(&models.DutyNotice{})
	g.Config.HourlyReminder = true
	if e := models.SaveDutyGroup(db, &g, "test"); e != nil {
		t.Fatal(e)
	}
	now := time.Now().Add(time.Second)
	if e := models.EnqueueDueDutyReminders(db, now); e != nil {
		t.Fatal(e)
	}
	g.Config.Endpoints = []models.DutyEndpoint{{Kind: "webhook", URL: "https://new.invalid/hook"}}
	if e := models.SaveDutyGroup(db, &g, "test"); e != nil {
		t.Fatal(e)
	}
	bodies := []string{}
	w := worker(db, &bodies)
	if e := w.RunOnce(context.Background(), now); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 0 || allJobs(t, db)[0].Status != "skipped" {
		t.Fatal("old destination still notified")
	}
}

func TestDutyFailureRecordHookExcludesPipelineDrop(t *testing.T) {
	db, g, r, ev := fixture(t)
	g.Config.Fallback = models.DutyFallback{Kind: "webhook", Target: "https://fallback.invalid/hook"}
	if e := models.SaveDutyGroup(db, &g, "test"); e != nil {
		t.Fatal(e)
	}
	c := nctx.NewContext(context.Background(), db, true)
	sender.NotifyRecord(c, []*models.AlertCurEvent{&ev}, r.ID, "Email", "张三", "", errors.New("processor_by_notify_rule_id:1 pipeline_id:2, drop by pipeline"))
	if len(allJobs(t, db)) != 0 {
		t.Fatal("pipeline drop triggered fallback")
	}
	sender.NotifyRecord(c, []*models.AlertCurEvent{&ev}, r.ID, "Email", "张三", "", errors.New("SMTP connection refused"))
	if len(allJobs(t, db)) != 1 {
		t.Fatal("failure hook did not enqueue fallback")
	}
	bodies := []string{}
	w := worker(db, &bodies)
	if e := w.RunOnce(context.Background(), time.Now()); e != nil {
		t.Fatal(e)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], "SMTP connection refused") {
		t.Fatal("hook did not reach worker")
	}
}

func TestDutyReminderConfigurableInterval(t *testing.T) {
	db, g, _, _ := fixture(t)
	if g.Config.ReminderDuration() != time.Hour {
		t.Fatal("legacy default changed")
	}
	g.Config.HourlyReminder = true
	g.Config.ReminderInterval = 30
	g.Config.ReminderUnit = "minute"
	if err := models.SaveDutyGroup(db, &g, "test"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Second)
	bodies := []string{}
	w := worker(db, &bodies)
	for _, at := range []time.Time{now, now.Add(29 * time.Minute), now.Add(30 * time.Minute)} {
		if err := w.RunOnce(context.Background(), at); err != nil {
			t.Fatal(err)
		}
	}
	if len(bodies) != 2 {
		t.Fatalf("30 minute cadence: %d", len(bodies))
	}
	if err := models.EnqueueDueDutyReminders(db, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	g.Config.ReminderInterval = 2
	g.Config.ReminderUnit = "hour"
	before := time.Now()
	if err := models.SaveDutyGroup(db, &g, "test"); err != nil {
		t.Fatal(err)
	}
	var clock models.DutyReminderClock
	db.First(&clock, "group_id=?", g.ID)
	if clock.NextAt < before.Add(2*time.Hour).Unix() || clock.NextAt > time.Now().Add(2*time.Hour).Unix() {
		t.Fatal("interval change did not reset clock")
	}
	for _, job := range allJobs(t, db) {
		if job.Status == "pending" {
			t.Fatal("old pending reminder remains")
		}
	}
	originalNext := clock.NextAt
	g.Name = "renamed"
	if err := models.SaveDutyGroup(db, &g, "test"); err != nil {
		t.Fatal(err)
	}
	db.First(&clock, "group_id=?", g.ID)
	if clock.NextAt != originalNext {
		t.Fatal("unrelated save reset clock")
	}
	if err := w.RunOnce(context.Background(), time.Unix(clock.NextAt-1, 0)); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 2 {
		t.Fatal("early delivery")
	}
	if err := w.RunOnce(context.Background(), time.Unix(clock.NextAt, 0)); err != nil {
		t.Fatal(err)
	}
	if len(bodies) != 3 {
		t.Fatal("missing two-hour delivery")
	}
}
func TestDutyReminderIntervalValidation(t *testing.T) {
	for _, tc := range []struct {
		n     int
		unit  string
		valid bool
	}{{0, "", true}, {1, "minute", true}, {1440, "minute", true}, {24, "hour", true}, {0, "minute", false}, {-1, "hour", false}, {1441, "minute", false}, {25, "hour", false}, {2, "day", false}} {
		t.Run(tc.unit+time.Duration(tc.n).String(), func(t *testing.T) {
			db, g, _, _ := fixture(t)
			g.Config.ReminderInterval = tc.n
			g.Config.ReminderUnit = tc.unit
			err := models.SaveDutyGroup(db, &g, "test")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
}
