package models

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func rotationFixture(t *testing.T) (*gorm.DB, DutyGroup) {
	t.Helper()
	dir := t.TempDir()
	db, e := gorm.Open(sqlite.Open(filepath.Join(dir, "test.db")), &gorm.Config{})
	if e != nil {
		t.Fatal(e)
	}
	sql, _ := db.DB()
	t.Cleanup(func() { sql.Close() })
	if e = db.Exec("CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, nickname TEXT, disabled INTEGER)").Error; e != nil {
		t.Fatal(e)
	}
	db.Exec("INSERT INTO users VALUES (1,'A','',0),(2,'B','',0),(3,'C','',0)")
	if e = InitDutySchema(db, filepath.Join(dir, "key")); e != nil {
		t.Fatal(e)
	}
	g := DutyGroup{Name: "rotation", Config: DutyConfig{MemberIDs: []int64{1, 2, 3}, Timezone: "Asia/Shanghai", Plans: []DutyPlan{{EffectiveDate: "2026-01-01", RotationDays: 7, UserIDs: []int64{1, 2, 3}}}, Fallback: DutyFallback{Kind: "inherit"}}}
	if e = SaveDutyGroup(db, &g, "test"); e != nil {
		t.Fatal(e)
	}
	return db, g
}
func at(s string) time.Time {
	t, e := time.Parse(time.RFC3339, s)
	if e != nil {
		panic(e)
	}
	return t
}
func TestRotationImmediatePreservesHistory(t *testing.T) {
	db, old := rotationFixture(t)
	now := at("2026-11-12T13:30:00+08:00")
	g := old
	before, _ := ResolveDuty(db, old, now.Add(-time.Second))
	g.RotationChange = &RotationChange{Mode: "immediate", RotationDays: 1, UserIDs: []int64{1, 2, 3}, StartUserID: 3}
	if e := PrepareDutyRotation(&g, old, now); e != nil {
		t.Fatal(e)
	}
	historical, _ := ResolveDuty(db, g, now.Add(-time.Second))
	current, _ := ResolveDuty(db, g, now)
	tomorrow, _ := ResolveDuty(db, g, at("2026-11-13T00:00:00+08:00"))
	if historical.UserID != before.UserID || current.UserID != 3 || tomorrow.UserID != 1 {
		t.Fatalf("history/current/tomorrow: %v %v %v", historical, current, tomorrow)
	}
	rows, e := DutyCalendarRows(db, g, at("2026-11-12T00:00:00+08:00"), 1)
	if e != nil || len(rows) != 2 {
		t.Fatalf("intraday transition missing: %v %v", rows, e)
	}
}
func TestRotationNextShiftAndDate(t *testing.T) {
	_, old := rotationFixture(t)
	now := at("2026-01-10T13:30:00+08:00")
	for _, tc := range []struct{ mode, date, want string }{{"next_shift", "", "2026-01-15T00:00:00+08:00"}, {"date", "2026-01-20", "2026-01-20T00:00:00+08:00"}} {
		g := old
		g.RotationChange = &RotationChange{Mode: tc.mode, Date: tc.date, RotationDays: 2, UserIDs: []int64{1, 2}, StartUserID: 2}
		if e := PrepareDutyRotation(&g, old, now); e != nil {
			t.Fatal(e)
		}
		p := g.Config.Plans[1]
		if p.EffectiveAt != at(tc.want).Unix() || p.UserIDs[0] != 2 {
			t.Fatalf("%+v", p)
		}
	}
}
func TestRotationReplaceCancelAndRejectHistory(t *testing.T) {
	_, old := rotationFixture(t)
	now := at("2026-11-12T13:30:00+08:00")
	old.Config.Plans = append(old.Config.Plans, DutyPlan{EffectiveDate: "2026-12-01", RotationDays: 1, UserIDs: []int64{1}}, DutyPlan{EffectiveDate: "2027-01-01", RotationDays: 2, UserIDs: []int64{2}})
	g := old
	g.RotationChange = &RotationChange{Mode: "date", Date: "2026-12-15", RotationDays: 3, UserIDs: []int64{3}, StartUserID: 3}
	if e := PrepareDutyRotation(&g, old, now); e != nil || len(g.Config.Plans) != 2 {
		t.Fatalf("replace failed %v", e)
	}
	g = old
	g.RotationChange = &RotationChange{Mode: "cancel"}
	if e := PrepareDutyRotation(&g, old, now); e != nil || len(g.Config.Plans) != 1 {
		t.Fatalf("cancel failed %v", e)
	}
	g = old
	g.Config.Plans = []DutyPlan{{EffectiveDate: "2026-01-01", RotationDays: 1, UserIDs: []int64{1}}}
	if PrepareDutyRotation(&g, old, now) == nil {
		t.Fatal("history overwrite accepted")
	}
	g = old
	g.Config.Timezone = "UTC"
	if PrepareDutyRotation(&g, old, now) == nil {
		t.Fatal("timezone history rewrite accepted")
	}
}
func TestRotationInvalidCommands(t *testing.T) {
	_, old := rotationFixture(t)
	now := at("2026-11-12T13:30:00+08:00")
	for _, ch := range []RotationChange{{Mode: "bogus", RotationDays: 1, UserIDs: []int64{1}, StartUserID: 1}, {Mode: "date", Date: "2026-11-12", RotationDays: 1, UserIDs: []int64{1}, StartUserID: 1}, {Mode: "immediate", RotationDays: 1, UserIDs: []int64{1}, StartUserID: 2}, {Mode: "immediate", RotationDays: 0, UserIDs: []int64{1}, StartUserID: 1}} {
		g := old
		g.RotationChange = &ch
		if PrepareDutyRotation(&g, old, now) == nil {
			t.Fatalf("accepted %+v", ch)
		}
	}
	g := old
	g.ID = 0
	g.Config.Plans = []DutyPlan{{EffectiveDate: "2026-12-01", RotationDays: 1, UserIDs: []int64{1}}}
	if PrepareDutyRotation(&g, DutyGroup{}, now) == nil {
		t.Fatal("initial gap accepted")
	}
}
func TestRotationSaveCASAndFormerMembers(t *testing.T) {
	db, old := rotationFixture(t)
	g := old
	g.Config.MemberIDs = []int64{2, 3}
	g.RotationChange = &RotationChange{Mode: "immediate", RotationDays: 1, UserIDs: []int64{2, 3}, StartUserID: 2}
	if e := SaveDutyGroup(db, &g, "admin"); e != nil {
		t.Fatal(e)
	}
	var saved DutyGroup
	db.First(&saved, g.ID)
	if len(saved.Config.Plans) != 2 || saved.Config.Plans[0].UserIDs[0] != 1 {
		t.Fatal("historical member lost")
	}
	if e := SaveDutyGroup(db, &old, "stale"); e != ErrDutyConflict {
		t.Fatalf("CAS: %v", e)
	}
	saved.RotationChange = &RotationChange{Mode: "date", Date: time.Now().AddDate(0, 0, 2).Format("2006-01-02"), RotationDays: 1, UserIDs: []int64{1}, StartUserID: 1}
	if SaveDutyGroup(db, &saved, "admin") == nil {
		t.Fatal("future nonmember accepted")
	}
}
func TestRotationDSTCalendarDays(t *testing.T) {
	c := DutyConfig{Timezone: "America/New_York", Plans: []DutyPlan{{EffectiveDate: "2026-03-07", RotationDays: 2, UserIDs: []int64{1, 2}}}}
	next := nextDutyShift(c, at("2026-03-08T12:00:00-04:00"))
	if !next.Equal(at("2026-03-09T00:00:00-04:00")) {
		t.Fatal(next)
	}
}
func TestRotationPreviewDoesNotWrite(t *testing.T) {
	db, g := rotationFixture(t)
	g.RotationChange = &RotationChange{Mode: "date", Date: time.Now().AddDate(0, 0, 40).Format("2006-01-02"), RotationDays: 1, UserIDs: []int64{2}, StartUserID: 2}
	p, e := PreviewDutyRotation(db, g, time.Now())
	if e != nil || len(p.Rows) < 14 {
		t.Fatalf("%+v %v", p, e)
	}
	if p.Rows[0].At[:10] != g.RotationChange.Date {
		t.Fatal("preview does not show future change")
	}
	var saved DutyGroup
	db.First(&saved, g.ID)
	if len(saved.Config.Plans) != 1 || saved.Version != g.Version {
		t.Fatal("preview wrote to database")
	}
}
