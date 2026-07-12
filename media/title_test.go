package media

import (
	"fmt"
	"testing"
)

func TestParseTitle(t *testing.T) {
	tests := []struct {
		raw     string
		want    MediaType
		episode int
		season  int
		year    int
	}{
		// Anime patterns
		{"[Nekomoe kissaten&LoliHouse] 20 Seiki Denki Mokuroku - 01 [WebRip 1080p HEVC-10bit AAC ASSx2].mkv", MediaAnime, 1, 0, 0},
		{"[SubsPlease] Sousou no Frieren - 12 (1080p) [B1D169A2].mkv", MediaAnime, 12, 0, 0},
		{"[Erai-raws] One Piece - 1090 [1080p][Multiple Subtitle].mkv", MediaAnime, 1090, 0, 0},

		// TV show patterns
		{"The.Mandalorian.S03E01.1080p.DSNP.WEB-DL.DDP5.1.H.264-NTb", MediaTV, 1, 3, 0},
		{"Breaking Bad S05E14 1080p BDRip x264", MediaTV, 14, 5, 0},
		{"Game.of.Thrones.S08E06.REPACK.720p.AMZN.WEB-DL.DDP5.1.H.264", MediaTV, 6, 8, 0},
		{"Rick and Morty - S07E01 - 720p HDTV x264", MediaTV, 1, 7, 0},

		// Movie patterns
		{"Oppenheimer.2023.1080p.BluRay.x264.AAC5.1", MediaMovie, 0, 0, 2023},
		{"Dune Part Two 2024 2160p WEB-DL HDR HEVC", MediaMovie, 0, 0, 2024},
		{"John.Wick.Chapter.4.2023.1080p.BluRay.x265-RARBG", MediaMovie, 0, 0, 2023},
	}

	for _, tt := range tests {
		info := ParseTitle(tt.raw)
		errs := []string{}
		if info.MediaType != tt.want {
			errs = append(errs, fmt.Sprintf("type: got %v want %v", info.MediaType, tt.want))
		}
		if tt.episode > 0 && info.Episode != tt.episode {
			errs = append(errs, fmt.Sprintf("episode: got %d want %d", info.Episode, tt.episode))
		}
		if tt.season > 0 && info.Season != tt.season {
			errs = append(errs, fmt.Sprintf("season: got %d want %d", info.Season, tt.season))
		}
		if tt.year > 0 && info.Year != tt.year {
			errs = append(errs, fmt.Sprintf("year: got %d want %d", info.Year, tt.year))
		}

		if len(errs) > 0 {
			t.Errorf("%s:\n  %v\n  summary=%s", tt.raw, errs, info.String())
		} else {
			t.Logf("✓ %s → %s [type=%s]", tt.raw, info.String(), info.MediaType)
		}
	}
}

func TestExtractAttributes(t *testing.T) {
	info := ParseTitle("[Nekomoe kissaten&LoliHouse] 20 Seiki Denki Mokuroku - 01 [WebRip 1080p HEVC-10bit AAC ASSx2].mkv")
	t.Logf("Title: %s", info.Title)
	t.Logf("Episode: %d", info.Episode)
	t.Logf("Resolution: %s", info.Resolution)
	t.Logf("Source: %s", info.Source)
	t.Logf("Codec: %s", info.Codec)
	t.Logf("Audio: %s", info.Audio)
	t.Logf("Group: %s", info.Group)
	t.Logf("MediaType: %s", info.MediaType)
}
