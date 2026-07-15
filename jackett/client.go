// Package jackett provides a minimal Jackett/Prowlarr API client.
package jackett

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	client    = &http.Client{Timeout: 60 * time.Second}
	cookieMu  sync.Mutex
	cookieJar *cookiejar.Jar
)

func init() {
	cookieJar, _ = cookiejar.New(nil)
}

// SetProxy configures an HTTP proxy for the Jackett client.
func SetProxy(proxyURL string) {
	if proxyURL == "" {
		client.Transport = nil
		return
	}
	if u, err := url.Parse(proxyURL); err == nil {
		client.Transport = &http.Transport{
			Proxy: http.ProxyURL(u),
		}
	}
}

// Login authenticates with the Jackett admin UI and stores the session cookie.
// Uses cfg.AdminPassword if set, otherwise falls back to cfg.APIKey.
func Login(cfg Config) error {
	password := cfg.AdminPassword
	if password == "" {
		password = cfg.APIKey
	}
	loginURL := strings.TrimRight(cfg.URL, "/") + "/UI/Dashboard"
	form := url.Values{}
	form.Set("password", password)

	req, err := http.NewRequest("POST", loginURL, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	defer resp.Body.Close()

	// Jackett redirects to Dashboard on success, back to Login on failure
	if resp.StatusCode == http.StatusOK {
		cookieMu.Lock()
		cookieJar.SetCookies(req.URL, resp.Cookies())
		// Also set cookies on the base URL for API calls
		baseURL, _ := url.Parse(strings.TrimRight(cfg.URL, "/"))
		cookieJar.SetCookies(baseURL, resp.Cookies())
		client.Jar = cookieJar
		cookieMu.Unlock()
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("login failed (HTTP %d): %s — check admin password", resp.StatusCode, strings.TrimSpace(string(body)))
}

// Config holds Jackett connection settings.
type Config struct {
	URL           string // e.g. "http://localhost:9117"
	APIKey        string
	AdminPassword string // web UI admin password (falls back to APIKey if empty)
}

// Result is a single search result from Jackett.
type Result struct {
	Title       string
	Link        string
	MagnetURI   string
	Size        int64
	Seeders     int
	Peers       int
	Tracker     string
	TrackerName string
	Category    string
	PublishDate string
}

// IndexerInfo is a Jackett/Prowlarr configured indexer.
type IndexerInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	SiteLink    string `json:"site_link"`
	Language    string `json:"language"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Status      int    `json:"status"`
	Configured  bool   `json:"configured,omitempty"`
}

// rss is the Jackett/Prowlarr RSS XML structure.
type rss struct {
	XMLName xml.Name `xml:"rss"`
	Channel channel  `xml:"channel"`
}

type channel struct {
	Items []item `xml:"item"`
}

type item struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Comments    string    `xml:"comments"`
	GUID        string    `xml:"guid"`
	MagnetURI   string    `xml:"magnetURI"`
	Size        string    `xml:"size"`
	Seeders     string    `xml:"seeders"`
	Peers       string    `xml:"peers"`
	Tracker     trackerEl `xml:"jackettindexer"`
	Category    string    `xml:"category"`
	PubDate     string    `xml:"pubDate"`
	Enclosure   enclosure `xml:"enclosure"`
	TorznabAttrs []torznabAttr `xml:"http://torznab.com/schemas/2015/feed attr"`
}

type trackerEl struct {
	ID   string `xml:"id,attr"`
	Name string `xml:",chardata"`
}

type torznabAttr struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

func (it *item) attr(name string) string {
	for _, a := range it.TorznabAttrs {
		if a.Name == name {
			return a.Value
		}
	}
	return ""
}

type enclosure struct {
	URL  string `xml:"url,attr"`
	Type string `xml:"type,attr"`
}

// Test verifies the Jackett connection by hitting the indexers endpoint.
func Test(cfg Config) error {
	u, err := url.Parse(strings.TrimRight(cfg.URL, "/") + "/api/v2.0/indexers/all/results/torznab/api")
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	q := u.Query()
	q.Set("apikey", cfg.APIKey)
	q.Set("t", "caps")
	u.RawQuery = q.Encode()

	resp, err := client.Get(u.String())
	if err != nil {
		return fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("API Key 无效 (HTTP 401)")
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// ListIndexers returns all configured indexers from a Jackett/Prowlarr instance
// using the Torznab t=indexers API (XML), which works with API key auth.
func ListIndexers(cfg Config) ([]IndexerInfo, error) {
	u, err := url.Parse(strings.TrimRight(cfg.URL, "/") + "/api/v2.0/indexers/all/results/torznab/api")
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	q := u.Query()
	q.Set("apikey", cfg.APIKey)
	q.Set("t", "indexers")
	q.Set("configured", "true")
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// Parse XML response
	type idxXML struct {
		XMLName     xml.Name `xml:"indexer"`
		ID          string   `xml:"id,attr"`
		Configured  string   `xml:"configured,attr"`
		Title       string   `xml:"title"`
		Description string   `xml:"description"`
		Link        string   `xml:"link"`
		Language    string   `xml:"language"`
		Type        string   `xml:"type"`
	}
	type indexersXML struct {
		XMLName  xml.Name `xml:"indexers"`
		Indexers []idxXML `xml:"indexer"`
	}

	var data indexersXML
	if err := xml.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("xml decode: %w", err)
	}

	var indexers []IndexerInfo
	for _, idx := range data.Indexers {
		if idx.Configured != "true" {
			continue // only show configured indexers
		}
		indexers = append(indexers, IndexerInfo{
			ID:          idx.ID,
			Name:        idx.Title,
			SiteLink:    idx.Link,
			Language:    idx.Language,
			Type:        idx.Type,
			Description: idx.Description,
			Status:      1,
		})
	}
	return indexers, nil
}

// ListAllIndexers returns ALL indexers (including unconfigured) from a Jackett instance.
func ListAllIndexers(cfg Config) ([]IndexerInfo, error) {
	u, err := url.Parse(strings.TrimRight(cfg.URL, "/") + "/api/v2.0/indexers/all/results/torznab/api")
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %w", err)
	}
	q := u.Query()
	q.Set("apikey", cfg.APIKey)
	q.Set("t", "indexers")
	// No configured filter — return all
	u.RawQuery = q.Encode()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	type idxXML struct {
		XMLName     xml.Name `xml:"indexer"`
		ID          string   `xml:"id,attr"`
		Configured  string   `xml:"configured,attr"`
		Title       string   `xml:"title"`
		Description string   `xml:"description"`
		Link        string   `xml:"link"`
		Language    string   `xml:"language"`
		Type        string   `xml:"type"`
	}
	type indexersXML struct {
		XMLName  xml.Name `xml:"indexers"`
		Indexers []idxXML `xml:"indexer"`
	}

	var data indexersXML
	if err := xml.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("xml decode: %w", err)
	}

	var indexers []IndexerInfo
	for _, idx := range data.Indexers {
		indexers = append(indexers, IndexerInfo{
			ID:          idx.ID,
			Name:        idx.Title,
			SiteLink:    idx.Link,
			Language:    idx.Language,
			Type:        idx.Type,
			Description: idx.Description,
			Status:      1,
			Configured:  idx.Configured == "true",
		})
	}
	return indexers, nil
}

// Search queries a Jackett/Prowlarr instance and returns parsed results.
// offset is the starting offset for pagination (0 = first page).
func Search(cfg Config, query string, categories []int, offset int) ([]Result, error) {
	u, err := url.Parse(strings.TrimRight(cfg.URL, "/") + "/api/v2.0/indexers/all/results/torznab/api")
	if err != nil {
		return nil, fmt.Errorf("jackett url: %w", err)
	}
	q := u.Query()
	q.Set("apikey", cfg.APIKey)
	q.Set("t", "search")
	q.Set("q", query)
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	if len(categories) > 0 {
		catStrs := make([]string, len(categories))
		for i, c := range categories {
			catStrs[i] = strconv.Itoa(c)
		}
		q.Set("cat", strings.Join(catStrs, ","))
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jackett request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("jackett HTTP %d: %s", resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("jackett read: %w", err)
	}

	var rssData rss
	if err := xml.Unmarshal(data, &rssData); err != nil {
		return nil, fmt.Errorf("jackett xml: %w", err)
	}

	results := make([]Result, 0, len(rssData.Channel.Items))
	for _, it := range rssData.Channel.Items {
		// Use <comments> (site page) as Link if available, fall back to <guid>/<link>
		pageLink := it.Comments
		if pageLink == "" {
			pageLink = it.GUID
		}
		if pageLink == "" {
			pageLink = it.Link
		}
		// Use enclosure URL for magnet/torrent if available
		magnetLink := it.MagnetURI
		if magnetLink == "" && it.Enclosure.URL != "" && it.Enclosure.Type == "application/x-bittorrent" {
			magnetLink = it.Enclosure.URL
		}
		// Use GUID (direct .torrent) if no magnet
		if magnetLink == "" && it.GUID != "" {
			magnetLink = it.GUID
		}
		// Fall back to Link
		if magnetLink == "" {
			magnetLink = it.Link
		}
		// Parse seeders from torznab attrs (torznab:attr), fallback to direct element
		seedersStr := it.attr("seeders")
		if seedersStr == "" {
			seedersStr = it.Seeders
		}
		seeders, _ := strconv.Atoi(strings.TrimSpace(seedersStr))
		peersStr := it.attr("peers")
		if peersStr == "" {
			peersStr = it.Peers
		}
		peers, _ := strconv.Atoi(strings.TrimSpace(peersStr))

		r := Result{
			Title:       it.Title,
			Link:        pageLink,
			MagnetURI:   magnetLink,
			Tracker:     it.Tracker.ID,
			TrackerName: strings.TrimSpace(it.Tracker.Name),
			Category:    it.Category,
			PublishDate: it.PubDate,
			Seeders:     seeders,
			Peers:       peers,
		}
		if s, err := strconv.ParseInt(it.Size, 10, 64); err == nil {
			r.Size = s
		}
		results = append(results, r)
	}
	return results, nil
}

// AddIndexer configures an indexer in Jackett by its ID.
// Uses Jackett's admin API: POST /api/v2.0/indexers
// AddIndexer adds/activates an indexer in Jackett by fetching its default config
// and posting it to the indexer's config endpoint.
// Requires admin login cookie; calls Login automatically.
func AddIndexer(cfg Config, id string) error {
	// Login for admin API access
	if err := Login(cfg); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	baseURL := strings.TrimRight(cfg.URL, "/")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Step 1: GET default config
	cfgURL, err := url.Parse(baseURL + "/api/v2.0/indexers/" + id + "/config")
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "GET", cfgURL.String(), nil)
	if err != nil {
		return fmt.Errorf("create GET request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("get config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("HTTP %d getting config: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	configJSON, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	// Step 2: POST config to add the indexer
	postURL, err := url.Parse(baseURL + "/api/v2.0/indexers/" + id + "/config")
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	postReq, err := http.NewRequestWithContext(ctx, "POST", postURL.String(), strings.NewReader(string(configJSON)))
	if err != nil {
		return fmt.Errorf("create POST request: %w", err)
	}
	postReq.Header.Set("Content-Type", "application/json")
	postReq.Header.Set("Accept", "application/json")

	postResp, err := client.Do(postReq)
	if err != nil {
		return fmt.Errorf("add indexer: %w", err)
	}
	defer postResp.Body.Close()

	if postResp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(postResp.Body, 1024))
		return fmt.Errorf("HTTP %d adding indexer: %s", postResp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// DeleteIndexer removes an indexer from Jackett.
// Requires admin login cookie; calls Login automatically.
func DeleteIndexer(cfg Config, id string) error {
	if err := Login(cfg); err != nil {
		return fmt.Errorf("login: %w", err)
	}

	baseURL := strings.TrimRight(cfg.URL, "/")
	delURL, err := url.Parse(baseURL + "/api/v2.0/indexers/" + id)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "DELETE", delURL.String(), nil)
	if err != nil {
		return fmt.Errorf("create DELETE request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("delete indexer: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("HTTP %d deleting indexer: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}
