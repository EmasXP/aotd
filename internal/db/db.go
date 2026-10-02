// Package db opens the database and migrates the schema.
package db

import (
	"fmt"
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
	if err := g.AutoMigrate(&model.Release{}); err != nil {
		return err
	}
	if err := moveAlbumsToReleases(g); err != nil {
		return fmt.Errorf("move albums to releases: %w", err)
	}
	return g.AutoMigrate(model.All()...)
}

// legacyPost is the posts table from before releases, when every post held
// its own copy of the album.
type legacyPost struct {
	ID        uint
	MBID      *string
	Title     string
	Artist    string
	Year      int
	CoverURL  string
	ReleaseID *uint
}

func (legacyPost) TableName() string { return "posts" }

// moveAlbumsToReleases gives every legacy post a release (one per MBID, one
// per manual entry) and drops the album columns from posts. It does nothing
// on a new or already migrated database.
func moveAlbumsToReleases(g *gorm.DB) error {
	if !g.Migrator().HasColumn(&legacyPost{}, "title") {
		return nil
	}
	return g.Transaction(func(tx *gorm.DB) error {
		m := tx.Migrator()
		if !m.HasColumn(&legacyPost{}, "release_id") {
			if err := m.AddColumn(&legacyPost{}, "ReleaseID"); err != nil {
				return err
			}
		}
		var releases []model.Release
		if err := tx.Where("mb_id IS NOT NULL").Find(&releases).Error; err != nil {
			return err
		}
		byMBID := map[string]uint{}
		for _, r := range releases {
			byMBID[*r.MBID] = r.ID
		}
		var posts []legacyPost
		if err := tx.Where("release_id IS NULL").Order("id DESC").Find(&posts).Error; err != nil {
			return err
		}
		// Newest first, so a release takes the latest metadata seen for its MBID.
		for _, p := range posts {
			var id uint
			if p.MBID != nil {
				id = byMBID[*p.MBID]
			}
			if id == 0 {
				r := model.Release{MBID: p.MBID, Title: p.Title, Artist: p.Artist, Year: p.Year, CoverURL: p.CoverURL}
				if err := tx.Create(&r).Error; err != nil {
					return err
				}
				id = r.ID
				if p.MBID != nil {
					byMBID[*p.MBID] = id
				}
			}
			if err := tx.Model(&legacyPost{}).Where("id = ?", p.ID).Update("release_id", id).Error; err != nil {
				return err
			}
		}
		if m.HasIndex(&legacyPost{}, "idx_posts_mb_id") {
			if err := m.DropIndex(&legacyPost{}, "idx_posts_mb_id"); err != nil {
				return err
			}
		}
		for _, col := range []string{"mb_id", "title", "artist", "year", "cover_url"} {
			if err := m.DropColumn(&legacyPost{}, col); err != nil {
				return err
			}
		}
		// Every post has a release now. AutoMigrate won't tighten an existing
		// column to NOT NULL, so do it here.
		return m.AlterColumn(&model.Post{}, "ReleaseID")
	})
}
