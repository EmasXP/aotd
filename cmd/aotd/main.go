// Command aotd runs the Album Of The Day web app.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/EmasXP/aotd/internal/config"
	"github.com/EmasXP/aotd/internal/db"
	"github.com/EmasXP/aotd/internal/linker"
	"github.com/EmasXP/aotd/internal/musicbrainz"
	"github.com/EmasXP/aotd/internal/spotify"
	"github.com/EmasXP/aotd/internal/store"
	"github.com/EmasXP/aotd/internal/web"
)

func main() {
	seedFlag := flag.Bool("seed", false, "add demo users, posts and a group, then start")
	flag.Parse()
	if err := run(*seedFlag); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(seedDemo bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	g, err := db.Open(cfg.DSN)
	if err != nil {
		return err
	}
	if err := db.Migrate(g); err != nil {
		return err
	}
	st := store.New(g)
	mb := musicbrainz.New(cfg.MBContact)
	srv, err := web.New(st, mb, cfg.DataDir, cfg.Dev)
	if err != nil {
		return err
	}
	srv.TrustedProxies = cfg.TrustedProxies
	srv.Linker = linker.New(st, mb)
	srv.Spotify = spotify.New(cfg.SpotifyClientID, cfg.SpotifyClientSecret)
	if seedDemo {
		if err := seed(st, srv); err != nil {
			return err
		}
	}
	go srv.PurgeSessions(time.Hour)

	hs := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.Linker.Run(ctx, 10*time.Minute)
	errc := make(chan error, 1)
	go func() {
		addr := cfg.Addr
		if strings.HasPrefix(addr, ":") {
			addr = "localhost" + addr
		}
		slog.Info("listening", "url", "http://"+addr, "db", redactDSN(cfg.DSN), "dev", cfg.Dev, "trusted_proxies", cfg.TrustedProxies,
			"spotify_api", srv.Spotify.HasAPI())
		errc <- hs.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return hs.Shutdown(shutdownCtx)
	}
	return nil
}

func redactDSN(dsn string) string {
	if i := strings.Index(dsn, "@"); i > 0 && strings.Contains(dsn, "://") {
		return dsn[:strings.Index(dsn, "://")+3] + "…" + dsn[i:]
	}
	return dsn
}
