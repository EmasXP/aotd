package store

import (
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/EmasXP/aotd/internal/cache"
	"github.com/EmasXP/aotd/internal/links"
	"github.com/EmasXP/aotd/internal/model"
)

// NewPost is the input for CreatePost: the album and the note. MBID is nil
// for manual entries. Link, if set, is the streaming link it was posted
// with; a link AOTD already knows decides the release, and the album
// fields are then only used to give that release an MBID.
type NewPost struct {
	MBID     *string
	Title    string
	Artist   string
	Year     int
	CoverURL string
	Link     *links.Link
	Note     string
}

// CreatePost posts userID's AOTD for today (CET). The unique index on
// (user_id, post_date) guarantees one per day even under concurrent requests.
func (s *Store) CreatePost(userID uint, in NewPost) (*model.Post, error) {
	in.Title = strings.TrimSpace(in.Title)
	in.Artist = strings.TrimSpace(in.Artist)
	in.Note = strings.TrimSpace(in.Note)
	if utf8.RuneCountInString(in.Note) > 500 {
		return nil, invalid("The note must be at most 500 characters.")
	}
	today := s.Today()
	if _, err := s.PostOn(userID, today); err == nil {
		return nil, ErrAlreadyPosted
	}
	p := &model.Post{UserID: userID, PostDate: today, Note: in.Note}
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		r, link, err := s.releaseFor(tx, in)
		if err != nil {
			return err
		}
		p.ReleaseID, p.Release = r.ID, *r
		if link != nil {
			p.LinkID = &link.ID
		}
		return tx.Omit("Release", "Link").Create(p).Error
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		return nil, ErrAlreadyPosted
	}
	if err != nil {
		return nil, err
	}
	s.invalidate("activity", userID)
	s.invalidate("release", p.ReleaseID) // its shown links count posts
	s.invalidateFeedsOf(userID)
	return p, nil
}

// validAlbum checks the album fields of a new release.
func (s *Store) validAlbum(in NewPost) error {
	switch {
	case in.Title == "" || in.Artist == "":
		return invalid("Title and artist are required.")
	case utf8.RuneCountInString(in.Title) > 300 || utf8.RuneCountInString(in.Artist) > 300:
		return invalid("Title and artist must be at most 300 characters.")
	case in.Year != 0 && (in.Year < 1850 || in.Year > s.Now().Year()+1):
		return invalid("That year doesn't look right.")
	}
	return nil
}

// releaseFor finds the release for a new post, or creates it, and the
// post's link row if it has a link. A known link wins: its release is used,
// and gets in.MBID if it has none. Otherwise posts with an MBID share one
// release, refreshed with the latest metadata, and every manual entry gets
// its own.
func (s *Store) releaseFor(tx *gorm.DB, in NewPost) (*model.Release, *model.ReleaseLink, error) {
	var link *model.ReleaseLink
	if in.Link != nil {
		var l model.ReleaseLink
		err := tx.Preload("Release").Where("source = ? AND external_id = ?", in.Link.Source, in.Link.ID).First(&l).Error
		switch {
		case err == nil:
			r := &l.Release
			if in.MBID != nil && r.MBID == nil {
				if err := s.validAlbum(in); err != nil {
					return nil, nil, err
				}
				if r, err = attachMBID(tx, r, in); err != nil {
					return nil, nil, err
				}
			}
			return r, &l, nil
		case !errors.Is(err, gorm.ErrRecordNotFound):
			return nil, nil, err
		}
		link = &model.ReleaseLink{Source: in.Link.Source, ExternalID: in.Link.ID}
	}
	if err := s.validAlbum(in); err != nil {
		return nil, nil, err
	}
	r := &model.Release{MBID: in.MBID, Title: in.Title, Artist: in.Artist, Year: in.Year, CoverURL: in.CoverURL}
	if in.MBID == nil {
		if err := tx.Create(r).Error; err != nil {
			return nil, nil, err
		}
	} else {
		err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "mb_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"title", "artist", "year", "cover_url", "updated_at"}),
		}).Create(r).Error
		if err != nil {
			return nil, nil, err
		}
		// On conflict the returned ID isn't reliable on every database; reload.
		if err := tx.Where("mb_id = ?", *in.MBID).First(r).Error; err != nil {
			return nil, nil, err
		}
	}
	if link != nil {
		link.ReleaseID = r.ID
		err := tx.Omit("Release").Create(link).Error
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			// Someone posted the same new link a moment ago.
			return nil, nil, invalid("Someone just posted that link. Please try again.")
		} else if err != nil {
			return nil, nil, err
		}
	}
	return r, link, nil
}

// PostOn returns userID's post on the given date.
func (s *Store) PostOn(userID uint, date string) (*model.Post, error) {
	id, err := cache.Fetch(s.ns("activity", userID), "on:"+date, ttl, func() (uint, error) {
		var ids []uint // 0 or 1; 0 is cached as "none"
		err := s.DB.Model(&model.Post{}).Where("user_id = ? AND post_date = ?", userID, date).Limit(1).Pluck("id", &ids).Error
		if len(ids) == 0 {
			return 0, err
		}
		return ids[0], err
	})
	if err != nil {
		return nil, err
	}
	if id == 0 {
		return nil, ErrNotFound
	}
	return s.PostByID(id)
}

// PostByID returns a post with its user and release.
func (s *Store) PostByID(id uint) (*model.Post, error) {
	ps, err := s.posts([]uint{id})
	if err != nil {
		return nil, err
	}
	if len(ps) == 0 {
		return nil, ErrNotFound
	}
	return &ps[0], nil
}

// ownTodayPost loads a post that userID may modify: their own, from today.
func (s *Store) ownTodayPost(userID, postID uint) (*model.Post, error) {
	p, err := s.PostByID(postID)
	if err != nil {
		return nil, err
	}
	if p.UserID != userID || p.PostDate != s.Today() {
		return nil, ErrForbidden
	}
	return p, nil
}

// UpdateNote changes the note on today's post.
func (s *Store) UpdateNote(userID, postID uint, note string) error {
	p, err := s.ownTodayPost(userID, postID)
	if err != nil {
		return err
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > 500 {
		return invalid("The note must be at most 500 characters.")
	}
	if err := s.DB.Model(&model.Post{}).Where("id = ?", p.ID).Update("note", note).Error; err != nil {
		return err
	}
	s.invalidate("post", p.ID)
	return nil
}

// DeletePost removes today's post (with its check-ins and comments), which
// frees the day for a new post.
func (s *Store) DeletePost(userID, postID uint) error {
	p, err := s.ownTodayPost(userID, postID)
	if err != nil {
		return err
	}
	// Everyone who checked in or commented loses one too.
	affected := []uint{userID}
	var notified []uint
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		var err error
		if notified, err = dropNotifications(tx, "post_id = ?", p.ID); err != nil {
			return err
		}
		var others []uint
		if err := tx.Raw("SELECT user_id FROM check_ins WHERE post_id = ? UNION SELECT user_id FROM comments WHERE post_id = ?", p.ID, p.ID).
			Scan(&others).Error; err != nil {
			return err
		}
		affected = append(affected, others...)
		if err := tx.Where("post_id = ?", p.ID).Delete(&model.CheckIn{}).Error; err != nil {
			return err
		}
		if err := tx.Unscoped().Where("post_id = ?", p.ID).Delete(&model.Comment{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(p).Error; err != nil {
			return err
		}
		return deleteOrphanedManualRelease(tx, p.ReleaseID)
	})
	if err != nil {
		return err
	}
	s.invalidate("post", p.ID)
	s.invalidate("release", p.ReleaseID)
	s.invalidate("activity", affected...)
	s.invalidate("notif", notified...)
	s.invalidateFeedsOf(userID)
	return nil
}

// deleteOrphanedManualRelease removes a release without an MBID once no post
// uses it. Releases with an MBID are kept for the next post of the album.
func deleteOrphanedManualRelease(tx *gorm.DB, releaseID uint) error {
	return tx.Where("id = ? AND mb_id IS NULL AND NOT EXISTS (SELECT 1 FROM posts WHERE release_id = ?)", releaseID, releaseID).
		Delete(&model.Release{}).Error
}

// --- check-ins ---

// CheckIn marks that userID listened to postID. Only allowed on other
// people's posts. Idempotent.
func (s *Store) CheckIn(userID, postID uint) error {
	p, err := s.PostByID(postID)
	if err != nil {
		return err
	}
	if p.UserID == userID {
		return ErrForbidden
	}
	c := model.CheckIn{UserID: userID, PostID: postID, CreatedAt: time.Now()}
	err = s.DB.Transaction(func(tx *gorm.DB) error {
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&c)
		if res.Error != nil || res.RowsAffected == 0 {
			return res.Error // already checked in: already notified
		}
		return tx.Create(&model.Notification{
			UserID: p.UserID, ActorID: userID, Type: model.NotifCheckIn,
			PostID: postID, CreatedAt: s.Now(),
		}).Error
	})
	if err != nil {
		return err
	}
	s.checkInsChanged(userID, postID)
	s.invalidate("notif", p.UserID)
	return nil
}

func (s *Store) UndoCheckIn(userID, postID uint) error {
	var notified []uint
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND post_id = ?", userID, postID).Delete(&model.CheckIn{}).Error; err != nil {
			return err
		}
		var err error
		notified, err = dropNotifications(tx, "actor_id = ? AND post_id = ? AND type = ?", userID, postID, model.NotifCheckIn)
		return err
	})
	if err != nil {
		return err
	}
	s.checkInsChanged(userID, postID)
	s.invalidate("notif", notified...)
	return nil
}

func (s *Store) checkInsChanged(userID, postID uint) {
	s.invalidate("activity", userID)
	s.invalidate("post", postID)
}

// CheckedInUsers lists who checked in on postID, oldest first.
func (s *Store) CheckedInUsers(postID uint) ([]model.User, error) {
	ids, err := cache.Fetch(s.ns("post", postID), "checked-in", ttl, func() ([]uint, error) {
		var ids []uint
		err := s.DB.Model(&model.CheckIn{}).Where("post_id = ?", postID).Order("created_at").Pluck("user_id", &ids).Error
		return ids, err
	})
	if err != nil {
		return nil, err
	}
	return s.users(ids)
}

// checkedInIDs is the set of posts userID has checked in on.
func (s *Store) checkedInIDs(userID uint) (map[uint]bool, error) {
	ids, err := cache.Fetch(s.ns("activity", userID), "checked-in", ttl, func() ([]uint, error) {
		var ids []uint
		return ids, s.DB.Model(&model.CheckIn{}).Where("user_id = ?", userID).Pluck("post_id", &ids).Error
	})
	set := make(map[uint]bool, len(ids))
	for _, id := range ids {
		set[id] = true
	}
	return set, err
}

// --- feeds ---

// FeedItem is a post decorated with counts and links for display.
type FeedItem struct {
	Post      model.Post
	CheckIns  int64
	Comments  int64
	CheckedIn bool                // by the viewer
	Links     []model.ReleaseLink // at most one per source
}

// Cursor is an opaque keyset-pagination position: "<post_date>.<id>".
type Cursor string

func (c Cursor) parse() (date string, id uint, ok bool) {
	d, i, found := strings.Cut(string(c), ".")
	if !found || len(d) != 10 {
		return "", 0, false
	}
	n, err := strconv.ParseUint(i, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return d, uint(n), true
}

// FeedPage is one page of a feed.
type FeedPage struct {
	Items []FeedItem
	Next  Cursor // empty when there are no more items
}

const FeedPageSize = 20

// Wall is the viewer's home feed: posts by people they follow, plus their own.
func (s *Store) Wall(viewerID uint, before Cursor) (FeedPage, error) {
	return s.feed(viewerID, before, s.ns("wall", viewerID), "first", func(q *gorm.DB) *gorm.DB {
		return q.Where("posts.user_id = ? OR posts.user_id IN (SELECT followee_id FROM follows WHERE follower_id = ?)", viewerID, viewerID)
	})
}

// UserPosts is a user's own AOTDs.
func (s *Store) UserPosts(viewerID, userID uint, before Cursor) (FeedPage, error) {
	return s.feed(viewerID, before, s.ns("activity", userID), "posts", func(q *gorm.DB) *gorm.DB {
		return q.Where("posts.user_id = ?", userID)
	})
}

// UserCheckIns is the posts a user has checked in on.
func (s *Store) UserCheckIns(viewerID, userID uint, before Cursor) (FeedPage, error) {
	return s.feed(viewerID, before, s.ns("activity", userID), "check-ins", func(q *gorm.DB) *gorm.DB {
		return q.Where("posts.id IN (SELECT post_id FROM check_ins WHERE user_id = ?)", userID)
	})
}

// GroupPosts is the AOTDs of a group's members.
func (s *Store) GroupPosts(viewerID, groupID uint, before Cursor) (FeedPage, error) {
	return s.feed(viewerID, before, s.ns("groupfeed", groupID), "first", func(q *gorm.DB) *gorm.DB {
		return q.Where("posts.user_id IN (SELECT user_id FROM group_members WHERE group_id = ?)", groupID)
	})
}

// idPage is a feed page before its posts are filled in.
type idPage struct {
	IDs  []uint
	Next Cursor
}

// feed returns a page of the posts scope selects. The first page's IDs are
// cached as key in ns.
func (s *Store) feed(viewerID uint, before Cursor, ns cache.Namespace, key string, scope func(*gorm.DB) *gorm.DB) (FeedPage, error) {
	load := func() (idPage, error) { return s.feedIDs(before, scope) }
	var ip idPage
	var err error
	if before == "" {
		ip, err = cache.Fetch(ns, key, ttl, load)
	} else {
		ip, err = load()
	}
	if err != nil {
		return FeedPage{}, err
	}
	posts, err := s.posts(ip.IDs)
	if err != nil {
		return FeedPage{}, err
	}
	items, err := s.decorate(viewerID, posts)
	return FeedPage{Items: items, Next: ip.Next}, err
}

func (s *Store) feedIDs(before Cursor, scope func(*gorm.DB) *gorm.DB) (idPage, error) {
	q := scope(s.DB.Model(&model.Post{}))
	if d, id, ok := before.parse(); ok {
		q = q.Where("posts.post_date < ? OR (posts.post_date = ? AND posts.id < ?)", d, d, id)
	}
	var rows []struct {
		ID       uint
		PostDate string
	}
	if err := q.Select("posts.id, posts.post_date").Order("posts.post_date DESC, posts.id DESC").
		Limit(FeedPageSize + 1).Scan(&rows).Error; err != nil {
		return idPage{}, err
	}
	var page idPage
	if len(rows) > FeedPageSize {
		rows = rows[:FeedPageSize]
		last := rows[len(rows)-1]
		page.Next = Cursor(last.PostDate + "." + strconv.FormatUint(uint64(last.ID), 10))
	}
	for _, r := range rows {
		page.IDs = append(page.IDs, r.ID)
	}
	return page, nil
}

type postCounts struct{ CheckIns, Comments int64 }

func (s *Store) postCounts(postIDs []uint) (map[uint]postCounts, error) {
	return cache.FetchMany(s.Cache, "post", postIDs, "counts", ttl, func(missing []uint) (map[uint]postCounts, error) {
		type row struct {
			PostID uint
			N      int64
		}
		var checkIns, comments []row
		err := errors.Join(
			s.DB.Model(&model.CheckIn{}).Select("post_id, COUNT(*) AS n").
				Where("post_id IN ?", missing).Group("post_id").Scan(&checkIns).Error,
			s.DB.Model(&model.Comment{}).Select("post_id, COUNT(*) AS n").
				Where("post_id IN ?", missing).Group("post_id").Scan(&comments).Error,
		)
		out := make(map[uint]postCounts, len(missing))
		for _, id := range missing {
			out[id] = postCounts{} // posts without any are cached too
		}
		for _, r := range checkIns {
			c := out[r.PostID]
			c.CheckIns = r.N
			out[r.PostID] = c
		}
		for _, r := range comments {
			c := out[r.PostID]
			c.Comments = r.N
			out[r.PostID] = c
		}
		return out, err
	})
}

// releaseLinks is the links shown for each release, at most one per source.
func (s *Store) releaseLinks(releaseIDs []uint) (map[uint][]model.ReleaseLink, error) {
	return cache.FetchMany(s.Cache, "release", releaseIDs, "links", ttl, func(missing []uint) (map[uint][]model.ReleaseLink, error) {
		shown, err := s.shownLinks(missing)
		for _, id := range missing {
			if _, ok := shown[id]; !ok {
				shown[id] = nil // releases without links are cached too
			}
		}
		return shown, err
	})
}

// decorate adds counts, links and the viewer's check-ins to posts.
func (s *Store) decorate(viewerID uint, posts []model.Post) ([]FeedItem, error) {
	if len(posts) == 0 {
		return nil, nil
	}
	ids := make([]uint, len(posts))
	releaseIDs := make([]uint, len(posts))
	for i, p := range posts {
		ids[i], releaseIDs[i] = p.ID, p.ReleaseID
	}
	mine, err := s.checkedInIDs(viewerID)
	if err != nil {
		return nil, err
	}
	counts, err := s.postCounts(ids)
	if err != nil {
		return nil, err
	}
	links, err := s.releaseLinks(releaseIDs)
	if err != nil {
		return nil, err
	}
	items := make([]FeedItem, len(posts))
	for i, p := range posts {
		c := counts[p.ID]
		items[i] = FeedItem{Post: p, CheckIns: c.CheckIns, Comments: c.Comments, CheckedIn: mine[p.ID], Links: links[p.ReleaseID]}
	}
	return items, nil
}

// Item returns a single decorated post.
func (s *Store) Item(viewerID, postID uint) (FeedItem, error) {
	p, err := s.PostByID(postID)
	if err != nil {
		return FeedItem{}, err
	}
	items, err := s.decorate(viewerID, []model.Post{*p})
	if err != nil {
		return FeedItem{}, err
	}
	return items[0], nil
}
