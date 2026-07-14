// Package notify provides notification sending for various channels.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

var (
	mu         sync.Mutex
	webhookURL string
	tzLoc      *time.Location
	logEnabled bool
	logQueue   = make(chan string, 200)
	client     = &http.Client{Timeout: 10 * time.Second}
)

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
func TaskSubmitted(count int, cid string) string {
	return fmt.Sprintf(`## 离线任务已提交
> **数量**: %d 个
> **目标目录**: %s
> **时间**: %s`, count, cid, now().Format("15:04:05"))
}

// TaskFailed formats a message for a failed task.
func TaskFailed(name string, cid string) string {
	return fmt.Sprintf(`## 离线任务失败
> **任务**: %s
> **目标目录**: %s
> **时间**: %s`, name, cid, now().Format("15:04:05"))
}

// RSSFound formats a message when new items are found in RSS feed.
func RSSFound(subName string, count int) string {
	return fmt.Sprintf(`## RSS 新资源
> **订阅**: %s
> **新增**: %d 条
> **时间**: %s`, subName, count, now().Format("15:04:05"))
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
