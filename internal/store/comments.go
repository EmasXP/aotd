package store

import (
	"strings"
	"unicode/utf8"

	"github.com/EmasXP/aotd/internal/model"
)

// AddComment adds a comment to postID. parentID makes it a reply. Threads are
// one level deep: replying to a reply attaches to that reply's top-level
// comment instead.
func (s *Store) AddComment(userID, postID uint, parentID *uint, body string) (*model.Comment, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, invalid("Write something first.")
	}
	if utf8.RuneCountInString(body) > 1000 {
		return nil, invalid("Comments must be at most 1000 characters.")
	}
	if _, err := s.PostByID(postID); err != nil {
		return nil, err
	}
	if parentID != nil {
		var parent model.Comment
		if err := s.DB.First(&parent, *parentID).Error; err != nil {
			return nil, notFound(err)
		}
		if parent.PostID != postID {
			return nil, ErrNotFound
		}
		if parent.ParentID != nil {
			parentID = parent.ParentID
		}
	}
	c := &model.Comment{PostID: postID, UserID: userID, ParentID: parentID, Body: body}
	if err := s.DB.Create(c).Error; err != nil {
		return nil, err
	}
	s.invalidateUsers(userID)
	return c, nil
}

// DeleteComment soft-deletes a comment by its author.
func (s *Store) DeleteComment(userID, commentID uint) (postID uint, err error) {
	var c model.Comment
	if err := s.DB.First(&c, commentID).Error; err != nil {
		return 0, notFound(err)
	}
	if c.UserID != userID {
		return 0, ErrForbidden
	}
	if err := s.DB.Delete(&c).Error; err != nil {
		return 0, err
	}
	s.invalidateUsers(userID)
	return c.PostID, nil
}

// Thread is a top-level comment and its replies.
type Thread struct {
	Comment model.Comment
	Deleted bool // shown as "[deleted]" because it still has replies
	Replies []model.Comment
}

// Comments returns postID's comment threads, oldest first.
func (s *Store) Comments(postID uint) ([]Thread, error) {
	var all []model.Comment
	err := s.DB.Unscoped().Preload("User").Where("post_id = ?", postID).
		Order("created_at, id").Find(&all).Error
	if err != nil {
		return nil, err
	}
	replies := map[uint][]model.Comment{}
	for _, c := range all {
		if c.ParentID != nil && !c.DeletedAt.Valid {
			replies[*c.ParentID] = append(replies[*c.ParentID], c)
		}
	}
	var threads []Thread
	for _, c := range all {
		if c.ParentID != nil {
			continue
		}
		t := Thread{Comment: c, Deleted: c.DeletedAt.Valid, Replies: replies[c.ID]}
		if t.Deleted && len(t.Replies) == 0 {
			continue
		}
		if t.Deleted {
			t.Comment.Body = ""
		}
		threads = append(threads, t)
	}
	return threads, nil
}
