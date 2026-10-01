// Package db opens the database and migrates the schema.
package db

import (
	"log"
	"os"
	"strings"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/EmasXP/aotd/internal/model"
)

// Open connects to Postgres when dsn is a postgres:// URL, otherwise to SQLite.
func Open(dsn string) (*gorm.DB, error) {
	lg := logger.New(log.New(os.Stderr, "", log.LstdFlags), logger.Config{
		SlowThreshold:             200 * time.Millisecond,
		LogLevel:                  logger.Warn,
		IgnoreRecordNotFoundError: true, // lookups that miss are normal here
	})
	cfg := &gorm.Config{Logger: lg, TranslateError: true}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		return gorm.Open(postgres.Open(dsn), cfg)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	dsn += sep + "_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	g, err := gorm.Open(sqlite.Open(dsn), cfg)
	if err != nil {
		return nil, err
	}
	if strings.Contains(dsn, ":memory:") {
		// Each new connection to :memory: is a separate database.
		sqlDB, err := g.DB()
		if err != nil {
			return nil, err
		}
		sqlDB.SetMaxOpenConns(1)
	}
	return g, nil
}

func Migrate(g *gorm.DB) error {
	return g.AutoMigrate(model.All()...)
}
