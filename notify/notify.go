// Package notify provides notification sending for various channels.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	mu          sync.Mutex
	webhookURL  string
	tzLoc       *time.Location
	logEnabled  bool
	logQueue    = make(chan string, 200)
	client      = &http.Client{Timeout: 10 * time.Second}
	recentItems []RecentItem
	recentMax   = 10
	recentFile  = "recent-items.json"
)

// RecentItem is a newly added subscription resource shown on dashboard.
type RecentItem struct {
	Name string `json:"name"`
	Time string `json:"time"`
	Sub  string `json:"sub"`
}

// RecordItems adds items to the recent list for dashboard display.
func RecordItems(sub string, names []string) {
	ts := now().Format("01-02 15:04")
	mu.Lock()
	for _, n := range names {
		recentItems = append([]RecentItem{{Name: n, Time: ts, Sub: sub}}, recentItems...)
	}
	if len(recentItems) > recentMax {
		recentItems = recentItems[:recentMax]
	}
	saveRecentLocked()
	n := len(recentItems)
	mu.Unlock()
	log.Printf("[notify] recorded %d items for %q, total=%d", len(names), sub, n)
}

// LoadRecentItems restores the recent items list from disk.
func LoadRecentItems() {
	mu.Lock()
	data, err := os.ReadFile(recentFile)
	if err != nil {
		mu.Unlock()
		return
	}
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	if !utf8.Valid(data) {
		mu.Unlock()
		return
	}
	if err := json.Unmarshal(data, &recentItems); err != nil {
		mu.Unlock()
		return
	}
	if len(recentItems) > recentMax {
		recentItems = recentItems[:recentMax]
	}
	n := len(recentItems)
	mu.Unlock()
	log.Printf("[notify] loaded %d recent items from disk", n)
}

func saveRecentLocked() {
	data, err := json.Marshal(recentItems)
	if err != nil {
		log.Printf("[notify] recent save marshal error: %v", err)
		return
	}
	if err := os.WriteFile(recentFile, data, 0644); err != nil {
		log.Printf("[notify] recent save write error: %v", err)
	}
}

// RecentItems returns a copy of the recent items list.
func GetRecentItems() []RecentItem {
	mu.Lock()
	defer mu.Unlock()
	out := make([]RecentItem, len(recentItems))
	copy(out, recentItems)
	return out
}

func init() {
	go logSender()
}

func logSender() {
	for msg := range logQueue {
		mu.Lock()
		url := webhookURL
		mu.Unlock()
		if url != "" {
			doSend(url, msg)
		}
		time.Sleep(1 * time.Second)
	}
}

// SetWebhook configures the WeChat Work webhook URL.
func SetWebhook(url string) {
	mu.Lock()
	defer mu.Unlock()
	webhookURL = url
}

// SetTimezone sets the timezone for notification timestamps.
func SetTimezone(tz string) {
	mu.Lock()
	defer mu.Unlock()
	if tz == "" {
		tzLoc = nil
		return
	}
	loc, err := time.LoadLocation(tz)
	if err == nil {
		tzLoc = loc
	}
}

func now() time.Time {
	mu.Lock()
	loc := tzLoc
	mu.Unlock()
	if loc != nil {
		return time.Now().In(loc)
	}
	return time.Now()
}

// Webhook returns the current webhook URL.
func Webhook() string {
	mu.Lock()
	defer mu.Unlock()
	return webhookURL
}

// weworkMsg is the WeChat Work markdown message payload.
type weworkMsg struct {
	MsgType  string       `json:"msgtype"`
	Markdown weworkMD     `json:"markdown"`
}

type weworkMD struct {
	Content string `json:"content"`
}

// Send sends a markdown notification if enabled.
func Send(content string, enabled bool) {
	if !enabled {
		return
	}
	mu.Lock()
	url := webhookURL
	mu.Unlock()
	if url == "" {
		return
	}
	go doSend(url, content)
}

func doSend(url, content string) {
	msg := weworkMsg{
		MsgType: "markdown",
		Markdown: weworkMD{
			Content: content,
		},
	}
	body, err := json.Marshal(&msg)
	if err != nil {
		log.Printf("[notify] marshal error: %v", err)
		return
	}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("[notify] send failed: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Printf("[notify] wework responded %d: %s", resp.StatusCode, string(respBody))
	}
}

// TaskSubmitted formats a message for magnet task submission.
func TaskSubmitted(count int, cid string, names []string) string {
	msg := fmt.Sprintf("## 离线任务已提交\n> **数量**: %d 个\n> **目标目录**: %s\n> **时间**: %s", count, cid, now().Format("15:04:05"))
	if len(names) > 0 {
		msg += "\n> **资源**:"
		for _, n := range names {
			msg += "\n> - " + n
		}
	}
	return msg
}

// TaskFailed formats a message for a failed task.
func TaskFailed(name string, cid string) string {
	return fmt.Sprintf(`## 离线任务失败
> **任务**: %s
> **目标目录**: %s
> **时间**: %s`, name, cid, now().Format("15:04:05"))
}

// RSSFound formats a message when new items are found in RSS feed.
func RSSFound(subName string, count int, names []string) string {
	msg := fmt.Sprintf("## RSS 新资源\n> **订阅**: %s\n> **新增**: %d 条\n> **时间**: %s", subName, count, now().Format("15:04:05"))
	if len(names) > 0 {
		msg += "\n> **资源**:"
		for _, n := range names {
			msg += "\n> - " + n
		}
	}
	return msg
}

// ServerStarted formats a startup message.
func ServerStarted(port int) string {
	return fmt.Sprintf(`## pan-fetcher 已启动
> **端口**: %d
> **时间**: %s`, port, now().Format("2006-01-02 15:04:05"))
}

// SetLogEnabled enables or disables log push notifications.
func SetLogEnabled(enabled bool) {
	mu.Lock()
	defer mu.Unlock()
	logEnabled = enabled
}

// Logf enqueues a log line for push notification (1/s interval).
func Logf(format string, args ...interface{}) {
	mu.Lock()
	enabled := logEnabled
	mu.Unlock()
	if !enabled {
		return
	}
	msg := fmt.Sprintf(format, args...)
	content := fmt.Sprintf("## pan-fetcher 运行日志\n> %s\n> **时间**: %s", msg, now().Format("15:04:05"))
	select {
	case logQueue <- content:
	default:
		// queue full, drop oldest
		select {
		case <-logQueue:
		default:
		}
		logQueue <- content
	}
}
// Returns nil on success, error otherwise.
func Test(webhookURL string) error {
	msg := weworkMsg{
		MsgType: "markdown",
		Markdown: weworkMD{
			Content: fmt.Sprintf("## pan-fetcher 通知测试 ✅\n> 时间: %s\n> Webhook 配置成功", now().Format("15:04:05")),
		},
	}
	body, err := json.Marshal(&msg)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	resp, err := client.Post(webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
