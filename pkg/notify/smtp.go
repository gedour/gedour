package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/netip"
	"net/smtp"
	"net/textproto"
	"slices"
	"strings"
	"time"
)

// SMTP TLS modes.
const (
	TLSStartTLS = "starttls"
	TLSImplicit = "tls"
	TLSNone     = "none"
)

// MaxRecipients caps the addresses of one email channel.
const MaxRecipients = 10

const smtpTimeout = 30 * time.Second

// Mailer sends email through the instance's SMTP server.
type Mailer struct {
	Addr     string
	Username string
	Password string `json:"-"`
	From     *mail.Address
	TLS      string
	// PlainNets may be reached with TLS "none" besides loopback (GEDOUR_SMTP_PLAIN_NETS).
	PlainNets []netip.Prefix
	// TLSConfig overrides the client TLS settings (tests trust their own CA through it).
	TLSConfig *tls.Config
}

func (m *Mailer) tlsConfig(host string) *tls.Config {
	if m.TLSConfig != nil {
		return m.TLSConfig.Clone()
	}
	return &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
}

// plainAllowed: credentials and mail in clear only to this machine or a network the admin opened.
func (m *Mailer) plainAllowed(addr net.Addr) bool {
	ap, err := netip.ParseAddrPort(addr.String())
	if err != nil {
		return false
	}
	ip := ap.Addr().Unmap()
	return ip.IsLoopback() || slices.ContainsFunc(m.PlainNets, func(p netip.Prefix) bool { return p.Contains(ip) })
}

// Send mails msg to every address of to.
func (m *Mailer) Send(ctx context.Context, to []string, msg Message, now time.Time) error {
	body, err := BuildMail(m.From, to, msg, now)
	if err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(m.Addr)
	if err != nil {
		return fmt.Errorf("smtp: address %q: %w", m.Addr, err)
	}
	ctx, cancel := context.WithTimeout(ctx, smtpTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", m.Addr)
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(deadline)
	switch m.TLS {
	case TLSNone:
		if !m.plainAllowed(conn.RemoteAddr()) {
			return fmt.Errorf("smtp: TLS none is only allowed to loopback or GEDOUR_SMTP_PLAIN_NETS, not %s", conn.RemoteAddr())
		}
	case TLSImplicit:
		tc := tls.Client(conn, m.tlsConfig(host))
		if err := tc.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("smtp: tls: %w", err)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	defer func() { _ = c.Close() }()
	if m.TLS == TLSStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("smtp: the server does not offer STARTTLS")
		}
		if err := c.StartTLS(m.tlsConfig(host)); err != nil {
			return fmt.Errorf("smtp: starttls: %w", err)
		}
	}
	if m.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", m.Username, m.Password, host)); err != nil {
			return fmt.Errorf("smtp: auth: %w", err)
		}
	}
	if err := c.Mail(m.From.Address); err != nil {
		return fmt.Errorf("smtp: MAIL FROM: %w", err)
	}
	for _, rcpt := range to {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp: RCPT TO %s: %w", rcpt, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: DATA: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("smtp: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: end of data: %w", err)
	}
	return c.Quit()
}

// BuildMail renders msg as a multipart/alternative (text + HTML) message with encoded headers.
func BuildMail(from *mail.Address, to []string, msg Message, now time.Time) ([]byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	idBytes := make([]byte, 12)
	_, _ = rand.Read(idBytes)
	domain := from.Address[strings.LastIndexByte(from.Address, '@')+1:]
	hdr := []string{
		"From: " + from.String(),
		"To: " + strings.Join(to, ", "),
		"Subject: " + mime.QEncoding.Encode("utf-8", msg.Title),
		"Date: " + now.Format(time.RFC1123Z),
		"Message-ID: <" + hex.EncodeToString(idBytes) + "@" + domain + ">",
		"MIME-Version: 1.0",
		// Notifications: autoresponders must not answer them.
		"Auto-Submitted: auto-generated",
		"Content-Type: multipart/alternative; boundary=" + mw.Boundary(),
	}
	var out bytes.Buffer
	out.WriteString(strings.Join(hdr, "\r\n") + "\r\n\r\n")

	text := msg.Title + "\r\n"
	if msg.Detail != "" {
		text += "\r\n" + msg.Detail + "\r\n"
	}
	if msg.URL != "" {
		text += "\r\n" + msg.URL + "\r\n"
	}
	if msg.Gedour != "" {
		text += "\r\nOpen in gedour: " + msg.Gedour + "\r\n"
	}
	page := "<p><strong>" + html.EscapeString(msg.Title) + "</strong></p>"
	if msg.Detail != "" {
		// A digest's Detail holds one line per event.
		page += "<p>" + strings.ReplaceAll(html.EscapeString(msg.Detail), "\r\n", "<br>") + "</p>"
	}
	if msg.URL != "" {
		page += `<p><a href="` + html.EscapeString(msg.URL) + `">Open upstream</a></p>`
	}
	if msg.Gedour != "" {
		page += `<p><a href="` + html.EscapeString(msg.Gedour) + `">Open in gedour</a></p>`
	}
	for _, part := range []struct{ typ, body string }{{"text/plain", text}, {"text/html", page}} {
		pw, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.typ + "; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, fmt.Errorf("mail part: %w", err)
		}
		qp := quotedprintable.NewWriter(pw)
		if _, err := qp.Write([]byte(part.body)); err != nil {
			return nil, fmt.Errorf("mail part: %w", err)
		}
		if err := qp.Close(); err != nil {
			return nil, fmt.Errorf("mail part: %w", err)
		}
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("mail: %w", err)
	}
	out.Write(buf.Bytes())
	return out.Bytes(), nil
}
