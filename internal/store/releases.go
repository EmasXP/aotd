package store

import (
	"errors"
	"slices"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/EmasXP/aotd/internal/links"
	"github.com/EmasXP/aotd/internal/model"
)

// LinkRefreshAge is how often a release's streaming links are refreshed
// from MusicBrainz.
const LinkRefreshAge = 30 * 24 * time.Hour

// AddLinks attaches links to a release. Links it already has, or that
// belong to another release, are skipped.
func (s *Store) AddLinks(releaseID uint, ls []links.Link) error {
	if len(ls) == 0 {
		return nil
	}
	rows := make([]model.ReleaseLink, len(ls))
	for i, l := range ls {
		rows[i] = model.ReleaseLink{ReleaseID: releaseID, Source: l.Source, ExternalID: l.ID}
	}
	return s.DB.Omit("Release").Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error
}

// ReleasesToCheck returns releases due a MusicBrainz check, never-checked
// ones first:
//   - with an MBID: for streaming links, every LinkRefreshAge;
//   - without an MBID but with a link: whether MusicBrainz has the link now,
//     daily for a month after it was posted, then weekly.
func (s *Store) ReleasesToCheck(limit int) ([]model.Release, error) {
	now := s.Now()
	var rs []model.Release
	err := s.DB.Where("mb_id IS NOT NULL AND (checked_at IS NULL OR checked_at < ?)", now.Add(-LinkRefreshAge)).
		Or(s.DB.Where("mb_id IS NULL AND EXISTS (SELECT 1 FROM release_links WHERE release_links.release_id = releases.id)").
			Where(s.DB.Where("checked_at IS NULL").
				Or("checked_at < ? AND created_at > ?", now.Add(-24*time.Hour), now.Add(-30*24*time.Hour)).
				Or("checked_at < ?", now.Add(-7*24*time.Hour)))).
		Order("checked_at IS NOT NULL, checked_at, id").Limit(limit).Find(&rs).Error
	return rs, err
}

// Links returns all of a release's links, oldest first.
func (s *Store) Links(releaseID uint) ([]model.ReleaseLink, error) {
	var ls []model.ReleaseLink
	return ls, s.DB.Where("release_id = ?", releaseID).Order("id").Find(&ls).Error
}

// MatchPost links the release of userID's post to a MusicBrainz album, if
// it has none. Any day's post: the album may reach MusicBrainz much later.
func (s *Store) MatchPost(userID, postID uint, album NewPost) (*model.Release, error) {
	p, err := s.PostByID(postID)
	if err != nil {
		return nil, err
	}
	if p.UserID != userID {
		return nil, ErrForbidden
	}
	return s.AttachMBID(p.ReleaseID, album)
}

// AttachMBID gives a release without an MBID the album's MBID and metadata
// (only the album fields of album are used), merging it into the release
// that already has that MBID if there is one. The result is due a link check.
func (s *Store) AttachMBID(releaseID uint, album NewPost) (*model.Release, error) {
	if album.MBID == nil {
		return nil, invalid("No MusicBrainz album given.")
	}
	if err := s.validAlbum(album); err != nil {
		return nil, err
	}
	var out *model.Release
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var r model.Release
		if err := tx.First(&r, releaseID).Error; err != nil {
			return notFound(err)
		}
		if r.MBID != nil {
			return invalid("That album is already linked to MusicBrainz.")
		}
		var err error
		if out, err = attachMBID(tx, &r, album); err != nil {
			return err
		}
		return tx.Model(out).UpdateColumn("checked_at", nil).Error
	})
	return out, err
}

// MarkChecked records that MusicBrainz was just asked about a release.
func (s *Store) MarkChecked(releaseID uint) error {
	return s.DB.Model(&model.Release{}).Where("id = ?", releaseID).
		UpdateColumn("checked_at", s.Now()).Error
}

// ReleaseByLink returns the release a link belongs to.
func (s *Store) ReleaseByLink(l links.Link) (*model.Release, error) {
	var rl model.ReleaseLink
	err := s.DB.Preload("Release").Where("source = ? AND external_id = ?", l.Source, l.ID).First(&rl).Error
	return &rl.Release, notFound(err)
}

// attachMBID gives a release without an MBID the album's MBID and metadata,
// keeping its cover (new MusicBrainz entries often have no art yet). If
// another release already has that MBID, r is merged into it instead, and
// that release is returned.
func attachMBID(tx *gorm.DB, r *model.Release, album NewPost) (*model.Release, error) {
	var into model.Release
	err := tx.Where("mb_id = ?", *album.MBID).First(&into).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		cover := r.CoverURL
		if cover == "" {
			cover = album.CoverURL
		}
		err := tx.Model(r).Updates(map[string]any{
			"mb_id": *album.MBID, "title": album.Title, "artist": album.Artist, "year": album.Year, "cover_url": cover,
		}).Error
		if err != nil {
			return nil, err
		}
		return r, tx.First(r, r.ID).Error
	} else if err != nil {
		return nil, err
	}
	for _, m := range []any{&model.Post{}, &model.ReleaseLink{}} {
		if err := tx.Model(m).Where("release_id = ?", r.ID).Update("release_id", into.ID).Error; err != nil {
			return nil, err
		}
	}
	return &into, tx.Delete(&model.Release{}, r.ID).Error
}

// shownLinks picks the link to show for each source, per release: the one
// most posts were made with, then the oldest. Sorted in links.Sources order.
func (s *Store) shownLinks(releaseIDs []uint) (map[uint][]model.ReleaseLink, error) {
	var all []model.ReleaseLink
	if err := s.DB.Where("release_id IN ?", releaseIDs).Order("id").Find(&all).Error; err != nil {
		return nil, err
	}
	var counts []struct {
		LinkID uint
		N      int
	}
	if err := s.DB.Model(&model.Post{}).Select("link_id, COUNT(*) AS n").
		Where("release_id IN ? AND link_id IS NOT NULL", releaseIDs).Group("link_id").Scan(&counts).Error; err != nil {
		return nil, err
	}
	posts := map[uint]int{}
	for _, c := range counts {
		posts[c.LinkID] = c.N
	}
	type key struct {
		release uint
		source  string
	}
	best := map[key]model.ReleaseLink{}
	for _, l := range all { // oldest first, so ties keep the oldest
		k := key{l.ReleaseID, l.Source}
		if b, ok := best[k]; !ok || posts[l.ID] > posts[b.ID] {
			best[k] = l
		}
	}
	out := map[uint][]model.ReleaseLink{}
	for _, src := range links.Sources {
		for _, id := range releaseIDs {
			if l, ok := best[key{id, src}]; ok && !slices.ContainsFunc(out[id], func(o model.ReleaseLink) bool { return o.ID == l.ID }) {
				out[id] = append(out[id], l)
			}
		}
	}
	return out, nil
}
