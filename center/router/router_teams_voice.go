package router

import (
	"crypto/subtle"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/center/teamsvoice"
	"github.com/ccfos/nightingale/v6/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// Public routes use their own Microsoft JWT or expiring audio capability, never user-session bypass.
func (rt *Router) teamsVoiceCallback(c *gin.Context) {
	auth := c.GetHeader("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		c.Status(401)
		return
	}
	id := c.Param("id")
	if _, e := uuid.Parse(id); e != nil {
		c.Status(404)
		return
	}
	var row models.TeamsVoiceCall
	if models.DB(rt.Ctx).First(&row, "id=?", id).Error != nil {
		c.Status(404)
		return
	}
	if time.Now().Unix() > row.ExpiresAt+3600 {
		c.Status(410)
		return
	}
	p, e := row.Decode()
	if e != nil {
		c.Status(500)
		return
	}
	if teamsvoice.DefaultVerifier.Verify(c.Request.Context(), strings.TrimPrefix(auth, "Bearer "), p.Config) != nil {
		c.Status(401)
		return
	}
	raw, e := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, 256*1024))
	if e != nil {
		c.Status(413)
		return
	}
	if e = teamsvoice.Callback(models.DB(rt.Ctx), id, raw, time.Now()); e != nil {
		c.Status(400)
		return
	}
	c.Status(200)
}
func (rt *Router) teamsVoiceAudio(c *gin.Context) {
	id := c.Param("id")
	if _, e := uuid.Parse(id); e != nil {
		c.Status(404)
		return
	}
	var row models.TeamsVoiceCall
	if models.DB(rt.Ctx).First(&row, "id=?", id).Error != nil || row.AudioToken == "" || time.Now().Unix() >= row.ExpiresAt || subtle.ConstantTimeCompare([]byte(row.AudioToken), []byte(c.Query("token"))) != 1 {
		c.Status(404)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.Header("Content-Type", "audio/wav")
	c.Header("X-Content-Type-Options", "nosniff")
	c.File(filepath.Join("data/teams-voice", row.ID+".wav"))
}
