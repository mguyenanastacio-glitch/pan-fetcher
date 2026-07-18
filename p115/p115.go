package p115

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/deadblue/elevengo"
	"github.com/deadblue/elevengo/option"
	"github.com/mguyenanastacio-glitch/pan-fetcher/config"
	"github.com/mguyenanastacio-glitch/pan-fetcher/media"
	"github.com/mguyenanastacio-glitch/pan-fetcher/request"
	"github.com/mguyenanastacio-glitch/pan-fetcher/rsssite"
	"github.com/mguyenanastacio-glitch/pan-fetcher/store"
	"github.com/mguyenanastacio-glitch/pan-fetcher/subscribe"
)

var defaultChunkSize = 200
var chunkDelay = 2
var cooldownMinMs uint
var cooldownMaxMs uint
var databasePath = "db.sqlite"
var webLang = "zh"
var currentCookies string
var jackettURL string
var jackettAPIKey string

type Option struct {
	ChunkDelay    int
	ChunkSize     int
	CooldownMinMs int
	CooldownMaxMs int
	DatabasePath  string
}

func SetOption(opt Option) {
	chunkDelay = 2
	if opt.ChunkDelay > 0 {
		chunkDelay = opt.ChunkDelay
	}
	defaultChunkSize = 200
	if opt.ChunkSize > 0 {
		defaultChunkSize = opt.ChunkSize
	}
	cooldownMinMs = 0
	cooldownMaxMs = 0
	if opt.CooldownMinMs > 0 {
		cooldownMinMs = uint(opt.CooldownMinMs)
	}
	if opt.CooldownMaxMs > 0 {
		cooldownMaxMs = uint(opt.CooldownMaxMs)
	}
	if cooldownMinMs > 0 && cooldownMaxMs == 0 {
		cooldownMaxMs = cooldownMinMs
	}
	if cooldownMaxMs > 0 && cooldownMaxMs < cooldownMinMs {
		cooldownMaxMs = cooldownMinMs
	}
	if opt.DatabasePath != "" {
		databasePath = opt.DatabasePath
	}
}

// DedupChecker is used to avoid re-submitting already-downloaded magnets.
type DedupChecker interface {
	Has(subKey, magnet string) bool
	Add(subKey, magnet string)
	Save()
}

type Agent struct {
	Agent         *elevengo.Agent
	StoreInstance *store.Store
	Dedup         DedupChecker
}

func parseCookies(cookiesString string) map[string]string {
	cookies := make(map[string]string)

	// Split the cookies string into individual cookies
	cookiePairs := strings.Split(cookiesString, ";")

	// Parse each cookie into key-value pair
	for _, cookiePair := range cookiePairs {
		cookie := strings.TrimSpace(cookiePair)
		cookieParts := strings.SplitN(cookie, "=", 2)
		if len(cookieParts) == 2 {
			key := cookieParts[0]
			value := cookieParts[1]
			cookies[key] = value
		}
	}

	return cookies
}

func New() (*Agent, error) {
	cookies, err := LoadCookiesWithError()
	if err != nil {
		return nil, err
	}
	if cookies != "" {
		agent, err := NewAgent(cookies)
		// cookies is invalid
		if err != nil {
			return nil, err
		}
		return agent, nil
	}
	return nil, errors.New(".cookies is empty or not exist")
}

func NewAgentByQrcode() (*Agent, error) {
	cookies, err := LoadCookiesWithError()
	if err != nil {
		return nil, err
	}
	if cookies != "" {
		agent, err := NewAgent(cookies)
		// cookies is invalid
		if err != nil {
			return QrcodeLogin()
		}
		return agent, nil
	}
	return QrcodeLogin()
}
func NewAgentByConfig() (*Agent, error) {
	config := request.ReadNodeSiteConfig()
	if p115Config, ok := config["115.com"]; ok {
		cookies, ok := p115Config.Headers["cookie"]
		if !ok {
			cookies = p115Config.Headers["Cookie"]
		}
		if cookies == "" {
			return nil, errors.New("115 cookie is empty")
		}
		return NewAgent(cookies)
	}
	return nil, errors.New("no 115.com config in node-site-config.json")
}

func NewAgent(cookies string) (*Agent, error) {
	currentCookies = cookies
	agent := newElevengoAgent()
	cookiesMap := parseCookies(cookies)
	err := agent.CredentialImport(&elevengo.Credential{
		UID: cookiesMap["UID"], CID: cookiesMap["CID"], SEID: cookiesMap["SEID"],
		KID: cookiesMap["KID"],
	})
	if err != nil {
		return nil, err
	}
	storeInstance, err := store.NewWithPath(databasePath)
	if err != nil {
		return nil, err
	}
	return &Agent{
		Agent:         agent,
		StoreInstance: storeInstance,
	}, nil
}

func chunkBy[T any](items []T, chunkSize int) (chunks [][]T) {
	for chunkSize < len(items) {
		items, chunks = items[chunkSize:], append(chunks, items[0:chunkSize:chunkSize])
	}
	return append(chunks, items)
}

func (ag *Agent) addCloudTasks(magnetItems []rsssite.MagnetItem, config *rsssite.RssConfig) {
	emptyNum := 0
	filterdItems := make([]rsssite.MagnetItem, 0)
	for _, item := range magnetItems {
		if item.Magnet == "" {
			emptyNum += 1
			continue
		}
		if !ag.StoreInstance.HasItem(item.Magnet) {
			filterdItems = append(filterdItems, item)
		}
	}
	if emptyNum != 0 {
		log.Printf("[warning] [%s] has %d empty task\n", config.Name, emptyNum)
	}
	if len(filterdItems) == 0 {
		log.Printf("[%s] has 0 task\n", config.Name)
		return
	}
	for i, items := range chunkBy(filterdItems, defaultChunkSize) {
		urls := make([]string, 0)
		for _, item := range items {
			urls = append(urls, item.Magnet)
		}
		_, err := ag.Agent.OfflineAddUrl(urls, &option.OfflineAddOptions{SaveDirId: config.Cid, SavePath: config.SavePath})
		if err != nil {
			log.Printf("Add offline error: %s\n", err)
			return
		}
		log.Printf("[%s] [%s] add %d tasks\n", config.Name, config.Url, len(urls))
		ag.StoreInstance.SaveMagnetItems(filterdItems)
		if i != len(filterdItems)/defaultChunkSize {
			time.Sleep(time.Second * time.Duration(chunkDelay))
		}
	}
}

func (ag *Agent) AddRssUrlTask(url string) {
	config := rsssite.GetRssConfigByURL(url)
	if config == nil {
		pwd := func() string { if d, err := os.Getwd(); err == nil { return d }; return "" }()
		log.Printf("config not found: %s for url: %s\n", pwd, url)
		return
	}
	magnetItems := rsssite.GetMagnetItemList(config)
	ag.addCloudTasks(magnetItems, config)
}

// ProcessRssWithSubscriptions fetches an RSS feed and submits matching items
// based on active subscriptions. Each subscription defines its own target CID.
func (ag *Agent) ProcessRssWithSubscriptions(rssURL string) {
	if ag.StoreInstance == nil {
		log.Printf("[subscribe] store not initialized")
		return
	}

	feed := rsssite.GetFeed(rssURL)
	if feed == nil {
		return
	}

	engine := subscribe.New(ag.StoreInstance)
	subs, err := ag.StoreInstance.ListSubscriptions()
	if err != nil {
		log.Printf("[subscribe] failed to list subscriptions: %v", err)
		return
	}

	enabledCount := 0
	for _, s := range subs {
		if s.Enabled {
			enabledCount++
		}
	}
	if enabledCount == 0 {
		log.Printf("[subscribe] no enabled subscriptions")
		return
	}

	log.Printf("[subscribe] processing %s (%d items, %d subscriptions)", rssURL, len(feed.Items), enabledCount)

	for _, item := range feed.Items {
		title := item.Title
		if title == "" {
			continue
		}

		// Parse the torrent title into structured metadata
		info := media.ParseTitle(title)

		// Match against subscriptions
		result := engine.Match(info)
		if result == nil {
			continue
		}

		sub := result.Subscription

		// Check for duplicate
		if !engine.IsNewEpisode(sub.ID, info.Season, info.Episode) {
			log.Printf("[subscribe] skip dup: %s (S%02dE%02d)", info.Title, info.Season, info.Episode)
			continue
		}

		// Extract magnet from enclosure, content, or link
		magnet := rsssite.GetMagnetByEnclosure(item)
		if magnet == "" {
			magnet = rsssite.ExtractMagnetFromText(item.Content)
		}
		if magnet == "" {
			magnet = rsssite.ExtractMagnetFromText(item.Description)
		}
		if magnet == "" {
			log.Printf("[subscribe] no magnet for: %s", title)
			continue
		}

		// Submit to 115 with subscription's target CID
		cid := sub.Cid
		savepath := sub.Savepath
		_, err := ag.Agent.OfflineAddUrl([]string{magnet}, &option.OfflineAddOptions{
			SaveDirId: cid,
			SavePath:  savepath,
		})
		if err != nil {
			log.Printf("[subscribe] submit error for %s: %v", title, err)
			continue
		}

		// Record in history
		if err := ag.StoreInstance.RecordSubmission(
			sub.ID, title, magnet, "",
			info.Episode, info.Season,
		); err != nil {
			log.Printf("[subscribe] failed to record history: %v", err)
		}

		log.Printf("[subscribe] ✓ %s → %s (cid=%s, S%02dE%02d, conf=%.0f%%)",
			title, sub.Name, cid, info.Season, info.Episode, result.Confidence*100)
	}
}

func (ag *Agent) ExecuteAllRssTask() {
	rssDict := rsssite.ReadRssConfigDict()
	if rssDict == nil {
		pwd := func() string { if d, err := os.Getwd(); err == nil { return d }; return "" }()
		log.Printf("rss config not found: %s\n", pwd)
		return
	}
	for _, configs := range *rssDict {
		for i, config := range configs {
			if !config.IsEnabled() {
				continue
			}
			magnetItems := rsssite.GetMagnetItemList(&config)
			ag.addCloudTasks(magnetItems, &config)
			if i != len(configs)-1 {
				time.Sleep(time.Second * time.Duration(chunkDelay))
			}
		}
	}
}

func (ag *Agent) AddMagnetTask(tasks []string, cid string, savepath string) error {
	type bucket struct {
		urls []string
		kind string
	}

	magnets := &bucket{kind: "magnet"}
	ed2ks := &bucket{kind: "ed2k"}
	httpURLs := &bucket{kind: "http"}
	skipped := 0

	for _, raw := range tasks {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		switch rsssite.ClassifyURL(raw) {
		case rsssite.TaskURLTorrent:
			if normalized := rsssite.NormalizeTaskURL(raw, ""); normalized != raw {
				magnets.urls = append(magnets.urls, normalized)
				log.Printf("[task] torrent converted: %s\n", raw)
			} else {
				httpURLs.urls = append(httpURLs.urls, raw)
				log.Printf("[task] torrent fallback to direct: %s\n", raw)
			}
		case rsssite.TaskURLMagnet:
			magnets.urls = append(magnets.urls, rsssite.NormalizeTaskURL(raw, ""))
		case rsssite.TaskURLEd2k:
			ed2ks.urls = append(ed2ks.urls, raw)
		case rsssite.TaskURLHttp:
			httpURLs.urls = append(httpURLs.urls, raw)
		default:
			log.Printf("[task] unknown type, skipped: %s\n", raw)
			skipped++
		}
	}

	// Submit each bucket
	submitted := 0
	for _, b := range []*bucket{magnets, ed2ks, httpURLs} {
		if len(b.urls) == 0 {
			continue
		}
		for i, urls := range chunkBy(b.urls, defaultChunkSize) {
			_, err := ag.Agent.OfflineAddUrl(urls, &option.OfflineAddOptions{SaveDirId: cid, SavePath: savepath})
			if err != nil {
				log.Printf("[%s] add offline error: %s\n", b.kind, err)
				return fmt.Errorf("add %s failed: %v", b.kind, err)
			}
			submitted += len(urls)
			log.Printf("[%s] add %d tasks\n", b.kind, len(urls))
			if i != len(b.urls)/defaultChunkSize {
				time.Sleep(time.Second * time.Duration(chunkDelay))
			}
		}
	}

	if submitted == 0 {
		return errors.New("no valid tasks to submit")
	}
	if skipped > 0 {
		log.Printf("[task] skipped %d unrecognized URLs\n", skipped)
	}
	return nil
}
func (ag *Agent) OfflineClear(num int) (err error) {
	flag := elevengo.OfflineClearFlag(num)
	return ag.Agent.OfflineClear(flag)
}

func (ag *Agent) UserGet(info *elevengo.UserInfo) error {
	if ag == nil || ag.Agent == nil {
		return errors.New("agent is nil")
	}
	return ag.Agent.UserGet(info)
}

func (ag *Agent) StoreClose() error {
	if ag == nil || ag.StoreInstance == nil {
		return nil
	}
	return ag.StoreInstance.Close()
}

// TestConnection quickly checks if the 115 agent is functional.
func (ag *Agent) TestConnection() error {
	if ag == nil || ag.Agent == nil {
		return errors.New("agent 未初始化")
	}
	var info elevengo.UserInfo
	return ag.Agent.UserGet(&info)
}

// TaskItem is a simplified offline task for display.
type TaskItem struct {
	InfoHash string
	Name     string
	Size     int64
	Status   int
	Percent  float64
	URL      string
}

func (ag *Agent) ListTasks() ([]TaskItem, error) {
	if ag == nil || ag.Agent == nil {
		return nil, errors.New("agent is nil")
	}
	it, err := ag.Agent.OfflineIterate()
	if err != nil {
		return nil, err
	}
	result := make([]TaskItem, 0, it.Count())
	for _, t := range it.Items() {
		result = append(result, TaskItem{
			InfoHash: t.InfoHash,
			Name:     t.Name,
			Size:     t.Size,
			Status:   t.Status,
			Percent:  t.Percent,
			URL:      t.Url,
		})
	}
	return result, nil
}

// DirEntry is a simplified directory/file entry for web display.
type DirEntry struct {
	ID       string
	Name     string
	IsDir    bool
	Size     int64
	ParentID string
}

// ListDir lists the contents of a 115 cloud directory.
func (ag *Agent) ListDir(dirID string) ([]DirEntry, error) {
	if ag == nil || ag.Agent == nil {
		return nil, errors.New("agent is nil")
	}
	if dirID == "" {
		dirID = "0"
	}
	it, err := ag.Agent.FileIterate(dirID)
	if err != nil {
		return nil, err
	}
	result := make([]DirEntry, 0, it.Count())
	for _, f := range it.Items() {
		result = append(result, DirEntry{
			ID:       f.FileId,
			Name:     f.Name,
			IsDir:    f.IsDirectory,
			Size:     f.Size,
			ParentID: f.ParentId,
		})
	}
	return result, nil
}

// GetEntry returns metadata for a single 115 cloud entry.
func (ag *Agent) GetEntry(entryID string) (DirEntry, error) {
	if ag == nil || ag.Agent == nil {
		return DirEntry{}, errors.New("agent is nil")
	}
	if entryID == "" || entryID == "0" {
		return DirEntry{ID: "0", Name: "根目录", IsDir: true}, nil
	}
	f := &elevengo.File{}
	if err := ag.Agent.FileGet(entryID, f); err != nil {
		return DirEntry{}, err
	}
	return DirEntry{
		ID:       f.FileId,
		Name:     f.Name,
		IsDir:    f.IsDirectory,
		Size:     f.Size,
		ParentID: f.ParentId,
	}, nil
}

// Mkdir creates a new directory and returns its entry.
func (ag *Agent) Mkdir(parentID, name string) (DirEntry, error) {
	if ag == nil || ag.Agent == nil {
		return DirEntry{}, errors.New("agent is nil")
	}
	dirID, err := ag.Agent.DirMake(parentID, name)
	if err != nil {
		return DirEntry{}, err
	}
	return ag.GetEntry(dirID)
}

// RenameEntry renames a file or directory.
func (ag *Agent) RenameEntry(entryID, newName string) error {
	if ag == nil || ag.Agent == nil {
		return errors.New("agent is nil")
	}
	return ag.Agent.FileRename(entryID, newName)
}

// DeleteEntry deletes a file or directory.
func (ag *Agent) DeleteEntry(entryID string) error {
	if ag == nil || ag.Agent == nil {
		return errors.New("agent is nil")
	}
	return ag.Agent.FileDelete([]string{entryID})
}

// MoveEntry moves a file or directory to a target directory.
func (ag *Agent) MoveEntry(targetDirID, entryID string) error {
	if ag == nil || ag.Agent == nil {
		return errors.New("agent is nil")
	}
	return ag.Agent.FileMove(targetDirID, []string{entryID})
}

// Copy copies an entry to a target directory.
func (ag *Agent) Copy(targetDirID, entryID string) error {
	if ag == nil || ag.Agent == nil {
		return errors.New("agent is nil")
	}
	return ag.Agent.FileCopy(targetDirID, []string{entryID})
}

// SubInfo is a subscription for web display.
type SubInfo struct {
	ID        int
	Name      string
	MediaType string
	Season    int
	Cid       string
	Savepath  string
	Enabled   bool
}

// ProcessRSSFeed fetches an RSS feed, filters by keyword, and submits all matches.
// Phase 1: lightweight info-hash extraction + dedup check (no torrent download).
// Phase 2: resolve full magnet only for items that pass dedup.
func (ag *Agent) ProcessRSSFeed(rssURL, cid, savepath, keyword, subKey string) []string {
	// Fallback: extract keyword from RSS URL if filter is empty
	if keyword == "" {
		if u, err := url.Parse(rssURL); err == nil {
			keyword = u.Query().Get("keyword")
		}
	}
	feed := rsssite.GetFeed(rssURL)
	if feed == nil {
		log.Printf("[feed] failed to fetch RSS: %s", rssURL)
		return nil
	}

	log.Printf("[feed] processing subKey=%q, items=%d", subKey, len(feed.Items))

	type candidate struct {
		magnet string
		title  string
	}
	var candidates []candidate
	skippedPhase1 := 0
	skippedPhase2 := 0

	for _, item := range feed.Items {
		if keyword != "" {
			titleLower := strings.ToLower(item.Title)
			match := false
			for _, line := range strings.Split(keyword, "\n") {
				k := strings.TrimSpace(line)
				if k == "" { continue }
				if strings.Contains(titleLower, strings.ToLower(k)) { match = true; break }
			}
			if !match { continue }
		}

		// Phase 1: try lightweight info hash from text/URL (no .torrent download)
		if ag.Dedup != nil {
			if ih := rsssite.TryExtractInfoHash(item); ih != "" {
				if ag.Dedup.Has(subKey, ih) {
					skippedPhase1++
					continue
				}
			}
		}

		// Phase 2: resolve full magnet (may download .torrent)
		magnet := rsssite.GetMagnetByEnclosure(item)
		if magnet == "" {
			magnet = rsssite.ExtractMagnetFromText(item.Content)
		}
		if magnet == "" {
			magnet = rsssite.ExtractMagnetFromText(item.Description)
		}
		if magnet == "" {
			continue
		}
		// Phase 2 dedup check (after torrent download + conversion if needed)
		if ag.Dedup != nil {
			if ag.Dedup.Has(subKey, magnet) {
				skippedPhase2++
				continue
			}
		}
		candidates = append(candidates, candidate{magnet: magnet, title: item.Title})
	}

	if skippedPhase1 > 0 {
		log.Printf("[feed] phase-1 dedup skipped %d (no download needed)", skippedPhase1)
	}
	if skippedPhase2 > 0 {
		log.Printf("[feed] phase-2 dedup skipped %d (torrent was downloaded but already cached)", skippedPhase2)
	}
	if len(candidates) == 0 {
		log.Printf("[feed] all items already cached for keyword=%q in %s", keyword, rssURL)
		return nil
	}
	log.Printf("[feed] found %d NEW items, submitting to cid=%s", len(candidates), cid)

	var magnets []string
	var names []string
	for _, c := range candidates {
		magnets = append(magnets, c.magnet)
		names = append(names, c.title)
	}
	for i, urls := range chunkBy(magnets, defaultChunkSize) {
		_, err := ag.Agent.OfflineAddUrl(urls, &option.OfflineAddOptions{SaveDirId: cid, SavePath: savepath})
		if err != nil {
			log.Printf("[feed] error: %v", err)
			return names
		}
		log.Printf("[feed] chunk %d: submitted %d tasks", i+1, len(urls))
		if ag.Dedup != nil {
			for _, u := range urls {
				ag.Dedup.Add(subKey, u)
			}
			ag.Dedup.Save()
		}
		if i != len(magnets)/defaultChunkSize {
			time.Sleep(time.Second * time.Duration(chunkDelay))
		}
	}
	return names
}

// AppSettings holds runtime-configurable application settings.
type AppSettings struct {
	ChunkDelay    int
	ChunkSize     int
	CooldownMinMs int
	CooldownMaxMs int
	DatabasePath  string
	WebPassword   string
	Lang          string
	SubsInterval  int
	JackettURL    string
	JackettAPIKey string
}

func (ag *Agent) GetSettings() AppSettings {
	return AppSettings{
		ChunkDelay:    chunkDelay,
		ChunkSize:     defaultChunkSize,
		CooldownMinMs: int(cooldownMinMs),
		CooldownMaxMs: int(cooldownMaxMs),
		DatabasePath:  databasePath,
		Lang:          webLang,
		JackettURL:    jackettURL,
		JackettAPIKey: jackettAPIKey,
	}
}

func (ag *Agent) UpdateSettings(s AppSettings) error {
	if s.ChunkDelay > 0 {
		chunkDelay = s.ChunkDelay
	}
	if s.ChunkSize > 0 {
		defaultChunkSize = s.ChunkSize
	}
	if s.CooldownMinMs > 0 {
		cooldownMinMs = uint(s.CooldownMinMs)
	}
	if s.CooldownMaxMs > 0 {
		cooldownMaxMs = uint(s.CooldownMaxMs)
	}
	if s.Lang != "" {
		webLang = s.Lang
	}
	if s.JackettURL != "" {
		jackettURL = s.JackettURL
	}
	if s.JackettAPIKey != "" {
		jackettAPIKey = s.JackettAPIKey
	}
	return nil
}

// LoadCookiesStr returns the current cookies string from memory.
func (ag *Agent) LoadCookiesStr() string {
	if currentCookies != "" {
		return currentCookies
	}
	return LoadCookies()
}

// QrcodeSession holds a live QR code login session for the web UI.
var webQrcodeSession *elevengo.QrcodeSession
var webQrcodeAgent *elevengo.Agent

// StartQrcodeLogin begins a QR code login and returns the QR image bytes.
func StartQrcodeLogin() ([]byte, error) {
	agent := newElevengoAgent()
	session := &elevengo.QrcodeSession{}
	if err := agent.QrcodeStart(session, option.Qrcode().LoginTv()); err != nil {
		return nil, err
	}
	webQrcodeAgent = agent
	webQrcodeSession = session
	return session.Image, nil
}

// PollQrcodeLogin checks if the QR code has been scanned and logged in.
// Returns (success, err).
func PollQrcodeLogin() (bool, error) {
	if webQrcodeSession == nil || webQrcodeAgent == nil {
		return false, errors.New("no active QR login session")
	}
	return webQrcodeAgent.QrcodePoll(webQrcodeSession)
}

// FinishQrcodeLogin saves cookies and creates a new Agent after successful QR login.
func FinishQrcodeLogin() (*Agent, error) {
	if webQrcodeAgent == nil {
		return nil, errors.New("no active QR login session")
	}
	cr := &elevengo.Credential{}
	webQrcodeAgent.CredentialExport(cr)
	SaveCookiesToFile(fmt.Sprintf("UID=%s; CID=%s; SEID=%s; KID=%s", cr.UID, cr.CID, cr.SEID, cr.KID))

	storeInstance, err := store.NewWithPath(databasePath)
	if err != nil {
		return nil, err
	}
	return &Agent{
		Agent:         webQrcodeAgent,
		StoreInstance: storeInstance,
	}, nil
}

// ReloginWithCookies re-creates the agent with new cookies.
func ReloginWithCookies(cookies string) (*Agent, error) {
	return NewAgent(cookies)
}

func (ag *Agent) ListSubscriptions() ([]SubInfo, error) {
	if ag.StoreInstance == nil {
		return nil, errors.New("store not initialized")
	}
	subs, err := ag.StoreInstance.ListSubscriptions()
	if err != nil {
		return nil, err
	}
	result := make([]SubInfo, 0, len(subs))
	for _, s := range subs {
		result = append(result, SubInfo{
			ID: s.ID, Name: s.Name, MediaType: s.MediaType,
			Season: s.Season, Cid: s.Cid, Savepath: s.Savepath, Enabled: s.Enabled,
		})
	}
	return result, nil
}

func (ag *Agent) AddSubscription(name, mediaType, cid, savepath string, season int) error {
	if ag.StoreInstance == nil {
		return errors.New("store not initialized")
	}
	_, err := ag.StoreInstance.AddSubscription(&store.Subscription{
		Name: name, MediaType: mediaType, Season: season,
		Cid: cid, Savepath: savepath, Enabled: true,
	})
	return err
}

func (ag *Agent) UpdateSubscription(id int, name, mediaType, cid, savepath string, season int, enabled bool) error {
	if ag.StoreInstance == nil {
		return errors.New("store not initialized")
	}
	return ag.StoreInstance.UpdateSubscription(&store.Subscription{
		ID: id, Name: name, MediaType: mediaType, Season: season,
		Cid: cid, Savepath: savepath, Enabled: enabled,
	})
}

func (ag *Agent) DeleteSubscription(id int) error {
	if ag.StoreInstance == nil {
		return errors.New("store not initialized")
	}
	return ag.StoreInstance.DeleteSubscription(id)
}

func SaveCookies(agent *elevengo.Agent) {
	cr := &elevengo.Credential{}
	agent.CredentialExport(cr)
	cookies := fmt.Sprintf("UID=%s; CID=%s; SEID=%s; KID=%s", cr.UID, cr.CID, cr.SEID, cr.KID)
	SaveCookiesToFile(cookies)
}

// SaveCookiesToFile writes cookies string directly to the cookiefile.
func SaveCookiesToFile(cookies string) {
	savePath := determineCookieSavePath()
	if err := os.WriteFile(savePath, []byte(cookies), 0600); err != nil {
		log.Printf("failed to save cookies to %s: %v\n", savePath, err)
	}
}

func LoadCookies() string {
	cookies, err := LoadCookiesWithError()
	if err != nil {
		return ""
	}
	return cookies
}

func LoadCookiesWithError() (string, error) {
	cfg, _, err := config.LoadWithOptions(config.CLIParams{}, config.LoadOptions{Auth: true})
	if err != nil {
		return "", err
	}
	return cfg.Auth.Cookies, nil
}

// determineCookieSavePath determines where to save cookies based on configuration source
// Priority: original file > TOML cookies_file > current directory .cookies
func determineCookieSavePath() string {
	_, source, err := config.LoadWithOptions(config.CLIParams{}, config.LoadOptions{Auth: true})
	if err != nil {
		// Fallback to current directory if config loading fails
		return config.ExistingCookiePathOrDefault()
	}

	// Priority 1: Use the file that was originally read
	if source.CookiesPath != "" {
		return source.CookiesPath
	}

	// Priority 2: Use TOML-configured cookies_file
	if source.TOMLPath != "" {
		cfg, err := config.LoadTOML(source.TOMLPath)
		if err == nil && cfg.Auth.CookiesFile != "" {
			return config.ResolveCookiesFile(cfg.Auth.CookiesFile, source.TOMLPath)
		}
	}

	// Priority 3: Fallback to current directory
	return config.ExistingCookiePathOrDefault()
}

func QrcodeLogin() (*Agent, error) {
	agent := newElevengoAgent()
	session := &elevengo.QrcodeSession{}
	// @TODO: add option; default is tv
	err := agent.QrcodeStart(session, option.Qrcode().LoginTv())
	if err != nil {
		return nil, err
	}
	err = DisplayQrcode(session.Image)
	if err != nil {
		return nil, err
	}
	after := time.Now().Add(2 * time.Minute)
	for {
		time.Sleep(200 * time.Millisecond)
		success, err := agent.QrcodePoll(session)
		if success {
			SaveCookies(agent)
			DisposeQrcode()

			storeInstance, err := store.NewWithPath(databasePath)
			if err != nil {
				return nil, err
			}
			return &Agent{
				Agent:         agent,
				StoreInstance: storeInstance,
			}, nil
		}
		if err != nil && err == elevengo.ErrQrcodeCancelled {
			return nil, errors.New("login cancelled")
		}
		if time.Now().After(after) {
			return nil, errors.New("login timed out")
		}
	}
}

func newElevengoAgent() *elevengo.Agent {
	opts := option.Agent()
	if cooldownMinMs > 0 || cooldownMaxMs > 0 {
		minMs := cooldownMinMs
		maxMs := cooldownMaxMs
		if maxMs == 0 {
			maxMs = minMs
		}
		if maxMs < minMs {
			maxMs = minMs
		}
		opts.WithCooldown(minMs, maxMs)
	}
	return elevengo.New(opts)
}
