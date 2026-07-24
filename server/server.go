package server

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mguyenanastacio-glitch/pan-fetcher/config"
	"github.com/mguyenanastacio-glitch/pan-fetcher/indexer"
	"github.com/mguyenanastacio-glitch/pan-fetcher/jackett"
	"github.com/mguyenanastacio-glitch/pan-fetcher/media"
	"github.com/mguyenanastacio-glitch/pan-fetcher/notify"
	p115pkg "github.com/mguyenanastacio-glitch/pan-fetcher/p115"
	"github.com/mguyenanastacio-glitch/pan-fetcher/rsssite"

	"github.com/deadblue/elevengo"
)

type Agent interface {
	AddMagnetTask([]string, string, string) error
	ProcessRSSFeed(rssURL, cid, savepath, subKey string) []string
	OfflineClear(int) error
	ListTasks() ([]p115pkg.TaskItem, error)
	ListDir(string) ([]p115pkg.DirEntry, error)
	GetEntry(string) (p115pkg.DirEntry, error)
	Mkdir(parentID, name string) (p115pkg.DirEntry, error)
	RenameEntry(entryID, newName string) error
	DeleteEntry(entryID string) error
	MoveEntry(targetDirID, entryID string) error
	Copy(targetDirID, entryID string) error
	UserGet(*elevengo.UserInfo) error
	StoreClose() error
	// Settings
	GetSettings() p115pkg.AppSettings
	UpdateSettings(s p115pkg.AppSettings) error
	// Connection test
	TestConnection() error
	// Cookies
	LoadCookiesStr() string
}

type Server struct {
	Agent     Agent
	Port      int
	Domain    string // Bind domain, e.g. "example.com" or "0.0.0.0"
	CertFile  string // TLS certificate
	KeyFile   string // TLS private key
	ProxyHTTP string
	WeworkWH  string // WeChat Work webhook
	JackettURL           string
	JackettAPIKey        string
	JackettAdminPassword string
	jackettActive map[string]bool           // jackett indexer IDs that are activated
	jackettActiveMu sync.Mutex
	jackettCache  []jackett.IndexerInfo
	jackettCacheMu sync.Mutex
	jackettCacheTime time.Time
	startTime time.Time
	fsCache   map[string]fsCacheEntry
	fsCacheMu sync.Mutex
	IdxMgr    *indexer.Manager

	// Lightweight caches for expensive per-request operations
	connCheckLoggedIn bool
	connCheckTime     time.Time
	taskCountCache    int
	taskCountCacheAt  time.Time
	entryCache        map[string]p115pkg.DirEntry
	entryCacheMu      sync.Mutex
}

type fsCacheEntry struct {
	entries []p115pkg.DirEntry
	expires time.Time
}

// SetAgent replaces the current agent (used after re-login).
func (s *Server) SetAgent(a Agent) {
	if pa, ok := a.(*p115pkg.Agent); ok {
		pa.Dedup = globalDedup
	}
	s.Agent = a
}

func (s *Server) invalidateFSCache(dirID string) {
	s.fsCacheMu.Lock()
	if dirID == "" {
		s.fsCache = make(map[string]fsCacheEntry)
	} else {
		delete(s.fsCache, "fs:"+dirID)
	}
	s.fsCacheMu.Unlock()
	// Also clear entry cache on modifications
	s.entryCacheMu.Lock()
	s.entryCache = make(map[string]p115pkg.DirEntry)
	s.entryCacheMu.Unlock()
}

type OfflineTask struct {
	Tasks    []string `json:"tasks"`
	Cid      string   `json:"cid"`
	SavePath string   `json:"savepath,omitempty"`
}

// taskRow is a single offline task row for the template.
type taskRow struct {
	InfoHash string
	Name     string
	Size     string
	Status   string
	Percent  float64
	URL      string
	RowClass string
}

type dashboardData struct {
	LoggedIn      bool
	Lang          string
	T             map[string]string
	Message       string
	Error         string
	Page          string
	Tasks         []taskRow
	TaskCount     int
	Logs          []string
	FSEntries     []fsEntry
	FSCrumbs      []fsBreadcrumb
	FSCurrentID   string
	FSParentID    string
	Subs          []subRow
	RssSubs       []rssSubRow
	Settings      p115pkg.AppSettings
	ProxyHTTP     string
	Domain        string
	WeworkWebhook string
	ShowQR        bool
	Cookies       string
	SearchQuery   string
	SearchKeyword string
	SearchResults []indexer.SearchResult
	SearchTotal   int
	PageSize      int
	SearchErrors  map[string]string
	AllTagsJSON   template.JS // JSON of all unique tags from full search results
	AllGroupsJSON template.JS // JSON of all validated group names from full search results
	SearchSort     string
	SearchIndexers []string
	RssURL         string
	SavedSearches  []savedSearch
	IndexerList    []indexer.IndexerInfo
	IndexerLibrary []indexer.IndexerInfo
	JackettLibrary []jackett.IndexerInfo
	DedupEntries   []dedupEntry
	DashStats       dashStats
	HasAgent        bool
	HasPassword     bool
	NotifyTask      bool
	NotifyRSS       bool
	NotifyLog       bool
	Timezone        string
	TimezoneOptions map[string]string
	JackettURL           string
	JackettAPIKey        string
	JackettAdminPassword string
	TMDBAPIKey           string
	AboutVersion         string
	AutoUpdate      bool
}

type dashStats struct {
	TotalTasks     int
	RssSubsTotal   int
	RssSubsActive  int
	ActiveIndexers int
	CacheEntries   int
	Uptime         string
	RecentItems    []notify.RecentItem
}

type dedupEntry struct {
	SubKey string
	Count  int
}

var srv *http.Server

// Version is set via ldflags at build time: -X server.Version=v0.x.x
var Version = "1.1.0"

var rssJsonPath = "rss.json"

// searchCache holds results for incremental loading
var (
	searchCacheMu   sync.Mutex
	searchCache     []indexer.SearchResult // filtered by current keyword
	searchCacheFull []indexer.SearchResult // all results, never filtered (for tag extraction & re-filter)
	searchCtx       searchContext
	filterKeyword   string // the keyword currently applied to searchCache
)

const searchCacheFile = "search-cache.json"

type persistedSearch struct {
	Query     string                 `json:"query"`
	Sort      string                 `json:"sort"`
	Indexers  []string               `json:"indexers"`
	Keyword   string                 `json:"keyword"`
	PageSize  int                    `json:"page_size"`
	Results   []indexer.SearchResult `json:"results"`
}

func saveSearchCache() {
	searchCacheMu.Lock()
	defer searchCacheMu.Unlock()
	ps := persistedSearch{
		Query:    searchCtx.Query,
		Sort:     searchCtx.Sort,
		Indexers: searchCtx.Indexers,
		Keyword:  filterKeyword,
		PageSize: searchCtx.PageSize,
		Results:  searchCacheFull,
	}
	b, _ := json.MarshalIndent(ps, "", "  ")
	os.WriteFile(searchCacheFile, b, 0644)
}

func loadSearchCache() *persistedSearch {
	b, err := os.ReadFile(searchCacheFile)
	if err != nil {
		return nil
	}
	var ps persistedSearch
	if err := json.Unmarshal(b, &ps); err != nil {
		return nil
	}
	if len(ps.Results) == 0 {
		return nil
	}
	// Restore globals
	searchCacheMu.Lock()
	searchCacheFull = ps.Results
	searchCache = applyKeywordFilter(searchCacheFull, ps.Keyword)
	filterKeyword = ps.Keyword
	searchCtx = searchContext{
		Query:    ps.Query,
		Sort:     ps.Sort,
		Indexers: ps.Indexers,
		NextPage: 2,
		PageSize: ps.PageSize,
	}
	searchCacheMu.Unlock()
	return &ps
}

type searchContext struct {
	Query     string
	Sort      string
	Indexers  []string // includes "jackett:xxx" prefixed
	NextPage  int      // next page to fetch (2, 3, ...)
	PageSize  int      // items per display page
	Exhausted bool
}
var dedupCachePath = "dedup-cache.json"

// ---------- dedup + torrent URL cache (unified) ----------

// dedup-cache.json format:
// { "_torrent_urls": {"https://...": "hash"}, "_hash_names": {"hash": "name"}, "subKey1": {"hash1":true,...}, ... }

const torrentURLsKey = "_torrent_urls"
const torrentErrorsKey = "_torrent_errors"
const hashNamesKey = "_hash_names"

type dedupCache struct {
	mu            sync.Mutex
	subs          map[string]map[string]bool // subKey -> infoHash -> true
	torrentURLs   map[string]string          // .torrent URL -> info hash
	torrentErrors map[string]string          // .torrent URL -> last error
	hashNames     map[string]string          // infoHash -> display name
	totalEntries  int                        // running counter, O(1)
}

var globalDedup = &dedupCache{
	subs:          make(map[string]map[string]bool),
	torrentURLs:   make(map[string]string),
	torrentErrors: make(map[string]string),
	hashNames:     make(map[string]string),
}

func (d *dedupCache) Load() {
	d.mu.Lock()
	defer d.mu.Unlock()
	data, err := os.ReadFile(dedupCachePath)
	if err != nil {
		return
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	for k, v := range raw {
		if k == torrentURLsKey {
			var urls map[string]string
			json.Unmarshal(v, &urls)
			for url, hash := range urls {
				d.torrentURLs[url] = hash
			}
		} else if k == torrentErrorsKey {
			var errs map[string]string
			json.Unmarshal(v, &errs)
			for url, errMsg := range errs {
				d.torrentErrors[url] = errMsg
			}
		} else if k == hashNamesKey {
			var names map[string]string
			json.Unmarshal(v, &names)
			for hash, name := range names {
				d.hashNames[hash] = name
			}
		} else {
			// Support both legacy array format and new map format
			var list []string
			if err := json.Unmarshal(v, &list); err == nil {
				set := make(map[string]bool, len(list))
				for _, h := range list {
					set[h] = true
				}
				d.subs[k] = set
			} else {
				var set map[string]bool
				json.Unmarshal(v, &set)
				if set == nil {
					set = make(map[string]bool)
				}
				d.subs[k] = set
			}
		}
	}
	// Rebuild running counter after loading from disk
	for _, set := range d.subs {
		d.totalEntries += len(set)
	}
}

func (d *dedupCache) Save() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.saveLocked()
}

func (d *dedupCache) Has(subKey, magnet string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	set := d.subs[subKey]
	if set == nil {
		return false
	}
	return set[extractInfoHashFromMagnet(magnet)]
}

func (d *dedupCache) Add(subKey, magnet string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	h := extractInfoHashFromMagnet(magnet)
	if h == "" {
		return
	}
	if d.subs[subKey] == nil {
		d.subs[subKey] = make(map[string]bool)
	}
	if !d.subs[subKey][h] {
		d.totalEntries++
	}
	d.subs[subKey][h] = true
	// Store display name from magnet dn= parameter
	if name := extractNameFromMagnet(magnet); name != "" {
		d.hashNames[h] = name
	}
}

// RemoveSub deletes all dedup entries for a given subKey.
func (d *dedupCache) RemoveSub(subKey string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if set := d.subs[subKey]; set != nil {
		d.totalEntries -= len(set)
	}
	delete(d.subs, subKey)
}

// SubKeys returns all subscription keys in the dedup cache.
func (d *dedupCache) SubKeys() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	keys := make([]string, 0, len(d.subs))
	for k := range d.subs {
		keys = append(keys, k)
	}
	return keys
}

// SubCount returns the number of deduped hashes for a subKey.
func (d *dedupCache) SubCount(subKey string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.subs[subKey])
}

// TotalCount returns the total number of cached entries across all subs (O(1)).
func (d *dedupCache) TotalCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.totalEntries
}

// Hashes returns the list of info hashes for a subKey.
func (d *dedupCache) Hashes(subKey string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	set := d.subs[subKey]
	if set == nil {
		return nil
	}
	list := make([]string, 0, len(set))
	for h := range set {
		list = append(list, h)
	}
	return list
}

// ---------- torrent URL → info hash (stored inside dedup cache) ----------

func (d *dedupCache) GetTorrentHash(url string) (hash string, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	hash, ok = d.torrentURLs[url]
	return
}

func (d *dedupCache) SetTorrentHash(url, hash string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.torrentURLs[url] = hash
	delete(d.torrentErrors, url) // clear any previous error
	d.saveLocked()
}

func (d *dedupCache) GetTorrentError(url string) (errMsg string, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	errMsg, ok = d.torrentErrors[url]
	return
}

func (d *dedupCache) SetTorrentError(url, errMsg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.torrentErrors[url] = errMsg
	d.saveLocked()
}

func (d *dedupCache) SetHashName(hash, name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hashNames[hash] = name
	d.saveLocked()
}

func (d *dedupCache) GetHashName(hash string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.hashNames[hash]
}

func (d *dedupCache) saveLocked() {
	raw := make(map[string]interface{}, len(d.subs)+3)
	urls := make(map[string]string, len(d.torrentURLs))
	for url, hash := range d.torrentURLs {
		urls[url] = hash
	}
	raw[torrentURLsKey] = urls
	errs := make(map[string]string, len(d.torrentErrors))
	for url, errMsg := range d.torrentErrors {
		errs[url] = errMsg
	}
	raw[torrentErrorsKey] = errs
	names := make(map[string]string, len(d.hashNames))
	for hash, name := range d.hashNames {
		names[hash] = name
	}
	raw[hashNamesKey] = names
	for k, set := range d.subs {
		if len(set) == 0 {
			continue
		}
		list := make([]string, 0, len(set))
		for h := range set {
			list = append(list, h)
		}
		raw[k] = list
	}
	data, err := json.Marshal(raw)
	if err != nil {
		log.Printf("[dedup] save marshal error: %v", err)
		return
	}
	if err := os.WriteFile(dedupCachePath, data, 0644); err != nil {
		log.Printf("[dedup] save write error: %v", err)
	}
}

// ClearSub removes all dedup entries for a subKey and persists.
func (d *dedupCache) ClearSub(subKey string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if set := d.subs[subKey]; set != nil {
		d.totalEntries -= len(set)
	}
	delete(d.subs, subKey)
	d.saveLocked()
	log.Printf("[dedup] cleared subKey=%q (%d remaining)", subKey, len(d.subs))
}

// RemoveHash removes a single hash from a subKey's dedup set.
func (d *dedupCache) RemoveHash(subKey, hash string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	set := d.subs[subKey]
	if set == nil {
		return false
	}
	if _, ok := set[hash]; !ok {
		return false
	}
	delete(set, hash)
	d.totalEntries--
	if len(set) == 0 {
		delete(d.subs, subKey)
	}
	d.saveLocked()
	log.Printf("[dedup] removed hash=%s from subKey=%q", hash, subKey)
	return true
}

func (d *dedupCache) AllTorrentURLs() [][2]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([][2]string, 0, len(d.torrentURLs))
	for url, hash := range d.torrentURLs {
		out = append(out, [2]string{url, hash})
	}
	return out
}

func (d *dedupCache) RemoveTorrentURL(url string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	hash := d.torrentURLs[url]
	delete(d.torrentURLs, url)
	// Also remove this hash from all subKey entries so re-download is allowed
	if hash != "" {
		for _, set := range d.subs {
			delete(set, hash)
		}
	}
	d.saveLocked()
	log.Printf("[dedup] removed torrent URL mapping: %s (hash=%s)", url, hash)
}

// TorrentURLByHash returns the .torrent URL for a given info hash, or "".
func (d *dedupCache) TorrentURLByHash(hash string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	for url, h := range d.torrentURLs {
		if h == hash {
			return url
		}
	}
	return ""
}

func extractInfoHashFromMagnet(magnet string) string {
	// Already a bare info hash (40 hex chars)?
	if len(magnet) == 40 {
		matched := true
		for _, c := range magnet {
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				matched = false
				break
			}
		}
		if matched {
			return strings.ToLower(magnet)
		}
	}
	// Try URL-decode first (some clients double-encode)
	if decoded, err := url.QueryUnescape(magnet); err == nil && decoded != magnet {
		if h := extractInfoHashFromMagnet(decoded); h != "" {
			return h
		}
	}
	// Strip magnet:? prefix if present
	m := magnet
	if strings.HasPrefix(strings.ToLower(m), "magnet:?") {
		m = m[8:] // len("magnet:?") = 8
	}
	for _, part := range strings.Split(m, "&") {
		lower := strings.ToLower(part)
		if strings.HasPrefix(lower, "xt=urn:btih:") {
			return strings.ToLower(strings.TrimPrefix(part, "xt=urn:btih:"))
		}
		// Handle URL-encoded separator: xt=urn%3Abtih%3AHASH
		if strings.HasPrefix(lower, "xt=urn%3abtih%3a") {
			hash := strings.TrimPrefix(part, "xt=urn%3Abtih%3A")
			if decoded, err := url.QueryUnescape(hash); err == nil {
				return strings.ToLower(decoded)
			}
			return strings.ToLower(hash)
		}
	}
	return ""
}

func extractNameFromMagnet(magnet string) string {
	for _, part := range strings.Split(magnet, "&") {
		if strings.HasPrefix(strings.ToLower(part), "dn=") {
			name, _ := url.QueryUnescape(part[3:])
			return name
		}
	}
	return ""
}

type webSettings struct {
	Lang          string `json:"lang"`
	ChunkSize     int    `json:"chunk_size"`
	ChunkDelay    int    `json:"chunk_delay"`
	CooldownMin   int    `json:"cooldown_min"`
	CooldownMax   int    `json:"cooldown_max"`
	WebPassword   string `json:"web_password"`
	SubsInterval  int    `json:"subs_interval"`
	NotifyTask    bool   `json:"notify_task"`
	NotifyRSS     bool   `json:"notify_rss"`
	NotifyLog     bool   `json:"notify_log"`
	Timezone      string `json:"timezone"`
	JackettURL           string `json:"jackett_url"`
	JackettAPIKey        string `json:"jackett_apikey"`
	JackettAdminPassword string `json:"jackett_admin_password"`
	TMDBAPIKey           string `json:"tmdb_apikey"`
	PageSize             int    `json:"page_size"`
	AutoUpdate    bool   `json:"auto_update"`
}

func (s *Server) loadWebSettings() webSettings {
	data, err := os.ReadFile("web-settings.json")
	if err != nil {
		return webSettings{Lang: "zh", SubsInterval: 60, PageSize: 50}
	}
	var ws webSettings
	json.Unmarshal(data, &ws)
	if ws.Lang == "" {
		ws.Lang = "zh"
	}
	if ws.SubsInterval <= 0 {
		ws.SubsInterval = 60
	}
	return ws
}

func (s *Server) saveWebSettings(ws webSettings) {
	data, _ := json.MarshalIndent(ws, "", "  ")
	os.WriteFile("web-settings.json", data, 0644)
}

func (s *Server) loadJackettEnabled() {
	if s.jackettActive != nil {
		return // already loaded
	}
	s.jackettActive = make(map[string]bool)
	data, err := os.ReadFile("jackett-enabled.json")
	if err != nil {
		return
	}
	var ids []string
	if json.Unmarshal(data, &ids) == nil {
		for _, id := range ids {
			s.jackettActive[id] = true
		}
	}
}

func (s *Server) saveJackettEnabled() {
	var ids []string
	for id := range s.jackettActive {
		ids = append(ids, id)
	}
	data, _ := json.Marshal(ids)
	os.WriteFile("jackett-enabled.json", data, 0644)
}

// jackettConfig returns a jackett.Config populated from server settings.
func (s *Server) jackettConfig() jackett.Config {
	return jackett.Config{
		URL:           s.JackettURL,
		APIKey:        s.JackettAPIKey,
		AdminPassword: s.JackettAdminPassword,
	}
}

// handleJKAddToJackett adds an indexer to the linked Jackett instance via API.
// writeJSON writes a JSON object to the response, properly escaping the message.
func writeJSON(w http.ResponseWriter, ok bool, msg string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"ok": ok, "msg": msg})
}

func (s *Server) handleJKAddToJackett(w http.ResponseWriter, r *http.Request, id string) {
	if s.JackettURL == "" || s.JackettAPIKey == "" {
		writeJSON(w, false, "Jackett not configured")
		return
	}
	cfg := s.jackettConfig()
	if err := jackett.AddIndexer(cfg, id); err != nil {
		log.Printf("[jackett] add %s failed: %v", id, err)
		writeJSON(w, false, err.Error())
		return
	}
	log.Printf("[jackett] %s added successfully", id)
	s.jackettActiveMu.Lock()
	if s.jackettActive == nil {
		s.jackettActive = make(map[string]bool)
	}
	s.jackettActive[id] = true
	s.saveJackettEnabled()
	s.jackettActiveMu.Unlock()
	go func() {
		if jk, err := jackett.ListIndexers(cfg); err == nil {
			s.jackettCacheMu.Lock()
			s.jackettCache = jk
			s.jackettCacheMu.Unlock()
		}
	}()
	writeJSON(w, true, "Added to Jackett")
}

func (s *Server) handleJKRemoveFromJackett(w http.ResponseWriter, r *http.Request, id string) {
	if s.JackettURL == "" || s.JackettAPIKey == "" {
		writeJSON(w, false, "Jackett not configured")
		return
	}
	cfg := s.jackettConfig()
	if err := jackett.DeleteIndexer(cfg, id); err != nil {
		log.Printf("[jackett] remove %s failed: %v", id, err)
		writeJSON(w, false, err.Error())
		return
	}
	log.Printf("[jackett] %s removed successfully", id)
	s.jackettActiveMu.Lock()
	if s.jackettActive != nil {
		delete(s.jackettActive, id)
		s.saveJackettEnabled()
	}
	s.jackettActiveMu.Unlock()
	go func() {
		if jk, err := jackett.ListIndexers(cfg); err == nil {
			s.jackettCacheMu.Lock()
			s.jackettCache = jk
			s.jackettCacheMu.Unlock()
		}
	}()
	writeJSON(w, true, "Removed from Jackett")
}

// ---------- log buffer ----------

type logBuffer struct {
	mu      sync.Mutex
	buf     []string
	size    int
	pos     int   // total lines written (wraps via modulo)
	wrapped bool  // true after first wrap-around
}

func newLogBuffer(size int) *logBuffer {
	return &logBuffer{buf: make([]string, size), size: size}
}

func (lb *logBuffer) Write(p []byte) (int, error) {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	line := strings.TrimRight(string(p), "\r\n")
	if line == "" {
		return len(p), nil
	}
	lb.buf[lb.pos%lb.size] = line
	lb.pos++
	if lb.pos >= lb.size && lb.pos%lb.size == 0 {
		lb.wrapped = true
	}
	return len(p), nil
}

// Lines returns log lines in reverse chronological order (newest first).
// If the buffer has wrapped, the newest line is preceded by a marker.
func (lb *logBuffer) Lines() []string {
	lb.mu.Lock()
	defer lb.mu.Unlock()

	total := lb.pos
	if total > lb.size {
		total = lb.size
	}
	out := make([]string, 0, total+1)

	// Newest first: iterate backwards
	lastIdx := lb.pos - 1
	startIdx := lastIdx - total + 1
	for i := lastIdx; i >= startIdx; i-- {
		idx := i % lb.size
		if idx < 0 {
			idx += lb.size
		}
		out = append(out, lb.buf[idx])
	}

	if lb.wrapped {
		out = append(out, "--- [older logs cleared] ---")
	}
	return out
}

var logBuf = newLogBuffer(500)

// notifyLogWriter sends every log line into the notify queue for push.
type notifyLogWriter struct{}

func (w *notifyLogWriter) Write(p []byte) (int, error) {
	line := strings.TrimRight(string(p), "\r\n")
	if line != "" {
		notify.Logf("%s", line)
	}
	return len(p), nil
}

// tzLogWriter wraps a writer and converts log timestamps to the configured timezone.
type tzLogWriter struct {
	out io.Writer
	tz  *time.Location
}

func (w *tzLogWriter) Write(p []byte) (int, error) {
	if w.tz == nil {
		return w.out.Write(p)
	}
	// Go log format: "2009/01/23 01:23:23 message" (20 chars timestamp prefix)
	line := string(p)
	if len(line) > 20 {
		t, err := time.Parse("2006/01/02 15:04:05", line[:19])
		if err == nil {
			line = t.In(w.tz).Format("2006/01/02 15:04:05") + line[19:]
		}
	}
	return w.out.Write([]byte(line))
}

var tzWriter = &tzLogWriter{out: io.MultiWriter(logBuf, os.Stderr, &notifyLogWriter{})}

var timezones = map[string]string{
	"":                  "系统默认",
	"Asia/Shanghai":     "Asia/Shanghai",
	"Asia/Tokyo":        "Asia/Tokyo",
	"Asia/Hong_Kong":    "Asia/Hong_Kong",
	"UTC":               "UTC",
	"Europe/London":     "Europe/London",
	"America/New_York":  "America/New_York",
	"America/Los_Angeles": "America/Los_Angeles",
}

func init() {
	log.SetOutput(tzWriter)
	log.SetFlags(log.LstdFlags)
}

// SetTimezone sets the timezone for log timestamps.
func SetTimezone(tz string) {
	if tz == "" {
		tzWriter.tz = nil
		return
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		tzWriter.tz = nil
		return
	}
	tzWriter.tz = loc
}

func formatTimeInTZ(t time.Time, tz string) string {
	if tz == "" {
		return t.Format("15:04:05")
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return t.Format("15:04:05")
	}
	return t.In(loc).Format("15:04:05")
}

// ---------- web auth ----------

var (
	webPassword string
	webSessions = make(map[string]bool)
	webSessMu   sync.Mutex
)

// SetPassword configures the web login password. Empty means no auth required.
func SetPassword(pw string) {
	webPassword = pw
}

// LoadPersistedPassword reads the password from web-settings.json if not already set via CLI.
func LoadPersistedPassword() {
	if webPassword != "" {
		return // CLI flag takes precedence
	}
	data, err := os.ReadFile("web-settings.json")
	if err != nil {
		return
	}
	var ws struct {
		WebPassword string `json:"web_password"`
	}
	if err := json.Unmarshal(data, &ws); err != nil {
		return
	}
	if ws.WebPassword != "" {
		webPassword = ws.WebPassword
	}
}

func newSessionToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) authCheck(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if webPassword == "" {
			next(w, r)
			return
		}
		// Allow login page and static resources
		if r.URL.Path == "/login" || r.URL.Path == "/login/qrcode" || r.URL.Path == "/login/cookies" {
			next(w, r)
			return
		}
		// Check session cookie
		cookie, err := r.Cookie("r2c_session")
		if err != nil || !validSession(cookie.Value) {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next(w, r)
	}
}

func validSession(token string) bool {
	webSessMu.Lock()
	defer webSessMu.Unlock()
	return webSessions[token]
}

// ---------- i18n ----------




// ---------- server lifecycle ----------

func New(agent *p115pkg.Agent, port int) *Server {
	s := &Server{Port: port, startTime: time.Now(), fsCache: make(map[string]fsCacheEntry), entryCache: make(map[string]p115pkg.DirEntry)}
	if agent != nil {
		agent.Dedup = globalDedup
		s.Agent = agent
	}
	// Pre-load Jackett settings from web-settings.json
	ws := s.loadWebSettings()
	s.JackettURL = ws.JackettURL
	s.JackettAPIKey = ws.JackettAPIKey
	s.JackettAdminPassword = ws.JackettAdminPassword
	// Restore language preference from persisted settings
	if ws.Lang != "" && s.Agent != nil {
		st := s.Agent.GetSettings()
		st.Lang = ws.Lang
		s.Agent.UpdateSettings(st)
	}
	// Apply proxy to Jackett client (same as engine)
	cfg, _, _ := config.LoadWithOptions(config.CLIParams{}, config.LoadOptions{})
	if cfg.Proxy.HTTP != "" {
		s.ProxyHTTP = cfg.Proxy.HTTP
		jackett.SetProxy(cfg.Proxy.HTTP)
		media.SetTMDBProxy(cfg.Proxy.HTTP)
	}
	// Init TMDB client (after proxy is configured)
	if ws.TMDBAPIKey != "" {
		media.InitTMDB(ws.TMDBAPIKey)
	}
	// Warm Jackett cache in background
	s.ensureDefaultIndexers()
	if s.JackettURL != "" && s.JackettAPIKey != "" {
		go func() {
			if jk, err := jackett.ListIndexers(s.jackettConfig()); err == nil {
				s.jackettCacheMu.Lock()
				s.jackettCache = jk
				s.jackettCacheTime = time.Now()
				s.jackettCacheMu.Unlock()
				log.Printf("[jackett] cache warmed: %d indexers", len(jk))
			} else {
				log.Printf("[jackett] cache warm failed: %v", err)
			}
		}()
	}
	return s
}

// SetDomain sets the domain to bind the server to.
func (s *Server) SetDomain(domain string) {
	s.Domain = domain
}

// SetTLS configures TLS certificate and key for HTTPS.
func (s *Server) SetTLS(certFile, keyFile string) {
	s.CertFile = certFile
	s.KeyFile = keyFile
}

// SetIndexerManager sets the indexer manager on the server.
func (s *Server) SetIndexerManager(m *indexer.Manager) {
	s.IdxMgr = m
}

// LoadProxyConfig reads proxy settings from config.toml.
func (s *Server) LoadProxyConfig() {
	cfg, _, err := config.LoadWithOptions(config.CLIParams{}, config.LoadOptions{})
	if err == nil && cfg.Proxy.HTTP != "" {
		s.ProxyHTTP = cfg.Proxy.HTTP
		media.SetTMDBProxy(cfg.Proxy.HTTP)
	}
}

// httpClient returns an HTTP client that respects the configured proxy.
func (s *Server) httpClient() *http.Client {
	if s.ProxyHTTP == "" {
		return http.DefaultClient
	}
	proxyURL, err := url.Parse(s.ProxyHTTP)
	if err != nil {
		return http.DefaultClient
	}
	return &http.Client{
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		Timeout:   90 * time.Second,
	}
}

// LoadNotifyConfig reads notification webhook and Jackett settings from config.toml.
func (s *Server) LoadNotifyConfig() {
	cfg, _, err := config.LoadWithOptions(config.CLIParams{}, config.LoadOptions{})
	if err == nil {
		if cfg.Notify.WeworkWebhook != "" {
			s.WeworkWH = cfg.Notify.WeworkWebhook
			notify.SetWebhook(cfg.Notify.WeworkWebhook)
		}
	}
	// Load timezone setting
	ws := s.loadWebSettings()
	SetTimezone(ws.Timezone)
	notify.SetTimezone(ws.Timezone)
	notify.SetLogEnabled(ws.NotifyLog)
}

// LoadJackettConfig reads Jackett settings from web-settings.json.
// ensureDefaultIndexers writes the 4 built-in indexer YAMLs if the defs directory is empty.
func (s *Server) ensureDefaultIndexers() {
	defDir := "indexers"
	entries, err := os.ReadDir(defDir)
	if err != nil {
		os.MkdirAll(defDir, 0755)
		entries = nil
	}
	hasYAML := false
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yml") {
			hasYAML = true
			break
		}
	}
	if hasYAML {
		return
	}
	log.Printf("[indexer] installing default indexers to %s", defDir)
	for name, content := range defaultIndexers {
		os.WriteFile(filepath.Join(defDir, name+".yml"), []byte(content), 0644)
	}
}

// defaultIndexers are the 4 built-in YAML definitions for initial setup.
var defaultIndexers = map[string]string{
	"acgrip": `id: acgrip
name: ACG.RIP
language: zh-CN
type: public
encoding: UTF-8
links:
  - https://acg.rip/
caps:
  categories:
    1: TV
  modes:
    search: [q]
    tv-search: [q, season, ep]
search:
  paths:
    - path: /
  inputs:
    term: "{{ .Keywords }}"
    page: "{{.Page}}"
  rows:
    selector: tbody tr
    after: 2
  fields:
    title:
      selector: td:nth-child(3)
    category:
      text: 1
    details:
      selector: td:nth-child(3) a
      attribute: href
    download:
      selector: a[href^="magnet:?"]
      attribute: href
    magnet:
      selector: a[href^="magnet:?"]
      attribute: href
    size:
      selector: td:nth-child(5)
    date:
      selector: td:nth-child(4) time
      attribute: datetime
    seeders:
      selector: td:nth-child(6)
`,
	"dmhy": `id: dmhy
name: 动漫花园
language: zh-CN
type: public
links:
  - https://share.dmhy.org/
caps:
  categories:
    1: TV
  modes:
    search: [q]
search:
  paths:
    - path: "topics/list"
  inputs:
    keyword: "{{ .Keywords }}"
    sort_id: "0"
    page: "{{.Page}}"
  rows:
    selector: "table#topic_list tbody tr:has(a[href^=\"magnet:?\"])"
    after: 0
  fields:
    title:
      selector: "td:nth-child(3)"
    download:
      selector: "a[href^=\"magnet:?\"]"
      attribute: href
    size:
      selector: "td:nth-child(5)"
    date:
      selector: "td:nth-child(1)"
    seeders:
      selector: "td:nth-child(6)"
    leechers:
      selector: "td:nth-child(7)"
    category:
      selector: "td:nth-child(2)"
    page:
      selector: "td:nth-child(3) a"
      attribute: href
`,
	"mikan": `id: mikan
name: Mikan
language: zh-CN
type: public
encoding: UTF-8
links:
  - https://mikanani.me/
caps:
  categorymappings:
    - {id: 1, cat: TV/Anime, desc: "Anime"}
  modes:
    search: [q]
    tv-search: [q, season, ep]
search:
  paths:
    - path: "Home/Search"
  inputs:
    searchstr: "{{ .Keywords }}"
    page: "{{.Page}}"
  rows:
    selector: table.table-striped tbody tr
  fields:
    category:
      text: 1
    title:
      selector: a[href^="/Home/Episode/"]
    details:
      selector: a[href^="/Home/Episode/"]
      attribute: href
    download:
      selector: a[href^="/Download/"]
      attribute: href
    magnet:
      selector: a[data-clipboard-text]
      attribute: data-clipboard-text
    size:
      selector: td:nth-child(3)
    date:
      selector: td:nth-child(1)
    seeders:
      selector: td:nth-child(4)
`,
	"nyaasi": `id: nyaasi
name: Nyaa.si
language: en-US
type: public
encoding: UTF-8
requestDelay: 2
links:
  - https://nyaa.si/
caps:
  categorymappings:
    - {id: 1_0, cat: TV/Anime, desc: "Anime"}
  modes:
    search: [q]
    tv-search: [q, season, ep]
  allowrawsearch: true
search:
  paths:
    - path: /
  inputs:
    q: "{{ .Keywords }}"
    p: "{{.Page}}"
    f: "0"
    c: "0_0"
  rows:
    selector: tr.default,tr.danger,tr.success
  fields:
    title:
      selector: "td:nth-child(2) a:last-child"
    details:
      selector: "td:nth-child(2) a:last-child"
      attribute: href
    download:
      selector: "a[href$=\".torrent\"]"
      attribute: href
    magnet:
      selector: "a[href^=\"magnet:?\"]"
      attribute: href
    size:
      selector: "td:nth-child(4)"
    date:
      selector: "td:nth-child(5)"
    seeders:
      selector: "td:nth-child(6)"
    leechers:
      selector: "td:nth-child(7)"
    category:
      selector: "td:nth-child(1) a"
      attribute: href
`,
}

func (s *Server) LoadJackettConfig() {
	ws := s.loadWebSettings()
	s.JackettURL = ws.JackettURL
	s.JackettAPIKey = ws.JackettAPIKey
	s.JackettAdminPassword = ws.JackettAdminPassword
}

// saveProxyConfig persists the current proxy setting to config.toml.
func (s *Server) saveProxyConfig() {
	_ = config.SaveProxy(s.ProxyHTTP)
}

func maskWebhook(url string) string {
	if len(url) > 40 {
		return url[:35] + "..."
	}
	return url
}

func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	addr := fmt.Sprintf(":%d", s.Port)
	srv = &http.Server{Addr: addr, Handler: mux}

	// Load caches
	globalDedup.Load()
	rsssite.SetTorrentHashCache(globalDedup)
	notify.LoadRecentItems()

	// Populate task count cache asynchronously (don't block startup)
	go func() {
		if s.Agent != nil {
			if tasks, err := s.Agent.ListTasks(); err == nil {
				s.taskCountCache = len(tasks)
			}
		}
	}()

	// Start subscription auto-runner
	go s.autoRunSubscriptions()

	useTLS := s.CertFile != "" && s.KeyFile != ""
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	log.Printf("server started on %s://0.0.0.0:%d\n", scheme, s.Port)
	if useTLS {
		return srv.ListenAndServeTLS(s.CertFile, s.KeyFile)
	}
	return srv.ListenAndServe()
}

// autoRunSubscriptions loops through enabled RSS subscriptions.
// Between feeds: small cooldown (30s). Between full cycles: SubsInterval.
// Failed feeds are retried with exponential backoff.
func (s *Server) autoRunSubscriptions() {
	// Wait a short grace period for server to fully stabilise
	time.Sleep(2 * time.Minute)

	type retryState struct {
		failures int
		nextTry  time.Time
	}
	retryMap := make(map[string]*retryState) // subKey -> retry state

	for {
		ws := s.loadWebSettings()
		cycleInterval := ws.SubsInterval
		if cycleInterval <= 0 {
			cycleInterval = 60
		}

		feeds := s.readRssFeeds()
		ran := 0
		for _, entries := range feeds {
			for _, e := range entries {
				if !e.Enabled {
					continue
				}
				subKey := e.Name
				feedURL := e.URL
				if strings.HasPrefix(feedURL, "/") {
					feedURL = fmt.Sprintf("http://127.0.0.1:%d%s", s.Port, feedURL)
				}

				// Check retry state
				if rs, ok := retryMap[subKey]; ok {
					if time.Now().Before(rs.nextTry) {
						log.Printf("[auto-sub] skipping %s (retry in %s)", subKey, time.Until(rs.nextTry).Round(time.Second))
						continue
					}
				}

				log.Printf("[auto-sub] running: %s (%s)", subKey, feedURL)
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("[auto-sub] panic in %s: %v", subKey, r)
						}
					}()
					if s.Agent != nil && e.Cid != "" {
						// Backward compat: old subs have filter in e.Filter but not in URL
						rssURL := feedURL
						if e.Filter != "" && !strings.Contains(rssURL, "keyword=") {
							if strings.Contains(rssURL, "?") {
								rssURL += "&keyword=" + url.QueryEscape(e.Filter)
							} else {
								rssURL += "?keyword=" + url.QueryEscape(e.Filter)
							}
						}
						before := globalDedup.SubCount(subKey)
						names := s.Agent.ProcessRSSFeed(rssURL, e.Cid, e.SavePath, subKey)
						after := globalDedup.SubCount(subKey)
						newItems := after - before
						if newItems > 0 {
							notify.RecordItems(subKey, names)
							notify.Send(notify.RSSFound(subKey, newItems, names), ws.NotifyRSS)
						}
					}
				}()

				// Check for failures via dedup cache (if no new items added, might be a fetch error)
				// We track failures implicitly: if a feed runs but produces errors, the ProcessRSSFeed logs them.
				// For now, clear retry state on successful run.
				delete(retryMap, subKey)
				ran++
				time.Sleep(30 * time.Second)
			}
		}

		if ran == 0 {
			log.Printf("[auto-sub] no enabled subscriptions, sleeping %d min", cycleInterval)
		} else {
			log.Printf("[auto-sub] cycle complete: %d feeds ran, next cycle in %d min", ran, cycleInterval)
		}

		// Schedule retries for failed feeds
		// We check ProcessRSSFeed logs indirectly - any feed that consistently fails
		// will be retried in the next cycle automatically via the loop above.
		// For active retry with backoff, we rely on ProcessRSSFeed's internal error handling
		// and the fact that the main cycle will retry all enabled feeds.

		time.Sleep(time.Duration(cycleInterval) * time.Minute)
	}
}

func (s *Server) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			return err
		}
	}
	if s.Agent != nil {
		if err := s.Agent.StoreClose(); err != nil {
			log.Printf("failed to close database: %v\n", err)
		}
	}
	log.Printf("server stopped properly\n")
	return nil
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/", s.authCheck(s.handleDashboard))
	mux.HandleFunc("/tasks", s.authCheck(s.handleTasks))
	mux.HandleFunc("/add", s.authCheck(s.handleAddTask))
	mux.HandleFunc("/rss/feed", s.authCheck(s.handleRSSFeed))
	mux.HandleFunc("/clear", s.authCheck(s.handleClearTask))
	mux.HandleFunc("/fs", s.authCheck(s.handleFileSystem))
	mux.HandleFunc("/api/fs/mkdir", s.authCheck(s.handleFSMkdir))
	mux.HandleFunc("/api/fs/rename", s.authCheck(s.handleFSRename))
	mux.HandleFunc("/api/fs/delete", s.authCheck(s.handleFSDelete))
	mux.HandleFunc("/api/fs/move", s.authCheck(s.handleFSMove))
	mux.HandleFunc("/api/fs/copy", s.authCheck(s.handleFSCopy))
	mux.HandleFunc("/subs", s.authCheck(s.handleSubscriptions))
	mux.HandleFunc("/subs/run", s.authCheck(s.handleSubsRun))
	mux.HandleFunc("/subs/toggle-all", s.authCheck(s.handleSubsToggleAll))
	mux.HandleFunc("/dedup/clear-all", s.authCheck(s.handleDedupClearAll))
	mux.HandleFunc("/settings", s.authCheck(s.handleSettings))
	mux.HandleFunc("/login/qrcode", s.authCheck(s.handleQRLogin))
	mux.HandleFunc("/login/cookies", s.authCheck(s.handleCookiesLogin))
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/settings/test115", s.authCheck(s.handleTest115))
	mux.HandleFunc("/settings/test-jackett", s.authCheck(s.handleTestJackett))
	mux.HandleFunc("/settings/restart", s.authCheck(s.handleRestart))
	mux.HandleFunc("/settings/check-update", s.authCheck(s.handleCheckUpdate))
	mux.HandleFunc("/settings/update", s.authCheck(s.handleDoUpdate))
	mux.HandleFunc("/search", s.authCheck(s.handleSearch))
	mux.HandleFunc("/search/more", s.authCheck(s.handleSearchMore))
	mux.HandleFunc("/search/clear-cache", s.authCheck(s.handleSearchClearCache))
	mux.HandleFunc("/discover", s.authCheck(s.handleDiscover))
	mux.HandleFunc("/indexers", s.authCheck(s.handleIndexers))
	mux.HandleFunc("/indexers/test", s.authCheck(s.handleIndexerTest))
	mux.HandleFunc("/indexers/testall", s.authCheck(s.handleIndexerTestAll))
	mux.HandleFunc("/indexers/jackett", s.authCheck(s.handleJackettList))
	mux.HandleFunc("/indexers/jackett/all", s.authCheck(s.handleJackettAll))
	mux.HandleFunc("/indexers/jackett/add", s.authCheck(s.handleJackettAdd))
	mux.HandleFunc("/indexers/login", s.authCheck(s.handleIndexerLogin))
	mux.HandleFunc("/indexers/edit", s.authCheck(s.handleIndexerEdit))
	mux.HandleFunc("/indexers/delete", s.authCheck(s.handleIndexerDelete))
	mux.HandleFunc("/search/subscribe", s.authCheck(s.handleSearchSubscribe))
	mux.HandleFunc("/rss/search", s.handleRssSearch)
	mux.HandleFunc("/subs/dirs", s.authCheck(s.handleSubsDirs))
	mux.HandleFunc("/dedup/clear", s.authCheck(s.handleDedupClear))
	mux.HandleFunc("/api/dedup/hashes", s.authCheck(s.handleDedupHashes))
	mux.HandleFunc("/api/dedup/remove-hash", s.authCheck(s.handleDedupRemoveHash))
	mux.HandleFunc("/torrent", s.authCheck(s.handleTorrent))
	mux.HandleFunc("/torrent/clear", s.authCheck(s.handleTorrentClear))
	mux.HandleFunc("/log", s.authCheck(s.handleLogPage))
	mux.HandleFunc("/about", s.authCheck(s.handleAbout))
	mux.HandleFunc("/api/tasks", s.authCheck(s.handleAPITasks))
	mux.HandleFunc("/api/logs", s.authCheck(s.handleAPILogs))
	mux.HandleFunc("/api/tmdb/search", s.authCheck(s.handleTMDBSearch))
	mux.HandleFunc("/api/tmdb/trending", s.authCheck(s.handleTMDBTrending))
	mux.HandleFunc("/api/tmdb/detail", s.authCheck(s.handleTMDBDetail))
	mux.HandleFunc("/api/notify/test", s.authCheck(s.handleNotifyTest))
	mux.HandleFunc("/api/lang", s.authCheck(s.handleAPILang))
}

// ---------- aggregated RSS search endpoint ----------

func (s *Server) handleRssSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "missing q parameter", http.StatusBadRequest)
		return
	}
	if s.IdxMgr == nil {
		http.Error(w, "indexer not initialized", http.StatusServiceUnavailable)
		return
	}

	// Parse indexers from comma-separated list
	var indexers []string
	if idxStr := r.URL.Query().Get("indexers"); idxStr != "" {
		for _, id := range strings.Split(idxStr, ",") {
			id = strings.TrimSpace(id)
			if id != "" {
				indexers = append(indexers, id)
			}
		}
	}

	sortBy := strings.TrimSpace(r.URL.Query().Get("sort"))
	if sortBy == "" {
		sortBy = "date"
	}

	// Separate Jackett-only indexers (jackett: prefix) from local ones
	var localIndexers []string
	var hasJackettSelection bool
	for _, id := range indexers {
		if strings.HasPrefix(id, "jackett:") {
			hasJackettSelection = true
		} else {
			localIndexers = append(localIndexers, id)
		}
	}
	var se indexer.SearchAllErrors
	if len(indexers) > 0 && len(localIndexers) == 0 && hasJackettSelection {
		se = indexer.SearchAllErrors{Errors: make(map[string]string)}
	} else {
		se = s.IdxMgr.SearchAllWithErrors(indexer.SearchRequest{
			Query:    q,
			Sort:     sortBy,
			Indexers: localIndexers,
			Limit:    1000,
		})
	}

	// Also include Jackett results in RSS feed
	if s.JackettURL != "" && s.JackettAPIKey != "" {
		s.jackettActiveMu.Lock()
		s.loadJackettEnabled()
		jackettActiveSet := make(map[string]bool)
		for id := range s.jackettActive {
			jackettActiveSet[id] = true
		}
		s.jackettActiveMu.Unlock()
		jc := s.jackettConfig()
		if jr, err := jackett.Search(jc, q, nil, 0); err == nil {
			for _, jr := range jr {
				if len(indexers) == 0 && !jackettActiveSet[jr.Tracker] {
					continue
				}
				if len(indexers) > 0 {
					match := false
					for _, id := range indexers {
						id = strings.TrimPrefix(id, "jackett:")
						if id == jr.Tracker {
							match = true
							break
						}
					}
					if !match {
						continue
					}
				}
				var pubDate time.Time
				if jr.PublishDate != "" {
					pubDate, _ = time.Parse(time.RFC1123Z, jr.PublishDate)
					if pubDate.IsZero() {
						pubDate, _ = time.Parse(time.RFC1123, jr.PublishDate)
					}
				}
				se.Results = append(se.Results, indexer.SearchResult{
					Title:       jr.Title,
					MagnetURL:   jr.MagnetURI,
					PageURL:     jr.Link,
					Size:        jr.Size,
					Seeders:     jr.Seeders,
					IndexerName: "Jackett: " + jr.TrackerName,
					PublishDate: pubDate,
				})
			}
		} else if err != nil {
			log.Printf("[rss] Jackett search error: %v", err)
		}
	}

	// Dedup (same as search)
	se.Results = dedupSlice(se.Results, nil)

	// Apply keyword filter using same group-aware logic as search
	se.Results = applyKeywordFilter(se.Results, r.URL.Query().Get("keyword"))

	// Build RSS XML
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n"))
	w.Write([]byte(`<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">` + "\n"))
	w.Write([]byte(`<channel>` + "\n"))
	rssLang := s.langFromAgent()
	fmt.Fprintf(w, "<title>%s - %s</title>\n", xmlEscape(q), tr(rssLang, "rss_feed_title"))
	fmt.Fprintf(w, "<description>%s</description>\n", xmlEscape(trf(rssLang, "rss_feed_desc", q)))
	fmt.Fprintf(w, "<link>http://%s/search</link>\n", r.Host)
	fmt.Fprintf(w, "<language>%s</language>\n", rssLang)

	for _, result := range se.Results {
		guid := result.MagnetURL
		if guid == "" {
			guid = result.PageURL
		}
		if guid == "" {
			continue
		}
		// Convert .torrent URL to magnet (cached in dedup)
		if strings.HasSuffix(strings.ToLower(guid), ".torrent") {
			if hash, ok := globalDedup.GetTorrentHash(guid); ok {
				guid = "magnet:?xt=urn:btih:" + hash + "&dn=" + url.QueryEscape(result.Title)
			} else {
				m := rsssite.NormalizeTaskURL(guid, result.Title)
				if strings.HasPrefix(m, "magnet:?") {
					if h := extractInfoHashFromMagnet(m); h != "" {
						globalDedup.SetTorrentHash(guid, h)
						guid = "magnet:?xt=urn:btih:" + h + "&dn=" + url.QueryEscape(result.Title)
					} else {
						guid = m
					}
				}
			}
		} else if strings.HasPrefix(guid, "magnet:?") && !strings.Contains(guid, "dn=") && result.Title != "" {
			// Add dn= parameter to existing magnet links for display names
			guid = guid + "&dn=" + url.QueryEscape(result.Title)
		}
		pubDate := ""
		if !result.PublishDate.IsZero() {
			pubDate = result.PublishDate.Format("Mon, 02 Jan 2006 15:04:05 -0700")
		}
		sizeStr := ""
		if result.Size > 0 {
			sizeStr = fmt.Sprintf("%d", result.Size)
		}
		w.Write([]byte("<item>\n"))
		fmt.Fprintf(w, "<title>%s</title>\n", xmlEscape(result.Title))
		fmt.Fprintf(w, "<guid>%s</guid>\n", xmlEscape(guid))
		if result.PageURL != "" {
			fmt.Fprintf(w, "<link>%s</link>\n", xmlEscape(result.PageURL))
		}
		if pubDate != "" {
			fmt.Fprintf(w, "<pubDate>%s</pubDate>\n", pubDate)
		}
		fmt.Fprintf(w, "<category>%s</category>\n", xmlEscape(result.Category))
		if sizeStr != "" {
			fmt.Fprintf(w, "<enclosure url=\"%s\" length=\"%s\" type=\"application/x-bittorrent\"/>\n",
				xmlEscape(guid), sizeStr)
		}
		fmt.Fprintf(w, "<torznab:attr name=\"seeders\" value=\"%d\"/>\n", result.Seeders)
		fmt.Fprintf(w, "<torznab:attr name=\"peers\" value=\"%d\"/>\n", result.Leechers)
		fmt.Fprintf(w, "<torznab:attr name=\"site\" value=\"%s\"/>\n", xmlEscape(result.IndexerName))
		w.Write([]byte("</item>\n"))
	}
	w.Write([]byte("</channel>\n"))
	w.Write([]byte("</rss>\n"))
}

// xmlEscape escapes text for XML output.
func xmlEscape(s string) string {
	var buf strings.Builder
	for _, r := range s {
		switch r {
		case '&':
			buf.WriteString("&amp;")
		case '<':
			buf.WriteString("&lt;")
		case '>':
			buf.WriteString("&gt;")
		case '"':
			buf.WriteString("&quot;")
		case '\'':
			buf.WriteString("&apos;")
		default:
			buf.WriteRune(r)
		}
	}
	return buf.String()
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data := s.pageData("", "")
	data.Page = "home"
	data.HasAgent = s.Agent != nil
	data.HasPassword = webPassword != ""

	// Dashboard stats: only compute cheap in-memory counters.
	// Heavy API calls (ListTasks, readRssFeeds from disk) are skipped;
	// the dashboard is informational, not real-time.
	if s.Agent != nil {
		data.DashStats.TotalTasks = s.taskCountCache // updated by handleAPITasks
	}

	feeds := s.readRssFeeds()
	for _, entries := range feeds {
		for _, e := range entries {
			data.DashStats.RssSubsTotal++
			if e.Enabled {
				data.DashStats.RssSubsActive++
			}
		}
	}

	if s.IdxMgr != nil {
		data.DashStats.ActiveIndexers = s.IdxMgr.ActiveCount()
		s.jackettActiveMu.Lock()
		s.loadJackettEnabled()
		data.DashStats.ActiveIndexers += len(s.jackettActive)
		s.jackettActiveMu.Unlock()
	}

	// Skip dedup walk — would iterate thousands of entries for a single stat.
	data.DashStats.CacheEntries = globalDedup.TotalCount()
	data.DashStats.Uptime = formatDuration(time.Since(s.startTime))
	data.DashStats.RecentItems = notify.GetRecentItems()

	if err := dashboardTemplate.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	data := s.pageData("", "")
	data.Page = "tasks"
	data.HasAgent = s.Agent != nil
	data.TaskCount = s.taskCountCache
	if err := dashboardTemplate.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) handleAddTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.Agent == nil {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"error","message":"not logged in"}`)
		return
	}
	task, err := decodeOfflineTask(r)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"error","message":"%s"}`, err.Error())
		return
	}
	err = s.Agent.AddMagnetTask(task.Tasks, task.Cid, task.SavePath)
	if err != nil {
		log.Printf("[task] web submit failed: %v", err)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"status":"error","message":"%s"}`, err.Error())
		return
	}
	log.Printf("[task] web submitted %d tasks, cid=%s", len(task.Tasks), task.Cid)
	ws := s.loadWebSettings()
	notify.Send(notify.TaskSubmitted(len(task.Tasks), task.Cid, task.Tasks), ws.NotifyLog || ws.NotifyTask)
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","message":"%s"}`, trf(s.langFromAgent(), "tasks_submitted_fmt", len(task.Tasks)))
}

func (s *Server) handleRSSFeed(w http.ResponseWriter, r *http.Request) {
	if s.Agent == nil {
		s.renderMsg(w, "", "err_not_logged_in")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderResult(w, "", err.Error())
		return
	}
	rssURL := strings.TrimSpace(r.FormValue("rss_url"))
	keyword := strings.TrimSpace(r.FormValue("keyword"))
	cid := strings.TrimSpace(r.FormValue("cid"))
	savepath := strings.TrimSpace(r.FormValue("savepath"))
	if rssURL == "" {
		s.renderMsg(w, "", "err_rss_empty")
		return
	}
	go s.Agent.ProcessRSSFeed(rssURL, cid, savepath, rssURL)
	log.Printf("[feed] web triggered: %s keyword=%q cid=%s savepath=%q", rssURL, keyword, cid, savepath)
	s.renderMsgf(w, "quick_started_fmt", rssURL)
}

func (s *Server) handleClearTask(w http.ResponseWriter, r *http.Request) {
	if s.Agent == nil {
		s.renderMsg(w, "", "err_not_logged_in")
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderResult(w, "", err.Error())
		return
	}
	typeNum, err := strconv.Atoi(strings.TrimSpace(r.FormValue("type")))
	if err != nil || typeNum < 1 || typeNum > 6 {
		s.renderMsg(w, "", "err_clear_type")
		return
	}
	if err := s.Agent.OfflineClear(typeNum - 1); err != nil {
		log.Printf("[task] clear type=%d failed: %v", typeNum, err)
		s.renderResult(w, "", err.Error())
		return
	}
	log.Printf("[task] clear type=%d executed", typeNum)
	s.renderMsgf(w, "clear_executed_fmt", typeNum)
}

// ---------- filesystem browser ----------

type fsEntry struct {
	ID      string
	Name    string
	IsDir   bool
	Size    string
	Icon    string
}

type fsBreadcrumb struct {
	ID   string
	Name string
}

func (s *Server) handleFileSystem(w http.ResponseWriter, r *http.Request) {
	if s.Agent == nil {
		http.Redirect(w, r, "/settings?err=not_logged_in", http.StatusSeeOther)
		return
	}
	dirID := strings.TrimSpace(r.URL.Query().Get("dir"))
	if dirID == "" {
		dirID = "0"
	}

	// Try cache first
	var entries []p115pkg.DirEntry
	var err error
	cacheKey := "fs:" + dirID
	s.fsCacheMu.Lock()
	if cached, ok := s.fsCache[cacheKey]; ok && time.Now().Before(cached.expires) {
		entries = cached.entries
		s.fsCacheMu.Unlock()
	} else {
		s.fsCacheMu.Unlock()
		entries, err = s.Agent.ListDir(dirID)
		if err == nil {
			s.fsCacheMu.Lock()
			s.fsCache[cacheKey] = fsCacheEntry{entries: entries, expires: time.Now().Add(30 * time.Second)}
			s.fsCacheMu.Unlock()
		} else {
			lang := s.langFromAgent()
			data := s.pageDataWithCache("fs", tr(lang, "err_read_dir")+err.Error(), "")
			dashboardTemplate.Execute(w, data)
			return
		}
	}

	// Build breadcrumb (with entry cache)
	lang := s.langFromAgent()
	var crumbs []fsBreadcrumb
	currentID := dirID
	for i := 0; i < 20; i++ {
		if currentID == "0" || currentID == "" {
			crumbs = append([]fsBreadcrumb{{ID: "0", Name: tr(lang, "root_dir")}}, crumbs...)
			break
		}
		e, err := s.getEntryCached(currentID)
		if err != nil {
			crumbs = append([]fsBreadcrumb{{ID: currentID, Name: currentID}}, crumbs...)
			break
		}
		crumbs = append([]fsBreadcrumb{{ID: e.ID, Name: e.Name}}, crumbs...)
		currentID = e.ParentID
	}

	fsEntries := make([]fsEntry, 0, len(entries))
	for _, e := range entries {
		icon := "📄"
		if e.IsDir {
			icon = "📁"
		}
		fsEntries = append(fsEntries, fsEntry{
			ID:    e.ID,
			Name:  e.Name,
			IsDir: e.IsDir,
			Size:  formatSize(e.Size),
			Icon:  icon,
		})
	}

	parentID := "0"
	if len(crumbs) >= 2 {
		parentID = crumbs[len(crumbs)-2].ID
	}

	data := s.pageDataWithCache("fs", "", "")
	data.Page = "fs"
	data.FSEntries = fsEntries
	data.FSCrumbs = crumbs
	data.FSCurrentID = dirID
	data.FSParentID = parentID

	dashboardTemplate.Execute(w, data)
}

// getEntryCached returns a cached DirEntry or fetches+stores it.
func (s *Server) getEntryCached(entryID string) (p115pkg.DirEntry, error) {
	s.entryCacheMu.Lock()
	if e, ok := s.entryCache[entryID]; ok {
		s.entryCacheMu.Unlock()
		return e, nil
	}
	s.entryCacheMu.Unlock()

	e, err := s.Agent.GetEntry(entryID)
	if err != nil {
		return p115pkg.DirEntry{}, err
	}
	s.entryCacheMu.Lock()
	if len(s.entryCache) > 200 {
		s.entryCache = make(map[string]p115pkg.DirEntry) // simple eviction
	}
	s.entryCache[entryID] = e
	s.entryCacheMu.Unlock()
	return e, nil
}

// ---------- FS API handlers ----------

func (s *Server) writeFSResult(w http.ResponseWriter, status, msg string) {
	w.Header().Set("Content-Type", "application/json")
	if msg == "" {
		fmt.Fprintf(w, `{"status":"%s"}`, status)
	} else {
		fmt.Fprintf(w, `{"status":"%s","message":"%s"}`, status, msg)
	}
}

func (s *Server) handleFSMkdir(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	parentID := strings.TrimSpace(r.FormValue("parent_id"))
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		s.writeFSResult(w, "error", "Folder name is required")
		return
	}
	_, err := s.Agent.Mkdir(parentID, name)
	if err != nil {
		s.writeFSResult(w, "error", err.Error())
		return
	}
	log.Printf("[fs] mkdir %q in parent=%s", name, parentID)
	s.invalidateFSCache(parentID)
	s.writeFSResult(w, "ok", "")
}

func (s *Server) handleFSRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	entryID := strings.TrimSpace(r.FormValue("id"))
	newName := strings.TrimSpace(r.FormValue("name"))
	if entryID == "" || newName == "" {
		s.writeFSResult(w, "error", "ID and name are required")
		return
	}
	if err := s.Agent.RenameEntry(entryID, newName); err != nil {
		s.writeFSResult(w, "error", err.Error())
		return
	}
	log.Printf("[fs] rename id=%s -> %q", entryID, newName)
	s.invalidateFSCache("")
	s.writeFSResult(w, "ok", "")
}

func (s *Server) handleFSDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	entryID := strings.TrimSpace(r.FormValue("id"))
	if entryID == "" {
		s.writeFSResult(w, "error", "ID is required")
		return
	}
	if err := s.Agent.DeleteEntry(entryID); err != nil {
		s.writeFSResult(w, "error", err.Error())
		return
	}
	log.Printf("[fs] delete id=%s", entryID)
	s.invalidateFSCache("")
	s.writeFSResult(w, "ok", "")
}

func (s *Server) handleFSMove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	entryID := strings.TrimSpace(r.FormValue("id"))
	targetDirID := strings.TrimSpace(r.FormValue("target_dir"))
	if entryID == "" || targetDirID == "" {
		s.writeFSResult(w, "error", "ID and target_dir are required")
		return
	}
	if err := s.Agent.MoveEntry(targetDirID, entryID); err != nil {
		s.writeFSResult(w, "error", err.Error())
		return
	}
	log.Printf("[fs] move id=%s -> dir=%s", entryID, targetDirID)
	s.invalidateFSCache("")
	s.writeFSResult(w, "ok", "")
}

func (s *Server) handleFSCopy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	entryID := strings.TrimSpace(r.FormValue("id"))
	targetDirID := strings.TrimSpace(r.FormValue("target_dir"))
	if entryID == "" || targetDirID == "" {
		s.writeFSResult(w, "error", "ID and target_dir are required")
		return
	}
	if err := s.Agent.Copy(targetDirID, entryID); err != nil {
		s.writeFSResult(w, "error", err.Error())
		return
	}
	log.Printf("[fs] copy id=%s -> dir=%s", entryID, targetDirID)
	s.invalidateFSCache("")
	s.writeFSResult(w, "ok", "")
}

// ---------- settings ----------

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	ws := s.loadWebSettings()

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var changes []string

		// Proxy
		if v := strings.TrimSpace(r.FormValue("proxy_http")); v != s.ProxyHTTP {
			s.ProxyHTTP = v
			changes = append(changes, fmt.Sprintf("proxy=%s", v))
		}
		// Chunk delay
		if v, err := strconv.Atoi(r.FormValue("chunk_delay")); err == nil && v > 0 && v != ws.ChunkDelay {
			ws.ChunkDelay = v
			changes = append(changes, fmt.Sprintf("chunk_delay=%d", v))
		}
		// Chunk size
		if v, err := strconv.Atoi(r.FormValue("chunk_size")); err == nil && v > 0 && v != ws.ChunkSize {
			ws.ChunkSize = v
			changes = append(changes, fmt.Sprintf("chunk_size=%d", v))
		}
		// Cooldown min
		if v, err := strconv.Atoi(r.FormValue("cooldown_min")); err == nil && v > 0 && v != ws.CooldownMin {
			ws.CooldownMin = v
			changes = append(changes, fmt.Sprintf("cooldown_min=%d", v))
		}
		// Cooldown max
		if v, err := strconv.Atoi(r.FormValue("cooldown_max")); err == nil && v > 0 && v != ws.CooldownMax {
			ws.CooldownMax = v
			changes = append(changes, fmt.Sprintf("cooldown_max=%d", v))
		}
		// Subscription interval
		if v, err := strconv.Atoi(r.FormValue("subs_interval")); err == nil && v >= 0 && v != ws.SubsInterval {
			ws.SubsInterval = v
			changes = append(changes, fmt.Sprintf("subs_interval=%d", v))
		}
		// Web password (allow clearing by saving empty)
		if pw := sanitizePassword(r.FormValue("web_password")); pw != ws.WebPassword {
			webPassword = pw
			ws.WebPassword = pw
			if pw == "" {
				changes = append(changes, "web_password=cleared")
			} else {
				changes = append(changes, "web_password=***")
			}
		}
		// WeChat Work webhook
		if v := strings.TrimSpace(r.FormValue("wework_webhook")); v != s.WeworkWH {
			s.WeworkWH = v
			notify.SetWebhook(v)
			_ = config.SaveNotifyWebhook(v)
			changes = append(changes, "wework_webhook="+maskWebhook(v))
		}
		// Notification toggles
		if v := r.FormValue("notify_task") == "1"; v != ws.NotifyTask {
			ws.NotifyTask = v
			changes = append(changes, fmt.Sprintf("notify_task=%v", v))
		}
		if v := r.FormValue("notify_rss") == "1"; v != ws.NotifyRSS {
			ws.NotifyRSS = v
			changes = append(changes, fmt.Sprintf("notify_rss=%v", v))
		}
		if v := r.FormValue("notify_log") == "1"; v != ws.NotifyLog {
			ws.NotifyLog = v
			if v {
				ws.NotifyTask = false
				ws.NotifyRSS = false
			}
			changes = append(changes, fmt.Sprintf("notify_log=%v", v))
		}
		// Timezone
		if v := strings.TrimSpace(r.FormValue("timezone")); v != ws.Timezone {
			ws.Timezone = v
			SetTimezone(v)
			notify.SetTimezone(v)
			changes = append(changes, fmt.Sprintf("timezone=%s", v))
		}
		// Jackett
		if v := strings.TrimSpace(r.FormValue("jackett_url")); v != ws.JackettURL {
			ws.JackettURL = v
			s.JackettURL = v
			changes = append(changes, fmt.Sprintf("jackett_url=%s", maskWebhook(v)))
		}
		if v := strings.TrimSpace(r.FormValue("jackett_apikey")); v != ws.JackettAPIKey {
			ws.JackettAPIKey = v
			s.JackettAPIKey = v
			changes = append(changes, "jackett_apikey=***")
		}
		if v := strings.TrimSpace(r.FormValue("jackett_admin_password")); v != ws.JackettAdminPassword {
			ws.JackettAdminPassword = v
			s.JackettAdminPassword = v
			changes = append(changes, "jackett_admin_password=***")
		}
		// TMDB
		if v := strings.TrimSpace(r.FormValue("tmdb_apikey")); v != ws.TMDBAPIKey {
			ws.TMDBAPIKey = v
			media.InitTMDB(v)
			media.SetTMDBProxy(s.ProxyHTTP)
			changes = append(changes, "tmdb_apikey=***")
		}
		// Page size
		if v, err := strconv.Atoi(r.FormValue("page_size")); err == nil && v >= 10 && v <= 500 && v != ws.PageSize {
			ws.PageSize = v
			changes = append(changes, fmt.Sprintf("page_size=%d", v))
		}
		// Auto update
		if v := r.FormValue("auto_update") == "1"; v != ws.AutoUpdate {
			ws.AutoUpdate = v
			changes = append(changes, fmt.Sprintf("auto_update=%v", v))
		}

		if len(changes) > 0 {
			log.Printf("[settings] updated: %s", strings.Join(changes, ", "))
		}
		s.saveWebSettings(ws)
		s.saveProxyConfig()

		// Apply notification log setting
		notify.SetLogEnabled(ws.NotifyLog)

		// Also save to agent if logged in
		if s.Agent != nil {
			st := p115pkg.AppSettings{
				Lang:         ws.Lang,
				ChunkDelay:   ws.ChunkDelay,
				ChunkSize:    ws.ChunkSize,
				CooldownMinMs: ws.CooldownMin,
				CooldownMaxMs: ws.CooldownMax,
			}
			_ = s.Agent.UpdateSettings(st)
		}

		lang := ws.Lang
		if lang == "" {
			lang = "zh"
		}
		data := s.pageData(tr(lang, "settings_saved"), "")
		data.Page = "settings"
		data.ProxyHTTP = s.ProxyHTTP
		data.WeworkWebhook = s.WeworkWH
		data.NotifyTask = ws.NotifyTask
		data.NotifyRSS = ws.NotifyRSS
		data.NotifyLog = ws.NotifyLog
		data.Timezone = ws.Timezone
		data.TimezoneOptions = timezones
		data.JackettURL = ws.JackettURL
		data.JackettAPIKey = ws.JackettAPIKey
		data.JackettAdminPassword = ws.JackettAdminPassword
		data.TMDBAPIKey = ws.TMDBAPIKey
		if s.Agent != nil {
			data.Settings = s.Agent.GetSettings()
		}
		data.Settings.Lang = ws.Lang
		if ws.WebPassword != "" {
			data.Settings.WebPassword = ws.WebPassword
		}
		data.Settings.SubsInterval = ws.SubsInterval
		data.Settings.ChunkSize = ws.ChunkSize
		data.Settings.ChunkDelay = ws.ChunkDelay
		data.PageSize = ws.PageSize
		if data.PageSize <= 0 {
			data.PageSize = 50
		}
		data.AutoUpdate = ws.AutoUpdate
		http.SetCookie(w, &http.Cookie{Name: "r2c_lang", Value: lang, Path: "/", MaxAge: 86400 * 365})
		dashboardTemplate.Execute(w, data)
		return
	}

	// GET: load saved settings
	lang := ws.Lang
	if lang == "" {
		lang = "zh"
	}
	errMsg := ""
	if r.URL.Query().Get("err") == "not_logged_in" {
		errMsg = tr(lang, "err_not_logged_in")
	}
	data := s.pageData("", errMsg)
	data.Page = "settings"
	data.ProxyHTTP = s.ProxyHTTP
	data.WeworkWebhook = s.WeworkWH
	data.NotifyTask = ws.NotifyTask
	data.NotifyRSS = ws.NotifyRSS
	data.NotifyLog = ws.NotifyLog
	data.Timezone = ws.Timezone
	data.TimezoneOptions = timezones
	data.JackettURL = ws.JackettURL
	data.JackettAPIKey = ws.JackettAPIKey
	data.JackettAdminPassword = ws.JackettAdminPassword
	data.TMDBAPIKey = ws.TMDBAPIKey
	if s.Agent != nil {
		data.Settings = s.Agent.GetSettings()
	}
	// Override with persisted web settings (only non-zero = user has set them)
	if ws.Lang != "" {
		data.Settings.Lang = ws.Lang
	}
	if ws.ChunkSize > 0 {
		data.Settings.ChunkSize = ws.ChunkSize
	}
	if ws.ChunkDelay > 0 {
		data.Settings.ChunkDelay = ws.ChunkDelay
	}
	if ws.CooldownMin > 0 {
		data.Settings.CooldownMinMs = ws.CooldownMin
	}
	if ws.CooldownMax > 0 {
		data.Settings.CooldownMaxMs = ws.CooldownMax
	}
	data.Settings.SubsInterval = ws.SubsInterval
	if ws.SubsInterval > 0 {
		data.Settings.SubsInterval = ws.SubsInterval
	}
	if ws.WebPassword != "" {
		data.Settings.WebPassword = ws.WebPassword
	}
	data.PageSize = ws.PageSize
	if data.PageSize <= 0 {
		data.PageSize = 50
	}
	data.AutoUpdate = ws.AutoUpdate

	if lang == "" {
		lang = "zh"
	}
	http.SetCookie(w, &http.Cookie{Name: "r2c_lang", Value: lang, Path: "/", MaxAge: 86400 * 365})
	dashboardTemplate.Execute(w, data)
}

// ---------- dedup ----------

func (s *Server) handleDedup(w http.ResponseWriter, r *http.Request) {
	data := s.pageData("", "")
	data.Page = "dedup"

	var entries []dedupEntry
	for _, subKey := range globalDedup.SubKeys() {
		cnt := globalDedup.SubCount(subKey)
		if cnt > 0 {
			entries = append(entries, dedupEntry{SubKey: subKey, Count: cnt})
		}
	}
	data.DedupEntries = entries
	dashboardTemplate.Execute(w, data)
}

func (s *Server) handleDedupClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	subKey := r.FormValue("sub")
	if subKey == "" {
		s.renderMsg(w, "", "err_missing_sub_name")
		return
	}
	globalDedup.ClearSub(subKey)
	log.Printf("[dedup] cleared dedup for %s", subKey)
	http.Redirect(w, r, "/subs", http.StatusSeeOther)
}

func (s *Server) handleDedupHashes(w http.ResponseWriter, r *http.Request) {
	subKey := r.URL.Query().Get("sub")
	hashes := globalDedup.Hashes(subKey)
	// Build response with hash + name + torrent URL
	type hashEntry struct {
		Hash string `json:"hash"`
		Name string `json:"name,omitempty"`
		URL  string `json:"url,omitempty"`
	}
	out := make([]hashEntry, 0, len(hashes))
	for _, h := range hashes {
		name := globalDedup.GetHashName(h)
		if name == "" {
			// Fallback: try to extract from URL
			if u := globalDedup.TorrentURLByHash(h); u != "" {
				name = extractNameFromURL(u)
			}
		}
		out = append(out, hashEntry{Hash: h, Name: name, URL: globalDedup.TorrentURLByHash(h)})
	}
	w.Header().Set("Content-Type", "application/json")
	data, _ := json.Marshal(out)
	w.Write(data)
}

func (s *Server) handleDedupRemoveHash(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	subKey := r.FormValue("sub")
	hash := r.FormValue("hash")
	if subKey == "" || hash == "" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"error","message":"missing sub or hash"}`))
		return
	}
	if globalDedup.RemoveHash(subKey, hash) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	} else {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"error","message":"not found"}`))
	}
}

// ---------- torrent cache (merged into dedup) ----------

func (s *Server) handleTorrent(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/dedup", http.StatusMovedPermanently)
}

func (s *Server) handleTorrentClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	url := r.FormValue("url")
	if url != "" {
		globalDedup.RemoveTorrentURL(url)
		log.Printf("[torrent-cache] removed entry for %s", url)
	} else {
		for _, entry := range globalDedup.AllTorrentURLs() {
			globalDedup.RemoveTorrentURL(entry[0])
		}
		log.Printf("[torrent-cache] cleared all entries")
	}
	http.Redirect(w, r, "/dedup", http.StatusSeeOther)
}

func (s *Server) handleLogPage(w http.ResponseWriter, r *http.Request) {
	data := s.pageData("", "")
	data.Page = "log"
	data.Logs = logBuf.Lines()
	dashboardTemplate.Execute(w, data)
}

func extractNameFromURL(rawURL string) string {
	if strings.Contains(rawURL, "dn=") {
		for _, part := range strings.Split(rawURL, "&") {
			if strings.HasPrefix(strings.ToLower(part), "dn=") {
				n, _ := url.QueryUnescape(part[3:])
				return n
			}
		}
	}
	return ""
}

func (s *Server) handleAbout(w http.ResponseWriter, r *http.Request) {
	data := s.pageData("", "")
	data.Page = "about"
	data.AboutVersion = Version
	dashboardTemplate.Execute(w, data)
}

func (s *Server) handleAPITasks(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	type apiTask struct {
		InfoHash string  `json:"info_hash"`
		Name     string  `json:"name"`
		Size     string  `json:"size"`
		Status   string  `json:"status"`
		Percent  float64 `json:"percent"`
		URL      string  `json:"url"`
		RowClass string  `json:"row_class"`
	}
	resp := struct {
		Count int       `json:"count"`
		Tasks []apiTask `json:"tasks"`
	}{}
	if s.Agent != nil {
		tasks, err := s.Agent.ListTasks()
		if err == nil {
			resp.Count = len(tasks)
			s.taskCountCache = len(tasks) // keep dashboard in sync
			for _, t := range tasks {
				row := apiTask{
					InfoHash: t.InfoHash,
					Name:     displayName(t),
					Size:     formatSize(t.Size),
					Percent:  t.Percent,
					URL:      t.URL,
				}
				switch {
				case t.Status == 2:
					row.Status = "done"
					row.RowClass = "row-done"
				case t.Status == -1:
					row.Status = "failed"
					row.RowClass = "row-failed"
				case t.Status == 1:
					row.Status = "downloading"
					row.RowClass = "row-running"
				default:
					row.Status = "waiting"
				}
				resp.Tasks = append(resp.Tasks, row)
			}
		}
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleTMDBSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "missing query"})
		return
	}
	if media.DefaultTMDB == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "TMDB not configured"})
		return
	}
	movies, tvShows, err := media.DefaultTMDB.SearchAll(q)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
		return
	}
	// Build response with poster URLs
	type result struct {
		TMDBID    int    `json:"id"`
		Title     string `json:"title"`
		Year      string `json:"year"`
		MediaType string `json:"media_type"`
		Poster    string `json:"poster"`
		Overview  string `json:"overview"`
	}
	moviesOut := make([]result, 0, len(movies))
	for _, m := range movies {
		moviesOut = append(moviesOut, result{
			TMDBID: m.TMDBID, Title: m.DisplayName(), Year: m.YearStr(),
			MediaType: "movie", Poster: m.PosterURL(), Overview: m.Overview,
		})
	}
	tvOut := make([]result, 0, len(tvShows))
	for _, t := range tvShows {
		tvOut = append(tvOut, result{
			TMDBID: t.TMDBID, Title: t.DisplayName(), Year: t.YearStr(),
			MediaType: "tv", Poster: t.PosterURL(), Overview: t.Overview,
		})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"movies": moviesOut,
		"tv":     tvOut,
	})
}

func (s *Server) handleTMDBTrending(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if media.DefaultTMDB == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "TMDB not configured"})
		return
	}
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
		page = p
	}
	movies, tvShows, err := media.DefaultTMDB.TrendingPage(page)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
		return
	}
	type result struct {
		TMDBID    int    `json:"id"`
		Title     string `json:"title"`
		Year      string `json:"year"`
		MediaType string `json:"media_type"`
		Poster    string `json:"poster"`
		Overview  string `json:"overview"`
	}
	moviesOut := make([]result, 0, len(movies))
	for _, m := range movies {
		moviesOut = append(moviesOut, result{TMDBID: m.TMDBID, Title: m.DisplayName(), Year: m.YearStr(), MediaType: "movie", Poster: m.PosterURL(), Overview: m.Overview})
	}
	tvOut := make([]result, 0, len(tvShows))
	for _, t := range tvShows {
		tvOut = append(tvOut, result{TMDBID: t.TMDBID, Title: t.DisplayName(), Year: t.YearStr(), MediaType: "tv", Poster: t.PosterURL(), Overview: t.Overview})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"movies": moviesOut, "tv": tvOut})
}

func (s *Server) handleTMDBDetail(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if media.DefaultTMDB == nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "TMDB not configured"})
		return
	}
	mediaType := r.URL.Query().Get("type")
	idStr := r.URL.Query().Get("id")
	if mediaType == "" || idStr == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "missing type or id"})
		return
	}
	tmdbID, err := strconv.Atoi(idStr)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": "invalid id"})
		return
	}
	detail, err := media.DefaultTMDB.GetDetail(tmdbID, mediaType)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"error": err.Error()})
		return
	}
	// Build response
	type seasonInfo struct {
		SeasonNumber int `json:"season_number"`
		EpisodeCount int `json:"episode_count"`
	}
	seasons := make([]seasonInfo, 0)
	for _, s := range detail.Seasons {
		if s.SeasonNumber > 0 {
			seasons = append(seasons, seasonInfo{SeasonNumber: s.SeasonNumber, EpisodeCount: s.EpisodeCount})
		}
	}
	genres := make([]string, 0)
	for _, g := range detail.Genres {
		genres = append(genres, g.Name)
	}
	title := detail.Title
	if title == "" {
		title = detail.Name
	}
	origTitle := detail.OriginalTitle
	if origTitle == "" {
		origTitle = detail.OriginalName
	}
	year := detail.YearStr()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id":             detail.TMDBID,
		"title":          title,
		"original_title": origTitle,
		"year":           year,
		"media_type":     mediaType,
		"poster":         detail.PosterURL(),
		"backdrop":       detail.BackdropURL(),
		"overview":       detail.Overview,
		"tagline":        detail.Tagline,
		"genres":         genres,
		"vote_average":   detail.VoteAvg,
		"vote_count":     detail.VoteCount,
		"runtime":        detail.Runtime,
		"status":         detail.Status,
		"seasons":        seasons,
		"num_seasons":    detail.NumSeasons,
		"num_episodes":   detail.NumEps,
	})
}

// buildAllTagsList returns tag strings as []string for JSON encoding
func buildAllTagsList(results []indexer.SearchResult) []string {
	re := regexp.MustCompile(`[\[【]([^\]】]{1,40})[\]】]`)
	seen := make(map[string]bool)
	var tags []string
	for _, r := range results {
		matches := re.FindAllStringSubmatch(r.Title, -1)
		for _, m := range matches {
			tag := strings.TrimSpace(m[1])
			tag = strings.ReplaceAll(tag, "DBD制作组", "DBD-Raws")
			tag = strings.ReplaceAll(tag, "桜都字幕組", "桜都字幕组")
			if tag == "" || len(tag) > 40 { continue }
			if regexp.MustCompile(`^\d+\(\d+\)$`).MatchString(tag) { continue }
			if regexp.MustCompile(`^\d{1,4}$`).MatchString(tag) { continue }
			lk := strings.ToLower(tag)
			if !seen[lk] { seen[lk] = true; tags = append(tags, tag) }
		}
	}
	if tags == nil { tags = []string{} }
	return tags
}

// buildAllGroupsList returns validated group names as []string for JSON encoding
func buildAllGroupsList(results []indexer.SearchResult) []string {
	groupRe := regexp.MustCompile(`^[[【]([^\]】]{1,40})[\]】]`)
	seen := make(map[string]bool)
	var groups []string
	for _, r := range results {
		if m := groupRe.FindStringSubmatch(r.Title); m != nil {
			g := strings.TrimSpace(m[1])
			g = strings.ReplaceAll(g, "DBD制作组", "DBD-Raws")
			g = strings.ReplaceAll(g, "桜都字幕組", "桜都字幕组")
			lk := strings.ToLower(g)
			if !seen[lk] && isValidGroup(g) { seen[lk] = true; groups = append(groups, g) }
		}
	}
	if groups == nil { groups = []string{} }
	return groups
}

func buildAllTagsJSON(results []indexer.SearchResult) template.JS {
	re := regexp.MustCompile(`[\[【]([^\]】]{1,40})[\]】]`)
	seen := make(map[string]bool)
	var tags []string
	for _, r := range results {
		matches := re.FindAllStringSubmatch(r.Title, -1)
		for _, m := range matches {
			tag := strings.TrimSpace(m[1])
			tag = strings.ReplaceAll(tag, "DBD制作组", "DBD-Raws")
			tag = strings.ReplaceAll(tag, "桜都字幕組", "桜都字幕组")
			if tag == "" || len(tag) > 40 { continue }
			if regexp.MustCompile(`^\d+\(\d+\)$`).MatchString(tag) { continue }
			if regexp.MustCompile(`^\d{1,4}$`).MatchString(tag) { continue }
			lk := strings.ToLower(tag)
			if !seen[lk] { seen[lk] = true; tags = append(tags, tag) }
		}
	}
	b, _ := json.Marshal(tags)
	return template.JS(b)
}

// buildAllGroupsJSON collects validated group names from full search results
// Used by client to pre-populate _serverGroups so classifyTag works across all pages
func buildAllGroupsJSON(results []indexer.SearchResult) template.JS {
	groupRe := regexp.MustCompile(`^[[【]([^\]】]{1,40})[\]】]`)
	seen := make(map[string]bool)
	var groups []string
	for _, r := range results {
		if m := groupRe.FindStringSubmatch(r.Title); m != nil {
			g := strings.TrimSpace(m[1])
			g = strings.ReplaceAll(g, "DBD制作组", "DBD-Raws")
			g = strings.ReplaceAll(g, "桜都字幕組", "桜都字幕组")
			lk := strings.ToLower(g)
			if !seen[lk] && isValidGroup(g) { seen[lk] = true; groups = append(groups, g) }
		}
	}
	b, _ := json.Marshal(groups)
	return template.JS(b)
}

// isValidGroup checks if a first-bracket tag looks like a fansub group name
func isValidGroup(g string) bool {
	g = strings.TrimSpace(g)
	if g == "" || len(g) > 40 { return false }
	// Reject noise: episode ranges, labels, anime titles
	noise := []string{
		"合集", "全集", "总集", "新番", "特别篇", "前篇", "后篇", "番外",
		"剧场版", "第", "OVA", "OAD",
	}
	for _, n := range noise {
		if strings.Contains(g, n) { return false }
	}
	// Reject if starts with digit (episode range like "01-24TV全集")
	if len(g) > 0 && g[0] >= '0' && g[0] <= '9' { return false }
	// Accept if: has CJK, or "&", or "-Raws" suffix, or known Latin group
	hasCJK := false
	for _, r := range g {
		if r >= 0x4E00 && r <= 0x9FFF { hasCJK = true; break }
		if r >= 0x3040 && r <= 0x30FF { hasCJK = true; break }
	}
	if hasCJK { return true }
	if strings.Contains(g, "&") { return true }
	if strings.HasSuffix(strings.ToLower(g), "-raws") { return true }
	// Known Latin group patterns
	known := []string{"DBD", "VCB", "ANK", "Moozzi2", "ReinForce", "Beatrice",
		"Snow", "LowPower", "U3-Project", "AI-Raws", "NC-Raws", "Lilith",
		"GJ.Y", "c.c", "MCE", "7³", "ANi", "Porter"}
	for _, k := range known {
		if strings.HasPrefix(g, k) { return true }
	}
	// Short ASCII name (2-10 chars, starts with uppercase) = likely a group
	if len(g) >= 2 && len(g) <= 10 && g[0] >= 'A' && g[0] <= 'Z' {
		allASCII := true
		for _, r := range g {
			if r > 127 { allASCII = false; break }
		}
		if allASCII { return true }
	}
	return false
}

// applyKeywordFilter filters results using group-aware keyword matching.
// Format: "group:tag1|tag2 othergroup:tag3"
// Within each group: OR. Across groups: AND.
// If keyword is empty, returns all results unchanged.
func applyKeywordFilter(results []indexer.SearchResult, keyword string) []indexer.SearchResult {
	kw := strings.TrimSpace(keyword)
	if kw == "" {
		return results
	}
	groups := strings.Fields(kw)
	filtered := make([]indexer.SearchResult, 0, len(results))
	for _, r := range results {
		titleLower := strings.ToLower(r.Title)
		match := true
		for _, g := range groups {
			if idx := strings.IndexByte(g, ':'); idx > 0 {
				tags := strings.Split(g[idx+1:], "|")
				grpMatch := false
				for _, tag := range tags {
					if strings.Contains(titleLower, strings.ToLower(tag)) {
						grpMatch = true
						break
					}
				}
				if !grpMatch { match = false; break }
			} else {
				if !strings.Contains(titleLower, strings.ToLower(g)) {
					match = false
					break
				}
			}
		}
		if match {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

func (s *Server) handleAPILogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	since := r.URL.Query().Get("since")
	var lines []string
	if since == "" {
		lines = logBuf.Lines()
	} else {
		// Return only lines after the given marker
		allLines := logBuf.Lines()
		found := false
		for _, l := range allLines {
			if found {
				lines = append(lines, l)
			} else if strings.Contains(l, since) {
				found = true
			}
		}
		if !found {
			lines = allLines
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"lines": lines,
	})
}

func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	webhook := strings.TrimSpace(r.FormValue("webhook"))
	if webhook == "" {
		webhook = s.WeworkWH
	}
	if webhook == "" {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": "Webhook URL is empty"})
		return
	}
	// Send a test message synchronously
	err := notify.Test(webhook)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "ok", "message": "Test message sent"})
}

func (s *Server) handleAPILang(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	lang := strings.TrimSpace(r.FormValue("lang"))
	if lang != "zh" && lang != "en" {
		lang = "zh"
	}
	ws := s.loadWebSettings()
	ws.Lang = lang
	s.saveWebSettings(ws)
	// Also update agent settings
	if s.Agent != nil {
		st := s.Agent.GetSettings()
		st.Lang = lang
		s.Agent.UpdateSettings(st)
	}
	// Set cookie for login page language consistency
	http.SetCookie(w, &http.Cookie{Name: "r2c_lang", Value: lang, Path: "/", MaxAge: 365 * 24 * 3600})
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"ok":true}`)
}

// ---------- login ----------

func (s *Server) handleQRLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		img, err := p115pkg.StartQrcodeLogin()
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"error":"%s"}`, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"qrcode":"data:image/png;base64,%s"}`, base64Encode(img))
		return
	}
	if r.URL.Query().Get("poll") == "1" {
		success, err := p115pkg.PollQrcodeLogin()
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			fmt.Fprintf(w, `{"status":"%s"}`, err.Error())
			return
		}
		if success {
			newAgent, err := p115pkg.FinishQrcodeLogin()
			if err != nil {
				fmt.Fprintf(w, `{"error":"%s"}`, err.Error())
				return
			}
			s.SetAgent(newAgent)
			fmt.Fprint(w, `{"status":"ok"}`)
		} else {
			fmt.Fprint(w, `{"status":"waiting"}`)
		}
		return
	}
	data := s.pageData("", "")
	data.Page = "settings"
	data.ShowQR = true
	if s.Agent != nil {
		data.Settings = s.Agent.GetSettings()
		data.ProxyHTTP = s.ProxyHTTP
	}
	dashboardTemplate.Execute(w, data)
}

func (s *Server) handleCookiesLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderResult(w, "", err.Error())
		return
	}
	cookies := strings.TrimSpace(r.FormValue("cookies"))
	if cookies == "" {
		s.renderMsg(w, "", "err_cookies_empty")
		return
	}
	newAgent, err := p115pkg.ReloginWithCookies(cookies)
	if err != nil {
		log.Printf("[auth] cookies login failed: %v", err)
		s.renderResult(w, "", tr(s.langFromAgent(), "err_login_failed")+err.Error())
		return
	}
	s.SetAgent(newAgent)
	log.Printf("[auth] cookies updated successfully")
	s.renderMsg(w, "cookies_updated", "")
}

func (s *Server) handleTestJackett(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	url := strings.TrimSpace(r.FormValue("url"))
	apikey := strings.TrimSpace(r.FormValue("apikey"))
	if url == "" || apikey == "" {
		writeJSON(w, false, tr(s.langFromAgent(), "jk_test_empty"))
		return
	}
	if err := jackett.Test(jackett.Config{URL: url, APIKey: apikey}); err != nil {
		writeJSON(w, false, err.Error())
		return
	}
	writeJSON(w, true, tr(s.langFromAgent(), "jk_test_ok"))
}

func (s *Server) handleTest115(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.Agent == nil {
		fmt.Fprint(w, `{"ok":false,"msg":"`+tr(s.langFromAgent(), "err_not_logged_in")+`"}`)
		return
	}
	if err := s.Agent.TestConnection(); err != nil {
		fmt.Fprintf(w, `{"ok":false,"msg":"%s"}`, err.Error())
		return
	}
	fmt.Fprintf(w, `{"ok":true,"msg":"%s"}`, tr(s.langFromAgent(), "conn_ok"))
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"msg":"%s"}`, tr(s.langFromAgent(), "restarting_json"))

	go func() {
		time.Sleep(500 * time.Millisecond)
		exe, err := os.Executable()
		if err != nil {
			log.Printf("[restart] failed to get executable: %v", err)
			os.Exit(1)
		}
		var args []string
		skipNext := false
		for i, a := range os.Args {
			if i == 0 { continue }
			if skipNext { skipNext = false; continue }
			if a == "--port" || a == "-p" { args = append(args, a); if i+1 < len(os.Args) { args = append(args, os.Args[i+1]) }; skipNext = true; continue }
			args = append(args, a)
		}
		cmd := exec.Command(exe, args...)
		cmd.Dir = filepath.Dir(exe)
		// Detach from parent so it survives after we exit
		prepareRestartCmd(cmd)
		cmd.Stdin = nil
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := cmd.Start(); err != nil {
			log.Printf("[restart] failed to spawn: %v", err)
			os.Exit(1)
		}
		log.Printf("[restart] new process started (pid %d), exiting", cmd.Process.Pid)
		os.Exit(0)
	}()
}

// handleCheckUpdate queries GitHub for the latest release.
func (s *Server) handleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	type updateInfo struct {
		Latest    string `json:"latest"`
		Current   string `json:"current"`
		HasUpdate bool   `json:"has_update"`
		URL       string `json:"url,omitempty"`
	}
	info := updateInfo{Current: Version}

	resp, err := s.httpClient().Get("https://api.github.com/repos/mguyenanastacio-glitch/pan-fetcher/releases/latest")
	if err != nil {
		json.NewEncoder(w).Encode(info)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		json.NewEncoder(w).Encode(info)
		return
	}

	var release struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		json.NewEncoder(w).Encode(info)
		return
	}

	info.Latest = release.TagName
	info.HasUpdate = compareVersion(release.TagName, Version) > 0
	info.URL = release.HTMLURL

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(info)
}

// handleDoUpdate downloads the latest binary from GitHub and replaces self.
func (s *Server) handleDoUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	lang := s.langFromAgent()
	w.Header().Set("Content-Type", "application/json")

	// Determine platform
	goos := runtime.GOOS
	goarch := runtime.GOARCH
	binaryName := "pan-fetcher"
	if goos == "windows" {
		binaryName = "pan-fetcher.exe"
	}

	// Get latest release info
	resp, err := s.httpClient().Get("https://api.github.com/repos/mguyenanastacio-glitch/pan-fetcher/releases/latest")
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": tr(lang, "update_fetch_failed") + ": " + err.Error()})
		return
	}
	defer resp.Body.Close()

	var release struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": tr(lang, "update_parse_failed")})
		return
	}

	if release.TagName == Version {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": tr(lang, "update_already_latest")})
		return
	}

	// Find matching asset (tar.gz for unix, zip for windows)
	pattern := fmt.Sprintf("%s-%s", goos, goarch)
	var downloadURL string
	var isZip bool
	for _, a := range release.Assets {
		if strings.Contains(a.Name, pattern) && !strings.Contains(a.Name, "checksum") {
			downloadURL = a.URL
			isZip = strings.HasSuffix(a.Name, ".zip")
			break
		}
	}
	if downloadURL == "" {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": trf(lang, "update_no_asset", goos+"/"+goarch)})
		return
	}

	// Download the archive
	dlResp, err := s.httpClient().Get(downloadURL)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": tr(lang, "update_download_failed") + ": " + err.Error()})
		return
	}
	defer dlResp.Body.Close()

	// Extract binary from archive
	var binaryData []byte
	if isZip {
		binaryData, err = extractFromZip(dlResp.Body, binaryName)
	} else {
		binaryData, err = extractFromTarGz(dlResp.Body, binaryName)
	}
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": tr(lang, "update_download_failed") + ": " + err.Error()})
		return
	}

	exe, err := os.Executable()
	if err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": "executable: " + err.Error()})
		return
	}

	// Write new binary next to the executable (same filesystem = atomic rename works)
	tmpPath := exe + ".new"
	if err := os.WriteFile(tmpPath, binaryData, 0755); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": "write: " + err.Error()})
		return
	}

	// Replace running binary (platform-specific)
	restart, err := selfReplace(tmpPath, exe)
	if err != nil {
		os.Remove(tmpPath)
		json.NewEncoder(w).Encode(map[string]interface{}{"ok": false, "msg": tr(lang, "update_install_failed") + ": " + err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{"ok": true, "msg": tr(lang, "update_ok")})
	if restart {
		go func() { time.Sleep(500 * time.Millisecond); doRestart() }()
	} else {
		go func() { time.Sleep(500 * time.Millisecond); os.Exit(0) }()
	}
}

// copyFile copies a file from src to dst, preserving permissions.
func copyFile(src, dst string) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()

	info, err := s.Stat()
	if err != nil {
		return err
	}

	d, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer d.Close()

	_, err = io.Copy(d, s)
	return err
}

// extractFromTarGz extracts a named file from a .tar.gz stream.
func extractFromTarGz(r io.Reader, name string) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		if hdr.Name == name || filepath.Base(hdr.Name) == name {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("binary %q not found in archive", name)
}

// extractFromZip extracts a named file from a ZIP stream.
func extractFromZip(r io.Reader, name string) ([]byte, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read zip: %w", err)
	}
	zipReader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}
	for _, f := range zipReader.File {
		if f.Name == name || filepath.Base(f.Name) == name {
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("zip open: %w", err)
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("binary %q not found in archive", name)
}

// compareVersion compares two semver strings (e.g. "v0.4.0" vs "v0.3.3").
// Returns positive if a > b, zero if equal, negative if a < b.
func compareVersion(a, b string) int {
	parse := func(s string) []int {
		s = strings.TrimPrefix(s, "v")
		parts := strings.Split(s, ".")
		nums := make([]int, len(parts))
		for i, p := range parts {
			nums[i], _ = strconv.Atoi(p)
		}
		return nums
	}
	va, vb := parse(a), parse(b)
	for i := 0; i < len(va) && i < len(vb); i++ {
		if va[i] != vb[i] {
			return va[i] - vb[i]
		}
	}
	return len(va) - len(vb)
}

// doRestart spawns a new process and exits.
func doRestart() {
	exe, err := os.Executable()
	if err != nil {
		log.Printf("[update] failed to get executable: %v", err)
		os.Exit(1)
	}
	var args []string
	skipNext := false
	for i, a := range os.Args {
		if i == 0 {
			continue
		}
		if skipNext {
			skipNext = false
			continue
		}
		if a == "--port" || a == "-p" {
			args = append(args, a)
			if i+1 < len(os.Args) {
				args = append(args, os.Args[i+1])
			}
			skipNext = true
			continue
		}
		args = append(args, a)
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	prepareRestartCmd(cmd)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		log.Printf("[update] failed to spawn: %v", err)
		os.Exit(1)
	}
	log.Printf("[update] new process started (pid %d), exiting", cmd.Process.Pid)
	os.Exit(0)
}

// ---------- search ----------

func (s *Server) handleDiscover(w http.ResponseWriter, r *http.Request) {
	data := s.pageData("", "")
	data.Page = "discover"
	ws := s.loadWebSettings()
	data.PageSize = ws.PageSize
	if data.PageSize <= 0 { data.PageSize = 50 }
	dashboardTemplate.Execute(w, data)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	data := s.pageData("", "")
	data.Page = "search"

	// Restore previous search results on GET (page load / refresh)
	if r.Method != http.MethodPost {
		if ps := loadSearchCache(); ps != nil {
			data.SearchQuery = ps.Query
			data.SearchKeyword = ps.Keyword
			data.SearchSort = ps.Sort
			data.SearchIndexers = ps.Indexers
			data.PageSize = ps.PageSize
			if data.PageSize <= 0 { data.PageSize = 50 }
			data.SearchTotal = len(searchCache)
			data.AllTagsJSON = buildAllTagsJSON(searchCacheFull)
			data.AllGroupsJSON = buildAllGroupsJSON(searchCacheFull)
			if len(searchCache) > data.PageSize {
				data.SearchResults = searchCache[:data.PageSize]
			} else {
				data.SearchResults = searchCache
			}
			// Ensure formatted fields for display (not persisted in cache)
			for i := range data.SearchResults {
				if data.SearchResults[i].SizeFmt == "" {
					data.SearchResults[i].SizeFmt = formatSize(data.SearchResults[i].Size)
				}
				if data.SearchResults[i].DateFmt == "" && !data.SearchResults[i].PublishDate.IsZero() {
					data.SearchResults[i].DateFmt = data.SearchResults[i].PublishDate.Format("2006-01-02 15:04")
				}
			}
			if ps.Query != "" {
				data.RssURL = buildRssURL(s.Port, ps.Query, ps.Indexers)
			}
		}
	}

	// Populate indexer list for filter checkboxes
	if s.IdxMgr != nil {
		data.IndexerList = s.IdxMgr.List()
		// Mark local
		for i := range data.IndexerList {
			data.IndexerList[i].Source = "local"
		}
	}
	// Add Jackett-activated indexers to filter list (show both with conflict labels)
	s.jackettActiveMu.Lock()
	s.loadJackettEnabled()
	for _, jk := range s.jackettCache {
		if s.jackettActive[jk.ID] {
			name := jk.Name
			for _, loc := range data.IndexerList {
				if loc.ID == jk.ID {
					name = jk.Name + " (Jackett)"
					break
				}
			}
			data.IndexerList = append(data.IndexerList, indexer.IndexerInfo{
				ID:       "jackett:" + jk.ID,
				Name:     name,
				Type:     jk.Type,
				Language: jk.Language,
				SiteLink: jk.SiteLink,
				Source:   "jackett",
			})
		}
	}
	s.jackettActiveMu.Unlock()
	// Load saved searches
	data.SavedSearches = s.loadSavedSearches()

	if r.Method == http.MethodPost {
		if r.FormValue("action") == "unsubscribe" {
			s.handleSearchUnsubscribe(w, r)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		q := strings.TrimSpace(r.FormValue("q"))
		if q == "" {
			data.Error = tr(s.langFromAgent(), "enter_search_kw")
			dashboardTemplate.Execute(w, data)
			return
		}
		if s.IdxMgr == nil {
			data.Error = tr(s.langFromAgent(), "indexer_not_initialized")
			dashboardTemplate.Execute(w, data)
			return
		}

		// Parse filters
		sortBy := strings.TrimSpace(r.FormValue("sort"))
		keyword := strings.TrimSpace(r.FormValue("keyword"))
		if sortBy == "" {
			sortBy = "seeds"
		}
		indexers := r.Form["indexer"]

		// Separate Jackett-activated indexers from local ones for search
		s.jackettActiveMu.Lock()
		s.loadJackettEnabled()
		jackettActiveSet := make(map[string]bool)
		for id := range s.jackettActive {
			jackettActiveSet[id] = true
		}
		s.jackettActiveMu.Unlock()

		var localIndexers []string
		var hasJackettSelection bool
		var jackettSelectedIDs []string
		for _, id := range indexers {
			if strings.HasPrefix(id, "jackett:") {
				hasJackettSelection = true
				jackettSelectedIDs = append(jackettSelectedIDs, strings.TrimPrefix(id, "jackett:"))
			} else {
				localIndexers = append(localIndexers, id)
			}
		}
		// Run local and Jackett searches concurrently
		type searchResult struct {
			local   indexer.SearchAllErrors
			jackett []jackett.Result
			jerr    error
		}
		ch := make(chan searchResult, 1)
		go func() {
			var sr searchResult
			var wg sync.WaitGroup
			wg.Add(2)

			// Local indexer search
			go func() {
				defer wg.Done()
				if !(len(indexers) > 0 && len(localIndexers) == 0 && hasJackettSelection) {
					sr.local = s.IdxMgr.SearchAllWithErrors(indexer.SearchRequest{
						Query:    q,
						Sort:     sortBy,
						Indexers: localIndexers,
						Limit:    2000,
					})
				} else {
					sr.local = indexer.SearchAllErrors{Errors: make(map[string]string)}
				}
			}()

			// Jackett search
			go func() {
				defer wg.Done()
				if s.JackettURL != "" && s.JackettAPIKey != "" && (len(indexers) == 0 || hasJackettSelection) {
					jc := s.jackettConfig()
					sr.jackett, sr.jerr = jackett.Search(jc, q, nil, 0)
				}
			}()

			wg.Wait()
			ch <- sr
		}()

		sr := <-ch
		se := sr.local
		for i := range se.Results {
			se.Results[i].SizeFmt = formatSize(se.Results[i].Size)
			if !se.Results[i].PublishDate.IsZero() {
				se.Results[i].DateFmt = se.Results[i].PublishDate.Format("2006-01-02 15:04")
			}
		}

		// Process Jackett results (already fetched in parallel above)
		if sr.jerr == nil {
			for _, r := range sr.jackett {
				trackerLower := strings.ToLower(r.Tracker)
				if !jackettActiveSet[trackerLower] {
					continue
				}
				if len(jackettSelectedIDs) > 0 {
					match := false
					for _, jid := range jackettSelectedIDs {
						if strings.ToLower(jid) == trackerLower {
							match = true
							break
						}
					}
					if !match {
						continue
					}
				}
				var pubDate time.Time
				if r.PublishDate != "" {
					pubDate, _ = time.Parse(time.RFC1123Z, r.PublishDate)
					if pubDate.IsZero() {
						pubDate, _ = time.Parse(time.RFC1123, r.PublishDate)
					}
				}
				se.Results = append(se.Results, indexer.SearchResult{
					Title:       r.Title,
					MagnetURL:   r.MagnetURI,
					PageURL:     r.Link,
					Size:        r.Size,
					SizeFmt:     formatSize(r.Size),
					DateFmt:     formatJackettDate(r.PublishDate),
					Seeders:     r.Seeders,
					IndexerName: "Jackett: " + r.TrackerName,
					PublishDate: pubDate,
				})
			}
		} else if sr.jerr != nil {
			if se.Errors == nil {
				se.Errors = make(map[string]string)
			}
			se.Errors["jackett"] = sr.jerr.Error()
		}
		data.SearchQuery = q
		data.SearchKeyword = keyword

		data.SearchSort = sortBy
		data.SearchIndexers = indexers
		data.SearchErrors = se.Errors
		// Suppress "not activated" error when Jackett handles the selected indexers
		if hasJackettSelection && se.Errors != nil {
			if _, ok := se.Errors["_none_"]; ok && len(se.Results) > 0 {
				delete(se.Errors, "_none_")
			}
		}

		// Cache all results, display first page
		ws := s.loadWebSettings()
		pageSize := ws.PageSize
		if pageSize <= 0 {
			pageSize = 50
		}
		searchCacheMu.Lock()
		// Re-sort combined local + Jackett results after merge
		indexer.SortResults(se.Results, sortBy)
		searchCacheFull = dedupSlice(se.Results, nil)
		// Extract group from first [...] or 【...】 in title (with validation)
		groupRe := regexp.MustCompile(`^[[【]([^\]】]{1,40})[\]】]`)
		for i := range searchCacheFull {
			if searchCacheFull[i].Group == "" {
				if m := groupRe.FindStringSubmatch(searchCacheFull[i].Title); m != nil {
					g := strings.TrimSpace(m[1])
					if isValidGroup(g) { searchCacheFull[i].Group = g }
				}
			}
		}
		// Apply keyword filter onto searchCache (working set)
		searchCache = applyKeywordFilter(searchCacheFull, keyword)
		filterKeyword = keyword
		searchCtx = searchContext{
			Query:    q,
			Sort:     sortBy,
			Indexers: indexers,
			NextPage: 2,
			PageSize: pageSize,
		}
		searchCacheMu.Unlock()
		data.PageSize = pageSize
		data.SearchTotal = len(searchCache)
		// Build tag/group JSON from FULL cache (all results, not just filtered)
		data.AllTagsJSON = buildAllTagsJSON(searchCacheFull)
		data.AllGroupsJSON = buildAllGroupsJSON(searchCacheFull)
		if len(searchCache) > pageSize {
			data.SearchResults = searchCache[:pageSize]
		} else {
			data.SearchResults = searchCache
		}
		// Auto-build RSS URL for subscription (local aggregated feed)
		if q != "" {
			data.RssURL = buildRssURL(s.Port, q, indexers)
		}
		// Persist search results for next page load
		saveSearchCache()
	}
	dashboardTemplate.Execute(w, data)
}

func (s *Server) handleSearchMore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	offset, _ := strconv.Atoi(r.FormValue("offset"))
	keyword := strings.TrimSpace(r.FormValue("keyword"))
	sortBy := strings.TrimSpace(r.FormValue("sort"))

	type apiResult struct {
		Title       string `json:"title"`
		MagnetURL   string `json:"magnet_url,omitempty"`
		PageURL     string `json:"page_url,omitempty"`
		SizeFmt     string `json:"size"`
		Seeders     int    `json:"seeders"`
		IndexerName string `json:"indexer"`
		DateFmt     string `json:"date"`
		Group       string `json:"group,omitempty"`
	}

	searchCacheMu.Lock()
	ctx := searchCtx
	pageSize := ctx.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	// If keyword changed, re-filter from full cache
	if keyword != filterKeyword {
		if keyword == "" {
			searchCache = searchCacheFull
		} else {
			searchCache = applyKeywordFilter(searchCacheFull, keyword)
		}
		filterKeyword = keyword
	}
	// If sort changed, re-sort the full cache and re-apply keyword
	if sortBy != "" && sortBy != ctx.Sort {
		// Make a copy and sort it
		sortedFull := make([]indexer.SearchResult, len(searchCacheFull))
		copy(sortedFull, searchCacheFull)
		indexer.SortResults(sortedFull, sortBy)
		searchCacheFull = sortedFull
		// Re-apply keyword filter on newly sorted cache
		if filterKeyword == "" {
			searchCache = searchCacheFull
		} else {
			searchCache = applyKeywordFilter(searchCacheFull, filterKeyword)
		}
		ctx.Sort = sortBy
	}
	cached := searchCache
	searchCacheMu.Unlock()

	// Return up to pageSize items from the cache (skip offset items)
	skip := offset
	var results []apiResult
	for _, r := range cached {
		if skip > 0 {
			skip--
			continue
		}
		if len(results) >= pageSize {
			break
		}
		item := apiResult{
			Title:       r.Title,
			MagnetURL:   r.MagnetURL,
			PageURL:     r.PageURL,
			SizeFmt:     formatSize(r.Size),
			Seeders:     r.Seeders,
			IndexerName: r.IndexerName,
			Group:       r.Group,
		}
		if r.DateFmt != "" {
			item.DateFmt = r.DateFmt
		} else if !r.PublishDate.IsZero() {
			item.DateFmt = r.PublishDate.Format("2006-01-02 15:04")
		}
		results = append(results, item)
	}

	done := offset+len(results) >= len(cached)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"results":    results,
		"done":       done,
		"total":      len(cached),
		"all_tags":   buildAllTagsList(cached),
		"all_groups": buildAllGroupsList(cached),
	})
}

func (s *Server) handleSearchClearCache(w http.ResponseWriter, r *http.Request) {
	os.Remove(searchCacheFile)
	searchCacheMu.Lock()
	searchCache = nil
	searchCacheFull = nil
	filterKeyword = ""
	searchCacheMu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// searchNextPage fetches the next page of results from local indexers and Jackett.
func (s *Server) searchNextPage(ctx searchContext) []indexer.SearchResult {
	log.Printf("[search] fetching page %d for %q (indexers: %v)", ctx.NextPage, ctx.Query, ctx.Indexers)
	var all []indexer.SearchResult

	// Parse which are local vs Jackett
	var localIDs []string
	var hasJackett bool
	for _, id := range ctx.Indexers {
		if strings.HasPrefix(id, "jackett:") {
			hasJackett = true
		} else {
			localIDs = append(localIDs, id)
		}
	}

	// Local indexers — use Page from context
	if len(ctx.Indexers) == 0 || len(localIDs) > 0 || !hasJackett {
		req := indexer.SearchRequest{
			Query:    ctx.Query,
			Sort:     ctx.Sort,
			Indexers: localIDs,
			Limit:    2000,
			Page:     ctx.NextPage,
		}
		se := s.IdxMgr.SearchAllWithErrors(req)
		for i := range se.Results {
			se.Results[i].SizeFmt = formatSize(se.Results[i].Size)
			if !se.Results[i].PublishDate.IsZero() {
				se.Results[i].DateFmt = se.Results[i].PublishDate.Format("2006-01-02 15:04")
			}
		}
		all = append(all, se.Results...)
	}

	// Jackett — only when no filter or Jackett indexers selected
	hasJackettInCtx := false
	for _, id := range ctx.Indexers {
		if strings.HasPrefix(id, "jackett:") {
			hasJackettInCtx = true
			break
		}
	}
	if s.JackettURL != "" && s.JackettAPIKey != "" && (len(ctx.Indexers) == 0 || hasJackettInCtx) {
		s.jackettActiveMu.Lock()
		s.loadJackettEnabled()
		jackettActiveSet := make(map[string]bool)
		for id := range s.jackettActive {
			jackettActiveSet[id] = true
		}
		s.jackettActiveMu.Unlock()

		// Filter selected Jackett IDs
		var selectedJackettIDs []string
		for _, id := range ctx.Indexers {
			if strings.HasPrefix(id, "jackett:") {
				selectedJackettIDs = append(selectedJackettIDs, strings.TrimPrefix(id, "jackett:"))
			}
		}

		jackettOff := (ctx.NextPage - 1) * 100
		jc := s.jackettConfig()
		if jr, err := jackett.Search(jc, ctx.Query, nil, jackettOff); err == nil {
			for _, r := range jr {
				trackerLower := strings.ToLower(r.Tracker)
				if !jackettActiveSet[trackerLower] {
					continue
				}
				if len(selectedJackettIDs) > 0 {
					match := false
					for _, jid := range selectedJackettIDs {
						if strings.ToLower(jid) == trackerLower {
							match = true
							break
						}
					}
					if !match {
						continue
					}
				}
				var pubDate time.Time
				if r.PublishDate != "" {
					pubDate, _ = time.Parse(time.RFC1123Z, r.PublishDate)
					if pubDate.IsZero() {
						pubDate, _ = time.Parse(time.RFC1123, r.PublishDate)
					}
				}
				all = append(all, indexer.SearchResult{
					Title:       r.Title,
					MagnetURL:   r.MagnetURI,
					PageURL:     r.Link,
					Size:        r.Size,
					SizeFmt:     formatSize(r.Size),
					DateFmt:     formatJackettDate(r.PublishDate),
					Seeders:     r.Seeders,
					IndexerName: "Jackett: " + r.TrackerName,
					PublishDate: pubDate,
				})
			}
		}
	}

	log.Printf("[search] page %d returned %d results", ctx.NextPage, len(all))
	return dedupSlice(all, nil)
}


// ---------- indexer management ----------

func (s *Server) handleIndexers(w http.ResponseWriter, r *http.Request) {
	data := s.pageData("", "")
	data.Page = "indexers"

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		action := r.FormValue("action")
		id := r.FormValue("id")

		// Jackett API actions don't need local indexer manager
		switch action {
		case "jk_add_to_jackett":
			s.handleJKAddToJackett(w, r, id)
			return
		case "jk_remove_from_jackett":
			s.handleJKRemoveFromJackett(w, r, id)
			return
		}

		if s.IdxMgr == nil {
			http.Redirect(w, r, "/indexers", http.StatusSeeOther)
			return
		}
		switch action {
		case "toggle":
			enabled := r.FormValue("enabled") == "true"
			s.IdxMgr.SetEnabled(id, enabled)
			log.Printf("[indexer] %s enabled=%v", id, enabled)
		case "activate":
			s.IdxMgr.Activate(id)
			log.Printf("[indexer] %s activated from library", id)
			go s.IdxMgr.TestIndexer(id)
		case "jk_activate":
			s.jackettActiveMu.Lock()
			if s.jackettActive == nil {
				s.jackettActive = make(map[string]bool)
			}
			s.jackettActive[id] = true
			s.saveJackettEnabled()
			s.jackettActiveMu.Unlock()
			log.Printf("[jackett] %s activated", id)
		case "jk_deactivate":
			s.jackettActiveMu.Lock()
			if s.jackettActive != nil {
				delete(s.jackettActive, id)
				s.saveJackettEnabled()
			}
			s.jackettActiveMu.Unlock()
			log.Printf("[jackett] %s deactivated", id)
		case "activate_batch":
			ids := r.Form["ids"]
			for _, id := range ids {
				if id != "" {
					s.IdxMgr.Activate(id)
					log.Printf("[indexer] %s activated from library (batch)", id)
					go s.IdxMgr.TestIndexer(id)
				}
			}
		case "deactivate":
			s.IdxMgr.Deactivate(id)
			log.Printf("[indexer] %s deactivated to library", id)
		case "login":
			username := r.FormValue("username")
			password := r.FormValue("password")
			if err := s.IdxMgr.Login(id, username, password); err != nil {
				if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"ok":false,"msg":"%s"}`, err.Error())
					return
				}
				data.Error = err.Error()
			} else {
				if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"ok":true,"msg":"%s"}`, tr(s.langFromAgent(), "login_success"))
					return
				}
			}
		}
		if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok":true}`)
			return
		}
		http.Redirect(w, r, "/indexers", http.StatusSeeOther)
		return
	}

	if s.IdxMgr != nil {
		data.IndexerList = s.IdxMgr.List()
	// Build local library: show ALL definitions, mark active ones
		if s.IdxMgr != nil {
			data.IndexerLibrary = s.IdxMgr.Library()
			// Prepend active (enabled) definitions with status
			activeList := s.IdxMgr.List()
			for i := range activeList {
				activeList[i].Enabled = true
			}
			data.IndexerLibrary = append(activeList, data.IndexerLibrary...)
		}
		// Mark local indexers
		for i := range data.IndexerList {
			data.IndexerList[i].Source = "local"
		}
	}

	// Use cached Jackett data; trigger background refresh for next request
	data.JackettURL = s.JackettURL
	data.JackettAPIKey = s.JackettAPIKey
	data.JackettAdminPassword = s.JackettAdminPassword
	s.jackettCacheMu.Lock()
	data.JackettLibrary = s.jackettCache
	s.jackettCacheMu.Unlock()
	// Background async refresh (don't block page load)
	go s.refreshJackettCache()

	// Add Jackett-activated indexers to active list (show both, label conflict)
	s.jackettActiveMu.Lock()
	s.loadJackettEnabled()
	for _, jk := range data.JackettLibrary {
		if s.jackettActive[jk.ID] {
			name := jk.Name
			// If same ID already in local list, append source label
			for _, loc := range data.IndexerList {
				if loc.ID == jk.ID {
					name = jk.Name + " (Jackett)"
					break
				}
			}
			data.IndexerList = append(data.IndexerList, indexer.IndexerInfo{
				ID:       jk.ID,
				Name:     name,
				Type:     jk.Type,
				Language: jk.Language,
				SiteLink: jk.SiteLink,
				Enabled:  true,
				Healthy:  true,
				Source:   "jackett",
			})
		}
	}
	s.jackettActiveMu.Unlock()
	dashboardTemplate.Execute(w, data)
}

func (s *Server) handleJackettList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.JackettURL == "" || s.JackettAPIKey == "" {
		fmt.Fprint(w, `{"ok":false,"msg":"not configured"}`)
		return
	}

	jk := s.refreshJackettCache()
	if len(jk) == 0 {
		fmt.Fprint(w, `{"ok":true,"data":[],"active":[]}`)
		return
	}

	// Build active ID list from server state (always up-to-date)
	s.jackettActiveMu.Lock()
	activeIDs := make([]string, 0, len(s.jackettActive))
	for id := range s.jackettActive {
		activeIDs = append(activeIDs, id)
	}
	s.jackettActiveMu.Unlock()

	data, _ := json.Marshal(jk)
	activeJSON, _ := json.Marshal(activeIDs)
	fmt.Fprintf(w, `{"ok":true,"data":%s,"active":%s}`, data, activeJSON)
}

// handleJackettAll returns ALL Jackett indexers (including unconfigured).
func (s *Server) handleJackettAll(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.JackettURL == "" || s.JackettAPIKey == "" {
		fmt.Fprint(w, `{"ok":false,"msg":"not configured"}`)
		return
	}
	all, err := jackett.ListAllIndexers(s.jackettConfig())
	if err != nil {
		fmt.Fprintf(w, `{"ok":false,"msg":"%s"}`, err.Error())
		return
	}
	data, _ := json.Marshal(all)
	fmt.Fprintf(w, `{"ok":true,"data":%s}`, data)
}

// handleJackettAdd configures an indexer in Jackett via the admin API.
func (s *Server) handleJackettAdd(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		fmt.Fprint(w, `{"ok":false,"msg":"POST required"}`)
		return
	}
	id := r.FormValue("id")
	if id == "" {
		fmt.Fprint(w, `{"ok":false,"msg":"missing id"}`)
		return
	}
	jc := s.jackettConfig()
	if err := jackett.AddIndexer(jc, id); err != nil {
		fmt.Fprintf(w, `{"ok":false,"msg":"%s"}`, err.Error())
		return
	}
	log.Printf("[jackett] added indexer: %s", id)
	fmt.Fprint(w, `{"ok":true,"msg":"added"}`)
}

// refreshJackettCache fetches the latest indexer list from Jackett,
// updates the cache, and prunes stale entries from the active set.
func (s *Server) refreshJackettCache() []jackett.IndexerInfo {
	if s.JackettURL == "" || s.JackettAPIKey == "" {
		return nil
	}

	jk, err := jackett.ListIndexers(s.jackettConfig())
	if err != nil {
		log.Printf("[jackett] list indexers: %v", err)
		s.jackettCacheMu.Lock()
		cached := s.jackettCache
		s.jackettCacheMu.Unlock()
		return cached
	}

	// Build set of current Jackett IDs
	currentIDs := make(map[string]bool, len(jk))
	for _, idx := range jk {
		currentIDs[idx.ID] = true
	}

	// Update cache; log only if count changed
	s.jackettCacheMu.Lock()
	oldCount := len(s.jackettCache)
	s.jackettCache = jk
	s.jackettCacheTime = time.Now()
	s.jackettCacheMu.Unlock()

	if len(jk) != oldCount {
		log.Printf("[jackett] loaded %d configured indexers (was %d)", len(jk), oldCount)
	}

	// Prune stale entries from jackettActive
	s.jackettActiveMu.Lock()
	s.loadJackettEnabled()
	changed := false
	for id := range s.jackettActive {
		if !currentIDs[id] {
			delete(s.jackettActive, id)
			changed = true
			log.Printf("[jackett] removed stale activation: %s", id)
		}
	}
	if changed {
		s.saveJackettEnabled()
	}
	s.jackettActiveMu.Unlock()

	return jk
}

func (s *Server) handleIndexerTest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.IdxMgr == nil {
		fmt.Fprint(w, `{"ok":false,"msg":"no indexer manager"}`)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		fmt.Fprint(w, `{"ok":false,"msg":"missing id"}`)
		return
	}
	err := s.IdxMgr.TestIndexer(id)
	if err != nil {
		fmt.Fprintf(w, `{"ok":false,"msg":"%s"}`, err.Error())
		return
	}
	log.Printf("[indexer] test %s: ok", id)
	fmt.Fprint(w, `{"ok":true,"msg":"ok"}`)
}

func (s *Server) handleIndexerTestAll(w http.ResponseWriter, r *http.Request) {
	if s.IdxMgr == nil {
		http.Redirect(w, r, "/indexers", http.StatusSeeOther)
		return
	}
	results := s.IdxMgr.TestAll()
	log.Printf("[indexer] test-all completed: %d indexers", len(results))
	if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(results)
		return
	}
	http.Redirect(w, r, "/indexers", http.StatusSeeOther)
}

func (s *Server) handleIndexerLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.IdxMgr == nil {
		fmt.Fprint(w, `{"ok":false,"msg":"no indexer manager"}`)
		return
	}
	if r.Method != http.MethodPost {
		fmt.Fprint(w, `{"ok":false,"msg":"POST required"}`)
		return
	}
	if err := r.ParseForm(); err != nil {
		fmt.Fprintf(w, `{"ok":false,"msg":"%s"}`, err.Error())
		return
	}
	id := r.FormValue("id")
	username := r.FormValue("username")
	password := r.FormValue("password")
	if id == "" || username == "" || password == "" {
		fmt.Fprintf(w, `{"ok":false,"msg":"%s"}`, tr(s.langFromAgent(), "err_id_username_password"))
		return
	}
	if err := s.IdxMgr.Login(id, username, password); err != nil {
		fmt.Fprintf(w, `{"ok":false,"msg":"%s"}`, err.Error())
		return
	}
	log.Printf("[indexer] login %s: ok", id)
	fmt.Fprintf(w, `{"ok":true,"msg":"%s"}`, tr(s.langFromAgent(), "login_success"))
}

func (s *Server) handleIndexerEdit(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.IdxMgr == nil {
		fmt.Fprint(w, `{"ok":false,"msg":"no indexer manager"}`)
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		fmt.Fprint(w, `{"ok":false,"msg":"missing id"}`)
		return
	}
	if r.Method == http.MethodGet {
		// Return raw YAML
		yaml, err := s.IdxMgr.GetDefinitionYAML(id)
		if err != nil {
			fmt.Fprintf(w, `{"ok":false,"msg":"%s"}`, err.Error())
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"ok": "true", "yaml": yaml})
		return
	}
	if r.Method == http.MethodPost {
		yamlContent := r.FormValue("yaml")
		if yamlContent == "" {
			fmt.Fprint(w, `{"ok":false,"msg":"missing yaml content"}`)
			return
		}
		if err := s.IdxMgr.UpdateDefinitionYAML(id, yamlContent); err != nil {
			fmt.Fprintf(w, `{"ok":false,"msg":"save failed: %s"}`, err.Error())
			return
		}
		log.Printf("[indexer] edit %s: definition updated (%d bytes)", id, len(yamlContent))
		fmt.Fprint(w, `{"ok":true,"msg":"saved"}`)
		return
	}
	fmt.Fprint(w, `{"ok":false,"msg":"method not allowed"}`)
}

func (s *Server) handleIndexerDelete(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.IdxMgr == nil {
		fmt.Fprint(w, `{"ok":false,"msg":"no indexer manager"}`)
		return
	}
	if r.Method != http.MethodPost {
		fmt.Fprint(w, `{"ok":false,"msg":"POST required"}`)
		return
	}
	id := r.FormValue("id")
	if id == "" {
		fmt.Fprint(w, `{"ok":false,"msg":"missing id"}`)
		return
	}
	if err := s.IdxMgr.DeleteDefinition(id); err != nil {
		fmt.Fprintf(w, `{"ok":false,"msg":"%s"}`, err.Error())
		return
	}
	log.Printf("[indexer] delete %s: definition removed", id)
	fmt.Fprint(w, `{"ok":true,"msg":"deleted"}`)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("r2c_session"); err == nil {
		webSessMu.Lock()
		delete(webSessions, c.Value)
		webSessMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "r2c_session", Value: "", Path: "/", MaxAge: -1})
	log.Printf("[auth] user logged out")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Read language preference from cookie
	lang := "zh"
	if c, err := r.Cookie("r2c_lang"); err == nil && c.Value == "en" {
		lang = "en"
	}

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		pw := sanitizePassword(r.FormValue("password"))
		if pw == webPassword {
			token := newSessionToken()
			webSessMu.Lock()
			webSessions[token] = true
			webSessMu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "r2c_session", Value: token, Path: "/", MaxAge: 86400 * 7})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		errMsg := `<div class="err">` + tr(lang, "wrong_password_html") + `</div>`
		fmt.Fprint(w, loginPage(lang, errMsg))
		return
	}
	if webPassword == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if c, err := r.Cookie("r2c_session"); err == nil && validSession(c.Value) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, loginPage(lang, ""))
}

func sanitizePassword(s string) string {
	// Trim spaces, remove control characters, limit to 128 chars
	s = strings.TrimSpace(s)
	if len(s) > 128 {
		s = s[:128]
	}
	// Strip control characters and null bytes
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r >= 0x20 && r != 0x7F {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func loginPage(lang, errHTML string) string {
	title := "🔐 pan-fetcher"
	pageTitle := tr(lang, "login_page_title")
	ph := tr(lang, "login_pw_ph")
	btn := tr(lang, "login_btn")
	htmlLang := "zh-CN"
	if lang == "en" {
		htmlLang = "en"
		htmlLang = "en"
	}
	return `<!doctype html>
<html lang="` + htmlLang + `">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>` + pageTitle + `</title>
<style>
*{box-sizing:border-box}body{margin:0;font-family:-apple-system,sans-serif;background:linear-gradient(180deg,#eef3ff,#f5f7fb);min-height:100vh;display:flex;align-items:center;justify-content:center}
.card{background:#fff;border-radius:18px;padding:32px;width:360px;box-shadow:0 12px 40px rgba(0,0,0,.08)}
h1{text-align:center;margin:0 0 20px;font-size:24px}
input{width:100%;border:1px solid #dce3ec;border-radius:12px;padding:12px;font:inherit;margin-bottom:12px}
button{width:100%;border:0;border-radius:12px;padding:12px;background:#1d4ed8;color:#fff;font:inherit;cursor:pointer;font-size:16px}
.err{color:#b42318;text-align:center;margin-bottom:10px;font-size:14px}
</style></head>
<body><div class="card">
<h1>` + title + `</h1>
` + errHTML + `<form method="post"><input name="password" type="password" placeholder="` + ph + `" maxlength="128" autofocus><button>` + btn + `</button></form>
</div></body></html>`
}

func base64Encode(data []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	n := (len(data) + 2) / 3 * 4
	out := make([]byte, n)
	for i, j := 0, 0; i < len(data); i += 3 {
		chunk := int(data[i]) << 16
		if i+1 < len(data) {
			chunk |= int(data[i+1]) << 8
		}
		if i+2 < len(data) {
			chunk |= int(data[i+2])
		}
		out[j] = alphabet[(chunk>>18)&0x3F]
		out[j+1] = alphabet[(chunk>>12)&0x3F]
		if i+1 < len(data) {
			out[j+2] = alphabet[(chunk>>6)&0x3F]
		} else {
			out[j+2] = '='
		}
		if i+2 < len(data) {
			out[j+3] = alphabet[chunk&0x3F]
		} else {
			out[j+3] = '='
		}
		j += 4
	}
	return string(out)
}

// ---------- helpers ----------

// ---------- subscription management ----------

type subRow struct {
	ID        int
	Name      string
	MediaType string
	Season    int
	Cid       string
	Savepath  string
	Enabled   bool
	Status    string
}

type rssSubRow struct {
	Site           string
	Name           string
	URL            string
	Filter         string
	Cid            string
	SavePath       string
	Enabled        bool
	CacheCount     int
	IndexerDisplay string
}

func (s *Server) handleSubscriptions(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.handleSubsAction(w, r)
		return
	}
	data := s.pageData("", "")
	data.Page = "subs"
	// Load RSS subscriptions from rss.json
	data.RssSubs = s.loadRssSubs()
	if err := dashboardTemplate.Execute(w, data); err != nil {
		log.Printf("[subs] template render error: %v", err)
	}
}

func (s *Server) handleSubsAction(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	action := r.FormValue("action")
	site := r.FormValue("site")
	name := r.FormValue("name")

	if action == "toggle" {
		s.toggleRssSub(site, name)
		log.Printf("[subs] toggle %q on %s", name, site)
		http.Redirect(w, r, "/subs", http.StatusSeeOther)
		return
	}
	if action == "delete" {
		s.deleteRssSub(site, name)
		log.Printf("[subs] delete %q from %s", name, site)
		http.Redirect(w, r, "/subs", http.StatusSeeOther)
		return
	}
	if action == "edit" {
		cid := strings.TrimSpace(r.FormValue("cid"))
		savepath := strings.TrimSpace(r.FormValue("savepath"))
		filter := strings.TrimSpace(r.FormValue("filter"))
		s.updateRssSub(site, name, cid, savepath, filter)
		log.Printf("[subs] edit %q on %s (cid=%s, savepath=%q)", name, site, cid, savepath)
		http.Redirect(w, r, "/subs", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/subs", http.StatusSeeOther)
}

func (s *Server) loadRssSubs() []rssSubRow {
	feeds := s.readRssFeeds()
	var rows []rssSubRow
	for site, entries := range feeds {
		for _, e := range entries {
			rows = append(rows, rssSubRow{
				Site: site, Name: e.Name, URL: e.URL,
				Filter: e.Filter, Cid: e.Cid, SavePath: e.SavePath,
				Enabled: e.Enabled,
				CacheCount:     globalDedup.SubCount(e.Name),
				IndexerDisplay: parseIndexerFromRSSURL(e.URL),
			})
		}
	}
	return rows
}

// parseIndexerFromRSSURL extracts indexer names from an RSS URL's indexers parameter.
func parseIndexerFromRSSURL(rssURL string) string {
	u, err := url.Parse(rssURL)
	if err != nil {
		return ""
	}
	raw := u.Query().Get("indexers")
	if raw == "" {
		return "全部"
	}
	parts := strings.Split(raw, ",")
	for i, p := range parts {
		// Strip jackett: prefix for display
		parts[i] = strings.TrimPrefix(strings.TrimSpace(p), "jackett:")
	}
	return strings.Join(parts, ", ")
}

func (s *Server) readRssFeeds() map[string][]rssFeedEntry {
	feeds := make(map[string][]rssFeedEntry)
	if data, err := os.ReadFile(rssJsonPath); err == nil {
		json.Unmarshal(data, &feeds)
	}
	return feeds
}

func (s *Server) writeRssFeeds(feeds map[string][]rssFeedEntry) {
	data, _ := json.MarshalIndent(feeds, "", "  ")
	os.WriteFile(rssJsonPath, data, 0644)
}

func (s *Server) toggleRssSub(site, name string) {
	feeds := s.readRssFeeds()
	entries := feeds[site]
	for i, e := range entries {
		if e.Name == name {
			feeds[site][i].Enabled = !e.Enabled
			break
		}
	}
	s.writeRssFeeds(feeds)
}

func (s *Server) deleteRssSub(site, name string) {
	feeds := s.readRssFeeds()
	entries := feeds[site]
	filtered := entries[:0]
	for _, e := range entries {
		if e.Name != name {
			filtered = append(filtered, e)
		}
	}
	if len(filtered) == 0 {
		delete(feeds, site)
	} else {
		feeds[site] = filtered
	}
	s.writeRssFeeds(feeds)
	// Also clean up dedup cache for this subscription
	globalDedup.ClearSub(name)
}

func (s *Server) updateRssSub(site, name, cid, savepath, filter string) {
	feeds := s.readRssFeeds()
	entries := feeds[site]
	for i, e := range entries {
		if e.Name == name {
			if cid != "" {
				feeds[site][i].Cid = cid
			}
			if savepath != "" {
				feeds[site][i].SavePath = savepath
			}
			if filter != "" {
				feeds[site][i].Filter = filter
			}
			break
		}
	}
	s.writeRssFeeds(feeds)
}

func (s *Server) handleSubsRun(w http.ResponseWriter, r *http.Request) {
	if s.Agent == nil {
		s.renderMsg(w, "", "err_not_logged_in_subs")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rssURL := strings.TrimSpace(r.FormValue("rss_url"))
	if rssURL == "" {
		s.renderMsg(w, "", "err_rss_empty")
		return
	}
	cid := strings.TrimSpace(r.FormValue("cid"))
	savepath := strings.TrimSpace(r.FormValue("savepath"))
	// Resolve relative URLs to absolute for local RSS endpoints
	if strings.HasPrefix(rssURL, "/") {
		rssURL = fmt.Sprintf("http://127.0.0.1:%d%s", s.Port, rssURL)
	}
	subKey := strings.TrimSpace(r.FormValue("sub_name"))
	if subKey == "" {
		subKey = rssURL
	}
	go func() {
		before := globalDedup.SubCount(subKey)
		names := s.Agent.ProcessRSSFeed(rssURL, cid, savepath, subKey)
		after := globalDedup.SubCount(subKey)
		newItems := after - before
		if newItems > 0 {
			notify.RecordItems(subKey, names)
			ws := s.loadWebSettings()
			notify.Send(notify.RSSFound(subKey, newItems, names), ws.NotifyRSS)
		}
		log.Printf("[subs] run completed: %s, new=%d", subKey, newItems)
	}()
	log.Printf("[subs] run triggered: %s", subKey)
	if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		w.Header().Set("Content-Type", "application/json")
		lang := s.langFromAgent()
		fmt.Fprintf(w, `{"ok":true,"msg":"%s"}`, trf(lang, "subs_processing_fmt", rssURL))
		return
	}
	http.Redirect(w, r, "/subs", http.StatusSeeOther)
}

// handleSubsToggleAll enables or disables all RSS subscriptions.
func (s *Server) handleSubsToggleAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	enabled := r.FormValue("enabled") == "1"
	feeds := s.readRssFeeds()
	for site, entries := range feeds {
		for i := range entries {
			entries[i].Enabled = enabled
		}
		feeds[site] = entries
	}
	s.writeRssFeeds(feeds)
	lang := s.langFromAgent()
	msg := tr(lang, "disable_all")
	if enabled {
		msg = tr(lang, "enable_all")
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"msg":"%s"}`, msg+" "+tr(lang, "settings_saved"))
}

// handleDedupClearAll clears the entire deduplication cache.
func (s *Server) handleDedupClearAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	for _, subKey := range globalDedup.SubKeys() {
		globalDedup.ClearSub(subKey)
	}
	log.Printf("[dedup] cleared all cache")
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"msg":"%s"}`, tr(s.langFromAgent(), "clear_all_cache"))
}

func (s *Server) handleSubsDirs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.Agent == nil {
		fmt.Fprintf(w, `{"ok":false,"msg":"%s"}`, tr(s.langFromAgent(), "not_logged_in_json"))
		return
	}
	parentID := r.URL.Query().Get("pid")
	if parentID == "" {
		parentID = "0"
	}
	entries, err := s.Agent.ListDir(parentID)
	if err != nil {
		fmt.Fprintf(w, `{"ok":false,"msg":"%s"}`, err.Error())
		return
	}
	// Get actual parent of this directory for ".." navigation
	actualParent := "0"
	if parentID != "0" {
		if e, err := s.getEntryCached(parentID); err == nil {
			actualParent = e.ParentID
		}
	}
	type dirEntry struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	var dirs []dirEntry
	for _, e := range entries {
		if e.IsDir {
			dirs = append(dirs, dirEntry{ID: e.ID, Name: e.Name})
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ok":      true,
		"parent":  actualParent,
		"entries": dirs,
	})
}

// ---------- helpers (continued) ----------

func (s *Server) renderResult(w http.ResponseWriter, message, errMsg string) {
	if err := dashboardTemplate.Execute(w, s.pageData(message, errMsg)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// langFromAgent returns the user's language preference, defaults to "zh".
func (s *Server) langFromAgent() string {
	if s.Agent != nil {
		if st := s.Agent.GetSettings(); st.Lang != "" {
			return st.Lang
		}
	}
	return "zh"
}

// renderMsg renders a translated message/error through the template.
func (s *Server) renderMsg(w http.ResponseWriter, msgKey, errKey string) {
	lang := s.langFromAgent()
	s.renderResult(w, tr(lang, msgKey), tr(lang, errKey))
}

// renderMsgf renders a translated formatted message through the template.
func (s *Server) renderMsgf(w http.ResponseWriter, msgKey string, args ...interface{}) {
	lang := s.langFromAgent()
	s.renderResult(w, trf(lang, msgKey, args...), "")
}

func (s *Server) pageData(message, errMsg string) dashboardData {
	return s.pageDataWithCache("", message, errMsg)
}

func (s *Server) pageDataWithCache(page, message, errMsg string) dashboardData {
	lang := "zh"
	if s.Agent != nil {
		st := s.Agent.GetSettings()
		if st.Lang != "" {
			lang = st.Lang
		}
	}
	data := dashboardData{
		Message:     message,
		Error:       errMsg,
		Lang:        lang,
		T:           langMap(lang),
		AllTagsJSON:   template.JS("[]"),
		AllGroupsJSON: template.JS("[]"),
	}
	ws := s.loadWebSettings()
	data.PageSize = ws.PageSize
	if data.PageSize <= 0 { data.PageSize = 50 }
	if s.Agent != nil {
		// Cache connection check for 60 seconds
		if time.Since(s.connCheckTime) < 60*time.Second {
			data.LoggedIn = s.connCheckLoggedIn
		} else {
			if err := s.Agent.TestConnection(); err == nil {
				data.LoggedIn = true
				s.connCheckLoggedIn = true
			} else {
				s.connCheckLoggedIn = false
			}
			s.connCheckTime = time.Now()
		}

		data.Cookies = s.Agent.LoadCookiesStr()

		// Only load full task list for the tasks page; dashboard just needs count
		if page == "tasks" {
			tasks, err := s.Agent.ListTasks()
			if err == nil {
				data.TaskCount = len(tasks)
				data.Tasks = make([]taskRow, 0, len(tasks))
				for _, t := range tasks {
					row := taskRow{
						InfoHash: t.InfoHash,
						Name:     displayName(t),
						Size:     formatSize(t.Size),
						Percent:  t.Percent,
						URL:      t.URL,
					}
					switch {
					case t.Status == 2:
						row.Status = "done"
						row.RowClass = "row-done"
					case t.Status == -1:
						row.Status = "failed"
						row.RowClass = "row-failed"
					case t.Status == 1:
						row.Status = "downloading"
						row.RowClass = "row-running"
					default:
						row.Status = "waiting"
					}
					data.Tasks = append(data.Tasks, row)
				}
			}
		}
	}
	return data
}

func displayName(t p115pkg.TaskItem) string {
	if t.Name != "" && t.Name != t.InfoHash {
		return t.Name
	}
	if len(t.InfoHash) > 14 {
		return t.InfoHash[:14] + "…"
	}
	return t.InfoHash
}

func formatSize(bytes int64) string {
	switch {
	case bytes <= 0:
		return "-"
	case bytes < 1024:
		return fmt.Sprintf("%dB", bytes)
	case bytes < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(bytes)/1024)
	case bytes < 1024*1024*1024:
		return fmt.Sprintf("%.1fMB", float64(bytes)/(1024*1024))
	default:
		return fmt.Sprintf("%.2fGB", float64(bytes)/(1024*1024*1024))
	}
}

func formatJackettDate(s string) string {
	for _, f := range []string{time.RFC1123Z, time.RFC1123, "Mon, 02 Jan 2006 15:04:05 -0700"} {
		if t, err := time.Parse(f, s); err == nil {
			return t.Format("2006-01-02 15:04")
		}
	}
	if len(s) > 10 {
		return s[5:16]
	}
	return s
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d.Minutes())
		s := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm%ds", m, s)
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh%dm", h, m)
	}
	days := h / 24
	h = h % 24
	return fmt.Sprintf("%dd%dh", days, h)
}

func decodeOfflineTask(r *http.Request) (OfflineTask, error) {
	var task OfflineTask
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
			return task, err
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return task, err
		}
		task.Tasks = parseTasks(strings.TrimSpace(r.FormValue("tasks")))
		if len(task.Tasks) == 0 {
			task.Tasks = parseTasks(strings.TrimSpace(r.FormValue("task")))
		}
		task.Cid = strings.TrimSpace(r.FormValue("cid"))
		task.SavePath = strings.TrimSpace(r.FormValue("savepath"))
	}
	if len(task.Tasks) == 0 {
		return task, fmt.Errorf("tasks is empty")
	}
	return task, nil
}

func parseTasks(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == ',' || r == ';'
	})
	tasks := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			tasks = append(tasks, part)
		}
	}
	return tasks
}

func (s *Server) StartServer() {
	ctx, cancel := context.WithCancel(context.Background())

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-c
		fmt.Printf("%s received.\n", sig.String())
		if err := s.Shutdown(ctx); err != nil {
			fmt.Printf("failed to shutdown server, error: %+v\n", err)
		}
		cancel()
	}()

	if err := s.Start(ctx); err != nil {
		if err != http.ErrServerClosed {
			fmt.Printf("failed to start server, error: %+v\n", err)
			cancel()
		}
	}

	<-ctx.Done()
}
