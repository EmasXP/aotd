# AOTD — Album Of The Day

Post one album a day, follow friends, check in on the AOTDs you've listened to,
comment, and gather in groups. Go + GORM + server-rendered HTML with htmx.

```sh
go run ./cmd/aotd -seed     # http://localhost:8080 — demo users alice/bob/carol, password "spin the black circle"
go test ./...
```

## Configuration (env)

| Variable          | Default                         | Notes                                              |
|-------------------|---------------------------------|----------------------------------------------------|
| `AOTD_ADDR`       | `:8080`                         | Listen address                                     |
| `AOTD_DATA_DIR`   | `data`                          | SQLite database and avatars                        |
| `AOTD_DB_DSN`     | `$AOTD_DATA_DIR/aotd.db`        | A `postgres://…` URL switches to Postgres          |
| `AOTD_DEV`        | `1`                             | `0` sets `Secure` on cookies; use it behind HTTPS  |
| `AOTD_MB_CONTACT` | project URL                     | Contact in the MusicBrainz User-Agent (required by their API policy) |
| `AOTD_TRUSTED_PROXIES` | _(none)_                   | Comma-separated proxy IPs/CIDRs, e.g. `10.0.0.5,172.16.0.0/12`. Only these may set the client IP via `X-Forwarded-For` (or `X-Real-IP`) |

## Rules

- **One AOTD per day, CET** (`Europe/Berlin`, DST aware). Enforced by a unique
  index on `(user_id, post_date)`. Today's post can be edited (note) or deleted,
  which frees the day.
- **MusicBrainz**: album + artist search against release groups (type Album),
  rate limited to 1 req/s and cached. On submit the server re-fetches the MBID
  rather than trusting the form. Albums not on MusicBrainz can be posted as
  typed, without an MBID. Covers come from the Cover Art Archive.
- **Releases**: posts of the same album share a release (one per MBID; each
  manual entry gets its own). Its streaming links (Spotify, Apple Music,
  Deezer, Tidal, Bandcamp) come from the URLs MusicBrainz has on the group's
  releases, fetched in the background and refreshed monthly. Each card shows
  one link per service: the one most posts were made with.
- **Pasting a link**: a streaming link in the album field resolves to the
  release AOTD already has for it, or the album MusicBrainz links it to. If
  neither knows it, Spotify's oEmbed (no credentials) gives the title and
  cover, and MusicBrainz candidates are offered to confirm, never preselected.
- **Matching later**: releases without an MBID that have a link are looked up
  on MusicBrainz daily for a month, then weekly; once someone adds the link
  there, the release gets the MBID (merging with the release that already has
  it, if any). A post's author can also pick the match from the post page. A
  release's MBID never changes once set.
- **Check-ins** belong to a post, not an album, and only on other people's posts.
- **Comments** are one level deep: replying to a reply attaches to the thread's
  top comment.
- **Groups** are open to join; a group page is its members' regular AOTDs.

## Security notes

- Passwords: argon2id (64 MiB, t=3, p=2), PHC-encoded so parameters can be
  raised later (hashes are upgraded on login). 10–128 chars, checked against the
  10k most common passwords. Login errors don't reveal whether a user exists,
  and unknown users cost the same hashing time.
- Sessions: random 256-bit token in an `HttpOnly`, `SameSite=Lax` cookie; only
  its SHA-256 is stored. Rotated on login, deleted on logout; a password change
  logs out other devices.
- CSRF: Go's `http.CrossOriginProtection` plus SameSite cookies.
- Strict CSP (no inline scripts or styles), `nosniff`, no framing. Avatars are
  decoded and re-encoded to 256×256 JPEG (drops EXIF and any payload).
- Login/signup rate limits are in-memory, keyed by client IP. Behind a reverse
  proxy, set `AOTD_TRUSTED_PROXIES`. Forwarded headers from anyone else are
  ignored, and `X-Forwarded-For` is read right to left, so a client can't
  spoof its IP by sending the header itself.

## Layout

```
cmd/aotd             main, demo seed
internal/auth        password hashing/policy, sessions, rate limiter
internal/avatar      upload validation and re-encoding
internal/day         CET date logic
internal/db          GORM open (SQLite/Postgres) and migrate
internal/linker      background MusicBrainz checks: streaming links, late MBID matches
internal/links       streaming link parsing and canonical URLs
internal/model       GORM models
internal/musicbrainz MusicBrainz client
internal/spotify     Spotify oEmbed client
internal/store       business rules and queries
internal/web         handlers, middleware, templates, static assets
```
