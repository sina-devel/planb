// Package notify delivers backup and restore notifications to webhooks, Slack, Telegram and email.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Message describes a finished Backup or Restore. It is also the JSON body sent to generic webhooks.
type Message struct {
	// Event is BackupCompleted, BackupFailed, RestoreCompleted, RestoreFailed or Test.
	Event     string `json:"event"`
	Cluster   string `json:"cluster,omitempty"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Phase     string `json:"phase"`
	Message   string `json:"message,omitempty"`
	// Source is what was backed up, e.g. "postgres · db.svc / app".
	Source string `json:"source,omitempty"`
	// Backup is the backup a Restore restored from.
	Backup          string     `json:"backup,omitempty"`
	Schedule        string     `json:"schedule,omitempty"`
	StorageLocation string     `json:"storageLocation,omitempty"`
	SizeBytes       int64      `json:"sizeBytes,omitempty"`
	Items           int64      `json:"items,omitempty"`
	StartTime       *time.Time `json:"startTime,omitempty"`
	CompletionTime  *time.Time `json:"completionTime,omitempty"`
}

// Failed reports whether the message is about a failure.
func (m *Message) Failed() bool { return strings.HasSuffix(m.Event, "Failed") }

// Title is a one-line summary.
func (m *Message) Title() string {
	if m.Event == "Test" {
		return "planb test notification"
	}
	icon, verb := "✅", "completed"
	if m.Failed() {
		icon, verb = "❌", "failed"
	}
	t := fmt.Sprintf("%s %s %s/%s %s", icon, m.Kind, m.Namespace, m.Name, verb)
	if m.Cluster != "" {
		t += " on " + m.Cluster
	}
	return t
}

// Fields are the details shown below the title, in order.
func (m *Message) Fields() [][2]string {
	var f [][2]string
	add := func(k, v string) {
		if v != "" {
			f = append(f, [2]string{k, v})
		}
	}
	if m.Event == "Test" {
		add("Notifier", m.Namespace+"/"+m.Name)
		add("Cluster", m.Cluster)
		add("Message", "If you can read this, notifications work.")
		return f
	}
	add("Source", m.Source)
	add("Backup", m.Backup)
	add("Schedule", m.Schedule)
	add("Storage location", m.StorageLocation)
	if m.SizeBytes > 0 {
		add("Size", fmt.Sprintf("%s (%d items)", humanBytes(m.SizeBytes), m.Items))
	}
	if m.StartTime != nil && m.CompletionTime != nil {
		add("Duration", m.CompletionTime.Sub(*m.StartTime).Round(time.Second).String())
	}
	if m.CompletionTime != nil {
		add("Finished", m.CompletionTime.UTC().Format(time.RFC3339))
	}
	if m.Failed() {
		add("Error", m.Message)
	}
	return f
}

// Text is the title followed by the fields as plain text.
func (m *Message) Text() string {
	var b strings.Builder
	b.WriteString(m.Title())
	for _, f := range m.Fields() {
		fmt.Fprintf(&b, "\n%s: %s", f[0], f[1])
	}
	return b.String()
}

func humanBytes(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	v, i := float64(n), 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// Sender delivers a message to one channel.
type Sender interface {
	Send(ctx context.Context, m *Message) error
}

// httpClient honours HTTP(S)_PROXY, so channels that are only reachable through a proxy work.
var httpClient = &http.Client{Timeout: 20 * time.Second}

func postJSON(ctx context.Context, url string, headers map[string]string, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "planb")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, redactURL(err, url)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode/100 != 2 {
		return respBody, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return respBody, nil
}

// redactURL keeps secrets embedded in URLs (webhook tokens, bot tokens) out of error messages and status.
func redactURL(err error, url string) error {
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), url, "<redacted url>"))
}

// Webhook POSTs the Message as JSON.
type Webhook struct {
	URL     string
	Headers map[string]string
}

func (w *Webhook) Send(ctx context.Context, m *Message) error {
	_, err := postJSON(ctx, w.URL, w.Headers, m)
	return err
}

// Slack posts to an incoming webhook (also Mattermost and Rocket.Chat).
type Slack struct {
	WebhookURL string
	Channel    string
}

func (s *Slack) Send(ctx context.Context, m *Message) error {
	color := "good"
	if m.Failed() {
		color = "danger"
	}
	fields := []map[string]any{}
	for _, f := range m.Fields() {
		fields = append(fields, map[string]any{"title": f[0], "value": f[1], "short": f[0] != "Error" && f[0] != "Source"})
	}
	body := map[string]any{
		"text":        m.Title(),
		"attachments": []map[string]any{{"color": color, "fields": fields, "footer": "planb"}},
	}
	if s.Channel != "" {
		body["channel"] = s.Channel
	}
	_, err := postJSON(ctx, s.WebhookURL, nil, body)
	return err
}

// Telegram sends a message through the Bot API.
type Telegram struct {
	APIURL   string
	BotToken string
	ChatID   string
}

func (t *Telegram) Send(ctx context.Context, m *Message) error {
	var b strings.Builder
	fmt.Fprintf(&b, "<b>%s</b>", htmlEscape(m.Title()))
	for _, f := range m.Fields() {
		fmt.Fprintf(&b, "\n<b>%s:</b> ", htmlEscape(f[0]))
		if f[0] == "Error" {
			fmt.Fprintf(&b, "<code>%s</code>", htmlEscape(f[1]))
		} else {
			b.WriteString(htmlEscape(f[1]))
		}
	}
	api := strings.TrimSuffix(t.APIURL, "/")
	if api == "" {
		api = "https://api.telegram.org"
	}
	_, err := postJSON(ctx, api+"/bot"+t.BotToken+"/sendMessage", nil, map[string]any{
		"chat_id": t.ChatID, "text": b.String(), "parse_mode": "HTML", "disable_web_page_preview": true,
	})
	if err != nil {
		return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), t.BotToken, "<redacted>"))
	}
	return nil
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
