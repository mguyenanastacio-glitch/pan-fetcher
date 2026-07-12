package media

import (
	"regexp"
	"strconv"
	"strings"
)

// MediaType represents the type of media.
type MediaType int

const (
	MediaUnknown MediaType = iota
	MediaMovie
	MediaTV
	MediaAnime
)

func (m MediaType) String() string {
	switch m {
	case MediaMovie:
		return "movie"
	case MediaTV:
		return "tv"
	case MediaAnime:
		return "anime"
	default:
		return "unknown"
	}
}

// MediaInfo stores parsed metadata from a torrent title.
type MediaInfo struct {
	RawTitle    string   // Original torrent title
	Title       string   // Clean show/movie name
	AltTitle    string   // Alternative title (e.g. English)
	Year        int      // Release year
	Season      int      // Season number (0 = movie or unknown)
	Episode     int      // Episode number (0 = movie or unknown)
	EpisodeRange []int   // Episode range [start, end] for batches
	Resolution  string   // 1080p, 720p, 4K, etc.
	Source      string   // Web-DL, BDRip, BluRay, etc.
	Codec       string   // x264, x265, HEVC, AV1, etc.
	Audio       string   // AAC, FLAC, DTS, etc.
	Group       string   // Sub group tag
	SeasonText  string   // Raw season text e.g. "S02"
	MediaType   MediaType
}

var (
	// Common patterns in torrent titles
	reYear       = regexp.MustCompile(`\b(19\d{2}|20\d{2})\b`)
	reSeason     = regexp.MustCompile(`(?i)s(?:eason)?[\.\s]*(\d{1,2})`)
	reEpisode    = regexp.MustCompile(`(?i)(?:ep?(?:isode)?|第)[\.\s]*(\d{1,4})`)
	reEpRange    = regexp.MustCompile(`(?i)(?:ep?(?:isode)?s?[\.\s]*)?(\d{1,4})\s*[-~&]\s*(\d{1,4})`)
	reResolution = regexp.MustCompile(`(?i)\b(2160p|1080p|720p|480p|4K)\b`)
	reSource     = regexp.MustCompile(`(?i)\b(Web[-.\s]?DL|WebRip|BDRip|BluRay|Blu[-.\s]?Ray|HDTV|DVD|Remux|HMAX|AMZN|NF|DSNP|ATVP)\b`)
	reCodec      = regexp.MustCompile(`(?i)\b(x264|h264|x265|h265|HEVC|AV1|VP9|Xvid|DivX)\b`)
	reAudio      = regexp.MustCompile(`(?i)\b(AAC|FLAC|DTS(?:[-.]HD)?(?:\s*MA)?|TrueHD|AC3|EAC3|MP3|AAC\d?\.\d?)\b`)
	reGroup      = regexp.MustCompile(`[-@]\s*\[?(\w[\]\[\w\s&.!+]+)\]?$`)

	// Anime-specific
	reAnimeEp    = regexp.MustCompile(`[-–]\s*(\d{1,4})(?:\s*(?:END|v\d|\[))?`)
	reAnimeBracket = regexp.MustCompile(`^\[([^\]]+)\]\s*\[([^\]]+)\]`)
)

// ParseTitle parses a torrent title into structured metadata.
func ParseTitle(raw string) *MediaInfo {
	info := &MediaInfo{RawTitle: raw, MediaType: MediaUnknown}

	// Strip leading/trailing noise
	cleaned := cleanTitle(raw)
	info.Title = cleaned

	// Detect media type: anime patterns first
	if isAnime(raw) {
		info.MediaType = MediaAnime
		parseAnime(raw, info)
	} else {
		// Check for season/episode → TV, otherwise movie
		if reSeason.MatchString(cleaned) || reEpisode.MatchString(cleaned) || reEpRange.MatchString(cleaned) {
			info.MediaType = MediaTV
			parseTV(cleaned, info)
		} else {
			info.MediaType = MediaMovie
			parseMovie(cleaned, info)
		}
	}

	// Extract common attributes
	extractAttributes(cleaned, info)

	return info
}

func cleanTitle(raw string) string {
	// Remove file extension
	raw = strings.TrimSpace(raw)
	if idx := strings.LastIndex(raw, "."); idx > 0 {
		ext := strings.ToLower(raw[idx:])
		if ext == ".mkv" || ext == ".mp4" || ext == ".ts" || ext == ".avi" || ext == ".torrent" {
			raw = raw[:idx]
		}
	}
	return raw
}

func isAnime(name string) bool {
	// Pattern 1: Starts with [GroupName] ... followed by episode dash like " - 01 "
	if strings.HasPrefix(name, "[") {
		closeIdx := strings.Index(name, "]")
		if closeIdx > 1 && closeIdx < 40 {
			rest := strings.TrimSpace(name[closeIdx+1:])
			// Check for episode number pattern: "Title - 01" or "Title 01"
			if matched, _ := regexp.MatchString(`[-–\s]\d{1,4}(?:\s|$|\[|\.mkv)`, rest); matched {
				return true
			}
		}
	}
	// Pattern 2: Japanese episode numbering "第01話"
	if strings.Contains(name, "第") && strings.Contains(name, "話") {
		return true
	}
	return false
}

func parseAnime(raw string, info *MediaInfo) {
	// Pattern: [Group] Title... - 01 [Metadata...]
	if strings.HasPrefix(raw, "[") {
		closeIdx := strings.Index(raw, "]")
		if closeIdx > 0 {
			info.Group = raw[1:closeIdx]
			rest := strings.TrimSpace(raw[closeIdx+1:])

			// Remove file extension
			rest = strings.TrimSuffix(rest, ".mkv")
			rest = strings.TrimSuffix(rest, ".mp4")
			rest = strings.TrimSuffix(rest, ".ts")
			rest = strings.TrimSuffix(rest, ".torrent")

			// Remove trailing bracketed metadata groups like [WebRip 1080p ...]
			for strings.Contains(rest, "[") && strings.Contains(rest, "]") {
				lastOpen := strings.LastIndex(rest, "[")
				lastClose := strings.LastIndex(rest, "]")
				if lastClose > lastOpen {
					rest = strings.TrimSpace(rest[:lastOpen])
				} else {
					break
				}
			}
			// Remove trailing parenthetical metadata like (1080p)
			for strings.HasSuffix(rest, ")") {
				if openIdx := strings.LastIndex(rest, "("); openIdx > 0 {
					rest = strings.TrimSpace(rest[:openIdx])
				} else {
					break
				}
			}

			// Extract episode number: "Title - 01" or "Title - 01v2" or "Title 01"
			epRe := regexp.MustCompile(`[-–\s]+(\d{1,4})(?:v\d)?$`)
			if m := epRe.FindStringSubmatch(rest); len(m) >= 2 {
				info.Episode, _ = strconv.Atoi(m[1])
				info.Title = strings.TrimSpace(rest[:strings.LastIndex(rest, m[0])])
			} else {
				info.Title = rest
			}
			return
		}
	}
	info.Title = raw
}

func parseTV(cleaned string, info *MediaInfo) {
	// Extract season
	if m := reSeason.FindStringSubmatch(cleaned); len(m) >= 2 {
		info.Season, _ = strconv.Atoi(m[1])
		info.SeasonText = m[0]
	}

	// Extract episode(s)
	if m := reEpRange.FindStringSubmatch(cleaned); len(m) >= 3 {
		start, _ := strconv.Atoi(m[1])
		end, _ := strconv.Atoi(m[2])
		info.EpisodeRange = []int{start, end}
		info.Episode = start
	} else if m := reEpisode.FindStringSubmatch(cleaned); len(m) >= 2 {
		info.Episode, _ = strconv.Atoi(m[1])
	}

	// Extract year
	if m := reYear.FindStringSubmatch(cleaned); len(m) >= 2 {
		info.Year, _ = strconv.Atoi(m[1])
	}

	// Clean title: remove season/episode/quality metadata
	info.Title = cleanShowTitle(cleaned)
}

func parseMovie(cleaned string, info *MediaInfo) {
	if m := reYear.FindStringSubmatch(cleaned); len(m) >= 2 {
		info.Year, _ = strconv.Atoi(m[1])
	}
	info.Title = cleanShowTitle(cleaned)
}

func cleanShowTitle(s string) string {
	// Remove common patterns to get the base title
	remove := []*regexp.Regexp{
		reSeason, reEpisode, reEpRange,
		reResolution, reSource, reCodec, reAudio,
		regexp.MustCompile(`(?i)\b(COMPLETE|PROPER|REPACK|RERiP|EXTENDED|UNCUT|DIRECTORS.CUT|THEATRICAL|LIMITED)\b`),
		regexp.MustCompile(`(?i)\[(?:SubsPlease|Erai-raws|Ohys-Raws|MTBB|UCCUSS|VCB-Studio|ANK-Raws|ReinForce|Moozzi2|LowPower|Snow-Raws|LoliHouse|Nekomoe|Comicat|Sakurato|SweetSub|CASO|SumiSora|FLsnow|KissSub|FZsub|HYSUB|DMG|Airota|Nekomoe kissaten|喵萌|奶茶|桜都|澄空|华盟|雪飘|极影|轻之国度|诸神|漫猫|爱恋|幻之|风之圣殿|千夏|云光|Lilith-Raws|DBD-Raws|7³ACG)\]`),
		regexp.MustCompile(`(?i)\b(JP|CN|TW|HK|US|UK|KR|FR|DE)\.?\b`),
		regexp.MustCompile(`[-_.+\s]+$`),
	}

	for _, r := range remove {
		s = r.ReplaceAllString(s, " ")
	}

	// Collapse whitespace
	s = regexp.MustCompile(`\s+`).ReplaceAllString(strings.TrimSpace(s), " ")
	return s
}

func extractAttributes(s string, info *MediaInfo) {
	if m := reResolution.FindStringSubmatch(s); len(m) >= 2 {
		info.Resolution = strings.ToUpper(m[1])
	}
	if m := reSource.FindStringSubmatch(s); len(m) >= 2 {
		info.Source = m[1]
	}
	if m := reCodec.FindStringSubmatch(s); len(m) >= 2 {
		info.Codec = strings.ToUpper(m[1])
	}
	if m := reAudio.FindStringSubmatch(s); len(m) >= 2 {
		info.Audio = strings.ToUpper(m[1])
	}
	if m := reGroup.FindStringSubmatch(s); len(m) >= 2 {
		info.Group = strings.TrimSpace(m[1])
	}
	// If no group found via regex, try anime bracket
	if info.Group == "" {
		if m := reAnimeBracket.FindStringSubmatch(s); len(m) >= 2 {
			info.Group = strings.TrimSpace(m[1])
		}
	}
}

// IsEpisode checks if this media entry represents a specific episode.
func (m *MediaInfo) IsEpisode() bool {
	return m.Episode > 0
}

// String returns a human-readable summary.
func (m *MediaInfo) String() string {
	parts := []string{m.Title}
	if m.Year > 0 {
		parts = append(parts, strconv.Itoa(m.Year))
	}
	if m.Season > 0 {
		parts = append(parts, "S"+padZero(m.Season))
	}
	if m.Episode > 0 {
		parts = append(parts, "E"+padZero(m.Episode))
	}
	if m.Resolution != "" {
		parts = append(parts, m.Resolution)
	}
	return strings.Join(parts, " ")
}

func padZero(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}
