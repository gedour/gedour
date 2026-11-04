package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Channel types.
const (
	TypeTelegram = "telegram"
	TypeEmail    = "email"
	TypeWebhook  = "webhook"
)

// Types lists the channel types in display order.
var Types = []string{TypeTelegram, TypeEmail, TypeWebhook}

// Webhook headers.
const (
	SignatureHeader = "X-Gedour-Signature"
	EventHeader     = "X-Gedour-Event"
	DeliveryHeader  = "X-Gedour-Delivery"
)

// SendTimeout bounds one HTTP delivery (webhook, Telegram).
const SendTimeout = 10 * time.Second

const (
	maxResponse    = 64 << 10
	maxTelegramLen = 4096
)

// TelegramConfig is the non-secret config of a telegram channel; the bot token is its secret.
type TelegramConfig struct {
	ChatID string `json:"chat_id"`
}

// EmailConfig is the config of an email channel; the SMTP server is instance config.
type EmailConfig struct {
	To []string `json:"to"`
}

// WebhookConfig is the config of a webhook channel; the HMAC key is its secret.
type WebhookConfig struct {
	URL string `json:"url"`
}

// Channel is a channel ready to send: its decrypted secret lives for one send only.
type Channel struct {
	ID     string
	Type   string
	Config json.RawMessage
	Secret string `json:"-"`
}

// Sender sends deliveries. Client must be a netguard client that follows no redirect.
type Sender struct {
	Client      *http.Client
	UserAgent   string
	TelegramURL string
	Mailer      *Mailer // nil: email is not configured
	Now         func() time.Time
}

func (s *Sender) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Send delivers d through ch. Errors never carry the secret, nor a URL that may embed it.
func (s *Sender) Send(ctx context.Context, ch Channel, d Delivery) error {
	msg := Format(d)
	switch ch.Type {
	case TypeTelegram:
		var c TelegramConfig
		if err := json.Unmarshal(ch.Config, &c); err != nil {
			return fmt.Errorf("telegram config: %w", err)
		}
		return s.telegram(ctx, c, ch.Secret, msg)
	case TypeEmail:
		var c EmailConfig
		if err := json.Unmarshal(ch.Config, &c); err != nil {
			return fmt.Errorf("email config: %w", err)
		}
		if s.Mailer == nil {
			return errors.New("email: no SMTP server configured")
		}
		return s.Mailer.Send(ctx, c.To, msg, s.now())
	case TypeWebhook:
		var c WebhookConfig
		if err := json.Unmarshal(ch.Config, &c); err != nil {
			return fmt.Errorf("webhook config: %w", err)
		}
		return s.webhook(ctx, c, ch.Secret, d)
	}
	return fmt.Errorf("unknown channel type %q", ch.Type)
}

// post sends body and returns the status and the first bytes of the answer. label names the
// destination in errors in place of the URL.
func (s *Sender) post(ctx context.Context, label, target string, body []byte, hdr http.Header) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("%s: invalid URL", label)
	}
	req.Header = hdr
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", s.UserAgent)
	resp, err := s.Client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, nil, fmt.Errorf("%s: %w", label, err)
	}
	defer func() { _ = resp.Body.Close() }()
	head, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	return resp.StatusCode, head, nil
}

func statusError(label string, code int, detail string) error {
	msg := fmt.Sprintf("%s: HTTP %d", label, code)
	if code >= 300 && code < 400 {
		msg += " (redirects are not followed)"
	}
	if detail != "" {
		msg += ": " + detail
	}
	return errors.New(msg)
}

// WebhookBody is the JSON a webhook receives.
type WebhookBody struct {
	DeliveryID int64     `json:"delivery_id"`
	Event      Event     `json:"event"`
	Watch      Watch     `json:"watch"`
	Rule       *Rule     `json:"rule"`
	SentAt     time.Time `json:"sent_at"`
}

// Sign is the X-Gedour-Signature value of body under key.
func Sign(key string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func (s *Sender) webhook(ctx context.Context, c WebhookConfig, key string, d Delivery) error {
	body, err := json.Marshal(WebhookBody{DeliveryID: d.ID, Event: d.Event, Watch: d.Watch, Rule: d.Rule, SentAt: s.now().UTC()})
	if err != nil {
		return fmt.Errorf("webhook body: %w", err)
	}
	label := "webhook"
	if u, err := url.Parse(c.URL); err == nil {
		label = "POST " + u.Host
	}
	hdr := http.Header{}
	hdr.Set(SignatureHeader, Sign(key, body))
	hdr.Set(EventHeader, d.Event.Type)
	hdr.Set(DeliveryHeader, strconv.FormatInt(d.ID, 10))
	code, _, err := s.post(ctx, label, c.URL, body, hdr)
	if err != nil {
		return err
	}
	if code < 200 || code > 299 {
		return statusError(label, code, "")
	}
	return nil
}

// TelegramText is the HTML-mode text of msg; every upstream string is escaped.
func TelegramText(msg Message) string {
	var b strings.Builder
	b.WriteString("<b>" + html.EscapeString(msg.Title) + "</b>")
	if msg.Detail != "" {
		b.WriteString("\n" + html.EscapeString(msg.Detail))
	}
	if msg.URL != "" {
		b.WriteString("\n<a href=\"" + html.EscapeString(msg.URL) + "\">Open upstream</a>")
	}
	if b.Len() <= maxTelegramLen {
		return b.String()
	}
	// Over Telegram's limit: the title alone, capped so that even fully escaped it fits.
	r := []rune(msg.Title)
	return "<b>" + html.EscapeString(string(r[:min(len(r), 600)])) + "</b>"
}

func (s *Sender) telegram(ctx context.Context, c TelegramConfig, token string, msg Message) error {
	body, err := json.Marshal(map[string]any{
		"chat_id": c.ChatID, "text": TelegramText(msg), "parse_mode": "HTML", "disable_web_page_preview": true,
	})
	if err != nil {
		return fmt.Errorf("telegram body: %w", err)
	}
	// The token is in the path: label stands in for the URL in every error.
	const label = "telegram"
	code, head, err := s.post(ctx, label, strings.TrimRight(s.TelegramURL, "/")+"/bot"+token+"/sendMessage", body, http.Header{})
	if err != nil {
		return err
	}
	if code < 200 || code > 299 {
		var answer struct {
			Description string `json:"description"`
		}
		_ = json.Unmarshal(head, &answer)
		return statusError(label, code, strings.ReplaceAll(answer.Description, token, "[redacted]"))
	}
	return nil
}
