package media

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// TMDBClient wraps the TMDB API v3.
type TMDBClient struct {
	apiKey string
	http   *http.Client
	cache  sync.Map
}

// TMDBResult is a search or detail result from TMDB.
type TMDBResult struct {
	TMDBID     int      `json:"id"`
	Title      string   `json:"title"`  // movie
	Name       string   `json:"name"`   // tv
	MediaType  string   `json:"media_type"`
	Year       string   `json:"release_date"`
	FirstAir   string   `json:"first_air_date"`
	Overview   string   `json:"overview"`
	PosterPath string   `json:"poster_path"`
	AltTitles  []string `json:"-"` // populated from alternative_titles
	Seasons    []TMDBSeason `json:"seasons,omitempty"`
	Genres     []TMDBGenre  `json:"genres,omitempty"`
}

// PosterURL returns the full poster image URL (w200 size).
func (r *TMDBResult) PosterURL() string {
	if r.PosterPath == "" {
		return ""
	}
	return "https://image.tmdb.org/t/p/w200" + r.PosterPath
}

// DisplayName returns the best display name.
func (r *TMDBResult) DisplayName() string {
	if r.Title != "" {
		return r.Title
	}
	return r.Name
}

// YearStr extracts the year from the date string.
func (r *TMDBResult) YearStr() string {
	date := r.FirstAir
	if date == "" {
		date = r.Year
	}
	if len(date) >= 4 {
		return date[:4]
	}
	return ""
}

// EpisodeCount returns total episode count across all seasons.
func (r *TMDBResult) EpisodeCount() int {
	total := 0
	for _, s := range r.Seasons {
		if s.SeasonNumber > 0 {
			total += s.EpisodeCount
		}
	}
	return total
}

type TMDBSeason struct {
	SeasonNumber int `json:"season_number"`
	EpisodeCount int `json:"episode_count"`
}

type TMDBGenre struct {
	Name string `json:"name"`
}

type tmdbSearchResp struct {
	Results []TMDBResult `json:"results"`
}

// NewTMDBClient creates a new TMDB API client.
func NewTMDBClient(apiKey string) *TMDBClient {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}
	if tmdbProxyURL != "" {
		if u, err := url.Parse(tmdbProxyURL); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &TMDBClient{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 10 * time.Second, Transport: transport},
	}
}

var tmdbProxyURL string

// SetTMDBProxy sets the HTTP proxy for TMDB API calls.
func SetTMDBProxy(proxyURL string) {
	tmdbProxyURL = proxyURL
	// Re-init default client if already created
	if DefaultTMDB != nil {
		DefaultTMDB = NewTMDBClient(DefaultTMDB.apiKey)
	}
}

// Search searches TMDB for a title.
func (c *TMDBClient) Search(query string, mediaType string) (*TMDBResult, error) {
	if c == nil || c.apiKey == "" {
		return nil, fmt.Errorf("TMDB API key not configured")
	}

	cacheKey := mediaType + ":" + query
	if cached, ok := c.cache.Load(cacheKey); ok {
		return cached.(*TMDBResult), nil
	}

	var results []TMDBResult
	var err error

	switch mediaType {
	case "tv", "anime":
		results, err = c.searchTV(query)
	default:
		results, err = c.searchMulti(query)
	}
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("no results for %q", query)
	}

	best := &results[0]
	// Get details for best match
	c.fillDetails(best)

	c.cache.Store(cacheKey, best)
	return best, nil
}

func (c *TMDBClient) searchTV(query string) ([]TMDBResult, error) {
	params := url.Values{}
	params.Set("api_key", c.apiKey)
	params.Set("query", query)
	params.Set("language", "zh-CN")

	reqURL := "https://api.themoviedb.org/3/search/tv?" + params.Encode()
	resp, err := c.http.Get(reqURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var r tmdbSearchResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	for i := range r.Results {
		r.Results[i].MediaType = "tv"
	}
	return r.Results, nil
}

func (c *TMDBClient) searchMulti(query string) ([]TMDBResult, error) {
	params := url.Values{}
	params.Set("api_key", c.apiKey)
	params.Set("query", query)
	params.Set("language", "zh-CN")

	reqURL := "https://api.themoviedb.org/3/search/multi?" + params.Encode()
	resp, err := c.http.Get(reqURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var r tmdbSearchResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return r.Results, nil
}

func (c *TMDBClient) fillDetails(result *TMDBResult) {
	if result == nil {
		return
	}
	params := url.Values{}
	params.Set("api_key", c.apiKey)
	params.Set("language", "zh-CN")

	var detailURL string
	if result.MediaType == "tv" {
		detailURL = fmt.Sprintf("https://api.themoviedb.org/3/tv/%d?%s", result.TMDBID, params.Encode())
	} else {
		detailURL = fmt.Sprintf("https://api.themoviedb.org/3/movie/%d?%s", result.TMDBID, params.Encode())
	}

	resp, err := c.http.Get(detailURL)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	var detail TMDBResult
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		return
	}
	result.Seasons = detail.Seasons
	result.Genres = detail.Genres
	result.FirstAir = detail.FirstAir
	result.Overview = detail.Overview
}

// BestMatch searches TMDB and returns the best matching title and its TMDB ID.
// Used to enrich subscription matching.
// DefaultTMDB is the global TMDB client instance. Call InitTMDB to configure.
var DefaultTMDB *TMDBClient

// InitTMDB configures the global TMDB client.
func InitTMDB(apiKey string) {
	if apiKey != "" {
		DefaultTMDB = NewTMDBClient(apiKey)
	}
}

// SearchAll searches both movies and TV shows from TMDB, returning top results.
func (c *TMDBClient) SearchAll(query string) (movies, tvShows []TMDBResult, err error) {
	if c == nil || c.apiKey == "" {
		return nil, nil, fmt.Errorf("TMDB API key not configured")
	}
	// Search movies
	params := url.Values{}
	params.Set("api_key", c.apiKey)
	params.Set("query", query)
	params.Set("language", "zh-CN")

	movieURL := "https://api.themoviedb.org/3/search/movie?" + params.Encode()
	resp, err := c.http.Get(movieURL)
	if err != nil {
		return nil, nil, err
	}
	var mr tmdbSearchResp
	json.NewDecoder(resp.Body).Decode(&mr)
	resp.Body.Close()
	for i := range mr.Results {
		mr.Results[i].MediaType = "movie"
	}
	movies = mr.Results
	if len(movies) > 8 {
		movies = movies[:8]
	}

	// Search TV
	tvURL := "https://api.themoviedb.org/3/search/tv?" + params.Encode()
	resp, err = c.http.Get(tvURL)
	if err != nil {
		return movies, nil, nil // movies still ok
	}
	var tr tmdbSearchResp
	json.NewDecoder(resp.Body).Decode(&tr)
	resp.Body.Close()
	for i := range tr.Results {
		tr.Results[i].MediaType = "tv"
	}
	tvShows = tr.Results
	if len(tvShows) > 8 {
		tvShows = tvShows[:8]
	}
	return movies, tvShows, nil
}

// Trending returns trending movies and TV shows for the week.
func (c *TMDBClient) Trending() (movies, tvShows []TMDBResult, err error) {
	return c.TrendingPage(1)
}

// TrendingPage returns trending movies and TV shows with pagination.
func (c *TMDBClient) TrendingPage(page int) (movies, tvShows []TMDBResult, err error) {
	if c == nil || c.apiKey == "" {
		return nil, nil, fmt.Errorf("TMDB API key not configured")
	}
	params := url.Values{}
	params.Set("api_key", c.apiKey)
	params.Set("language", "zh-CN")
	params.Set("page", fmt.Sprintf("%d", page))

	// Trending movies
	movieURL := "https://api.themoviedb.org/3/trending/movie/week?" + params.Encode()
	resp, err := c.http.Get(movieURL)
	if err != nil {
		return nil, nil, err
	}
	var mr tmdbSearchResp
	json.NewDecoder(resp.Body).Decode(&mr)
	resp.Body.Close()
	for i := range mr.Results {
		mr.Results[i].MediaType = "movie"
	}
	movies = mr.Results
	if len(movies) > 8 {
		movies = movies[:8]
	}

	// Trending TV
	tvURL := "https://api.themoviedb.org/3/trending/tv/week?" + params.Encode()
	resp, err = c.http.Get(tvURL)
	if err != nil {
		return movies, nil, nil
	}
	var tr tmdbSearchResp
	json.NewDecoder(resp.Body).Decode(&tr)
	resp.Body.Close()
	for i := range tr.Results {
		tr.Results[i].MediaType = "tv"
	}
	tvShows = tr.Results
	if len(tvShows) > 8 {
		tvShows = tvShows[:8]
	}
	return movies, tvShows, nil
}

func (c *TMDBClient) BestMatch(rawTitle string, mediaType string) *TMDBResult {
	if c == nil || c.apiKey == "" {
		return nil
	}
	// Clean the title for search
	clean := cleanForSearch(rawTitle)
	result, err := c.Search(clean, mediaType)
	if err != nil {
		return nil
	}
	// Verify a reasonable match
	if result != nil && titleSimilarity(rawTitle, result.DisplayName()) > 0.3 {
		return result
	}
	return nil
}

func cleanForSearch(s string) string {
	s = strings.TrimSpace(s)
	// Remove year in parentheses
	s = strings.ReplaceAll(s, "(", " ")
	s = strings.ReplaceAll(s, ")", " ")
	// Remove common tags
	for _, tag := range []string{"[", "]", "Season", "season"} {
		s = strings.ReplaceAll(s, tag, " ")
	}
	return strings.Join(strings.Fields(s), " ")
}

func titleSimilarity(a, b string) float64 {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == b {
		return 1.0
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return 0.85
	}
	ta := strings.Fields(a)
	tb := strings.Fields(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	overlap := 0
	for _, x := range ta {
		if len(x) < 2 {
			continue
		}
		for _, y := range tb {
			if x == y {
				overlap++
				break
			}
		}
	}
	return float64(overlap) / float64(max(len(ta), len(tb)))
}
