package store

import (
	"slices"
	"time"

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

// ReleasesToCheck returns releases with an MBID whose links haven't been
// fetched from MusicBrainz in LinkRefreshAge, never-checked ones first.
func (s *Store) ReleasesToCheck(limit int) ([]model.Release, error) {
	var rs []model.Release
	err := s.DB.Where("mb_id IS NOT NULL AND (checked_at IS NULL OR checked_at < ?)", s.Now().Add(-LinkRefreshAge)).
		Order("checked_at IS NOT NULL, checked_at, id").Limit(limit).Find(&rs).Error
	return rs, err
}

// MarkChecked records that MusicBrainz was just asked about a release.
func (s *Store) MarkChecked(releaseID uint) error {
	return s.DB.Model(&model.Release{}).Where("id = ?", releaseID).
		UpdateColumn("checked_at", s.Now()).Error
}

// shownLinks picks the link to show for each source, per release: the
// oldest, in links.Sources order.
func (s *Store) shownLinks(releaseIDs []uint) (map[uint][]model.ReleaseLink, error) {
	var all []model.ReleaseLink
	if err := s.DB.Where("release_id IN ?", releaseIDs).Order("id").Find(&all).Error; err != nil {
		return nil, err
	}
	out := map[uint][]model.ReleaseLink{}
	for _, l := range all {
		if !slices.Contains(links.Sources, l.Source) || slices.ContainsFunc(out[l.ReleaseID], func(o model.ReleaseLink) bool { return o.Source == l.Source }) {
			continue
		}
		out[l.ReleaseID] = append(out[l.ReleaseID], l)
	}
	for _, ls := range out {
		slices.SortFunc(ls, func(a, b model.ReleaseLink) int {
			return slices.Index(links.Sources, a.Source) - slices.Index(links.Sources, b.Source)
		})
	}
	return out, nil
}
