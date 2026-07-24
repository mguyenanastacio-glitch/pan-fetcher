package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/mguyenanastacio-glitch/pan-fetcher/indexer"
)

// dedupKey returns a compact deduplication key.  Uses the most specific
// identifier available: InfoHash > MagnetURL > Title+Size.
// The leading char encodes the type: 'h'=InfoHash, 'm'=MagnetURL, 't'=Title+Size.
func dedupKey(r indexer.SearchResult) string {
	if r.InfoHash != "" {
		return "h" + r.InfoHash
	}
	if r.MagnetURL != "" {
		return "m" + r.MagnetURL
	}
	// Avoid fmt.Sprintf in hot path
	return "t" + r.Title + "\x00" + strconv.FormatInt(r.Size, 10)
}

// dedupSlice removes duplicates from a slice, optionally using and updating
// an existing seen map for cross-batch dedup.  Pass nil to create a fresh set.
// Keeps the first occurrence of each key (stable ordering).
func dedupSlice(results []indexer.SearchResult, seen map[string]bool) []indexer.SearchResult {
	if seen == nil {
		seen = make(map[string]bool, len(results))
	}
	out := results[:0]
	for i := range results {
		key := dedupKey(results[i])
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, results[i])
	}
	return out
}

// savedSearch represents a user-saved search query.
type savedSearch struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Query     string `json:"query"`
	Sort      string `json:"sort"`
	CreatedAt string `json:"created_at"`
}

func (s *Server) loadSavedSearches() []savedSearch {
	data, err := os.ReadFile("saved-searches.json")
	if err != nil {
		return nil
	}
	var searches []savedSearch
	json.Unmarshal(data, &searches)
	return searches
}

func (s *Server) saveSearches(searches []savedSearch) {
	data, _ := json.MarshalIndent(searches, "", "  ")
	os.WriteFile("saved-searches.json", data, 0644)
}

func (s *Server) handleSearchSubscribe(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	q := strings.TrimSpace(r.FormValue("query"))
	name := strings.TrimSpace(r.FormValue("name"))
	rssURL := strings.TrimSpace(r.FormValue("url"))
	filter := strings.TrimSpace(r.FormValue("filter"))
	cid := strings.TrimSpace(r.FormValue("cid"))
	savepath := strings.TrimSpace(r.FormValue("savepath"))
	if name == "" {
		name = q
	}

	if rssURL == "" && q != "" {
		// Auto-build local aggregated RSS URL
		rssURL = buildRssURL(s.Port, q, nil)
	}
	if rssURL == "" {
		data := s.pageData("", tr(s.langFromAgent(), "please_fill_rss"))
		data.SearchQuery = q
		dashboardTemplate.Execute(w, data)
		return
	}

	// Save to rss.json
	if err := addRSSFeed(rssURL, name, filter, cid, savepath); err != nil {
		data := s.pageData("", tr(s.langFromAgent(), "saving_failed")+err.Error())
		data.SearchQuery = q
		dashboardTemplate.Execute(w, data)
		return
	}
	log.Printf("[search] subscribed: name=%s url=%s", name, rssURL)
	http.Redirect(w, r, "/?subscribed=1", http.StatusSeeOther)
}

// buildRssURL constructs a local aggregated RSS feed URL from search parameters.
func buildRssURL(port int, query string, indexers []string) string {
	v := url.Values{}
	v.Set("q", query)
	if len(indexers) > 0 {
		v.Set("indexers", strings.Join(indexers, ","))
	}
	v.Set("sort", "date")
	return fmt.Sprintf("http://127.0.0.1:%d/rss/search?%s", port, v.Encode())
}

// addRSSFeed appends an RSS feed entry to rss.json.
func addRSSFeed(rssURL, name, filter, cid, savepath string) error {
	// Determine site from URL
	site := "pan-fetcher"
	if u, err := url.Parse(rssURL); err == nil && u.Host != "" {
		site = u.Host
		if strings.Contains(site, "mikanani") || strings.Contains(site, "mikanime") {
			site = "mikanani.me"
		}
		if strings.Contains(site, "dmhy") {
			site = "share.dmhy.org"
		}
	}

	// Read existing rss.json
	feeds := make(map[string][]rssFeedEntry)
	if data, err := os.ReadFile(rssJsonPath); err == nil {
		json.Unmarshal(data, &feeds)
	}

	entry := rssFeedEntry{
		Name:     name,
		URL:      rssURL,
		Filter:   filter,
		Cid:      cid,
		SavePath: savepath,
		Enabled:  false, // disabled by default, user must enable in subs page
	}
	feeds[site] = append(feeds[site], entry)

	data, err := json.MarshalIndent(feeds, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(rssJsonPath, data, 0644)
}

type rssFeedEntry struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Filter   string `json:"filter,omitempty"`
	Cid      string `json:"cid,omitempty"`
	SavePath string `json:"savepath,omitempty"`
	Enabled  bool   `json:"enabled"`
}

func (s *Server) handleSearchUnsubscribe(w http.ResponseWriter, r *http.Request) {
	id := r.FormValue("id")
	searches := s.loadSavedSearches()
	filtered := searches[:0]
	for _, ss := range searches {
		if ss.ID != id {
			filtered = append(filtered, ss)
		}
	}
	s.saveSearches(filtered)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
