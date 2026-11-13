package teamsvoice

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/glebarez/sqlite"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type transport func(*http.Request) (*http.Response, error)

func (f transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(body string, code int) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func config() models.TeamsVoiceConfig {
	return models.TeamsVoiceConfig{TenantID: "11111111-1111-4111-8111-111111111111", AppID: "22222222-2222-4222-8222-222222222222", SecretEnv: "N9E_TEAMS_TEST_SECRET", SpeechKeyEnv: "N9E_TEAMS_TEST_SPEECH", PublicURL: "https://alerts.example.invalid", SpeechRegion: "eastasia", Voice: "zh-CN-XiaoxiaoNeural", Mappings: []models.TeamsVoiceMapping{{Email: "oncall@example.invalid", UserID: "33333333-3333-4333-8333-333333333333"}}}
}
func setup(t *testing.T) (*gorm.DB, models.TeamsVoiceCall) {
	t.Helper()
	db, e := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if e != nil {
		t.Fatal(e)
	}
	sql, _ := db.DB()
	sql.SetMaxOpenConns(1)
	t.Cleanup(func() { sql.Close() })
	if e = models.InitDutySchema(db, filepath.Join(t.TempDir(), "key")); e != nil {
		t.Fatal(e)
	}
	if e = db.AutoMigrate(&models.User{}, &models.NotifyChannelConfig{}); e != nil {
		t.Fatal(e)
	}
	cfg := config()
	ch := models.NotifyChannelConfig{ID: 1, Enable: true, RequestType: models.RequestTypeTeamsVoice, RequestConfig: &models.RequestConfig{TeamsVoiceConfig: &cfg}}
	if e = db.Create(&ch).Error; e != nil {
		t.Fatal(e)
	}
	if e = db.Create(&models.User{Id: 1, Username: "oncall", Email: "oncall@example.invalid"}).Error; e != nil {
		t.Fatal(e)
	}
	evt := &models.AlertCurEvent{Hash: "test-hash", FirstTriggerTime: 100, TriggerTime: 100, RuleName: "数据库连接数过高 < & >"}
	ids, e := models.EnqueueTeamsVoice(db, &ch, 0, 0, []*models.AlertCurEvent{evt}, []string{"oncall@example.invalid"})
	if e != nil {
		t.Fatal(e)
	}
	twice, e := models.EnqueueTeamsVoice(db, &ch, 0, 0, []*models.AlertCurEvent{evt}, []string{"oncall@example.invalid"})
	if e != nil || twice[0] != ids[0] {
		t.Fatal("duplicate notification not deduplicated", e)
	}
	var row models.TeamsVoiceCall
	if e = db.First(&row, "id=?", ids[0]).Error; e != nil {
		t.Fatal(e)
	}
	return db, row
}
func notify(db *gorm.DB, id, state string) error {
	return Callback(db, id, []byte(`{"value":[{"resourceUrl":"/communications/calls/platform-call","resourceData":{"state":"`+state+`"}}]}`), time.Now())
}
func TestCallEarlyDuplicateAndOutOfOrderCallbacks(t *testing.T) {
	db, row := setup(t)
	t.Setenv(config().SecretEnv, "fake-secret")
	t.Setenv(config().SpeechKeyEnv, "fake-speech-key")
	creates, plays, deletes := 0, 0, 0
	w := Worker{DB: db, AudioDir: t.TempDir(), Client: Client{HTTP: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		raw, _ := io.ReadAll(r.Body)
		switch r.URL.Host {
		case "login.microsoftonline.com":
			return reply(`{"access_token":"fake-token"}`, 200), nil
		case "eastasia.tts.speech.microsoft.com":
			if !strings.Contains(string(raw), "&amp;") || strings.Contains(string(raw), "< & >") {
				t.Fatal("title not SSML escaped", string(raw))
			}
			return reply("RIFF0000WAVE"+strings.Repeat("0", 32), 200), nil
		case "graph.microsoft.com":
			if r.Header.Get("Authorization") != "Bearer fake-token" {
				t.Fatal("missing authorization")
			}
			switch {
			case r.Method == "DELETE":
				deletes++
				if e := notify(db, row.ID, "terminated"); e != nil {
					t.Fatal(e)
				}
				return reply("", 204), nil
			case strings.HasSuffix(r.URL.Path, "/playPrompt"):
				plays++
				var body map[string]interface{}
				if json.Unmarshal(raw, &body) != nil || body["clientContext"] != row.ID {
					t.Fatal("invalid prompt body")
				}
				if e := Callback(db, row.ID, []byte(`{"value":[{"resourceUrl":"/communications/calls/platform-call/operations/op1","resourceData":{"status":"completed","clientContext":"`+row.ID+`"}}]}`), time.Now()); e != nil {
					t.Fatal(e)
				}
				return reply(`{"id":"op1"}`, 200), nil
			default:
				creates++
				if !strings.Contains(string(raw), "serviceHostedMediaConfig") || !strings.Contains(string(raw), config().Mappings[0].UserID) {
					t.Fatal("invalid create call")
				}
				if e := notify(db, row.ID, "established"); e != nil {
					t.Fatal(e)
				}
				return reply(`{"id":"platform-call"}`, 201), nil
			}
		}
		t.Fatal("unexpected external host", r.URL.Host)
		return nil, nil
	})}}}
	for i := 0; i < 4; i++ {
		if e := w.RunOnce(context.Background(), time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	for _, state := range []string{"ringing", "established", "terminated"} {
		if e := notify(db, row.ID, state); e != nil {
			t.Fatal(e)
		}
	}
	db.First(&row, "id=?", row.ID)
	if row.State != "ended" || row.AnsweredAt == 0 || row.PlayState != "completed" || creates != 1 || plays != 1 || deletes != 1 {
		t.Fatalf("bad state: %+v, counts=%d/%d/%d", row, creates, plays, deletes)
	}
	if e := Callback(db, row.ID, []byte(`{"value":[{"resourceUrl":"/communications/calls/other","resourceData":{"state":"established"}}]}`), time.Now()); e == nil {
		t.Fatal("accepted mismatched call")
	}
	public, _ := json.Marshal(row)
	if strings.Contains(string(public), row.AudioToken) || strings.Contains(string(public), string(row.Payload)) {
		t.Fatal("private data in response")
	}
	if e := w.RunOnce(context.Background(), time.Now().Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(w.AudioDir, row.ID+".wav")); !os.IsNotExist(e) {
		t.Fatal("expired audio not removed")
	}
}
func TestUnknownNotRedialedAndClaimCancels(t *testing.T) {
	for _, state := range []string{"calling", "claimed"} {
		t.Run(state, func(t *testing.T) {
			db, row := setup(t)
			if state == "calling" {
				db.Model(&row).Updates(map[string]interface{}{"state": "calling", "created_at": time.Now().Add(-5 * time.Minute).Unix()})
			} else {
				if e := db.Create(&models.EventResponse{Episode: row.Episode, OwnerID: 1}).Error; e != nil {
					t.Fatal(e)
				}
			}
			w := Worker{DB: db, AudioDir: t.TempDir(), Client: Client{HTTP: &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) { t.Fatal("must not send"); return nil, nil })}}}
			for i := 0; i < 2; i++ {
				if e := w.RunOnce(context.Background(), time.Now()); e != nil {
					t.Fatal(e)
				}
			}
			db.First(&row, "id=?", row.ID)
			want := "unknown"
			if state == "claimed" {
				want = "skipped"
			}
			if row.State != want {
				t.Fatal(row.State)
			}
		})
	}
}
func TestMicrosoftCallbackAuthentication(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	cfg := config()
	v := Verifier{keys: map[string]*rsa.PublicKey{"test": &key.PublicKey}, fetched: time.Now()}
	claims := func() jwt.MapClaims {
		return jwt.MapClaims{"iss": "https://graph.microsoft.com", "aud": cfg.AppID, "exp": time.Now().Add(time.Minute).Unix(), "tid": cfg.TenantID}
	}
	for _, name := range []string{"valid", "issuer", "audience", "tenant", "expired", "no_exp", "signature", "algorithm"} {
		t.Run(name, func(t *testing.T) {
			c := claims()
			method := jwt.SigningMethodRS256
			var signing any = key
			switch name {
			case "issuer":
				c["iss"] = "https://attacker.invalid"
			case "audience":
				c["aud"] = "other"
			case "tenant":
				c["tid"] = "other"
			case "expired":
				c["exp"] = time.Now().Add(-time.Minute).Unix()
			case "no_exp":
				delete(c, "exp")
			case "signature":
				other, _ := rsa.GenerateKey(rand.Reader, 2048)
				signing = other
			}
			var token *jwt.Token
			if name == "algorithm" {
				token = jwt.NewWithClaims(jwt.SigningMethodHS256, c)
				signing = []byte("not-a-public-key")
			} else {
				token = jwt.NewWithClaims(method, c)
			}
			token.Header["kid"] = "test"
			raw, e := token.SignedString(signing)
			if e != nil {
				t.Fatal(e)
			}
			e = v.Verify(context.Background(), raw, cfg)
			if (e == nil) != (name == "valid") {
				t.Fatalf("unexpected verification %v", e)
			}
		})
	}
	// Cold-start OIDC/JWKS discovery uses fixed trusted metadata, never JWT jku.
	v = Verifier{Client: Client{HTTP: &http.Client{Transport: transport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() == metadataURL {
			return reply(`{"jwks_uri":"https://keys.example.invalid/keys"}`, 200), nil
		}
		if r.URL.Host != "keys.example.invalid" {
			t.Fatal("unexpected key fetch")
		}
		return reply(`{"keys":[{"kty":"RSA","kid":"test","n":"`+base64.RawURLEncoding.EncodeToString(key.N.Bytes())+`","e":"AQAB"}]}`, 200), nil
	})}}}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims())
	token.Header["kid"] = "test"
	raw, _ := token.SignedString(key)
	if e := v.Verify(context.Background(), raw, cfg); e != nil {
		t.Fatal(e)
	}
}
func TestConfigAndSpeechBoundaries(t *testing.T) {
	cfg := config()
	if e := cfg.Validate(); e != nil {
		t.Fatal(e)
	}
	cfg.SecretEnv = "ACTUAL-SECRET"
	if cfg.Validate() == nil {
		t.Fatal("accepted credential instead of environment reference")
	}
	cfg = config()
	cfg.PublicURL = "http://localhost"
	if cfg.Validate() == nil {
		t.Fatal("accepted non-HTTPS callback")
	}
	if len([]rune(SpeechText(strings.Repeat("告", 1000)))) > 220 {
		t.Fatal("unbounded speech")
	}
	if strings.Contains(SpeechText("<break/>\n标题"), "<break") {
		t.Fatal("markup retained")
	}
}

func TestTTSFailureDoesNotCallAndQueuesFallbackOnce(t *testing.T) {
	db, row := setup(t)
	w := Worker{DB: db, AudioDir: t.TempDir(), Client: Client{HTTP: &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
		t.Fatal("missing Speech key must not issue a request")
		return nil, nil
	})}}}
	t.Setenv(config().SpeechKeyEnv, "")
	for i := 0; i < 2; i++ {
		if e := w.RunOnce(context.Background(), time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	db.First(&row, "id=?", row.ID)
	if row.State != "failed" || !row.FallbackQueued || !strings.Contains(row.Reason, "环境变量") {
		t.Fatalf("missing durable failure: %+v", row)
	}
}
func TestAudioFailureAndCallTerminationDoNotDuplicateFallback(t *testing.T) {
	db, row := setup(t)
	db.Model(&row).Updates(map[string]interface{}{"state": "connected", "call_id": "platform-call", "answered_at": time.Now().Unix(), "play_state": "accepted"})
	body := []byte(`{"value":[{"resourceUrl":"/communications/calls/platform-call/operations/op1","resourceData":{"status":"failed","clientContext":"` + row.ID + `"}}]}`)
	for i := 0; i < 2; i++ {
		if e := Callback(db, row.ID, body, time.Now()); e != nil {
			t.Fatal(e)
		}
	}
	if e := Callback(db, row.ID, []byte(`{"value":[{"resourceUrl":"/communications/calls/platform-call","resourceData":{"state":"terminated","resultInfo":{"code":500,"subcode":0}}}]}`), time.Now()); e != nil {
		t.Fatal(e)
	}
	db.First(&row, "id=?", row.ID)
	if !row.FallbackQueued || row.PlayState != "failed" || row.AnsweredAt == 0 {
		t.Fatal("callback failure lost")
	}
	var count int64
	db.Model(&models.DutyNotice{}).Count(&count)
	if count > 1 {
		t.Fatal("duplicate fallback")
	}
}
func TestMissingKeyCannotSilentlyReplaceEncryptedVoiceState(t *testing.T) {
	db, _ := setup(t)
	if e := models.InitDutySchema(db, filepath.Join(t.TempDir(), "missing-key")); e == nil {
		t.Fatal("replaced key despite existing encrypted call")
	}
}
