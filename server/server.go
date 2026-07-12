package server

import (
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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/deadblue/elevengo"
	"github.com/mguyenanastacio-glitch/pan-fetcher/config"
	"github.com/mguyenanastacio-glitch/pan-fetcher/indexer"
	p115pkg "github.com/mguyenanastacio-glitch/pan-fetcher/p115"
	"github.com/mguyenanastacio-glitch/pan-fetcher/rsssite"
)

type Agent interface {
	AddMagnetTask([]string, string, string)
	QuickGrabRSS(rssURL, cid, savepath, keyword, subKey string)
	OfflineClear(int) error
	ListTasks() ([]p115pkg.TaskItem, error)
	ListDir(string) ([]p115pkg.DirEntry, error)
	GetEntry(string) (p115pkg.DirEntry, error)
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
	ProxyHTTP string
	fsCache   map[string]fsCacheEntry
	fsCacheMu sync.Mutex
	IdxMgr    *indexer.Manager
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
	ShowQR        bool
	Cookies       string
	SearchQuery   string
	SearchResults []indexer.SearchResult
	SearchErrors  map[string]string
	SearchCategory string
	SearchSort     string
	SearchIndexers []string
	RssURL         string
	SavedSearches  []savedSearch
	IndexerList    []indexer.IndexerInfo
	IndexerLibrary []indexer.IndexerInfo
	DedupEntries   []dedupEntry
}

type dedupEntry struct {
	SubKey string
	Count  int
}

var srv *http.Server

var rssJsonPath = "rss.json"
var dedupCachePath = "dedup-cache.json"

// ---------- dedup + torrent URL cache (unified) ----------

// dedup-cache.json format:
// { "_torrent_urls": {"https://...": "hash"}, "subKey1": ["hash1",...], ... }

const torrentURLsKey = "_torrent_urls"

type dedupCache struct {
	mu          sync.Mutex
	subs        map[string]map[string]bool // subKey -> infoHash -> true
	torrentURLs map[string]string          // .torrent URL -> info hash
}

var globalDedup = &dedupCache{
	subs:        make(map[string]map[string]bool),
	torrentURLs: make(map[string]string),
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
		} else {
			var list []string
			json.Unmarshal(v, &list)
			set := make(map[string]bool, len(list))
			for _, h := range list {
				set[h] = true
			}
			d.subs[k] = set
		}
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
	d.subs[subKey][h] = true
}

// RemoveSub deletes all dedup entries for a given subKey.
func (d *dedupCache) RemoveSub(subKey string) {
	d.mu.Lock()
	defer d.mu.Unlock()
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
	d.saveLocked()
}

func (d *dedupCache) saveLocked() {
	raw := make(map[string]interface{}, len(d.subs)+1)
	if len(d.torrentURLs) > 0 {
		urls := make(map[string]string, len(d.torrentURLs))
		for url, hash := range d.torrentURLs {
			urls[url] = hash
		}
		raw[torrentURLsKey] = urls
	}
	for k, set := range d.subs {
		list := make([]string, 0, len(set))
		for h := range set {
			list = append(list, h)
		}
		raw[k] = list
	}
	data, _ := json.Marshal(raw)
	os.WriteFile(dedupCachePath, data, 0644)
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
	delete(d.torrentURLs, url)
	d.saveLocked()
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
	// Strip magnet:? prefix if present
	m := magnet
	if strings.HasPrefix(m, "magnet:?") {
		m = m[8:] // len("magnet:?") = 8
	}
	for _, part := range strings.Split(m, "&") {
		if strings.HasPrefix(strings.ToLower(part), "xt=urn:btih:") {
			return strings.ToLower(strings.TrimPrefix(part, "xt=urn:btih:"))
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
	DisableCache  bool   `json:"disable_cache"`
	WebPassword   string `json:"web_password"`
	SubsInterval  int    `json:"subs_interval"`
}

func (s *Server) loadWebSettings() webSettings {
	data, err := os.ReadFile("web-settings.json")
	if err != nil {
		return webSettings{Lang: "zh"}
	}
	var ws webSettings
	json.Unmarshal(data, &ws)
	if ws.Lang == "" {
		ws.Lang = "zh"
	}
	return ws
}

func (s *Server) saveWebSettings(ws webSettings) {
	data, _ := json.MarshalIndent(ws, "", "  ")
	os.WriteFile("web-settings.json", data, 0644)
}

// ---------- log buffer ----------

type logBuffer struct {
	mu   sync.Mutex
	buf  []string
	size int
	pos  int
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
	return len(p), nil
}

func (lb *logBuffer) Lines() []string {
	lb.mu.Lock()
	defer lb.mu.Unlock()
	out := make([]string, 0, lb.size)
	// Return newest-first, up to size
	total := lb.pos
	if total > lb.size {
		total = lb.size
	}
	for i := 0; i < total; i++ {
		idx := (lb.pos - 1 - i) % lb.size
		if idx < 0 {
			idx += lb.size
		}
		out = append(out, lb.buf[idx])
	}
	return out
}

var logBuf = newLogBuffer(100)

func init() {
	log.SetOutput(io.MultiWriter(logBuf, os.Stderr))
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
		"home":            "🏠 首页",
		"files":           "📂 文件浏览",
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
		"runtime_log":     "运行日志",
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
		"disable_cache":   "禁用缓存",
		"web_pw":          "Web 管理密码 (留空=无认证)",
		"web_pw_ph":       "设置登录密码",
		"save":            "保存",
		"db_path":         "DB 路径",
		"cloud_files":     "云文件浏览",
		"root_dir":        "根目录",
		"name":            "名称",
		"size":            "大小",
		"empty_dir":       "此目录为空",
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
		"conn_ok":         "115 连接正常",
		"login_title":     "🔐 pan-fetcher",
		"login_pw_ph":     "输入管理密码",
		"login_btn":       "登录",
		"login_err":       "密码错误",
		"lang_label":      "界面语言",
		"search":          "🔍 资源搜索",
		"search_ph":       "输入关键词搜索资源...",
		"search_btn":      "搜索",
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
		"idx_no_defs":     "暂无索引器定义文件，请在 indexers/ 目录添加 YAML 文件。",
		"idx_no_active":   "暂无激活的索引器，请从下方索引器库添加。",
		"idx_library":     "📚 索引器库",
		"idx_lib_empty":   "索引器库为空。",
		"idx_batch_add":   "批量添加选中",
		"dedup":           "🗄️ 缓存库",
		"dedup_title":     "缓存库",
		"dedup_empty":     "暂无缓存记录。订阅自动执行后会自动记录已下载的种子。",
		"dedup_clear_sub": "清空此项",
		"torrent":         "🧲 缓存库",
		"torrent_title":   "Torrent 缓存库",
		"torrent_empty":   "暂无缓存记录。本地聚合 RSS 首次遇到新 .torrent 时会下载转换并缓存。",
		"torrent_clear":   "清空缓存",
	},
	"en": {
		"title":           "pan-fetcher Dashboard",
		"logout":          "Logout",
		"home":            "🏠 Home",
		"files":           "📂 Files",
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
		"runtime_log":     "Runtime Log",
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
		"disable_cache":   "Disable Cache",
		"web_pw":          "Web Password (empty=no auth)",
		"web_pw_ph":       "Set login password",
		"save":            "Save",
		"db_path":         "DB Path",
		"cloud_files":     "Cloud Files",
		"root_dir":        "Root",
		"name":            "Name",
		"size":            "Size",
		"empty_dir":       "This directory is empty",
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
		"conn_ok":         "115 connection OK",
		"login_title":     "🔐 pan-fetcher",
		"login_pw_ph":     "Enter password",
		"login_btn":       "Login",
		"login_err":       "Wrong password",
		"lang_label":      "Language",
		"search":          "🔍 Search",
		"search_ph":       "Enter keywords to search...",
		"search_btn":      "Search",
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
		"idx_no_defs":     "No indexer definitions found. Add YAML files to the indexers/ directory.",
		"idx_no_active":   "No active indexers. Add from the library below.",
		"idx_library":     "📚 Indexer Library",
		"idx_lib_empty":   "Indexer library is empty.",
		"idx_batch_add":   "Add Selected",
		"dedup":           "🗄️ Cache",
		"dedup_title":     "Cache Library",
		"dedup_empty":     "No cache records yet. They are created automatically when subscriptions run.",
		"dedup_clear_sub": "Clear",
		"torrent":         "🧲 Cache",
		"torrent_title":   "Torrent Cache Library",
		"torrent_empty":   "No cache entries yet. Created when the local aggregated RSS first encounters a new .torrent URL.",
		"torrent_clear":   "Clear Cache",
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

// ---------- template ----------

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
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
    .sidebar-search { padding: 8px 12px; }
    .sidebar-search input {
      width: 100%; padding: 7px 10px; font-size: 12px; border-radius: 10px;
      border: 1px solid var(--line); background: var(--bg); cursor: pointer;
    }
    .sidebar-search input:focus { outline: none; border-color: var(--accent); background: #fff; }
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
    }
    .card h2 { margin: 0 0 8px; font-size: 17px; }
    .meta { display: flex; gap: 10px; flex-wrap: wrap; margin: 8px 0 0; color: var(--muted); font-size: 13px; }
    label { display: block; margin: 8px 0 4px; font-size: 13px; color: var(--muted); }
    input, textarea, select {
      width: 100%;
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
    .breadcrumb { display: flex; flex-wrap: wrap; align-items: center; gap: 0; margin-bottom: 10px; font-size: 13px; }
    .crumb { color: var(--accent); text-decoration: none; }
    .crumb:hover { text-decoration: underline; }
    .crumb-sep { color: var(--muted); margin: 0 4px; }
    .fs-tbl td.muted { color: var(--muted); font-size: 12px; }
    .fs-tbl td.mono { font-family: monospace; font-size: 11px; }
    .topbar { margin-bottom: 18px; }
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
    function toggleSidebar(){
      var sb=document.getElementById('sidebar');
      var mn=document.getElementById('main');
      if(sb){sb.classList.toggle('collapsed');}
      if(mn){mn.classList.toggle('expanded');}
    }
    async function restartServer(){
      if(!confirm('确定要重启服务吗？'))return;
      try{
        var r=await fetch('/settings/restart',{method:'POST'});
        var j=await r.json();
        if(j.ok){alert('服务正在重启，请稍候刷新页面…');}
        else{alert('重启失败: '+j.msg);}
      }catch(e){alert('重启请求失败: '+e.message);}
    }
  </script>
</head>
<body>
  <!-- left sidebar -->
  <div class="sidebar" id="sidebar">
    <div class="sidebar-logo">pan-fetcher</div>
    <div class="sidebar-search">
      <input type="text" id="quick-search-input" placeholder="🔍 搜索…" value="{{.SearchQuery}}" readonly onclick="openSearchModal()" autocomplete="off">
    </div>
    <div class="sidebar-nav">
      <a href="/"{{if or (eq .Page "home") (eq .Page "")}} class="active"{{end}}>{{index .T "home"}}</a>
      <a href="/indexers"{{if eq .Page "indexers"}} class="active"{{end}}>{{index .T "indexers"}}</a>
      <a href="/fs"{{if eq .Page "fs"}} class="active"{{end}}>{{index .T "files"}}</a>
      <a href="/subs"{{if eq .Page "subs"}} class="active"{{end}}>{{index .T "subs"}}</a>
      <a href="/dedup"{{if eq .Page "dedup"}} class="active"{{end}}>{{index .T "dedup"}}</a>
      <a href="/tasks"{{if eq .Page "tasks"}} class="active"{{end}}>📥 离线任务</a>
      <a href="/log"{{if eq .Page "log"}} class="active"{{end}}>📜 运行日志</a>
      <a href="/settings"{{if eq .Page "settings"}} class="active"{{end}}>{{index .T "settings"}}</a>
    </div>
    <div class="sidebar-footer">
      <a href="/logout" class="logout-text">{{index .T "logout"}}</a>
    </div>
  </div>

  <!-- main content -->
  <div class="main" id="main">
    <div class="topbar">
      <button class="sidebar-toggle-btn" onclick="toggleSidebar()" title="折叠侧边栏">☰</button>
    </div>

    {{if .Message}}<div class="status ok">{{.Message}}</div>{{end}}
    {{if .Error}}<div class="status err">{{.Error}}</div>{{end}}

    {{if or (eq .Page "home") (eq .Page "")}}
    <div class="grid">
      <div class="card">
        <h2>{{index .T "add_magnet"}}</h2>
        <form action="/add" method="post">
          <label for="tasks">{{index .T "task_url"}}</label>
          <textarea id="tasks" name="tasks" placeholder="{{index .T "task_url_ph"}}"></textarea>
          <label for="cid">{{index .T "cid"}}</label>
          <div style="display:flex;gap:4px;">
            <input id="cid" name="cid" placeholder="{{index .T "cid_ph"}}" style="flex:1;">
            <button type="button" onclick="browseDirsFor('cid')" style="margin:0;padding:4px 8px;font-size:12px;background:var(--accent-2);white-space:nowrap;">📁 浏览</button>
          </div>
          <label for="savepath">{{index .T "savepath"}}</label>
          <input id="savepath" name="savepath" placeholder="{{index .T "savepath_ph"}}">
          <button type="submit">{{index .T "submit_task"}}</button>
        </form>
        <div class="hint">{{index .T "json_api"}} <code>POST /add</code></div>
      </div>

      <div class="card">
        <h2>{{index .T "quick_grab"}}</h2>
        <form action="/rss/quick" method="post">
          <label for="rss-url">{{index .T "rss_url"}}</label>
          <input id="rss-url" name="rss_url" placeholder="{{index .T "rss_url_ph"}}" required>
          <label for="quick-keyword">{{index .T "keyword_opt"}}</label>
          <input id="quick-keyword" name="keyword" placeholder="{{index .T "keyword_ph"}}">
          <label for="quick-cid">{{index .T "target_cid"}}</label>
          <div style="display:flex;gap:4px;">
            <input id="quick-cid" name="cid" placeholder="{{index .T "target_cid_ph"}}" required style="flex:1;">
            <button type="button" onclick="browseDirsFor('quick-cid')" style="margin:0;padding:4px 8px;font-size:12px;background:var(--accent-2);white-space:nowrap;">📁 浏览</button>
          </div>
          <button type="submit">{{index .T "grab_btn"}}</button>
        </form>
        <div class="hint">{{index .T "grab_hint"}}</div>
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
      {{if .FSEntries}}
      <table class="tbl fs-tbl">
        <thead><tr><th></th><th>{{index .T "name"}}</th><th>{{index .T "size"}}</th><th>ID</th></tr></thead>
        <tbody>
        {{if ne .FSCurrentID "0"}}<tr><td>⬆</td><td><a href="/fs?dir={{.FSParentID}}">..</a></td><td></td><td></td></tr>{{end}}
        {{range .FSEntries}}<tr>
          <td>{{.Icon}}</td>
          <td>{{if .IsDir}}<a href="/fs?dir={{.ID}}">{{.Name}}</a>{{else}}{{.Name}}{{end}}</td>
          <td class="muted">{{.Size}}</td>
          <td class="muted mono">{{.ID}}</td>
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
        <span style="font-weight:400;font-size:12px;color:var(--muted);margin-left:8px;">通过资源搜索页 📌 订阅此搜索 添加</span>
      </h2>
      {{if .RssSubs}}
      <table class="tbl">
        <thead><tr><th>{{index .T "name"}}</th><th>RSS</th><th>CID</th><th>过滤</th><th>{{index .T "sub_status"}}</th><th></th></tr></thead>
        <tbody>
        {{range .RssSubs}}<tr>
          <td><strong>{{.Name}}</strong><br><small class="muted">{{.Site}}</small></td>
          <td class="muted" style="font-size:11px;max-width:180px;"><div style="overflow:hidden;text-overflow:ellipsis;white-space:nowrap;" title="{{.URL}}">{{.URL}}</div></td>
          <td class="muted mono">{{.Cid}}</td>
          <td class="muted">{{.Filter}}</td>
          <td>
            <form action="/subs" method="post" style="display:inline;">
              <input type="hidden" name="action" value="toggle">
              <input type="hidden" name="site" value="{{.Site}}">
              <input type="hidden" name="name" value="{{.Name}}">
              <button type="submit" style="padding:2px 8px;font-size:11px;margin:0;{{if .Enabled}}background:var(--accent-2);{{else}}background:var(--danger);{{end}}">{{if .Enabled}}启用{{else}}禁用{{end}}</button>
            </form>
          </td>
          <td style="white-space:nowrap;">
            <form action="/subs/run" method="post" style="display:inline;">
              <input type="hidden" name="rss_url" value="{{.URL}}">
              <input type="hidden" name="cid" value="{{.Cid}}">
              <input type="hidden" name="savepath" value="{{.SavePath}}">
              <input type="hidden" name="filter" value="{{.Filter}}">
              <button type="submit" style="padding:2px 6px;font-size:11px;margin:0;background:var(--accent);" title="立即执行">▶</button>
            </form>
            <button onclick="editSub('{{.Site}}','{{.Name}}','{{.Cid}}','{{.SavePath}}','{{.Filter}}')" style="padding:2px 6px;font-size:11px;margin:0;">✎</button>
            <form action="/subs" method="post" style="display:inline;">
              <input type="hidden" name="action" value="delete">
              <input type="hidden" name="site" value="{{.Site}}">
              <input type="hidden" name="name" value="{{.Name}}">
              <button type="submit" style="background:var(--danger);padding:2px 8px;font-size:11px;margin:0;" onclick="return confirm('删除？')">✕</button>
            </form>
          </td>
        </tr>{{end}}
        </tbody>
      </table>
      {{else}}
      <div class="hint">暂无订阅，请在 <a href="/search">资源搜索</a> 页面搜索后点击 📌 订阅此搜索 添加。</div>
      {{end}}
      <!-- edit modal -->
      <div id="edit-sub-modal" style="display:none;margin-top:12px;padding:14px;background:#f8fafc;border:1px solid var(--line);border-radius:10px;">
        <h3 style="margin:0 0 10px;">编辑订阅</h3>
        <form action="/subs" method="post">
          <input type="hidden" name="action" value="edit">
          <input type="hidden" name="site" id="edit-site">
          <input type="hidden" name="name" id="edit-name">
          <div style="display:flex;gap:8px;flex-wrap:wrap;align-items:flex-end;">
            <div><label>CID</label><input name="cid" id="edit-cid" style="font-size:13px;width:120px;"></div>
            <div><label>子目录</label><input name="savepath" id="edit-savepath" style="font-size:13px;width:100px;"></div>
            <div><label>过滤</label><input name="filter" id="edit-filter" style="font-size:13px;width:100px;"></div>
            <button type="submit" style="margin-top:0;">保存</button>
            <button type="button" onclick="document.getElementById('edit-sub-modal').style.display='none'" style="margin-top:0;background:var(--danger);">取消</button>
          </div>
        </form>
      </div>
      <script>
        function editSub(site,name,cid,sp,f){
          document.getElementById('edit-sub-modal').style.display='block';
          document.getElementById('edit-site').value=site;
          document.getElementById('edit-name').value=name;
          document.getElementById('edit-cid').value=cid;
          document.getElementById('edit-savepath').value=sp;
          document.getElementById('edit-filter').value=f;
        }
      </script>
      {{if .Agent}}<div style="margin-top:10px;">
        <form action="/subs/run" method="post">
          <input name="rss_url" placeholder="{{index .T "sub_rss_ph"}}" style="width:auto;min-width:300px;display:inline;">
          <button type="submit" style="margin-top:0;">{{index .T "sub_run"}}</button>
        </form>
      </div>{{end}}
    </div>
    {{end}}

    <!-- offline task list -->
    {{if eq .Page "tasks"}}
    <div class="card panel">
      <h2>{{index .T "offline_tasks"}} ({{.TaskCount}})
        <span style="font-weight:400;font-size:12px;margin-left:8px;">
          <span id="tab-downloading" style="cursor:pointer;color:var(--accent);border-bottom:2px solid var(--accent);" onclick="switchTaskTab('downloading')">下载中 <span id="cnt-downloading"></span></span>
          <span style="margin:0 8px;color:var(--line);">|</span>
          <span id="tab-failed" style="cursor:pointer;color:var(--muted);" onclick="switchTaskTab('failed')">失败 <span id="cnt-failed"></span></span>
          <span style="margin:0 8px;color:var(--line);">|</span>
          <span id="tab-done" style="cursor:pointer;color:var(--muted);" onclick="switchTaskTab('done')">已完成 <span id="cnt-done"></span></span>
        </span>
      </h2>
      <form action="/clear" method="post" style="margin-bottom:8px;">
        <select name="type" style="width:auto;display:inline;">
          <option value="1">{{index .T "clear_done"}}</option>
          <option value="3">{{index .T "clear_failed"}}</option>
          <option value="2">{{index .T "clear_all"}}</option>
        </select>
        <button type="submit" style="margin-top:0;padding:6px 12px;font-size:13px;">{{index .T "execute"}}</button>
      </form>
      {{if .Tasks}}
      <table class="tbl" id="task-table">
        <thead><tr><th>{{index .T "name"}}</th><th>{{index .T "size"}}</th><th style="width:60px;">%</th><th style="width:40px;"></th></tr></thead>
        <tbody>
        {{range .Tasks}}<tr class="{{.RowClass}}" data-status="{{.Status}}" data-url="{{.URL}}">
          <td title="{{.InfoHash}}">{{.Name}}</td>
          <td>{{.Size}}</td>
          <td>{{printf "%.0f" .Percent}}%</td>
          <td><button onclick="copyTaskURL('{{.URL}}')" style="padding:2px 6px;font-size:10px;margin:0;" title="复制链接">📋</button></td>
        </tr>{{end}}
        </tbody>
      </table>
      {{else}}
      <div class="hint">{{index .T "no_tasks"}}</div>
      {{end}}
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
          prompt('复制以下链接:',url);
        });
      }
      // init
      (function(){
        var rows=document.querySelectorAll('#task-table tbody tr');
        var c={downloading:0,failed:0,done:0};
        rows.forEach(function(r){c[r.getAttribute('data-status')]=(c[r.getAttribute('data-status')]||0)+1;});
        document.getElementById('cnt-downloading').textContent='('+c.downloading+')';
        document.getElementById('cnt-failed').textContent='('+c.failed+')';
        document.getElementById('cnt-done').textContent='('+c.done+')';
        switchTaskTab('downloading');
      })();
    </script>
    {{end}}

    <!-- log panel -->
    {{if eq .Page "log"}}
    <div class="card panel">
      <h2>{{index .T "runtime_log"}}</h2>
      <div class="log-panel" style="max-height:none;">{{range .Logs}}{{.}}
{{end}}</div>
    </div>
    {{end}}

    <!-- search modal (replaces /search page) -->
    <div class="smodal-overlay" id="search-modal">
      <div class="smodal-card">
        <div style="display:flex;align-items:center;justify-content:space-between;margin-bottom:16px;">
          <h2>{{index .T "search"}}</h2>
          <button onclick="closeSearchModal()" style="margin:0;padding:4px 12px;background:var(--danger);font-size:12px;">✕</button>
        </div>
        <form action="/search" method="post" id="search-form">
          <div style="display:flex;gap:8px;flex-wrap:wrap;align-items:flex-end;">
            <input name="q" id="search-q" placeholder="{{index .T "search_ph"}}" value="{{.SearchQuery}}" style="flex:3;min-width:160px;">
            <select name="category" style="flex:1;min-width:100px;">
              <option value="">全部分类</option>
              <option value="anime"{{if eq .SearchCategory "anime"}} selected{{end}}>动漫</option>
              <option value="tv"{{if eq .SearchCategory "tv"}} selected{{end}}>剧集</option>
              <option value="movie"{{if eq .SearchCategory "movie"}} selected{{end}}>电影</option>
              <option value="music"{{if eq .SearchCategory "music"}} selected{{end}}>音乐</option>
              <option value="other"{{if eq .SearchCategory "other"}} selected{{end}}>其他</option>
            </select>
            <select name="sort" style="flex:1;min-width:100px;">
              <option value="seeds"{{if eq .SearchSort "seeds"}} selected{{end}}>按做种数</option>
              <option value="size"{{if eq .SearchSort "size"}} selected{{end}}>按大小</option>
              <option value="date"{{if eq .SearchSort "date"}} selected{{end}}>按时间</option>
            </select>
            <button type="submit" style="margin-top:0;white-space:nowrap;">{{index .T "search_btn"}}</button>
          </div>
          {{if .IndexerList}}
          <div style="margin-top:8px;display:flex;flex-wrap:wrap;gap:4px;align-items:center;">
            <span style="font-size:12px;color:var(--muted);white-space:nowrap;">搜索站点:</span>
            {{range .IndexerList}}
            <label style="font-size:11px;display:flex;align-items:center;gap:2px;cursor:pointer;padding:2px 6px;background:var(--bg);border:1px solid var(--line);border-radius:6px;">
              <input type="checkbox" name="indexer" value="{{.ID}}" style="width:auto;margin:0;"{{if indexerChecked $.SearchIndexers .ID}} checked{{end}}>
              {{.Name}}
            </label>
            {{end}}
          </div>
          {{end}}
          <div style="margin-top:6px;display:flex;gap:8px;">
            <button type="button" onclick="toggleSubForm()" style="margin-top:0;padding:4px 12px;font-size:11px;background:var(--accent-2);white-space:nowrap;">📌 订阅此搜索</button>
          </div>
        </form>
        <!-- subscription form (hidden) -->
        <div id="sub-form" style="display:none;margin-top:12px;padding:14px;background:#f8fafc;border:1px solid var(--line);border-radius:10px;">
          <h3 style="margin:0 0 10px;">📌 添加 RSS 订阅</h3>
          <form action="/search/subscribe" method="post">
            <input type="hidden" name="query" value="{{.SearchQuery}}">
            <input type="hidden" name="category" value="{{.SearchCategory}}">
            <div style="display:flex;gap:8px;flex-wrap:wrap;align-items:flex-end;">
              <div style="flex:2;min-width:140px;">
                <label style="font-size:12px;">名称</label>
                <input name="name" placeholder="订阅名称" value="{{.SearchQuery}}" style="font-size:13px;">
              </div>
              <div style="flex:3;min-width:200px;">
                <label style="font-size:12px;">RSS 地址</label>
                <input name="url" placeholder="RSS 地址" value="{{.RssURL}}" style="font-size:13px;">
              </div>
              <div style="flex:1;min-width:80px;">
                <label style="font-size:12px;">过滤 (可选)</label>
                <input name="filter" placeholder="关键词" style="font-size:13px;">
              </div>
            </div>
            <div style="display:flex;gap:8px;flex-wrap:wrap;align-items:flex-end;margin-top:8px;">
              <div style="flex:1;min-width:140px;">
                <label style="font-size:12px;">115 目录 ID (可选)
                  <button type="button" onclick="browseDirs()" style="padding:2px 8px;font-size:11px;margin:0 0 0 4px;background:var(--accent-2);">📁 浏览</button>
                </label>
                <input name="cid" id="sub-cid" placeholder="cid" style="font-size:13px;">
              </div>
              <div style="flex:1;min-width:100px;">
                <label style="font-size:12px;">子目录 (可选)</label>
                <input name="savepath" placeholder="savepath" style="font-size:13px;">
              </div>
              <button type="submit" style="margin-top:0;background:var(--accent-2);">添加订阅</button>
            </div>
          </form>
        </div>
        <!-- search results -->
        <div id="search-results-area">
          {{if .SearchResults}}
          <h3 style="margin-top:14px;">{{index .T "search_results"}} ({{len .SearchResults}})</h3>
          <table class="tbl">
            <thead><tr><th>{{index .T "name"}}</th><th>{{index .T "size"}}</th><th>S</th><th>{{index .T "search_from"}}</th><th></th></tr></thead>
            <tbody>
            {{range .SearchResults}}<tr>
              <td>{{if .PageURL}}<a href="{{.PageURL}}" target="_blank">{{.Title}}</a>{{else}}{{.Title}}{{end}}</td>
              <td class="muted">{{.SizeFmt}}</td>
              <td>{{.Seeders}}</td>
              <td class="muted">{{.IndexerName}}</td>
              <td>
                {{if .MagnetURL}}<form action="/add" method="post" style="display:inline;">
                  <input type="hidden" name="tasks" value="{{.MagnetURL}}">
                  <input type="hidden" name="cid" value="">
                  <button type="submit" style="background:var(--accent-2);padding:2px 8px;font-size:11px;margin:0;">+</button>
                </form>{{end}}
              </td>
            </tr>{{end}}
            </tbody>
          </table>
          {{else}}{{if .SearchQuery}}
          <div class="hint" style="margin-top:12px;">{{index .T "search_no_result"}}</div>
          {{if .SearchErrors}}
          <div style="margin-top:8px;font-size:12px;">
            {{range $id, $err := .SearchErrors}}
            <div style="padding:4px 8px;margin:2px 0;background:#fef2f2;color:#991b1b;border-radius:6px;word-break:break-all;">⚠ {{$err}}</div>
            {{end}}
          </div>
          {{end}}
          {{end}}{{end}}
        </div>
        <!-- saved searches -->
        {{if .SavedSearches}}
        <div style="margin-top:16px;padding-top:12px;border-top:1px solid var(--line);">
          <h3 style="margin:0 0 8px;">📌 已保存的搜索</h3>
          {{range .SavedSearches}}
          <div style="display:flex;align-items:center;gap:8px;padding:4px 0;font-size:13px;">
            <span style="flex:1;">🔍 {{.Query}}{{if .Category}} <span style="color:var(--muted);font-size:11px;">[{{.Category}}]</span>{{end}}</span>
            <form action="/search" method="post" style="display:inline;">
              <input type="hidden" name="q" value="{{.Query}}">
              <input type="hidden" name="category" value="{{.Category}}">
              <input type="hidden" name="sort" value="{{.Sort}}">
              <button type="submit" style="padding:2px 8px;font-size:11px;margin:0;">搜索</button>
            </form>
            <form action="/search" method="post" style="display:inline;">
              <input type="hidden" name="action" value="unsubscribe">
              <input type="hidden" name="id" value="{{.ID}}">
              <button type="submit" style="padding:2px 8px;font-size:11px;margin:0;background:var(--danger);">删除</button>
            </form>
          </div>
          {{end}}
        </div>
        {{end}}
      </div>
    </div>
    <script>
      function openSearchModal(){
        document.getElementById('search-modal').classList.add('active');
        document.getElementById('search-q').focus();
      }
      function closeSearchModal(){
        document.getElementById('search-modal').classList.remove('active');
      }
      // Close on overlay click
      document.getElementById('search-modal').addEventListener('click',function(e){
        if(e.target===this)closeSearchModal();
      });
      // Close on Escape
      document.addEventListener('keydown',function(e){
        if(e.key==='Escape')closeSearchModal();
      });
      // Auto-open if search was performed (results or error present)
      {{if or .SearchQuery .Error}}
      openSearchModal();
      {{end}}
      // Show error in modal if present
      {{if .Error}}
      (function(){
        var area=document.getElementById('search-results-area');
        var div=document.createElement('div');
        div.style.cssText='margin-top:12px;padding:10px 12px;background:#fef2f2;color:#991b1b;border-radius:10px;font-size:14px;';
        div.textContent='{{.Error}}';
        area.insertBefore(div,area.firstChild);
      })();
      {{end}}

      function toggleSubForm(){
        var el=document.getElementById('sub-form');
        el.style.display=el.style.display==='none'?'block':'none';
      }
      var browseTargetId='sub-cid';
      function browseDirsFor(targetId){browseTargetId=targetId;browseDirs('0');}
      async function browseDirs(pid){
        if(!pid)pid='0';
        try{
          let r=await fetch('/subs/dirs?pid='+pid);
          let j=await r.json();
          if(!j.ok){showModal('错误','<p>'+j.msg+'</p>');return;}
          if(!j.entries)j.entries=[];
          if(!Array.isArray(j.entries))j.entries=[];
          var html='<div style="max-height:300px;overflow-y:auto;">';
          if(pid!=='0'){
            html+='<div style="cursor:pointer;padding:6px 8px;color:var(--accent-2);border-radius:6px;" onclick="browseDirs(\''+j.parent+'\')">📁 .. (上级)</div>';
          }
          if(j.entries.length===0)html+='<p style="color:var(--muted);">此目录下没有子文件夹</p>';
          j.entries.forEach(function(e){
            html+='<div style="cursor:pointer;padding:6px 8px;margin:2px 0;border-radius:6px;display:flex;align-items:center;gap:8px;" onmouseover="this.style.background=\'#f0f4ff\'" onmouseout="this.style.background=\'\'">';
            html+='📁 <span style="flex:1;cursor:pointer;" onclick="browseDirs(\''+e.id+'\')">'+e.name+'</span>';
            html+='<code style="font-size:11px;color:var(--muted);cursor:pointer;" onclick="document.getElementById(browseTargetId).value=\''+e.id+'\';closeModal()" title="选定此目录">'+e.id+'</code></div>';
          });
          html+='</div>';
          updateBrowseModal('选择 115 目录 (当前: '+pid+')',html);
        }catch(e){showModal('错误','<p>'+e.message+'</p>');}
      }
      function updateBrowseModal(title,body){
        document.getElementById('g-modal-title').textContent=title;
        document.getElementById('g-modal-body').innerHTML=body;
        var btns=document.getElementById('g-modal-btns');
        btns.innerHTML='<button onclick="closeModal()" style="margin:0;padding:6px 16px;background:var(--danger);">关闭</button>';
        document.getElementById('g-modal').style.display='flex';
      }
    </script>

    <!-- dedup library -->
    {{if eq .Page "dedup"}}
    <div class="card panel">
      <h2>{{index .T "dedup_title"}}</h2>
      {{if .DedupEntries}}
      <table class="tbl">
        <thead><tr><th>订阅名称</th><th>缓存数</th><th></th></tr></thead>
        <tbody>
        {{range .DedupEntries}}<tr>
          <td>
            <span style="cursor:pointer;user-select:none;" onclick="toggleDedupHashes('{{.SubKey}}',this)">▶ {{.SubKey}}</span>
          </td>
          <td class="muted"><span id="cnt-{{.SubKey}}">{{.Count}}</span> 条</td>
          <td>
            <form action="/dedup/clear" method="post" style="display:inline;">
              <input type="hidden" name="sub" value="{{.SubKey}}">
              <button type="submit" style="padding:2px 8px;font-size:11px;margin:0;background:var(--danger);" onclick="return confirm('确定清空 {{.SubKey}} 的缓存记录？')">{{index $.T "dedup_clear_sub"}}</button>
            </form>
          </td>
        </tr>
        <tr id="hashes-{{.SubKey}}" style="display:none;"><td colspan="3" style="padding:0;">
          <div style="padding:4px 8px;background:#f8fafc;max-height:300px;overflow-y:auto;" id="hash-list-{{.SubKey}}">加载中…</div>
        </td></tr>
        {{end}}
        </tbody>
      </table>
      {{else}}
      <div class="hint">{{index .T "dedup_empty"}}</div>
      {{end}}
    </div>
    <script>
      var dedupCache={};
      async function toggleDedupHashes(subKey,el){
        var row=document.getElementById('hashes-'+subKey);
        if(row.style.display==='none'){
          row.style.display='table-row';
          el.textContent=el.textContent.replace('▶','▼');
          if(dedupCache[subKey]){
            document.getElementById('hash-list-'+subKey).innerHTML=dedupCache[subKey];
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
              var shortUrl=it.url?it.url.split('/').pop():'';
              html+='<tr style="border-bottom:1px solid #e8ecf1;">';
              html+='<td style="padding:3px 6px;font-family:monospace;color:#374151;">'+it.hash+'</td>';
              html+='<td style="padding:3px 6px;max-width:280px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;color:var(--muted);" title="'+(it.url||'')+'">'+(shortUrl||'(无)')+'</td>';
              html+='<td style="padding:3px 6px;text-align:right;">';
              if(it.url){
                html+='<form action="/torrent/clear" method="post" style="display:inline;"><input type="hidden" name="url" value="'+it.url+'"><button type="submit" style="padding:1px 6px;font-size:10px;margin:0;background:var(--danger);" onclick="return confirm(\'删除此 URL 映射？\')">✕</button></form>';
              }
              html+='</td></tr>';
            });
            html+='</table>';
            if(cnt===0)html='(暂无记录)';
            dedupCache[subKey]=html;
            document.getElementById('hash-list-'+subKey).innerHTML=html;
            document.getElementById('cnt-'+subKey).textContent=cnt;
          }catch(e){
            document.getElementById('hash-list-'+subKey).innerHTML='加载失败: '+e.message;
          }
        }else{
          row.style.display='none';
          el.textContent=el.textContent.replace('▼','▶');
        }
      }
    </script>
    {{end}}

    <!-- indexer management -->
    {{if eq .Page "indexers"}}
    <div class="card panel">
      <h2>{{index .T "indexer_list"}} ({{len .IndexerList}})
        {{if .IndexerList}}<button onclick="testAll()" style="margin:0 0 0 12px;padding:4px 12px;font-size:12px;background:var(--accent-2);">{{index .T "test_all"}}</button>{{end}}
      </h2>
      {{if .IndexerList}}
      <table class="tbl" id="idx-active">
        <thead><tr><th>{{index .T "name"}}</th><th>{{index .T "sub_type"}}</th><th>{{index .T "idx_site"}}</th><th>{{index .T "idx_health"}}</th><th>{{index .T "idx_tested"}}</th><th></th></tr></thead>
        <tbody>
        {{range .IndexerList}}<tr id="row-{{.ID}}">
          <td><strong>{{.Name}}</strong><br><small class="err-msg" style="color:var(--danger);">{{.LastError}}</small></td>
          <td class="muted">{{.Type}}</td>
          <td>{{if .SiteLink}}<a href="{{.SiteLink}}" target="_blank" class="muted" style="font-size:12px;">🔗</a>{{end}}</td>
          <td><span class="health-dot" style="color:{{if .Healthy}}var(--accent-2){{else}}var(--danger){{end}};">●</span></td>
          <td class="muted test-time" style="font-size:12px;">{{.LastTest}}</td>
          <td style="white-space:nowrap;">
            <button onclick="testIdx('{{.ID}}','{{.Name}}')" style="padding:2px 6px;font-size:11px;margin:0;background:var(--accent-2);">🔬</button>
            {{if .HasLogin}}<button onclick="showLogin('{{.ID}}','{{.Name}}')" style="padding:2px 6px;font-size:11px;margin:0;background:var(--warn);">🔑</button>{{end}}
            <button onclick="deactivateIdx('{{.ID}}')" style="padding:2px 6px;font-size:11px;margin:0;background:var(--danger);" title="移回索引器库">−</button>
          </td>
        </tr>{{end}}
        </tbody>
      </table>
      {{else}}
      <div class="hint">{{index .T "idx_no_active"}}</div>
      {{end}}
    </div>

    <!-- indexer library -->
    <div class="card panel" style="margin-top:16px;">
      <h2>{{index .T "idx_library"}} (<span id="lib-count">{{len .IndexerLibrary}}</span>)
        <button onclick="activateSelected()" style="margin:0 0 0 12px;padding:4px 12px;font-size:12px;background:var(--accent);">{{index .T "idx_batch_add"}}</button>
      </h2>
      {{if .IndexerLibrary}}
      <div id="idx-library" style="display:flex;flex-wrap:wrap;gap:6px;margin-top:8px;">
        {{range .IndexerLibrary}}
          <label id="lib-{{.ID}}" style="padding:4px 10px;font-size:12px;background:var(--bg);color:var(--text);border:1px solid var(--line);border-radius:8px;cursor:pointer;display:flex;align-items:center;gap:4px;">
            <input type="checkbox" name="ids" value="{{.ID}}" style="width:auto;margin:0;">
            <span style="display:inline-block;width:8px;height:8px;border-radius:50%;background:{{typeColor .Type}};flex-shrink:0;" title="{{.Type}}"></span>
            {{.Name}} <span style="color:var(--muted);font-size:10px;">{{.Language}}</span>
          </label>
        {{end}}
      </div>
      {{else}}
      <div class="hint">{{index .T "idx_lib_empty"}}</div>
      {{end}}
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
        await fetch('/indexers',{method:'POST',body:form,headers:{'X-Requested-With':'XMLHttpRequest'}});
      }

      async function testIdx(id,name){
        let dot=document.querySelector('#row-'+id+' .health-dot');
        let timeEl=document.querySelector('#row-'+id+' .test-time');
        let errEl=document.querySelector('#row-'+id+' .err-msg');
        dot.textContent='…';dot.style.color='var(--warn)';
        try{
          let r=await fetch('/indexers/test?id='+encodeURIComponent(id));
          let j=await r.json();
          if(j.ok){dot.textContent='●';dot.style.color='var(--accent-2)';errEl.textContent='';}
          else{dot.textContent='●';dot.style.color='var(--danger)';errEl.textContent=j.msg;}
          timeEl.textContent=new Date().toLocaleString('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit'});
        }catch(e){dot.textContent='●';dot.style.color='var(--danger)';errEl.textContent=e.message;}
      }

      async function testAll(){
        let r=await fetch('/indexers/testall',{method:'POST',headers:{'X-Requested-With':'XMLHttpRequest'}});
        let j=await r.json();
        for(let id in j){
          let dot=document.querySelector('#row-'+id+' .health-dot');
          let errEl=document.querySelector('#row-'+id+' .err-msg');
          if(dot){
            if(j[id]==='ok'){dot.style.color='var(--accent-2)';if(errEl)errEl.textContent='';}
            else{dot.style.color='var(--danger)';if(errEl)errEl.textContent=j[id];}
          }
        }
      }

      async function deactivateIdx(id){
        await apiPost('deactivate',{id});
        location.reload();
      }

      async function showLogin(id,name){
        var body='<div><label>用户名</label><input id="login-user" style="width:100%;"></div>';
        body+='<div style="margin-top:8px;"><label>密码</label><input id="login-pass" type="password" style="width:100%;"></div>';
        showModal('登录 - '+name, body, [
          {text:'取消',cls:'var(--danger)',cb:function(){closeModal()}},
          {text:'登录',cs:'var(--accent-2)',cb:async function(){
            var u=document.getElementById('login-user').value;
            var p=document.getElementById('login-pass').value;
            if(!u||!p){showModal('错误','<p>请输入用户名和密码</p>');return;}
            closeModal();
            try{
              let r=await fetch('/indexers/login',{
                method:'POST',
                body:new URLSearchParams({action:'login',id,username:u,password:p}),
                headers:{'X-Requested-With':'XMLHttpRequest'}
              });
              let j=await r.json();
              if(j.ok){showModal('成功','<p>登录成功</p>');testIdx(id,name);}
              else{showModal('失败','<p>'+j.msg+'</p>');}
            }catch(e){showModal('错误','<p>'+e.message+'</p>');}
          }}
        ]);
      }

      async function activateSelected(){
        let checks=document.querySelectorAll('#idx-library input[type="checkbox"]:checked');
        if(checks.length===0) return;
        let ids=[];
        checks.forEach(c=>ids.push(c.value));
        await apiPost('activate_batch',{ids:ids});
        location.reload();
      }
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
              btn.disabled=true;btn.textContent='等待中...';
              try {
                let r=await fetch('/login/qrcode',{method:'POST'});
                let d=await r.json();
                if(d.qrcode){
                  document.getElementById('qr-img').src=d.qrcode;
                  document.getElementById('qr-img').style.display='inline';
                  let poll=setInterval(async()=>{
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
      </script>
      <!-- settings form -->
      <form action="/settings" method="post">
        <div style="display:flex;gap:10px;flex-wrap:wrap;align-items:flex-end;">
          <div style="flex:1;min-width:80px;">
            <label>{{index .T "chunk_size"}}</label>
            <input name="chunk_size" type="number" value="{{.Settings.ChunkSize}}">
          </div>
          <div style="flex:1;min-width:80px;">
            <label>{{index .T "chunk_delay"}}</label>
            <input name="chunk_delay" type="number" value="{{.Settings.ChunkDelay}}">
          </div>
          <div style="flex:1;min-width:90px;">
            <label>{{index .T "cooldown_min"}}</label>
            <input name="cooldown_min" type="number" value="{{.Settings.CooldownMinMs}}">
          </div>
          <div style="flex:1;min-width:90px;">
            <label>{{index .T "cooldown_max"}}</label>
            <input name="cooldown_max" type="number" value="{{.Settings.CooldownMaxMs}}">
          </div>
          <div style="flex:0.5;min-width:80px;">
            <label style="display:flex;align-items:center;gap:4px;">
              <input type="checkbox" name="disable_cache" {{if .Settings.DisableCache}}checked{{end}}> {{index .T "disable_cache"}}
            </label>
          </div>
          <div style="flex:1;min-width:120px;">
            <label>{{index .T "web_pw"}}</label>
            <input name="web_password" type="password" placeholder="{{index .T "web_pw_ph"}}" value="{{.Settings.WebPassword}}" maxlength="128">
          </div>
          <div style="flex:0.5;min-width:80px;">
            <label>{{index .T "lang_label"}}</label>
            <select name="lang">
              <option value="zh"{{if eq .Lang "zh"}} selected{{end}}>中文</option>
              <option value="en"{{if eq .Lang "en"}} selected{{end}}>English</option>
            </select>
          </div>
          <div style="flex:2;min-width:200px;">
            <label>HTTP 代理</label>
            <input name="proxy_http" placeholder="http://127.0.0.1:7890" value="{{.ProxyHTTP}}">
          </div>
          <div style="flex:0.8;min-width:100px;">
            <label>订阅间隔(分)</label>
            <input name="subs_interval" type="number" placeholder="0=不自动" value="{{.Settings.SubsInterval}}" min="0" style="font-size:13px;">
          </div>
          <button type="submit" style="margin-top:0;">{{index .T "save"}}</button>
          <button type="button" onclick="restartServer()" style="margin-top:0;background:var(--danger);">🔄 重启服务</button>
        </div>
        <div class="hint">{{index .T "db_path"}}: {{.Settings.DatabasePath}}</div>
      </form>
    </div>
    {{end}}

  </div><!-- /.main -->

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
      (buttons||[{text:'确定',cls:'',cb:function(){closeModal()}}]).forEach(function(b){
        var btn=document.createElement('button');
        btn.textContent=b.text;btn.style.margin='0';btn.style.padding='6px 16px';
        if(b.cls)btn.style.background=b.cls;
        btn.onclick=function(){if(b.cb)b.cb();};
        btns.appendChild(btn);
      });
      document.getElementById('g-modal').style.display='flex';
    }
    function closeModal(){document.getElementById('g-modal').style.display='none';modalCb=null;}
  </script>
</body>
</html>`))

// ---------- server lifecycle ----------

func New(agent *p115pkg.Agent, port int) *Server {
	s := &Server{Port: port, fsCache: make(map[string]fsCacheEntry)}
	// Only assign if non-nil to avoid the Go nil-interface trap:
	// a nil *p115pkg.Agent wrapped in the Agent interface is NOT == nil.
	if agent != nil {
		agent.Dedup = globalDedup
		s.Agent = agent
	}
	return s
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
	}
}

// saveProxyConfig persists the current proxy setting to config.toml.
func (s *Server) saveProxyConfig() {
	_ = config.SaveProxy(s.ProxyHTTP)
}

func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	srv = &http.Server{Addr: fmt.Sprintf(":%d", s.Port), Handler: mux}

	// Load caches
	globalDedup.Load()

	// Start subscription auto-runner
	go s.autoRunSubscriptions()

	log.Printf("server started on port %d\n", s.Port)
	return srv.ListenAndServe()
}

// autoRunSubscriptions loops through enabled RSS subscriptions.
// Between feeds: small cooldown (30s). Between full cycles: SubsInterval.
func (s *Server) autoRunSubscriptions() {
	// Wait a short grace period for server to fully stabilise
	time.Sleep(2 * time.Minute)

	for {
		ws := s.loadWebSettings()
		cycleInterval := ws.SubsInterval
		if cycleInterval <= 0 {
			cycleInterval = 60 // default 60 minutes between full cycles
		}

		feeds := s.readRssFeeds()
		ran := 0
		for _, entries := range feeds {
			for _, e := range entries {
				if !e.Enabled {
					continue
				}
				url := e.URL
				if strings.HasPrefix(url, "/") {
					url = fmt.Sprintf("http://127.0.0.1:%d%s", s.Port, url)
				}
				log.Printf("[auto-sub] running: %s (%s)", e.Name, url)
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("[auto-sub] panic in %s: %v", e.Name, r)
						}
					}()
					if s.Agent != nil && e.Cid != "" {
						s.Agent.QuickGrabRSS(url, e.Cid, e.SavePath, e.Filter, e.Name)
					}
				}()
				ran++
				// Small cooldown between individual feeds (avoid hammering)
				time.Sleep(30 * time.Second)
			}
		}

		if ran == 0 {
			log.Printf("[auto-sub] no enabled subscriptions, sleeping %d min", cycleInterval)
		} else {
			log.Printf("[auto-sub] cycle complete: %d feeds ran, next cycle in %d min", ran, cycleInterval)
		}
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
	mux.HandleFunc("/add", s.authCheck(s.handleAddTask))
	mux.HandleFunc("/rss/quick", s.authCheck(s.handleQuickGrab))
	mux.HandleFunc("/clear", s.authCheck(s.handleClearTask))
	mux.HandleFunc("/fs", s.authCheck(s.handleFileSystem))
	mux.HandleFunc("/subs", s.authCheck(s.handleSubscriptions))
	mux.HandleFunc("/subs/run", s.authCheck(s.handleSubsRun))
	mux.HandleFunc("/settings", s.authCheck(s.handleSettings))
	mux.HandleFunc("/login/qrcode", s.authCheck(s.handleQRLogin))
	mux.HandleFunc("/login/cookies", s.authCheck(s.handleCookiesLogin))
	mux.HandleFunc("/logout", s.handleLogout)
	mux.HandleFunc("/settings/test115", s.authCheck(s.handleTest115))
	mux.HandleFunc("/settings/restart", s.authCheck(s.handleRestart))
	mux.HandleFunc("/search", s.authCheck(s.handleSearch))
	mux.HandleFunc("/indexers", s.authCheck(s.handleIndexers))
	mux.HandleFunc("/indexers/test", s.authCheck(s.handleIndexerTest))
	mux.HandleFunc("/indexers/testall", s.authCheck(s.handleIndexerTestAll))
	mux.HandleFunc("/indexers/login", s.authCheck(s.handleIndexerLogin))
	mux.HandleFunc("/search/subscribe", s.authCheck(s.handleSearchSubscribe))
	mux.HandleFunc("/rss/search", s.handleRssSearch)
	mux.HandleFunc("/subs/dirs", s.authCheck(s.handleSubsDirs))
	mux.HandleFunc("/dedup", s.authCheck(s.handleDedup))
	mux.HandleFunc("/dedup/clear", s.authCheck(s.handleDedupClear))
	mux.HandleFunc("/api/dedup/hashes", s.authCheck(s.handleDedupHashes))
	mux.HandleFunc("/torrent", s.authCheck(s.handleTorrent))
	mux.HandleFunc("/torrent/clear", s.authCheck(s.handleTorrentClear))
	mux.HandleFunc("/tasks", s.authCheck(s.handleTasks))
	mux.HandleFunc("/log", s.authCheck(s.handleLogPage))
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

	se := s.IdxMgr.SearchAllWithErrors(indexer.SearchRequest{
		Query:    q,
		Category: category,
		Sort:     sortBy,
		Indexers: indexers,
		Limit:    100,
	})

	// Build RSS XML
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?>` + "\n"))
	w.Write([]byte(`<rss version="2.0" xmlns:torznab="http://torznab.com/schemas/2015/feed">` + "\n"))
	w.Write([]byte(`<channel>` + "\n"))
	fmt.Fprintf(w, "<title>%s - pan-fetcher 聚合搜索</title>\n", xmlEscape(q))
	fmt.Fprintf(w, "<description>聚合搜索: %s</description>\n", xmlEscape(q))
	fmt.Fprintf(w, "<link>http://localhost:%d/search</link>\n", s.Port)
	fmt.Fprintf(w, "<language>zh-cn</language>\n")

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
				guid = "magnet:?xt=urn:btih:" + hash
			} else {
				m := rsssite.NormalizeTaskURL(guid, result.Title)
				if strings.HasPrefix(m, "magnet:?") {
					if h := extractInfoHashFromMagnet(m); h != "" {
						globalDedup.SetTorrentHash(guid, h)
						guid = "magnet:?xt=urn:btih:" + h
					} else {
						guid = m
					}
				}
			}
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
		s.renderResult(w, "", "115 未登录，请先在设置中配置 Cookies")
		return
	}
	task, err := decodeOfflineTask(r)
	if err != nil {
		s.renderResult(w, "", err.Error())
		return
	}
	s.Agent.AddMagnetTask(task.Tasks, task.Cid, task.SavePath)
	log.Printf("[task] web submitted %d tasks, cid=%s", len(task.Tasks), task.Cid)
	s.renderResult(w, fmt.Sprintf("已提交 %d 个磁力任务", len(task.Tasks)), "")
}

func (s *Server) handleQuickGrab(w http.ResponseWriter, r *http.Request) {
	if s.Agent == nil {
		s.renderResult(w, "", "115 未登录")
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
	if rssURL == "" {
		s.renderResult(w, "", "RSS 地址不能为空")
		return
	}
	go s.Agent.QuickGrabRSS(rssURL, cid, "", keyword, "manual")
	log.Printf("[quick] web triggered: %s keyword=%q cid=%s", rssURL, keyword, cid)
	s.renderResult(w, fmt.Sprintf("快速抓取已启动: %s", rssURL), "")
}

func (s *Server) handleClearTask(w http.ResponseWriter, r *http.Request) {
	if s.Agent == nil {
		s.renderResult(w, "", "115 未登录")
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
		s.renderResult(w, "", "清理类型必须是 1-6")
		return
	}
	if err := s.Agent.OfflineClear(typeNum - 1); err != nil {
		log.Printf("[task] clear type=%d failed: %v", typeNum, err)
		s.renderResult(w, "", err.Error())
		return
	}
	log.Printf("[task] clear type=%d executed", typeNum)
	s.renderResult(w, fmt.Sprintf("已执行清理类型 %d", typeNum), "")
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
		data := s.pageData("", "115 未登录，请先在设置中配置 Cookies")
		dashboardTemplate.Execute(w, data)
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
			data := s.pageData("", "读取目录失败: "+err.Error())
			dashboardTemplate.Execute(w, data)
			return
		}
	}

	// Build breadcrumb
	var crumbs []fsBreadcrumb
	currentID := dirID
	for i := 0; i < 20; i++ {
		if currentID == "0" || currentID == "" {
			crumbs = append([]fsBreadcrumb{{ID: "0", Name: "根目录"}}, crumbs...)
			break
		}
		e, err := s.Agent.GetEntry(currentID)
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

	// Build parent dir ID for "up" link
	parentID := "0"
	if len(crumbs) >= 2 {
		parentID = crumbs[len(crumbs)-2].ID
	}

	data := s.pageData("", "")
	data.Page = "fs"
	data.FSEntries = fsEntries
	data.FSCrumbs = crumbs
	data.FSCurrentID = dirID
	data.FSParentID = parentID

	dashboardTemplate.Execute(w, data)
}

// ---------- settings ----------

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	ws := s.loadWebSettings()

	if r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		// Collect all settings from form
		if v := strings.TrimSpace(r.FormValue("proxy_http")); v != "" {
			s.ProxyHTTP = v
			log.Printf("[settings] proxy set to %s", v)
		}
		if v := strings.TrimSpace(r.FormValue("lang")); v != "" {
			ws.Lang = v
		}
		if v, err := strconv.Atoi(r.FormValue("chunk_delay")); err == nil && v > 0 {
			ws.ChunkDelay = v
		}
		if v, err := strconv.Atoi(r.FormValue("chunk_size")); err == nil && v > 0 {
			ws.ChunkSize = v
		}
		if v, err := strconv.Atoi(r.FormValue("cooldown_min")); err == nil && v > 0 {
			ws.CooldownMin = v
		}
		if v, err := strconv.Atoi(r.FormValue("cooldown_max")); err == nil && v > 0 {
			ws.CooldownMax = v
		}
		if v, err := strconv.Atoi(r.FormValue("subs_interval")); err == nil && v >= 0 {
			ws.SubsInterval = v
		}
		ws.DisableCache = r.FormValue("disable_cache") == "on"
		if pw := sanitizePassword(r.FormValue("web_password")); pw != "" {
			webPassword = pw
			ws.WebPassword = pw
		}
		s.saveWebSettings(ws)
		s.saveProxyConfig()

		// Also save to agent if logged in
		if s.Agent != nil {
			st := p115pkg.AppSettings{
				DisableCache: ws.DisableCache,
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
		if s.Agent != nil {
			data.Settings = s.Agent.GetSettings()
		}
		data.Settings.Lang = ws.Lang
		if ws.WebPassword != "" {
			data.Settings.WebPassword = ws.WebPassword
		}
		http.SetCookie(w, &http.Cookie{Name: "r2c_lang", Value: lang, Path: "/", MaxAge: 86400 * 365})
		dashboardTemplate.Execute(w, data)
		return
	}

	// GET: load saved settings
	data := s.pageData("", "")
	data.Page = "settings"
	data.ProxyHTTP = s.ProxyHTTP
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
	if ws.DisableCache {
		data.Settings.DisableCache = ws.DisableCache
	}
	if ws.SubsInterval > 0 {
		data.Settings.SubsInterval = ws.SubsInterval
	}
	if ws.WebPassword != "" {
		data.Settings.WebPassword = ws.WebPassword
	}

	lang := ws.Lang
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
	feeds := s.readRssFeeds()
	for _, feedEntries := range feeds {
		for _, e := range feedEntries {
			cnt := globalDedup.SubCount(e.Name)
			entries = append(entries, dedupEntry{SubKey: e.Name, Count: cnt})
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
		s.renderResult(w, "", "缺少订阅名")
		return
	}
	globalDedup.RemoveSub(subKey)
	globalDedup.Save()
	log.Printf("[dedup] cleared dedup for %s", subKey)
	http.Redirect(w, r, "/dedup", http.StatusSeeOther)
}

func (s *Server) handleDedupHashes(w http.ResponseWriter, r *http.Request) {
	subKey := r.URL.Query().Get("sub")
	hashes := globalDedup.Hashes(subKey)
	// Build response with hash + torrent URL
	type hashEntry struct {
		Hash string `json:"hash"`
		URL  string `json:"url,omitempty"`
	}
	out := make([]hashEntry, 0, len(hashes))
	for _, h := range hashes {
		out = append(out, hashEntry{Hash: h, URL: globalDedup.TorrentURLByHash(h)})
	}
	w.Header().Set("Content-Type", "application/json")
	data, _ := json.Marshal(out)
	w.Write(data)
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

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	data := s.pageData("", "")
	data.Page = "tasks"
	dashboardTemplate.Execute(w, data)
}

func (s *Server) handleLogPage(w http.ResponseWriter, r *http.Request) {
	data := s.pageData("", "")
	data.Page = "log"
	dashboardTemplate.Execute(w, data)
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
		s.renderResult(w, "", "Cookies 不能为空")
		return
	}
	newAgent, err := p115pkg.ReloginWithCookies(cookies)
	if err != nil {
		log.Printf("[auth] cookies login failed: %v", err)
		s.renderResult(w, "", "登录失败: "+err.Error())
		return
	}
	s.SetAgent(newAgent)
	log.Printf("[auth] cookies updated successfully")
	s.renderResult(w, "Cookies 已更新并验证成功", "")
}

func (s *Server) handleTest115(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.Agent == nil {
		fmt.Fprint(w, `{"ok":false,"msg":"115 未登录"}`)
		return
	}
	if err := s.Agent.TestConnection(); err != nil {
		fmt.Fprintf(w, `{"ok":false,"msg":"%s"}`, err.Error())
		return
	}
	fmt.Fprint(w, `{"ok":true,"msg":"115 连接正常"}`)
}

func (s *Server) handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"ok":true,"msg":"正在重启…"}`)

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
		cmd.SysProcAttr = &syscall.SysProcAttr{
			CreationFlags: 0x00000008, // DETACHED_PROCESS
		}
		if err := cmd.Start(); err != nil {
			log.Printf("[restart] failed to spawn: %v", err)
			os.Exit(1)
		}
		// Release the child so it is not tied to this process
		cmd.Process.Release()
		log.Printf("[restart] new process started (pid %d), exiting", cmd.Process.Pid)
		os.Exit(0)
	}()
}

// ---------- search ----------

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	data := s.pageData("", "")

	// Populate indexer list for filter checkboxes
	if s.IdxMgr != nil {
		data.IndexerList = s.IdxMgr.List()
	}
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
			data.Error = "请输入搜索关键词"
			dashboardTemplate.Execute(w, data)
			return
		}
		if s.IdxMgr == nil {
			data.Error = "索引器未初始化"
			dashboardTemplate.Execute(w, data)
			return
		}

		// Parse filters
		category := strings.TrimSpace(r.FormValue("category"))
		sortBy := strings.TrimSpace(r.FormValue("sort"))
		if sortBy == "" {
			sortBy = "seeds"
		}
		indexers := r.Form["indexer"]

		se := s.IdxMgr.SearchAllWithErrors(indexer.SearchRequest{
			Query:    q,
			Category: category,
			Sort:     sortBy,
			Indexers: indexers,
			Limit:    50,
		})
		for i := range se.Results {
			se.Results[i].SizeFmt = formatSize(se.Results[i].Size)
		}
		data.SearchResults = se.Results
		data.SearchErrors = se.Errors
		data.SearchQuery = q
		data.SearchCategory = category
		data.SearchSort = sortBy
		data.SearchIndexers = indexers
		// Auto-build RSS URL for subscription (local aggregated feed)
		if q != "" {
			data.RssURL = buildRssURL(s.Port, q, indexers, category)
		}
	}
	dashboardTemplate.Execute(w, data)
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
		data := s.pageData("", "请填写 RSS 地址")
		data.SearchQuery = q
		dashboardTemplate.Execute(w, data)
		return
	}

	// Save to rss.json
	if err := addRSSFeed(rssURL, name, filter, cid, savepath); err != nil {
		data := s.pageData("", "保存失败: "+err.Error())
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
					fmt.Fprint(w, `{"ok":true,"msg":"登录成功"}`)
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
		data.IndexerLibrary = s.IdxMgr.Library()
	}
	dashboardTemplate.Execute(w, data)
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
	fmt.Fprint(w, `{"ok":true,"msg":"ok"}`)
}

func (s *Server) handleIndexerTestAll(w http.ResponseWriter, r *http.Request) {
	if s.IdxMgr == nil {
		http.Redirect(w, r, "/indexers", http.StatusSeeOther)
		return
	}
	results := s.IdxMgr.TestAll()
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
		fmt.Fprint(w, `{"ok":false,"msg":"需要 id, username, password"}`)
		return
	}
	if err := s.IdxMgr.Login(id, username, password); err != nil {
		fmt.Fprintf(w, `{"ok":false,"msg":"%s"}`, err.Error())
		return
	}
	fmt.Fprint(w, `{"ok":true,"msg":"登录成功"}`)
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
		errMsg := `<div class="err">密码错误</div>`
		if lang == "en" {
			errMsg = `<div class="err">Wrong password</div>`
		}
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
	pageTitle := "pan-fetcher 登录"
	ph := "输入管理密码"
	btn := "登录"
	htmlLang := "zh-CN"
	if lang == "en" {
		pageTitle = "pan-fetcher Login"
		ph = "Enter password"
		btn = "Login"
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
	Site     string
	Name     string
	URL      string
	Filter   string
	Cid      string
	SavePath string
	Enabled  bool
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
	dashboardTemplate.Execute(w, data)
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
		http.Redirect(w, r, "/subs", http.StatusSeeOther)
		return
	}
	if action == "delete" {
		s.deleteRssSub(site, name)
		http.Redirect(w, r, "/subs", http.StatusSeeOther)
		return
	}
	if action == "edit" {
		cid := strings.TrimSpace(r.FormValue("cid"))
		savepath := strings.TrimSpace(r.FormValue("savepath"))
		filter := strings.TrimSpace(r.FormValue("filter"))
		s.updateRssSub(site, name, cid, savepath, filter)
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
			})
		}
	}
	return rows
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
	globalDedup.RemoveSub(name)
	globalDedup.Save()
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
		s.renderResult(w, "", "115 未登录，无法运行订阅")
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	rssURL := strings.TrimSpace(r.FormValue("rss_url"))
	if rssURL == "" {
		s.renderResult(w, "", "RSS地址不能为空")
		return
	}
	cid := strings.TrimSpace(r.FormValue("cid"))
	savepath := strings.TrimSpace(r.FormValue("savepath"))
	filter := strings.TrimSpace(r.FormValue("filter"))
	// Resolve relative URLs to absolute for local RSS endpoints
	if strings.HasPrefix(rssURL, "/") {
		rssURL = fmt.Sprintf("http://127.0.0.1:%d%s", s.Port, rssURL)
	}
	go s.Agent.QuickGrabRSS(rssURL, cid, savepath, filter, "manual")
	s.renderResult(w, fmt.Sprintf("已开始处理 %s", rssURL), "")
}

func (s *Server) handleSubsDirs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.Agent == nil {
		fmt.Fprint(w, `{"ok":false,"msg":"115 未登录"}`)
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
		if e, err := s.Agent.GetEntry(parentID); err == nil {
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

func (s *Server) pageData(message, errMsg string) dashboardData {
	// Determine language
	lang := "zh"
	if s.Agent != nil {
		st := s.Agent.GetSettings()
		if st.Lang != "" {
			lang = st.Lang
		}
	}
	data := dashboardData{
		Message: message,
		Error:   errMsg,
		Lang:    lang,
		T:       langMap(lang),
	}
	if s.Agent != nil {
		// Quick 115 connectivity check
		if err := s.Agent.TestConnection(); err == nil {
			data.LoggedIn = true
		}
		data.Cookies = s.Agent.LoadCookiesStr()
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
	data.Logs = logBuf.Lines()
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
