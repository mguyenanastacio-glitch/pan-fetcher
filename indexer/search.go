package indexer

import "time"

// SearchResult is a unified search result from any indexer.
type SearchResult struct {
	Title       string    `json:"title"`
	MagnetURL   string    `json:"magnet_url"`
	TorrentURL  string    `json:"torrent_url,omitempty"`
	Size        int64     `json:"size"`
	SizeFmt     string    `json:"-"` // pre-formatted size string for display
	Seeders     int       `json:"seeders"`
	Leechers    int       `json:"leechers"`
	PublishDate time.Time `json:"publish_date"`
	Category    string    `json:"category"`
	IndexerID   string    `json:"indexer_id"`
	IndexerName string    `json:"indexer_name"`
	InfoHash    string    `json:"info_hash,omitempty"`
	PageURL     string    `json:"page_url,omitempty"`
}

// SearchRequest is the input for a search operation.
type SearchRequest struct {
	Query     string
	Season    int
	Episode   int
	Category  string
	Sort      string   // "seeds" (default), "size", "date"
	Indexers  []string // specific indexer IDs to search (empty = all enabled)
	Limit     int
}

// IndexerInfo is the runtime info for a configured indexer.
type IndexerInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Language  string `json:"language"`
	SiteLink  string `json:"site_link"`
	Enabled   bool   `json:"enabled"`
	Healthy   bool   `json:"healthy"`
	HasLogin  bool   `json:"has_login"`
	LastError string `json:"last_error,omitempty"`
	LastTest  string `json:"last_test,omitempty"`
}
