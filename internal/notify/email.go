package notify

import (
	"context"
	"crypto/tls"
	"fmt"
	"mime"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

// EmailTLS modes.
const (
	TLSStartTLS = "StartTLS"
	TLSImplicit = "TLS"
	TLSNone     = "None"
)

// Email sends a plain-text mail over SMTP.
type Email struct {
	Host          string
	Port          int
	TLS           string
	SkipTLSVerify bool
	Username      string
	Password      string
	From          string
	To            []string
}

func (e *Email) Send(ctx context.Context, m *Message) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	addr := net.JoinHostPort(e.Host, strconv.Itoa(e.Port))
	tlsCfg := &tls.Config{ServerName: e.Host, InsecureSkipVerify: e.SkipTLSVerify} //nolint:gosec // opt-in

	var conn net.Conn
	var err error
	d := &net.Dialer{}
	if e.TLS == TLSImplicit {
		conn, err = (&tls.Dialer{NetDialer: d, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, e.Host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer func() { _ = c.Close() }()

	if e.TLS == TLSStartTLS || e.TLS == "" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("%s does not support STARTTLS (set tls: TLS or None)", addr)
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if e.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", e.Username, e.Password, e.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(e.From); err != nil {
		return err
	}
	for _, to := range e.To {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(e.compose(m)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func (e *Email) compose(m *Message) []byte {
	var b strings.Builder
	header := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	header("From", e.From)
	header("To", strings.Join(e.To, ", "))
	header("Subject", mime.QEncoding.Encode("utf-8", m.Title()))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("MIME-Version", "1.0")
	header("Content-Type", `text/plain; charset="utf-8"`)
	header("Content-Transfer-Encoding", "8bit")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(m.Text(), "\n", "\r\n"))
	b.WriteString("\r\n")
	return []byte(b.String())
}
