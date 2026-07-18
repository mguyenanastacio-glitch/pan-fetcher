package indexer

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/PuerkitoBio/goquery"
	"gopkg.in/yaml.v3"

	"github.com/mguyenanastacio-glitch/pan-fetcher/config"
)

// Engine is the core indexer engine: loads definitions, executes searches.
type Engine struct {
	definitions  map[string]*IndexerDefinition
	httpClient   *http.Client
	searchClient *http.Client // shorter timeout for search
	cookieJars   map[string]*cookiejar.Jar // per-indexer cookie jars (by def.ID)
	cjMu         sync.RWMutex
}

// NewEngine creates an indexer engine by loading YAML definitions from a directory.
func NewEngine(defDir string) (*Engine, error) {
	e := &Engine{
		definitions:  make(map[string]*IndexerDefinition),
		httpClient:   newBrowserClient(),
		searchClient: newSearchClient(),
		cookieJars:   make(map[string]*cookiejar.Jar),
	}
	if defDir == "" {
		return e, nil
	}
	entries, err := os.ReadDir(defDir)
	if err != nil {
		return e, nil // empty engine is valid
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yml") {
			continue
		}
		path := filepath.Join(defDir, entry.Name())
		def, err := e.loadDefinition(path)
		if err != nil {
			log.Printf("[indexer] failed to load %s: %v", entry.Name(), err)
			continue
		}
		e.definitions[def.ID] = def
		log.Printf("[indexer] loaded %s (%s)", def.ID, def.Name)
	}
	return e, nil
}

// newBrowserClient creates a shared HTTP client with connection pooling, keep-alive,
// and proxy support. Proxy priority: HTTPS_PROXY env → config file → direct connection.
func newBrowserClient() *http.Client {
	// Determine proxy function
	proxyFunc := proxyFromConfig()
	return &http.Client{
		Timeout: 45 * time.Second,
		Transport: &http.Transport{
			Proxy: proxyFunc,
			DialContext: (&net.Dialer{
				Timeout:   15 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          50,
			MaxIdleConnsPerHost:   10,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 15 {
				return fmt.Errorf("stopped after %d redirects", len(via))
			}
			return nil
		},
	}
}

// newSearchClient creates a client with shorter timeout for search operations.
func newSearchClient() *http.Client {
	c := newBrowserClient()
	c.Timeout = 30 * time.Second
	return c
}

// proxyFromConfig returns a proxy function. Priority:
// 1. HTTPS_PROXY / HTTP_PROXY environment variables
// 2. Explicit proxy in config.toml [proxy].http
// 3. Direct connection (no proxy)
func proxyFromConfig() func(*http.Request) (*url.URL, error) {
	// Priority 1: environment variables
	if u, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https"}}); err == nil && u != nil {
		return http.ProxyFromEnvironment
	}

	// Priority 2: explicit config.toml proxy (only if user configured it)
	cfg, src, err := config.LoadWithOptions(config.CLIParams{}, config.LoadOptions{})
	if err == nil && cfg.Proxy.HTTP != "" {
		// Only use the config proxy if it came from a TOML file (not the hard-coded default)
		if src != nil && src.TOMLPath != "" {
			if proxyURL, err := url.Parse(cfg.Proxy.HTTP); err == nil {
				return http.ProxyURL(proxyURL)
			}
		}
	}

	// Priority 3: direct connection
	return nil
}

// doRequest performs an HTTP GET with browser-like headers using the shared client.
func (e *Engine) doRequest(url string) (*http.Response, error) {
	return e.doRequestWithJar(url, "")
}

// doRequestWithJar performs an HTTP GET using the cookie jar for the given indexer (if any).
func (e *Engine) doRequestWithJar(rawURL string, defID string) (*http.Response, error) {
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	// Use per-indexer cookie jar if available
	client := e.httpClient
	if defID != "" {
		e.cjMu.RLock()
		jar, ok := e.cookieJars[defID]
		e.cjMu.RUnlock()
		if ok && jar != nil {
			// Clone the client with the cookie jar
			clone := *e.httpClient
			clone.Jar = jar
			client = &clone
		}
	}
	return client.Do(req)
}

// doPostWithJar performs an HTTP POST with form data using the cookie jar for the given indexer.
func (e *Engine) doPostWithJar(rawURL string, defID string, formData url.Values) (*http.Response, error) {
	req, err := http.NewRequest("POST", rawURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := e.httpClient
	if defID != "" {
		e.cjMu.RLock()
		jar, ok := e.cookieJars[defID]
		e.cjMu.RUnlock()
		if ok && jar != nil {
			clone := *e.httpClient
			clone.Jar = jar
			client = &clone
		}
		// Create jar if not exists
		if !ok {
			e.cjMu.Lock()
			if _, exists := e.cookieJars[defID]; !exists {
				jar, _ := cookiejar.New(nil)
				e.cookieJars[defID] = jar
			}
			e.cjMu.Unlock()
		}
	}
	return client.Do(req)
}

// loadDefinition parses a single YAML definition file.
func (e *Engine) loadDefinition(path string) (*IndexerDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var def IndexerDefinition
	if err := yaml.Unmarshal(data, &def); err != nil {
		return nil, fmt.Errorf("yaml parse: %w", err)
	}
	// Use Site as ID fallback (Cardigann convention)
	if def.ID == "" {
		def.ID = def.Site
	}
	if def.ID == "" {
		return nil, fmt.Errorf("missing id/site")
	}
	return &def, nil
}

// ListDefinitions returns all loaded indexer definitions.
func (e *Engine) ListDefinitions() []IndexerInfo {
	out := make([]IndexerInfo, 0, len(e.definitions))
	for _, def := range e.definitions {
		siteLink := ""
		if len(def.Links) > 0 {
			siteLink = def.Links[0]
		}
		out = append(out, IndexerInfo{
			ID:       def.ID,
			Name:     def.Name,
			Type:     def.Type,
			Language: def.Language,
			SiteLink: siteLink,
			HasLogin: def.Login != nil,
		})
	}
	return out
}

// GetDefinition returns a single definition by ID.
func (e *Engine) GetDefinition(id string) *IndexerDefinition {
	return e.definitions[id]
}

// ReloadDefinition reloads a single definition from its YAML file.
func (e *Engine) ReloadDefinition(id, path string) error {
	def, err := e.loadDefinition(path)
	if err != nil {
		return err
	}
	if def.ID != "" {
		id = def.ID
	}
	e.definitions[id] = def
	return nil
}

// RemoveDefinition removes a definition from memory.
func (e *Engine) RemoveDefinition(id string) {
	delete(e.definitions, id)
}

// Search performs a search on a single indexer.
func (e *Engine) Search(defID string, req SearchRequest) ([]SearchResult, error) {
	def := e.definitions[defID]
	if def == nil {
		return nil, fmt.Errorf("unknown indexer: %s", defID)
	}
	return e.searchSingle(def, req)
}

// SearchAll performs a search across all loaded indexers concurrently.
func (e *Engine) SearchAll(req SearchRequest) []SearchResult {
	type result struct {
		results []SearchResult
		err     error
		id      string
	}
	ch := make(chan result, len(e.definitions))
	for _, def := range e.definitions {
		go func(d *IndexerDefinition) {
			r, err := e.searchSingle(d, req)
			ch <- result{results: r, err: err, id: d.ID}
		}(def)
	}
	var all []SearchResult
	for i := 0; i < len(e.definitions); i++ {
		r := <-ch
		if r.err != nil {
			log.Printf("[indexer] %s search error: %v", r.id, r.err)
			continue
		}
		all = append(all, r.results...)
	}
	// Sort by seeders descending
	sortResults(all, "")
	if req.Limit > 0 && len(all) > req.Limit {
		all = all[:req.Limit]
	}
	return all
}

// TestConnection makes a GET request with browser headers to verify real connectivity.
func (e *Engine) TestConnection(def *IndexerDefinition) error {
	if len(def.Links) == 0 {
		return fmt.Errorf("no site link")
	}
	// Test the main site directly (faster than building a search URL)
	resp, err := e.doRequestWithJar(def.Links[0], def.ID)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return nil
}

// Login authenticates with a non-public tracker using its login definition.
// Username and password are passed as the login inputs.
func (e *Engine) Login(defID, username, password string) error {
	def := e.definitions[defID]
	if def == nil {
		return fmt.Errorf("unknown indexer: %s", defID)
	}
	if def.Login == nil {
		return fmt.Errorf("indexer %s has no login definition (public tracker)", defID)
	}

	baseURL := def.Links[0]
	loginURL, err := url.JoinPath(baseURL, def.Login.Path)
	if err != nil {
		return fmt.Errorf("invalid login url: %w", err)
	}

	// Build form data from login inputs, substituting {{ .Username }} and {{ .Password }}
	formData := url.Values{}
	for key, val := range def.Login.Inputs {
		v := val
		v = strings.ReplaceAll(v, "{{ .Username }}", username)
		v = strings.ReplaceAll(v, "{{ .Password }}", password)
		formData.Set(key, v)
	}

	resp, err := e.doPostWithJar(loginURL, defID, formData)
	if err != nil {
		return fmt.Errorf("login request failed: %w", err)
	}
	defer resp.Body.Close()

	// Check for login errors
	for _, errBlock := range def.Login.Error {
		if errBlock.Selector == "" {
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		doc, docErr := goquery.NewDocumentFromReader(bytes.NewReader(body))
		if docErr == nil {
			sel := doc.Find(errBlock.Selector)
			if sel.Length() > 0 {
				msg := "authentication failed"
				if errBlock.Message != nil && errBlock.Message.Selector != "" {
					msg = strings.TrimSpace(sel.Find(errBlock.Message.Selector).Text())
				}
				return fmt.Errorf("login error: %s", msg)
			}
		}
		// Reset body for further reading
		resp.Body = io.NopCloser(bytes.NewReader(body))
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("login returned http %d", resp.StatusCode)
	}

	log.Printf("[indexer] %s login successful", defID)
	return nil
}
func (e *Engine) searchSingle(def *IndexerDefinition, req SearchRequest) ([]SearchResult, error) {
	// Build search URL from template
	searchURL, err := e.buildSearchURL(def, req)
	if err != nil {
		return nil, err
	}

	// Fetch page with search-specific client (shorter timeout)
	httpReq, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/132.0.0.0 Safari/537.36")
	httpReq.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	httpReq.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	client := e.searchClient
	if def.ID != "" {
		e.cjMu.RLock()
		jar, ok := e.cookieJars[def.ID]
		e.cjMu.RUnlock()
		if ok && jar != nil {
			clone := *e.searchClient
			clone.Jar = jar
			client = &clone
		}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Parse HTML with CSS selectors
	return e.parseResults(def, bytes.NewReader(body), req)
}

// buildSearchURL constructs the search URL from the indexer definition and request.
func (e *Engine) buildSearchURL(def *IndexerDefinition, req SearchRequest) (string, error) {
	baseURL := def.Links[0]
	if baseURL == "" {
		baseURL = "https://" + def.ID
	}

	// Build template data (compatible with Cardigann {{ .Query.Keywords }})
	tmplData := map[string]interface{}{
		"Keywords": req.Query,
		"Keyword":  req.Query,
		"Category": req.Category,
		"Page":     req.Page,
		"Query": map[string]string{
			"Keywords": req.Query,
			"Keyword":  req.Query,
			"Q":        req.Query,
		},
	}
	if req.Season > 0 {
		tmplData["Season"] = req.Season
	}
	if req.Episode > 0 {
		tmplData["Episode"] = req.Episode
	}

	// Collect path templates: from .Paths (our format) or .Path (Cardigann single)
	var pathTemplates []string
	for _, sp := range def.Search.Paths {
		pathTemplates = append(pathTemplates, sp.Path)
	}
	if len(pathTemplates) == 0 && def.Search.Path != "" {
		pathTemplates = append(pathTemplates, def.Search.Path)
	}
	if len(pathTemplates) == 0 {
		return "", fmt.Errorf("no search path defined")
	}

	// Try each path template
	var lastErr error
	for _, pathTmpl := range pathTemplates {
		// Render Go template
		renderedPath, err := renderTemplate(pathTmpl, tmplData)
		if err != nil {
			lastErr = err
			continue
		}

		// Gather query parameters from inputs
		params := url.Values{}
		if def.Search.Inputs != nil {
			for k, v := range def.Search.Inputs {
				val, err := renderTemplate(v, tmplData)
				if err != nil {
					val = v
				}
				if val != "" {
					// Skip page/p param when value is "0" (first page uses no page param)
					if (k == "page" || k == "p") && val == "0" {
						continue
					}
					params.Set(k, val)
				}
			}
		}

		u, err := url.Parse(baseURL)
		if err != nil {
			return "", err
		}
		u = u.JoinPath(renderedPath)
		if len(params) > 0 {
			u.RawQuery = params.Encode()
		}
		return u.String(), nil
	}
	return "", fmt.Errorf("all path templates failed: %w", lastErr)
}

// renderTemplate renders a Go template string with the given data.
func renderTemplate(tmplStr string, data interface{}) (string, error) {
	tmpl, err := template.New("t").Parse(tmplStr)
	if err != nil {
		return tmplStr, nil // return original on parse error
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return tmplStr, nil
	}
	return strings.TrimSpace(buf.String()), nil
}

// parseResults extracts search results from HTML using CSS selectors.
func (e *Engine) parseResults(def *IndexerDefinition, body io.Reader, req SearchRequest) ([]SearchResult, error) {
	doc, err := goquery.NewDocumentFromReader(body)
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}

	rowsSel := def.Search.Rows.Selector
	sel := doc.Find(rowsSel)
	if sel.Length() == 0 {
		return nil, nil
	}

	var results []SearchResult
	after := def.Search.Rows.After

	sel.Each(func(i int, row *goquery.Selection) {
		if i < after {
			return
		}
		if req.Limit > 0 && len(results) >= req.Limit {
			return
		}

		result := SearchResult{
			IndexerID:   def.ID,
			IndexerName: def.Name,
			Category:    "other",
		}

		// Phase 1: extract selector-based fields into a map for template evaluation
		rawFields := make(map[string]string)
		for name, f := range def.Search.Fields {
			if f.Text != "" {
				continue // defer text-template fields to phase 2
			}
			val := extractField(row, f)
			rawFields[name] = val
		}

		// Phase 2: evaluate text-template fields against rawFields
		for name, f := range def.Search.Fields {
			if f.Text == "" {
				continue
			}
			val := evalFieldText(f.Text.String(), rawFields)
			for _, flt := range f.Filters {
				val = applyFilter(val, flt)
			}
			rawFields[name] = val
		}

		// Map extracted fields to result
		if v, ok := rawFields["title"]; ok && v != "" {
			result.Title = v
		}
		if result.Title == "" {
			return
		}

		// Map download/magnet to MagnetURL
		if v, ok := rawFields["download"]; ok && v != "" {
			result.MagnetURL = resolveURL(def.Links[0], v)
		}
		if result.MagnetURL == "" {
			if v, ok := rawFields["magnet"]; ok && v != "" {
				result.MagnetURL = v
			}
		}
		if v, ok := rawFields["size"]; ok {
			result.Size = parseSize(v)
		}
		if v, ok := rawFields["seeders"]; ok {
			result.Seeders, _ = strconv.Atoi(strings.TrimSpace(v))
		}
		if v, ok := rawFields["leechers"]; ok {
			result.Leechers, _ = strconv.Atoi(strings.TrimSpace(v))
		}
		if v, ok := rawFields["date"]; ok {
			result.PublishDate = parseDate(v)
		}
		if v, ok := rawFields["category"]; ok {
			result.Category = strings.TrimSpace(v)
		}
		if v, ok := rawFields["page"]; ok && v != "" {
			result.PageURL = resolveURL(def.Links[0], v)
		}
		if result.PageURL == "" {
			if v, ok := rawFields["details"]; ok && v != "" {
				result.PageURL = resolveURL(def.Links[0], v)
			}
		}

		// Extract infohash from magnet
		if result.MagnetURL != "" {
			result.InfoHash = extractInfoHash(result.MagnetURL)
		}

		results = append(results, result)
	})

	return results, nil
}

// extractField extracts a single field from a DOM row using CSS selector.
func extractField(row *goquery.Selection, f FieldBlock) string {
	// Selector-based extraction
	var val string
	if f.Selector != "" {
		sel := row.Find(f.Selector)
		if sel.Length() > 0 {
			if f.Attribute != "" {
				val, _ = sel.First().Attr(f.Attribute)
			} else {
				val = strings.TrimSpace(sel.First().Text())
			}
		}
	} else if f.Text == "" && len(f.Filters) > 0 {
		// Filter-only: apply filter to the entire row text
		val = strings.TrimSpace(row.Text())
	}
	// Apply filters
	for _, flt := range f.Filters {
		val = applyFilter(val, flt)
	}
	return val
}

// evalFieldText evaluates a field text template against raw extracted field values.
func evalFieldText(tmplStr string, rawFields map[string]string) string {
	data := struct {
		Result map[string]string
		Config map[string]bool
	}{
		Result: rawFields,
		Config: map[string]bool{
			"sonarr_compatibility": false,
			"prefer_magnet_links":  true,
		},
	}
	tmpl, err := template.New("field").Funcs(template.FuncMap{
		"or": func(args ...string) string {
			for _, a := range args {
				if a != "" {
					return a
				}
			}
			return ""
		},
	}).Option("missingkey=zero").Parse(tmplStr)
	if err != nil {
		return tmplStr // return raw text if template parse fails
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return tmplStr
	}
	return strings.TrimSpace(buf.String())
}

// applyFilter transforms a string value using a named filter.
func applyFilter(val string, f FilterBlock) string {
	// Evaluate template expressions in args before applying
	evalArgs := make([]string, len(f.Args))
	copy(evalArgs, f.Args)
	for i, a := range evalArgs {
		if strings.Contains(a, "{{") {
			evalArgs[i] = evalFilterArgTemplate(a)
		}
	}

	switch f.Name {
	case "trim":
		return strings.TrimSpace(val)
	case "replace":
		if len(evalArgs) >= 2 {
			val = strings.ReplaceAll(val, evalArgs[0], evalArgs[1])
		}
	case "regexp":
		if len(evalArgs) >= 1 {
			re, err := regexp.Compile(evalArgs[0])
			if err == nil {
				val = re.FindString(val)
			}
		}
	case "re_replace":
		if len(evalArgs) >= 2 {
			re, err := regexp.Compile(evalArgs[0])
			if err == nil {
				val = re.ReplaceAllString(val, evalArgs[1])
			}
		}
	case "prefix":
		if len(evalArgs) >= 1 {
			val = evalArgs[0] + val
		}
	case "suffix":
		if len(evalArgs) >= 1 {
			val = val + evalArgs[0]
		}
	case "lower":
		return strings.ToLower(val)
	case "upper":
		return strings.ToUpper(val)
	case "urldecode":
		decoded, err := url.QueryUnescape(val)
		if err == nil {
			return decoded
		}
	case "split":
		if len(evalArgs) >= 2 {
			parts := strings.Split(val, evalArgs[0])
			idx, err := strconv.Atoi(evalArgs[1])
			if err == nil && idx >= 0 && idx < len(parts) {
				return parts[idx]
			}
			// Negative index counts from end
			if err == nil && idx < 0 && -idx <= len(parts) {
				return parts[len(parts)+idx]
			}
		}
	case "append":
		if len(evalArgs) >= 1 {
			return val + evalArgs[0]
		}
	case "dateparse":
		return val
	}
	return val
}

// evalFilterArgTemplate evaluates simple boolean template expressions in filter args.
func evalFilterArgTemplate(tmplStr string) string {
	// Simple case: just return the raw string if it's not a recognizable template
	if !strings.Contains(tmplStr, "{{") {
		return tmplStr
	}
	return tmplStr // Return as-is for now - complex eval deferred
}

// ------ helpers ------

func parseSize(s string) int64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" {
		return 0
	}
	re := regexp.MustCompile(`([\d.]+)\s*(GB|MB|KB|TB|B)`)
	m := re.FindStringSubmatch(s)
	if len(m) != 3 {
		return 0
	}
	v, _ := strconv.ParseFloat(m[1], 64)
	switch m[2] {
	case "TB":
		return int64(v * 1024 * 1024 * 1024 * 1024)
	case "GB":
		return int64(v * 1024 * 1024 * 1024)
	case "MB":
		return int64(v * 1024 * 1024)
	case "KB":
		return int64(v * 1024)
	default:
		return int64(v)
	}
}

func parseDate(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "now" || s == "Now" {
		return time.Now()
	}
	// Unix timestamp (seconds since epoch)
	if ts, err := strconv.ParseInt(s, 10, 64); err == nil && ts > 1000000000 && ts < 9999999999 {
		return time.Unix(ts, 0)
	}
	layouts := []string{
		"2006-01-02 15:04",
		"2006-01-02",
		"01-02 15:04",
		"2006/01/02 15:04",
		"2006/01/02 15:04 -07:00", // Mikan: 2026/07/10 16:35 +08:00
		"2006-01-02 15:04 -07:00",
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"1/2/2006 3:04 PM",     // Mikan US format: 7/10/2026 8:35 AM
		"1/2/2006 15:04",       // Mikan alternate
		"1/2/2006 3:04 PM -07:00", // Mikan with timezone
		"01/02/2006 15:04",     // dmhy alternate
		time.RFC1123,
		time.RFC1123Z,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	// Try relative time
	if strings.Contains(s, "分钟") || strings.Contains(s, "min") {
		return time.Now().Add(-10 * time.Minute)
	}
	if strings.Contains(s, "小时") || strings.Contains(s, "hour") {
		return time.Now().Add(-2 * time.Hour)
	}
	if strings.Contains(s, "天") || strings.Contains(s, "day") {
		return time.Now().Add(-24 * time.Hour)
	}
	return time.Time{}
}

func extractInfoHash(magnet string) string {
	re := regexp.MustCompile(`btih:([0-9a-fA-F]{40})`)
	m := re.FindStringSubmatch(magnet)
	if len(m) >= 2 {
		return strings.ToLower(m[1])
	}
	return ""
}

func resolveURL(baseURL, href string) string {
	if strings.HasPrefix(href, "http") {
		return href
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return href
	}
	ref, err := url.Parse(href)
	if err != nil {
		return href
	}
	return u.ResolveReference(ref).String()
}

func sortResults(results []SearchResult, sortBy string) {
	sort.Slice(results, func(i, j int) bool {
		switch sortBy {
		case "size":
			return results[i].Size > results[j].Size
		case "date":
			return results[i].PublishDate.After(results[j].PublishDate)
		default: // "seeds"
			if results[i].Seeders != results[j].Seeders {
				return results[i].Seeders > results[j].Seeders
			}
			return results[i].PublishDate.After(results[j].PublishDate)
		}
	})
}

