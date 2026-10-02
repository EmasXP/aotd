// Package model holds the GORM models.
package model

import (
	"time"

	"gorm.io/gorm"

	"github.com/EmasXP/aotd/internal/links"
)

type User struct {
	ID           uint   `gorm:"primaryKey"`
	Username     string `gorm:"size:30;uniqueIndex;not null"`  // lowercase
	Email        string `gorm:"size:254;uniqueIndex;not null"` // lowercase
	PasswordHash string `gorm:"not null"`
	DisplayName  string `gorm:"size:60;not null"`
	Bio          string `gorm:"size:280"`
	AvatarPath   string `gorm:"size:100"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Name returns the display name, falling back to the username.
func (u User) Name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

type Session struct {
	ID         uint      `gorm:"primaryKey"`
	UserID     uint      `gorm:"index;not null"`
	TokenHash  []byte    `gorm:"uniqueIndex;not null"`
	ExpiresAt  time.Time `gorm:"index"`
	CreatedAt  time.Time
	LastSeenAt time.Time
}

type Follow struct {
	FollowerID uint `gorm:"primaryKey"`
	FolloweeID uint `gorm:"primaryKey;index"`
	CreatedAt  time.Time
}

// Release is an album as AOTD knows it, shared by every post of it. MBID is
// nil until it's matched to a MusicBrainz release group.
type Release struct {
	ID       uint    `gorm:"primaryKey"`
	MBID     *string `gorm:"size:36;uniqueIndex"` // MusicBrainz release-group
	Title    string  `gorm:"size:300;not null"`
	Artist   string  `gorm:"size:300;not null"`
	Year     int
	CoverURL string `gorm:"size:500"`
	// CheckedAt is when MusicBrainz was last asked about this release: for
	// its streaming links, or for an MBID if it has none.
	CheckedAt *time.Time `gorm:"index"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ReleaseLink is a release's album page on a streaming or download service.
// A release can have several per source (remasters, deluxe and regional
// editions), but each one belongs to a single release.
type ReleaseLink struct {
	ID         uint    `gorm:"primaryKey"`
	ReleaseID  uint    `gorm:"not null;index"`
	Release    Release `gorm:"constraint:OnDelete:CASCADE"`
	Source     string  `gorm:"size:20;not null;uniqueIndex:idx_link"`  // see package links
	ExternalID string  `gorm:"size:200;not null;uniqueIndex:idx_link"` // the album's ID on the source
	CreatedAt  time.Time
}

func (l ReleaseLink) Link() links.Link { return links.Link{Source: l.Source, ID: l.ExternalID} }

// Post is one Album Of The Day.
type Post struct {
	ID        uint      `gorm:"primaryKey"`
	UserID    uint      `gorm:"not null;uniqueIndex:idx_user_day"`
	User      User      `gorm:"constraint:OnDelete:CASCADE"`
	PostDate  string    `gorm:"size:10;not null;uniqueIndex:idx_user_day;index"` // YYYY-MM-DD, CET
	ReleaseID uint      `gorm:"not null;index"`
	Release   Release   `gorm:"constraint:OnDelete:RESTRICT"`
	Note      string    `gorm:"size:500"`
	CreatedAt time.Time `gorm:"index"`
	UpdatedAt time.Time
}

type CheckIn struct {
	UserID    uint      `gorm:"primaryKey"`
	PostID    uint      `gorm:"primaryKey;index"`
	CreatedAt time.Time `gorm:"index"`
}

type Comment struct {
	ID        uint   `gorm:"primaryKey"`
	PostID    uint   `gorm:"index;not null"`
	UserID    uint   `gorm:"not null"`
	User      User   `gorm:"constraint:OnDelete:CASCADE"`
	ParentID  *uint  `gorm:"index"` // nil for top-level; replies always point at a top-level comment
	Body      string `gorm:"size:1000;not null"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt gorm.DeletedAt `gorm:"index"`
}

type Group struct {
	ID          uint   `gorm:"primaryKey"`
	Slug        string `gorm:"size:40;uniqueIndex;not null"`
	Name        string `gorm:"size:60;not null"`
	NameLower   string `gorm:"size:60;index"`
	Description string `gorm:"size:500"`
	OwnerID     uint   `gorm:"not null"`
	CreatedAt   time.Time
}

const (
	RoleOwner  = "owner"
	RoleMember = "member"
)

type GroupMember struct {
	GroupID  uint   `gorm:"primaryKey"`
	UserID   uint   `gorm:"primaryKey;index"`
	Role     string `gorm:"size:10;not null"`
	JoinedAt time.Time
}

// All lists every model for AutoMigrate.
func All() []any {
	return []any{&User{}, &Session{}, &Follow{}, &Release{}, &ReleaseLink{}, &Post{}, &CheckIn{}, &Comment{}, &Group{}, &GroupMember{}}
}
