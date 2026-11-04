package route

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/db"
	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/notify"
	"github.com/gedour/gedour/pkg/secret"
)

// Per-user caps: what one user creates bounds the router's and the sender's work for them.
const (
	MaxChannels = 50
	MaxRules    = 100
)

// underCap refuses a create past max rows of table for owner. The user row lock serializes that
// user's creates, so two at once cannot both pass at max-1.
func underCap(ctx context.Context, tx pgx.Tx, owner, table string, limit int) error {
	if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`, owner); err != nil {
		return err
	}
	var n int
	// table is one of the cap constants' tables, never input.
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+table+` WHERE owner_id = $1`, owner).Scan(&n); err != nil {
		return err
	}
	if n >= limit {
		return huma.Error409Conflict(fmt.Sprintf("you have %d %s, the most one user can have: delete one first", n, table))
	}
	return nil
}

// TestInterval is the minimum gap between two channel tests of one user.
const TestInterval = 10 * time.Second

// Channel is a channel as the API returns it. It has no field for the secret, so neither the API nor
// the audit log (which records this type) can carry it.
type Channel struct {
	ID                 string         `json:"id" format:"uuid"`
	Name               string         `json:"name"`
	Type               string         `json:"type" enum:"telegram,email,webhook,script"`
	Config             map[string]any `json:"config" doc:"telegram: chat_id; email: to; webhook: url; script: script_id"`
	Last4              string         `json:"last4" doc:"Last 4 characters of the secret (bot token, signing key); empty for short ones or none"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
	LastDeliveryStatus *string        `json:"last_delivery_status" enum:"pending,sent,failed,dead" nullable:"true" doc:"Status of the newest delivery"`
	LastDeliveryAt     *time.Time     `json:"last_delivery_at" nullable:"true"`
}

type channelOutput struct {
	Body Channel
}

type createdChannel struct {
	Channel
	SigningSecret string `json:"signing_secret,omitempty" doc:"Webhook only, generated when none was given: shown in this response only"`
}

type createChannelOutput struct {
	Body createdChannel
}

type listChannelsOutput struct {
	Body struct {
		Items []Channel `json:"items" doc:"Sorted by name"`
	}
}

type createChannelInput struct {
	Body struct {
		Name   string         `json:"name" minLength:"1" maxLength:"200"`
		Type   string         `json:"type" enum:"telegram,email,webhook,script"`
		Config map[string]any `json:"config"`
		Secret string         `json:"secret,omitempty" maxLength:"512" doc:"Write-only. telegram: bot token (required); webhook: HMAC key, generated when empty; email, script: none"`
	}
}

type updateChannelInput struct {
	ID   string `path:"id" format:"uuid"`
	Body struct {
		Name   *string        `json:"name,omitempty" minLength:"1" maxLength:"200"`
		Config map[string]any `json:"config,omitempty" doc:"Replaces the whole config"`
		Secret *string        `json:"secret,omitempty" maxLength:"512" doc:"Write-only; replaces the secret"`
	}
}

type channelTypesOutput struct {
	Body struct {
		Items []ChannelType `json:"items"`
	}
}

// ChannelType tells the UI which channel types this instance can send to.
type ChannelType struct {
	Type      string `json:"type" enum:"telegram,email,webhook,script"`
	Available bool   `json:"available"`
	Hint      string `json:"hint,omitempty" doc:"Why it is unavailable"`
}

type testResult struct {
	Body struct {
		OK    bool     `json:"ok"`
		Error string   `json:"error,omitempty" doc:"Why the test message was not sent; for a script, why the run failed"`
		Logs  []string `json:"logs,omitempty" doc:"Script channels: the log lines of the run (a live run: its fetches were sent)"`
	}
}

const emailUnavailable = "email needs an SMTP server: set GEDOUR_SMTP_ADDR and GEDOUR_SMTP_FROM"

const channelSelect = `SELECT c.id::text, c.name, c.type, c.config, c.last4, c.created_at, c.updated_at, d.status, d.created_at
	FROM channels c LEFT JOIN LATERAL (SELECT status, created_at FROM deliveries WHERE channel_id = c.id ORDER BY id DESC LIMIT 1) d ON true
	WHERE c.owner_id = @owner`

func scanChannels(rows pgx.Rows) ([]Channel, error) {
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Channel, error) {
		var c Channel
		err := r.Scan(&c.ID, &c.Name, &c.Type, &c.Config, &c.Last4, &c.CreatedAt, &c.UpdatedAt, &c.LastDeliveryStatus, &c.LastDeliveryAt)
		return c, err
	})
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ownChannel loads one of owner's channels; anyone else's is a 404, like a missing one.
func ownChannel(ctx context.Context, q querier, owner, id string, lock bool) (Channel, error) {
	if uuid.Validate(id) != nil {
		return Channel{}, huma.Error404NotFound("channel not found")
	}
	if lock {
		rows, err := q.Query(ctx, `SELECT 1 FROM channels WHERE id = $1 AND owner_id = $2 FOR UPDATE`, id, owner)
		if err != nil {
			return Channel{}, err
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return Channel{}, err
		}
	}
	rows, err := q.Query(ctx, channelSelect+" AND c.id = @id", pgx.NamedArgs{"owner": owner, "id": id})
	if err != nil {
		return Channel{}, err
	}
	cs, err := scanChannels(rows)
	if err != nil {
		return Channel{}, err
	}
	if len(cs) == 0 {
		return Channel{}, huma.Error404NotFound("channel not found")
	}
	return cs[0], nil
}

// last4 shows a hint of long secrets only, as for credentials.
func last4(s string) string {
	if utf8.RuneCountInString(s) < 16 {
		return ""
	}
	r := []rune(s)
	return string(r[len(r)-4:])
}

func configError(field, msg string, value any) error {
	return &huma.ErrorDetail{Location: "body.config." + field, Message: msg, Value: value}
}

// decodeConfig reads raw into dst, refusing unknown keys so a typo is not silently dropped.
func decodeConfig(raw map[string]any, dst any) error {
	b, err := json.Marshal(raw)
	if err != nil {
		return &huma.ErrorDetail{Location: "body.config", Message: "not a JSON object"}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &huma.ErrorDetail{Location: "body.config", Message: strings.TrimPrefix(err.Error(), "json: ")}
	}
	return nil
}

// A dot-atom local part: one that needs quoting ("a b"@x) is refused, as few servers handle it the same.
var localPartRe = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+(\\.[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+)*$")

func plainAddress(a string) (string, bool) {
	addr, err := mail.ParseAddress(strings.TrimSpace(a))
	if err != nil {
		return "", false
	}
	at := strings.LastIndexByte(addr.Address, '@')
	if at <= 0 || at == len(addr.Address)-1 || !localPartRe.MatchString(addr.Address[:at]) {
		return "", false
	}
	return addr.Address, true
}

var chatIDRe = regexp.MustCompile(`^(-?[0-9]{1,20}|@[A-Za-z][A-Za-z0-9_]{3,31})$`)

// The token goes into the request path: only its documented alphabet may reach the URL.
var botTokenRe = regexp.MustCompile(`^[0-9]{1,20}:[A-Za-z0-9_-]{20,200}$`)

// validateConfig normalizes a channel config; it returns field errors, never a server error.
func (s *Service) validateConfig(typ string, raw map[string]any) (map[string]any, []error) {
	var norm any
	var errs []error
	switch typ {
	case notify.TypeTelegram:
		var c notify.TelegramConfig
		if err := decodeConfig(raw, &c); err != nil {
			return nil, []error{err}
		}
		if c.ChatID = strings.TrimSpace(c.ChatID); !chatIDRe.MatchString(c.ChatID) {
			errs = append(errs, configError("chat_id", "a numeric chat id (e.g. -1001234567890) or @channelname", c.ChatID))
		}
		norm = c
	case notify.TypeEmail:
		var c notify.EmailConfig
		if err := decodeConfig(raw, &c); err != nil {
			return nil, []error{err}
		}
		var to []string
		for i, a := range c.To {
			addr, ok := plainAddress(a)
			if !ok {
				errs = append(errs, configError("to["+strconv.Itoa(i)+"]", "not a plain email address (no quoted local part)", a))
				continue
			}
			if !slices.Contains(to, addr) {
				to = append(to, addr)
			}
		}
		switch {
		case len(c.To) == 0:
			errs = append(errs, configError("to", "at least one recipient", nil))
		case len(to) > notify.MaxRecipients:
			errs = append(errs, configError("to", fmt.Sprintf("at most %d recipients", notify.MaxRecipients), len(to)))
		}
		norm = notify.EmailConfig{To: to}
	case notify.TypeWebhook:
		var c notify.WebhookConfig
		if err := decodeConfig(raw, &c); err != nil {
			return nil, []error{err}
		}
		c.URL = strings.TrimSpace(c.URL)
		if msg := s.checkWebhookURL(c.URL); msg != "" {
			errs = append(errs, configError("url", msg, c.URL))
		}
		norm = c
	case notify.TypeScript:
		var c ScriptConfig
		if err := decodeConfig(raw, &c); err != nil {
			return nil, []error{err}
		}
		// Canonical form: the in-use check matches config->>'script_id' as text.
		if id, err := uuid.Parse(c.ScriptID); err != nil {
			errs = append(errs, configError("script_id", "pick one of your scripts", c.ScriptID))
		} else {
			c.ScriptID = id.String()
		}
		norm = c
	default:
		return nil, []error{&huma.ErrorDetail{Location: "body.type", Message: "unknown channel type", Value: typ}}
	}
	b, _ := json.Marshal(norm)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out, errs
}

// checkWebhookURL refuses what can be refused on save; the dial-time guard is the real SSRF check.
func (s *Service) checkWebhookURL(raw string) string {
	u, err := url.Parse(raw)
	switch {
	case raw == "" || err != nil || u.Host == "" || u.Opaque != "":
		return "an absolute URL, e.g. https://hooks.example.com/gedour"
	case len(raw) > 2048:
		return "at most 2048 characters"
	case u.User != nil:
		return "no credentials in the URL: the receiver checks the signature instead"
	case netguard.CheckScheme(u.Scheme, u.Hostname(), s.httpHosts) != nil:
		return "must use https (http only for hosts in GEDOUR_ALLOW_HTTP_HOSTS)"
	}
	if ip, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); err == nil && !s.guard.Allowed(ip) {
		return "a private, loopback or link-local address: allow its range in GEDOUR_ALLOW_PRIVATE_NETS first"
	}
	return ""
}

// checkSecret validates the secret of a channel type and fills a generated webhook key; generated
// reports whether the caller must be shown it.
func checkSecret(typ, given string) (sec string, generated bool, errs []error) {
	bad := func(msg string) []error {
		return []error{&huma.ErrorDetail{Location: "body.secret", Message: msg}}
	}
	switch typ {
	case notify.TypeTelegram:
		if !botTokenRe.MatchString(given) {
			return "", false, bad("a bot token from @BotFather, e.g. 123456789:AAE…")
		}
	case notify.TypeEmail:
		if given != "" {
			return "", false, bad("email channels have no secret: the SMTP server is instance config")
		}
	case notify.TypeScript:
		if given != "" {
			return "", false, bad("script channels have no secret")
		}
	case notify.TypeWebhook:
		if given == "" {
			b := make([]byte, 32)
			_, _ = rand.Read(b) // crypto/rand.Read never fails (Go 1.24+)
			return base64.RawURLEncoding.EncodeToString(b), true, nil
		}
		if utf8.RuneCountInString(given) < 16 {
			return "", false, bad("at least 16 characters, or leave it empty to have one generated")
		}
	}
	return given, false, nil
}

func (s *Service) registerChannels(api huma.API) {
	tags := []string{"channels"}
	huma.Register(api, huma.Operation{
		OperationID: "list-channel-types", Method: http.MethodGet, Path: "/api/v1/channel-types",
		Summary: "Channel types and whether this instance can send to them", Tags: tags,
	}, func(ctx context.Context, _ *struct{}) (*channelTypesOutput, error) {
		if _, err := s.me(ctx); err != nil {
			return nil, err
		}
		out := &channelTypesOutput{}
		for _, t := range notify.Types {
			ct := ChannelType{Type: t, Available: true}
			if t == notify.TypeEmail && s.sender.Mailer == nil {
				ct.Available, ct.Hint = false, emailUnavailable
			}
			out.Body.Items = append(out.Body.Items, ct)
		}
		return out, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "list-channels", Method: http.MethodGet, Path: "/api/v1/channels",
		Summary: "The caller's channels; secrets are never returned", Tags: tags,
	}, s.listChannels)
	huma.Register(api, huma.Operation{
		OperationID: "get-channel", Method: http.MethodGet, Path: "/api/v1/channels/{id}",
		Summary: "One of the caller's channels", Tags: tags,
	}, func(ctx context.Context, in *idPath) (*channelOutput, error) {
		me, err := s.me(ctx)
		if err != nil {
			return nil, err
		}
		c, err := ownChannel(ctx, s.pool, me, in.ID, false)
		if err != nil {
			return nil, s.fail("get-channel", err)
		}
		return &channelOutput{Body: c}, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "create-channel", Method: http.MethodPost, Path: "/api/v1/channels",
		Summary: "Create a channel; a webhook's generated signing secret is returned once", Tags: tags, DefaultStatus: http.StatusCreated,
	}, s.createChannel)
	huma.Register(api, huma.Operation{
		OperationID: "update-channel", Method: http.MethodPatch, Path: "/api/v1/channels/{id}",
		Summary: "Change a channel's name, config or secret", Tags: tags,
	}, s.updateChannel)
	huma.Register(api, huma.Operation{
		OperationID: "delete-channel", Method: http.MethodDelete, Path: "/api/v1/channels/{id}",
		Summary: "Delete a channel no rule uses", Tags: tags, DefaultStatus: http.StatusNoContent,
	}, s.deleteChannel)
	huma.Register(api, huma.Operation{
		OperationID: "test-channel", Method: http.MethodPost, Path: "/api/v1/channels/{id}/test",
		Summary: "Send a test message through the channel now (one test per 10 s per user)", Tags: tags,
	}, s.testChannel)
}

type idPath struct {
	ID string `path:"id" format:"uuid"`
}

func (s *Service) listChannels(ctx context.Context, _ *struct{}) (*listChannelsOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, channelSelect+" ORDER BY lower(c.name), c.id", pgx.NamedArgs{"owner": me})
	if err != nil {
		return nil, s.fail("list-channels", err)
	}
	out := &listChannelsOutput{}
	if out.Body.Items, err = scanChannels(rows); err != nil {
		return nil, s.fail("list-channels", err)
	}
	if out.Body.Items == nil {
		out.Body.Items = []Channel{}
	}
	return out, nil
}

func nameError(name string) error {
	return huma.Error409Conflict("a channel with this name exists",
		&huma.ErrorDetail{Location: "body.name", Message: "you already have a channel with this name", Value: name})
}

func (s *Service) createChannel(ctx context.Context, in *createChannelInput) (*createChannelOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	if b.Type == notify.TypeEmail && s.sender.Mailer == nil {
		return nil, huma.Error400BadRequest(emailUnavailable, &huma.ErrorDetail{Location: "body.type", Message: emailUnavailable, Value: b.Type})
	}
	name := strings.TrimSpace(b.Name)
	cfg, errs := s.validateConfig(b.Type, b.Config)
	if name == "" {
		errs = append(errs, &huma.ErrorDetail{Location: "body.name", Message: "must not be blank"})
	}
	sec, generated, serrs := checkSecret(b.Type, b.Secret)
	if errs = append(errs, serrs...); len(errs) > 0 {
		return nil, huma.Error400BadRequest("invalid channel", errs...)
	}
	id := uuid.NewString()
	var ct, nonce []byte
	var keyVersion *int
	if sec != "" {
		ct, nonce = s.box.Seal([]byte(sec), []byte(id))
		v := secret.KeyVersion
		keyVersion = &v
	}
	out := &createChannelOutput{}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := underCap(ctx, tx, me, "channels", MaxChannels); err != nil {
			return err
		}
		if b.Type == notify.TypeScript {
			if err := checkScriptRef(ctx, tx, me, cfg); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO channels (id, owner_id, name, type, config, secret_ciphertext, secret_nonce, key_version, last4)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, id, me, name, b.Type, cfg, ct, nonce, keyVersion, last4(sec))
		if isUniqueViolation(err) {
			return nameError(name)
		}
		if err != nil {
			return err
		}
		if out.Body.Channel, err = ownChannel(ctx, tx, me, id, false); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "channel.created", TargetType: "channel", TargetID: id, After: out.Body.Channel})
	})
	if err != nil {
		return nil, s.fail("create-channel", err)
	}
	if generated {
		out.Body.SigningSecret = sec
	}
	return out, nil
}

func (s *Service) updateChannel(ctx context.Context, in *updateChannelInput) (*channelOutput, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	b := in.Body
	var after Channel
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := ownChannel(ctx, tx, me, in.ID, true)
		if err != nil {
			return err
		}
		next := before
		var errs []error
		if b.Name != nil {
			if next.Name = strings.TrimSpace(*b.Name); next.Name == "" {
				errs = append(errs, &huma.ErrorDetail{Location: "body.name", Message: "must not be blank"})
			}
		}
		if b.Config != nil {
			var cerrs []error
			next.Config, cerrs = s.validateConfig(before.Type, b.Config)
			errs = append(errs, cerrs...)
		}
		var sec string
		if b.Secret != nil {
			if *b.Secret == "" {
				errs = append(errs, &huma.ErrorDetail{Location: "body.secret", Message: "must not be empty: leave it out to keep the current one"})
			} else {
				var serrs []error
				sec, _, serrs = checkSecret(before.Type, *b.Secret)
				errs = append(errs, serrs...)
			}
		}
		if len(errs) > 0 {
			return huma.Error400BadRequest("invalid channel", errs...)
		}
		if b.Config != nil && before.Type == notify.TypeScript {
			if err := checkScriptRef(ctx, tx, me, next.Config); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE channels SET name = $2, config = $3, updated_at = now() WHERE id = $1`, in.ID, next.Name, next.Config)
		if isUniqueViolation(err) {
			return nameError(next.Name)
		}
		if err != nil {
			return err
		}
		if sec != "" {
			ct, nonce := s.box.Seal([]byte(sec), []byte(in.ID))
			if _, err := tx.Exec(ctx, `UPDATE channels SET secret_ciphertext = $2, secret_nonce = $3, key_version = $4, last4 = $5 WHERE id = $1`,
				in.ID, ct, nonce, secret.KeyVersion, last4(sec)); err != nil {
				return err
			}
		}
		if after, err = ownChannel(ctx, tx, me, in.ID, false); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "channel.updated", TargetType: "channel", TargetID: in.ID, Before: before, After: after})
	})
	if err != nil {
		return nil, s.fail("update-channel", err)
	}
	return &channelOutput{Body: after}, nil
}

func (s *Service) deleteChannel(ctx context.Context, in *idPath) (*struct{}, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		before, err := ownChannel(ctx, tx, me, in.ID, true)
		if err != nil {
			return err
		}
		// A channel only attaches to its owner's rules, so every user of it is visible to the caller.
		rows, err := tx.Query(ctx, `SELECT r.name FROM rules r JOIN rule_channels rc ON rc.rule_id = r.id
			WHERE rc.channel_id = $1 ORDER BY lower(r.name)`, in.ID)
		if err != nil {
			return err
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if len(names) > 0 {
			details := make([]error, len(names))
			for i, n := range names {
				details[i] = &huma.ErrorDetail{Location: "rules", Message: "sends to this channel", Value: n}
			}
			return huma.Error409Conflict("used by "+strings.Join(names, ", ")+": remove it from these rules first", details...)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM channels WHERE id = $1`, in.ID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "channel.deleted", TargetType: "channel", TargetID: in.ID, Before: before})
	})
	if err != nil {
		return nil, s.fail("delete-channel", err)
	}
	return nil, nil
}

// errTooSoon carries the wait of a rate-limited channel test.
type errTooSoon struct{ wait time.Duration }

func (e errTooSoon) Error() string { return "too soon" }

// testChannel audits the attempt first (that row is also the cross-replica rate limit), then sends
// outside the transaction so a slow receiver holds no lock.
func (s *Service) testChannel(ctx context.Context, in *idPath) (*testResult, error) {
	me, err := s.me(ctx)
	if err != nil {
		return nil, err
	}
	// A script test needs a sandbox: take it before the audit row, which is also the rate limit, so a
	// busy answer costs the user nothing. The type never changes, so this read stays true.
	pre, err := ownChannel(ctx, s.pool, me, in.ID, false)
	if err != nil {
		return nil, s.fail("test-channel", err)
	}
	if pre.Type == notify.TypeScript {
		release, ok := s.runner.TryReserve()
		if !ok {
			return nil, busy()
		}
		defer release()
	}
	var ch notify.Channel
	err = db.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// The user row lock comes first, as in every user-scoped transaction that takes it (the caps):
		// one lock order, no deadlock with a concurrent create. It also serializes this user's tests.
		if _, err := tx.Exec(ctx, `SELECT 1 FROM users WHERE id = $1 FOR NO KEY UPDATE`, me); err != nil {
			return err
		}
		c, err := ownChannel(ctx, tx, me, in.ID, false)
		if err != nil {
			return err
		}
		var waitMS float64
		// The actor index, bounded to the interval, finds the user's last test.
		if err := tx.QueryRow(ctx, `SELECT COALESCE(EXTRACT(EPOCH FROM (max(at) + make_interval(secs => $2) - now())) * 1000, 0)
			FROM audit_log WHERE actor_user_id = $1 AND at > now() - make_interval(secs => $2) AND action = 'channel.tested'`,
			me, TestInterval.Seconds()).Scan(&waitMS); err != nil {
			return err
		}
		if waitMS > 0 {
			return errTooSoon{time.Duration(waitMS) * time.Millisecond}
		}
		if ch, err = loadChannel(ctx, tx, s.box, in.ID); errors.Is(err, pgx.ErrNoRows) {
			// Deleted since ownChannel read it (no row lock, the user lock orders this transaction).
			return huma.Error404NotFound("channel not found")
		} else if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{ActorUserID: me, Action: "channel.tested", TargetType: "channel", TargetID: in.ID, After: c})
	})
	var soon errTooSoon
	if errors.As(err, &soon) {
		secs := int(soon.wait.Seconds()) + 1
		return nil, huma.ErrorWithHeaders(huma.Error429TooManyRequests(fmt.Sprintf("you sent a test less than %s ago: retry in %d s", TestInterval, secs)),
			http.Header{"Retry-After": {strconv.Itoa(secs)}})
	}
	if err != nil {
		return nil, s.fail("test-channel", err)
	}
	out := &testResult{}
	logs, sendErr := send(ctx, s.pool, s.sender, s.runner, ch, notify.TestDelivery(time.Now()), true)
	out.Body.Logs = logs
	if sendErr != nil {
		out.Body.Error = truncate(sendErr.Error())
	}
	out.Body.OK = sendErr == nil
	return out, nil
}

// loadChannel reads a channel with its secret opened, for one send.
func loadChannel(ctx context.Context, q rowQuerier, box *secret.Box, id string) (notify.Channel, error) {
	ch := notify.Channel{ID: id}
	var ct, nonce []byte
	if err := q.QueryRow(ctx, `SELECT type, config, secret_ciphertext, secret_nonce FROM channels WHERE id = $1`, id).
		Scan(&ch.Type, &ch.Config, &ct, &nonce); err != nil {
		return ch, fmt.Errorf("load channel: %w", err)
	}
	if ct != nil {
		plain, err := box.Open(ct, nonce, []byte(id))
		if err != nil {
			return ch, fmt.Errorf("channel secret: %w", err)
		}
		ch.Secret = string(plain)
	}
	return ch, nil
}
