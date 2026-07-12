package store

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mguyenanastacio-glitch/pan-fetcher/rsssite"
)

type Store struct {
	DBInstance *sql.DB
}

// New creates a new Store instance with the given database connection.
// If db is nil, it opens a SQLite database at the default path "db.sqlite".
// Deprecated: Use NewWithPath to specify a custom database path.
func New(db *sql.DB) *Store {
	if db == nil {
		store, err := NewWithPath("db.sqlite")
		if err != nil {
			panic(err)
		}
		return store
	}
	if err := initSchema(db); err != nil {
		panic(err)
	}
	return &Store{
		DBInstance: db,
	}
}

// NewWithPath creates a new Store instance with a database at the specified path.
// If path is empty, it defaults to "db.sqlite".
func NewWithPath(path string) (*Store, error) {
	if path == "" {
		path = "db.sqlite"
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}
	if err := initSchema(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{
		DBInstance: db,
	}, nil
}

func initSchema(db *sql.DB) error {
	if _, err := db.Exec("CREATE TABLE if not exists `rss_items` (`id` INTEGER PRIMARY KEY AUTOINCREMENT, `link` VARCHAR(255), `title` VARCHAR(255), `guid` VARCHAR(255), `pubDate` DATETIME, `creator` VARCHAR(255), `summary` TEXT, `content` VARCHAR(255), `isoDate` DATETIME, `categories` VARCHAR(255), `contentSnippet` VARCHAR(255), `done` TINYINT(1) DEFAULT 0, `magnet` VARCHAR(255) NOT NULL, `createdAt` DATETIME NOT NULL, `updatedAt` DATETIME NOT NULL)"); err != nil {
		return err
	}
	if _, err := db.Exec("CREATE TABLE if not exists `sites_status` (`id` INTEGER PRIMARY KEY AUTOINCREMENT, `name` VARCHAR(255), `needLogin` TINYINT(1), `abnormalOp` TINYINT(1), `createdAt` DATETIME NOT NULL, `updatedAt` DATETIME NOT NULL)"); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE if not exists subscriptions (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name VARCHAR(255) NOT NULL,
		tmdb_id INTEGER DEFAULT 0,
		media_type VARCHAR(16) DEFAULT 'tv',
		season INTEGER DEFAULT 0,
		cid VARCHAR(64) DEFAULT '',
		savepath VARCHAR(255) DEFAULT '',
		filter_rules TEXT DEFAULT '',
		enabled TINYINT(1) DEFAULT 1,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	)`); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE if not exists subscription_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		subscription_id INTEGER NOT NULL,
		title VARCHAR(512),
		magnet VARCHAR(1024),
		enclosure VARCHAR(1024),
		episode INTEGER DEFAULT 0,
		season INTEGER DEFAULT 0,
		submitted_at DATETIME NOT NULL,
		FOREIGN KEY (subscription_id) REFERENCES subscriptions(id)
	)`); err != nil {
		return err
	}
	return nil
}

func (s *Store) SaveMagnetItems(items []rsssite.MagnetItem) error {
	now := time.Now()
	for _, item := range items {
		sql := "INSERT INTO rss_items (`link`,`title`,`content`,`magnet`,`done`,`createdAt`,`updatedAt`) VALUES (?,?,?,?,?,?,?)"
		_, err := s.DBInstance.Exec(sql, item.Link, item.Title, item.Content, item.Magnet, 0, now, now)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) HasItem(magnet string) bool {
	var count int
	s.DBInstance.QueryRow("SELECT count(*) AS num FROM rss_items WHERE magnet = ?", magnet).Scan(&count)
	return count > 0
}

// @TODO 替换 HasItem. 注意目前 magnet 存的长度是 VARCHAR(255)。有tracker的长URI会存不了.
func (s *Store) HasMagnetByXt(magnet string) bool {
	var count int
	u, err := url.Parse(magnet)
	if err != nil {
		return false
	}
	params := u.Query()
	xt := params.Get("xt")
	s.DBInstance.QueryRow("SELECT count(*) AS num FROM rss_items WHERE magnet LIKE ?", "%"+xt+"%").Scan(&count)
	return count > 0
}

func (s *Store) Close() error {
	return s.DBInstance.Close()
}

// ---------- subscriptions ----------

// Subscription represents a user's media subscription.
type Subscription struct {
	ID          int
	Name        string
	TmdbID      int
	MediaType   string
	Season      int
	Cid         string
	Savepath    string
	FilterRules string
	Enabled     bool
}

// AddSubscription inserts a new subscription.
func (s *Store) AddSubscription(sub *Subscription) (int64, error) {
	now := time.Now()
	res, err := s.DBInstance.Exec(
		`INSERT INTO subscriptions (name, tmdb_id, media_type, season, cid, savepath, filter_rules, enabled, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?,?)`,
		sub.Name, sub.TmdbID, sub.MediaType, sub.Season, sub.Cid, sub.Savepath, sub.FilterRules, sub.Enabled, now, now,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateSubscription updates an existing subscription.
func (s *Store) UpdateSubscription(sub *Subscription) error {
	now := time.Now()
	_, err := s.DBInstance.Exec(
		`UPDATE subscriptions SET name=?, tmdb_id=?, media_type=?, season=?, cid=?, savepath=?, filter_rules=?, enabled=?, updated_at=? WHERE id=?`,
		sub.Name, sub.TmdbID, sub.MediaType, sub.Season, sub.Cid, sub.Savepath, sub.FilterRules, sub.Enabled, now, sub.ID,
	)
	return err
}

// DeleteSubscription removes a subscription by ID.
func (s *Store) DeleteSubscription(id int) error {
	_, err := s.DBInstance.Exec("DELETE FROM subscriptions WHERE id=?", id)
	return err
}

// ListSubscriptions returns all subscriptions.
func (s *Store) ListSubscriptions() ([]Subscription, error) {
	rows, err := s.DBInstance.Query(
		"SELECT id, name, tmdb_id, media_type, season, cid, savepath, filter_rules, enabled FROM subscriptions ORDER BY id DESC",
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Subscription
	for rows.Next() {
		var sub Subscription
		if err := rows.Scan(&sub.ID, &sub.Name, &sub.TmdbID, &sub.MediaType, &sub.Season, &sub.Cid, &sub.Savepath, &sub.FilterRules, &sub.Enabled); err != nil {
			return nil, err
		}
		result = append(result, sub)
	}
	return result, nil
}

// GetSubscription returns a single subscription by ID.
func (s *Store) GetSubscription(id int) (*Subscription, error) {
	sub := &Subscription{}
	err := s.DBInstance.QueryRow(
		"SELECT id, name, tmdb_id, media_type, season, cid, savepath, filter_rules, enabled FROM subscriptions WHERE id=?",
		id,
	).Scan(&sub.ID, &sub.Name, &sub.TmdbID, &sub.MediaType, &sub.Season, &sub.Cid, &sub.Savepath, &sub.FilterRules, &sub.Enabled)
	if err != nil {
		return nil, err
	}
	return sub, nil
}

// ---------- subscription history ----------

// HasSubmitted checks if a given episode for a subscription has already been submitted.
func (s *Store) HasSubmitted(subID int, episode, season int) bool {
	var count int
	s.DBInstance.QueryRow(
		"SELECT COUNT(*) FROM subscription_history WHERE subscription_id=? AND episode=? AND season=?",
		subID, episode, season,
	).Scan(&count)
	return count > 0
}

// HasSubmittedMagnet checks if a magnet link was already submitted for any subscription.
func (s *Store) HasSubmittedMagnet(magnet string) bool {
	var count int
	s.DBInstance.QueryRow(
		"SELECT COUNT(*) FROM subscription_history WHERE magnet=?",
		magnet,
	).Scan(&count)
	return count > 0
}

// RecordSubmission saves a submission to history.
func (s *Store) RecordSubmission(subID int, title, magnet, enclosure string, episode, season int) error {
	_, err := s.DBInstance.Exec(
		`INSERT INTO subscription_history (subscription_id, title, magnet, enclosure, episode, season, submitted_at) VALUES (?,?,?,?,?,?,?)`,
		subID, title, magnet, enclosure, episode, season, time.Now(),
	)
	return err
}
