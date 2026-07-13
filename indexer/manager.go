package indexer

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type healthInfo struct {
	healthy   bool
	lastError string
	lastTest  time.Time
}

// Manager handles the lifecycle of indexers: load, enable/disable, search.
type Manager struct {
	engine  *Engine
	mu      sync.RWMutex
	enabled map[string]bool
	health  map[string]*healthInfo
	defDir  string // for persisting enabled list
}

// NewManager creates a new indexer manager. Loads previously activated indexers from file.
func NewManager(defDir string) (*Manager, error) {
	engine, err := NewEngine(defDir)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		engine:  engine,
		enabled: make(map[string]bool),
		health:  make(map[string]*healthInfo),
		defDir:  defDir,
	}
	for _, info := range engine.ListDefinitions() {
		m.health[info.ID] = &healthInfo{healthy: true}
	}
	// Restore previously activated indexers
	m.loadEnabled(defDir)
	return m, nil
}

func (m *Manager) enabledFile(defDir string) string {
	return filepath.Join(defDir, ".enabled.json")
}

func (m *Manager) loadEnabled(defDir string) {
	data, err := os.ReadFile(m.enabledFile(defDir))
	if err != nil {
		return
	}
	var ids []string
	if err := json.Unmarshal(data, &ids); err != nil {
		return
	}
	for _, id := range ids {
		m.enabled[id] = true
	}
	log.Printf("[indexer] restored %d activated indexers", len(ids))
}

func (m *Manager) saveEnabled(defDir string) {
	m.mu.RLock()
	var ids []string
	for id := range m.enabled {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	data, _ := json.Marshal(ids)
	os.WriteFile(m.enabledFile(defDir), data, 0644)
}

// List returns only activated (enabled) indexers with their runtime state.
func (m *Manager) List() []IndexerInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	all := m.engine.ListDefinitions()
	var active []IndexerInfo
	for _, info := range all {
		if m.enabled[info.ID] {
			info.Enabled = true
			if h, ok := m.health[info.ID]; ok {
				info.Healthy = h.healthy
				info.LastError = h.lastError
				if !h.lastTest.IsZero() {
					info.LastTest = h.lastTest.Format("01-02 15:04")
				}
			}
			active = append(active, info)
		}
	}
	sort.Slice(active, func(i, j int) bool {
		return strings.ToLower(active[i].Name) < strings.ToLower(active[j].Name)
	})
	return active
}

// SetEnabled enables or disables an indexer.
func (m *Manager) SetEnabled(id string, enabled bool) {
	m.mu.Lock()
	m.enabled[id] = enabled
	m.mu.Unlock()
	m.saveEnabled(m.defDir)
}

// Activate moves an indexer from library to active (enabled).
func (m *Manager) Activate(id string) {
	m.SetEnabled(id, true)
}

// Deactivate moves an indexer back to library (disabled + removed from active).
func (m *Manager) Deactivate(id string) {
	m.mu.Lock()
	delete(m.enabled, id)
	m.mu.Unlock()
	m.saveEnabled(m.defDir)
}

// Library returns indexers that are available but NOT yet activated.
// Sorted by type (public → semi-private → private) then alphabetically.
func (m *Manager) Library() []IndexerInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	all := m.engine.ListDefinitions()
	var lib []IndexerInfo
	for _, info := range all {
		if !m.enabled[info.ID] {
			lib = append(lib, info)
		}
	}
	sort.Slice(lib, func(i, j int) bool {
		// type order: public < "private" types (semi-private, private)
		ti := typeOrder(lib[i].Type)
		tj := typeOrder(lib[j].Type)
		if ti != tj {
			return ti < tj
		}
		return strings.ToLower(lib[i].Name) < strings.ToLower(lib[j].Name)
	})
	return lib
}

// typeOrder returns a sort weight: public=0, semi-private=1, private=2, other=3.
func typeOrder(t string) int {
	switch strings.ToLower(t) {
	case "public":
		return 0
	case "semi-private":
		return 1
	case "private":
		return 2
	default:
		return 3
	}
}

// ActiveCount returns the number of activated indexers.
func (m *Manager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.enabled)
}

// LibraryCount returns the total number of definitions in the library.
func (m *Manager) LibraryCount() int {
	return len(m.engine.ListDefinitions())
}

// IsEnabled returns whether an indexer is enabled.
func (m *Manager) IsEnabled(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.enabled[id]
}

// Search searches a single indexer by ID.
func (m *Manager) Search(id string, req SearchRequest) ([]SearchResult, error) {
	if !m.IsEnabled(id) {
		return nil, nil
	}
	return m.engine.Search(id, req)
}

// SearchAllErrors holds search results and per-indexer error messages.
type SearchAllErrors struct {
	Results []SearchResult
	Errors  map[string]string // indexer ID → error message
}

// SearchAllWithErrors searches across all enabled indexers and returns both results and errors.
func (m *Manager) SearchAllWithErrors(req SearchRequest) SearchAllErrors {
	// Build enabled indexer list (lock only for this quick read)
	m.mu.RLock()
	allDefs := m.engine.ListDefinitions()
	indexerSet := make(map[string]bool)
	if len(req.Indexers) > 0 {
		for _, id := range req.Indexers {
			indexerSet[id] = true
		}
	}
	var enabledDefs []string
	for _, info := range allDefs {
		if !m.enabled[info.ID] {
			continue
		}
		if len(indexerSet) > 0 && !indexerSet[info.ID] {
			continue
		}
		enabledDefs = append(enabledDefs, info.ID)
	}
	m.mu.RUnlock()

	out := SearchAllErrors{
		Errors: make(map[string]string),
	}
	if len(enabledDefs) == 0 {
		if len(req.Indexers) > 0 {
			out.Errors["_none_"] = "选中的索引器未激活，请先在索引器管理页面激活"
		} else {
			out.Errors["_none_"] = "没有激活的索引器，请先在索引器管理页面添加"
		}
		return out
	}

	// Run searches concurrently (no lock held during HTTP requests)
	type result struct {
		results []SearchResult
		err     error
		id      string
	}
	ch := make(chan result, len(enabledDefs))
	for _, id := range enabledDefs {
		go func(defID string) {
			r, err := m.engine.Search(defID, req)
			ch <- result{results: r, err: err, id: defID}
		}(id)
	}
	for i := 0; i < len(enabledDefs); i++ {
		r := <-ch
		if r.err != nil {
			name := r.id
			if def := m.engine.GetDefinition(r.id); def != nil {
				name = def.Name
			}
			out.Errors[r.id] = fmt.Sprintf("%s: %v", name, r.err)
			continue
		}
		out.Results = append(out.Results, r.results...)
	}
	sortResults(out.Results, req.Sort)
	if req.Limit > 0 && len(out.Results) > req.Limit {
		out.Results = out.Results[:req.Limit]
	}
	return out
}

// Engine returns the underlying engine.
func (m *Manager) Engine() *Engine {
	return m.engine
}

// TestIndexer tests connectivity for a single indexer by ID.
func (m *Manager) TestIndexer(id string) error {
	def := m.engine.GetDefinition(id)
	if def == nil {
		return nil
	}
	err := m.engine.TestConnection(def)
	m.mu.Lock()
	if h, ok := m.health[id]; ok {
		h.lastTest = time.Now()
		h.healthy = (err == nil)
		if err != nil {
			h.lastError = err.Error()
		} else {
			h.lastError = ""
		}
	} else {
		m.health[id] = &healthInfo{
			healthy:   err == nil,
			lastError: "",
			lastTest:  time.Now(),
		}
		if err != nil {
			m.health[id].lastError = err.Error()
		}
	}
	m.mu.Unlock()
	if err != nil {
		log.Printf("[indexer] %s test failed: %v", id, err)
	}
	return err
}

// TestAll tests connectivity for only activated indexers.
func (m *Manager) TestAll() map[string]string {
	m.mu.RLock()
	var activeIDs []string
	for id := range m.enabled {
		activeIDs = append(activeIDs, id)
	}
	m.mu.RUnlock()

	results := make(map[string]string)
	for _, id := range activeIDs {
		err := m.TestIndexer(id)
		if err != nil {
			results[id] = err.Error()
		} else {
			results[id] = "ok"
		}
	}
	return results
}

// Login authenticates with a non-public tracker.
func (m *Manager) Login(id, username, password string) error {
	return m.engine.Login(id, username, password)
}

// GetDefinitionYAML returns the raw YAML content of an indexer definition.
func (m *Manager) GetDefinitionYAML(id string) (string, error) {
	path := filepath.Join(m.defDir, id+".yml")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// UpdateDefinitionYAML overwrites an indexer's YAML file and reloads it.
func (m *Manager) UpdateDefinitionYAML(id, yamlContent string) error {
	path := filepath.Join(m.defDir, id+".yml")
	if err := os.WriteFile(path, []byte(yamlContent), 0644); err != nil {
		return err
	}
	// Reload the definition
	return m.engine.ReloadDefinition(id, path)
}

// DeleteDefinition removes an indexer's YAML file and unloads it.
func (m *Manager) DeleteDefinition(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	path := filepath.Join(m.defDir, id+".yml")
	if err := os.Remove(path); err != nil {
		return err
	}
	delete(m.enabled, id)
	delete(m.health, id)
	m.engine.RemoveDefinition(id)
	m.saveEnabled(m.defDir)
	return nil
}
