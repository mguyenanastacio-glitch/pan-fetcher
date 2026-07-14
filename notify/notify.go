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
	client     = &http.Client{Timeout: 10 * time.Second}
)

// SetWebhook configures the WeChat Work webhook URL.
func SetWebhook(url string) {
	mu.Lock()
	defer mu.Unlock()
	webhookURL = url
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
> **时间**: %s`, count, cid, time.Now().Format("15:04:05"))
}

// TaskFailed formats a message for a failed task.
func TaskFailed(name string, cid string) string {
	return fmt.Sprintf(`## 离线任务失败
> **任务**: %s
> **目标目录**: %s
> **时间**: %s`, name, cid, time.Now().Format("15:04:05"))
}

// RSSFound formats a message when new items are found in RSS feed.
func RSSFound(subName string, count int) string {
	return fmt.Sprintf(`## RSS 新资源
> **订阅**: %s
> **新增**: %d 条
> **时间**: %s`, subName, count, time.Now().Format("15:04:05"))
}

// ServerStarted formats a startup message.
func ServerStarted(port int) string {
	return fmt.Sprintf(`## pan-fetcher 已启动
> **端口**: %d
> **时间**: %s`, port, time.Now().Format("2006-01-02 15:04:05"))
}

// Test sends a test message to the given webhook synchronously.
// Returns nil on success, error otherwise.
func Test(webhookURL string) error {
	msg := weworkMsg{
		MsgType: "markdown",
		Markdown: weworkMD{
			Content: fmt.Sprintf("## pan-fetcher 通知测试 ✅\n> 时间: %s\n> Webhook 配置成功", time.Now().Format("15:04:05")),
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
