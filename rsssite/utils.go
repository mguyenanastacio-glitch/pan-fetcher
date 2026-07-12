package rsssite

import (
	"bufio"
	"os"
	"regexp"
	"strings"

	"github.com/mmcdole/gofeed"
)

// reMagnet matches magnet:?xt=urn:btih:... patterns in text.
var reMagnet = regexp.MustCompile(`magnet:\?xt=urn:btih:[A-Za-z0-9]{32,64}`)

// reBtih matches bare btih info hashes (40 hex chars) in URLs or text.
var reBtih = regexp.MustCompile(`(?i)(?:btih[:/-])([A-Fa-f0-9]{40})`)

// TryExtractInfoHash attempts lightweight info hash extraction from an RSS item
// without downloading any .torrent files. Returns the 40-char hex info hash or "".
func TryExtractInfoHash(item *gofeed.Item) string {
	// 1. Check enclosure URL for magnet: or btih pattern
	for _, enc := range item.Enclosures {
		if strings.HasPrefix(enc.URL, "magnet:?") {
			if h := extractHashFromMagnet(enc.URL); h != "" {
				return h
			}
		}
		if m := reBtih.FindStringSubmatch(enc.URL); len(m) >= 2 {
			return strings.ToLower(m[1])
		}
	}
	// 2. Check HTML content
	if h := extractHashFromText(item.Content); h != "" {
		return h
	}
	// 3. Check description
	if h := extractHashFromText(item.Description); h != "" {
		return h
	}
	// 4. Check title for btih
	if m := reBtih.FindStringSubmatch(item.Title); len(m) >= 2 {
		return strings.ToLower(m[1])
	}
	return ""
}

func extractHashFromText(text string) string {
	if text == "" {
		return ""
	}
	if m := reMagnet.FindString(text); m != "" {
		return extractHashFromMagnet(m)
	}
	if m := reBtih.FindStringSubmatch(text); len(m) >= 2 {
		return strings.ToLower(m[1])
	}
	return ""
}

func extractHashFromMagnet(magnet string) string {
	for _, part := range strings.Split(magnet, "&") {
		if strings.HasPrefix(strings.ToLower(part), "xt=urn:btih:") {
			return strings.ToLower(strings.TrimPrefix(part, "xt=urn:btih:"))
		}
	}
	return ""
}

// ExtractMagnetFromText scans text for a magnet link.
func ExtractMagnetFromText(text string) string {
	if text == "" {
		return ""
	}
	return reMagnet.FindString(text)
}

func HasPrefix(str string, prefixArr []string) bool {
	for _, prefix := range prefixArr {
		if strings.HasPrefix(str, prefix) {
			return true
		}
	}
	return false
}

func GetMagnetsFromText(textFile string) ([]string, error) {
	file, err := os.Open(textFile)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	prefixArr := []string{"magnet:", "ed2k://", "https://", "http://", "ftp://"}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		text := scanner.Text()
		if HasPrefix(text, prefixArr) {
			lines = append(lines, text)
		}
	}
	return lines, scanner.Err()
}
