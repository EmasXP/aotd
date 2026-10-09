package store

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/EmasXP/aotd/internal/cache"
	"github.com/EmasXP/aotd/internal/model"
)

const (
	notificationsShown = 50
	keepRead           = 60 * 24 * time.Hour
	keepUnread         = 90 * 24 * time.Hour
)

// MentionRe matches an @username. The character before the @ is matched
// too, so it isn't part of an email address or a URL; the name is group 1.
var MentionRe = regexp.MustCompile(`(?i)(?:^|[^\w@/])@([a-z0-9_]{3,30})\b`)

// Mentions returns the lowercased usernames mentioned in body, each once.
func Mentions(body string) []string {
	var names []string
	for _, m := range MentionRe.FindAllStringSubmatch(body, -1) {
		name := strings.ToLower(m[1])
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// commentRecipients returns who to notify about c, a new comment on a post by
// ownerID, mapped to the notification type. Everyone is notified once, with
// the most specific type: a mention, then a reply, then a comment on their
// post. The commenter is never notified.
func (s *Store) commentRecipients(c *model.Comment, ownerID uint) (map[uint]string, error) {
	types := map[uint]string{ownerID: model.NotifComment}
	if c.ParentID != nil {
		var thread []uint
		err := s.DB.Model(&model.Comment{}).
			Where("id = ? OR parent_id = ?", *c.ParentID, *c.ParentID).
			Pluck("user_id", &thread).Error
		if err != nil {
			return nil, err
		}
		for _, id := range thread {
			types[id] = model.NotifReply
		}
	}
	for _, name := range Mentions(c.Body) {
		u, err := s.UserByUsername(name)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		types[u.ID] = model.NotifMention
	}
	delete(types, c.UserID)
	return types, nil
}

// notifyComment inserts the notifications for c, which tx has just created.
func (s *Store) notifyComment(tx *gorm.DB, c *model.Comment, recipients map[uint]string) error {
	if len(recipients) == 0 {
		return nil
	}
	ns := make([]model.Notification, 0, len(recipients))
	for userID, typ := range recipients {
		ns = append(ns, model.Notification{
			UserID: userID, ActorID: c.UserID, Type: typ,
			PostID: c.PostID, CommentID: &c.ID, CreatedAt: s.Now(),
		})
	}
	return tx.Create(&ns).Error
}

// dropNotifications deletes the notifications matching the condition and
// returns their recipients, whose notif namespaces the caller invalidates
// once the change has committed.
func dropNotifications(tx *gorm.DB, query string, args ...any) ([]uint, error) {
	var users []uint
	if err := tx.Model(&model.Notification{}).Where(query, args...).Pluck("user_id", &users).Error; err != nil {
		return nil, err
	}
	if len(users) == 0 {
		return nil, nil
	}
	slices.Sort(users)
	users = slices.Compact(users)
	return users, tx.Where(query, args...).Delete(&model.Notification{}).Error
}

// UnreadCount is the number of userID's unread notifications.
func (s *Store) UnreadCount(userID uint) (int64, error) {
	return cache.Fetch(s.ns("notif", userID), "unread", ttl, func() (int64, error) {
		var n int64
		err := s.DB.Model(&model.Notification{}).
			Where("user_id = ? AND read_at IS NULL", userID).Count(&n).Error
		return n, err
	})
}

// NotificationView is a notification with its actor and post filled in.
type NotificationView struct {
	model.Notification
	Snippet string // the comment's body, for comment notifications
}

// Unread reports whether the notification hasn't been read.
func (n NotificationView) Unread() bool { return n.ReadAt == nil }

// notificationRow is how a notification is cached: without associations.
type notificationRow struct {
	model.Notification
	Snippet string
}

// Notifications returns userID's newest notifications, newest first.
func (s *Store) Notifications(userID uint) ([]NotificationView, error) {
	rows, err := cache.Fetch(s.ns("notif", userID), "page", ttl, func() ([]notificationRow, error) {
		return s.loadNotifications(userID)
	})
	if err != nil {
		return nil, err
	}
	var actorIDs, postIDs []uint
	for _, r := range rows {
		actorIDs = append(actorIDs, r.ActorID)
		postIDs = append(postIDs, r.PostID)
	}
	actors, err := s.usersByID(actorIDs)
	if err != nil {
		return nil, err
	}
	ps, err := s.posts(postIDs)
	if err != nil {
		return nil, err
	}
	posts := byID(ps, func(p model.Post) uint { return p.ID })
	views := make([]NotificationView, 0, len(rows))
	for _, r := range rows {
		a, aok := actors[r.ActorID]
		p, pok := posts[r.PostID]
		if !aok || !pok {
			continue // deleted while we read
		}
		r.Actor, r.Post = a, p
		views = append(views, NotificationView{Notification: r.Notification, Snippet: r.Snippet})
	}
	return views, nil
}

func (s *Store) loadNotifications(userID uint) ([]notificationRow, error) {
	var ns []model.Notification
	err := s.DB.Where("user_id = ?", userID).
		Order("created_at DESC, id DESC").Limit(notificationsShown).Find(&ns).Error
	if err != nil {
		return nil, err
	}
	var commentIDs []uint
	for _, n := range ns {
		if n.CommentID != nil {
			commentIDs = append(commentIDs, *n.CommentID)
		}
	}
	bodies := map[uint]string{}
	if len(commentIDs) > 0 {
		var cs []model.Comment
		if err := s.DB.Select("id", "body").Where("id IN ?", commentIDs).Find(&cs).Error; err != nil {
			return nil, err
		}
		for _, c := range cs {
			bodies[c.ID] = c.Body
		}
	}
	rows := make([]notificationRow, 0, len(ns))
	for _, n := range ns {
		r := notificationRow{Notification: n}
		if n.CommentID != nil {
			body, ok := bodies[*n.CommentID]
			if !ok {
				continue // the comment is gone
			}
			r.Snippet = body
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// OpenNotification marks userID's notification read and returns the page
// it points at.
func (s *Store) OpenNotification(userID, id uint) (string, error) {
	var n model.Notification
	if err := s.DB.Where("id = ? AND user_id = ?", id, userID).First(&n).Error; err != nil {
		return "", notFound(err)
	}
	if err := s.markRead(userID, id); err != nil {
		return "", err
	}
	target := fmt.Sprintf("/posts/%d", n.PostID)
	if n.CommentID != nil {
		target += fmt.Sprintf("#comment-%d", *n.CommentID)
	}
	return target, nil
}

// MarkNotificationRead marks one of userID's notifications read.
func (s *Store) MarkNotificationRead(userID, id uint) error { return s.markRead(userID, id) }

// MarkAllNotificationsRead marks all of userID's notifications read.
func (s *Store) MarkAllNotificationsRead(userID uint) error { return s.markRead(userID) }

// markRead marks userID's unread notifications with the given IDs read, or
// all of them without IDs.
func (s *Store) markRead(userID uint, ids ...uint) error {
	q := s.DB.Model(&model.Notification{}).Where("user_id = ? AND read_at IS NULL", userID)
	if len(ids) > 0 {
		q = q.Where("id IN ?", ids)
	}
	if err := q.Update("read_at", s.Now()).Error; err != nil {
		return err
	}
	s.invalidate("notif", userID)
	return nil
}

// PurgeNotifications deletes read notifications after keepRead and unread
// ones after keepUnread.
func (s *Store) PurgeNotifications() error {
	now := s.Now()
	users, err := dropNotifications(s.DB, "read_at < ? OR (read_at IS NULL AND created_at < ?)",
		now.Add(-keepRead), now.Add(-keepUnread))
	if err != nil {
		return err
	}
	s.invalidate("notif", users...)
	return nil
}
