package store

import (
	"errors"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/EmasXP/aotd/internal/cache"
	"github.com/EmasXP/aotd/internal/model"
)

var usernameRe = regexp.MustCompile(`^[a-z0-9_]{3,30}$`)

// NormalizeSignup lowercases and validates signup fields.
func NormalizeSignup(username, email, displayName string) (string, string, string, error) {
	username = strings.ToLower(strings.TrimSpace(username))
	email = strings.ToLower(strings.TrimSpace(email))
	displayName = strings.TrimSpace(displayName)
	if !usernameRe.MatchString(username) {
		return "", "", "", invalid("Username must be 3–30 characters: a–z, 0–9 and _.")
	}
	if a, err := mail.ParseAddress(email); err != nil || a.Address != email || len(email) > 254 {
		return "", "", "", invalid("Enter a valid email address.")
	}
	if displayName == "" {
		displayName = username
	}
	if utf8.RuneCountInString(displayName) > 60 {
		return "", "", "", invalid("Display name must be at most 60 characters.")
	}
	return username, email, displayName, nil
}

// CreateUser inserts a user. Fields must already be normalised.
func (s *Store) CreateUser(username, email, displayName, passwordHash string) (*model.User, error) {
	u := &model.User{Username: username, Email: email, DisplayName: displayName, PasswordHash: passwordHash}
	err := s.DB.Create(u).Error
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		// Tell which one clashed so the form can say something useful.
		var n int64
		s.DB.Model(&model.User{}).Where("username = ?", username).Count(&n)
		if n > 0 {
			return nil, ErrUsernameTaken
		}
		return nil, ErrEmailTaken
	}
	return u, err
}

func (s *Store) UserByID(id uint) (*model.User, error) {
	var u model.User
	return &u, notFound(s.DB.First(&u, id).Error)
}

func (s *Store) UserByUsername(username string) (*model.User, error) {
	var u model.User
	err := s.DB.Where("username = ?", strings.ToLower(username)).First(&u).Error
	return &u, notFound(err)
}

// UserByLogin finds a user by username or email.
func (s *Store) UserByLogin(login string) (*model.User, error) {
	login = strings.ToLower(strings.TrimSpace(login))
	var u model.User
	err := s.DB.Where("username = ? OR email = ?", login, login).First(&u).Error
	return &u, notFound(err)
}

func (s *Store) UpdateProfile(userID uint, displayName, bio string) error {
	displayName = strings.TrimSpace(displayName)
	bio = strings.TrimSpace(bio)
	if displayName == "" || utf8.RuneCountInString(displayName) > 60 {
		return invalid("Display name must be 1–60 characters.")
	}
	if utf8.RuneCountInString(bio) > 280 {
		return invalid("Bio must be at most 280 characters.")
	}
	return s.DB.Model(&model.User{}).Where("id = ?", userID).
		Updates(map[string]any{"display_name": displayName, "bio": bio}).Error
}

// SetAvatar stores the new avatar name and returns the previous one.
func (s *Store) SetAvatar(userID uint, name string) (old string, err error) {
	u, err := s.UserByID(userID)
	if err != nil {
		return "", err
	}
	// Read the old name before Update, which writes the new one into u.
	old = u.AvatarPath
	return old, s.DB.Model(u).Update("avatar_path", name).Error
}

func (s *Store) SetPasswordHash(userID uint, hash string) error {
	return s.DB.Model(&model.User{}).Where("id = ?", userID).Update("password_hash", hash).Error
}

// --- follows ---

func (s *Store) Follow(followerID, followeeID uint) error {
	if followerID == followeeID {
		return invalid("You can't follow yourself.")
	}
	f := model.Follow{FollowerID: followerID, FolloweeID: followeeID, CreatedAt: time.Now()}
	return s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&f).Error
}

func (s *Store) Unfollow(followerID, followeeID uint) error {
	return s.DB.Where("follower_id = ? AND followee_id = ?", followerID, followeeID).
		Delete(&model.Follow{}).Error
}

func (s *Store) IsFollowing(followerID, followeeID uint) bool {
	var n int64
	s.DB.Model(&model.Follow{}).Where("follower_id = ? AND followee_id = ?", followerID, followeeID).Count(&n)
	return n > 0
}

type FollowCounts struct{ Followers, Following int64 }

func (s *Store) FollowCounts(userID uint) FollowCounts {
	var c FollowCounts
	s.DB.Model(&model.Follow{}).Where("followee_id = ?", userID).Count(&c.Followers)
	s.DB.Model(&model.Follow{}).Where("follower_id = ?", userID).Count(&c.Following)
	return c
}

type ActivityCounts struct{ Posts, CheckIns, Comments int64 }

// ActivityCounts counts a user's AOTDs, check-ins and (not deleted) comments.
// Cached; every write that changes them invalidates the user.
func (s *Store) ActivityCounts(userID uint) ActivityCounts {
	c, _ := cache.Fetch(s.userNS(userID), "activity", time.Hour, func() (ActivityCounts, error) {
		var c ActivityCounts
		return c, errors.Join(
			s.DB.Model(&model.Post{}).Where("user_id = ?", userID).Count(&c.Posts).Error,
			s.DB.Model(&model.CheckIn{}).Where("user_id = ?", userID).Count(&c.CheckIns).Error,
			s.DB.Model(&model.Comment{}).Where("user_id = ?", userID).Count(&c.Comments).Error,
		)
	})
	return c
}

// Followers lists users following userID (newest first).
func (s *Store) Followers(userID uint) ([]model.User, error) {
	var us []model.User
	err := s.DB.Joins("JOIN follows ON follows.follower_id = users.id").
		Where("follows.followee_id = ?", userID).Order("follows.created_at DESC").Find(&us).Error
	return us, err
}

// Following lists users userID follows (newest first).
func (s *Store) Following(userID uint) ([]model.User, error) {
	var us []model.User
	err := s.DB.Joins("JOIN follows ON follows.followee_id = users.id").
		Where("follows.follower_id = ?", userID).Order("follows.created_at DESC").Find(&us).Error
	return us, err
}

// SuggestUsers returns some users viewerID doesn't follow yet, for the empty wall.
func (s *Store) SuggestUsers(viewerID uint, limit int) ([]model.User, error) {
	var us []model.User
	err := s.DB.Where("id <> ? AND id NOT IN (SELECT followee_id FROM follows WHERE follower_id = ?)", viewerID, viewerID).
		Order("created_at DESC").Limit(limit).Find(&us).Error
	return us, err
}

func (s *Store) SearchUsers(q string, limit int) ([]model.User, error) {
	if strings.TrimSpace(q) == "" {
		return nil, nil
	}
	p := likePattern(q)
	var us []model.User
	err := s.DB.Where(`username LIKE ? ESCAPE '\' OR LOWER(display_name) LIKE ? ESCAPE '\'`, p, p).
		Order("username").Limit(limit).Find(&us).Error
	return us, err
}
