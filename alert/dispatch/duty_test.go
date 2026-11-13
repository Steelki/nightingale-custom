package dispatch

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestDutyRecipientsDoNotIncludeLegacyTeams(t *testing.T) {
	dir := t.TempDir()
	db, err := gorm.Open(sqlite.Open(filepath.Join(dir, "duty.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	if err = db.Exec("CREATE TABLE users (id INTEGER PRIMARY KEY, username TEXT, nickname TEXT, disabled INTEGER, email TEXT, phone TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	if err = db.Exec("INSERT INTO users VALUES (1,'oncall','',0,'oncall@example.invalid','10001'),(2,'legacy','',0,'legacy@example.invalid','10002')").Error; err != nil {
		t.Fatal(err)
	}
	if err = models.InitDutySchema(db, filepath.Join(dir, "key")); err != nil {
		t.Fatal(err)
	}
	g := models.DutyGroup{Name: "test", Config: models.DutyConfig{MemberIDs: []int64{1}, Timezone: "UTC", Plans: []models.DutyPlan{{EffectiveDate: "2020-01-01", RotationDays: 1, UserIDs: []int64{1}}}, Fallback: models.DutyFallback{Kind: "inherit"}}}
	if err = models.SaveDutyGroup(db, &g, "test"); err != nil {
		t.Fatal(err)
	}
	config := &models.NotifyConfig{DutyGroupID: g.ID, Params: map[string]interface{}{"user_ids": []int64{2}, "user_group_ids": []int64{99}, "subject": "retain"}}
	channel := &models.NotifyChannelConfig{RequestType: "smtp", ParamConfig: &models.NotifyParamConfig{UserInfo: &models.UserInfo{ContactKey: "email"}}}
	c := ctx.NewContext(context.Background(), db, true)
	_, prepared, sendtos, _, err := prepareDutyContext(c, g.ID, channel, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(sendtos) != 1 || sendtos[0] != "oncall@example.invalid" {
		t.Fatalf("wrong recipients: %v", sendtos)
	}
	if _, ok := prepared.Params["user_group_ids"]; ok {
		t.Fatal("legacy groups retained")
	}
	if _, ok := prepared.Params["user_ids"]; ok {
		t.Fatal("legacy users retained")
	}
	if prepared.Params["subject"] != "retain" || config.Params["user_ids"] == nil {
		t.Fatal("custom params lost or original cache mutated")
	}
	if err = db.Exec("UPDATE users SET disabled=1 WHERE id=1").Error; err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err = prepareDutyContext(c, g.ID, channel, config); err == nil {
		t.Fatal("disabled oncall accepted or fell back to legacy")
	}
	recovery := &models.AlertCurEvent{Hash: "recovery", FirstTriggerTime: 123, IsRecovered: true}
	if e := db.Create(&models.EventResponse{Episode: models.ResponseEpisode("recovery", 123), Hash: "recovery", FirstTriggerTime: 123, OwnerID: 2, Recovered: true}).Error; e != nil {
		t.Fatal(e)
	}
	_, _, recoveryTargets, _, e := prepareDutyContext(c, g.ID, channel, config, recovery)
	if e != nil || len(recoveryTargets) != 1 || recoveryTargets[0] != "legacy@example.invalid" {
		t.Fatalf("recovery must reach claimant even when oncall disabled: %v %v", recoveryTargets, e)
	}

}

func TestClaimStopsRepeatsButAllowsRecovery(t *testing.T) {
	dir := t.TempDir()
	db, err := gorm.Open(sqlite.Open(filepath.Join(dir, "response.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	if err = models.InitDutySchema(db, filepath.Join(dir, "key")); err != nil {
		t.Fatal(err)
	}
	event := &models.AlertCurEvent{Hash: "stable", FirstTriggerTime: time.Now().Unix()}
	if err = models.ObserveEventResponse(db, event); err != nil {
		t.Fatal(err)
	}
	db.Model(&models.EventResponse{}).Where("episode=?", models.ResponseEpisode(event.Hash, event.FirstTriggerTime)).Update("owner_id", 1)
	if claimed, err := models.ResponseIsClaimed(db, event); err != nil || !claimed {
		t.Fatal("claimed repeat not suppressed", err)
	}
	event.IsRecovered = true
	if claimed, err := models.ResponseIsClaimed(db, event); err != nil || claimed {
		t.Fatal("recovery wrongly suppressed", err)
	}
	if err = models.ObserveEventResponse(db, event); err != nil {
		t.Fatal(err)
	}
	var state models.EventResponse
	db.First(&state)
	if !state.Recovered {
		t.Fatal("recovery state missing")
	}
	event.IsRecovered = false
	event.FirstTriggerTime++
	if claimed, err := models.ResponseIsClaimed(db, event); err != nil || claimed {
		t.Fatal("new episode inherited old claim", err)
	}
}
