package store

// Caching. Reads go through the cache; every write invalidates, after it
// has committed, the namespaces whose data it changed. Namespaces:
//
//	user:{id}         the user row
//	follows:{id}      follower and followee IDs
//	activity:{id}     activity counts, the first page of AOTDs and of
//	                  check-ins, checked-in post IDs, the post per date
//	memberships:{id}  IDs of the user's groups
//	wall:{id}         the first page of the user's wall
//	post:{id}         the post row, its counts, comments, who checked in
//	release:{id}      the release row and its shown links
//	group:{id}        the group row and member IDs
//	groupfeed:{id}    the first page of the group's feed
//	groups            popular groups
//
// Plain keys map things that don't change to IDs: username:{name} and
// slug:{slug} (deleted with its group).
//
// Rows are cached without their associations, which are filled in from
// their own namespaces on the way out, so a changed user or release never
// needs every post that shows it invalidated.
//
// Feeds cache only their first page of IDs. Later pages query their IDs and
// still fill the posts in from the cache; caching them too would let
// anyone fill the cache with made-up cursors.

import (
	"errors"
	"log/slog"
	"time"

	"github.com/EmasXP/aotd/internal/cache"
	"github.com/EmasXP/aotd/internal/model"
)

const ttl = time.Hour

func (s *Store) ns(kind string, id uint) cache.Namespace { return cache.NS(s.Cache, kind, id) }

func (s *Store) groupsNS() cache.Namespace { return cache.NS(s.Cache, "groups") }

// invalidate drops kind:{id} for each id. Call it after the change has
// committed. A failure is only logged: the write itself worked, and the
// TTL bounds how long anything stays stale.
func (s *Store) invalidate(kind string, ids ...uint) {
	for _, id := range ids {
		s.drop(s.ns(kind, id))
	}
}

func (s *Store) drop(n cache.Namespace) {
	if err := n.Invalidate(); err != nil {
		slog.Warn("cache invalidate", "err", err)
	}
}

// invalidateFeedsOf drops the feeds that show userID's posts: their own and
// their followers' walls, and their groups' feeds.
func (s *Store) invalidateFeedsOf(userID uint) {
	var followers, groups []uint
	err := errors.Join(
		s.DB.Model(&model.Follow{}).Where("followee_id = ?", userID).Pluck("follower_id", &followers).Error,
		s.DB.Model(&model.GroupMember{}).Where("user_id = ?", userID).Pluck("group_id", &groups).Error,
	)
	if err != nil {
		slog.Warn("cache invalidate feeds", "user", userID, "err", err)
	}
	s.invalidate("wall", append(followers, userID)...)
	s.invalidate("groupfeed", groups...)
}

// The batch loaders below read many rows with one cache read, one query
// for the misses and one cache write. The single-row lookups use them too.

// byID indexes rows by their ID.
func byID[T any](rows []T, id func(T) uint) map[uint]T {
	m := make(map[uint]T, len(rows))
	for _, r := range rows {
		m[id(r)] = r
	}
	return m
}

func (s *Store) usersByID(ids []uint) (map[uint]model.User, error) {
	return cache.FetchMany(s.Cache, "user", ids, "row", ttl, func(missing []uint) (map[uint]model.User, error) {
		var us []model.User
		err := s.DB.Where("id IN ?", missing).Find(&us).Error
		return byID(us, func(u model.User) uint { return u.ID }), err
	})
}

// users returns the users with the given IDs in order.
func (s *Store) users(ids []uint) ([]model.User, error) {
	m, err := s.usersByID(ids)
	if err != nil {
		return nil, err
	}
	us := make([]model.User, 0, len(ids))
	for _, id := range ids {
		if u, ok := m[id]; ok {
			us = append(us, u)
		}
	}
	return us, nil
}

func (s *Store) releasesByID(ids []uint) (map[uint]model.Release, error) {
	return cache.FetchMany(s.Cache, "release", ids, "row", ttl, func(missing []uint) (map[uint]model.Release, error) {
		var rs []model.Release
		err := s.DB.Where("id IN ?", missing).Find(&rs).Error
		return byID(rs, func(r model.Release) uint { return r.ID }), err
	})
}

func (s *Store) releaseByID(id uint) (*model.Release, error) {
	m, err := s.releasesByID([]uint{id})
	if err != nil {
		return nil, err
	}
	r, ok := m[id]
	if !ok {
		return nil, ErrNotFound
	}
	return &r, nil
}

// posts returns the posts with the given IDs in order, with their users and
// releases, skipping any deleted since the IDs were read.
func (s *Store) posts(ids []uint) ([]model.Post, error) {
	rows, err := cache.FetchMany(s.Cache, "post", ids, "row", ttl, func(missing []uint) (map[uint]model.Post, error) {
		var ps []model.Post
		err := s.DB.Where("id IN ?", missing).Find(&ps).Error
		return byID(ps, func(p model.Post) uint { return p.ID }), err
	})
	if err != nil {
		return nil, err
	}
	var userIDs, releaseIDs []uint
	for _, p := range rows {
		userIDs = append(userIDs, p.UserID)
		releaseIDs = append(releaseIDs, p.ReleaseID)
	}
	users, err := s.usersByID(userIDs)
	if err != nil {
		return nil, err
	}
	releases, err := s.releasesByID(releaseIDs)
	if err != nil {
		return nil, err
	}
	ps := make([]model.Post, 0, len(ids))
	for _, id := range ids {
		p, ok := rows[id]
		u, uok := users[p.UserID]
		r, rok := releases[p.ReleaseID]
		if !ok || !uok || !rok {
			continue // deleted while we read
		}
		p.User, p.Release = u, r
		ps = append(ps, p)
	}
	return ps, nil
}
