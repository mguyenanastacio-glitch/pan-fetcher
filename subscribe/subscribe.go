package subscribe

import (
	"log"
	"strings"

	"github.com/mguyenanastacio-glitch/pan-fetcher/media"
	"github.com/mguyenanastacio-glitch/pan-fetcher/store"
)

// MatchResult describes the result of matching a torrent against subscriptions.
type MatchResult struct {
	Subscription *store.Subscription
	MediaInfo    *media.MediaInfo
	Confidence   float64 // 0-1 match confidence
}

// Engine handles subscription matching and RSS processing.
type Engine struct {
	Store *store.Store
}

func New(store *store.Store) *Engine {
	return &Engine{Store: store}
}

// Match checks a parsed title against all enabled subscriptions.
// Returns the best match or nil.
func (e *Engine) Match(info *media.MediaInfo) *MatchResult {
	subs, err := e.Store.ListSubscriptions()
	if err != nil {
		log.Printf("[subscribe] failed to list subscriptions: %v", err)
		return nil
	}

	var best *MatchResult
	var bestScore float64

	for i := range subs {
		sub := &subs[i]
		if !sub.Enabled {
			continue
		}

		score := matchScore(info, sub)
		if score > bestScore && score > 0.3 { // minimum threshold
			bestScore = score
			best = &MatchResult{
				Subscription: sub,
				MediaInfo:    info,
				Confidence:   score,
			}
		}
	}

	return best
}

// matchScore returns a 0-1 score indicating how well a title matches a subscription.
func matchScore(info *media.MediaInfo, sub *store.Subscription) float64 {
	infoType := info.MediaType.String()

	if sub.MediaType == "anime" && infoType != "anime" && infoType != "unknown" {
		return 0
	}
	if sub.MediaType == "tv" && infoType == "anime" {
		return 0
	}

	titleScore := titleSimilarity(info.Title, sub.Name)

	// TMDB-enhanced matching: try matching against the parsed title's best TMDB entry
	if titleScore < 0.5 && media.DefaultTMDB != nil {
		if tmdbResult := media.DefaultTMDB.BestMatch(info.Title, sub.MediaType); tmdbResult != nil {
			// Also check TMDB display name against subscription
			if s := titleSimilarity(tmdbResult.DisplayName(), sub.Name); s > titleScore {
				titleScore = s
			}
		}
	}

	if titleScore < 0.3 {
		return 0
	}

	weight := titleScore * 0.7

	// Season bonus: if subscription specifies a season and the torrent matches
	if sub.Season > 0 && info.Season > 0 {
		if info.Season == sub.Season {
			weight += 0.2
		} else {
			return 0 // Wrong season
		}
	} else if info.Season > 0 {
		weight += 0.1 // Has season info
	}

	// Episode bonus
	if info.Episode > 0 {
		weight += 0.1
	}

	if weight > 1.0 {
		weight = 1.0
	}
	return weight
}

// titleSimilarity computes a 0-1 score for title matching.
func titleSimilarity(a, b string) float64 {
	a = normalizeTitle(a)
	b = normalizeTitle(b)

	if a == "" || b == "" {
		return 0
	}

	// Exact match
	if a == b {
		return 1.0
	}

	// One contains the other
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return 0.85
	}

	// Token overlap
	ta := strings.Fields(a)
	tb := strings.Fields(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}

	overlap := 0
	for _, tokenA := range ta {
		if len(tokenA) < 2 {
			continue
		}
		for _, tokenB := range tb {
			if len(tokenB) < 2 {
				continue
			}
			if tokenA == tokenB {
				overlap++
				break
			}
		}
	}

	ratio := float64(overlap) / float64(max(len(ta), len(tb)))
	return ratio
}

func normalizeTitle(s string) string {
	s = strings.ToLower(s)
	s = strings.TrimSpace(s)
	// Remove common noise
	for _, ch := range []string{"[", "]", "(", ")", ".", "_", "-", ":", "!", "~"} {
		s = strings.ReplaceAll(s, ch, " ")
	}
	s = strings.Join(strings.Fields(s), " ")
	return s
}

// IsNewEpisode checks if this specific episode has not been submitted yet.
func (e *Engine) IsNewEpisode(subID int, season, episode int) bool {
	return !e.Store.HasSubmitted(subID, episode, season)
}
