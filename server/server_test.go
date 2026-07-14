package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/deadblue/elevengo"
	p115pkg "github.com/mguyenanastacio-glitch/pan-fetcher/p115"
)

type fakeServerAgent struct {
	magnetTasks [][]string
	cidValues   []string
	savePaths   []string
	rssURLs     []string
	clearTypes   []int
	userInfo     elevengo.UserInfo
	storeClosed  bool
}

func (f *fakeServerAgent) AddMagnetTask(tasks []string, cid, savepath string) error {
	f.magnetTasks = append(f.magnetTasks, append([]string(nil), tasks...))
	f.cidValues = append(f.cidValues, cid)
	f.savePaths = append(f.savePaths, savepath)
	return nil
}

func (f *fakeServerAgent) AddRssUrlTask(url string) {
	f.rssURLs = append(f.rssURLs, url)
}

func (f *fakeServerAgent) ExecuteAllRssTask() {}
func (f *fakeServerAgent) ProcessRSSFeed(url, cid, savepath, kw, subKey string) {}

func (f *fakeServerAgent) OfflineClear(num int) error {
	f.clearTypes = append(f.clearTypes, num)
	return nil
}

func (f *fakeServerAgent) UserGet(info *elevengo.UserInfo) error {
	*info = f.userInfo
	return nil
}

func (f *fakeServerAgent) StoreClose() error {
	f.storeClosed = true
	return nil
}

func (f *fakeServerAgent) ListTasks() ([]p115pkg.TaskItem, error) {
	return nil, nil
}

func (f *fakeServerAgent) ListDir(dirID string) ([]p115pkg.DirEntry, error) {
	return nil, nil
}

func (f *fakeServerAgent) GetEntry(entryID string) (p115pkg.DirEntry, error) {
	return p115pkg.DirEntry{}, nil
}

func (f *fakeServerAgent) ProcessRssWithSubscriptions(url string)           {}
func (f *fakeServerAgent) ListSubscriptions() ([]p115pkg.SubInfo, error)    { return nil, nil }
func (f *fakeServerAgent) AddSubscription(name, mtype, cid, sp string, s int) error { return nil }
func (f *fakeServerAgent) UpdateSubscription(id int, name, mtype, cid, sp string, s int, en bool) error { return nil }
func (f *fakeServerAgent) DeleteSubscription(id int) error                  { return nil }
func (f *fakeServerAgent) GetSettings() p115pkg.AppSettings                   { return p115pkg.AppSettings{} }
func (f *fakeServerAgent) UpdateSettings(s p115pkg.AppSettings) error         { return nil }
func (f *fakeServerAgent) TestConnection() error                               { return nil }
func (f *fakeServerAgent) LoadCookiesStr() string                               { return "" }
func (f *fakeServerAgent) Mkdir(parentID, name string) (p115pkg.DirEntry, error) { return p115pkg.DirEntry{}, nil }
func (f *fakeServerAgent) RenameEntry(entryID, newName string) error              { return nil }
func (f *fakeServerAgent) DeleteEntry(entryID string) error                       { return nil }
func (f *fakeServerAgent) MoveEntry(targetDirID, entryID string) error            { return nil }
func (f *fakeServerAgent) Copy(targetDirID, entryID string) error                 { return nil }

func newTestServer() (*Server, *fakeServerAgent, *http.ServeMux) {
	fake := &fakeServerAgent{
		userInfo: elevengo.UserInfo{Id: 123, Name: "tester", IsVip: true},
	}
	srv := &Server{Agent: fake, Port: 8115, startTime: time.Now()}
	mux := http.NewServeMux()
	srv.registerRoutes(mux)
	return srv, fake, mux
}

func TestDashboardRenders(t *testing.T) {
	_, _, mux := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	if !strings.Contains(body, "pan-fetcher") {
		t.Fatalf("dashboard title missing: %s", body)
	}
	// Dashboard shows stats cards (check for rendered label)
	if !strings.Contains(body, "推送") && !strings.Contains(body, "Push") {
		t.Fatalf("dashboard stats missing: %s", body)
	}
	if !strings.Contains(body, "/tasks") {
		t.Fatalf("dashboard tasks link missing: %s", body)
	}
}

func TestTasksPageHasForms(t *testing.T) {
	_, _, mux := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/tasks", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	if !strings.Contains(body, "/add") || !strings.Contains(body, "/clear") {
		t.Fatalf("tasks page forms missing: %s", body)
	}
}

func TestAddTaskForm(t *testing.T) {
	_, fake, mux := newTestServer()
	body := strings.NewReader("tasks=magnet%3A%3Fxt%3Durn%3Abtih%3A1%0Amagnet%3A%3Fxt%3Durn%3Abtih%3A2&cid=cid-1&savepath=anime")
	req := httptest.NewRequest(http.MethodPost, "/add", body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d", rec.Code)
	}
	if len(fake.magnetTasks) != 1 {
		t.Fatalf("expected one magnet task batch, got %d", len(fake.magnetTasks))
	}
	if len(fake.magnetTasks[0]) != 2 {
		t.Fatalf("expected 2 tasks, got %#v", fake.magnetTasks[0])
	}
	if fake.cidValues[0] != "cid-1" || fake.savePaths[0] != "anime" {
		t.Fatalf("unexpected cid/savepath: %v %v", fake.cidValues, fake.savePaths)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}
}

func TestRssAndClearActions(t *testing.T) {
	_, fake, mux := newTestServer()

	// RSS feed grab
	form := "rss_url=https%3A%2F%2Fexample.com%2Frss&keyword=&cid=123"
	rssReq := httptest.NewRequest(http.MethodPost, "/rss/feed", strings.NewReader(form))
	rssReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rssRec := httptest.NewRecorder()
	mux.ServeHTTP(rssRec, rssReq)
	if rssRec.Code != http.StatusOK {
		t.Fatalf("unexpected rss status: %d", rssRec.Code)
	}

	// Clear
	clearReq := httptest.NewRequest(http.MethodPost, "/clear", strings.NewReader("type=1"))
	clearReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	clearRec := httptest.NewRecorder()
	mux.ServeHTTP(clearRec, clearReq)
	if len(fake.clearTypes) != 1 || fake.clearTypes[0] != 0 {
		t.Fatalf("unexpected clear calls: %#v", fake.clearTypes)
	}
	if !strings.Contains(clearRec.Body.String(), "已执行清理类型 1") {
		t.Fatalf("unexpected clear response: %s", clearRec.Body.String())
	}
}
