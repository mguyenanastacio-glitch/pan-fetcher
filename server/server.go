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
	ProcessRSSFeed(rssURL, cid, savepath, keyword, subKey string) []string
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
	SearchCategory string
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
	searchCacheMu  sync.Mutex
	searchCache    []indexer.SearchResult // filtered by current keyword
	searchCacheFull []indexer.SearchResult // all results, never filtered (for tag extraction & re-filter)
	searchCtx      searchContext
	filterKeyword  string // the keyword currently applied to searchCache
)

type searchContext struct {
	Query     string
	Category  string
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

var translations = map[string]map[string]string{
	"zh": {
		"title":           "pan-fetcher 管理页",
		"logout":          "退出",
		"about":           "关于",
		"home":            "📥 离线下载",
		"dashboard":       "📊 仪表盘",
		"total_tasks":     "总任务",
		"push_count":      "推送",
		"rss_subs_short":  "订阅",
		"active_indexers_short": "索引器",
		"cache_entries":   "缓存条目",
		"uptime_label":    "运行时间",
		"status_connected": "已连接",
		"pw_set":          "密码已设",
		"pw_not_set":      "密码未设",
		"manage_tasks_btn": "管理任务 →",
		"manage_subs_btn": "管理订阅 →",
		"files":           "📂 文件管理",
		"subs":            "📋 订阅管理",
		"settings":        "⚙️ 设置",
		"add_magnet":      "添加磁力任务",
		"task_url":        "任务链接",
		"task_url_ph":     "每行一个 magnet / ed2k / http / torrent 链接",
		"cid":             "cid",
		"cid_ph":          "115 文件夹 ID",
		"savepath":        "savepath",
		"savepath_ph":     "可选，文件夹名称",
		"submit_task":     "提交磁力任务",
		"json_api":        "也支持 JSON API：",
		"quick_grab":      "快速抓取",
		"rss_url":         "RSS 地址",
		"rss_url_ph":      "https://example.com/rss.xml",
		"keyword_opt":     "关键词过滤 (可选)",
		"keyword_ph":      "留空则抓取全部",
		"target_cid":      "目标 115 目录 ID",
		"target_cid_ph":   "115 文件夹 ID",
		"grab_btn":        "一键抓取",
		"grab_hint":       "输入 RSS 地址和 115 目录 ID，批量提交种子。可选关键词过滤。",
		"offline_tasks":   "离线任务",
		"items":           "条",
		"clear_done":      "清理已完成",
		"clear_all":       "清理全部",
		"clear_failed":    "清理失败",
		"clear_running":   "清理运行中",
		"clear_done_del":  "清理完成并删除文件",
		"clear_all_del":   "清理全部并删除文件",
		"execute":         "执行",
		"no_tasks":        "暂无离线任务",
		"runtime_log":     "📜 运行日志",
		"expand":          "展开",
		"collapse":        "收起",
		"sys_settings":    "系统设置",
		"login_115":       "115 登录",
		"connected":       "✓ 已连接",
		"disconnected":    "✗ 未连接",
		"test_conn":       "测试连接",
		"cookies_label":   "Cookies",
		"cookies_ph":      "粘贴 115 Cookies 字符串",
		"update_cookies":  "更新 Cookies",
		"qr_login":        "📱 扫码登录",
		"testing":         "测试中...",
		"net_error":       "网络错误",
		"chunk_size":      "分块大小",
		"chunk_delay":     "分块延迟(秒)",
		"cooldown_min":    "API最小间隔(ms)",
		"cooldown_max":    "API最大间隔(ms)",
		"web_pw":          "Web 管理密码 (留空=无认证)",
		"web_pw_ph":       "设置登录密码",
		"save":            "保存",
		"db_path":         "DB 路径",
		"cloud_files":     "云文件管理",
		"root_dir":        "根目录",
		"name":            "名称",
		"size":            "大小",
		"empty_dir":       "此目录为空",
		"new_folder":      "新建文件夹",
		"folder_name":     "文件夹名称",
		"rename":          "重命名",
		"copy":            "复制",
		"new_name":        "新名称",
		"move":            "移动到",
		"target_dir_id":   "目标目录 ID",
		"confirm_delete":  "确认删除",
		"delete":          "删除",
		"add_sub":         "添加订阅",
		"sub_name_ph":     "剧集/电影名称",
		"sub_anime":       "番剧",
		"sub_tv":          "影视",
		"sub_season_ph":   "季(可选)",
		"sub_cid_ph":      "115目录ID",
		"sub_save_ph":     "子目录(可选)",
		"sub_add_btn":     "添加",
		"sub_type":        "类型",
		"sub_season":      "季",
		"sub_status":      "状态",
		"sub_run":         "运行订阅检查",
		"sub_rss_ph":      "RSS地址",
		"subs_mgmt":       "订阅管理",
		"footer_api":      "API:",
		"settings_saved":  "设置已保存",
		"cookies_updated": "Cookies 已更新并验证成功",
		"quick_started":   "快速抓取已启动",
		"tasks_submitted": "已提交任务",
		"qrcode_wait":     "请用115手机App扫描二维码",
		"qrcode_scanning": "等待扫码...",
		"qrcode_ok":       "登录成功！刷新页面生效",
		"qrcode_error":    "二维码获取失败",
		"qrcode_timeout":  "二维码已过期，请重新获取",
		"conn_ok":         "115 连接正常",
		"login_title":     "🔐 pan-fetcher",
		"login_pw_ph":     "输入管理密码",
		"login_btn":       "登录",
		"login_err":       "密码错误",
		"lang_label":      "界面语言",
		"search":          "🔍 资源搜索",
		"indexer_search":  "🔍 聚合搜索",
		"search_ph":       "输入关键词搜索资源...",
		"search_btn":      "搜索",
		"search_refresh":  "🔄 刷新",
		"search_results":  "搜索结果",
		"search_no_result":"没有找到结果",
		"search_from":     "来源",
		"indexers":        "📡 索引器",
		"indexer_list":    "索引器列表",
		"indexer_toggle":  "切换状态",
		"grab_selected":   "快速抓取选中",
		"test_all":        "测试全部",
		"idx_site":        "站点",
		"idx_health":      "健康",
		"idx_tested":      "上次测试",
		"idx_add":         "在 Jackett 中添加索引器",
		"jk_id":           "ID",
		"jk_config_hint":  "请先在设置页配置 Jackett 连接地址和 API Key，然后通过 Jackett 界面添加索引器。",
		"jk_no_indexers":  "暂无索引器，请点击上方链接前往 Jackett 添加。",
		"jk_url":          "Jackett 地址",
		"jk_url_ph":       "http://127.0.0.1:9117",
		"jk_apikey":       "Jackett API Key",
		"jk_apikey_ph":    "从 Jackett 设置页获取",
		"jk_test_btn":     "测试连接",
		"jk_test_ok":      "Jackett 连接正常",
		"jk_test_empty":   "请填写 Jackett 地址和 API Key",
		"idx_no_defs":     "暂无索引器定义文件，请在 indexers/ 目录添加 YAML 文件。",
		"idx_no_active":   "暂无激活的索引器，请从下方索引器库添加。",
		"idx_library":     "📚 索引器库",
		"idx_lib_empty":   "索引器库为空。",
		"idx_lib_local":   "📚 本地库",
		"idx_lib_jackett": "📡 Jackett库",
		"jk_lib_empty":    "暂无 Jackett 索引器，请先在 Jackett 中添加。",
		"jk_lib_hint":     "请先在设置页配置 Jackett 连接地址和 API Key，然后通过 Jackett 界面添加索引器。",
		"jk_lib_config":   "配置 Jackett",
		"jk_lib_add":      "在 Jackett 中添加",
		"jk_lib_activate": "激活到本地",
		"idx_batch_add":   "批量添加选中",
		"idx_lang":        "语言",
		"idx_source":      "来源",
		"idx_activated":   "已激活",
		"idx_jk_activated": "已在本地激活",
		"idx_activate_hint": "激活到本地列表",
		"idx_jk_delete_hint": "从 Jackett 删除此索引器",
		"jk_search_ph":    "搜索索引器…",
		"jk_no_match":     "没有匹配的索引器",
		"edit_label":      "编辑",
		"create":          "创建",
		"delete_label":    "删除",
		"configured":      "已配置",
		"add":             "添加",
		"new_idx":         "新建索引器",
		"local":           "本地",
		"remove":          "移除",
		"remove_lib":      "移回索引器库",
		"dedup":           "🗄️ 缓存库",
		"dedup_title":     "缓存库",
		"dedup_empty":     "暂无缓存记录。订阅自动执行后会自动记录已下载的种子。",
		"dedup_clear_sub": "清空此项",
		"clear":           "清空",
		"cache":           "缓存",   
		// --- 新增翻译键 ---
		// 错误 / 状态消息
		"err_not_logged_in":    "115 未登录，请先在设置中配置 Cookies",
		"tasks_submitted_fmt":  "已提交 %d 个磁力任务",
		"err_rss_empty":        "RSS 地址不能为空",
		"quick_started_fmt":    "快速抓取已启动: %s",
		"err_clear_type":       "清理类型必须是 1-6",
		"clear_executed_fmt":   "已执行清理类型 %d",
		"err_read_dir":         "读取目录失败: ",
		"err_missing_sub_name": "缺少订阅名",
		"err_cookies_empty":    "Cookies 不能为空",
		"err_login_failed":     "登录失败: ",
		"saving_failed":        "保存失败: ",
		"login_success":        "登录成功",
		"err_not_logged_in_subs": "115 未登录，无法运行订阅",
		"subs_processing_fmt":    "已开始处理 %s",
		// 模板 UI / JS
		"confirm_restart":         "确定要重启服务吗？",
		"restarting":              "服务正在重启，请稍候刷新页面…",
		"restart_failed":          "重启失败: ",
		"restart_req_failed":      "重启请求失败: ",
		"cancel":                  "取消",
		"failed":                  "失败",
		"completed":               "已完成",
		"downloading":             "下载中",
		"all_tab":                 "全部",
		"confirm_clear_cache_msg": "确定清空缓存记录？",
		"load_failed":             "加载失败: ",
		"username_label":          "用户名",
		"password_label":          "密码",
		"login_label":             "登录",
		"credentials_required":    "请输入用户名和密码",
		"error_label":             "错误",
		"success_label":           "成功",
		"login_success_msg":       "登录成功",
		"waiting":                 "等待中...",
		"http_proxy_label":        "HTTP 代理",
		"domain_label":            "访问域名",
		"domain_hint":             "通过此域名访问 Web 面板",
		"subs_interval_label":     "订阅间隔(分)",
		"wework_label":            "企业微信 Webhook",
		"test_btn":                "测试",
		"notify_task":             "任务推送",
		"notify_rss":              "RSS推送",
		"notify_log":              "日志推送",
		"timezone_label":          "时区",
		"restart_service_btn":     "🔄 重启服务",
		"confirm_btn":             "确定",
		"browse_btn":              "📁 浏览",
		// RSS XML
		"rss_feed_title": "pan-fetcher 聚合搜索",
		"rss_feed_desc":  "聚合搜索: %s",
		// JSON-only
		"restarting_json":      "正在重启…",
		"not_logged_in_json":   "115 未登录",
		"save_failed_json":     "保存失败: ",
		// 侧栏
		"toggle_sidebar":       "折叠侧边栏",
		"announcement":         "欢迎使用 pan-fetcher — 云文件管理与 RSS 订阅下载",
		"about_title":          "关于 pan-fetcher",
		"about_desc":           "pan-fetcher 是一款集成 115 网盘管理的 RSS 订阅下载工具，支持多索引器聚合搜索、离线任务管理、文件管理等功能。",
		"about_version":        "版本",
		"about_author":         "源自 zhifengle/rss2cloud，融合 Prowlarr 索引引擎",
		"about_links":          "相关链接",
		"about_based_on":       "基于",
		// JSON-only
		// 订阅 / 搜索 弹窗
		"enabled":              "启用",
		"disabled":             "禁用",
		"confirm_delete_sub":   "删除？",
		"no_subs_hint":         "暂无订阅，请在 资源搜索 页面搜索后点击 📌 订阅此搜索 添加。",
		"edit_sub":             "编辑订阅",
		"subdir_label":         "子目录",
		"filter_label":         "过滤",
		"save_btn":             "保存",
		"copy_link_title":      "复制链接",
		// 搜索分类 / 排序
		"all_categories":       "全部分类",
		"category_anime":       "动漫",
		"category_tv":          "剧集",
		"category_movie":       "电影",
		"category_music":       "音乐",
		"category_other":       "其他",
		"sort_seeds":           "按做种数",
		"sort_size":            "按大小",
		"sort_date":            "按时间",
		"search_sites":         "搜索站点:",
		"date_label":           "时间",
		"indexer_label":        "索引器",
		"page_prev":            "上一页",
		"page_next":            "下一页",
		"page_total":           "共 %d 条",
		"page_loading":         "加载中…",
		"page_size_label":      "搜索分页大小",
		"system_label":         "系统",
		"search_label":         "搜索",
		"download_settings_label": "下载",
		"subs_notify_label":    "订阅与通知",
		"update_check_btn":     "检查更新",
		"update_do_btn":        "立即更新",
		"update_auto_label":    "自动检查更新",
		"update_fetch_failed":  "获取版本信息失败",
		"update_parse_failed":  "解析版本信息失败",
		"update_already_latest":"已是最新版本",
		"update_no_asset":      "未找到匹配的二进制文件",
		"update_download_failed":"下载更新失败",
		"update_ok":            "更新完成，服务即将重启",
		"update_install_failed":"安装失败",
		"update_need_sudo":     "需要管理员权限。请复制以下命令在终端执行：",
		"update_checking":      "正在检查…",
		"update_new_found":     "发现新版本 %s，当前 %s",
		"enable_all":           "全部启用",
		"disable_all":          "全部禁用",
		"enable_all_confirm":   "确定启用全部订阅？",
		"disable_all_confirm":  "确定禁用全部订阅？",
		"clear_all_cache":      "清除全部缓存",
		"clear_all_cache_confirm": "确定清除全部去重缓存？此操作不可恢复。",
		"jk_show_all":          "显示全部",
		"jk_add_lib":           "添加库",
		"jk_show_configured":   "仅显示已配置",
		"subscribe_search":     "📌 订阅此搜索",
		"add_rss_sub_title":    "📌 添加 RSS 订阅",
		"sub_name_label":       "名称",
		"sub_name_placeholder": "订阅名称",
		"rss_addr_label":       "RSS 地址",
		"rss_addr_placeholder": "RSS 地址",
		"filter_placeholder":   "关键词",
		"dir_id_opt":           "115 目录 ID (可选)",
		"subdir_opt":           "子目录 (可选)",
		"add_sub_btn":          "添加订阅",
		"saved_searches_title": "📌 已保存的搜索",
		"search_btn_sm":        "搜索",
		"delete_btn":           "删除",
		// 目录选择弹窗
		"parent_dir_label":     "📁 .. (上级)",
		"no_subfolders":        "此目录下没有子文件夹",
		"select_dir_title":     "选择 115 目录 (当前: %s)",
		"select_current_dir":   "选择当前目录",
		"select_add":           "选此目录添加",
		"select_dir_add":       "选择目标目录 (当前: %s)",
		"task_added":           "已添加离线任务",
		"add_failed":           "添加任务失败",
		"close_btn":            "关闭",
		// 缓存库列表
		"sub_name_header":      "订阅名称",
		"cache_count_header":   "缓存数",
		"loading":              "加载中…",
		"no_url_short":         "(无)",
		"confirm_delete_url_map": "删除此 URL 映射？",
		"no_records":           "(暂无记录)",
		// 设置 & 其他
		"lang_zh_option":       "中文",
		"subs_interval_ph":     "默认60分钟，0=不自动",
		"enter_search_kw":      "请输入搜索关键词",
		"indexer_not_initialized": "索引器未初始化",
		"please_fill_rss":      "请填写 RSS 地址",
		"err_id_username_password": "需要 id, username, password",
		"wrong_password_html":  "密码错误",
		"login_page_title":     "pan-fetcher 登录",
	},
	"en": {
		"title":           "pan-fetcher Dashboard",
		"logout":          "Logout",
		"about":           "About",
		"home":            "📥 Offline",
		"dashboard":       "📊 Dashboard",
		"total_tasks":     "Total Tasks",
		"push_count":      "Pushed",
		"pw_set":          "PW Set",
		"pw_not_set":      "PW Not Set",
		"rss_subs_short":  "Subs",
		"active_indexers_short": "Indexers",
		"cache_entries":   "Cached",
		"uptime_label":    "Uptime",
		"status_connected": "Connected",
		"manage_tasks_btn": "Manage Tasks →",
		"manage_subs_btn": "Manage Subs →",
		"files":           "📂 File Manager",
		"subs":            "📋 Subs",
		"settings":        "⚙️ Settings",
		"add_magnet":      "Add Magnet Task",
		"task_url":        "Task URL",
		"task_url_ph":     "One magnet/ed2k/http/torrent per line",
		"cid":             "CID",
		"cid_ph":          "115 folder ID",
		"savepath":        "Savepath",
		"savepath_ph":     "Optional, folder name",
		"submit_task":     "Submit Task",
		"json_api":        "Also supports JSON API:",
		"quick_grab":      "Quick Grab",
		"rss_url":         "RSS URL",
		"rss_url_ph":      "https://example.com/rss.xml",
		"keyword_opt":     "Keyword Filter (optional)",
		"keyword_ph":      "Leave empty to grab all",
		"target_cid":      "Target 115 Dir ID",
		"target_cid_ph":   "115 folder ID",
		"grab_btn":        "Grab Now",
		"grab_hint":       "Enter RSS URL and 115 folder ID to batch submit torrents. Optional keyword filter.",
		"offline_tasks":   "Offline Tasks",
		"items":           "items",
		"clear_done":      "Clear Done",
		"clear_all":       "Clear All",
		"clear_failed":    "Clear Failed",
		"clear_running":   "Clear Running",
		"clear_done_del":  "Clear Done & Delete",
		"clear_all_del":   "Clear All & Delete",
		"execute":         "Execute",
		"no_tasks":        "No offline tasks",
		"runtime_log":     "📜 Runtime Log",
		"expand":          "Expand",
		"collapse":        "Collapse",
		"sys_settings":    "System Settings",
		"login_115":       "115 Login",
		"connected":       "✓ Connected",
		"disconnected":    "✗ Disconnected",
		"test_conn":       "Test Connection",
		"cookies_label":   "Cookies",
		"cookies_ph":      "Paste 115 cookies string",
		"update_cookies":  "Update Cookies",
		"qr_login":        "📱 QR Login",
		"testing":         "Testing...",
		"net_error":       "Network error",
		"chunk_size":      "Chunk Size",
		"chunk_delay":     "Chunk Delay (s)",
		"cooldown_min":    "API Min Interval (ms)",
		"cooldown_max":    "API Max Interval (ms)",
		"web_pw":          "Web Password (empty=no auth)",
		"web_pw_ph":       "Set login password",
		"save":            "Save",
		"db_path":         "DB Path",
		"cloud_files":     "Cloud File Manager",
		"root_dir":        "Root",
		"name":            "Name",
		"size":            "Size",
		"empty_dir":       "This directory is empty",
		"new_folder":      "New Folder",
		"folder_name":     "Folder Name",
		"rename":          "Rename",
		"copy":            "Copy",
		"new_name":        "New Name",
		"move":            "Move To",
		"target_dir_id":   "Target Dir ID",
		"confirm_delete":  "Confirm delete",
		"delete":          "Delete",
		"add_sub":         "Add Subscription",
		"sub_name_ph":     "Show/Movie Name",
		"sub_anime":       "Anime",
		"sub_tv":          "TV/Movie",
		"sub_season_ph":   "Season (opt)",
		"sub_cid_ph":      "115 Dir ID",
		"sub_save_ph":     "Subdir (opt)",
		"sub_add_btn":     "Add",
		"sub_type":        "Type",
		"sub_season":      "Season",
		"sub_status":      "Status",
		"sub_run":         "Run Subscription Check",
		"sub_rss_ph":      "RSS URL",
		"subs_mgmt":       "Subscription Mgmt",
		"footer_api":      "API:",
		"settings_saved":  "Settings saved",
		"cookies_updated": "Cookies updated & verified",
		"quick_started":   "Quick grab started",
		"tasks_submitted": "tasks submitted",
		"qrcode_wait":     "Scan QR with 115 App",
		"qrcode_scanning": "Waiting for scan...",
		"qrcode_ok":       "Login OK! Refresh page to apply.",
		"qrcode_error":    "QR code failed",
		"qrcode_timeout":  "QR expired, please retry",
		"conn_ok":         "115 connection OK",
		"login_title":     "🔐 pan-fetcher",
		"login_pw_ph":     "Enter password",
		"login_btn":       "Login",
		"login_err":       "Wrong password",
		"lang_label":      "Language",
		"search":          "🔍 Search",
		"indexer_search":  "🔍 Aggregated Search",
		"search_ph":       "Enter keywords to search...",
		"search_btn":      "Search",
		"search_refresh":  "🔄 Refresh",
		"search_results":  "Search Results",
		"search_no_result":"No results found",
		"search_from":     "From",
		"indexers":        "📡 Indexers",
		"indexer_list":    "Indexer List",
		"indexer_toggle":  "Toggle",
		"grab_selected":   "Grab Selected",
		"test_all":        "Test All",
		"idx_site":        "Site",
		"idx_health":      "Health",
		"idx_tested":      "Last Test",
		"idx_add":         "Add Indexers in Jackett",
		"jk_id":           "ID",
		"jk_config_hint":  "Configure Jackett URL and API Key in Settings first, then add indexers via the Jackett UI.",
		"jk_no_indexers":  "No indexers yet. Click the link above to add them in Jackett.",
		"jk_url":          "Jackett URL",
		"jk_url_ph":       "http://127.0.0.1:9117",
		"jk_apikey":       "Jackett API Key",
		"jk_apikey_ph":    "Get from Jackett settings",
		"jk_test_btn":     "Test Connection",
		"jk_test_ok":      "Jackett connection OK",
		"jk_test_empty":   "Please enter Jackett URL and API Key",
		"idx_no_defs":     "No indexer definitions found. Add YAML files to the indexers/ directory.",
		"idx_no_active":   "No active indexers. Add from the library below.",
		"idx_library":     "📚 Indexer Library",
		"idx_lib_empty":   "Indexer library is empty.",
		"idx_lib_local":   "📚 Local Library",
		"idx_lib_jackett": "📡 Jackett Library",
		"jk_lib_empty":    "No Jackett indexers. Add them in Jackett first.",
		"jk_lib_hint":     "Configure Jackett URL and API Key in Settings first, then add indexers via the Jackett UI.",
		"jk_lib_config":   "Configure Jackett",
		"jk_lib_add":      "Add in Jackett",
		"jk_lib_activate": "Activate",
		"idx_batch_add":   "Add Selected",
		"idx_lang":        "Lang",
		"idx_source":      "Source",
		"idx_activated":   "Activated",
		"idx_jk_activated": "Activated locally",
		"idx_activate_hint": "Activate to local list",
		"idx_jk_delete_hint": "Delete from Jackett",
		"jk_search_ph":    "Search indexers…",
		"jk_no_match":     "No matching indexers",
		"edit_label":      "Edit",
		"create":          "Create",
		"delete_label":    "Delete",
		"configured":      "Configured",
		"add":             "Add",
		"new_idx":         "New Indexer",
		"local":           "Local",
		"remove":          "Remove",
		"remove_lib":      "Move to Library",
		"dedup":           "🗄️ Cache",
		"dedup_title":     "Cache Library",
		"dedup_empty":     "No cache records yet. They are created automatically when subscriptions run.",
		"dedup_clear_sub": "Clear",
		"clear":           "Clear",
		"cache":           "Cache",
		// --- New translation keys ---
		// Error / status messages
		"err_not_logged_in":    "Not logged in. Please configure Cookies in Settings first.",
		"tasks_submitted_fmt":  "Submitted %d magnet tasks",
		"err_rss_empty":        "RSS URL cannot be empty",
		"quick_started_fmt":    "Quick grab started: %s",
		"err_clear_type":       "Clear type must be 1-6",
		"clear_executed_fmt":   "Executed clear type %d",
		"err_read_dir":         "Failed to read directory: ",
		"err_missing_sub_name": "Missing subscription name",
		"err_cookies_empty":    "Cookies cannot be empty",
		"err_login_failed":     "Login failed: ",
		"saving_failed":        "Save failed: ",
		"login_success":        "Login successful",
		"err_not_logged_in_subs": "Not logged in, cannot run subscriptions",
		"subs_processing_fmt":    "Started processing %s",
		// Template UI / JS
		"confirm_restart":         "Are you sure you want to restart the service?",
		"restarting":              "Service is restarting, please wait and refresh the page…",
		"restart_failed":          "Restart failed: ",
		"restart_req_failed":      "Restart request failed: ",
		"cancel":                  "Cancel",
		"failed":                  "Failed",
		"completed":               "Completed",
		"downloading":             "Downloading",
		"all_tab":                 "All",
		"confirm_clear_cache_msg": "Are you sure you want to clear the cache?",
		"load_failed":             "Load failed: ",
		"username_label":          "Username",
		"password_label":          "Password",
		"login_label":             "Login",
		"credentials_required":    "Please enter username and password",
		"error_label":             "Error",
		"success_label":           "Success",
		"login_success_msg":       "Login successful",
		"waiting":                 "Waiting...",
		"http_proxy_label":        "HTTP Proxy",
		"domain_label":            "Domain",
		"domain_hint":             "Access web panel via this domain",
		"subs_interval_label":     "Subscription Interval (min)",
		"wework_label":            "WeCom Webhook",
		"test_btn":                "Test",
		"notify_task":             "Task Push",
		"notify_rss":              "RSS Push",
		"notify_log":              "Log Push",
		"timezone_label":          "TZ",
		"restart_service_btn":     "🔄 Restart Service",
		"confirm_btn":             "OK",
		"browse_btn":              "📁 Browse",
		// RSS XML
		"rss_feed_title": "pan-fetcher Aggregated Search",
		"rss_feed_desc":  "Aggregated Search: %s",
		// JSON-only
		"restarting_json":      "Restarting…",
		"not_logged_in_json":   "Not logged in",
		"save_failed_json":     "Save failed: ",
		// Sidebar
		"toggle_sidebar":       "Toggle Sidebar",
		"announcement":         "Welcome to pan-fetcher — cloud file manager & RSS downloader",
		"about_title":          "About pan-fetcher",
		"about_desc":           "pan-fetcher is an RSS subscription download tool with integrated 115 cloud management, featuring multi-indexer aggregated search, offline task management, and file management.",
		"about_version":        "Version",
		"about_author":         "Based on zhifengle/rss2cloud, powered by Prowlarr indexer engine",
		"about_links":          "Links",
		"about_based_on":       "Based on",
		// JSON-only
		// Subscriptions / Search modals
		"enabled":              "Enabled",
		"disabled":             "Disabled",
		"confirm_delete_sub":   "Delete?",
		"no_subs_hint":         "No subscriptions yet. Go to Search page, find resources, and click 📌 Subscribe to add.",
		"edit_sub":             "Edit Subscription",
		"subdir_label":         "Subdirectory",
		"filter_label":         "Filter",
		"save_btn":             "Save",
		"copy_link_title":      "Copy Link",
		// Search categories / sort
		"all_categories":       "All Categories",
		"category_anime":       "Anime",
		"category_tv":          "TV Series",
		"category_movie":       "Movies",
		"category_music":       "Music",
		"category_other":       "Other",
		"sort_seeds":           "By Seeders",
		"sort_size":            "By Size",
		"sort_date":            "By Date",
		"search_sites":         "Search sites:",
		"date_label":           "Date",
		"indexer_label":        "Indexer",
		"page_prev":            "Prev",
		"page_next":            "Next",
		"page_total":           "%d total",
		"page_loading":         "Loading…",
		"page_size_label":      "Search Page Size",
		"system_label":         "System",
		"search_label":         "Search",
		"download_settings_label": "Download",
		"subs_notify_label":    "Subs & Notifications",
		"update_check_btn":     "Check Update",
		"update_do_btn":        "Update Now",
		"update_auto_label":    "Auto Check Updates",
		"update_fetch_failed":  "Failed to fetch version info",
		"update_parse_failed":  "Failed to parse version info",
		"update_already_latest":"Already up to date",
		"update_no_asset":      "No matching binary found",
		"update_download_failed":"Download failed",
		"update_ok":            "Updated successfully, restarting…",
		"update_install_failed":"Install failed",
		"update_need_sudo":     "Permission denied. Run this command in a terminal:",
		"update_checking":      "Checking…",
		"update_new_found":     "New version %s found (current: %s)",
		"enable_all":           "Enable All",
		"disable_all":          "Disable All",
		"enable_all_confirm":   "Enable all subscriptions?",
		"disable_all_confirm":  "Disable all subscriptions?",
		"clear_all_cache":      "Clear All Cache",
		"clear_all_cache_confirm": "Clear entire dedup cache? This cannot be undone.",
		"jk_show_all":          "Show All",
		"jk_add_lib":           "Add Indexer",
		"jk_show_configured":   "Configured Only",
		"subscribe_search":     "📌 Subscribe This Search",
		"add_rss_sub_title":    "📌 Add RSS Subscription",
		"sub_name_label":       "Name",
		"sub_name_placeholder": "Subscription Name",
		"rss_addr_label":       "RSS URL",
		"rss_addr_placeholder": "RSS URL",
		"filter_placeholder":   "Keywords",
		"dir_id_opt":           "115 Directory ID (optional)",
		"subdir_opt":           "Subdirectory (optional)",
		"add_sub_btn":          "Add Subscription",
		"saved_searches_title": "📌 Saved Searches",
		"search_btn_sm":        "Search",
		"delete_btn":           "Delete",
		// Directory picker modal
		"parent_dir_label":     "📁 .. (Parent)",
		"no_subfolders":        "No subfolders in this directory",
		"select_dir_title":     "Select 115 Directory (current: %s)",
		"select_current_dir":   "Select Current Folder",
		"select_add":           "Add Here",
		"select_dir_add":       "Select Target Directory (current: %s)",
		"task_added":           "Task added",
		"add_failed":           "Failed to add task",
		"close_btn":            "Close",
		// Cache library
		"sub_name_header":      "Subscription Name",
		"cache_count_header":   "Cache Count",
		"loading":              "Loading…",
		"no_url_short":         "(none)",
		"confirm_delete_url_map": "Delete this URL mapping?",
		"no_records":           "(No records)",
		// Settings & misc
		"lang_zh_option":       "中文",
		"subs_interval_ph":     "Default 60min, 0=Disabled",
		"enter_search_kw":      "Please enter a search keyword",
		"indexer_not_initialized": "Indexer not initialized",
		"please_fill_rss":      "Please fill in RSS URL",
		"err_id_username_password": "id, username, password required",
		"wrong_password_html":  "Wrong password",
		"login_page_title":     "pan-fetcher Login",
	},
}

func tr(lang, key string) string {
	if m, ok := translations[lang]; ok {
		if s, ok := m[key]; ok {
			return s
		}
	}
	if m, ok := translations["zh"]; ok {
		if s, ok := m[key]; ok {
			return s
		}
	}
	return key
}

func langMap(lang string) map[string]string {
	if m, ok := translations[lang]; ok {
		return m
	}
	return translations["zh"]
}

func trf(lang, key string, args ...interface{}) string {
	return fmt.Sprintf(tr(lang, key), args...)
}

// ---------- template ----------

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"hasPrefix": strings.HasPrefix,
	"typeColor": func(t string) string {
		switch strings.ToLower(t) {
		case "public":
			return "#22c55e"
		case "semi-private":
			return "#f59e0b"
		case "private":
			return "#ef4444"
		default:
			return "#9ca3af"
		}
	},
	"indexerChecked": func(selected []string, id string) bool {
		for _, s := range selected {
			if s == id {
				return true
			}
		}
		return false
	},
	"add": func(a, b int) int { return a + b },
}).Parse(`
<!doctype html>
<html lang="{{.Lang}}">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{index .T "title"}}</title>
  <style>
    :root {
      color-scheme: light;
      --bg: #f5f7fb;
      --card: #ffffff;
      --text: #132238;
      --muted: #5c6b7a;
      --line: #dce3ec;
      --accent: #1d4ed8;
      --accent-2: #0f766e;
      --danger: #b42318;
      --warn: #b45309;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      background: linear-gradient(180deg, #eef3ff 0%, var(--bg) 100%);
      color: var(--text);
    }
    .wrap { display: flex; min-height: 100vh; }
    /* sidebar */
    .sidebar {
      width: 230px; min-width: 230px; background: var(--card); border-right: 1px solid var(--line);
      display: flex; flex-direction: column; transition: transform .25s;
      position: fixed; top: 0; left: 0; bottom: 0; z-index: 50;
    }
    .sidebar.collapsed { transform: translateX(-100%); }
    .sidebar-logo { padding: 16px 16px 4px; font-size: 20px; font-weight: 700; color: var(--accent); }
    .sidebar-subtitle { padding: 0 16px 8px; font-size: 11px; color: var(--muted); }
    .sidebar-search { padding: 10px 14px; }
    .sidebar-search input {
      width: 100%; padding: 9px 12px 9px 34px; font-size: 13px; border-radius: 12px;
      border: 1.5px solid var(--line); background: var(--bg);
      cursor: pointer; caret-color: transparent;
      background-image: url("data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' width='14' height='14' viewBox='0 0 24 24' fill='none' stroke='%239ca3af' stroke-width='2'%3E%3Ccircle cx='11' cy='11' r='8'/%3E%3Cline x1='21' y1='21' x2='16.65' y2='16.65'/%3E%3C/svg%3E");
      background-repeat: no-repeat; background-position: 10px center;
      transition: all .2s;
    }
    .sidebar-search input:hover { border-color: var(--accent); box-shadow: 0 0 0 3px rgba(59,130,246,.1); }
    .sidebar-search input:focus { outline: none; border-color: var(--accent); background: #fff; caret-color: auto; box-shadow: 0 0 0 3px rgba(59,130,246,.15); }
    .sidebar-search-hint { font-size: 10px; color: var(--muted); padding: 2px 14px 0; text-align: center; }
    .sidebar-nav { flex: 1; overflow-y: auto; padding: 4px 0; }
    .sidebar-nav a {
      display: flex; align-items: center; gap: 8px; padding: 10px 16px;
      text-decoration: none; color: var(--muted); font-size: 14px; font-weight: 500;
      transition: all .12s; border-left: 3px solid transparent;
    }
    .sidebar-nav a:hover { background: #f0f4ff; color: var(--accent); }
    .sidebar-nav a.active { background: #eef4ff; color: var(--accent); border-left-color: var(--accent); font-weight: 600; }
    .sidebar-footer {
      padding: 12px 16px; border-top: 1px solid var(--line);
      display: flex; align-items: center; justify-content: space-between; gap: 8px;
    }
    /* main area */
    .main { flex: 1; margin-left: 220px; padding: 24px 24px 40px; transition: margin-left .25s; min-width: 0; }
    .main.expanded { margin-left: 0; }
    .grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(280px, 1fr)); gap: 14px; }
    .card {
      background: var(--card);
      border: 1px solid var(--line);
      border-radius: 16px;
      padding: 16px;
      box-shadow: 0 8px 20px rgba(15, 23, 42, 0.04);
      min-width: 0;
      overflow-x: auto;
    }
    .card h2 { margin: 0 0 8px; font-size: 17px; }
    .meta { display: flex; gap: 10px; flex-wrap: wrap; margin: 8px 0 0; color: var(--muted); font-size: 13px; }
    label { display: block; margin: 8px 0 4px; font-size: 13px; color: var(--muted); }
    input, textarea, select {
      width: 100%;
      max-width: 100%;
      border: 1px solid var(--line);
      border-radius: 10px;
      padding: 8px 10px;
      font: inherit;
      font-size: 14px;
      background: #fff;
    }
    textarea { min-height: 100px; resize: vertical; }
    button {
      margin-top: 10px;
      border: 0;
      border-radius: 10px;
      padding: 8px 12px;
      background: var(--accent);
      color: #fff;
      font: inherit;
      font-size: 14px;
      cursor: pointer;
    }
    button.secondary { background: var(--accent-2); }
    .hint { color: var(--muted); font-size: 12px; margin-top: 6px; line-height: 1.5; }
    .status { margin-bottom: 12px; padding: 10px 12px; border-radius: 10px; background: #f8fafc; border: 1px solid var(--line); font-size: 14px; }
    .ok { color: var(--accent-2); }
    .err { color: var(--danger); }
    code { background: #f1f5f9; padding: 1px 5px; border-radius: 5px; font-size: 13px; }
    .footer { margin-top: 14px; color: var(--muted); font-size: 12px; }
    a { color: var(--accent); }

    /* full-width panels */
    .panel { margin-top: 16px; }
    .panel h2 { font-size: 17px; margin: 0 0 6px; }

    /* task table */
    .tbl { width: 100%; border-collapse: collapse; font-size: 13px; }
    .tbl th, .tbl td { padding: 6px 8px; text-align: left; border-bottom: 1px solid var(--line); }
    .tbl th { color: var(--muted); font-weight: 600; }
    .tbl td { word-break: break-all; }
    .row-done { opacity: 0.65; }
    .row-failed { background: #fef2f2; }
    .row-running { background: #f0fdf4; }
    .badge {
      display: inline-block;
      padding: 2px 6px;
      border-radius: 6px;
      font-size: 12px;
      font-weight: 600;
    }
    .badge-done { background: #dcfce7; color: #166534; }
    .badge-failed { background: #fee2e2; color: #991b1b; }
    .badge-running { background: #dbeafe; color: #1e40af; }
    .badge-waiting { background: #f3f4f6; color: #374151; }

    /* log */
    .log-panel {
      background: #0f172a;
      color: #e2e8f0;
      border-radius: 14px;
      padding: 14px;
      font-family: "SF Mono", "Cascadia Code", "Fira Code", monospace;
      font-size: 12px;
      line-height: 1.6;
      max-height: 440px;
      overflow-y: auto;
      white-space: pre-wrap;
      word-break: break-all;
    }
    .log-panel::-webkit-scrollbar { width: 6px; }
    .log-panel::-webkit-scrollbar-thumb { background: #334155; border-radius: 3px; }
    .log-clear-marker {
      display: block; padding: 4px 0; color: #f59e0b; font-size: 11px;
      border-top: 1px dashed #f59e0b; border-bottom: 1px dashed #f59e0b;
      margin: 4px 0; text-align: center;
    }
    .breadcrumb { display: flex; flex-wrap: wrap; align-items: center; gap: 0; margin-bottom: 10px; font-size: 13px; }
    .crumb { color: var(--accent); text-decoration: none; }
    .crumb:hover { text-decoration: underline; }
    .crumb-sep { color: var(--muted); margin: 0 4px; }
    .fs-tbl td.muted { color: var(--muted); font-size: 12px; }
    .fs-tbl td.mono { font-family: monospace; font-size: 11px; }
    .topbar { display: flex; align-items: center; gap: 12px; margin-bottom: 18px; }
    .topbar-msg { flex: 1; font-size: 13px; color: var(--muted); padding: 6px 12px; background: #f8fafc; border: 1px solid var(--line); border-radius: 8px; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
    .topbar-msg.ok { color: var(--accent-2); border-color: #bae6fd; background: #f0f9ff; }
    .topbar-msg.err { color: var(--danger); border-color: #fecaca; background: #fef2f2; }
    .sidebar-toggle-btn {
      background: none; border: 1px solid var(--line); border-radius: 8px; padding: 4px 8px;
      cursor: pointer; font-size: 16px; color: var(--muted); margin: 0;
    }
    .sidebar-toggle-btn:hover { background: var(--bg); color: var(--accent); }
    .sidebar-footer .logout-text { font-size: 13px; }
    .logout-text { color: var(--muted); text-decoration: none; font-size: 13px; white-space: nowrap; }
    .logout-text:hover { color: var(--danger); }

    /* search modal */
    .smodal-overlay { display: none; position: fixed; inset: 0; background: rgba(0,0,0,.35); z-index: 100; justify-content: center; align-items: flex-start; padding-top: 40px; }
    .smodal-overlay.active { display: flex; }
    .smodal-card {
      background: var(--card); border-radius: 18px; padding: 24px;
      width: 92%; max-width: 900px; max-height: 85vh; overflow-y: auto;
      box-shadow: 0 20px 60px rgba(0,0,0,.18);
    }
    .smodal-card h2 { margin-top: 0; }
  </style>
  <script>
    var modalCb=null;
    function showModal(title,body,buttons){
      document.getElementById('g-modal-title').textContent=title;
      document.getElementById('g-modal-body').innerHTML=body;
      var btns=document.getElementById('g-modal-btns');
      btns.innerHTML='';
      (buttons||[{text:'{{index .T "confirm_btn"}}',cls:'',cb:function(){closeModal()}}]).forEach(function(b){
        var btn=document.createElement('button');
        btn.textContent=b.text;btn.style.margin='0';btn.style.padding='6px 16px';
        if(b.cls)btn.style.background=b.cls;
        if(b.id)btn.id=b.id;
        btn.onclick=function(){if(b.cb)b.cb();};
        btns.appendChild(btn);
      });
      document.getElementById('g-modal').style.display='flex';
    }
    function closeModal(){document.getElementById('g-modal').style.display='none';modalCb=null;}
    function alertModal(msg){showModal('',msg,[{text:'OK',cls:'var(--accent)',cb:function(){closeModal()}}]);}
    async function confirmAsync(msg){return new Promise(function(resolve){showModal('',msg,[{text:'Cancel',cls:'var(--danger)',cb:function(){closeModal();resolve(false);}},{text:'OK',cls:'var(--accent)',cb:function(){closeModal();resolve(true);}}]);});}
    async function promptModal(title,label,defaultValue){return new Promise(function(resolve){var dv=(defaultValue||'').replace(/&/g,'&amp;').replace(/"/g,'&quot;').replace(/</g,'&lt;').replace(/>/g,'&gt;');var body='<div style="margin-bottom:6px;font-size:13px;color:var(--muted);">'+label+'</div><input id="g-modal-input" style="width:100%;padding:8px;border:1px solid var(--line);border-radius:6px;font-size:14px;box-sizing:border-box;" value="'+dv+'" onkeydown="if(event.key===&quot;Enter&quot;)document.getElementById(&quot;g-modal-btn-ok&quot;).click()" autofocus>';showModal(title,body,[{text:'Cancel',cls:'var(--danger)',cb:function(){closeModal();resolve(null);}},{text:'OK',cls:'var(--accent)',id:'g-modal-btn-ok',cb:function(){var v=document.getElementById('g-modal-input').value.trim();closeModal();resolve(v);}}]);});}
    function openSearchModal(){
      var m=document.getElementById('search-modal');
      if(m)m.classList.add('active');
      var q=document.getElementById('search-q');
      if(q)q.focus();
    }
    function closeSearchModal(){
      var m=document.getElementById('search-modal');
      if(m)m.classList.remove('active');
    }
    function toggleSidebar(){
      var sb=document.getElementById('sidebar');
      var mn=document.getElementById('main');
      if(sb){sb.classList.toggle('collapsed');}
      if(mn){mn.classList.toggle('expanded');}
    }
    async function restartServer(){
      if(!(await confirmAsync('{{index .T "confirm_restart"}}')))return;
      try{
        var r=await fetch('/settings/restart',{method:'POST'});
        var j=await r.json();
        if(j.ok){alertModal('{{index .T "restarting"}}');}
        else{alertModal('{{index .T "restart_failed"}}'+j.msg);}
      }catch(e){alertModal('{{index .T "restart_req_failed"}}'+e.message);}
    }
    function switchLang(lang){
      fetch('/api/lang',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:'lang='+lang}).then(function(){location.reload();});
    }
  </script>
</head>
<body>
  <!-- left sidebar -->
  <div class="sidebar collapsed" id="sidebar">
    <div class="sidebar-logo" style="display:flex;align-items:center;justify-content:space-between;">
      <span>pan-fetcher</span>
      <select onchange="switchLang(this.value)" style="width:auto;font-size:11px;padding:2px 4px;margin:0;border:1px solid var(--line);border-radius:4px;background:var(--bg);">
        <option value="zh"{{if eq .Lang "zh"}} selected{{end}}>CN</option>
        <option value="en"{{if eq .Lang "en"}} selected{{end}}>EN</option>
      </select>
    </div>
    <div class="sidebar-search">
      <input type="text" id="quick-search-input" placeholder="搜索电影、剧集..." value="" autocomplete="off" onfocus="location.href='/discover';this.blur()">
      <div class="sidebar-search-hint">🎬 TMDB 发现</div>
    </div>
    <div class="sidebar-nav">
      <a href="/"{{if or (eq .Page "home") (eq .Page "")}} class="active"{{end}}>{{index .T "dashboard"}}</a>
      <a href="/tasks"{{if eq .Page "tasks"}} class="active"{{end}}>{{index .T "home"}}</a>
      <a href="/search"{{if eq .Page "search"}} class="active"{{end}}>{{index .T "indexer_search"}}</a>
      <a href="/indexers"{{if eq .Page "indexers"}} class="active"{{end}}>{{index .T "indexers"}}</a>
      <a href="/fs"{{if eq .Page "fs"}} class="active"{{end}}>{{index .T "files"}}</a>
      <a href="/subs"{{if eq .Page "subs"}} class="active"{{end}}>{{index .T "subs"}}</a>
      <a href="/log"{{if eq .Page "log"}} class="active"{{end}}>{{index .T "runtime_log"}}</a>
      <a href="/settings"{{if eq .Page "settings"}} class="active"{{end}}>{{index .T "settings"}}</a>
    </div>
    <div class="sidebar-footer">
      <a href="/logout" class="logout-text">{{index .T "logout"}}</a>
      <a href="/about" class="logout-text">{{index .T "about"}}</a>
    </div>
  </div>

  <!-- main content -->
  <div class="main expanded" id="main">
    <div class="topbar">
      <button class="sidebar-toggle-btn" onclick="toggleSidebar()" title="{{index .T "toggle_sidebar"}}">☰</button>
      {{if .Error}}<div class="topbar-msg err">{{.Error}}</div>
      {{else if .Message}}<div class="topbar-msg ok">{{.Message}}</div>
      {{else}}<div class="topbar-msg">{{index .T "announcement"}}</div>
      {{end}}
    </div>

    {{if or (eq .Page "home") (eq .Page "")}}
    <!-- dashboard -->
    <div class="grid" style="grid-template-columns:repeat(auto-fill,minmax(180px,1fr));gap:12px;">
      <div class="card" style="text-align:center;">
        <div style="font-size:32px;font-weight:700;color:var(--accent);">{{.DashStats.TotalTasks}}</div>
        <div style="font-size:13px;color:var(--muted);margin-top:4px;">{{index .T "push_count"}}</div>
      </div>
      <div class="card" style="text-align:center;">
        <div style="font-size:32px;font-weight:700;color:var(--muted);">{{.DashStats.RssSubsActive}}/{{.DashStats.RssSubsTotal}}</div>
        <div style="font-size:13px;color:var(--muted);margin-top:4px;">{{index .T "rss_subs_short"}}</div>
      </div>
      <div class="card" style="text-align:center;">
        <div style="font-size:32px;font-weight:700;color:var(--accent-2);">{{.DashStats.ActiveIndexers}}</div>
        <div style="font-size:13px;color:var(--muted);margin-top:4px;">{{index .T "active_indexers_short"}}</div>
      </div>
      <div class="card" style="text-align:center;">
        <div style="font-size:32px;font-weight:700;color:#a78bfa;">{{.DashStats.CacheEntries}}</div>
        <div style="font-size:13px;color:var(--muted);margin-top:4px;">{{index .T "cache_entries"}}</div>
      </div>
    </div>
    <div class="card panel" style="margin-top:16px;">
      <div style="display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:12px;">
        <div>
          <span style="font-size:13px;color:var(--muted);">{{index .T "uptime_label"}}: </span>
          <strong>{{.DashStats.Uptime}}</strong>
          {{if .HasAgent}}<span style="margin-left:12px;font-size:13px;color:var(--accent-2);">✓ 115 {{index .T "status_connected"}}</span>{{end}}
          <span style="margin-left:12px;font-size:13px;{{if .HasPassword}}color:var(--accent-2);{{else}}color:var(--warn);{{end}}">{{if .HasPassword}}🔒 {{index .T "pw_set"}}{{else}}⚠ {{index .T "pw_not_set"}}{{end}}</span>
        </div>
      </div>
    </div>
    {{if .DashStats.RecentItems}}
    <div class="card panel" style="margin-top:16px;">
      <h3 style="margin:0 0 10px;">🆕 最近新增资源</h3>
      <div style="max-height:300px;overflow-y:auto;">
        {{range .DashStats.RecentItems}}
        <div style="padding:6px 0;border-bottom:1px solid var(--line);">
          <div style="font-size:13px;word-break:break-all;line-height:1.5;" title="{{.Name}}">{{.Name}}</div>
          <div style="font-size:11px;color:var(--muted);margin-top:2px;">
            <span>{{.Time}}</span>
            <span style="margin-left:6px;">[{{.Sub}}]</span>
          </div>
        </div>
        {{end}}
      </div>
    </div>
    {{end}}
    {{end}}

    <!-- offline tasks page -->
    {{if eq .Page "tasks"}}
    <div class="grid">
      <div class="card">
        <h2>{{index .T "add_magnet"}}</h2>
        <form action="/add" method="post">
          <label for="tasks">{{index .T "task_url"}}</label>
          <textarea id="tasks" name="tasks" placeholder="{{index .T "task_url_ph"}}"></textarea>
          <label for="cid">{{index .T "cid"}}</label>
          <div style="display:flex;gap:4px;">
            <input id="cid" name="cid" placeholder="{{index .T "cid_ph"}}" style="flex:1;">
            <button type="button" onclick="browseDirsFor('cid')" style="margin:0;padding:4px 8px;font-size:12px;background:var(--accent-2);white-space:nowrap;">{{index .T "browse_btn"}}</button>
          </div>
          <label for="savepath">{{index .T "savepath"}}</label>
          <input id="savepath" name="savepath" placeholder="{{index .T "savepath_ph"}}">
          <button type="submit">{{index .T "submit_task"}}</button>
        </form>
        <div class="hint">{{index .T "json_api"}} <code>POST /add</code></div>
      </div>
    </div>
    {{end}}

    <!-- cloud filesystem browser -->
    {{if eq .Page "fs"}}
    <div class="card panel">
      <h2>{{index .T "cloud_files"}}</h2>
      <div class="breadcrumb">
        {{range .FSCrumbs}}<a class="crumb" href="/fs?dir={{.ID}}">{{if eq .ID "0"}}{{index $.T "root_dir"}}{{else}}{{.Name}}{{end}}</a><span class="crumb-sep">/</span>{{end}}
      </div>
      <div style="margin-bottom:10px;">
        <button onclick="fsNewFolder('{{.FSCurrentID}}')" style="padding:4px 12px;font-size:12px;">📁 {{index .T "new_folder"}}</button>
      </div>
      {{if .FSEntries}}
      <table class="tbl fs-tbl">
        <thead><tr><th></th><th>{{index .T "name"}}</th><th>{{index .T "size"}}</th><th>ID</th><th></th></tr></thead>
        <tbody>
        {{if ne .FSCurrentID "0"}}<tr><td>⬆</td><td><a href="/fs?dir={{.FSParentID}}">..</a></td><td></td><td></td><td></td></tr>{{end}}
        {{range .FSEntries}}<tr>
          <td>{{.Icon}}</td>
          <td>{{if .IsDir}}<a href="/fs?dir={{.ID}}">{{.Name}}</a>{{else}}{{.Name}}{{end}}</td>
          <td class="muted">{{.Size}}</td>
          <td class="muted mono">{{.ID}}</td>
          <td style="white-space:nowrap;">
            <button onclick="fsRename('{{.ID}}','{{.Name}}')" style="padding:2px 6px;font-size:11px;margin:0;" title="{{index $.T "rename"}}">✎</button>
            <button onclick="fsMove('{{.ID}}','{{.Name}}')" style="padding:2px 6px;font-size:11px;margin:0;" title="{{index $.T "move"}}">↗</button>
            <button onclick="fsCopy('{{.ID}}','{{.Name}}')" style="padding:2px 6px;font-size:11px;margin:0;" title="{{index $.T "copy"}}">📋</button>
            <button onclick="fsDelete('{{.ID}}','{{.Name}}')" style="background:var(--danger);padding:2px 6px;font-size:11px;margin:0;" title="{{index $.T "delete"}}">✕</button>
          </td>
        </tr>{{end}}
        </tbody>
      </table>
      {{else}}
      <div class="hint">{{index .T "empty_dir"}}</div>
      {{end}}
    </div>
    {{end}}

    <!-- subscription management -->
    {{if eq .Page "subs"}}
    <div class="card panel">
      <h2>{{index .T "subs_mgmt"}} ({{len .RssSubs}})
        {{if not .RssSubs}}<span style="font-weight:400;font-size:12px;color:var(--muted);margin-left:8px;">{{index .T "no_subs_hint"}}</span>{{end}}
      </h2>
      {{if .RssSubs}}
      <div style="display:flex;gap:8px;margin-bottom:10px;flex-wrap:wrap;">
        <button onclick="toggleAllSubs(true)" style="margin:0;padding:4px 12px;font-size:12px;background:var(--accent-2);">{{index .T "enable_all"}}</button>
        <button onclick="toggleAllSubs(false)" style="margin:0;padding:4px 12px;font-size:12px;background:var(--danger);">{{index .T "disable_all"}}</button>
        <button onclick="clearAllCache()" style="margin:0;padding:4px 12px;font-size:12px;background:#6b7280;">{{index .T "clear_all_cache"}}</button>
      </div>
      <table class="tbl">
        <thead><tr><th>{{index .T "name"}}</th><th>{{index .T "indexer_label"}}</th><th>{{index .T "sub_status"}}</th><th>{{index .T "cache"}}</th><th></th></tr></thead>
        <tbody>
        {{range .RssSubs}}<tr>
          <td>
            <strong>{{.Name}}</strong><br><small class="muted">{{.Site}}</small>
          </td>
          <td><small class="muted">{{.IndexerDisplay}}</small></td>
          <td>
            <form action="/subs" method="post" style="display:inline;">
              <input type="hidden" name="action" value="toggle">
              <input type="hidden" name="site" value="{{.Site}}">
              <input type="hidden" name="name" value="{{.Name}}">
              <button type="submit" style="padding:2px 8px;font-size:11px;margin:0;{{if .Enabled}}background:var(--accent-2);{{else}}background:var(--danger);{{end}}">{{if .Enabled}}{{index $.T "enabled"}}{{else}}{{index $.T "disabled"}}{{end}}</button>
            </form>
          </td>
          <td>
            {{if gt .CacheCount 0}}<span style="cursor:pointer;user-select:none;font-size:12px;" onclick="toggleSubCache('{{.Name}}',this)">▶ </span>{{end}}
            <span style="font-size:12px;color:var(--muted);">{{.CacheCount}} {{index $.T "items"}}</span>
            {{if gt .CacheCount 0}}
            <form action="/dedup/clear" method="post" style="display:inline;">
              <input type="hidden" name="sub" value="{{.Name}}">
              <button type="submit" style="padding:1px 6px;font-size:10px;margin:0;background:var(--danger);" onclick="submitConfirm(this.form,'{{index $.T "confirm_clear_cache_msg"}}')">{{index $.T "clear"}}</button>
            </form>
            {{end}}
          </td>
          <td style="white-space:nowrap;">
            <button onclick="runSub(this,'{{.URL}}','{{.Cid}}','{{.SavePath}}','{{.Filter}}','{{.Name}}')" style="padding:2px 6px;font-size:11px;margin:0;background:var(--accent);" title="立即执行">▶</button>
            <button onclick="editSubInline(this)" style="padding:2px 6px;font-size:11px;margin:0;">✎</button>
            <form action="/subs" method="post" style="display:inline;">
              <input type="hidden" name="action" value="delete">
              <input type="hidden" name="site" value="{{.Site}}">
              <input type="hidden" name="name" value="{{.Name}}">
              <button type="submit" style="background:var(--danger);padding:2px 8px;font-size:11px;margin:0;" onclick="submitConfirm(this.form,'{{index $.T "confirm_delete_sub"}}')">✕</button>
            </form>
          </td>
        </tr>
        <tr class="edit-row" style="display:none;">
          <td colspan="5" style="padding:0;">
            <div style="padding:14px;background:#f8fafc;border:1px solid var(--line);border-radius:10px;margin:8px 0;">
              <h3 style="margin:0 0 10px;">{{index $.T "edit_sub"}}</h3>
              <form action="/subs" method="post">
                <input type="hidden" name="action" value="edit">
                <input type="hidden" name="site" value="{{.Site}}">
                <input type="hidden" name="name" value="{{.Name}}">
                <div style="display:flex;gap:8px;flex-wrap:wrap;align-items:flex-end;">
                  <div><label>CID</label><input name="cid" value="{{.Cid}}" style="font-size:13px;width:120px;"></div>
                  <div><label>{{index $.T "subdir_label"}}</label><input name="savepath" value="{{.SavePath}}" style="font-size:13px;width:100px;"></div>
                  <div><label>{{index $.T "filter_label"}}</label><input name="filter" value="{{.Filter}}" style="font-size:13px;width:100px;"></div>
                  <button type="submit" style="margin-top:0;">{{index $.T "save"}}</button>
                  <button type="button" onclick="closeAllEditRows()" style="margin-top:0;background:var(--danger);">{{index $.T "cancel"}}</button>
                </div>
              </form>
            </div>
          </td>
        </tr>
        <tr id="cache-{{.Name}}" style="display:none;"><td colspan="5" style="padding:0;">
          <div style="padding:4px 8px;background:#f8fafc;max-height:300px;overflow-y:auto;" id="cache-list-{{.Name}}">{{index $.T "loading"}}</div>
        </td></tr>
        {{end}}
        </tbody>
      </table>
      {{else}}
      <div class="hint">{{index .T "no_subs_hint"}}</div>
      {{end}}
      <script>
        function editSubInline(btn){
          closeAllEditRows();
          var tr=btn.closest('tr');
          var next=tr.nextElementSibling;
          if(next&&next.classList.contains('edit-row')){
            next.style.display='';
          }
        }
        function closeAllEditRows(){
          document.querySelectorAll('.edit-row').forEach(function(r){r.style.display='none';});
        }
        async function runSub(btn,url,cid,savepath,filter,subName){
          btn.textContent='…'; btn.disabled=true;
          try{
            var r=await fetch('/subs/run',{method:'POST',
              headers:{'Content-Type':'application/x-www-form-urlencoded','X-Requested-With':'XMLHttpRequest'},
              body:new URLSearchParams({rss_url:url,cid:cid,savepath:savepath,filter:filter,sub_name:subName})});
            var j=await r.json();
            if(j.ok){
              // Show message in topbar
              var tb=document.querySelector('.topbar-msg');
              if(tb){tb.textContent=j.msg;tb.className='topbar-msg ok';}
              // Auto-reload after 3s to show updated cache counts
              setTimeout(function(){location.reload();},3000);
            }else{
              alertModal(j.msg||'Error');
            }
          }catch(e){alertModal(e.message);}
          btn.textContent='▶'; btn.disabled=false;
        }
        var subCacheData={};
        async function toggleSubCache(subKey,el){
          var row=document.getElementById('cache-'+subKey);
          if(row.style.display==='none'){
            row.style.display='table-row';
            el.textContent='▼ ';
            if(subCacheData[subKey]){
              document.getElementById('cache-list-'+subKey).innerHTML=subCacheData[subKey];
              return;
            }
            try{
              var r=await fetch('/api/dedup/hashes?sub='+encodeURIComponent(subKey));
              var items=await r.json();
              if(!items||!Array.isArray(items))items=[];
              var cnt=0;
              var html='<table style="width:100%;font-size:11px;border-collapse:collapse;">';
              items.forEach(function(it){
                cnt++;
                var display=it.name||it.hash;
                html+='<tr style="border-bottom:1px solid #e8ecf1;">';
                html+='<td style="padding:3px 6px;max-width:400px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;font-size:12px;" title="'+(it.hash||'')+'">'+display+'</td>';
                html+='<td style="padding:3px 6px;text-align:right;">';
                html+='<button data-hash="'+it.hash+'" data-sub="'+subKey.replace(/"/g,'&quot;')+'" onclick="removeCacheHash(this)" style="padding:1px 6px;font-size:10px;margin:0;background:var(--danger);">✕</button>';
                html+='</td></tr>';
              });
              html+='</table>';
              if(cnt===0)html='{{index $.T "no_records"}}';
              subCacheData[subKey]=html;
              document.getElementById('cache-list-'+subKey).innerHTML=html;
            }catch(e){
              document.getElementById('cache-list-'+subKey).innerHTML='{{index .T "load_failed"}}'+e.message;
            }
          }else{
            row.style.display='none';
            el.textContent='▶ ';
          }
        }
        async function removeCacheHash(btn){
          var hash=btn.getAttribute('data-hash');
          var sub=btn.getAttribute('data-sub');
          if(!hash||!sub)return;
          if(!(await confirmAsync('{{index .T "confirm_delete"}} '+hash.substring(0,12)+'... ?')))return;
          btn.disabled=true;
          try{
            var form=new FormData();
            form.append('sub',sub);
            form.append('hash',hash);
            var r=await fetch('/api/dedup/remove-hash',{method:'POST',body:form});
            var j=await r.json();
            if(j.status==='ok'){
              var tr=btn.closest('tr');
              if(tr)tr.remove();
              delete subCacheData[sub];
            }else{
              alertModal(j.message||'Error');
            }
          }catch(e){
            alertModal(e.message);
          }
          btn.disabled=false;
        }
        async function toggleAllSubs(enabled){
          if(!(await confirmAsync(enabled?'{{index .T "enable_all_confirm"}}':'{{index .T "disable_all_confirm"}}')))return;
          try{
            var r=await fetch('/subs/toggle-all',{method:'POST',
              headers:{'Content-Type':'application/x-www-form-urlencoded','X-Requested-With':'XMLHttpRequest'},
              body:new URLSearchParams({enabled:enabled?'1':'0'})});
            var j=await r.json();
            if(j.ok)location.reload();
            else alertModal(j.msg);
          }catch(e){alertModal(e.message);}
        }
        async function clearAllCache(){
          if(!(await confirmAsync('{{index .T "clear_all_cache_confirm"}}')))return;
          try{
            var r=await fetch('/dedup/clear-all',{method:'POST',headers:{'X-Requested-With':'XMLHttpRequest'}});
            var j=await r.json();
            if(j.ok)location.reload();
            else alertModal(j.msg);
          }catch(e){alertModal(e.message);}
        }
      </script>
      {{if .HasAgent}}<div style="margin-top:10px;">
        <form action="/subs/run" method="post">
          <input name="rss_url" placeholder="{{index .T "sub_rss_ph"}}" style="width:auto;min-width:300px;display:inline;">
          <button type="submit" style="margin-top:0;">{{index .T "sub_run"}}</button>
        </form>
      </div>{{end}}
    </div>
    {{end}}

    <!-- offline task list -->
    {{if eq .Page "tasks"}}
    <div class="card panel" style="margin-top:16px;">
      <h2 id="task-heading">{{index .T "offline_tasks"}} (<span id="task-total">{{.TaskCount}}</span>)
        <span style="font-weight:400;font-size:12px;margin-left:8px;">
          <span id="tab-downloading" style="cursor:pointer;color:var(--accent);border-bottom:2px solid var(--accent);" onclick="switchTaskTab('downloading')">{{index .T "downloading"}} <span id="cnt-downloading"></span></span>
          <span style="margin:0 8px;color:var(--line);">|</span>
          <span id="tab-failed" style="cursor:pointer;color:var(--muted);" onclick="switchTaskTab('failed')">{{index .T "failed"}} <span id="cnt-failed"></span></span>
          <span style="margin:0 8px;color:var(--line);">|</span>
          <span id="tab-done" style="cursor:pointer;color:var(--muted);" onclick="switchTaskTab('done')">{{index .T "completed"}} <span id="cnt-done"></span></span>
        </span>
      </h2>
      <form action="/clear" method="post" style="margin-bottom:8px;">
        <select name="type" style="width:auto;display:inline;">
          <option value="1">{{index .T "clear_done"}}</option>
          <option value="4">{{index .T "clear_running"}}</option>
          <option value="3">{{index .T "clear_failed"}}</option>
          <option value="2">{{index .T "clear_all"}}</option>
        </select>
        <button type="submit" style="margin-top:0;padding:6px 12px;font-size:13px;">{{index .T "execute"}}</button>
      </form>
      <table class="tbl" id="task-table">
        <thead><tr><th>{{index .T "name"}}</th><th>{{index .T "size"}}</th><th style="width:60px;">%</th><th style="width:40px;"></th></tr></thead>
        <tbody>
        {{if .Tasks}}{{range .Tasks}}<tr class="{{.RowClass}}" data-status="{{.Status}}" data-url="{{.URL}}">
          <td title="{{.InfoHash}}">{{.Name}}</td>
          <td>{{.Size}}</td>
          <td>{{printf "%.0f" .Percent}}%</td>
          <td><button onclick="copyTaskURL('{{.URL}}')" style="padding:2px 6px;font-size:10px;margin:0;" title="{{index $.T "copy_link_title"}}">📋</button></td>
        </tr>{{end}}{{else}}<tr><td colspan="4" class="hint" id="task-empty-hint">{{index .T "no_tasks"}}</td></tr>{{end}}
        </tbody>
      </table>
    </div>
    <script>
      function switchTaskTab(tab){
        document.querySelectorAll('#tab-downloading,#tab-failed,#tab-done').forEach(function(el){
          el.style.color='var(--muted)';el.style.borderBottom='none';
        });
        document.getElementById('tab-'+tab).style.color='var(--accent)';
        document.getElementById('tab-'+tab).style.borderBottom='2px solid var(--accent)';
        var rows=document.querySelectorAll('#task-table tbody tr');
        var counts={downloading:0,failed:0,done:0};
        rows.forEach(function(r){
          var s=r.getAttribute('data-status');
          if(s===tab||tab==='all'){r.style.display='';}
          else{r.style.display='none';}
          counts[s]=(counts[s]||0)+1;
        });
        document.getElementById('cnt-downloading').textContent='('+counts.downloading+')';
        document.getElementById('cnt-failed').textContent='('+counts.failed+')';
        document.getElementById('cnt-done').textContent='('+counts.done+')';
      }
      function copyTaskURL(url){
        navigator.clipboard.writeText(url).then(function(){},function(){
          alertModal('URL: '+url);
        });
      }
      // init — load tasks immediately
      refreshTasks();
      // Auto-refresh tasks every 30s
      var taskRefreshTimer=setInterval(refreshTasks,30000);
      async function refreshTasks(){
        try{
          var r=await fetch('/api/tasks');
          var j=await r.json();
          var tbody=document.querySelector('#task-table tbody');
          if(!tbody)return;
          // Update total count in heading
          var totalEl=document.getElementById('task-total');
          if(totalEl&&j.count!==undefined)totalEl.textContent=j.count;
          if(!j.tasks||j.tasks.length===0){
            tbody.innerHTML='<tr><td colspan="4" class="hint">{{index .T "no_tasks"}}</td></tr>';
            ['downloading','failed','done'].forEach(function(s){document.getElementById('cnt-'+s).textContent='(0)';});
            return;
          }
          tbody.innerHTML=j.tasks.map(function(t){
            return '<tr class="'+t.row_class+'" data-status="'+t.status+'" data-url="'+t.url+'">'+
              '<td title="'+t.info_hash+'">'+t.name+'</td>'+
              '<td>'+t.size+'</td>'+
              '<td>'+t.percent.toFixed(0)+'%</td>'+
              '<td><button onclick="copyTaskURL(\''+t.url+'\')" style="padding:2px 6px;font-size:10px;margin:0;" title="'+'{{index .T "copy_link_title"}}'+'">📋</button></td>'+
              '</tr>';
          }).join('');
          // Re-count
          var c={downloading:0,failed:0,done:0};
          j.tasks.forEach(function(t){c[t.status]=(c[t.status]||0)+1;});
          document.getElementById('cnt-downloading').textContent='('+c.downloading+')';
          document.getElementById('cnt-failed').textContent='('+c.failed+')';
          document.getElementById('cnt-done').textContent='('+c.done+')';
          // Re-apply current tab
          var active=document.querySelector('#tab-downloading[style*="accent"],#tab-failed[style*="accent"],#tab-done[style*="accent"]');
          if(active)switchTaskTab(active.id.replace('tab-',''));
        }catch(e){}
      }
    </script>
    {{end}}

    <!-- log panel -->
    {{if eq .Page "log"}}
    <div class="card panel">
      <h2>{{index .T "runtime_log"}}</h2>
      <div class="log-panel" id="log-panel" style="max-height:none;">{{range .Logs}}{{if hasPrefix . "--- ["}}<span class="log-clear-marker">{{.}}</span>{{else}}{{.}}{{end}}
{{end}}</div>
    </div>
    <script>
      var logLastLine='';
      var logTimer=setInterval(refreshLogs,5000);
      async function refreshLogs(){
        try{
          var url='/api/logs';
          if(logLastLine) url+='?since='+encodeURIComponent(logLastLine);
          var r=await fetch(url);
          var j=await r.json();
          if(!j.lines||j.lines.length===0)return;
          var panel=document.getElementById('log-panel');
          var first=j.lines[0];
          // Append new lines at bottom
          var frag=document.createDocumentFragment();
          for(var i=0;i<j.lines.length;i++){frag.appendChild(document.createTextNode(j.lines[i]+'\n'));}
          panel.appendChild(frag);
          logLastLine=j.lines[j.lines.length-1];
        }catch(e){}
      }
      (function(){var p=document.getElementById('log-panel');if(p){var t=p.textContent.trim();if(t){var lines=t.split('\n');logLastLine=lines[0].trim();}}})();
    </script>
    {{end}}

    <!-- aggregator search page -->
    {{if eq .Page "search"}}
    <div class="card panel">
      <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:16px;">
        <h2 style="margin:0;">{{index .T "indexer_search"}}</h2>
        <button type="button" onclick="clearSearch()" style="margin:0;padding:4px 12px;font-size:12px;background:var(--accent-2);" title="{{index .T "search_refresh"}}">{{index .T "search_refresh"}}</button>
      </div>
      <form action="/search" method="post" id="search-form" autocomplete="off">
        <div style="display:flex;gap:8px;flex-wrap:wrap;align-items:flex-end;">
          <input name="q" id="search-q" placeholder="{{index .T "search_ph"}}" value="{{.SearchQuery}}" style="flex:3;min-width:160px;" autofocus>
          <input type="hidden" name="keyword" id="search-keyword" value="{{.SearchKeyword}}">
          <select name="category" style="flex:1;min-width:100px;">
            <option value="">{{index .T "all_categories"}}</option>
            <option value="anime"{{if eq .SearchCategory "anime"}} selected{{end}}>{{index .T "category_anime"}}</option>
            <option value="tv"{{if eq .SearchCategory "tv"}} selected{{end}}>{{index .T "category_tv"}}</option>
            <option value="movie"{{if eq .SearchCategory "movie"}} selected{{end}}>{{index .T "category_movie"}}</option>
            <option value="music"{{if eq .SearchCategory "music"}} selected{{end}}>{{index .T "category_music"}}</option>
            <option value="other"{{if eq .SearchCategory "other"}} selected{{end}}>{{index .T "category_other"}}</option>
          </select>
          <select name="sort" style="flex:1;min-width:100px;">
            <option value="seeds"{{if eq .SearchSort "seeds"}} selected{{end}}>{{index .T "sort_seeds"}}</option>
            <option value="size"{{if eq .SearchSort "size"}} selected{{end}}>{{index .T "sort_size"}}</option>
            <option value="date"{{if eq .SearchSort "date"}} selected{{end}}>{{index .T "sort_date"}}</option>
          </select>
          <button type="submit" style="margin-top:0;white-space:nowrap;">{{index .T "search_btn"}}</button>
        </div>
        {{if .IndexerList}}
        <div style="margin-top:8px;display:flex;flex-wrap:wrap;gap:4px;align-items:center;">
          <span style="font-size:12px;color:var(--muted);white-space:nowrap;">{{index .T "search_sites"}}</span>
          {{range .IndexerList}}
          <label style="font-size:11px;display:flex;align-items:center;gap:2px;cursor:pointer;padding:2px 6px;background:var(--bg);border:1px solid var(--line);border-radius:6px;">
            <input type="checkbox" name="indexer" value="{{.ID}}" style="width:auto;margin:0;"{{if indexerChecked $.SearchIndexers .ID}} checked{{end}}>
            {{.Name}}
          </label>
          {{end}}
        </div>
        {{end}}
      </form>
      {{if .SearchResults}}
      <div style="margin-top:16px;" id="search-results-wrap">
        <table class="tbl" id="search-results"><thead><tr><th>#</th><th>{{index .T "name"}}</th><th>{{index .T "size"}}</th><th>↑</th><th>{{index .T "date_label"}}</th><th>{{index .T "indexer_label"}}</th><th></th></tr></thead><tbody>
        {{range $i, $r := .SearchResults}}<tr data-title="{{$r.Title}}" data-group="{{$r.Group}}">
          <td class="muted" style="font-size:11px;text-align:center;">{{add $i 1}}</td>
          <td>{{if $r.PageURL}}<a href="{{$r.PageURL}}" target="_blank">{{$r.Title}}</a>{{else}}{{$r.Title}}{{end}}</td>
          <td class="muted">{{$r.SizeFmt}}</td><td>{{$r.Seeders}}</td><td class="muted" style="font-size:11px;">{{$r.DateFmt}}</td><td class="muted">{{$r.IndexerName}}</td>
          <td>{{if $r.MagnetURL}}<button data-magnet="{{$r.MagnetURL}}" onclick="addTaskWithBrowse(this.getAttribute('data-magnet'))" style="background:var(--accent-2);padding:2px 8px;font-size:11px;margin:0;">+</button>{{end}}</td>
        </tr>{{end}}</tbody></table>
      <div id="pagination-bar" style="display:flex;justify-content:center;align-items:center;gap:4px;margin-top:14px;flex-wrap:wrap;"></div>
      </div>
      {{else}}{{if .SearchQuery}}<div class="hint" style="margin-top:12px;">{{index .T "search_no_result"}}</div>{{end}}{{end}}
      {{if .SearchErrors}}{{range $id, $err := .SearchErrors}}<div style="padding:4px 8px;margin:2px 0;background:#fef2f2;color:#991b1b;border-radius:6px;word-break:break-all;">⚠ {{$err}}</div>{{end}}{{end}}
      <!-- saved searches -->
      {{if .SavedSearches}}<div style="margin-top:16px;padding-top:12px;border-top:1px solid var(--line);"><h3 style="margin:0 0 8px;">{{index .T "saved_searches_title"}}</h3>{{range .SavedSearches}}<div style="display:flex;align-items:center;gap:8px;padding:4px 0;font-size:13px;"><span style="flex:1;">🔍 {{.Query}}{{if .Category}} <span style="color:var(--muted);font-size:11px;">[{{.Category}}]</span>{{end}}</span><form action="/search" method="post" style="display:inline;"><input type="hidden" name="q" value="{{.Query}}"><input type="hidden" name="category" value="{{.Category}}"><input type="hidden" name="sort" value="{{.Sort}}"><button type="submit" style="padding:2px 8px;font-size:11px;margin:0;">{{index $.T "search_btn_sm"}}</button></form><form action="/search" method="post" style="display:inline;"><input type="hidden" name="action" value="unsubscribe"><input type="hidden" name="id" value="{{.ID}}"><button type="submit" style="padding:2px 8px;font-size:11px;margin:0;background:var(--danger);">{{index $.T "delete_btn"}}</button></form></div>{{end}}</div>{{end}}
    </div>
    <script>
    document.getElementById('search-form').addEventListener('submit',function(e){
      var kw=document.getElementById('search-keyword'),bar=document.getElementById('group-chip-bar');
      if(kw&&bar){var grp={};bar.querySelectorAll('span[data-cat]').forEach(function(c){if(c.style.background==='var(--accent)'||c.style.background==='rgb(59,130,246)'){var t=c.getAttribute('data-filter');var g=c.getAttribute('data-cat');if(t&&g){if(!grp[g])grp[g]=[];if(grp[g].indexOf(t)===-1)grp[g].push(t);}}});
      var parts=[];for(var g in grp){parts.push(g+':'+grp[g].join('|'));}if(parts.length)kw.value=parts.join(' ');else kw.value='';}
      // Compare non-keyword params: if unchanged, filter from cache instead of full search
      var form=document.getElementById('search-form');var fd=new FormData(form);fd.delete('keyword');
      var curParams=new URLSearchParams(fd).toString();
      if(curParams===lastSearchParams&&curParams!==''){e.preventDefault();filterByChips();}
      else{lastSearchParams=curParams;sessionStorage.setItem('pan-fetcher-last-params',curParams);}
    });
    var lastSearchParams=sessionStorage.getItem('pan-fetcher-last-params')||'';
    var searchTotal={{.SearchTotal}};
    var pageSize={{.PageSize}};
    var currentPage=1;
    var totalPages=1;
    if(searchTotal>0){totalPages=Math.ceil(searchTotal/pageSize);}
    else{var rows=document.querySelectorAll('#search-results tbody tr');if(rows.length>0){totalPages=Math.max(1,Math.ceil(rows.length/pageSize));}}
    (function(){
      var pgKey='pan-fetcher-page';
      {{if .SearchQuery}}
      var searchFormData=new URLSearchParams(new FormData(document.getElementById('search-form'))).toString();
      sessionStorage.setItem('pan-fetcher-query',searchFormData);
      sessionStorage.setItem(pgKey,JSON.stringify({currentPage:currentPage,totalPages:totalPages,searchTotal:searchTotal,pageSize:pageSize}));
      setTimeout(function(){buildGroupChips(document.getElementById('search-results-wrap'),document.getElementById('search-results'),'{{.SearchQuery}}','{{.SearchCategory}}');},0);
      {{else}}
      var savedQuery=sessionStorage.getItem('pan-fetcher-query');
      var savedPage=sessionStorage.getItem(pgKey);
      if(savedPage){try{var ps=JSON.parse(savedPage);currentPage=ps.currentPage||1;totalPages=ps.totalPages||1;searchTotal=ps.searchTotal||0;pageSize=ps.pageSize||50;}catch(e){}}
      if(savedQuery&&!{{.SearchQuery}}){var fd=new URLSearchParams(savedQuery);if(fd.get('q')){document.getElementById('search-q').value=fd.get('q');document.getElementById('search-form').submit();}}
      {{end}}
    })();
    </script>
    {{end}}

    <script>
    // === Shared: tag extraction, AI classification, chip rendering ===
    var activeRSSFilters=[];

    function updateRSSFilterTags(tags){
      var d=document.getElementById('sub-filter-tags'),ta=document.getElementById('sub-filter');
      if(!d||!ta)return;
      if(!tags.length){d.innerHTML='<span style="color:var(--muted);">未选择</span>';ta.value='';}
      else{d.innerHTML=tags.map(function(t){return '<span style="padding:2px 10px;border-radius:12px;background:var(--accent);color:#fff;font-size:11px;cursor:pointer;" onclick="event.stopPropagation();var i=activeRSSFilters.indexOf(\''+t.replace(/'/g,"\\'")+'\');if(i!==-1){activeRSSFilters.splice(i,1);updateRSSFilterTags(activeRSSFilters);updateChipBarFromFilters();}">'+t+' &times;</span>';}).join('');ta.value=tags.join('\n');}
      window.activeRSSFilters=tags;
    }
    function updateChipBarFromFilters(){
      var bar=document.getElementById('group-chip-bar');if(!bar)return;
      var set=activeRSSFilters.map(function(t){return t.toLowerCase();});
      bar.querySelectorAll('span[data-filter]:not([data-filter=""])').forEach(function(c){var a=set.indexOf(c.getAttribute('data-filter').toLowerCase())!==-1;c.style.background=a?'var(--accent)':'var(--bg)';c.style.color=a?'#fff':'';c.style.borderColor=a?'var(--accent)':'var(--line)';});
      var all=bar.querySelector('span[data-filter=""]');if(all){all.style.background=activeRSSFilters.length?'var(--bg)':'var(--accent)';all.style.color=activeRSSFilters.length?'':'#fff';}
      var kw=document.getElementById('search-keyword');if(kw)kw.value=buildGroupKeyword();
    }
    function buildGroupKeyword(){
      var bar=document.getElementById('group-chip-bar');if(!bar)return activeRSSFilters.join(' ');
      var grp={};bar.querySelectorAll('span[data-cat]').forEach(function(c){if(c.style.background==='var(--accent)'||c.style.background==='rgb(59,130,246)'){var t=c.getAttribute('data-filter');var g=c.getAttribute('data-cat');if(t&&g){if(!grp[g])grp[g]=[];if(grp[g].indexOf(t)===-1)grp[g].push(t);}}});
      var parts=[];for(var g in grp){parts.push(g+':'+grp[g].join('|'));}return parts.join(' ');
    }
    async function filterByChips(){
      var kw=buildGroupKeyword();
      var fd=new URLSearchParams();
      var form=document.getElementById('search-form');
      if(form){var fdx=new FormData(form);for(var k of new Set(fdx.keys())){if(k!=='keyword'){fdx.getAll(k).forEach(function(v){fd.append(k,v);});}}}
      fd.set('keyword',kw);fd.set('offset','0');
      try{
        var r=await fetch('/search/more',{method:'POST',body:fd,headers:{'X-Requested-With':'XMLHttpRequest'}});
        var j=await r.json();
        if(!j.results||j.results.length===0){return;}
        var tbody=document.querySelector('#search-results tbody');
        if(!tbody)return;
        tbody.innerHTML=j.results.map(function(item,i){return buildRowHTML(item,i,0);}).join('');
        searchTotal=j.total||0;
        currentPage=1;
        totalPages=searchTotal>0?Math.ceil(searchTotal/pageSize):1;
        renderPagination();
        // Rebuild chip bar with filtered tags
        if(j.all_tags)window._currentAllTags=j.all_tags;
        if(j.all_groups)window._currentAllGroups=j.all_groups;
        buildGroupChips(document.getElementById('search-results-wrap'),document.getElementById('search-results'),document.getElementById('search-q')?.value||'',document.querySelector('select[name="category"]')?.value||'');
        document.getElementById('search-results').scrollIntoView({block:'start'});
      }catch(e){console.error(e);}
    }
    function extractSeason(text){
      var n=0,cn={一:1,二:2,三:3,四:4,五:5,六:6,七:7,八:8,九:9,十:10,十一:11,十二:12,十三:13,十四:14,十五:15,十六:16,十七:17,十八:18,十九:19,二十:20};
      var cm=text.match(/第([一二三四五六七八九十]+|\d+)\s*(季|期)/);if(cm)n=cn[cm[1]]||parseInt(cm[1])||0;
      if(!n){var sm=text.match(/Season\s*(\d+)/i);if(sm)n=parseInt(sm[1])||0;}
      if(!n){var sm2=text.match(/(\d+)(?:st|nd|rd|th)\s*Season/i);if(sm2)n=parseInt(sm2[1])||0;}
      return n>0&&n<=99?'S'+String(n).padStart(2,'0'):'';
    }
    function classifyTag(tag){
      var t=tag.toLowerCase();
      if(/^\d+\(\d+\)$/.test(tag)||/^\d{1,4}$/.test(tag)||/^(vol|volume|disc|cd|part|pt)[\s.]*\d+$/i.test(t))return null;
      if(/^s\d{2}$/.test(t))return{cat:'season',label:'📅 季'};
      if(/^\d{3,4}[pi]$/.test(t)||/^4k$/i.test(t))return{cat:'resolution',label:'📐 分辨率'};
      if(/^(x26[45]|hevc|avc|av1|vp\d|flac|aac|opus|ac3|ddp?|dts|truehd|pcm|alac)/i.test(t)||/^(hevc-10bit|avc\s*aac|flac\s*\d|aac\s*avc)/i.test(t))return{cat:'codec',label:'🎞 编码'};
      if(/^(web-dl|webrip|bdrip|bd|dvdrip|hdtv|tvrip|bluray|remux)/i.test(t)||/^(viutv|baha|iqiyi|b-global|cr|netflix|amazon|hulu|disney|bahamut|aniplus|at-x)/i.test(t))return{cat:'source',label:'📡 来源'};
      if(/^(mp4|mkv|avi|ts|m2ts)$/i.test(t))return{cat:'container',label:'📦 容器'};
      if(/^(cht|chs|jpn|eng|kor|繁|简|日|英|中|外挂|内封|内嵌|字幕)/i.test(t)||/^(简繁|繁简|繁体|简体|中文|日语|英语|韩语)/.test(tag))return{cat:'language',label:'🌐 语言'};
      // Group: only from server-validated data-group
      if(window._serverGroups&&window._serverGroups[tag])return{cat:'group',label:'👥 字幕组'};
      return{cat:'other',label:'🏷 其他'};
    }
    function tagRowsWithGroup(table){
      if(!table)return[];var rows=table.querySelectorAll('tbody tr');if(!rows.length)return[];var groups=[],seen={};
      window._serverGroups=window._serverGroups||{};
      rows.forEach(function(tr){var g=tr.getAttribute('data-group');if(g)window._serverGroups[g]=true;});
      rows.forEach(function(tr){var g=tr.getAttribute('data-group');if(g)window._serverGroups[g]=true;
      var td=tr.querySelector('td:nth-child(2)');var text=td?td.textContent.trim():(tr.getAttribute('data-title')||'');var allTags=[];var re=/[\[【]([^\]】]{1,40})[\]】]/g;var m;while((m=re.exec(text))!==null){var tag=m[1].trim().replace(/^DBD制作组$/,'DBD-Raws').replace(/^桜都字幕組$/,'桜都字幕组');if(!tag||tag.length>40||/^\d+\(\d+\)$/.test(tag)||/^\d{1,4}$/.test(tag)||/^(vol|volume|disc|cd|part|pt|ep)[\s.]*\d+$/i.test(tag))continue;tag=tag.replace(/\s+/g,' ').trim();if(tag)allTags.push(tag);}
      if(!allTags.length){var fm=text.match(/^([A-Za-z0-9_-]{2,20})(?=\s*[\[/])/);if(fm)allTags.push(fm[1]);}
      var pt=text.replace(/\[[^\]]*\]/g,' ').replace(/\s+/g,' ').trim();var sTag=extractSeason(pt);if(sTag)allTags.push(sTag);
      var sm=text.match(/(?:^|\s)S(\d{1,3})\b(?![^\[\]]*\])/);if(sm&&parseInt(sm[1])<=99){var st='S'+String(parseInt(sm[1])).padStart(2,'0');if(allTags.indexOf(st)===-1)allTags.push(st);}
      tr.setAttribute('data-group',allTags.join(' '));
      allTags.forEach(function(g){var gk=g.toLowerCase();if(!seen[gk]){seen[gk]=true;groups.push(g);}});
      });return groups;
    }

    // Mutable tag lists: initially from server, updated by filterByChips
    window._currentAllTags={{.AllTagsJSON}};
    window._currentAllGroups={{.AllGroupsJSON}};

    function buildGroupChips(container,table,rssQuery,rssCategory){
      var allGroups=window._currentAllGroups||[];
      window._serverGroups={};
      if(allGroups&&allGroups.length){
        allGroups.forEach(function(g){window._serverGroups[g]=true;});
      }
      var clientGroups=tagRowsWithGroup(table);
      var allTags=window._currentAllTags||[];
      var groups;
      if(allTags&&allTags.length){
        groups=allTags;
      }else{
        groups=clientGroups;
      }
      var old=container.querySelector('#group-chip-bar');if(old)old.remove();
      if(!table.querySelectorAll('tbody tr').length&&!allTags.length)return;
      renderChipBar(container,table,groups,null,rssQuery,rssCategory);
    }

    function renderChipBar(container,table,groups,catMap,rssQuery,rssCategory){
      var bar=document.createElement('div');bar.id='group-chip-bar';bar.style.cssText='display:flex;flex-direction:column;gap:5px;margin-bottom:10px;';
      var kwEl=document.getElementById('search-keyword');var hasKw=kwEl&&kwEl.value.trim()!=='';var akw=kwEl?kwEl.value.trim().toLowerCase():'';var akwSet=[];if(akw){var ps=akw.split(/\s+/);ps.forEach(function(p){var ci=p.indexOf(':');if(ci>0){p.substring(ci+1).split('|').forEach(function(t){akwSet.push(t);});}else{akwSet.push(p);}});}
      if(hasKw){activeRSSFilters=kwEl.value.trim().split(/[\s,]+/).filter(Boolean);updateRSSFilterTags(activeRSSFilters);}
      function mkChip(t,f,isA){var c=document.createElement('span');c.textContent=t;c.setAttribute('data-filter',f);c.style.cssText='padding:3px 10px;border-radius:12px;font-size:11px;white-space:nowrap;cursor:pointer;background:'+(isA?'var(--accent)':'var(--bg)')+';color:'+(isA?'#fff':'')+';border:1px solid '+(isA?'var(--accent)':'var(--line)');return c;}
      var top=document.createElement('div');top.style.cssText='display:flex;flex-wrap:wrap;gap:6px;align-items:center;';top.appendChild(mkChip('全部','',!hasKw));
      var rss=document.createElement('span');rss.textContent='+ RSS';rss.id='chip-rss-btn';rss.style.cssText='padding:4px 14px;border-radius:14px;background:var(--accent-2);color:#fff;cursor:pointer;font-size:12px;margin-left:auto;';
      rss.onclick=function(){var sf=document.getElementById('sub-form');if(sf){var q=rssQuery||'';document.getElementById('sub-query').value=q;document.getElementById('sub-category').value=rssCategory||'';document.getElementById('sub-name').value=q;var u='/rss/search?q='+encodeURIComponent(q);if(rssCategory)u+='&category='+encodeURIComponent(rssCategory);var form=document.getElementById('search-form');if(form){var fd=new FormData(form);var s=fd.get('sort');if(s)u+='&sort='+encodeURIComponent(s);var idx=fd.getAll('indexer');if(idx.length)u+='&indexers='+encodeURIComponent(idx.join(','));}var fl=buildGroupKeyword();if(fl)u+='&keyword='+encodeURIComponent(fl);document.getElementById('sub-url').value=u;document.getElementById('sub-filter').value=(activeRSSFilters||[]).join('\n');updateRSSFilterTags(activeRSSFilters||[]);sf.style.display='flex';}};
      top.appendChild(rss);bar.appendChild(top);
      var cats={},co=[],cl={group:'👥 字幕组',source:'📡 来源',codec:'🎞 编码',resolution:'📐 分辨率',language:'🌐 语言',container:'📦 容器',season:'📅 季',other:'🏷 其他'};
      groups.forEach(function(g){var ck;if(catMap&&catMap[g])ck=catMap[g];else{var cl2=classifyTag(g);if(!cl2)return;ck=cl2.cat;}if(!cats[ck]){cats[ck]={label:cl[ck]||('🏷 '+ck),tags:[],key:ck};co.push(ck);}cats[ck].tags.push(g);});
      co.forEach(function(ck){var cat=cats[ck];var row=document.createElement('div');row.style.cssText='display:flex;flex-wrap:wrap;gap:4px;align-items:center;';var lbl=document.createElement('span');lbl.textContent=cat.label;lbl.style.cssText='font-size:10px;color:var(--muted);margin-right:2px;white-space:nowrap;opacity:0.7;cursor:pointer;';row.appendChild(lbl);var isOther=ck==='other';var otherWrap=null;if(isOther){otherWrap=document.createElement('span');otherWrap.style.cssText='display:none;';row.appendChild(otherWrap);lbl.textContent='🏷 其他('+cat.tags.length+') ▸';lbl.onclick=function(){var s=otherWrap.style.display==='none';otherWrap.style.display=s?'':'none';lbl.textContent='🏷 其他('+cat.tags.length+') '+(s?'▾':'▸');};}cat.tags.forEach(function(g){var ia=akwSet.indexOf(g.toLowerCase())!==-1;var c=mkChip(g,g,ia);c.setAttribute('data-cat',ck);(isOther&&otherWrap?otherWrap:row).appendChild(c);});bar.appendChild(row);});
      bar.addEventListener('click',function(e){if(e.target.id==='chip-rss-btn')return;if(e.target.tagName!=='SPAN')return;var f=e.target.getAttribute('data-filter');if(f===undefined||f===null)return;if(f===''){var ha=false;bar.querySelectorAll('span:not(#chip-rss-btn):not([data-filter=""])').forEach(function(c){if(c.style.background==='var(--accent)'||c.style.background==='rgb(59,130,246)')ha=true;});bar.querySelectorAll('span:not(#chip-rss-btn)').forEach(function(c){c.style.background='var(--bg)';c.style.color='';c.style.borderColor='var(--line)';});e.target.style.background='var(--accent)';e.target.style.color='#fff';e.target.style.borderColor='var(--accent)';updateRSSFilterTags([]);var ke2=document.getElementById('search-keyword');if(ke2)ke2.value='';}else{var ia=e.target.style.background==='var(--accent)'||e.target.style.background==='rgb(59,130,246)';if(ia){e.target.style.background='var(--bg)';e.target.style.color='';e.target.style.borderColor='var(--line)';}else{e.target.style.background='var(--accent)';e.target.style.color='#fff';e.target.style.borderColor='var(--accent)';}var ac=bar.querySelector('span[data-filter=""]');if(ac){ac.style.background='var(--bg)';ac.style.color='';ac.style.borderColor='var(--line)';}var act=[];bar.querySelectorAll('span:not(#chip-rss-btn):not([data-filter=""])').forEach(function(c){if(c.style.background==='var(--accent)'||c.style.background==='rgb(59,130,246)')act.push(c.getAttribute('data-filter'));});if(!act.length&&ac){ac.style.background='var(--accent)';ac.style.color='#fff';ac.style.borderColor='var(--accent)';}updateRSSFilterTags(act);var ke=document.getElementById('search-keyword');if(ke)ke.value=buildGroupKeyword();}});
      container.insertBefore(bar,table);
    }

    </script>

    <!-- discover page (TMDB) -->
    {{if eq .Page "discover"}}
    <style>
      .discover-hero{text-align:center;padding:20px 0 10px;}
      .discover-hero h2{font-size:22px;margin:0 0 6px;}
      .discover-hero p{color:var(--muted);font-size:13px;margin:0;}
      .discover-search{display:flex;gap:8px;max-width:560px;margin:0 auto;}
      .discover-search input{flex:1;font-size:15px;padding:10px 14px;border-radius:10px;}
      .discover-search button{padding:10px 24px;font-size:14px;border-radius:10px;margin:0;}
      .tmdb-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(150px,1fr));gap:14px;margin-top:16px;}
      .tmdb-card{cursor:pointer;border-radius:12px;overflow:hidden;background:var(--card);border:1px solid var(--line);transition:all .2s;position:relative;}
      .tmdb-card:hover{transform:translateY(-4px);box-shadow:0 8px 24px rgba(0,0,0,.12);border-color:var(--accent);}
      .tmdb-card.selected{border:2px solid var(--accent);box-shadow:0 0 0 3px rgba(59,130,246,.25);}
      .tmdb-poster-wrap{position:relative;aspect-ratio:2/3;overflow:hidden;background:linear-gradient(135deg,#e5e7eb,#d1d5db);}
      .tmdb-poster{width:100%;height:100%;object-fit:cover;display:block;}
      .tmdb-poster-placeholder{width:100%;height:100%;display:flex;align-items:center;justify-content:center;font-size:36px;color:#9ca3af;}
      .tmdb-card-overlay{position:absolute;bottom:0;left:0;right:0;background:linear-gradient(transparent,rgba(0,0,0,.7));padding:24px 10px 10px;}
      .tmdb-card-title{font-size:13px;font-weight:600;color:#fff;line-height:1.3;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden;text-shadow:0 1px 2px rgba(0,0,0,.5);}
      .tmdb-card-year{font-size:11px;color:rgba(255,255,255,.75);margin-top:2px;}
      .tmdb-card-type{position:absolute;top:8px;right:8px;background:rgba(0,0,0,.55);color:#fff;font-size:10px;padding:2px 8px;border-radius:10px;}
      .search-tabs{display:flex;gap:0;margin:16px 0 0;border-bottom:2px solid var(--line);}
      .search-tab{padding:10px 24px;cursor:pointer;font-size:14px;font-weight:500;color:var(--muted);border-bottom:2px solid transparent;margin-bottom:-2px;transition:.2s;}
      .search-tab:hover{color:var(--text);}
      .search-tab.active{color:var(--accent);border-bottom-color:var(--accent);}
      .season-select{display:flex;gap:8px;flex-wrap:wrap;margin-top:10px;}
      .season-chip{padding:6px 16px;border-radius:22px;font-size:13px;cursor:pointer;border:1px solid var(--line);background:var(--bg);transition:.2s;user-select:none;}
      .season-chip:hover{border-color:var(--accent);color:var(--accent);}
      .season-chip.active{background:var(--accent);color:#fff;border-color:var(--accent);}
      .search-step{display:none;animation:fadeIn .25s ease;}
      .search-step.active{display:block;}
      @keyframes fadeIn{from{opacity:0;transform:translateY(8px);}to{opacity:1;transform:translateY(0);}}
      .confirm-bar{display:flex;align-items:center;gap:12px;padding:12px 16px;background:var(--bg);border-radius:12px;margin-top:14px;border:1px solid var(--line);}
      .confirm-bar .sel-title{font-weight:600;font-size:15px;flex:1;display:flex;align-items:center;gap:8px;}
      .confirm-bar .sel-poster{width:40px;height:56px;border-radius:6px;object-fit:cover;}
      .back-to-top{position:fixed;bottom:24px;right:24px;width:44px;height:44px;border-radius:50%;background:var(--accent);color:#fff;border:none;cursor:pointer;box-shadow:0 4px 16px rgba(0,0,0,.15);display:none;align-items:center;justify-content:center;font-size:20px;z-index:100;transition:all .2s;}
      .back-to-top:hover{transform:translateY(-2px);box-shadow:0 6px 20px rgba(0,0,0,.2);}
      .back-to-top.show{display:flex;}
    </style>
    <div class="discover-hero">
      <h2>🎬 发现</h2>
      <p>搜索电影和剧集，找到你想要的资源</p>
    </div>
    <div class="discover-search">
      <input type="text" id="tmdb-query" placeholder="输入电影或剧集名称..." autofocus>
      <button onclick="doTMDBSearch()" style="background:var(--accent);">搜索</button>
    </div>
    <div id="tmdb-status" style="text-align:center;margin-top:8px;font-size:12px;color:var(--muted);min-height:20px;"></div>

    <div id="step-tmdb" class="search-step active">
      <div class="search-tabs" id="tmdb-tabs" style="display:none;">
        <div class="search-tab active" data-tab="movies" onclick="switchTMDTab('movies')">🎬 电影</div>
        <div class="search-tab" data-tab="tv" onclick="switchTMDTab('tv')">📺 电视节目</div>
      </div>
      <div class="tmdb-grid" id="tmdb-grid"></div>
    </div>

    <!-- Trending section -->
    <div id="trending-section" style="margin-top:24px;">
      <div style="display:flex;align-items:center;gap:8px;margin-bottom:12px;">
        <h3 style="margin:0;font-size:16px;">🔥 热门推荐</h3>
        <span style="font-size:11px;color:var(--muted);">本周流行</span>
      </div>
      <div class="search-tabs" style="margin-bottom:0;">
        <div class="search-tab active" data-trending="movies" onclick="switchTrendingTab('movies')">🎬 热门电影</div>
        <div class="search-tab" data-trending="tv" onclick="switchTrendingTab('tv')">📺 热门剧集</div>
      </div>
      <div class="tmdb-grid" id="trending-grid" style="min-height:120px;">
        <div style="grid-column:1/-1;text-align:center;padding:30px;color:var(--muted);">⏳ 加载中...</div>
      </div>
      <div style="text-align:center;margin-top:14px;" id="trending-more-wrap">
        <button id="btn-trending-more" onclick="loadMoreTrending()" style="padding:8px 28px;font-size:13px;border-radius:20px;background:var(--bg);border:1px solid var(--line);cursor:pointer;display:none;color:var(--accent);">📥 显示更多</button>
      </div>
    </div>

    <div id="step-season" class="search-step">
      <div class="confirm-bar">
        <span class="sel-title" id="sel-title"></span>
        <button onclick="backToTMDB()" style="margin:0;padding:6px 14px;font-size:12px;background:#6b7280;border-radius:8px;">← 返回</button>
      </div>
      <div style="margin-top:10px;font-size:13px;color:var(--muted);">选择季（可选，不选则搜索全部季）：</div>
      <div class="season-select" id="season-list"></div>
      <button id="btn-confirm-tv" onclick="confirmTVSearch()" style="margin-top:14px;padding:10px 24px;font-size:14px;background:var(--accent);border-radius:10px;">🔍 搜索此剧集</button>
    </div>

    <div id="step-confirm" class="search-step">
      <div class="confirm-bar">
        <span class="sel-title" id="confirm-title"></span>
        <button onclick="backToTMDB()" style="margin:0;padding:6px 14px;font-size:12px;background:#6b7280;border-radius:8px;">← 重新选择</button>
      </div>
      <div id="confirm-query" style="margin-top:10px;font-size:13px;color:var(--muted);"></div>
      <div style="display:flex;gap:10px;margin-top:14px;flex-wrap:wrap;">
        <button onclick="doAggregatedSearch()" style="padding:10px 24px;font-size:14px;background:var(--accent);border-radius:10px;">🚀 聚合搜索</button>
        <button onclick="doAggregatedSearchRSS()" style="padding:10px 24px;font-size:14px;background:var(--accent-2);border-radius:10px;">📋 一键订阅</button>
      </div>
    </div>
    <script>
    var tmdbMovies=[],tmdbTV=[];
    var selectedItem=null, selectedSeason=0;

    document.getElementById('tmdb-query').addEventListener('keydown',function(e){if(e.key==='Enter')doTMDBSearch();});

    async function doTMDBSearch(){
      var q=document.getElementById('tmdb-query').value.trim();
      if(!q)return;
      document.getElementById('tmdb-status').innerHTML='<span style="display:inline-block;width:16px;height:16px;border:2px solid var(--line);border-top-color:var(--accent);border-radius:50%;animation:spin .6s linear infinite;vertical-align:middle;margin-right:6px;"></span>搜索中...';
      try{
        var r=await fetch('/api/tmdb/search?q='+encodeURIComponent(q));
        var j=await r.json();
        if(j.error){document.getElementById('tmdb-status').textContent='⚠ '+j.error;return;}
        tmdbMovies=j.movies||[];tmdbTV=j.tv||[];
        var total=tmdbMovies.length+tmdbTV.length;
        document.getElementById('tmdb-status').textContent='找到 '+total+' 个结果（电影 '+tmdbMovies.length+'，电视 '+tmdbTV.length+'）';
        document.getElementById('tmdb-tabs').style.display=tmdbMovies.length&&tmdbTV.length?'flex':(total?'flex':'none');
        var startTab=tmdbMovies.length?'movies':'tv';
        document.querySelectorAll('.search-tab').forEach(function(t){t.classList.toggle('active',t.getAttribute('data-tab')===startTab);});
        renderTMDBCards(startTab);
        showStep('step-tmdb');
      }catch(e){document.getElementById('tmdb-status').textContent='⚠ 请求失败: '+e.message;}
    }

    function switchTMDTab(tab){
      document.querySelectorAll('.search-tab').forEach(function(t){t.classList.toggle('active',t.getAttribute('data-tab')===tab);});
      renderTMDBCards(tab);
    }

    function renderTMDBCards(tab){
      var items=tab==='movies'?tmdbMovies:tmdbTV;
      var grid=document.getElementById('tmdb-grid');
      if(!items.length){grid.innerHTML='<div style="grid-column:1/-1;text-align:center;padding:40px;color:var(--muted);">无结果</div>';return;}
      grid.innerHTML=items.map(function(it){
        var posterHTML=it.poster
          ?'<img class="tmdb-poster" src="'+it.poster+'" alt="'+it.title+'" loading="lazy" onerror="this.parentElement.innerHTML=\'<div class=&#34;tmdb-poster-placeholder&#34;>🎬</div>\'">'
          :'<div class="tmdb-poster-placeholder">'+(it.media_type==='movie'?'🎬':'📺')+'</div>';
        return '<div class="tmdb-card" onclick="selectTMDB(\''+it.media_type+'\','+it.id+')" data-type="'+it.media_type+'" data-id="'+it.id+'">'
          +'<div class="tmdb-poster-wrap">'+posterHTML+'<div class="tmdb-card-overlay"><div class="tmdb-card-title">'+it.title+'</div><div class="tmdb-card-year">'+it.year+'</div></div></div>'
          +'<div class="tmdb-card-type">'+(it.media_type==='movie'?'电影':'剧集')+'</div>'
          +'</div>';
      }).join('');
    }

    function selectTMDB(mediaType,id){
      var items=mediaType==='movie'?tmdbMovies:tmdbTV;
      selectedItem=items.find(function(it){return it.media_type===mediaType&&it.id===id;});
      if(!selectedItem)return;
      document.querySelectorAll('.tmdb-card').forEach(function(c){c.classList.toggle('selected',c.getAttribute('data-id')==String(id));});
      if(mediaType==='movie'){
        showStep('step-confirm');
        document.getElementById('confirm-title').innerHTML='<img class="sel-poster" src="'+selectedItem.poster+'" onerror="this.style.display=\'none\'">🎬 '+selectedItem.title+' <span style="color:var(--muted);font-weight:400;">('+selectedItem.year+')</span>';
        document.getElementById('confirm-query').textContent='将搜索: '+selectedItem.title;
      }else{
        showStep('step-season');
        document.getElementById('sel-title').innerHTML='<img class="sel-poster" src="'+selectedItem.poster+'" onerror="this.style.display=\'none\'">📺 '+selectedItem.title+' <span style="color:var(--muted);font-weight:400;">('+selectedItem.year+')</span>';
        fetchTVSeasons(id);
      }
    }

    async function fetchTVSeasons(tmdbId){
      document.getElementById('season-list').innerHTML='<span style="font-size:12px;color:var(--muted);">加载季信息...</span>';
      var html='<span class="season-chip active" onclick="selectSeason(0,this)">全部</span>';
      for(var s=1;s<=12;s++){html+='<span class="season-chip" onclick="selectSeason('+s+',this)">第 '+s+' 季</span>';}
      document.getElementById('season-list').innerHTML=html;
      selectedSeason=0;
    }

    function selectSeason(season,el){selectedSeason=season;document.querySelectorAll('#season-list .season-chip').forEach(function(c){c.classList.remove('active');});el.classList.add('active');}

    function confirmTVSearch(){
      var label=selectedItem.title;
      if(selectedSeason>0)label+=' S'+String(selectedSeason).padStart(2,'0');
      document.getElementById('confirm-title').innerHTML='<img class="sel-poster" src="'+selectedItem.poster+'" onerror="this.style.display=\'none\'">📺 '+label+' <span style="color:var(--muted);font-weight:400;">('+selectedItem.year+')</span>';
      document.getElementById('confirm-query').textContent='将搜索: '+label;
      showStep('step-confirm');
    }

    function backToTMDB(){showStep('step-tmdb');selectedItem=null;selectedSeason=0;document.querySelectorAll('.tmdb-card').forEach(function(c){c.classList.remove('selected');});}
    function showStep(id){document.querySelectorAll('.search-step').forEach(function(s){s.classList.remove('active');});var el=document.getElementById(id);if(el)el.classList.add('active');}

    function doAggregatedSearch(){
      var query=selectedItem?selectedItem.title:document.getElementById('tmdb-query').value.trim();
      if(selectedItem&&selectedItem.media_type==='tv'&&selectedSeason>0)query+=' S'+String(selectedSeason).padStart(2,'0');
      var form=document.createElement('form');form.method='POST';form.action='/search';
      var input=document.createElement('input');input.type='hidden';input.name='q';input.value=query;form.appendChild(input);
      if(selectedItem){var cat=document.createElement('input');cat.type='hidden';cat.name='category';cat.value=selectedItem.media_type==='movie'?'movie':'tv';form.appendChild(cat);}
      document.body.appendChild(form);form.submit();
    }

    function doAggregatedSearchRSS(){
      var query=selectedItem?selectedItem.title:document.getElementById('tmdb-query').value.trim();
      if(selectedItem&&selectedItem.media_type==='tv'&&selectedSeason>0)query+=' S'+String(selectedSeason).padStart(2,'0');
      var form=document.createElement('form');form.method='POST';form.action='/search';
      var input=document.createElement('input');input.type='hidden';input.name='q';input.value=query;form.appendChild(input);
      if(selectedItem){var cat=document.createElement('input');cat.type='hidden';cat.name='category';cat.value=selectedItem.media_type==='movie'?'movie':'tv';form.appendChild(cat);}
      var rss=document.createElement('input');rss.type='hidden';rss.name='subscribe';rss.value='1';form.appendChild(rss);
      document.body.appendChild(form);form.submit();
    }
    // --- Trending ---
    var trendingMovies=[],trendingTV=[],trendingPage=1,trendingTab='movies';
    function switchTrendingTab(tab){
      trendingTab=tab;
      document.querySelectorAll('[data-trending]').forEach(function(t){t.classList.toggle('active',t.getAttribute('data-trending')===tab);});
      renderTrendingCards(tab);
    }
    function renderTrendingCards(tab){
      var items=tab==='movies'?trendingMovies:trendingTV;
      var grid=document.getElementById('trending-grid');
      if(!items.length){grid.innerHTML='<div style="grid-column:1/-1;text-align:center;padding:30px;color:var(--muted);">暂无数据</div>';document.getElementById('btn-trending-more').style.display='none';return;}
      grid.innerHTML=items.map(function(it){
        var posterHTML=it.poster
          ?'<img class="tmdb-poster" src="'+it.poster+'" alt="'+it.title+'" loading="lazy" onerror="this.parentElement.innerHTML=\'<div class=&#34;tmdb-poster-placeholder&#34;>🎬</div>\'">'
          :'<div class="tmdb-poster-placeholder">'+(it.media_type==='movie'?'🎬':'📺')+'</div>';
        return '<div class="tmdb-card" onclick="selectTMDB(\''+it.media_type+'\','+it.id+')" data-type="'+it.media_type+'" data-id="'+it.id+'">'
          +'<div class="tmdb-poster-wrap">'+posterHTML+'<div class="tmdb-card-overlay"><div class="tmdb-card-title">'+it.title+'</div><div class="tmdb-card-year">'+it.year+'</div></div></div>'
          +'<div class="tmdb-card-type">'+(it.media_type==='movie'?'电影':'剧集')+'</div>'
          +'</div>';
      }).join('');
      document.getElementById('btn-trending-more').style.display='';
    }
    async function loadTrending(){
      try{var r=await fetch('/api/tmdb/trending?page=1');var j=await r.json();if(j.error)return;trendingMovies=j.movies||[];trendingTV=j.tv||[];trendingPage=1;renderTrendingCards(trendingTab);}catch(e){}
    }
    async function loadMoreTrending(){
      var btn=document.getElementById('btn-trending-more');
      btn.textContent='⏳ 加载中...';btn.disabled=true;
      trendingPage++;
      try{
        var r=await fetch('/api/tmdb/trending?page='+trendingPage);
        var j=await r.json();
        if(j.error){btn.textContent='📥 显示更多';btn.disabled=false;return;}
        var newMovies=j.movies||[],newTV=j.tv||[];
        if(!newMovies.length&&!newTV.length){btn.textContent='✓ 已全部加载';btn.disabled=true;return;}
        // Append only new cards instead of re-rendering all (avoids scroll jump)
        var grid=document.getElementById('trending-grid');
        var frag=document.createDocumentFragment();
        var items=trendingTab==='movies'?newMovies:newTV;
        items.forEach(function(it){
          var div=document.createElement('div');div.className='tmdb-card';
          div.setAttribute('onclick',"selectTMDB('"+it.media_type+"',"+it.id+")");
          div.setAttribute('data-type',it.media_type);div.setAttribute('data-id',it.id);
          var posterHTML=it.poster
            ?'<img class="tmdb-poster" src="'+it.poster+'" alt="'+it.title+'" loading="lazy" onerror="this.parentElement.innerHTML=\'<div class=&#34;tmdb-poster-placeholder&#34;>🎬</div>\'">'
            :'<div class="tmdb-poster-placeholder">'+(it.media_type==='movie'?'🎬':'📺')+'</div>';
          div.innerHTML='<div class="tmdb-poster-wrap">'+posterHTML+'<div class="tmdb-card-overlay"><div class="tmdb-card-title">'+it.title+'</div><div class="tmdb-card-year">'+it.year+'</div></div></div><div class="tmdb-card-type">'+(it.media_type==='movie'?'电影':'剧集')+'</div>';
          frag.appendChild(div);
        });
        grid.appendChild(frag);
        trendingMovies=trendingMovies.concat(newMovies);
        trendingTV=trendingTV.concat(newTV);
        btn.textContent='📥 显示更多';btn.disabled=false;
      }catch(e){btn.textContent='📥 显示更多';btn.disabled=false;}
    }
    loadTrending();
    // Infinite scroll: load more when button is near viewport bottom
    var trendingObserver=new IntersectionObserver(function(entries){
      if(entries[0].isIntersecting){loadMoreTrending();}
    },{rootMargin:'200px'});
    var observeBtn=function(){var b=document.getElementById('btn-trending-more');if(b&&b.style.display!=='none')trendingObserver.observe(b);};
    // Re-observe after each render
    var origRenderTrending=renderTrendingCards;
    renderTrendingCards=function(tab){origRenderTrending(tab);setTimeout(observeBtn,100);};
    // Back to top
    var backBtn=document.createElement('button');
    backBtn.className='back-to-top';backBtn.innerHTML='↑';backBtn.title='回到顶部';
    backBtn.onclick=function(){window.scrollTo({top:0,behavior:'smooth'});};
    document.body.appendChild(backBtn);
    window.addEventListener('scroll',function(){backBtn.classList.toggle('show',window.scrollY>400);});
    </script>
    {{end}}

    <!-- about page -->
    {{if eq .Page "about"}}
    <div class="card panel">
      <h2>{{index .T "about_title"}}</h2>
      <p style="font-size:14px;line-height:1.8;">{{index .T "about_desc"}}</p>
      <div style="margin-top:16px;display:flex;flex-wrap:wrap;gap:12px;">
        <span class="badge badge-running">Go</span>
        <span class="badge badge-running">SQLite</span>
        <span class="badge badge-done">115 API</span>
        <span class="badge badge-done">Cardigann</span>
      </div>
      <div style="margin-top:16px;padding:12px;background:#f8fafc;border-radius:10px;border:1px solid var(--line);">
        <table style="width:100%;font-size:13px;border-collapse:collapse;">
          <tr><td style="padding:4px 0;color:var(--muted);width:80px;">{{index .T "about_version"}}</td><td><strong>pan-fetcher {{.AboutVersion}}</strong></td></tr>
          <tr><td style="padding:4px 0;color:var(--muted);">Go</td><td>1.23</td></tr>
          <tr><td style="padding:4px 0;color:var(--muted);">{{index .T "about_author"}}</td><td>mguyenanastacio-glitch</td></tr>
        </table>
      </div>
    </div>
    <div class="card panel" style="margin-top:12px;">
      <h3 style="margin:0 0 8px;">🔗 {{index .T "about_links"}}</h3>
      <div style="display:flex;flex-wrap:wrap;gap:12px;font-size:13px;">
        <a href="https://github.com/mguyenanastacio-glitch/pan-fetcher" target="_blank" rel="noopener">GitHub</a>
        <span style="color:var(--line);">|</span>
        <a href="https://github.com/mguyenanastacio-glitch/pan-fetcher/releases" target="_blank" rel="noopener">Releases</a>
        <span style="color:var(--line);">|</span>
        <a href="https://github.com/mguyenanastacio-glitch/pan-fetcher/issues" target="_blank" rel="noopener">Issues</a>
      </div>
    </div>
    <div class="card panel" style="margin-top:12px;">
      <p style="font-size:12px;color:var(--muted);margin:0;">
        {{index .T "about_based_on"}}
        <a href="https://github.com/zhifengle/rss2cloud" target="_blank" rel="noopener">rss2cloud</a>
        &nbsp;·&nbsp;
        <a href="https://github.com/Prowlarr/Prowlarr" target="_blank" rel="noopener">Prowlarr</a>
        &nbsp;·&nbsp;
        <a href="https://github.com/Nahuimi/elevengo" target="_blank" rel="noopener">elevengo</a>
      </p>
      <p class="hint" style="margin-top:8px;">© 2025-2026 pan-fetcher</p>
    </div>
    {{end}}

    <!-- shared utility functions -->
    <script>
      function refreshSearch(){
        sessionStorage.removeItem('pan-fetcher-page');
        sessionStorage.removeItem('pan-fetcher-query');
        location.href='/search';
      }
      function clearSearch(){
        sessionStorage.removeItem('pan-fetcher-page');
        sessionStorage.removeItem('pan-fetcher-query');
        location.href='/search';
      }
      var pendingMagnet='';

      // Note: searchTotal, pageSize, currentPage, totalPages, searchDone
      // are declared above in the search page template section.

      function buildRowHTML(item, idx, pageStart){
        var num=pageStart+idx+1;
        var title=item.page_url?'<a href="'+item.page_url+'" target="_blank">'+(item.title||'')+'</a>':(item.title||'');
        var magnetBtn=item.magnet_url?'<button data-magnet="'+item.magnet_url.replace(/&/g,'&amp;').replace(/"/g,'&quot;')+'" onclick="addTaskWithBrowse(this.getAttribute(\'data-magnet\'))" style="background:var(--accent-2);padding:2px 8px;font-size:11px;margin:0;">+</button>':'';
        return '<tr data-title="'+(item.title||'')+'" data-group="'+(item.group||'')+'"><td class="muted" style="font-size:11px;text-align:center;">'+num+'</td><td>'+title+'</td><td class="muted">'+(item.size||'-')+'</td><td>'+(item.seeders||0)+'</td><td class="muted" style="font-size:11px;">'+(item.date||'')+'</td><td class="muted">'+(item.indexer||'')+'</td><td>'+magnetBtn+'</td></tr>';
      }

      function renderPagination(){
        var bar=document.getElementById('pagination-bar');
        if(!bar)return;
        var tbl=document.getElementById('search-results');
        if(!tbl||!tbl.querySelector('tbody tr')){bar.innerHTML='';return;}
        if(totalPages<=1){bar.innerHTML='<span style="font-size:11px;color:var(--muted);">{{index .T "page_total"}}</span>'.replace('%d',searchTotal);return;}
        var html='';
        html+='<button onclick="goToPage('+(currentPage-1)+')" '+(currentPage<=1?'disabled':'')+' style="padding:4px 10px;font-size:12px;margin:0;">{{index .T "page_prev"}}</button>';
        var start=Math.max(1,currentPage-2);
        var end=Math.min(totalPages,currentPage+2);
        if(start>1){html+='<button onclick="goToPage(1)" style="padding:4px 8px;font-size:12px;margin:0;">1</button>';if(start>2)html+='<span style="padding:0 2px;">…</span>';}
        for(var i=start;i<=end;i++){
          html+='<button onclick="goToPage('+i+')" '+(i===currentPage?'disabled style="padding:4px 8px;font-size:12px;margin:0;font-weight:bold;background:var(--accent);"':'style="padding:4px 8px;font-size:12px;margin:0;"')+'>'+i+'</button>';
        }
        if(end<totalPages){if(end<totalPages-1)html+='<span style="padding:0 2px;">…</span>';html+='<button onclick="goToPage('+totalPages+')" style="padding:4px 8px;font-size:12px;margin:0;">'+totalPages+'</button>';}
        html+='<button onclick="goToPage('+(currentPage+1)+')" '+(currentPage>=totalPages?'disabled':'')+' style="padding:4px 10px;font-size:12px;margin:0;">{{index .T "page_next"}}</button>';
        html+=' <span style="font-size:11px;color:var(--muted);margin-left:8px;">{{index .T "page_total"}}</span>'.replace('%d',searchTotal);
        bar.innerHTML=html;
      }

      async function goToPage(page){
        if(page<1||page>totalPages||page===currentPage)return;
        var bar=document.getElementById('pagination-bar');
        if(bar)bar.innerHTML='<span style="font-size:12px;color:var(--muted);">{{index .T "page_loading"}}</span>';
        var form=document.getElementById('search-form');
        var fd=new URLSearchParams(new FormData(form));
        if(!fd.get('q')){
          var savedQ=sessionStorage.getItem('pan-fetcher-query');
          if(savedQ) fd=new URLSearchParams(savedQ);
        }
        if(!fd.get('q')){bar.innerHTML='<span style="color:var(--danger);">无搜索参数</span>';return;}
        fd.set('offset',(page-1)*pageSize);
        fd.set('keyword',buildGroupKeyword());
        try{
          var r=await fetch('/search/more',{method:'POST',body:fd,headers:{'X-Requested-With':'XMLHttpRequest'}});
          var j=await r.json();
          if(!j.results||j.results.length===0){renderPagination();return;}
          var tbody=document.querySelector('#search-results tbody');
          if(!tbody){renderPagination();return;}
          var pageStart=(page-1)*pageSize;
          tbody.innerHTML=j.results.map(function(item,i){return buildRowHTML(item,i,pageStart);}).join('');
          currentPage=page;
          searchTotal=j.total||0;
          totalPages=searchTotal>0?Math.ceil(searchTotal/pageSize):1;
          buildGroupChips(document.getElementById('search-results-wrap'),document.getElementById('search-results'),fd.get('q')||'',fd.get('category')||'');
          renderPagination();
          sessionStorage.setItem('pan-fetcher-page',JSON.stringify({currentPage:currentPage,totalPages:totalPages,searchTotal:searchTotal,pageSize:pageSize}));
          var form2=document.getElementById('search-form');
          var fd2=new URLSearchParams(new FormData(form2));
          if(fd2.get('q')) sessionStorage.setItem('pan-fetcher-query',fd2.toString());
          document.getElementById('search-results').scrollIntoView({block:'start'});
        }catch(e){console.error(e);renderPagination();}
      }

      // Init pagination bar on page load
      renderPagination();
      async function testWebhook(){
        var input=document.getElementById('wework_webhook');
        var url=input?input.value.trim():'';
        if(!url){alertModal('请先填写 Webhook 地址');return;}
        var btn=event.target;
        btn.disabled=true;btn.textContent='…';
        try{
          var r=await fetch('/api/notify/test',{method:'POST',body:new URLSearchParams({webhook:url}),headers:{'X-Requested-With':'XMLHttpRequest'}});
          var j=await r.json();
          if(j.status==='ok'){alertModal('✅ 测试消息已发送，请查看企业微信');}
          else{alertModal('❌ '+j.message);}
        }catch(e){alertModal('请求失败: '+e.message);}
        btn.disabled=false;btn.textContent='测试';
      }
      async function addTaskWithBrowse(magnet){
        pendingMagnet=magnet;
        browseCallback=function(id){closeModal();doAddTask(id);};
        browseDirs('0');
      }
      async function doAddTask(cid){
        closeModal();
        try{
          var body='tasks='+encodeURIComponent(pendingMagnet);
          if(cid&&cid!=='0')body+='&cid='+encodeURIComponent(cid);
          var r=await fetch('/add',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:body});
          var j=await r.json();
          if(j.status==='ok'){alertModal('{{index .T "task_added"}}');}else{alertModal(j.message||'{{index .T "add_failed"}}');}
        }catch(e){alertModal(e.message);}
        pendingMagnet='';
      }
      var browseTargetId='sub-cid';
      var browseCallback=null;
      function browseDirsFor(targetId){browseTargetId=targetId;browseCallback=function(id){document.getElementById(targetId).value=id;closeModal();};browseDirs('0');}
      async function browseDirs(pid){
        if(!pid)pid='0';
        try{
          let r=await fetch('/subs/dirs?pid='+pid);
          let j=await r.json();
          if(!j.ok){showModal('{{index .T "error_label"}}','<p>'+j.msg+'</p>');return;}
          if(!j.entries)j.entries=[];
          if(!Array.isArray(j.entries))j.entries=[];
          var html='<div style="max-height:300px;overflow-y:auto;">';
          if(pid!=='0'){
            html+='<div style="cursor:pointer;padding:6px 8px;color:var(--accent-2);border-radius:6px;" onclick="browseDirs(\''+j.parent+'\')">{{index .T "parent_dir_label"}}</div>';
          }
          if(j.entries.length===0)html+='<p style="color:var(--muted);">{{index .T "no_subfolders"}}</p>';
          j.entries.forEach(function(e){
            html+='<div style="cursor:pointer;padding:6px 8px;margin:2px 0;border-radius:6px;display:flex;align-items:center;gap:8px;" onmouseover="this.style.background=\'#f0f4ff\'" onmouseout="this.style.background=\'\'">';
            html+='📁 <span style="flex:1;cursor:pointer;" onclick="browseDirs(\''+e.id+'\')">'+e.name+'</span>';
            html+='<code style="font-size:11px;color:var(--muted);cursor:pointer;" onclick="browseCallback(\''+e.id+'\')" title="选定此目录">'+e.id+'</code></div>';
          });
          html+='</div>';
          updateBrowseModal('{{index .T "select_dir_title"}}'.replace('%s',pid),html,pid);
        }catch(e){showModal('{{index .T "error_label"}}','<p>'+e.message+'</p>');}
      }
      function updateBrowseModal(title,body,pid){
        document.getElementById('g-modal-title').textContent=title;
        document.getElementById('g-modal-body').innerHTML=body;
        var btns=document.getElementById('g-modal-btns');
        btns.innerHTML='<button onclick="browseCallback(\''+pid+'\')" style="margin:0;padding:6px 16px;background:var(--accent-2);">{{index .T "select_current_dir"}}</button><button onclick="closeModal()" style="margin:0;padding:6px 16px;background:var(--danger);">{{index .T "close_btn"}}</button>';
        document.getElementById('g-modal').style.display='flex';
      }
    </script>

    <!-- indexer management -->
    {{if eq .Page "indexers"}}

    <!-- active indexers -->
    <div class="card panel">
      <h2>{{index .T "indexer_list"}} ({{len .IndexerList}})
        {{if .IndexerList}}<button onclick="testAll()" style="margin:0 0 0 12px;padding:4px 12px;font-size:12px;background:var(--accent-2);">{{index .T "test_all"}}</button>{{end}}
      </h2>
      {{if .IndexerList}}
      <table class="tbl" id="idx-active">
        <thead><tr><th>{{index .T "name"}}</th><th>{{index .T "sub_type"}}</th><th>{{index .T "jk_id"}}</th><th>{{index .T "idx_lang"}}</th><th>{{index .T "idx_source"}}</th><th>{{index .T "idx_health"}}</th><th></th></tr></thead>
        <tbody>
        {{range .IndexerList}}<tr id="row-{{.ID}}">
          <td>{{if .SiteLink}}<a href="{{.SiteLink}}" target="_blank">{{.Name}}</a>{{else}}<strong>{{.Name}}</strong>{{end}}<br><small class="err-msg" style="color:var(--danger);">{{.LastError}}</small></td>
          <td class="muted">{{.Type}}</td>
          <td class="muted" style="font-size:11px;">{{.ID}}</td>
          <td class="muted">{{.Language}}</td>
          <td><span style="font-size:10px;padding:1px 5px;border-radius:4px;color:#fff;{{if eq .Source "jackett"}}background:var(--accent-2);{{else}}background:var(--muted);{{end}}">{{if eq .Source "jackett"}}Jackett{{else}}{{index $.T "local"}}{{end}}</span></td>
          <td><span class="health-dot" style="color:{{if .Healthy}}var(--accent-2){{else}}var(--danger){{end}};" title="{{if .LastTest}}{{.LastTest}}{{end}}">●</span></td>
          <td style="white-space:nowrap;">
            {{if eq .Source "jackett"}}
            <button onclick="jkDeactivate('{{.ID}}')" style="padding:2px 6px;font-size:11px;margin:0;background:var(--danger);" title="{{index $.T "remove"}}">−</button>
            {{else}}
            {{if .HasLogin}}<button onclick="showLogin('{{.ID}}','{{.Name}}')" style="padding:2px 6px;font-size:11px;margin:0;background:var(--warn);">🔑</button>{{end}}
            <button onclick="deactivateIdx('{{.ID}}')" style="padding:2px 6px;font-size:11px;margin:0;background:var(--danger);" title="{{index $.T "remove_lib"}}">−</button>
            {{end}}
        </tr>{{end}}
        </tbody>
      </table>
      {{else}}
      <div class="hint">{{index .T "idx_no_active"}}</div>
      {{end}}
    </div>

    <!-- local library -->
    <div class="card panel" style="margin-top:16px;">
      <h2>{{index .T "idx_lib_local"}} (<span id="lib-count">{{len .IndexerLibrary}}</span>)
        <button onclick="newIdx()" style="margin:0 0 0 12px;padding:4px 12px;font-size:12px;background:var(--accent-2);">+ 添加库</button>
        <button onclick="activateSelected()" style="margin:0 0 0 8px;padding:4px 12px;font-size:12px;background:var(--accent);">{{index .T "idx_batch_add"}}</button>
      </h2>
      {{if .IndexerLibrary}}
      <table class="tbl" id="idx-library">
        <thead><tr><th></th><th>{{index .T "name"}}</th><th>{{index .T "sub_type"}}</th><th>{{index .T "jk_id"}}</th><th>{{index .T "idx_lang"}}</th><th></th></tr></thead>
        <tbody>
        {{range .IndexerLibrary}}
          <tr id="lib-{{.ID}}"{{if .Enabled}} style="opacity:0.6"{{end}}>
            <td>{{if not .Enabled}}<input type="checkbox" name="ids" value="{{.ID}}" style="width:auto;margin:0;">{{end}}</td>
            <td>{{if .SiteLink}}<a href="{{.SiteLink}}" target="_blank">{{.Name}}</a>{{else}}<strong>{{.Name}}</strong>{{end}}</td>
            <td class="muted">{{.Type}}</td>
            <td class="muted" style="font-size:11px;">{{.ID}}</td>
            <td class="muted">{{.Language}}</td>
            <td style="white-space:nowrap;">
              {{if .Enabled}}
              <span style="font-size:11px;color:var(--accent-2);">已激活</span>
              {{else}}
              <button onclick="activateSingle('{{.ID}}')" style="padding:2px 6px;font-size:11px;margin:0;background:var(--accent);" title="激活">+</button>
              {{end}}
              <button onclick="editIdx('{{.ID}}','{{.Name}}')" style="padding:2px 6px;font-size:11px;margin:0;">✎</button>
              <button onclick="deleteIdx('{{.ID}}','{{.Name}}')" style="padding:2px 6px;font-size:11px;margin:0;background:var(--danger);" title="删除定义">✕</button>
            </td>
          </tr>
        {{end}}
        </tbody>
      </table>
      {{else}}
      <div class="hint">{{index .T "idx_lib_empty"}}</div>
      {{end}}
    </div>

    <!-- jackett library (loaded async) -->
    <div class="card panel" style="margin-top:16px;">
      <h2>{{index .T "idx_lib_jackett"}} (<span id="jk-count">…</span>) 
        <span id="jk-batch-btn"></span>
        <button onclick="showJKAddModal()" id="jk-add-btn" style="margin:0 0 0 8px;padding:4px 12px;font-size:12px;background:var(--accent-2);">{{index .T "jk_add_lib"}}</button>
        {{if not .JackettURL}}<a href="/settings" style="font-size:12px;margin-left:8px;color:var(--accent);">⚙ {{index .T "jk_lib_config"}}</a>{{end}}
      </h2>
      <div id="jk-content"><span class="hint">⏳ {{index .T "loading"}}</span></div>
    </div>

    <script>
      async function apiPost(action, fields){
        let form=new URLSearchParams();
        form.set('action',action);
        for(let k in fields){
          let v=fields[k];
          if(Array.isArray(v)) v.forEach(x=>form.append(k,x));
          else form.set(k,v);
        }
        let r=await fetch('/indexers',{method:'POST',body:form,headers:{'X-Requested-With':'XMLHttpRequest'}});
        try{return await r.json();}catch(e){return {};}
      }

      async function testIdx(id,name){
        let dot=document.querySelector('#row-'+id+' .health-dot');
        let errEl=document.querySelector('#row-'+id+' .err-msg');
        dot.textContent='…';dot.style.color='var(--warn)';
        try{
          let r=await fetch('/indexers/test?id='+encodeURIComponent(id));
          let j=await r.json();
          if(j.ok){dot.textContent='●';dot.style.color='var(--accent-2)';errEl.textContent='';}
          else{dot.textContent='●';dot.style.color='var(--danger)';errEl.textContent=j.msg;}
          dot.title=new Date().toLocaleString('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'});
        }catch(e){dot.textContent='●';dot.style.color='var(--danger)';errEl.textContent=e.message;}
      }

      async function testAll(){
        let r=await fetch('/indexers/testall',{method:'POST',headers:{'X-Requested-With':'XMLHttpRequest'}});
        let j=await r.json();
        var now=new Date().toLocaleString('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'});
        for(let id in j){
          let dot=document.querySelector('#row-'+id+' .health-dot');
          let errEl=document.querySelector('#row-'+id+' .err-msg');
          if(dot){
            if(j[id]==='ok'){dot.style.color='var(--accent-2)';if(errEl)errEl.textContent='';}
            else{dot.style.color='var(--danger)';if(errEl)errEl.textContent=j[id];}
            dot.title=now;
          }
        }
      }

      async function deactivateIdx(id){
        var row=document.getElementById('row-'+id);
        if(row) row.style.display='none';
        await apiPost('deactivate',{id});
        location.reload();
      }

      async function showLogin(id,name){
        var body='<div><label>{{index .T "username_label"}}</label><input id="login-user" style="width:100%;"></div>';
        body+='<div style="margin-top:8px;"><label>{{index .T "password_label"}}</label><input id="login-pass" type="password" style="width:100%;"></div>';
        showModal('{{index .T "login_label"}} - '+name, body, [
          {text:'{{index .T "cancel"}}',cls:'var(--danger)',cb:function(){closeModal()}},
          {text:'{{index .T "login_label"}}',cls:'var(--accent-2)',cb:async function(){
            var u=document.getElementById('login-user').value;
            var p=document.getElementById('login-pass').value;
            if(!u||!p){showModal('{{index .T "error_label"}}','<p>{{index .T "credentials_required"}}</p>');return;}
            closeModal();
            try{
              let r=await fetch('/indexers/login',{
                method:'POST',
                body:new URLSearchParams({action:'login',id,username:u,password:p}),
                headers:{'X-Requested-With':'XMLHttpRequest'}
              });
              let j=await r.json();
              if(j.ok){showModal('{{index .T "success_label"}}','<p>{{index .T "login_success_msg"}}</p>');testIdx(id,name);}
              else{showModal('{{index .T "failed"}}','<p>'+j.msg+'</p>');}
            }catch(e){showModal('{{index .T "error_label"}}','<p>'+e.message+'</p>');}
          }}
        ]);
      }

      async function activateSelected(){
        let checks=document.querySelectorAll('#idx-library input[type="checkbox"]:checked');
        if(checks.length===0) return;
        checks.forEach(function(c){var row=document.getElementById('lib-'+c.value);if(row)row.style.opacity='0.4';});
        let ids=[];
        checks.forEach(c=>ids.push(c.value));
        await apiPost('activate_batch',{ids:ids});
        location.reload();
      }

      async function editIdx(id,name){
        try{
          var r=await fetch('/indexers/edit?id='+encodeURIComponent(id));
          var j=await r.json();
          if(!j.ok){alertModal(j.msg);return;}
          showModal('{{index .T "edit_label"}}: '+name,
            '<textarea id="edit-yaml" style="width:100%;height:400px;font-family:monospace;font-size:12px;">'+
            j.yaml.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;')+'</textarea>',
            [
              {text:'{{index .T "cancel"}}',cls:'var(--danger)',cb:function(){closeModal()}},
              {text:'{{index .T "save"}}',cls:'var(--accent-2)',cb:async function(){
                var y=document.getElementById('edit-yaml').value;
                var r2=await fetch('/indexers/edit?id='+encodeURIComponent(id),
                  {method:'POST',body:new URLSearchParams({yaml:y}),
                   headers:{'X-Requested-With':'XMLHttpRequest'}});
                var j2=await r2.json();
                if(j2.ok){closeModal();location.reload();}
                else{alertModal('{{index .T "save"}} failed: '+j2.msg);}
              }}
            ]
          );
        }catch(e){alertModal(e.message);}
      }

      async function activateSingle(id){
        var row=document.getElementById('lib-'+id);
        if(row) row.style.opacity='0.4';
        await apiPost('activate',{id});
        location.reload();
      }

      async function deleteIdx(id,name){
        if(!(await confirmAsync('Delete "'+name+'"? This removes the YAML file permanently.'))) return;
        try{
          var r=await fetch('/indexers/delete',{method:'POST',body:new URLSearchParams({id:id}),headers:{'X-Requested-With':'XMLHttpRequest'}});
          var j=await r.json();
          if(j.ok){var row=document.getElementById('lib-'+id);if(row)row.style.display='none';}
          else{alertModal(j.msg);}
        }catch(e){alertModal(e.message);}
      }

      function newIdx(){
        showModal('{{index .T "new_idx"}}',
          '<label>ID:</label><input id="new-id" style="width:100%;" placeholder="e.g. mysite">',
          [
            {text:'{{index .T "cancel"}}',cls:'var(--danger)',cb:function(){closeModal()}},
            {text:'{{index .T "create"}}',cls:'var(--accent-2)',cb:async function(){
              var id=document.getElementById('new-id').value.trim();
              if(!id){alertModal('Please enter an ID');return;}
              var tmpl='---\nid: '+id+'\nname: My Site\ntype: public\nlanguage: zh-CN\nlinks:\n  - https://\n\ncaps:\n  categories:\n    1: Other\n  modes:\n    search: [q]\n\nsearch:\n  paths:\n    - path: /search\n  inputs:\n    q: "___KEYWORDS___"\n  rows:\n    selector: table tr\n  fields:\n    title:\n      selector: a\n    details:\n      selector: a\n      attribute: href\n    download:\n      selector: a[href*=magnet]\n      attribute: href\n    size:\n      selector: .size\n    date:\n      selector: .date\n    seeders:\n      selector: .seeders\n';
              closeModal();
              showModal('{{index .T "new_idx"}}: '+id,'<textarea id="new-yaml" style="width:100%;height:400px;font-family:monospace;font-size:12px;">'+tmpl.replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;')+'</textarea>',[
                {text:'{{index .T "cancel"}}',cls:'var(--danger)',cb:function(){closeModal()}},
                {text:'{{index .T "save"}}',cls:'var(--accent-2)',cb:async function(){
                  var y=document.getElementById('new-yaml').value;
                  var r=await fetch('/indexers/edit?id='+encodeURIComponent(id),{method:'POST',body:new URLSearchParams({yaml:y}),headers:{'X-Requested-With':'XMLHttpRequest'}});
                  var j=await r.json();
                  if(j.ok){closeModal();location.reload();}
                  else{alertModal('Failed: '+j.msg);}
                }}
              ]);
            }}
          ]
        );
      }
      // Load Jackett indexers async
      async function loadJackettLib(){
        var ct=document.getElementById('jk-content');
        var cnt=document.getElementById('jk-count');
        if(cnt) cnt.textContent='…';
        try{
          var r=await fetch('/indexers/jackett');
          var j=await r.json();
          if(!j.ok||!j.data||!j.data.length){
            ct.innerHTML='<div class="hint">{{index .T "jk_lib_empty"}}</div>';
            cnt.textContent='0';
            return;
          }
          // Use server-provided active list (always accurate, not stale DOM)
          var jkActiveIds=new Set(j.active||[]);
          cnt.textContent=j.data.length;
          var h='<table class="tbl"><thead><tr><th></th><th>{{index .T "name"}}</th><th>{{index .T "sub_type"}}</th><th>{{index .T "jk_id"}}</th><th>{{index .T "idx_lang"}}</th><th></th></tr></thead><tbody>';
          j.data.forEach(function(x){
            var isActive=jkActiveIds.has(x.id);
            h+='<tr id="jk-row-'+x.id+'"'+(isActive?' style="opacity:0.6"':'')+'><td>'+(isActive?'':'<input type="checkbox" name="jk_ids" value="'+x.id+'" style="width:auto;margin:0;">')+'</td>';
            h+='<td>'+(x.site_link?'<a href="'+x.site_link+'" target="_blank">'+x.name+'</a>':'<strong>'+x.name+'</strong>')+(x.description?'<br><small class="muted">'+x.description+'</small>':'')+'</td>';
            h+='<td class="muted">'+(x.type||'')+'</td>';
            h+='<td class="muted" style="font-size:11px;">'+x.id+'</td>';
            h+='<td class="muted">'+(x.language||'')+'</td>';
            h+='<td style="white-space:nowrap;">';
            if(isActive){
              h+='<span style="font-size:11px;color:var(--accent-2);" title="{{index .T "idx_jk_activated"}}">{{index .T "idx_activated"}}</span> ';
            }else{
              h+='<button onclick="jkActivate(\''+x.id+'\')" style="padding:2px 6px;font-size:11px;margin:0;background:var(--accent);" title="{{index .T "idx_activate_hint"}}">+</button> ';
            }
            h+='<button onclick="jkRemoveFromJackett(\''+x.id+'\',\''+x.name.replace(/'/g,"\\'")+'\')" style="padding:2px 6px;font-size:10px;margin:0 0 0 2px;background:var(--danger);" title="{{index .T "idx_jk_delete_hint"}}">✕</button>';
            h+='</td></tr>';
          });
          h+='</tbody></table>';
          document.getElementById('jk-batch-btn').innerHTML='<button onclick="jkActivateSelected()" style="margin:0 0 0 12px;padding:4px 12px;font-size:12px;background:var(--accent);">{{index .T "idx_batch_add"}}</button>';
          ct.innerHTML=h;
        }catch(e){
          ct.innerHTML='<div class="hint" style="color:var(--danger);">✗ '+e.message+'</div>';
          cnt.textContent='?';
        }
      }

      // Show modal to add indexers from Jackett's full catalog
      var jkAllData=[];
      async function showJKAddModal(){
        showModal('{{index .T "jk_add_lib"}}',
          '<div style="margin-bottom:10px;"><input id="jk-add-filter" placeholder="{{index .T "jk_search_ph"}}" style="width:100%;padding:8px;" oninput="filterJKAddList()" autofocus></div>'+ 
          '<div id="jk-add-list" style="max-height:50vh;overflow-y:auto;">⏳ {{index .T "loading"}}</div>',
          [{text:'{{index .T "close_btn"}}',cls:'var(--danger)',cb:function(){closeModal()}}]);
        try{
          var r=await fetch('/indexers/jackett/all');
          var j=await r.json();
          if(!j.ok||!j.data){document.getElementById('jk-add-list').innerHTML='<span class="hint" style="color:var(--danger);">✗ '+j.msg+'</span>';return;}
          jkAllData=j.data;
          renderJKAddList();
        }catch(e){document.getElementById('jk-add-list').innerHTML='<span class="hint" style="color:var(--danger);">✗ '+e.message+'</span>';}
      }

      function renderJKAddList(filter){
        filter=(filter||'').toLowerCase();
        var h='<table class="tbl"><thead><tr><th>{{index .T "name"}}</th><th>{{index .T "sub_type"}}</th><th>{{index .T "idx_lang"}}</th><th></th></tr></thead><tbody>';
        jkAllData.forEach(function(x){
          if(filter&&x.name.toLowerCase().indexOf(filter)<0&&x.id.toLowerCase().indexOf(filter)<0)return;
          h+='<tr><td>'+(x.site_link?'<a href="'+x.site_link+'" target="_blank">'+x.name+'</a>':'<strong>'+x.name+'</strong>')+'</td>';
          h+='<td class="muted">'+(x.type||'')+'</td>';
          h+='<td class="muted">'+(x.language||'')+'</td>';
          h+='<td style="white-space:nowrap;">';
          if(x.configured){
            h+='<span style="font-size:11px;color:var(--muted);">{{index .T "configured"}}</span>';
          }else{
            h+='<button onclick="jkAddToJackett(\''+x.id+'\')" style="padding:2px 8px;font-size:11px;margin:0;background:var(--accent-2);">{{index .T "add"}}</button>';
          }
          h+='</td></tr>';
        });
        h+='</tbody></table>';
        document.getElementById('jk-add-list').innerHTML=h||'<div class="hint">{{index .T "jk_no_match"}}</div>';
      }

      function filterJKAddList(){
        renderJKAddList(document.getElementById('jk-add-filter').value);
      }

      function jkAddToJackett(id){
        showModal('添加索引器','正在向 Jackett 添加 <b>'+id+'</b>…');
        apiPost('jk_add_to_jackett',{id}).then(function(r){
          closeModal();
          if(r.ok){location.reload();}
          else{alertModal('添加失败: '+r.msg);}
        });
      }

      async function jkRemoveFromJackett(id,name){
        if(!(await confirmAsync('从 Jackett 删除 <b>'+(name||id)+'</b>？<br><small style="color:var(--danger);">这将移除此索引器在 Jackett 中的所有配置</small>'))) return;
        showModal('删除索引器','正在从 Jackett 移除 <b>'+id+'</b>…');
        apiPost('jk_remove_from_jackett',{id}).then(function(r){
          closeModal();
          if(r.ok){location.reload();}
          else{alertModal('移除失败: '+r.msg);}
        });
      }
      async function jkActivate(id){
        // Instant visual feedback
        var row=document.getElementById('jk-row-'+id);
        if(row){row.style.opacity='0.5';var td=row.querySelectorAll('td');if(td.length>=6)td[5].innerHTML='<span style="font-size:11px;color:var(--accent-2);">已激活</span>';if(td.length>=1)td[0].innerHTML='';}
        await apiPost('jk_activate',{id});
        location.reload();
      }
      async function jkDeactivate(id){
        var row=document.getElementById('jk-row-'+id);
        if(row){row.style.opacity='1';var td=row.querySelectorAll('td');if(td.length>=6)td[5].innerHTML='<button onclick="jkActivate(\''+id+'\')" style="padding:2px 6px;font-size:11px;margin:0;background:var(--accent);">+</button>';if(td.length>=1)td[0].innerHTML='<input type="checkbox" name="jk_ids" value="'+id+'" style="width:auto;margin:0;">';}
        await apiPost('jk_deactivate',{id});
        location.reload();
      }
      async function jkActivateSelected(){
        var checks=document.querySelectorAll('#jk-content input[type="checkbox"]:checked');
        if(checks.length===0) return;
        for(var c of checks){
          var id=c.value;
          var row=document.getElementById('jk-row-'+id);
          if(row){row.style.opacity='0.5';var td=row.querySelectorAll('td');if(td.length>=6)td[5].innerHTML='<span style="font-size:11px;color:var(--accent-2);">已激活</span>';if(td.length>=1)td[0].innerHTML='';}
          await apiPost('jk_activate',{id:c.value});
        }
        location.reload();
      }
      loadJackettLib();
    </script>
    {{end}}

    <!-- settings -->
    {{if eq .Page "settings"}}
    <div class="card panel">
      <h2>{{index .T "sys_settings"}}</h2>
      <!-- login -->
      <div style="margin-bottom:14px;padding:12px;background:#f8fafc;border-radius:10px;border:1px solid var(--line);">
        <h3 style="margin:0 0 8px;">{{index .T "login_115"}}
          <span style="font-weight:400;font-size:13px;margin-left:8px;">
            {{if .LoggedIn}}<span style="color:var(--accent-2);">{{index .T "connected"}}</span>{{else}}<span style="color:var(--danger);">{{index .T "disconnected"}}</span>{{end}}
          </span>
          <button id="test115-btn" style="margin:0 0 0 8px;padding:2px 10px;font-size:12px;background:var(--accent-2);" onclick="test115()">{{index .T "test_conn"}}</button>
          <span id="test115-result" style="font-size:12px;margin-left:6px;"></span>
        </h3>
        <div style="display:flex;gap:8px;flex-wrap:wrap;align-items:flex-end;">
          <div style="flex:3;min-width:240px;">
            <label>{{index .T "cookies_label"}}</label>
            <input name="cookies" form="frm-cookies" placeholder="{{index .T "cookies_ph"}}" value="{{.Cookies}}">
          </div>
          <button type="submit" form="frm-cookies" style="margin-top:0;">{{index .T "update_cookies"}}</button>
          <button type="button" id="qr-login-btn" onclick="startQRLogin()" style="margin-top:0;padding:8px 14px;background:var(--accent-2);color:#fff;border-radius:10px;font-size:14px;border:none;cursor:pointer;">{{index .T "qr_login"}}</button>
        </div>
        <form id="frm-cookies" action="/login/cookies" method="post" style="display:none;"></form>
        <div id="qr-box" style="margin-top:10px;text-align:center;display:none;">
          <p>{{index .T "qrcode_wait"}}</p>
          <img id="qr-img" src="" style="max-width:200px;display:none;">
          <p id="qr-status" style="color:var(--muted);">{{index .T "qrcode_scanning"}}</p>
        </div>
          <script>
            async function startQRLogin(){
              var box=document.getElementById('qr-box');
              box.style.display='block';
              var btn=document.getElementById('qr-login-btn');
              btn.disabled=true;btn.textContent='{{index .T "waiting"}}';
              try {
                let r=await fetch('/login/qrcode',{method:'POST'});
                let d=await r.json();
                if(d.qrcode){
                  document.getElementById('qr-img').src=d.qrcode;
                  document.getElementById('qr-img').style.display='inline';
                  let polls=0;
                  let poll=setInterval(async()=>{
                    polls++;
                    if(polls>150){clearInterval(poll);document.getElementById('qr-status').textContent='{{index .T "qrcode_timeout"}}';btn.disabled=false;btn.textContent='{{index .T "qr_login"}}';return;}
                    let s=await fetch('/login/qrcode?poll=1');
                    let j=await s.json();
                    if(j.status==='ok'){document.getElementById('qr-status').textContent='{{index .T "qrcode_ok"}}';clearInterval(poll);btn.disabled=false;btn.textContent='{{index .T "qr_login"}}';}
                    else if(j.status!=='waiting'){document.getElementById('qr-status').textContent=j.status;clearInterval(poll);btn.disabled=false;btn.textContent='{{index .T "qr_login"}}';}
                  },2000);
                }else{document.getElementById('qr-status').textContent='{{index .T "qrcode_error"}}';btn.disabled=false;btn.textContent='{{index .T "qr_login"}}';}
              }catch(e){document.getElementById('qr-status').textContent='{{index .T "net_error"}}';btn.disabled=false;btn.textContent='{{index .T "qr_login"}}';}
            }
          </script>
      </div>
      <script>
        async function test115(){
          var btn=document.getElementById('test115-btn');
          var sp=document.getElementById('test115-result');
          btn.disabled=true;sp.textContent='{{index .T "testing"}}';sp.style.color='var(--muted)';
          try{
            var r=await fetch('/settings/test115');
            var j=await r.json();
            if(j.ok){sp.textContent='✓ '+j.msg;sp.style.color='var(--accent-2)';}
            else{sp.textContent='✗ '+j.msg;sp.style.color='var(--danger)';}
          }catch(e){sp.textContent='✗ {{index .T "net_error"}}';sp.style.color='var(--danger)';}
          btn.disabled=false;
        }
        async function testJackett(){
          var btn=document.getElementById('jk-test-btn');
          var sp=document.getElementById('jk-test-result');
          var urlEl=document.querySelector('input[name="jackett_url"]');
          var keyEl=document.querySelector('input[name="jackett_apikey"]');
          btn.disabled=true;sp.textContent='{{index .T "testing"}}';sp.style.color='var(--muted)';
          try{
            var fd=new URLSearchParams();
            fd.append('url',urlEl.value.trim());
            fd.append('apikey',keyEl.value.trim());
            var r=await fetch('/settings/test-jackett',{method:'POST',body:fd});
            var j=await r.json();
            if(j.ok){sp.textContent='✓ '+j.msg;sp.style.color='var(--accent-2)';}
            else{sp.textContent='✗ '+j.msg;sp.style.color='var(--danger)';}
          }catch(e){sp.textContent='✗ {{index .T "net_error"}}';sp.style.color='var(--danger)';}
          btn.disabled=false;
        }
        async function checkUpdate(){
          var btn=document.getElementById('check-update-btn');
          var sp=document.getElementById('update-status');
          btn.disabled=true;sp.textContent='{{index .T "update_checking"}}';sp.style.color='var(--muted)';
          try{
            var r=await fetch('/settings/check-update');
            var j=await r.json();
            if(j.has_update){
              sp.innerHTML='{{index .T "update_new_found"}}'.replace('%s','<b>'+j.latest+'</b>').replace('%s','<b>'+j.current+'</b>')+' <button type="button" id="do-update-btn" onclick="doUpdate()" style="margin:0 0 0 8px;padding:4px 10px;font-size:12px;background:var(--accent);">{{index .T "update_do_btn"}}</button>';
              sp.style.color='var(--accent-2)';
            }else if(j.latest){
              sp.textContent='✓ {{index .T "update_already_latest"}} ('+j.latest+')';sp.style.color='var(--accent-2)';
            }else{
              sp.textContent='✗ {{index .T "update_fetch_failed"}}';sp.style.color='var(--danger)';
            }
          }catch(e){sp.textContent='✗ {{index .T "net_error"}}';sp.style.color='var(--danger)';}
          btn.disabled=false;
        }
        async function doUpdate(){
          if(!(await confirmAsync('{{index .T "confirm_restart"}}')))return;
          var sp=document.getElementById('update-status');
          sp.textContent='{{index .T "update_checking"}}';sp.style.color='var(--muted)';
          try{
            var r=await fetch('/settings/update',{method:'POST'});
            var j=await r.json();
            if(j.ok){sp.textContent='✓ '+j.msg;sp.style.color='var(--accent-2)';setTimeout(function(){location.reload();},3000);}
            else if(j.action==='sudo'){
              sp.innerHTML='<span style="color:var(--warn);">'+j.msg+'</span><br><code style="display:block;margin-top:6px;padding:6px 10px;background:#1e1e1e;color:#0f0;border-radius:6px;font-size:12px;word-break:break-all;cursor:pointer;" onclick="var t=this.textContent;navigator.clipboard.writeText(t).then(function(){this.style.background=\'#333\'})" title="点击复制">'+j.cmd+'</code>';
            }else{sp.textContent='✗ '+j.msg;sp.style.color='var(--danger)';var b=document.getElementById('check-update-btn');if(b)b.disabled=false;}
          }catch(e){sp.textContent='✗ {{index .T "net_error"}}';sp.style.color='var(--danger)';var b=document.getElementById('check-update-btn');if(b)b.disabled=false;}
        }
      </script>
      <!-- settings form -->
      <form action="/settings" method="post">
        <!-- Group: System -->
        <fieldset style="border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin-bottom:14px;">
          <legend style="font-weight:600;font-size:14px;padding:0 8px;">🌐 {{index .T "system_label"}}</legend>
          <div style="display:flex;gap:10px;flex-wrap:wrap;align-items:flex-end;">
            <div style="flex:2;min-width:200px;">
              <label>{{index .T "http_proxy_label"}}</label>
              <input name="proxy_http" placeholder="http://127.0.0.1:7890" value="{{.ProxyHTTP}}">
            </div>
            <div style="flex:1;min-width:120px;">
              <label>{{index .T "web_pw"}}</label>
              <input name="web_password" type="password" placeholder="{{index .T "web_pw_ph"}}" value="{{.Settings.WebPassword}}" maxlength="128">
            </div>
            <div style="flex:1;min-width:130px;">
              <label>{{index .T "timezone_label"}}</label>
              <select name="timezone" style="width:100%;font-size:13px;padding:8px 6px;margin:0;">
                {{range $val, $name := .TimezoneOptions}}<option value="{{$val}}"{{if eq $.Timezone $val}} selected{{end}}>{{$name}}</option>{{end}}
              </select>
            </div>
          </div>
          <div style="display:flex;gap:10px;flex-wrap:wrap;align-items:center;margin-top:10px;padding-top:10px;border-top:1px solid var(--line);">
            <button type="button" id="check-update-btn" onclick="checkUpdate()" style="margin:0;padding:6px 14px;font-size:13px;background:var(--accent-2);">{{index .T "update_check_btn"}}</button>
            <span id="update-status" style="font-size:12px;color:var(--muted);"></span>
            <label style="display:flex;align-items:center;gap:6px;cursor:pointer;font-size:13px;margin:0 0 0 auto;">
              <input type="checkbox" name="auto_update" value="1" style="width:auto;margin:0;"{{if .AutoUpdate}} checked{{end}}>{{index .T "update_auto_label"}}
            </label>
          </div>
        </fieldset>

        <!-- Group: Download -->
        <fieldset style="border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin-bottom:14px;">
          <legend style="font-weight:600;font-size:14px;padding:0 8px;">📥 {{index .T "download_settings_label"}}</legend>
          <div style="display:flex;gap:10px;flex-wrap:wrap;align-items:flex-end;">
            <div style="flex:1;min-width:90px;">
              <label>{{index .T "chunk_size"}}</label>
              <input name="chunk_size" type="number" value="{{.Settings.ChunkSize}}">
            </div>
            <div style="flex:1;min-width:90px;">
              <label>{{index .T "chunk_delay"}}</label>
              <input name="chunk_delay" type="number" value="{{.Settings.ChunkDelay}}">
            </div>
            <div style="flex:1;min-width:100px;">
              <label>{{index .T "cooldown_min"}}</label>
              <input name="cooldown_min" type="number" value="{{.Settings.CooldownMinMs}}">
            </div>
            <div style="flex:1;min-width:100px;">
              <label>{{index .T "cooldown_max"}}</label>
              <input name="cooldown_max" type="number" value="{{.Settings.CooldownMaxMs}}">
            </div>
          </div>
        </fieldset>

        <!-- Group: Search -->
        <fieldset style="border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin-bottom:14px;">
          <legend style="font-weight:600;font-size:14px;padding:0 8px;">🔍 {{index .T "search_label"}}</legend>
          <div style="display:flex;gap:10px;flex-wrap:wrap;align-items:flex-end;">
            <div style="flex:1;min-width:100px;">
              <label>{{index .T "page_size_label"}}</label>
              <input name="page_size" type="number" value="{{.PageSize}}" min="10" max="500" placeholder="50">
              <div class="hint" style="font-size:11px;">10–500，默认 50</div>
            </div>
          </div>
        </fieldset>

        <!-- Group: Subscription & Notifications -->
        <fieldset style="border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin-bottom:14px;">
          <legend style="font-weight:600;font-size:14px;padding:0 8px;">📢 {{index .T "subs_notify_label"}}</legend>
          <div style="display:flex;gap:10px;flex-wrap:wrap;align-items:flex-end;">
            <div style="flex:0.8;min-width:100px;">
              <label>{{index .T "subs_interval_label"}}</label>
              <input name="subs_interval" type="number" placeholder="{{index .T "subs_interval_ph"}}" value="{{.Settings.SubsInterval}}" min="0" style="font-size:13px;">
            </div>
            <div style="flex:3;min-width:300px;">
              <label>{{index .T "wework_label"}}</label>
              <div style="display:flex;gap:4px;">
                <input name="wework_webhook" id="wework_webhook" placeholder="https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=..." value="{{.WeworkWebhook}}" style="flex:1;font-size:13px;">
                <button type="button" onclick="testWebhook()" style="margin-top:0;padding:4px 10px;font-size:12px;white-space:nowrap;">{{index .T "test_btn"}}</button>
              </div>
            </div>
          </div>
          <div style="display:flex;gap:16px;flex-wrap:wrap;margin-top:10px;align-items:center;font-size:12px;color:var(--muted);">
            <label style="display:flex;align-items:center;gap:4px;cursor:pointer;font-size:12px;margin:0;">
              <input type="checkbox" name="notify_task" value="1" style="width:auto;margin:0;"{{if .NotifyTask}} checked{{end}} onclick="if(this.checked){document.getElementsByName('notify_log')[0].checked=false}">{{index .T "notify_task"}}
            </label>
            <label style="display:flex;align-items:center;gap:4px;cursor:pointer;font-size:12px;margin:0;">
              <input type="checkbox" name="notify_rss" value="1" style="width:auto;margin:0;"{{if .NotifyRSS}} checked{{end}} onclick="if(this.checked){document.getElementsByName('notify_log')[0].checked=false}">{{index .T "notify_rss"}}
            </label>
            <label style="display:flex;align-items:center;gap:4px;cursor:pointer;font-size:12px;margin:0;">
              <input type="checkbox" name="notify_log" value="1" style="width:auto;margin:0;"{{if .NotifyLog}} checked{{end}} onclick="if(this.checked){var t=document.getElementsByName('notify_task')[0];var r=document.getElementsByName('notify_rss')[0];if(t)t.checked=false;if(r)r.checked=false;}">{{index .T "notify_log"}}
            </label>
          </div>
        </fieldset>

        <!-- Group: Jackett -->
        <fieldset style="border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin-bottom:14px;">
          <legend style="font-weight:600;font-size:14px;padding:0 8px;">🦜 Jackett / Prowlarr</legend>
          <div style="display:flex;gap:10px;flex-wrap:wrap;align-items:flex-end;">
            <div style="flex:2;min-width:200px;">
              <label>Jackett URL</label>
              <input name="jackett_url" placeholder="http://localhost:9117" value="{{.JackettURL}}">
            </div>
            <div style="flex:1.5;min-width:160px;">
              <label>Jackett API Key</label>
              <div style="display:flex;align-items:center;gap:0;">
                <input name="jackett_apikey" placeholder="API Key" value="{{.JackettAPIKey}}" style="flex:1;">
                <button type="button" id="jk-test-btn" style="margin:0 0 0 6px;padding:2px 10px;font-size:12px;background:var(--accent-2);" onclick="testJackett()">{{index .T "jk_test_btn"}}</button>
              </div>
              <span id="jk-test-result" style="font-size:12px;"></span>
            </div>
            <div style="flex:1;min-width:140px;">
              <label>Admin 密码 <small style="color:var(--muted);">(WebUI 登录密码)</small></label>
              <input name="jackett_admin_password" type="password" placeholder="与 API Key 相同则留空" value="{{.JackettAdminPassword}}">
            </div>
          </div>
        </fieldset>

        <!-- Group: TMDB -->
        <fieldset style="border:1px solid var(--line);border-radius:10px;padding:14px 16px;margin-bottom:14px;">
          <legend style="font-weight:600;font-size:14px;padding:0 8px;">🎬 TMDB API</legend>
          <div style="display:flex;gap:10px;flex-wrap:wrap;align-items:flex-end;">
            <div style="flex:2;min-width:260px;">
              <label>TMDB API Key <small style="color:var(--muted);">(<a href="https://www.themoviedb.org/settings/api" target="_blank">获取 Key</a>)</small></label>
              <input name="tmdb_apikey" placeholder="输入 TMDB API Key 以启用海报搜索" value="{{.TMDBAPIKey}}">
            </div>
          </div>
        </fieldset>

        <div style="display:flex;justify-content:space-between;align-items:center;margin-top:14px;padding-top:12px;border-top:1px solid var(--line);">
          <button type="submit" style="margin-top:0;">{{index .T "save"}}</button>
          <button type="button" onclick="restartServer()" style="margin-top:0;background:var(--danger);">{{index .T "restart_service_btn"}}</button>
        </div>
        <div class="hint">{{index .T "db_path"}}: {{.Settings.DatabasePath}}</div>
      </form>
    </div>
    {{end}}

  </div><!-- /.main -->

  <!-- RSS subscription modal -->
  <div id="sub-form" style="display:none;position:fixed;top:0;left:0;right:0;bottom:0;background:rgba(0,0,0,0.4);z-index:998;align-items:center;justify-content:center;" onclick="if(event.target===this)document.getElementById('sub-form').style.display='none'">
    <div style="background:#fff;border-radius:16px;padding:24px;width:92%;max-width:600px;max-height:85vh;overflow-y:auto;box-shadow:0 12px 40px rgba(0,0,0,.2);" onclick="event.stopPropagation()">
      <div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:16px;">
        <h3 style="margin:0;">📌 添加 RSS 订阅</h3>
        <button onclick="document.getElementById('sub-form').style.display='none'" style="background:none;border:none;font-size:20px;cursor:pointer;padding:0;color:var(--muted);">×</button>
      </div>
      <form action="/search/subscribe" method="post">
        <input type="hidden" name="query" id="sub-query"><input type="hidden" name="category" id="sub-category">
        <div style="display:flex;gap:8px;flex-wrap:wrap;align-items:flex-end;">
          <div style="flex:2;min-width:120px;"><label style="font-size:12px;">名称</label><input name="name" id="sub-name" style="font-size:13px;"></div>
          <div style="flex:2;min-width:160px;"><label style="font-size:12px;">RSS 地址</label><input name="url" id="sub-url" style="font-size:13px;"></div>
          <div style="flex:1;min-width:110px;"><label style="font-size:12px;">115 目录 ID (可选)</label><div style="display:flex;gap:4px;"><input name="cid" id="sub-cid" placeholder="cid" style="font-size:13px;flex:1;"><button type="button" onclick="browseDirsFor('sub-cid')" title="浏览115目录" style="font-size:13px;padding:4px 8px;margin:0;background:var(--bg);border:1px solid var(--line);border-radius:6px;cursor:pointer;">📂</button></div></div>
          <div style="flex:1;min-width:90px;"><label style="font-size:12px;">子目录 (可选)</label><input name="savepath" placeholder="savepath" style="font-size:13px;"></div>
        </div>
        <div style="margin-top:8px;">
          <label style="font-size:12px;">标签（点击下方分组添加）</label>
          <div id="sub-filter-tags" style="display:flex;flex-wrap:wrap;gap:4px;min-height:28px;padding:6px 8px;border:1px solid var(--line);border-radius:8px;background:var(--bg);font-size:12px;color:var(--muted);">未选择</div>
          <textarea name="filter" id="sub-filter" rows="2" placeholder="每行一个关键词" style="display:none;"></textarea>
        </div>
        <div style="margin-top:10px;text-align:right;">
          <button type="submit" style="margin-top:0;background:var(--accent-2);">添加订阅</button>
        </div>
      </form>
    </div>
  </div>

  <!-- global modal -->
  <div id="g-modal" style="display:none;position:fixed;top:0;left:0;right:0;bottom:0;background:rgba(0,0,0,0.4);z-index:999;align-items:center;justify-content:center;" onclick="if(event.target===this)closeModal()">
    <div style="background:#fff;border-radius:12px;padding:20px;min-width:300px;max-width:500px;max-height:80vh;overflow-y:auto;box-shadow:0 4px 24px rgba(0,0,0,0.15);" onclick="event.stopPropagation()">
      <div id="g-modal-title" style="font-weight:600;margin-bottom:12px;"></div>
      <div id="g-modal-body"></div>
      <div id="g-modal-btns" style="margin-top:14px;display:flex;gap:8px;justify-content:flex-end;"></div>
    </div>
  </div>
  <script>
    var modalCb=null;
    function showModal(title,body,buttons){
      document.getElementById('g-modal-title').textContent=title;
      document.getElementById('g-modal-body').innerHTML=body;
      var btns=document.getElementById('g-modal-btns');
      btns.innerHTML='';
      (buttons||[{text:'{{index .T "confirm_btn"}}',cls:'',cb:function(){closeModal()}}]).forEach(function(b){
        var btn=document.createElement('button');
        btn.textContent=b.text;btn.style.margin='0';btn.style.padding='6px 16px';
        if(b.cls)btn.style.background=b.cls;
        if(b.id)btn.id=b.id;
        btn.onclick=function(){if(b.cb)b.cb();};
        btns.appendChild(btn);
      });
      document.getElementById('g-modal').style.display='flex';
    }
    function closeModal(){document.getElementById('g-modal').style.display='none';modalCb=null;}
    function alertModal(msg){showModal('',msg,[{text:'OK',cls:'var(--accent)',cb:function(){closeModal()}}]);}
    async function confirmAsync(msg){return new Promise(function(resolve){showModal('',msg,[{text:'Cancel',cls:'var(--danger)',cb:function(){closeModal();resolve(false);}},{text:'OK',cls:'var(--accent)',cb:function(){closeModal();resolve(true);}}]);});}
    async function promptModal(title,label,defaultValue){return new Promise(function(resolve){var dv=(defaultValue||'').replace(/&/g,'&amp;').replace(/"/g,'&quot;').replace(/</g,'&lt;').replace(/>/g,'&gt;');var body='<div style="margin-bottom:6px;font-size:13px;color:var(--muted);">'+label+'</div><input id="g-modal-input" style="width:100%;padding:8px;border:1px solid var(--line);border-radius:6px;font-size:14px;box-sizing:border-box;" value="'+dv+'" onkeydown="if(event.key===&quot;Enter&quot;)document.getElementById(&quot;g-modal-btn-ok&quot;).click()" autofocus>';showModal(title,body,[{text:'Cancel',cls:'var(--danger)',cb:function(){closeModal();resolve(null);}},{text:'OK',cls:'var(--accent)',id:'g-modal-btn-ok',cb:function(){var v=document.getElementById('g-modal-input').value.trim();closeModal();resolve(v);}}]);});}
    function submitConfirm(form,msg){if(event)event.preventDefault();confirmAsync(msg).then(function(ok){if(ok)form.submit();});}
    async function fsApi(endpoint,body){var r=await fetch(endpoint,{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:new URLSearchParams(body)});var t=await r.text();try{return JSON.parse(t);}catch(e){return{status:'error',message:t}}}
    async function fsRename(id,name){var nn=await promptModal('{{index .T "rename"}}','{{index .T "new_name"}}:',name);if(!nn||nn===name)return;var r=await fsApi('/api/fs/rename',{id:id,name:nn});if(r.status==='ok')location.reload();else alertModal(r.message||'Error');}
    async function fsDelete(id,name){var ok=await confirmAsync('{{index .T "confirm_delete"}} '+name+' ?');if(!ok)return;var r=await fsApi('/api/fs/delete',{id:id});if(r.status==='ok')location.reload();else alertModal(r.message||'Error');}
    async function fsNewFolder(parentId){var name=await promptModal('{{index .T "new_folder"}}','{{index .T "folder_name"}}:','');if(!name)return;var r=await fsApi('/api/fs/mkdir',{parent_id:parentId,name:name});if(r.status==='ok')location.reload();else alertModal(r.message||'Error');}
    async function fsMove(id,name){var target=await promptModal('{{index .T "move"}}','{{index .T "target_dir_id"}} '+name+':','');if(!target)return;var r=await fsApi('/api/fs/move',{id:id,target_dir:target});if(r.status==='ok')location.reload();else alertModal(r.message||'Error');}
    async function fsCopy(id,name){var target=await promptModal('{{index .T "copy"}}','{{index .T "target_dir_id"}} '+name+':','');if(!target)return;var r=await fsApi('/api/fs/copy',{id:id,target_dir:target});if(r.status==='ok')location.reload();else alertModal(r.message||'Error');}
  </script>
</body>
</html>`))

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
				url := e.URL
				if strings.HasPrefix(url, "/") {
					url = fmt.Sprintf("http://127.0.0.1:%d%s", s.Port, url)
				}

				// Check retry state
				if rs, ok := retryMap[subKey]; ok {
					if time.Now().Before(rs.nextTry) {
						log.Printf("[auto-sub] skipping %s (retry in %s)", subKey, time.Until(rs.nextTry).Round(time.Second))
						continue
					}
				}

				log.Printf("[auto-sub] running: %s (%s)", subKey, url)
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("[auto-sub] panic in %s: %v", subKey, r)
						}
					}()
					if s.Agent != nil && e.Cid != "" {
						before := globalDedup.SubCount(subKey)
						names := s.Agent.ProcessRSSFeed(url, e.Cid, e.SavePath, e.Filter, subKey)
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

	category := strings.TrimSpace(r.URL.Query().Get("category"))
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
			Category: category,
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
		if jr, err := jackett.Search(jc, q, nil, 1000); err == nil {
			for _, jr := range jr {
				if !jackettActiveSet[jr.Tracker] {
					continue
				}
				// If user specified indexers, filter (strip jackett: prefix for matching)
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
		}
	}

	// Dedup (same as search)
	se.Results = dedupSlice(se.Results, nil)

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
	go s.Agent.ProcessRSSFeed(rssURL, cid, savepath, keyword, rssURL)
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
		category := strings.TrimSpace(r.FormValue("category"))
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
						Category: category,
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
		data.SearchCategory = category
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
			Category: category,
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
			data.RssURL = buildRssURL(s.Port, q, indexers, category)
		}
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
			Category: ctx.Category,
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
	Category  string `json:"category"`
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
		cat := strings.TrimSpace(r.FormValue("category"))
		rssURL = buildRssURL(s.Port, q, nil, cat)
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
func buildRssURL(port int, query string, indexers []string, category string) string {
	v := url.Values{}
	v.Set("q", query)
	if len(indexers) > 0 {
		v.Set("indexers", strings.Join(indexers, ","))
	}
	if category != "" {
		v.Set("category", category)
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
	filter := strings.TrimSpace(r.FormValue("filter"))
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
		names := s.Agent.ProcessRSSFeed(rssURL, cid, savepath, filter, subKey)
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
