package store

import (
	"errors"
	"net/mail"
	"regexp"
	"slices"
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
	m, err := s.usersByID([]uint{id})
	if err != nil {
		return nil, err
	}
	u, ok := m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return &u, nil
}

// UserByUsername looks the ID up by username, which never changes.
func (s *Store) UserByUsername(username string) (*model.User, error) {
	username = strings.ToLower(username)
	id, err := cache.FetchKey(s.Cache, "username:"+username, 24*time.Hour, func() (uint, error) {
		var u model.User
		err := s.DB.Select("id").Where("username = ?", username).First(&u).Error
		return u.ID, notFound(err)
	})
	if err != nil {
		return &model.User{}, err
	}
	return s.UserByID(id)
}

// UserByLogin finds a user by username or email. Not cached: logging in
// should see the current password hash.
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
	err := s.DB.Model(&model.User{}).Where("id = ?", userID).
		Updates(map[string]any{"display_name": displayName, "bio": bio}).Error
	if err != nil {
		return err
	}
	s.invalidate("user", userID)
	return nil
}

// SetAvatar stores the new avatar name and returns the previous one.
func (s *Store) SetAvatar(userID uint, name string) (old string, err error) {
	u, err := s.UserByID(userID)
	if err != nil {
		return "", err
	}
	// Read the old name before Update, which writes the new one into u.
	old = u.AvatarPath
	if err := s.DB.Model(u).Update("avatar_path", name).Error; err != nil {
		return "", err
	}
	s.invalidate("user", userID)
	return old, nil
}

func (s *Store) SetPasswordHash(userID uint, hash string) error {
	if err := s.DB.Model(&model.User{}).Where("id = ?", userID).Update("password_hash", hash).Error; err != nil {
		return err
	}
	s.invalidate("user", userID)
	return nil
}

// --- follows ---

func (s *Store) Follow(followerID, followeeID uint) error {
	if followerID == followeeID {
		return invalid("You can't follow yourself.")
	}
	f := model.Follow{FollowerID: followerID, FolloweeID: followeeID, CreatedAt: time.Now()}
	if err := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&f).Error; err != nil {
		return err
	}
	s.followsChanged(followerID, followeeID)
	return nil
}

func (s *Store) Unfollow(followerID, followeeID uint) error {
	err := s.DB.Where("follower_id = ? AND followee_id = ?", followerID, followeeID).
		Delete(&model.Follow{}).Error
	if err != nil {
		return err
	}
	s.followsChanged(followerID, followeeID)
	return nil
}

func (s *Store) followsChanged(followerID, followeeID uint) {
	s.invalidate("follows", followerID, followeeID)
	s.invalidate("wall", followerID)
}

// followIDs returns the IDs of userID's followers, or of who they follow,
// newest first.
func (s *Store) followIDs(userID uint, followers bool) ([]uint, error) {
	key, match, pick := "following", "follower_id", "followee_id"
	if followers {
		key, match, pick = "followers", "followee_id", "follower_id"
	}
	return cache.Fetch(s.ns("follows", userID), key, ttl, func() ([]uint, error) {
		var ids []uint
		err := s.DB.Model(&model.Follow{}).Where(match+" = ?", userID).Order("created_at DESC").Pluck(pick, &ids).Error
		return ids, err
	})
}

func (s *Store) IsFollowing(followerID, followeeID uint) bool {
	ids, _ := s.followIDs(followerID, false)
	return slices.Contains(ids, followeeID)
}

type FollowCounts struct{ Followers, Following int64 }

func (s *Store) FollowCounts(userID uint) FollowCounts {
	followers, _ := s.followIDs(userID, true)
	following, _ := s.followIDs(userID, false)
	return FollowCounts{Followers: int64(len(followers)), Following: int64(len(following))}
}

type ActivityCounts struct{ Posts, CheckIns, Comments int64 }

// ActivityCounts counts a user's AOTDs, check-ins and (not deleted) comments.
// Cached; every write that changes them invalidates the user.
func (s *Store) ActivityCounts(userID uint) ActivityCounts {
	c, _ := cache.Fetch(s.ns("activity", userID), "counts", ttl, func() (ActivityCounts, error) {
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
	ids, err := s.followIDs(userID, true)
	if err != nil {
		return nil, err
	}
	return s.users(ids)
}

// Following lists users userID follows (newest first).
func (s *Store) Following(userID uint) ([]model.User, error) {
	ids, err := s.followIDs(userID, false)
	if err != nil {
		return nil, err
	}
	return s.users(ids)
}

// SuggestUsers and SearchUsers aren't cached: they're rare, and the search
// query is free text.

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
