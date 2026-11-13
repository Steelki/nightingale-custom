package teamsvoice

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"gorm.io/gorm"
)

func validCallID(id string) bool { return regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`).MatchString(id) }

// Callback processes only authenticated notifications. Separate fact columns make
// duplicates and late ringing/established notifications unable to undo termination.
func Callback(db *gorm.DB, id string, body []byte, now time.Time) error {
	var doc struct {
		Value []struct {
			Resource string          `json:"resourceUrl"`
			Data     json.RawMessage `json:"resourceData"`
		}
	}
	if json.Unmarshal(body, &doc) != nil || len(doc.Value) == 0 || len(doc.Value) > 100 {
		return errors.New("invalid callback body")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		for _, n := range doc.Value {
			parts := strings.Split(strings.Trim(n.Resource, "/"), "/")
			if len(parts) < 3 || parts[0] != "communications" || parts[1] != "calls" || !validCallID(parts[2]) {
				return errors.New("invalid call resource")
			}
			var row models.TeamsVoiceCall
			if e := tx.First(&row, "id=?", id).Error; e != nil {
				return e
			}
			if row.CallID != "" && row.CallID != parts[2] {
				return errors.New("call ID mismatch")
			}
			if row.State == "pending" || row.State == "preparing" || row.State == "skipped" || row.State == "failed" {
				return errors.New("call has not been submitted")
			}
			var data struct {
				State   string `json:"state"`
				Status  string `json:"status"`
				Context string `json:"clientContext"`
				Result  struct {
					Code    int `json:"code"`
					Subcode int `json:"subcode"`
				} `json:"resultInfo"`
			}
			if json.Unmarshal(n.Data, &data) != nil {
				return errors.New("invalid resource data")
			}
			if e := bindCall(tx, id, parts[2]); e != nil {
				return e
			}
			q := tx.Model(&models.TeamsVoiceCall{}).Where("id=?", id)
			if len(parts) >= 5 && parts[3] == "operations" {
				if data.Context != id {
					return errors.New("operation context mismatch")
				}
				if data.Status == "completed" || data.Status == "failed" {
					if data.Status == "failed" {
						if e := reportFailure(tx, row, "Teams告警标题播放失败"); e != nil {
							return e
						}
					}
					// Completion is a playback fact, not proof that the person listened to every word.
					if e := q.Where("play_state NOT IN ?", []string{"completed", "failed"}).Update("play_state", data.Status).Error; e != nil {
						return e
					}
				}
				continue
			}
			switch data.State {
			case "established":
				if e := q.Where("answered_at=0").Update("answered_at", now.Unix()).Error; e != nil {
					return e
				}
				if e := tx.Model(&models.TeamsVoiceCall{}).Where("id=? AND ended_at=0", id).Update("state", "connected").Error; e != nil {
					return e
				}
			case "ringing", "establishing":
				if e := q.Where("answered_at=0 AND ended_at=0").Update("state", data.State).Error; e != nil {
					return e
				}
			case "terminated":
				if data.Result.Code >= 400 {
					if e := reportFailure(tx, row, fmt.Sprintf("Teams通话结束，平台错误码%d/%d", data.Result.Code, data.Result.Subcode)); e != nil {
						return e
					}
				}
				// Keep provider reason codes; don't map all terminated calls to a failed delivery.
				raw, _ := json.Marshal(data.Result)
				if e := q.Where("ended_at=0").Updates(map[string]interface{}{"state": "ended", "ended_at": now.Unix(), "reason": string(raw)}).Error; e != nil {
					return e
				}
			}
		}
		return nil
	})
}

// MySQL may report zero affected rows when a duplicate sets the same value.
// Re-read the binding instead of treating that as a conflicting callback.
func bindCall(db *gorm.DB, id, callID string) error {
	if e := db.Model(&models.TeamsVoiceCall{}).Where("id=? AND (call_id=? OR call_id=?)", id, "", callID).Update("call_id", callID).Error; e != nil {
		return e
	}
	var row models.TeamsVoiceCall
	if e := db.Select("id", "call_id").First(&row, "id=?", id).Error; e != nil {
		return e
	}
	if row.CallID != callID {
		return errors.New("call ID mismatch")
	}
	return nil
}
