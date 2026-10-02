// Package linker keeps releases' streaming links up to date from
// MusicBrainz, in the background.
package linker

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/EmasXP/aotd/internal/links"
	"github.com/EmasXP/aotd/internal/model"
	"github.com/EmasXP/aotd/internal/musicbrainz"
	"github.com/EmasXP/aotd/internal/store"
)

type Linker struct {
	Store *store.Store
	MB    *musicbrainz.Client
	Log   *slog.Logger
	// Pause between releases leaves most of the MusicBrainz rate limit to
	// people searching.
	Pause time.Duration
	kick  chan struct{}
}

func New(st *store.Store, mb *musicbrainz.Client) *Linker {
	return &Linker{Store: st, MB: mb, Log: slog.Default(), Pause: 2 * time.Second, kick: make(chan struct{}, 1)}
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

// Pass checks every release that is due and returns how many it checked. It
// stops early if MusicBrainz fails, leaving the rest for the next pass.
func (l *Linker) Pass(ctx context.Context) int {
	n := 0
	for {
		rs, err := l.Store.ReleasesToCheck(20)
		if err != nil {
			l.Log.Error("linker: list releases", "err", err)
			return n
		}
		if len(rs) == 0 {
			return n
		}
		for _, r := range rs {
			if err := l.check(ctx, r); err != nil {
				if ctx.Err() == nil {
					l.Log.Warn("linker: check release", "release", r.ID, "err", err)
				}
				return n
			}
			n++
			select {
			case <-ctx.Done():
				return n
			case <-time.After(l.Pause):
			}
		}
	}
}

// check adds the links MusicBrainz knows for a release's group.
func (l *Linker) check(ctx context.Context, r model.Release) error {
	urls, err := l.MB.ReleaseGroupURLs(ctx, *r.MBID)
	if err != nil && !errors.Is(err, musicbrainz.ErrNotFound) {
		return err
	}
	var found []links.Link
	for _, u := range urls {
		if lk, ok := links.Parse(u); ok && !slices.Contains(found, lk) {
			found = append(found, lk)
		}
	}
	if err := l.Store.AddLinks(r.ID, found); err != nil {
		return err
	}
	return l.Store.MarkChecked(r.ID)
}
