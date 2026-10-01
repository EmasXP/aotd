package store

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/EmasXP/aotd/internal/model"
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(name string) string {
	s := nonSlug.ReplaceAllString(strings.ToLower(name), "-")
	s = strings.Trim(s, "-")
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	if s == "" {
		s = "group"
	}
	return s
}

func validateGroup(name, description string) (string, string, error) {
	name = strings.TrimSpace(name)
	description = strings.TrimSpace(description)
	if n := utf8.RuneCountInString(name); n < 2 || n > 60 {
		return "", "", invalid("Group name must be 2–60 characters.")
	}
	if utf8.RuneCountInString(description) > 500 {
		return "", "", invalid("Description must be at most 500 characters.")
	}
	return name, description, nil
}

// CreateGroup creates a group owned (and joined) by ownerID. The slug is
// derived from the name, with a numeric suffix if it's taken.
func (s *Store) CreateGroup(ownerID uint, name, description string) (*model.Group, error) {
	name, description, err := validateGroup(name, description)
	if err != nil {
		return nil, err
	}
	base := slugify(name)
	for i := 1; i <= 50; i++ {
		slug := base
		if i > 1 {
			slug += "-" + strconv.Itoa(i)
		}
		g := &model.Group{Slug: slug, Name: name, NameLower: strings.ToLower(name), Description: description, OwnerID: ownerID}
		err := s.DB.Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(g).Error; err != nil {
				return err
			}
			return tx.Create(&model.GroupMember{GroupID: g.ID, UserID: ownerID, Role: model.RoleOwner, JoinedAt: time.Now()}).Error
		})
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			continue
		}
		return g, err
	}
	return nil, invalid("Couldn't find a free URL for that name. Try another.")
}

func (s *Store) GroupBySlug(slug string) (*model.Group, error) {
	var g model.Group
	return &g, notFound(s.DB.Where("slug = ?", slug).First(&g).Error)
}

func (s *Store) UpdateGroup(userID, groupID uint, name, description string) error {
	name, description, err := validateGroup(name, description)
	if err != nil {
		return err
	}
	res := s.DB.Model(&model.Group{}).Where("id = ? AND owner_id = ?", groupID, userID).
		Updates(map[string]any{"name": name, "name_lower": strings.ToLower(name), "description": description})
	if res.Error == nil && res.RowsAffected == 0 {
		return ErrForbidden
	}
	return res.Error
}

func (s *Store) DeleteGroup(userID, groupID uint) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Where("id = ? AND owner_id = ?", groupID, userID).Delete(&model.Group{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrForbidden
		}
		return tx.Where("group_id = ?", groupID).Delete(&model.GroupMember{}).Error
	})
}

func (s *Store) JoinGroup(userID, groupID uint) error {
	m := model.GroupMember{GroupID: groupID, UserID: userID, Role: model.RoleMember, JoinedAt: time.Now()}
	return s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&m).Error
}

// LeaveGroup removes userID from the group. Owners can't leave; they delete
// the group instead.
func (s *Store) LeaveGroup(userID, groupID uint) error {
	return s.DB.Where("group_id = ? AND user_id = ? AND role <> ?", groupID, userID, model.RoleOwner).
		Delete(&model.GroupMember{}).Error
}

func (s *Store) IsMember(userID, groupID uint) bool {
	var n int64
	s.DB.Model(&model.GroupMember{}).Where("group_id = ? AND user_id = ?", groupID, userID).Count(&n)
	return n > 0
}

func (s *Store) GroupMembers(groupID uint) ([]model.User, error) {
	var us []model.User
	err := s.DB.Joins("JOIN group_members ON group_members.user_id = users.id").
		Where("group_members.group_id = ?", groupID).Order("group_members.joined_at").Find(&us).Error
	return us, err
}

// GroupSummary is a group with its member count, for lists.
type GroupSummary struct {
	model.Group
	Members int64
}

func (s *Store) summaries(q *gorm.DB) ([]GroupSummary, error) {
	var gs []GroupSummary
	err := q.Model(&model.Group{}).
		Select("groups.*, (SELECT COUNT(*) FROM group_members gm WHERE gm.group_id = groups.id) AS members").
		Find(&gs).Error
	return gs, err
}

// UserGroups lists the groups userID belongs to.
func (s *Store) UserGroups(userID uint) ([]GroupSummary, error) {
	return s.summaries(s.DB.Where("groups.id IN (SELECT group_id FROM group_members WHERE user_id = ?)", userID).Order("groups.name_lower"))
}

// PopularGroups lists groups by member count.
func (s *Store) PopularGroups(limit int) ([]GroupSummary, error) {
	return s.summaries(s.DB.Order("members DESC, groups.name_lower").Limit(limit))
}

func (s *Store) SearchGroups(q string, limit int) ([]GroupSummary, error) {
	if strings.TrimSpace(q) == "" {
		return nil, nil
	}
	p := likePattern(q)
	return s.summaries(s.DB.Where(`groups.name_lower LIKE ? ESCAPE '\' OR groups.slug LIKE ? ESCAPE '\'`, p, p).
		Order("groups.name_lower").Limit(limit))
}
