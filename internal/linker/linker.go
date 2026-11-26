// Package linker keeps releases' streaming links and metadata up to date
// from MusicBrainz, and asks ListenBrainz how popular posted albums are, in
// the background.
package linker

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/EmasXP/aotd/internal/links"
	"github.com/EmasXP/aotd/internal/listenbrainz"
	"github.com/EmasXP/aotd/internal/model"
	"github.com/EmasXP/aotd/internal/musicbrainz"
	"github.com/EmasXP/aotd/internal/store"
)

type Linker struct {
	Store *store.Store
	MB    *musicbrainz.Client
	LB    *listenbrainz.Client // nil skips popularity
	Log   *slog.Logger
	// Pause between MusicBrainz requests leaves most of the rate limit to
	// people searching.
	Pause time.Duration
	// MaxPerPass caps how many releases (and how many artists) one pass
	// checks, spreading large backlogs across intervals. Zero means 20.
	MaxPerPass int
	kick       chan struct{}
}

func New(st *store.Store, mb *musicbrainz.Client, lb *listenbrainz.Client) *Linker {
	return &Linker{Store: st, MB: mb, LB: lb, Log: slog.Default(), Pause: 2 * time.Second, MaxPerPass: 20, kick: make(chan struct{}, 1)}
}

// Kick asks for a pass soon, e.g. after an album was posted. It never
// blocks, and does nothing on a nil Linker.
func (l *Linker) Kick() {
	if l == nil {
		return
	}
	select {
	case l.kick <- struct{}{}:
	default:
	}
}

// Run makes a pass at start, on every Kick and every interval, until ctx is
// done.
func (l *Linker) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		l.Pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-l.kick:
		}
	}
}

// Pass asks ListenBrainz about new posts, then checks every release and
// artist that is due, and returns how many releases it checked. It stops
// early if MusicBrainz fails, leaving the rest for the next pass.
func (l *Linker) Pass(ctx context.Context) int {
	l.popularity(ctx)
	n, ok := l.releases(ctx)
	if ok {
		l.artists(ctx)
	}
	return n
}

func (l *Linker) maxPerPass() int {
	if l.MaxPerPass > 0 {
		return l.MaxPerPass
	}
	return 20
}

func (l *Linker) releases(ctx context.Context) (int, bool) {
	n, cap := 0, l.maxPerPass()
	for n < cap {
		rs, err := l.Store.ReleasesToCheck(cap - n)
		if err != nil {
			l.Log.Error("linker: list releases", "err", err)
			return n, false
		}
		if len(rs) == 0 {
			return n, true
		}
		for _, r := range rs {
			if err := l.check(ctx, r); err != nil {
				if ctx.Err() == nil {
					l.Log.Warn("linker: check release", "release", r.ID, "err", err)
				}
				return n, false
			}
			n++
			if n >= cap {
				return n, true
			}
			if l.rest(ctx) != nil {
				return n, false
			}
		}
	}
	return n, true
}

func (l *Linker) artists(ctx context.Context) {
	n, cap := 0, l.maxPerPass()
	for n < cap {
		as, err := l.Store.ArtistsToCheck(cap - n)
		if err != nil {
			l.Log.Error("linker: list artists", "err", err)
			return
		}
		if len(as) == 0 {
			return
		}
		for _, a := range as {
			if err := l.checkArtist(ctx, a); err != nil {
				if ctx.Err() == nil {
					l.Log.Warn("linker: check artist", "artist", a.ID, "err", err)
				}
				return
			}
			n++
			if n >= cap {
				return
			}
			if l.rest(ctx) != nil {
				return
			}
		}
	}
}

// rest waits Pause, or until ctx is done.
func (l *Linker) rest(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(l.Pause):
		return nil
	}
}

// check asks MusicBrainz about a release: for its group's links and
// metadata if it has an MBID, otherwise for an MBID via its links.
func (l *Linker) check(ctx context.Context, r model.Release) error {
	if r.MBID == nil {
		return l.findMBID(ctx, r)
	}
	rels, err := l.MB.ReleaseGroupReleases(ctx, *r.MBID)
	if err != nil && !errors.Is(err, musicbrainz.ErrNotFound) {
		return err
	}
	var found []links.Link
	for _, rel := range rels {
		for _, u := range rel.URLs {
			if lk, ok := links.Parse(u); ok && !slices.Contains(found, lk) {
				found = append(found, lk)
			}
		}
	}
	if err := l.Store.AddLinks(r.ID, found); err != nil {
		return err
	}
	if err := l.meta(ctx, r, rels); err != nil {
		return err
	}
	return l.Store.MarkChecked(r.ID)
}

// meta fetches a release's artists, genres and length. If MusicBrainz no
// longer has the group, it keeps what it had.
func (l *Linker) meta(ctx context.Context, r model.Release, rels []musicbrainz.Release) error {
	if err := l.rest(ctx); err != nil {
		return err
	}
	gm, err := l.MB.ReleaseGroupMeta(ctx, *r.MBID)
	if errors.Is(err, musicbrainz.ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	m := store.ReleaseMeta{}
	for _, c := range gm.Credits {
		m.Credits = append(m.Credits, store.ArtistCredit{MBID: c.MBID, Name: c.Name})
	}
	for _, g := range gm.Genres {
		m.Genres = append(m.Genres, store.Genre{Name: g.Name, Votes: g.Votes})
	}
	if r.TrackCount > 0 && r.Length > 0 {
		m.TrackCount = r.TrackCount
		m.Length = time.Duration(r.Length) * time.Second
	} else if rep, ok := representative(rels); ok {
		if err := l.rest(ctx); err != nil {
			return err
		}
		m.TrackCount, m.Length, err = l.MB.ReleaseLength(ctx, rep.MBID)
		if err != nil && !errors.Is(err, musicbrainz.ErrNotFound) {
			return err
		}
		if m.TrackCount == 0 {
			m.TrackCount = rep.Tracks
		}
	}
	return l.Store.SetReleaseMeta(r.ID, m)
}

// representative picks the release whose length stands for its group's:
// the earliest official one, and the shortest of those out the same day, so
// a deluxe edition doesn't count.
func representative(rels []musicbrainz.Release) (musicbrainz.Release, bool) {
	var best musicbrainz.Release
	found := false
	for _, r := range rels {
		if r.Tracks > 0 && (!found || before(r, best)) {
			best, found = r, true
		}
	}
	return best, found
}

func before(a, b musicbrainz.Release) bool {
	if ao, bo := a.Status == "Official", b.Status == "Official"; ao != bo {
		return ao
	}
	if (a.Date == "") != (b.Date == "") {
		return a.Date != ""
	}
	if a.Date != b.Date {
		return a.Date < b.Date
	}
	return a.Tracks < b.Tracks
}

// checkArtist fetches an artist's country, area and genres.
func (l *Linker) checkArtist(ctx context.Context, a model.Artist) error {
	ma, err := l.MB.LookupArtist(ctx, a.MBID)
	if errors.Is(err, musicbrainz.ErrNotFound) {
		return l.Store.MarkArtistChecked(a.ID)
	} else if err != nil {
		return err
	}
	m := store.ArtistMeta{MBID: a.MBID, Name: ma.Name, Country: ma.Country, Area: ma.Area}
	for _, g := range ma.Genres {
		m.Genres = append(m.Genres, store.Genre{Name: g.Name, Votes: g.Votes})
	}
	return l.Store.SetArtist(m)
}

// popularity asks ListenBrainz how popular newly posted albums are. A
// failure is only logged: the next pass asks again, within the window.
func (l *Linker) popularity(ctx context.Context) {
	if l.LB == nil {
		return
	}
	for {
		ps, err := l.Store.PostsWithoutPopularity(listenbrainz.MaxBatch)
		if err != nil {
			l.Log.Error("linker: list posts without popularity", "err", err)
			return
		}
		if len(ps) == 0 {
			return
		}
		var mbids []string
		for _, p := range ps {
			if !slices.Contains(mbids, p.MBID) {
				mbids = append(mbids, p.MBID)
			}
		}
		pops, err := l.LB.Popularity(ctx, mbids)
		if err != nil {
			if ctx.Err() == nil {
				l.Log.Warn("linker: listenbrainz popularity", "err", err)
			}
			return
		}
		out := make(map[uint]store.Popularity, len(ps))
		for _, p := range ps {
			out[p.PostID] = store.Popularity{Listens: pops[p.MBID].Listens, Listeners: pops[p.MBID].Listeners}
		}
		if err := l.Store.SetPopularity(out); err != nil {
			l.Log.Error("linker: store popularity", "err", err)
			return
		}
		if len(ps) < listenbrainz.MaxBatch {
			return
		}
	}
}

// findMBID looks the release's links up on MusicBrainz. The first one that
// MusicBrainz knows gives the release its MBID; the release then gets its
// links on the next check.
func (l *Linker) findMBID(ctx context.Context, r model.Release) error {
	ls, err := l.Store.Links(r.ID)
	if err != nil {
		return err
	}
	for i, rl := range ls {
		if i > 0 {
			if err := l.rest(ctx); err != nil {
				return err
			}
		}
		a, err := l.MB.LookupURL(ctx, rl.Link().URL())
		if errors.Is(err, musicbrainz.ErrNotFound) {
			continue
		} else if err != nil {
			return err
		}
		_, err = l.Store.AttachMBID(r.ID, store.NewPost{
			MBID: &a.MBID, Title: a.Title, Artist: a.Artist, Year: a.Year, CoverURL: musicbrainz.CoverURL(a.MBID),
		})
		var ve store.ValidationError
		if errors.As(err, &ve) {
			// Bad data from MusicBrainz: don't let it block the queue.
			l.Log.Warn("linker: can't attach MBID", "release", r.ID, "mbid", a.MBID, "err", err)
			break
		} else if err != nil {
			return err
		}
		l.Log.Info("linker: matched release to MusicBrainz", "release", r.ID, "mbid", a.MBID)
		return nil
	}
	return l.Store.MarkChecked(r.ID)
}
