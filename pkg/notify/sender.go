package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
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
	"unicode/utf8"
)

// Channel types.
const (
	TypeTelegram      = "telegram"
	TypeNextcloudTalk = "nextcloud_talk"
	TypeEmail         = "email"
	TypeWebhook       = "webhook"
	// TypeScript channels run a script in the sandbox (pkg/script), never through Send.
	TypeScript = "script"
)

// Types lists the channel types in display order.
var Types = []string{TypeTelegram, TypeNextcloudTalk, TypeEmail, TypeWebhook, TypeScript}

// Webhook headers.
const (
	SignatureHeader = "X-Gedour-Signature"
	EventHeader     = "X-Gedour-Event"
	DeliveryHeader  = "X-Gedour-Delivery"
)

// Nextcloud Talk bot headers.
const (
	TalkRandomHeader    = "X-Nextcloud-Talk-Bot-Random"
	TalkSignatureHeader = "X-Nextcloud-Talk-Bot-Signature"
)

// SendTimeout bounds one HTTP delivery (webhook, Telegram, Nextcloud Talk).
const SendTimeout = 10 * time.Second

const (
	maxResponse    = 64 << 10
	maxTelegramLen = 4096
	// Talk refuses over 32000 characters (413); the margin leaves room for a lower server setting.
	maxTalkLen = 31000
	// The longest encoded URL kept: it leaves a cut title close to 1000 characters.
	maxTalkLink = 30000
)

// ErrPermanent marks a refusal a retry cannot fix: the delivery is dead at once.
var ErrPermanent = errors.New("permanent failure")

type permanentError struct{ error }

func (permanentError) Is(target error) bool { return target == ErrPermanent }

// TelegramConfig is the non-secret config of a telegram channel; the bot token is its secret.
type TelegramConfig struct {
	ChatID string `json:"chat_id"`
	Silent bool   `json:"silent"`
}

// NextcloudTalkConfig is the config of a Nextcloud Talk channel; the bot's shared secret is its secret.
type NextcloudTalkConfig struct {
	URL          string `json:"url"`
	Conversation string `json:"conversation"`
	Silent       bool   `json:"silent"`
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
	case TypeNextcloudTalk:
		var c NextcloudTalkConfig
		if err := json.Unmarshal(ch.Config, &c); err != nil {
			return fmt.Errorf("nextcloud talk config: %w", err)
		}
		return s.talk(ctx, c, ch.Secret, d.ID, msg)
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

// postLabel names a destination by its host only: paths and queries stay out of errors.
func postLabel(fallback, target string) string {
	if u, err := url.Parse(target); err == nil {
		return "POST " + u.Host
	}
	return fallback
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
	label := postLabel("webhook", c.URL)
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
		"disable_notification": c.Silent,
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

// Upstream text cannot add inline Markdown links, emphasis or code, nor mention anyone: Talk's mention
// pattern refuses a backslash before the @. Block Markdown and bare URLs stay as they are.
var talkEscaper = strings.NewReplacer(`\`, `\\`, `*`, `\*`, `_`, `\_`, "`", "\\`", `[`, `\[`, `]`, `\]`, `~`, `\~`, `<`, `\<`, `>`, `\>`, `@`, `\@`)

// talkLink percent-encodes what could close the Markdown link, break its line or mention anyone.
func talkLink(u string) string {
	var b strings.Builder
	for i := range len(u) {
		c := u[i]
		if c < 0x20 || c == 0x7f || strings.IndexByte(` ()<>\@`, c) >= 0 {
			fmt.Fprintf(&b, "%%%02X", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// talkTitle is the bold title in at most n characters of Markdown, never cut inside an escape sequence.
func talkTitle(title string, n int) string {
	var b strings.Builder
	used := len("****")
	for _, r := range title {
		e := talkEscaper.Replace(string(r))
		if used += utf8.RuneCountInString(e); used > n {
			break
		}
		b.WriteString(e)
	}
	return "**" + b.String() + "**"
}

// TalkText is the Markdown text of msg. A URL over maxTalkLink is always left out. Over the cap the text
// falls back to the title and link, cutting the title to keep the link: Talk never answers 413.
func TalkText(msg Message) string {
	title := "**" + talkEscaper.Replace(msg.Title) + "**"
	var detail, link string
	if msg.Detail != "" {
		detail = "\n" + talkEscaper.Replace(msg.Detail)
	}
	if u := talkLink(msg.URL); u != "" && utf8.RuneCountInString(u) <= maxTalkLink {
		link = "\n[Open upstream](" + u + ")"
	}
	if text := title + detail + link; utf8.RuneCountInString(text) <= maxTalkLen {
		return text
	}
	if text := title + link; utf8.RuneCountInString(text) <= maxTalkLen {
		return text
	}
	return talkTitle(msg.Title, maxTalkLen-utf8.RuneCountInString(link)) + link
}

// TalkSignature is hex(HMAC-SHA256(secret, random + message)): Talk checks it over the decoded message.
func TalkSignature(secret, random, message string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(random + message))
	return hex.EncodeToString(mac.Sum(nil))
}

func (s *Sender) talk(ctx context.Context, c NextcloudTalkConfig, secret string, deliveryID int64, msg Message) error {
	text := TalkText(msg)
	reference := sha256.Sum256([]byte(strconv.FormatInt(deliveryID, 10)))
	body, err := json.Marshal(map[string]any{"message": text, "referenceId": hex.EncodeToString(reference[:]), "silent": c.Silent})
	if err != nil {
		return fmt.Errorf("nextcloud talk body: %w", err)
	}
	random := make([]byte, 32)
	_, _ = rand.Read(random) // crypto/rand.Read never fails (Go 1.24+)
	nonce := hex.EncodeToString(random)
	hdr := http.Header{}
	hdr.Set("OCS-APIRequest", "true")
	hdr.Set("Accept", "application/json")
	hdr.Set(TalkRandomHeader, nonce)
	hdr.Set(TalkSignatureHeader, TalkSignature(secret, nonce, text))
	target := strings.TrimRight(c.URL, "/") + "/ocs/v2.php/apps/spreed/api/v1/bot/" + url.PathEscape(c.Conversation) + "/message"
	label := postLabel("nextcloud talk", c.URL)
	code, _, err := s.post(ctx, label, target, body, hdr)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusCreated:
		return nil // a proxy or SSO page can answer another 2xx: only Talk's 201 proves the post
	// Not 400: gedour's request cannot fail Talk's validation, what is left is a caught server exception.
	case http.StatusUnauthorized, http.StatusNotFound, http.StatusRequestEntityTooLarge:
		return permanentError{statusError(label, code, "")}
	}
	return statusError(label, code, "")
}
