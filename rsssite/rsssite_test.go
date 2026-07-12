package rsssite

import (
	"crypto/sha1"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mmcdole/gofeed"
)

func TestDmhy(t *testing.T) {
	dmhy := &Dmhy{}
	feed := mustParseFeed(t, `
<rss version="2.0">
  <channel>
    <title>dmhy</title>
    <item>
      <title>episode 1</title>
      <link>https://example.com/1</link>
      <description>desc</description>
      <content:encoded xmlns:content="http://purl.org/rss/1.0/modules/content/">content</content:encoded>
      <enclosure url="magnet:?xt=urn:btih:1111111111111111111111111111111111111111&dn=episode-1" type="application/x-bittorrent"/>
    </item>
  </channel>
</rss>`)

	got := dmhy.GetMagnetItem(feed.Items[0])
	if got.Magnet != "magnet:?xt=urn:btih:1111111111111111111111111111111111111111" {
		t.Fatalf("unexpected magnet: %+v", got)
	}
}

func TestAcgnx(t *testing.T) {
	acgnx := &Acgnx{}
	feed := mustParseFeed(t, `
<rss version="2.0">
  <channel>
    <title>acgnx</title>
    <item>
      <title>episode 1</title>
      <link>https://example.com/1</link>
      <description>desc</description>
      <enclosure url="magnet:?xt=urn:btih:2222222222222222222222222222222222222222" type="application/x-bittorrent"/>
    </item>
  </channel>
</rss>`)

	got := acgnx.GetMagnetItem(feed.Items[0])
	if got.Magnet != "magnet:?xt=urn:btih:2222222222222222222222222222222222222222" {
		t.Fatalf("unexpected magnet: %+v", got)
	}
}

func TestAcgrip(t *testing.T) {
	acgrip := &Acgrip{}
	info := []byte("d4:name4:test12:piece lengthi16384e6:lengthi12345e6:pieces20:aaaaaaaaaaaaaaaaaaaae")
	torrentBody := append([]byte("d8:announce14:http://tracker4:info"), info...)
	torrentBody = append(torrentBody, 'e')
	infoHash := sha1.Sum(info)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".torrent") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(torrentBody)
	}))
	defer server.Close()

	feed := mustParseFeed(t, fmt.Sprintf(`
<rss xmlns:torrent="http://xmlns.ezrss.it/0.1/" xmlns:media="http://search.yahoo.com/mrss/" version="2.0">
  <channel>
    <title>ACG.RIP</title>
    <item>
      <title>[喵萌奶茶屋&LoliHouse] 二十世纪电气目录 - 01</title>
      <link>https://acg.rip/t/357724</link>
      <guid>https://acg.rip/t/357724</guid>
      <enclosure url="%s/t/357724.torrent" type="application/x-bittorrent"/>
      <torrent:contentLength>819421184</torrent:contentLength>
      <media:content url="%s/t/357724.torrent" fileSize="819421184"/>
    </item>
  </channel>
</rss>`, server.URL, server.URL))

	got := acgrip.GetMagnetItem(feed.Items[0])
	want := fmt.Sprintf("magnet:?xt=urn:btih:%X&dn=%s", infoHash, url.QueryEscape(feed.Items[0].Title))
	if got.Magnet != want {
		t.Fatalf("unexpected magnet: got %q want %q", got.Magnet, want)
	}
}

func TestNormalizeTaskURL(t *testing.T) {
	info := []byte("d4:name4:test12:piece lengthi16384e6:lengthi12345e6:pieces20:aaaaaaaaaaaaaaaaaaaae")
	torrentBody := append([]byte("d8:announce14:http://tracker4:info"), info...)
	torrentBody = append(torrentBody, 'e')
	infoHash := sha1.Sum(info)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(torrentBody)
	}))
	defer server.Close()

	gotMagnet := NormalizeTaskURL("magnet:?xt=urn:btih:1234567890abcdef1234567890abcdef12345678&dn=test", "")
	if gotMagnet != "magnet:?xt=urn:btih:1234567890abcdef1234567890abcdef12345678" {
		t.Fatalf("unexpected normalized magnet: %q", gotMagnet)
	}

	want := fmt.Sprintf("magnet:?xt=urn:btih:%X&dn=%s", infoHash, url.QueryEscape("episode 1"))
	gotTorrent := NormalizeTaskURL(server.URL+"/episode.torrent", "episode 1")
	if gotTorrent != want {
		t.Fatalf("unexpected normalized torrent: got %q want %q", gotTorrent, want)
	}
}

func TestClassifyURL(t *testing.T) {
	tests := []struct {
		url  string
		want TaskURLType
	}{
		{"magnet:?xt=urn:btih:abc123&dn=test", TaskURLMagnet},
		{"magnet:?xt=urn:btih:abc123", TaskURLMagnet},
		{"https://example.com/file.torrent", TaskURLTorrent},
		{"http://acg.rip/t/123.torrent", TaskURLTorrent},
		{"ed2k://|file|test|123|abc|/", TaskURLEd2k},
		{"ed2k://|file|name|size|md4|/", TaskURLEd2k},
		{"https://example.com/file.zip", TaskURLHttp},
		{"http://example.com/download", TaskURLHttp},
		{"  ", TaskURLUnknown},
		{"", TaskURLUnknown},
		{"not-a-url", TaskURLUnknown},
	}
	for _, tt := range tests {
		got := ClassifyURL(tt.url)
		if got != tt.want {
			t.Errorf("ClassifyURL(%q) = %v, want %v", tt.url, got, tt.want)
		}
	}
}

func mustParseFeed(t *testing.T, rss string) *gofeed.Feed {
	t.Helper()
	feed, err := gofeed.NewParser().ParseString(rss)
	if err != nil {
		t.Fatalf("ParseString failed: %v", err)
	}
	if len(feed.Items) == 0 {
		t.Fatal("expected at least one item")
	}
	return feed
}

func TestAnibt(t *testing.T) {
	anibt := &Anibt{}
	data, err := os.ReadFile("../test/anibt.rss")
	if err != nil {
		t.Fatal(err)
	}

	fp := gofeed.NewParser()
	feed, err := fp.ParseString(string(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Items) == 0 {
		t.Fatal("expected at least one anibt item")
	}

	got := anibt.GetMagnet(feed.Items[0])
	want := "magnet:?xt=urn:btih:8b8c2f0a461a212b0b7417289376ff243284edc6"
	if got != want {
		t.Fatalf("expected first magnet %q, got %q", want, got)
	}

	for _, item := range feed.Items {
		magnet := anibt.GetMagnet(item)
		if !strings.HasPrefix(magnet, "magnet:?xt=urn:btih:") {
			t.Fatalf("expected magnet URI for %q, got %q", item.Title, magnet)
		}
	}
}

func TestGetRssConfigByURL(t *testing.T) {
	rssConfig := GetRssConfigByURL("http://share.dmhy.org/topics/rss/rss.xml")
	t.Log(rssConfig)
}

func TestGetRssConfigByURLMatchesSchemeAndQueryOrder(t *testing.T) {
	SetRssJsonPath("../rss.json")
	t.Cleanup(func() {
		SetRssJsonPath("")
	})

	rssConfig := GetRssConfigByURL("https://share.dmhy.org/topics/rss/rss.xml?sort_id=2&team_id=0&order=date-desc&keyword=%E6%B0%B4%E6%98%9F%E7%9A%84%E9%AD%94%E5%A5%B3")
	if rssConfig == nil {
		t.Fatal("expected config, got nil")
	}
	if rssConfig.SavePath != "文件夹名称" {
		t.Fatalf("expected savepath 文件夹名称, got %q", rssConfig.SavePath)
	}
	if rssConfig.Cid != "" {
		t.Fatalf("expected empty cid from sample config, got %q", rssConfig.Cid)
	}
}

func TestReadRssConfigDictFromUserConfigDir(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("USERPROFILE", homeDir)
	t.Setenv("HOMEDRIVE", "")
	t.Setenv("HOMEPATH", "")

	configDir := filepath.Join(homeDir, ".config", "pan-fetcher")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("failed to create config dir: %v", err)
	}
	configFile := filepath.Join(configDir, "rss.json")
	configContent := `{"example.com":[{"name":"from-config-dir","url":"https://example.com/rss"}]}`
	if err := os.WriteFile(configFile, []byte(configContent), 0o600); err != nil {
		t.Fatalf("failed to create rss config: %v", err)
	}

	originalWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("failed to change working directory: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(originalWD)
		SetRssJsonPath("")
		RssConfigDict = nil
	})
	SetRssJsonPath("")
	RssConfigDict = nil

	configs := ReadRssConfigDict()
	if configs == nil {
		t.Fatalf("expected rss config to be read")
	}
	got := (*configs)["example.com"]
	if len(got) != 1 || got[0].Name != "from-config-dir" {
		t.Fatalf("unexpected rss config: %#v", got)
	}
}
