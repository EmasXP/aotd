package store

import (
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/EmasXP/aotd/internal/cache"
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
		if err != nil {
			return nil, err
		}
		s.invalidate("memberships", ownerID)
		s.drop(s.groupsNS())
		return g, nil
	}
	return nil, invalid("Couldn't find a free URL for that name. Try another.")
}

// GroupBySlug looks the ID up by slug, which doesn't change while the group
// exists.
func (s *Store) GroupBySlug(slug string) (*model.Group, error) {
	key := "slug:" + slug
	load := func() (uint, error) {
		var g model.Group
		err := s.DB.Select("id").Where("slug = ?", slug).First(&g).Error
		return g.ID, notFound(err)
	}
	id, err := cache.FetchKey(s.Cache, key, 24*time.Hour, load)
	if err != nil {
		return nil, err
	}
	g, err := s.groupByID(id)
	if errors.Is(err, ErrNotFound) {
		// The group was deleted, and the slug may belong to a new one.
		s.Cache.Delete(key)
		if id, err = load(); err != nil {
			return nil, err
		}
		g, err = s.groupByID(id)
	}
	return g, err
}

func (s *Store) groupsByID(ids []uint) (map[uint]model.Group, error) {
	return cache.FetchMany(s.Cache, "group", ids, "row", ttl, func(missing []uint) (map[uint]model.Group, error) {
		var gs []model.Group
		err := s.DB.Where("id IN ?", missing).Find(&gs).Error
		return byID(gs, func(g model.Group) uint { return g.ID }), err
	})
}

func (s *Store) groupByID(id uint) (*model.Group, error) {
	m, err := s.groupsByID([]uint{id})
	if err != nil {
		return nil, err
	}
	g, ok := m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return &g, nil
}

func (s *Store) UpdateGroup(userID, groupID uint, name, description string) error {
	name, description, err := validateGroup(name, description)
	if err != nil {
		return err
	}
	res := s.DB.Model(&model.Group{}).Where("id = ? AND owner_id = ?", groupID, userID).
		Updates(map[string]any{"name": name, "name_lower": strings.ToLower(name), "description": description})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrForbidden
	}
	s.invalidate("group", groupID)
	s.drop(s.groupsNS()) // the name breaks ties in popular groups
	return nil
}

func (s *Store) DeleteGroup(userID, groupID uint) error {
	var g model.Group
	var members []uint
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id = ? AND owner_id = ?", groupID, userID).First(&g).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrForbidden
		} else if err != nil {
			return err
		}
		if err := tx.Model(&model.GroupMember{}).Where("group_id = ?", groupID).Pluck("user_id", &members).Error; err != nil {
			return err
		}
		if err := tx.Delete(&g).Error; err != nil {
			return err
		}
		return tx.Where("group_id = ?", groupID).Delete(&model.GroupMember{}).Error
	})
	if err != nil {
		return err
	}
	if err := s.Cache.Delete("slug:" + g.Slug); err != nil {
		slog.Warn("cache delete", "err", err)
	}
	s.invalidate("group", groupID)
	s.invalidate("groupfeed", groupID)
	s.invalidate("memberships", members...)
	s.drop(s.groupsNS())
	return nil
}

func (s *Store) JoinGroup(userID, groupID uint) error {
	m := model.GroupMember{GroupID: groupID, UserID: userID, Role: model.RoleMember, JoinedAt: time.Now()}
	if err := s.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&m).Error; err != nil {
		return err
	}
	s.membersChanged(userID, groupID)
	return nil
}

// LeaveGroup removes userID from the group. Owners can't leave; they delete
// the group instead.
func (s *Store) LeaveGroup(userID, groupID uint) error {
	err := s.DB.Where("group_id = ? AND user_id = ? AND role <> ?", groupID, userID, model.RoleOwner).
		Delete(&model.GroupMember{}).Error
	if err != nil {
		return err
	}
	s.membersChanged(userID, groupID)
	return nil
}

func (s *Store) membersChanged(userID, groupID uint) {
	s.invalidate("group", groupID)
	s.invalidate("groupfeed", groupID)
	s.invalidate("memberships", userID)
	s.drop(s.groupsNS())
}

// membersOf lists each group's members in the order they joined.
func (s *Store) membersOf(groupIDs []uint) (map[uint][]uint, error) {
	return cache.FetchMany(s.Cache, "group", groupIDs, "members", ttl, func(missing []uint) (map[uint][]uint, error) {
		var rows []model.GroupMember
		err := s.DB.Where("group_id IN ?", missing).Order("joined_at").Find(&rows).Error
		out := make(map[uint][]uint, len(missing))
		for _, id := range missing {
			out[id] = nil // so deleted groups are cached as empty, too
		}
		for _, m := range rows {
			out[m.GroupID] = append(out[m.GroupID], m.UserID)
		}
		return out, err
	})
}

func (s *Store) memberIDs(groupID uint) ([]uint, error) {
	m, err := s.membersOf([]uint{groupID})
	return m[groupID], err
}

func (s *Store) IsMember(userID, groupID uint) bool {
	ids, _ := s.memberIDs(groupID)
	return slices.Contains(ids, userID)
}

func (s *Store) GroupMembers(groupID uint) ([]model.User, error) {
	ids, err := s.memberIDs(groupID)
	if err != nil {
		return nil, err
	}
	return s.users(ids)
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

// groupSummaries returns the groups with the given IDs in order, skipping
// any deleted since the IDs were read.
func (s *Store) groupSummaries(ids []uint) ([]GroupSummary, error) {
	groups, err := s.groupsByID(ids)
	if err != nil {
		return nil, err
	}
	members, err := s.membersOf(ids)
	if err != nil {
		return nil, err
	}
	gs := make([]GroupSummary, 0, len(ids))
	for _, id := range ids {
		if g, ok := groups[id]; ok {
			gs = append(gs, GroupSummary{Group: g, Members: int64(len(members[id]))})
		}
	}
	return gs, nil
}

// UserGroups lists the groups userID belongs to, by name.
func (s *Store) UserGroups(userID uint) ([]GroupSummary, error) {
	ids, err := cache.Fetch(s.ns("memberships", userID), "groups", ttl, func() ([]uint, error) {
		var ids []uint
		return ids, s.DB.Model(&model.GroupMember{}).Where("user_id = ?", userID).Pluck("group_id", &ids).Error
	})
	if err != nil {
		return nil, err
	}
	gs, err := s.groupSummaries(ids)
	// Sorted here, so a renamed group doesn't invalidate all its members.
	slices.SortFunc(gs, func(a, b GroupSummary) int { return strings.Compare(a.NameLower, b.NameLower) })
	return gs, err
}

// PopularGroups lists groups by member count.
func (s *Store) PopularGroups(limit int) ([]GroupSummary, error) {
	ids, err := cache.Fetch(s.groupsNS(), "popular:"+strconv.Itoa(limit), ttl, func() ([]uint, error) {
		gs, err := s.summaries(s.DB.Order("members DESC, groups.name_lower").Limit(limit))
		ids := make([]uint, len(gs))
		for i, g := range gs {
			ids[i] = g.ID
		}
		return ids, err
	})
	if err != nil {
		return nil, err
	}
	return s.groupSummaries(ids)
}

// SearchGroups isn't cached: the query is free text.
func (s *Store) SearchGroups(q string, limit int) ([]GroupSummary, error) {
	if strings.TrimSpace(q) == "" {
		return nil, nil
	}
	p := likePattern(q)
	return s.summaries(s.DB.Where(`groups.name_lower LIKE ? ESCAPE '\' OR groups.slug LIKE ? ESCAPE '\'`, p, p).
		Order("groups.name_lower").Limit(limit))
}
