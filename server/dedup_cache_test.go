package server

import (
	"os"
	"testing"
)

func TestDedupImplementsTorrentHashCache(t *testing.T) {
	// Use temp file to avoid messing with real dedup cache
	tmpFile := t.TempDir() + "/test-dedup.json"
	oldPath := dedupCachePath
	dedupCachePath = tmpFile
	defer func() { dedupCachePath = oldPath }()

	dc := &dedupCache{
		subs:          make(map[string]map[string]bool),
		torrentURLs:   make(map[string]string),
		torrentErrors: make(map[string]string),
		hashNames:     make(map[string]string),
	}

	// Test 1: Get/Set torrent hash
	_, ok := dc.GetTorrentHash("http://example.com/test.torrent")
	if ok {
		t.Error("expected cache miss")
	}
	dc.SetTorrentHash("http://example.com/test.torrent", "abc123def456")
	hash, ok := dc.GetTorrentHash("http://example.com/test.torrent")
	if !ok {
		t.Error("expected cache hit after Set")
	}
	if hash != "abc123def456" {
		t.Errorf("expected 'abc123def456', got '%s'", hash)
	}

	// Test 2: Get/Set torrent error
	dc.SetTorrentError("http://fail.example.com/bad.torrent", "timeout")
	errMsg, ok := dc.GetTorrentError("http://fail.example.com/bad.torrent")
	if !ok {
		t.Error("expected error cache hit")
	}
	if errMsg != "timeout" {
		t.Errorf("expected 'timeout', got '%s'", errMsg)
	}

	// Test 3: SetHash clears error
	dc.SetTorrentHash("http://fail.example.com/bad.torrent", "fixed456")
	_, ok = dc.GetTorrentError("http://fail.example.com/bad.torrent")
	if ok {
		t.Error("expected error cleared after SetHash")
	}

	// Test 4: Persistence
	if _, err := os.Stat(tmpFile); err != nil {
		t.Errorf("expected cache file to exist after save: %v", err)
	}

	// Test 5: Load from disk
	dc2 := &dedupCache{
		subs:          make(map[string]map[string]bool),
		torrentURLs:   make(map[string]string),
		torrentErrors: make(map[string]string),
		hashNames:     make(map[string]string),
	}
	oldPath2 := dedupCachePath
	dedupCachePath = tmpFile
	defer func() { dedupCachePath = oldPath2 }()
	dc2.Load()
	hash2, ok2 := dc2.GetTorrentHash("http://example.com/test.torrent")
	if !ok2 {
		t.Error("expected hash loaded from disk")
	}
	if hash2 != "abc123def456" {
		t.Errorf("expected 'abc123def456', got '%s'", hash2)
	}
	errMsg2, ok2 := dc2.GetTorrentError("http://fail.example.com/bad.torrent")
	if ok2 {
		t.Error("expected error NOT present after SetHash cleared it")
	}
	_ = errMsg2
}
