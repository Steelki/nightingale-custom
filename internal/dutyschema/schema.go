// Package dutyschema renders custom schema SQL without connecting to a server.
package dutyschema

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/models/migrate"
	"github.com/glebarez/sqlite"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type recorder struct {
	logger.Interface
	statements []string
}

func (r *recorder) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	r.statements = append(r.statements, sql+";")
}

// Render creates DDL using the same GORM dialects and models as startup.
func Render(dialect string, notifyRule bool) (string, error) {
	local, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		return "", err
	}
	conn, err := local.DB()
	if err != nil {
		return "", err
	}
	defer conn.Close()
	var d gorm.Dialector
	switch dialect {
	case "sqlite":
		d = sqlite.Dialector{Conn: conn}
	case "mysql":
		d = mysql.New(mysql.Config{Conn: conn, SkipInitializeWithVersion: true})
	case "postgres":
		d = postgres.New(postgres.Config{Conn: conn})
	default:
		return "", fmt.Errorf("unsupported dialect %q", dialect)
	}
	log := &recorder{Interface: logger.Default.LogMode(logger.Silent)}
	db, err := gorm.Open(d, &gorm.Config{DryRun: true, DisableAutomaticPing: true, Logger: log})
	if err != nil {
		return "", err
	}
	values := models.DutySchemaModels()
	if notifyRule {
		values = []interface{}{&migrate.NotifyRule{}}
	}
	if dialect == "mysql" {
		db = db.Set("gorm:table_options", "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4")
	}
	if err = db.Migrator().CreateTable(values...); err != nil {
		return "", err
	}
	// GORM indexes are held in a map; stabilize emitted order for drift checks.
	if dialect == "mysql" {
		re := regexp.MustCompile(",((?:UNIQUE )?INDEX `[^`]+` \\([^)]*\\))")
		for i, sql := range log.statements {
			matches := re.FindAllString(sql, -1)
			sort.Strings(matches)
			sql = re.ReplaceAllString(sql, "")
			at := strings.LastIndex(sql, ")ENGINE=")
			if at >= 0 {
				sql = sql[:at] + strings.Join(matches, "") + sql[at:]
			}
			log.statements[i] = sql
		}
	}
	sort.Slice(log.statements, func(i, j int) bool {
		a, b := strings.HasPrefix(log.statements[i], "CREATE TABLE"), strings.HasPrefix(log.statements[j], "CREATE TABLE")
		if a != b {
			return a
		}
		return log.statements[i] < log.statements[j]
	})
	return strings.Join(log.statements, "\n") + "\n", nil
}
