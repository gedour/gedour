package notify

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/http/httptest"
	"net/mail"
	"net/netip"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"
)

// sink is a minimal in-process SMTP server (EHLO, STARTTLS, AUTH PLAIN, MAIL, RCPT, DATA): no container
// for the Go tests, and the TLS modes stay under the test's control. Mailpit covers the real thing in e2e.
type sink struct {
	ln       net.Listener
	tls      *tls.Config
	implicit bool
	starttls bool

	mu    sync.Mutex
	auth  string
	from  string
	rcpts []string
	data  string
	tlsOn bool
}

// testTLS borrows httptest's certificate (valid for 127.0.0.1) and a client config trusting it.
func testTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	srv := httptest.NewTLSServer(nil)
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return &tls.Config{Certificates: srv.TLS.Certificates, MinVersion: tls.VersionTLS12},
		&tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
}

func newSink(t *testing.T, cfg *tls.Config, implicit, starttls bool) *sink {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &sink{ln: ln, tls: cfg, implicit: implicit, starttls: starttls}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *sink) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	if s.implicit {
		c = tls.Server(c, s.tls)
		s.mu.Lock()
		s.tlsOn = true
		s.mu.Unlock()
	}
	tp := textproto.NewConn(c)
	_ = tp.PrintfLine("220 sink ready")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		s.mu.Lock()
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			if s.starttls && !s.tlsOn {
				_ = tp.PrintfLine("250-sink")
				_ = tp.PrintfLine("250-STARTTLS")
			} else {
				_ = tp.PrintfLine("250-sink")
			}
			_ = tp.PrintfLine("250 AUTH PLAIN")
		case "STARTTLS":
			_ = tp.PrintfLine("220 go ahead")
			s.tlsOn = true
			s.mu.Unlock()
			c = tls.Server(c, s.tls)
			tp = textproto.NewConn(c)
			continue
		case "AUTH":
			b, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(arg, "PLAIN "))
			s.auth = string(b)
			_ = tp.PrintfLine("235 ok")
		case "MAIL":
			s.from = arg
			_ = tp.PrintfLine("250 ok")
		case "RCPT":
			s.rcpts = append(s.rcpts, arg)
			_ = tp.PrintfLine("250 ok")
		case "DATA":
			_ = tp.PrintfLine("354 go")
			s.mu.Unlock()
			b, _ := io.ReadAll(tp.DotReader())
			s.mu.Lock()
			s.data = string(b)
			_ = tp.PrintfLine("250 queued")
		case "QUIT":
			_ = tp.PrintfLine("221 bye")
			s.mu.Unlock()
			return
		default:
			_ = tp.PrintfLine("502 no")
		}
		s.mu.Unlock()
	}
}

var from = &mail.Address{Name: "gédour", Address: "gedour@example.test"}

var mailMsg = Message{Title: "o/r: v1.2.3 publié ✓", Summary: "v1.2.3 publié ✓", Detail: "<b>notes</b>", URL: "https://x.test/r?a=1&b=2"}

func TestMailerModes(t *testing.T) {
	srvTLS, cliTLS := testTLS(t)
	for _, tt := range []struct {
		mode               string
		implicit, starttls bool
		user               string
	}{
		{TLSNone, false, false, ""},
		{TLSStartTLS, false, true, "u"},
		{TLSImplicit, true, false, "u"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			s := newSink(t, srvTLS, tt.implicit, tt.starttls)
			m := &Mailer{Addr: s.ln.Addr().String(), From: from, TLS: tt.mode, TLSConfig: cliTLS}
			if tt.user != "" {
				m.Username, m.Password = tt.user, "pw"
			}
			if err := m.Send(context.Background(), []string{"a@x.test", "b@x.test"}, mailMsg, time.Now()); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.from != "FROM:<gedour@example.test>" || strings.Join(s.rcpts, ",") != "TO:<a@x.test>,TO:<b@x.test>" || s.data == "" {
				t.Fatalf("envelope: %q %q", s.from, s.rcpts)
			}
			if s.tlsOn != (tt.mode != TLSNone) || (tt.user != "" && s.auth != "\x00u\x00pw") {
				t.Fatalf("tls=%v auth=%q", s.tlsOn, s.auth)
			}
		})
	}
	t.Run("starttls not offered", func(t *testing.T) {
		s := newSink(t, srvTLS, false, false)
		m := &Mailer{Addr: s.ln.Addr().String(), From: from, TLS: TLSStartTLS, TLSConfig: cliTLS}
		if err := m.Send(context.Background(), []string{"a@x.test"}, mailMsg, time.Now()); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
			t.Fatalf("want a refusal, got %v", err)
		}
	})
	t.Run("untrusted certificate", func(t *testing.T) {
		s := newSink(t, srvTLS, true, false)
		m := &Mailer{Addr: s.ln.Addr().String(), From: from, TLS: TLSImplicit}
		if err := m.Send(context.Background(), []string{"a@x.test"}, mailMsg, time.Now()); err == nil {
			t.Fatal("want a certificate error")
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		m := &Mailer{Addr: "127.0.0.1:1", From: from, TLS: TLSNone}
		if err := m.Send(context.Background(), []string{"a@x.test"}, mailMsg, time.Now()); err == nil {
			t.Fatal("want a dial error")
		}
	})
}

// TLS none: in clear only to loopback or an allowlisted network.
func TestPlainAllowed(t *testing.T) {
	m := &Mailer{PlainNets: []netip.Prefix{netip.MustParsePrefix("10.20.0.0/16")}}
	for addr, want := range map[string]bool{"127.0.0.1:25": true, "[::1]:25": true, "10.20.1.2:25": true, "10.21.0.1:25": false, "203.0.113.5:25": false} {
		a, _ := net.ResolveTCPAddr("tcp", addr)
		if got := m.plainAllowed(a); got != want {
			t.Errorf("%s: got %v", addr, got)
		}
	}
}

// The message is multipart/alternative with an RFC 2047 subject, Date, Message-ID and quoted-printable parts.
func TestBuildMail(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	raw, err := BuildMail(from, []string{"a@x.test", "b@x.test"}, mailMsg, now)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	h := msg.Header
	subject, err := new(mime.WordDecoder).DecodeHeader(h.Get("Subject"))
	if err != nil || subject != mailMsg.Title || !strings.HasPrefix(h.Get("Subject"), "=?utf-8?q?") {
		t.Fatalf("subject %q -> %q %v", h.Get("Subject"), subject, err)
	}
	if d, err := h.Date(); err != nil || !d.Equal(now) {
		t.Fatalf("date: %v %v", d, err)
	}
	if !strings.HasSuffix(h.Get("Message-Id"), "@example.test>") || h.Get("To") != "a@x.test, b@x.test" ||
		h.Get("From") != "=?utf-8?q?g=C3=A9dour?= <gedour@example.test>" || h.Get("Auto-Submitted") != "auto-generated" {
		t.Fatalf("headers: %v", h)
	}
	mt, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("content type %q", h.Get("Content-Type"))
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	var parts []string
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if p.Header.Get("Content-Transfer-Encoding") != "quoted-printable" {
			t.Fatalf("encoding %q", p.Header.Get("Content-Transfer-Encoding"))
		}
		b, err := io.ReadAll(quotedprintable.NewReader(bufio.NewReader(p)))
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, p.Header.Get("Content-Type")+"\n"+string(b))
	}
	if len(parts) != 2 ||
		parts[0] != "text/plain; charset=utf-8\no/r: v1.2.3 publié ✓\r\n\r\n<b>notes</b>\r\n\r\nhttps://x.test/r?a=1&b=2\r\n" ||
		parts[1] != `text/html; charset=utf-8`+"\n"+`<p><strong>o/r: v1.2.3 publié ✓</strong></p><p>&lt;b&gt;notes&lt;/b&gt;</p><p><a href="https://x.test/r?a=1&amp;b=2">Open upstream</a></p>` {
		t.Fatalf("parts: %q", parts)
	}
}

func TestSenderEmail(t *testing.T) {
	s := newSink(t, nil, false, false)
	snd := newSender(t)
	snd.Mailer = &Mailer{Addr: s.ln.Addr().String(), From: from, TLS: TLSNone}
	if err := snd.Send(context.Background(), Channel{Type: TypeEmail, Config: []byte(`{"to":["a@x.test"]}`)}, sample); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !strings.Contains(s.data, "Subject: o/r: v1.2.3 published") {
		t.Fatalf("data: %s", s.data)
	}
	bad := &Mailer{Addr: "nohost", From: from, TLS: TLSNone}
	if err := bad.Send(context.Background(), []string{"a@x.test"}, mailMsg, time.Now()); err == nil {
		t.Fatal("want an address error")
	}
}
