package store

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/EmasXP/aotd/internal/model"
)

// Album metadata from MusicBrainz and ListenBrainz, kept for statistics
// such as a year in review. None of it is shown or cached yet, except the
// release and post rows it's on.

// PopularityWindow is how long after posting an album's popularity is still
// asked for, should it only get an MBID later. After that it would no
// longer be its popularity when it was posted.
const PopularityWindow = 7 * 24 * time.Hour

type Genre struct {
	Name  string
	Votes int
}

type ArtistCredit struct {
	MBID string
	Name string
}

// ReleaseMeta is what MusicBrainz says about a release beyond its album
// fields.
type ReleaseMeta struct {
	TrackCount int
	Length     time.Duration // 0 if unknown
	Credits    []ArtistCredit
	Genres     []Genre
}

// ArtistMeta is what MusicBrainz says about an artist.
type ArtistMeta struct {
	MBID    string
	Name    string
	Country string
	Area    string
	Genres  []Genre
}

// SetReleaseMeta replaces a release's metadata, artists and genres. Artists
// new to AOTD are added, due a check.
func (s *Store) SetReleaseMeta(releaseID uint, m ReleaseMeta) error {
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Model(&model.Release{}).Where("id = ?", releaseID).UpdateColumns(map[string]any{
			"track_count": m.TrackCount, "length": int(m.Length / time.Second),
		}).Error
		if err != nil {
			return err
		}
		if err := tx.Where("release_id = ?", releaseID).Delete(&model.ReleaseGenre{}).Error; err != nil {
			return err
		}
		if len(m.Genres) > 0 {
			rows := make([]model.ReleaseGenre, len(m.Genres))
			for i, g := range m.Genres {
				rows[i] = model.ReleaseGenre{ReleaseID: releaseID, Genre: g.Name, Votes: g.Votes}
			}
			if err := tx.Omit("Release").Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("release_id = ?", releaseID).Delete(&model.ReleaseArtist{}).Error; err != nil {
			return err
		}
		var credits []model.ReleaseArtist
		for i, c := range m.Credits {
			a := model.Artist{MBID: c.MBID, Name: c.Name}
			err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "mb_id"}}, DoNothing: true}).Create(&a).Error
			if err != nil {
				return err
			}
			if err := tx.Where("mb_id = ?", c.MBID).First(&a).Error; err != nil {
				return err
			}
			credits = append(credits, model.ReleaseArtist{ReleaseID: releaseID, ArtistID: a.ID, Position: i})
		}
		if len(credits) > 0 {
			return tx.Omit("Release", "Artist").Clauses(clause.OnConflict{DoNothing: true}).Create(&credits).Error
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.invalidate("release", releaseID)
	return nil
}

// ArtistsToCheck returns credited artists due a MusicBrainz check, every
// LinkRefreshAge, never-checked ones first.
func (s *Store) ArtistsToCheck(limit int) ([]model.Artist, error) {
	var as []model.Artist
	err := s.DB.Where("checked_at IS NULL OR checked_at < ?", s.Now().Add(-LinkRefreshAge)).
		Where("EXISTS (SELECT 1 FROM release_artists WHERE release_artists.artist_id = artists.id)").
		Order("checked_at IS NOT NULL, checked_at, id").Limit(limit).Find(&as).Error
	return as, err
}

// SetArtist stores what MusicBrainz says about an artist AOTD has, and
// marks it checked.
func (s *Store) SetArtist(m ArtistMeta) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		var a model.Artist
		if err := tx.Where("mb_id = ?", m.MBID).First(&a).Error; err != nil {
			return notFound(err)
		}
		updates := map[string]any{"country": m.Country, "area": m.Area, "checked_at": s.Now()}
		if m.Name != "" {
			updates["name"] = m.Name
		}
		if err := tx.Model(&a).UpdateColumns(updates).Error; err != nil {
			return err
		}
		if err := tx.Where("artist_id = ?", a.ID).Delete(&model.ArtistGenre{}).Error; err != nil {
			return err
		}
		if len(m.Genres) == 0 {
			return nil
		}
		rows := make([]model.ArtistGenre, len(m.Genres))
		for i, g := range m.Genres {
			rows[i] = model.ArtistGenre{ArtistID: a.ID, Genre: g.Name, Votes: g.Votes}
		}
		return tx.Omit("Artist").Clauses(clause.OnConflict{DoNothing: true}).Create(&rows).Error
	})
}

// MarkArtistChecked records that MusicBrainz was just asked about an
// artist, when it had nothing to say.
func (s *Store) MarkArtistChecked(artistID uint) error {
	return s.DB.Model(&model.Artist{}).Where("id = ?", artistID).UpdateColumn("checked_at", s.Now()).Error
}

// PostMBID is a post and its album's MBID.
type PostMBID struct {
	PostID uint
	MBID   string
}

// PostsWithoutPopularity returns posts from the last PopularityWindow whose
// album has an MBID but no popularity yet, oldest first.
func (s *Store) PostsWithoutPopularity(limit int) ([]PostMBID, error) {
	var out []PostMBID
	err := s.DB.Model(&model.Post{}).Select("posts.id AS post_id, releases.mb_id AS mb_id").
		Joins("JOIN releases ON releases.id = posts.release_id").
		Where("posts.popularity_at IS NULL AND posts.created_at > ? AND releases.mb_id IS NOT NULL", s.Now().Add(-PopularityWindow)).
		Order("posts.id").Limit(limit).Scan(&out).Error
	return out, err
}

// Popularity is an album's ListenBrainz popularity; nil when unknown.
type Popularity struct {
	Listens   *int
	Listeners *int
}

// SetPopularity stores the popularity of posts' albums, by post ID.
func (s *Store) SetPopularity(pops map[uint]Popularity) error {
	now := s.Now()
	ids := make([]uint, 0, len(pops))
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		for id, p := range pops {
			err := tx.Model(&model.Post{}).Where("id = ?", id).UpdateColumns(map[string]any{
				"listens": p.Listens, "listeners": p.Listeners, "popularity_at": now,
			}).Error
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.invalidate("post", ids...)
	return nil
}
