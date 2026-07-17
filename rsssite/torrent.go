package rsssite

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	urlPkg "net/url"
	"strings"
	"time"

	"github.com/mguyenanastacio-glitch/pan-fetcher/config"
)

// TorrentHashCache caches .torrent URL → info hash mappings.
// Implementations should be thread-safe and may persist to disk.
type TorrentHashCache interface {
	GetTorrentHash(url string) (hash string, ok bool)
	SetTorrentHash(url, hash string)
	GetTorrentError(url string) (errMsg string, ok bool)
	SetTorrentError(url, errMsg string)
}

// torrentCache is the global cache instance, set by server at startup.
var torrentCache TorrentHashCache

// SetTorrentHashCache sets the global torrent hash cache.
func SetTorrentHashCache(c TorrentHashCache) {
	torrentCache = c
}

// TaskURLType classifies a task URL into its protocol category.
type TaskURLType int

const (
	TaskURLUnknown TaskURLType = iota
	TaskURLMagnet
	TaskURLTorrent
	TaskURLEd2k
	TaskURLHttp
)

func (t TaskURLType) String() string {
	switch t {
	case TaskURLMagnet:
		return "magnet"
	case TaskURLTorrent:
		return "torrent"
	case TaskURLEd2k:
		return "ed2k"
	case TaskURLHttp:
		return "http"
	default:
		return "unknown"
	}
}

// ClassifyURL returns the type of the given task URL.
func ClassifyURL(rawURL string) TaskURLType {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return TaskURLUnknown
	}
	switch {
	case strings.HasPrefix(rawURL, "magnet:?"):
		return TaskURLMagnet
	case strings.HasPrefix(strings.ToLower(rawURL), "ed2k://"):
		return TaskURLEd2k
	case strings.HasSuffix(strings.ToLower(rawURL), ".torrent"):
		return TaskURLTorrent
	case strings.HasPrefix(strings.ToLower(rawURL), "http://") || strings.HasPrefix(strings.ToLower(rawURL), "https://"):
		return TaskURLHttp
	default:
		return TaskURLUnknown
	}
}

func NormalizeTaskURL(taskURL, title string) string {
	taskURL = strings.TrimSpace(taskURL)
	if taskURL == "" {
		return ""
	}
	if strings.HasPrefix(taskURL, "magnet:?") {
		return taskURL // keep full magnet including dn= for display names
	}
	if strings.HasSuffix(strings.ToLower(taskURL), ".torrent") {
		return normalizeTorrentURL(taskURL, title)
	}
	// Try downloading as torrent for URLs that don't end with .torrent
	// (e.g. Jackett API download links). Use cache to avoid repeated downloads.
	if strings.HasPrefix(strings.ToLower(taskURL), "http://") || strings.HasPrefix(strings.ToLower(taskURL), "https://") {
		if magnet := normalizeTorrentURL(taskURL, title); !strings.HasPrefix(magnet, "http") {
			return magnet // successfully converted to magnet
		}
	}
	return taskURL
}

func normalizeTorrentURL(taskURL, title string) string {
	magnet, err := torrentURLToMagnet(taskURL, title)
	if err != nil {
		log.Printf("[torrent] download/parse failed for %s: %v\n", taskURL, err)
		return taskURL
	}
	if magnet != "" {
		log.Printf("[torrent] converted %s -> %s\n", taskURL, magnet)
		return magnet
	}
	return taskURL
}

func torrentURLToMagnet(torrentURL, title string) (string, error) {
	// Check cache
	if torrentCache != nil {
		if hash, ok := torrentCache.GetTorrentHash(torrentURL); ok {
			return buildMagnet(hash, title), nil
		}
		if errMsg, ok := torrentCache.GetTorrentError(torrentURL); ok {
			return "", errors.New(errMsg)
		}
	}

	body, err := downloadTorrentFast(torrentURL)
	if err != nil {
		if torrentCache != nil {
			torrentCache.SetTorrentError(torrentURL, err.Error())
		}
		return "", err
	}
	hash, err := torrentInfoHash(body)
	if err != nil {
		if torrentCache != nil {
			torrentCache.SetTorrentError(torrentURL, err.Error())
		}
		return "", err
	}
	if torrentCache != nil {
		torrentCache.SetTorrentHash(torrentURL, hash)
	}
	return buildMagnet(hash, title), nil
}

func buildMagnet(hash, title string) string {
	magnet := "magnet:?xt=urn:btih:" + hash
	if title != "" {
		magnet += "&dn=" + urlPkg.QueryEscape(title)
	}
	return magnet
}

// fastHTTPClient returns a lightweight client with proxy support for quick torrent downloads.
func fastHTTPClient() *http.Client {
	proxyFunc := http.ProxyFromEnvironment
	if u, _ := http.ProxyFromEnvironment(&http.Request{URL: &urlPkg.URL{Scheme: "https"}}); u == nil {
		// Fallback to config proxy
		if cfg, _, err := config.LoadWithOptions(config.CLIParams{}, config.LoadOptions{}); err == nil && cfg.Proxy.HTTP != "" {
			if proxyURL, err := urlPkg.Parse(cfg.Proxy.HTTP); err == nil {
				proxyFunc = http.ProxyURL(proxyURL)
			}
		}
	}
	transport := &http.Transport{
		Proxy:                 proxyFunc,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   12 * time.Second,
	}
}

func downloadTorrentFast(torrentURL string) ([]byte, error) {
	client := fastHTTPClient()
	resp, err := client.Get(torrentURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func torrentInfoHash(data []byte) (string, error) {
	if len(data) == 0 || data[0] != 'd' {
		return "", errors.New("invalid torrent file")
	}
	pos := 1
	for pos < len(data) && data[pos] != 'e' {
		key, next, err := parseBencodeString(data, pos)
		if err != nil {
			return "", err
		}
		pos = next
		if key == "info" {
			start := pos
			end, err := skipBencodeValue(data, pos)
			if err != nil {
				return "", err
			}
			sum := sha1.Sum(data[start:end])
			return strings.ToUpper(hex.EncodeToString(sum[:])), nil
		}
		pos, err = skipBencodeValue(data, pos)
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("info dictionary not found")
}

func parseBencodeString(data []byte, pos int) (string, int, error) {
	start := pos
	for pos < len(data) && data[pos] != ':' {
		if data[pos] < '0' || data[pos] > '9' {
			return "", 0, errors.New("invalid bencode string length")
		}
		pos++
	}
	if pos >= len(data) || data[pos] != ':' {
		return "", 0, errors.New("invalid bencode string")
	}
	length := 0
	for i := start; i < pos; i++ {
		length = length*10 + int(data[i]-'0')
	}
	pos++
	end := pos + length
	if end > len(data) {
		return "", 0, errors.New("bencode string out of range")
	}
	return string(data[pos:end]), end, nil
}

func skipBencodeValue(data []byte, pos int) (int, error) {
	if pos >= len(data) {
		return 0, errors.New("unexpected end of bencode")
	}
	switch data[pos] {
	case 'i':
		end := indexByte(data, pos+1, 'e')
		if end < 0 {
			return 0, errors.New("invalid bencode integer")
		}
		return end + 1, nil
	case 'l', 'd':
		pos++
		if data[pos-1] == 'd' {
			for pos < len(data) && data[pos] != 'e' {
				if data[pos] < '0' || data[pos] > '9' {
					return 0, errors.New("invalid bencode dict key")
				}
				_, next, err := parseBencodeString(data, pos)
				if err != nil {
					return 0, err
				}
				pos = next
				next, err = skipBencodeValue(data, pos)
				if err != nil {
					return 0, err
				}
				pos = next
			}
			if pos >= len(data) || data[pos] != 'e' {
				return 0, errors.New("unterminated bencode dictionary")
			}
			return pos + 1, nil
		}
		for pos < len(data) && data[pos] != 'e' {
			next, err := skipBencodeValue(data, pos)
			if err != nil {
				return 0, err
			}
			pos = next
		}
		if pos >= len(data) || data[pos] != 'e' {
			return 0, errors.New("unterminated bencode list")
		}
		return pos + 1, nil
	default:
		if data[pos] < '0' || data[pos] > '9' {
			return 0, errors.New("invalid bencode value")
		}
		_, next, err := parseBencodeString(data, pos)
		return next, err
	}
}

func indexByte(data []byte, start int, target byte) int {
	for i := start; i < len(data); i++ {
		if data[i] == target {
			return i
		}
	}
	return -1
}