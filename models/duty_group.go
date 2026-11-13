package models

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
)

type DutyPlan struct {
	EffectiveDate string  `json:"effective_date"`
	EffectiveAt   int64   `json:"effective_at,omitempty"`
	RotationDays  int     `json:"rotation_days"`
	UserIDs       []int64 `json:"user_ids"`
}
type DutyOverride struct {
	Date   string `json:"date"`
	UserID int64  `json:"user_id"`
	Reason string `json:"reason"`
}

// DutyStep目前仅保存升级配置，尚无消费这些级别的自研升级执行器。
// 后续执行须区分未认领超时和平台明确失败，不能因普通邮件/群消息失败直接触发电话升级。
type DutyStep struct {
	WaitMinutes int     `json:"wait_minutes"`
	UserIDs     []int64 `json:"user_ids"`
}
type DutyEndpoint struct {
	Kind       string `json:"kind"`
	URL        string `json:"url"`
	Secret     string `json:"secret"`
	Configured bool   `json:"configured"`
	Clear      bool   `json:"clear,omitempty"`
}
type DutyFallback struct {
	SMTPChannelID int64  `json:"smtp_channel_id,omitempty"`
	Kind          string `json:"kind"`
	Target        string `json:"target"`
	Secret        string `json:"secret"`
	Configured    bool   `json:"configured"`
	Clear         bool   `json:"clear,omitempty"`
}
type DutyConfig struct {
	ReminderInterval int            `json:"reminder_interval"`
	ReminderUnit     string         `json:"reminder_unit"`
	HourlyReminder   bool           `json:"hourly_reminder"`
	MemberIDs        []int64        `json:"member_ids"`
	Timezone         string         `json:"timezone"`
	Plans            []DutyPlan     `json:"plans"`
	Overrides        []DutyOverride `json:"overrides"`
	Steps            []DutyStep     `json:"steps"`
	Endpoints        []DutyEndpoint `json:"endpoints"`
	Fallback         DutyFallback   `json:"fallback"`
}
type DutyGroup struct {
	RotationChange *RotationChange `json:"rotation_change,omitempty" gorm:"-"`
	ID             int64           `json:"id" gorm:"primaryKey"`
	Name           string          `json:"name" gorm:"size:80;uniqueIndex"`
	Config         DutyConfig      `json:"config" gorm:"serializer:json;type:text"`
	Secrets        string          `json:"-" gorm:"type:text"`
	Version        int64           `json:"version"`
	UpdatedAt      int64           `json:"updated_at"`
	UpdatedBy      string          `json:"updated_by"`
}

func (DutyGroup) TableName() string { return "response_duty_group" }

type DutyAudit struct {
	ID        int64  `json:"id" gorm:"primaryKey"`
	GroupID   int64  `json:"group_id" gorm:"index"`
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	Version   int64  `json:"version"`
	CreatedAt int64  `json:"created_at"`
}

func (DutyAudit) TableName() string { return "response_duty_audit" }

type DutySystemSettings struct {
	ID       int64        `json:"id" gorm:"primaryKey"`
	Fallback DutyFallback `json:"fallback" gorm:"serializer:json;type:text"`
	Version  int64        `json:"version"`
	Secrets  string       `json:"-" gorm:"type:text"`
}

func (DutySystemSettings) TableName() string { return "response_duty_settings" }

var dutyAEAD cipher.AEAD

// InitDutySchema 先加载通知加密密钥，再迁移二次开发表；已有密文但缺密钥时拒绝创建新密钥。
// 它是启动初始化，不负责创建示例排班或发送消息；SQL生成与运行迁移共用 DutySchemaModels。
func InitDutySchema(db *gorm.DB, keyPath string) error {
	key, err := os.ReadFile(keyPath)
	if os.IsNotExist(err) {
		var count int64
		if db.Migrator().HasTable(&DutyGroup{}) {
			if err = db.Model(&DutyGroup{}).Where("secrets <> ''").Count(&count).Error; err != nil {
				return err
			}
		}
		if db.Migrator().HasTable(&DutySystemSettings{}) {
			var n int64
			if err = db.Model(&DutySystemSettings{}).Where("secrets <> ''").Count(&n).Error; err != nil {
				return err
			}
			count += n
		}
		if count > 0 {
			return errors.New("值班通知密钥文件缺失，请恢复配套密钥")
		}
		if err = os.MkdirAll(filepath.Dir(keyPath), 0700); err != nil {
			return err
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return err
		}
		f, e := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		_, err = f.Write(key)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	} else if err != nil {
		return err
	}
	if len(key) != 32 {
		return errors.New("值班通知密钥长度错误")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	dutyAEAD, err = cipher.NewGCM(block)
	if err != nil {
		return err
	}
	return db.AutoMigrate(DutySchemaModels()...)
}

// DutySchemaModels is shared by runtime migration and the checked-in SQL generator.
func DutySchemaModels() []interface{} {
	return []interface{}{&DutyGroup{}, &DutyAudit{}, &DutySystemSettings{}, &EventResponse{}, &EventResponseLog{}, &ClaimNotice{}, &DutyNotice{}, &DutyReminderClock{}}
}

func sealDuty(v any) (string, error) {
	if dutyAEAD == nil {
		return "", errors.New("值班通知密钥未初始化")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, dutyAEAD.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(dutyAEAD.Seal(nonce, nonce, raw, []byte("duty-v1"))), nil
}
func openDuty(raw string, v any) error {
	if raw == "" {
		return nil
	}
	if dutyAEAD == nil {
		return errors.New("值班通知密钥未初始化")
	}
	data, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(data) < dutyAEAD.NonceSize() {
		return errors.New("值班配置解密失败")
	}
	n := dutyAEAD.NonceSize()
	plain, err := dutyAEAD.Open(nil, data[:n], data[n:], []byte("duty-v1"))
	if err != nil {
		return errors.New("值班配置解密失败")
	}
	return json.Unmarshal(plain, v)
}

type dutySecrets struct {
	Endpoints []DutyEndpoint `json:"endpoints"`
	Fallback  DutyFallback   `json:"fallback"`
}

func (g DutyGroup) PrivateConfig() (DutyConfig, error) {
	c := g.Config
	var secrets dutySecrets
	if err := openDuty(g.Secrets, &secrets); err != nil {
		return c, err
	}
	c.Endpoints = secrets.Endpoints
	c.Fallback = secrets.Fallback
	return c, nil
}
func validDutyURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.Fragment == "" && len(raw) <= 4096
}
func validateDutyFallback(f DutyFallback) error {
	switch f.Kind {
	case "", "inherit", "none":
		return nil
	case "email":
		a, e := mail.ParseAddress(f.Target)
		if e != nil || a.Address != f.Target {
			return errors.New("兜底邮箱格式无效")
		}
	case "dingtalk", "feishu", "webhook":
		if !validDutyURL(f.Target) {
			return errors.New("兜底地址必须为HTTP(S)地址")
		}
	default:
		return errors.New("兜底方式无效")
	}
	return nil
}
func activeDutyUser(db *gorm.DB, id int64) error {
	var u User
	if id <= 0 || db.First(&u, id).Error != nil || u.IsDisabled() {
		return fmt.Errorf("账号%d不存在或已停用", id)
	}
	return nil
}
func uniqueDutyIDs(ids []int64) bool {
	m := map[int64]bool{}
	for _, id := range ids {
		if id <= 0 || m[id] {
			return false
		}
		m[id] = true
	}
	return true
}
func (g *DutyGroup) Validate(db *gorm.DB) error {
	g.Name = strings.TrimSpace(g.Name)
	if len([]rune(g.Name)) < 1 || len([]rune(g.Name)) > 80 {
		return errors.New("名称须为1至80字")
	}
	c := &g.Config
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return errors.New("时区无效")
	}
	if len(c.MemberIDs) == 0 || len(c.MemberIDs) > 500 || !uniqueDutyIDs(c.MemberIDs) {
		return errors.New("请选择1至500名不重复的排班人员")
	}
	members := map[int64]bool{}
	for _, id := range c.MemberIDs {
		if err := activeDutyUser(db, id); err != nil {
			return err
		}
		members[id] = true
	}
	if len(c.Plans) == 0 || len(c.Plans) > 10000 {
		return errors.New("轮班规则数量无效")
	}
	loc, _ := time.LoadLocation(c.Timezone)
	sort.Slice(c.Plans, func(i, j int) bool { return dutyPlanStart(c.Plans[i], loc).Before(dutyPlanStart(c.Plans[j], loc)) })
	starts := map[int64]bool{}
	current := activeDutyPlan(*c, time.Now())
	for i, p := range c.Plans {
		start := dutyPlanStart(p, loc)
		if _, e := time.Parse("2006-01-02", p.EffectiveDate); e != nil || starts[start.Unix()] || p.EffectiveAt < 0 || start.Format("2006-01-02") != p.EffectiveDate {
			return errors.New("规则生效时间无效或重复")
		}
		starts[start.Unix()] = true
		if p.RotationDays < 1 || p.RotationDays > 365 || len(p.UserIDs) == 0 || !uniqueDutyIDs(p.UserIDs) {
			return errors.New("轮换周期须1至365天且必须有不重复的参与人员")
		}
		// Retired versions may refer to former members; do not rewrite their history.
		if current != nil && dutyPlanStart(c.Plans[i], loc).Before(dutyPlanStart(*current, loc)) {
			continue
		}
		for _, id := range p.UserIDs {
			if !members[id] {
				return errors.New("当前及待生效规则的人员必须在排班成员中")
			}
		}
	}
	dates := map[string]bool{}
	for _, o := range c.Overrides {
		if _, e := time.Parse("2006-01-02", o.Date); e != nil || dates[o.Date] || o.Date < c.Plans[0].EffectiveDate {
			return errors.New("替班日期无效、重复或早于计划")
		}
		dates[o.Date] = true
		if !members[o.UserID] || strings.TrimSpace(o.Reason) == "" || len(o.Reason) > 3000 {
			return errors.New("替班人须在成员中且必须填写原因")
		}
	}
	if len(c.Steps) > 8 {
		return errors.New("最多配置8级上报")
	}
	for _, st := range c.Steps {
		if st.WaitMinutes < 1 || st.WaitMinutes > 1440 || len(st.UserIDs) == 0 || len(st.UserIDs) > 100 || !uniqueDutyIDs(st.UserIDs) {
			return errors.New("每级须1至1440分钟及不重复的接收人")
		}
		for _, id := range st.UserIDs {
			if err := activeDutyUser(db, id); err != nil {
				return err
			}
		}
	}
	kinds := map[string]bool{}
	for _, ep := range c.Endpoints {
		if ep.Kind != "dingtalk" && ep.Kind != "feishu" && ep.Kind != "webhook" {
			return errors.New("公共通知渠道无效")
		}
		if kinds[ep.Kind] || !validDutyURL(ep.URL) || len(ep.Secret) > 1024 {
			return errors.New("公共通知地址无效或重复")
		}
		kinds[ep.Kind] = true
		u, _ := url.Parse(ep.URL)
		if ep.Kind == "dingtalk" && (u.Scheme != "https" || u.Hostname() != "oapi.dingtalk.com" || u.Path != "/robot/send" || u.Query().Get("access_token") == "") {
			return errors.New("请填写完整钉钉机器人地址")
		}
		if ep.Kind == "feishu" && (u.Scheme != "https" || u.Hostname() != "open.feishu.cn" || !strings.HasPrefix(u.Path, "/open-apis/bot/v2/hook/")) {
			return errors.New("请填写完整飞书机器人地址")
		}
	}
	if c.ReminderInterval == 0 && c.ReminderUnit == "" {
		c.ReminderInterval = 1
		c.ReminderUnit = "hour"
	}
	if c.ReminderInterval < 1 || (c.ReminderUnit != "minute" && c.ReminderUnit != "hour") || c.ReminderInterval > 1440 || (c.ReminderUnit == "hour" && c.ReminderInterval > 24) {
		return errors.New("值班提醒间隔须为1至1440分钟或1至24小时")
	}
	if c.HourlyReminder && len(c.Endpoints) == 0 {
		return errors.New("开启值班提醒须至少配置一个组公共通知渠道")
	}
	return validateDutyFallback(c.Fallback)
}

var ErrDutyConflict = errors.New("配置已更新，请刷新后重试")

// SaveDutyGroup 在同一事务内处理轮班变更、校验、密钥加密、版本更新、审计及提醒时钟。
// 回显隐藏的地址/密钥留空表示保留，Clear 才是显式清除；不能直接用前端回显覆盖密文配置。
func SaveDutyGroup(db *gorm.DB, g *DutyGroup, actor string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var old DutyGroup
		oldConfig := DutyConfig{}
		if g.ID > 0 {
			if err := tx.First(&old, g.ID).Error; err != nil {
				return err
			}
			if old.Version != g.Version {
				return ErrDutyConflict
			}
			var err error
			oldConfig, err = old.PrivateConfig()
			if err != nil {
				return err
			}
		}
		if err := PrepareDutyRotation(g, old, time.Now()); err != nil {
			return err
		}
		for i := range g.Config.Endpoints {
			e := &g.Config.Endpoints[i]
			if e.Clear {
				continue
			}
			for _, prev := range oldConfig.Endpoints {
				if prev.Kind == e.Kind {
					if e.URL == "" {
						e.URL = prev.URL
					}
					if e.Secret == "" {
						e.Secret = prev.Secret
					}
				}
			}
		}
		eps := []DutyEndpoint{}
		for _, ep := range g.Config.Endpoints {
			if !ep.Clear {
				eps = append(eps, ep)
			}
		}
		g.Config.Endpoints = eps
		f := &g.Config.Fallback
		if f.Clear {
			*f = DutyFallback{Kind: "inherit"}
		} else if f.Kind == oldConfig.Fallback.Kind {
			if f.Target == "" {
				f.Target = oldConfig.Fallback.Target
			}
			if f.Secret == "" {
				f.Secret = oldConfig.Fallback.Secret
			}
		}
		if err := validateDutyEmailChannel(tx, g.Config.Fallback); err != nil {
			return err
		}
		if err := g.Validate(tx); err != nil {
			return err
		}
		encrypted, err := sealDuty(dutySecrets{g.Config.Endpoints, g.Config.Fallback})
		if err != nil {
			return err
		}
		g.Secrets = encrypted
		for i := range g.Config.Endpoints {
			g.Config.Endpoints[i].URL = ""
			g.Config.Endpoints[i].Secret = ""
			g.Config.Endpoints[i].Configured = true
		}
		g.Config.Fallback.Target = ""
		g.Config.Fallback.Secret = ""
		g.Config.Fallback.Configured = g.Config.Fallback.Kind != "" && g.Config.Fallback.Kind != "inherit" && g.Config.Fallback.Kind != "none"
		g.Version = old.Version + 1
		g.UpdatedAt = time.Now().Unix()
		g.UpdatedBy = actor
		if g.ID == 0 {
			err = tx.Create(g).Error
		} else {
			res := tx.Model(&DutyGroup{}).Where("id=? AND version=?", g.ID, old.Version).Updates(map[string]any{"name": g.Name, "config": mustDutyJSON(g.Config), "secrets": g.Secrets, "version": g.Version, "updated_at": g.UpdatedAt, "updated_by": actor})
			err = res.Error
			if err == nil && res.RowsAffected != 1 {
				return ErrDutyConflict
			}
		}
		if err != nil {
			return err
		}
		if err := SyncDutyReminderClock(tx, *g, old); err != nil {
			return err
		}
		return tx.Create(&DutyAudit{GroupID: g.ID, Actor: actor, Action: "save", Version: g.Version, CreatedAt: g.UpdatedAt}).Error
	})
}
func mustDutyJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

type DutyResolution struct {
	GroupID   int64  `json:"group_id"`
	At        string `json:"at"`
	UserID    int64  `json:"user_id"`
	Name      string `json:"name"`
	Source    string `json:"source"`
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
	Version   int64  `json:"version"`
}

// ResolveDuty 计算指定时刻的当班人：有效计划 → 按组时区轮换 → 当日替班覆盖 → 账号可用性检查。
// 这是排班解析，既不认领告警也不改变已有负责人；换班后已认领事件仍归原负责人。
func ResolveDuty(db *gorm.DB, g DutyGroup, at time.Time) (DutyResolution, error) {
	r := DutyResolution{GroupID: g.ID, At: at.Format(time.RFC3339), Version: g.Version}
	loc, err := time.LoadLocation(g.Config.Timezone)
	if err != nil {
		return r, err
	}
	day := at.In(loc).Format("2006-01-02")
	plan := activeDutyPlan(g.Config, at)
	if plan == nil {
		r.Reason = "计划尚未生效"
		return r, nil
	}
	if len(plan.UserIDs) == 0 || plan.RotationDays < 1 {
		return r, errors.New("排班计划无效")
	}
	// Calendar-day arithmetic in UTC avoids 23/25-hour DST days shifting the roster.
	start, e := time.Parse("2006-01-02", plan.EffectiveDate)
	if e != nil {
		return r, e
	}
	end, e := time.Parse("2006-01-02", day)
	if e != nil {
		return r, e
	}
	days := int(end.Sub(start).Hours() / 24)
	r.UserID = plan.UserIDs[(days/plan.RotationDays)%len(plan.UserIDs)]
	r.Source = "rotation"
	for _, o := range g.Config.Overrides {
		if o.Date == day {
			r.UserID = o.UserID
			r.Source = "override"
			break
		}
	}
	var u User
	if db.First(&u, r.UserID).Error != nil || u.IsDisabled() {
		r.Reason = "当班账号不存在或已停用，需要调整排班"
		return r, nil
	}
	r.Name = u.Nickname
	if r.Name == "" {
		r.Name = u.Username
	}
	r.Available = true
	return r, nil
}
func SaveDutySettings(db *gorm.DB, s *DutySystemSettings, actor string) error {
	return db.Transaction(func(tx *gorm.DB) error {
		var old DutySystemSettings
		err := tx.First(&old, 1).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if s.Version != old.Version {
			return ErrDutyConflict
		}
		var prev DutyFallback
		if err = openDuty(old.Secrets, &prev); err != nil {
			return err
		}
		if s.Fallback.Clear {
			s.Fallback = DutyFallback{Kind: "none"}
		} else if s.Fallback.Kind == prev.Kind {
			if s.Fallback.Target == "" {
				s.Fallback.Target = prev.Target
			}
			if s.Fallback.Secret == "" {
				s.Fallback.Secret = prev.Secret
			}
		}
		if s.Fallback.Kind == "inherit" {
			return errors.New("系统兜底不能继承自身")
		}
		if err = validateDutyEmailChannel(tx, s.Fallback); err != nil {
			return err
		}
		if err = validateDutyFallback(s.Fallback); err != nil {
			return err
		}
		s.Secrets, err = sealDuty(s.Fallback)
		if err != nil {
			return err
		}
		s.ID = 1
		s.Version = old.Version + 1
		s.Fallback.Target = ""
		s.Fallback.Secret = ""
		s.Fallback.Configured = s.Fallback.Kind != "" && s.Fallback.Kind != "none"
		if old.Version == 0 {
			err = tx.Create(s).Error
		} else {
			res := tx.Model(&DutySystemSettings{}).Where("id=1 AND version=?", old.Version).Updates(map[string]any{"fallback": mustDutyJSON(s.Fallback), "secrets": s.Secrets, "version": s.Version})
			err = res.Error
			if err == nil && res.RowsAffected != 1 {
				return ErrDutyConflict
			}
		}
		if err != nil {
			return err
		}
		return tx.Create(&DutyAudit{Actor: actor, Action: "system_fallback", Version: s.Version, CreatedAt: time.Now().Unix()}).Error
	})
}
