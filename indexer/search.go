package indexer

import (
	"strings"
	"time"
)

// SearchResult is a unified search result from any indexer.
type SearchResult struct {
	Title       string    `json:"title"`
	MagnetURL   string    `json:"magnet_url"`
	TorrentURL  string    `json:"torrent_url,omitempty"`
	Size        int64     `json:"size"`
	SizeFmt     string    `json:"-"` // pre-formatted size string for display
	DateFmt     string    `json:"-"` // pre-formatted date string for display
	Seeders     int       `json:"seeders"`
	Leechers    int       `json:"leechers"`
	PublishDate time.Time `json:"publish_date"`
	Category    string    `json:"category"`
	IndexerID   string    `json:"indexer_id"`
	IndexerName string    `json:"indexer_name"`
	InfoHash    string    `json:"info_hash,omitempty"`
	PageURL     string    `json:"page_url,omitempty"`
	Group       string    `json:"group,omitempty"` // fansub group from first [...] tag
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
	Page      int      // page number (1-based), 0 = first page
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
	Source    string `json:"source,omitempty"` // "local" or "jackett"
}

// catKeywords maps user-facing category values to keywords found in tracker category fields.
var catKeywords = map[string][]string{
	"anime": {"anime", "动漫", "动画", "animation"},
	"tv":    {"tv", "剧集", "电视剧", "tv series"},
	"movie": {"movie", "电影", "movies"},
	"music": {"music", "音乐", "audio", "mp3"},
	"other": {},
}

// FilterByCategory filters results by category string, using a case-insensitive
// substring match against each result's Category field.
// An empty category returns all results unchanged.
// Results with an empty Category field are kept (treated as unknown/all).
func FilterByCategory(results []SearchResult, category string) []SearchResult {
	category = strings.TrimSpace(category)
	if category == "" {
		return results
	}
	keywords, ok := catKeywords[category]
	if !ok || len(keywords) == 0 {
		return results
	}
	filtered := make([]SearchResult, 0, len(results))
	for _, r := range results {
		catLower := strings.ToLower(r.Category)
		// Keep results with unknown/empty category
		if catLower == "" {
			filtered = append(filtered, r)
			continue
		}
		for _, kw := range keywords {
			if strings.Contains(catLower, kw) {
				filtered = append(filtered, r)
				break
			}
		}
	}
	return filtered
}

// TorznabCategoryIDs maps user-facing category values to Torznab/Newznab category IDs for Jackett.
func TorznabCategoryIDs(category string) []int {
	switch category {
	case "movie":
		return []int{2000}
	case "tv":
		return []int{5000}
	case "anime":
		return []int{5070}
	case "music":
		return []int{3000}
	default:
		return nil
	}
}
