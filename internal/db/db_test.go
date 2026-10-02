package db

import (
	"testing"

	"github.com/EmasXP/aotd/internal/model"
)

// legacySchema is the users and posts tables as the first release created
// them on SQLite, before releases existed.
const legacySchema = "CREATE TABLE `users` (`id` integer PRIMARY KEY AUTOINCREMENT,`username` text NOT NULL,`email` text NOT NULL,`password_hash` text NOT NULL,`display_name` text NOT NULL,`bio` text,`avatar_path` text,`created_at` datetime,`updated_at` datetime);" +
	"CREATE TABLE `posts` (`id` integer PRIMARY KEY AUTOINCREMENT,`user_id` integer NOT NULL,`post_date` text NOT NULL,`mb_id` text,`title` text NOT NULL,`artist` text NOT NULL,`year` integer,`cover_url` text,`note` text,`created_at` datetime,`updated_at` datetime,CONSTRAINT `fk_posts_user` FOREIGN KEY (`user_id`) REFERENCES `users`(`id`) ON DELETE CASCADE);" +
	"CREATE INDEX `idx_posts_created_at` ON `posts`(`created_at`);" +
	"CREATE INDEX `idx_posts_mb_id` ON `posts`(`mb_id`);" +
	"CREATE INDEX `idx_posts_post_date` ON `posts`(`post_date`);" +
	"CREATE UNIQUE INDEX `idx_user_day` ON `posts`(`user_id`,`post_date`);"

func TestMigrateMovesAlbumsToReleases(t *testing.T) {
	g, err := Open("file:" + t.Name() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Exec(legacySchema).Error; err != nil {
		t.Fatal(err)
	}
	const okc = "b1392450-e666-3926-a536-22c65f834433"
	err = g.Exec("INSERT INTO users (username, email, password_hash, display_name) VALUES ('alice','a@x','x','A'), ('bob','b@x','x','B');" +
		"INSERT INTO posts (user_id, post_date, mb_id, title, artist, year, cover_url, note) VALUES" +
		" (1, '2026-01-01', '" + okc + "', 'OK Computer', 'Radiohead', 1997, 'old', 'first')," +
		" (2, '2026-01-02', '" + okc + "', 'OK Computer', 'Radiohead', 1997, 'new', '')," +
		" (1, '2026-01-02', NULL, 'Garage Demo', 'Bob''s Band', 2024, '', '')," +
		" (2, '2026-01-03', NULL, 'Garage Demo', 'Bob''s Band', 2024, '', '')").Error
	if err != nil {
		t.Fatal(err)
	}
	for range 2 { // the second run must be a no-op
		if err := Migrate(g); err != nil {
			t.Fatal(err)
		}
	}

	var posts []model.Post
	if err := g.Preload("Release").Order("id").Find(&posts).Error; err != nil {
		t.Fatal(err)
	}
	if len(posts) != 4 {
		t.Fatalf("%d posts", len(posts))
	}
	if posts[0].ReleaseID != posts[1].ReleaseID {
		t.Error("posts of one MBID got separate releases")
	}
	if r := posts[0].Release; r.MBID == nil || *r.MBID != okc || r.Title != "OK Computer" || r.Year != 1997 || r.CoverURL != "new" {
		t.Errorf("MBID release = %+v, want the newest post's metadata", r)
	}
	if posts[0].Note != "first" {
		t.Errorf("note = %q", posts[0].Note)
	}
	if posts[2].ReleaseID == posts[3].ReleaseID {
		t.Error("manual entries share a release")
	}
	if r := posts[2].Release; r.MBID != nil || r.Artist != "Bob's Band" || r.Year != 2024 {
		t.Errorf("manual release = %+v", r)
	}
	var n int64
	g.Model(&model.Release{}).Count(&n)
	if n != 3 {
		t.Errorf("%d releases, want 3", n)
	}
	for _, col := range []string{"mb_id", "title", "artist", "year", "cover_url"} {
		if g.Migrator().HasColumn("posts", col) {
			t.Errorf("posts.%s still exists", col)
		}
	}
	err = g.Exec("INSERT INTO posts (user_id, post_date) VALUES (1, '2026-02-01')").Error
	if err == nil {
		t.Error("posts.release_id accepts NULL")
	}
	// The daily limit still holds after the table was rebuilt.
	err = g.Omit("Release").Create(&model.Post{UserID: 1, PostDate: "2026-01-01", ReleaseID: posts[0].ReleaseID}).Error
	if err == nil {
		t.Error("unique (user_id, post_date) lost in migration")
	}
}
