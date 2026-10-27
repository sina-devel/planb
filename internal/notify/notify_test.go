package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testMessage() *Message {
	start := time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC)
	end := start.Add(95 * time.Second)
	return &Message{
		Event: "BackupFailed", Cluster: "prod", Kind: "Backup", Namespace: "db", Name: "nightly-1",
		Phase: "Failed", Message: "pg_dump: <connection refused>", Source: "postgres · pg / app",
		Schedule: "nightly", StorageLocation: "default", StartTime: &start, CompletionTime: &end,
	}
}

// capture starts a server that records the last request body.
func capture(t *testing.T, status int) (*httptest.Server, *[]byte, *http.Header) {
	var body []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		hdr = r.Header
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &body, &hdr
}

func TestMessageText(t *testing.T) {
	m := testMessage()
	want := "❌ Backup db/nightly-1 failed on prod"
	if m.Title() != want {
		t.Fatalf("title %q", m.Title())
	}
	for _, s := range []string{"Duration: 1m35s", "Schedule: nightly", "Error: pg_dump"} {
		if !strings.Contains(m.Text(), s) {
			t.Fatalf("text misses %q:\n%s", s, m.Text())
		}
	}
}

func TestWebhook(t *testing.T) {
	srv, body, hdr := capture(t, 200)
	w := &Webhook{URL: srv.URL, Headers: map[string]string{"Authorization": "Bearer x"}}
	if err := w.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	got := &Message{}
	if err := json.Unmarshal(*body, got); err != nil || got.Name != "nightly-1" || got.Event != "BackupFailed" {
		t.Fatalf("body %s (%v)", *body, err)
	}
	if hdr.Get("Authorization") != "Bearer x" {
		t.Fatal("header not sent")
	}
}

func TestWebhookErrorRedactsURL(t *testing.T) {
	w := &Webhook{URL: "http://127.0.0.1:1/secret-token"}
	err := w.Send(context.Background(), testMessage())
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("expected redacted error, got %v", err)
	}
}

func TestSlack(t *testing.T) {
	srv, body, _ := capture(t, 200)
	if err := (&Slack{WebhookURL: srv.URL, Channel: "#ops"}).Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Text        string
		Channel     string
		Attachments []struct{ Color string }
	}
	if err := json.Unmarshal(*body, &got); err != nil || got.Channel != "#ops" || got.Attachments[0].Color != "danger" {
		t.Fatalf("body %s", *body)
	}
}

func TestTelegram(t *testing.T) {
	var path string
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, body = r.URL.Path, must(io.ReadAll(r.Body))
		if strings.Contains(path, "badtoken") {
			http.Error(w, `{"ok":false,"description":"Unauthorized"}`, http.StatusUnauthorized)
		}
	}))
	defer srv.Close()
	tg := &Telegram{APIURL: srv.URL + "/", BotToken: "123:abc", ChatID: "-100"}
	if err := tg.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if path != "/bot123:abc/sendMessage" || got["chat_id"] != "-100" || got["parse_mode"] != "HTML" ||
		!strings.Contains(got["text"].(string), "&lt;connection refused&gt;") {
		t.Fatalf("path %s body %s", path, body)
	}
	tg.BotToken = "badtoken"
	if err := tg.Send(context.Background(), testMessage()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401, got %v", err)
	}
}

func TestEmail(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	got := make(chan string, 1)
	go fakeSMTP(ln, got)

	port, _ := strconv.Atoi(strings.Split(ln.Addr().String(), ":")[1])
	e := &Email{Host: "127.0.0.1", Port: port, TLS: TLSNone, From: "planb@example.com", To: []string{"a@example.com", "b@example.com"}}
	if err := e.Send(context.Background(), testMessage()); err != nil {
		t.Fatal(err)
	}
	mail := <-got
	for _, s := range []string{"RCPT TO:<a@example.com>", "RCPT TO:<b@example.com>", "Subject: =?utf-8?q?", "Error: pg_dump"} {
		if !strings.Contains(mail, s) {
			t.Fatalf("mail misses %q:\n%s", s, mail)
		}
	}

	// StartTLS is required by default; a server without it is refused rather than used in plain text.
	go fakeSMTP(ln, got)
	e.TLS = TLSStartTLS
	if err := e.Send(context.Background(), testMessage()); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("expected STARTTLS error, got %v", err)
	}
}

// fakeSMTP accepts one session without TLS or auth and sends the transcript.
func fakeSMTP(ln net.Listener, out chan<- string) {
	c, err := ln.Accept()
	if err != nil {
		return
	}
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	var log strings.Builder
	say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
	say("220 fake")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			out <- log.String()
			return
		}
		log.WriteString(line)
		switch cmd := strings.ToUpper(strings.TrimSpace(line)); {
		case strings.HasPrefix(cmd, "EHLO"):
			say("250-fake")
			say("250 8BITMIME")
		case strings.HasPrefix(cmd, "DATA"):
			say("354 go")
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				log.WriteString(l)
			}
			say("250 queued")
		case strings.HasPrefix(cmd, "QUIT"):
			say("221 bye")
			out <- log.String()
			return
		default:
			say("250 ok")
		}
	}
}

func must[T any](v T, _ error) T { return v }

func TestSuccessHasNoErrorField(t *testing.T) {
	m := testMessage()
	m.Event, m.Message = "RestoreCompleted", "restore completed: 1 items"
	if strings.Contains(m.Text(), "Error") {
		t.Fatalf("success must not show an error:\n%s", m.Text())
	}
}
