package server

import (
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gedour/gedour/pkg/filter"
	"github.com/gedour/gedour/pkg/notify"
)

type channelJSON struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	Type          string         `json:"type"`
	Config        map[string]any `json:"config"`
	Last4         string         `json:"last4"`
	SigningSecret string         `json:"signing_secret"`
	LastStatus    *string        `json:"last_delivery_status"`
}

type ruleJSON struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	WatchID    *string  `json:"watch_id"`
	WatchName  *string  `json:"watch_name"`
	Filter     string   `json:"filter"`
	Enabled    bool     `json:"enabled"`
	ChannelIDs []string `json:"channel_ids"`
}

func (a *devApp) problemOf(t *testing.T, u apiUser, method, path string, body any, code int) problem {
	t.Helper()
	var p problem
	a.mustCall(t, u, method, path, body, code, &p)
	return p
}

const botToken = "123456789:AAEhBOweik6ad9r_QXMENQjcrGbqCr4K-ra"

// 003-AC1: a Talk channel is validated on save, signs with a secret the API and audit log never show,
// can be edited, tested and deleted. 003-AC5: silent only exists on Telegram and Talk configs.
func TestTalkChannels(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	secret := strings.Repeat("s3cret-talk-", 4)
	cfg := func(url, conv string) map[string]any { return map[string]any{"url": url, "conversation": conv} }
	talk := func(c map[string]any, sec string) map[string]any {
		return map[string]any{"name": "Room", "type": "nextcloud_talk", "config": c, "secret": sec}
	}
	for _, tt := range []struct {
		body map[string]any
		locs []string
	}{
		{talk(cfg("http://cloud.example", "abcd"), secret), []string{"body.config.url"}},
		{talk(cfg("https://u:p@cloud.example", "abcd"), secret), []string{"body.config.url"}}, //nolint:gosec // the refused shape itself
		{talk(cfg("/relative", "abcd"), secret), []string{"body.config.url"}},
		{talk(cfg("https://cloud.example/?x=1", "abcd"), secret), []string{"body.config.url"}},
		{talk(cfg("https://cloud.example/#f", "abcd"), secret), []string{"body.config.url"}},
		{talk(cfg("https://10.9.2.3", "abcd"), secret), []string{"body.config.url"}},
		{talk(cfg("https://cloud.example", "abc"), secret), []string{"body.config.conversation"}},
		{talk(cfg("https://cloud.example", "../../users"), secret), []string{"body.config.conversation"}},
		{talk(cfg("https://cloud.example", "ABCD"), secret), []string{"body.config.conversation"}},
		{talk(cfg("https://cloud.example", strings.Repeat("a", 31)), secret), []string{"body.config.conversation"}},
		{talk(cfg("https://cloud.example", "abcd"), ""), []string{"body.secret"}},
		{talk(cfg("https://cloud.example", "abcd"), strings.Repeat("s", 39)), []string{"body.secret"}},
		{talk(cfg("https://cloud.example", "abcd"), strings.Repeat("s", 129)), []string{"body.secret"}},
		{talk(cfg("https://cloud.example", "abcd"), secret+" "), []string{"body.secret"}},
		{talk(cfg("https://cloud.example", "abcd"), " "+secret), []string{"body.secret"}},
		{talk(cfg("https://cloud.example", "abcd"), secret+"\n"), []string{"body.secret"}},
		{talk(cfg("https://cloud.example", "abcd"), "\t"+secret), []string{"body.secret"}},
		{talk(map[string]any{"url": "https://cloud.example", "conversation": "abcd", "silent": "yes"}, secret), []string{"body.config"}},
		{talk(map[string]any{"url": "https://cloud.example", "conversation": "abcd", "extra": 1}, secret), []string{"body.config"}},
		// 003-AC5: no silent field on the other types.
		{map[string]any{"name": "w", "type": "webhook", "config": map[string]any{"url": "https://hooks.example/x", "silent": true}}, []string{"body.config"}},
	} {
		p := a.problemOf(t, alice, http.MethodPost, "/api/v1/channels", tt.body, http.StatusBadRequest)
		if got := p.locations(); !slices.Equal(got, tt.locs) {
			t.Errorf("%v: locations %v, want %v", tt.body, got, tt.locs)
		}
	}
	// One field error, one message: the form places it once.
	_, refused := a.call(t, alice, http.MethodPost, "/api/v1/channels", talk(cfg("https://cloud.example", "BAD"), secret))
	if n := strings.Count(string(refused), "the conversation token"); n != 1 {
		t.Fatalf("conversation error %d times: %s", n, refused)
	}
	for _, n := range []int{40, 128} {
		var c channelJSON
		a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": fmt.Sprintf("bounds %d", n), "type": "nextcloud_talk", "config": cfg("https://cloud.example", "abcd"), "secret": strings.Repeat("s", n)}, http.StatusCreated, &c)
	}

	var mu sync.Mutex
	var random, sig string
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		random, sig = r.Header.Get(notify.TalkRandomHeader), r.Header.Get(notify.TalkSignatureHeader)
		_ = json.NewDecoder(r.Body).Decode(&sent)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	var ch channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", talk(map[string]any{"url": srv.URL + "/ ", "conversation": " abcd1234 ", "silent": true}, secret), http.StatusCreated, &ch)
	if ch.Config["url"] != srv.URL || ch.Config["conversation"] != "abcd1234" || ch.Config["silent"] != true || ch.SigningSecret != "" {
		t.Fatalf("normalized config: %+v", ch)
	}

	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels/"+ch.ID+"/test", nil, http.StatusOK, &res)
	mu.Lock()
	if !res.OK || sent["silent"] != true || sig != notify.TalkSignature(secret, random, fmt.Sprint(sent["message"])) {
		t.Fatalf("test send: %+v %v", res, sent)
	}
	mu.Unlock()

	// A patch replaces the whole config: silent left out reads as false.
	var upd channelJSON
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/channels/"+ch.ID, map[string]any{"name": "Room 2", "config": cfg(srv.URL, "efgh5678")}, http.StatusOK, &upd)
	if upd.Name != "Room 2" || upd.Config["silent"] != false || upd.Config["conversation"] != "efgh5678" {
		t.Fatalf("update: %+v", upd)
	}
	a.problemOf(t, alice, http.MethodPatch, "/api/v1/channels/"+ch.ID, map[string]any{"secret": "short"}, http.StatusBadRequest)

	// A patch without a secret keeps it: the next send still signs with the original.
	a.exec(t, `ALTER TABLE audit_log DISABLE TRIGGER audit_log_append_only`)
	a.exec(t, `UPDATE audit_log SET at = at - interval '1 minute' WHERE action = 'channel.tested'`)
	a.exec(t, `ALTER TABLE audit_log ENABLE TRIGGER audit_log_append_only`)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels/"+ch.ID+"/test", nil, http.StatusOK, &res)
	mu.Lock()
	if !res.OK || sig != notify.TalkSignature(secret, random, fmt.Sprint(sent["message"])) {
		t.Fatalf("send after a patch without secret: %+v", res)
	}
	mu.Unlock()

	_, got := a.call(t, alice, http.MethodGet, "/api/v1/channels/"+ch.ID, nil)
	_, list := a.call(t, alice, http.MethodGet, "/api/v1/channels", nil)
	a.mustCall(t, alice, http.MethodDelete, "/api/v1/channels/"+ch.ID, nil, http.StatusNoContent, nil)
	var auditText string
	if err := a.pool.QueryRow(t.Context(), `SELECT string_agg(coalesce(before::text, '') || coalesce(after::text, ''), ' ') FROM audit_log WHERE target_type = 'channel'`).Scan(&auditText); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]string{"get": string(got), "list": string(list), "audit": auditText} {
		if strings.Contains(s, secret) {
			t.Errorf("%s carries the secret: %s", name, s)
		}
	}
	if !strings.Contains(auditText, "nextcloud_talk") || a.countRows(t, `SELECT count(*) FROM audit_log WHERE action = 'channel.deleted' AND target_id = $1`, ch.ID) != 1 {
		t.Fatalf("not audited: %s", auditText)
	}
}

// 003-AC4: the silent flag of a Telegram channel; a config stored before it reads as false.
func TestTelegramSilentConfig(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	var tg channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Quiet", "type": "telegram", "config": map[string]any{"chat_id": "-1", "silent": true}, "secret": botToken}, http.StatusCreated, &tg)
	if tg.Config["silent"] != true {
		t.Fatalf("silent: %+v", tg.Config)
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Loud", "type": "telegram", "config": map[string]any{"chat_id": "-2"}, "secret": botToken}, http.StatusCreated, &tg)
	if tg.Config["silent"] != false {
		t.Fatalf("default: %+v", tg.Config)
	}
	a.exec(t, `UPDATE channels SET config = '{"chat_id":"-3"}' WHERE id = '`+tg.ID+`'`)
	var old channelJSON
	a.mustCall(t, alice, http.MethodGet, "/api/v1/channels/"+tg.ID, nil, http.StatusOK, &old)
	if v, set := old.Config["silent"]; set {
		t.Fatalf("an existing channel must stay unchanged: %v", v)
	}
	a.problemOf(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Bad", "type": "telegram", "config": map[string]any{"chat_id": "-4", "silent": 1}, "secret": botToken}, http.StatusBadRequest)
}

// 001-AC15/001-AC16: channels per type, validated on save, secrets write-only, private to their owner.
func TestChannels(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")

	var types struct {
		Items []struct {
			Type      string `json:"type"`
			Available bool   `json:"available"`
			Hint      string `json:"hint"`
		} `json:"items"`
	}
	a.mustCall(t, alice, http.MethodGet, "/api/v1/channel-types", nil, http.StatusOK, &types)
	if len(types.Items) != 5 || types.Items[1].Type != "nextcloud_talk" || !types.Items[1].Available || types.Items[4].Type != "script" || !types.Items[4].Available || types.Items[2].Type != "email" || types.Items[2].Available || !strings.Contains(types.Items[2].Hint, "GEDOUR_SMTP_ADDR") {
		t.Fatalf("channel types without SMTP: %+v", types)
	}
	p := a.problemOf(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "mail", "type": "email", "config": map[string]any{"to": []string{"a@x.test"}}}, http.StatusBadRequest)
	if !slices.Contains(p.locations(), "body.type") {
		t.Fatalf("email without SMTP: %+v", p)
	}

	// Validation: every field error at once.
	for _, tt := range []struct {
		body map[string]any
		locs []string
	}{
		{map[string]any{"name": " ", "type": "telegram", "config": map[string]any{"chat_id": "nope"}, "secret": "bad/token"},
			[]string{"body.config.chat_id", "body.name", "body.secret"}},
		{map[string]any{"name": "t", "type": "telegram", "config": map[string]any{"chat_id": "1", "extra": 1}, "secret": botToken}, []string{"body.config"}},
		{map[string]any{"name": "w", "type": "webhook", "config": map[string]any{"url": "http://hooks.example/x"}}, []string{"body.config.url"}},
		{map[string]any{"name": "w", "type": "webhook", "config": map[string]any{"url": "https://u:p@hooks.example/x"}}, []string{"body.config.url"}}, //nolint:gosec // the refused shape itself
		{map[string]any{"name": "w", "type": "webhook", "config": map[string]any{"url": "/relative"}}, []string{"body.config.url"}},
		{map[string]any{"name": "w", "type": "webhook", "config": map[string]any{"url": "https://hooks.example/x"}, "secret": "short"}, []string{"body.secret"}},
	} {
		p := a.problemOf(t, alice, http.MethodPost, "/api/v1/channels", tt.body, http.StatusBadRequest)
		if got := p.locations(); !slices.Equal(got, tt.locs) {
			t.Errorf("%v: locations %v, want %v", tt.body, got, tt.locs)
		}
	}
	// 001-AC16: a literal private, loopback or link-local target is refused on save unless allowlisted
	// (the test guard allows 127.0.0.0/8).
	for _, u := range []string{"https://10.9.2.3/hook", "https://169.254.169.254/latest", "https://[::1]/hook", "https://192.168.1.1/"} {
		p := a.problemOf(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "w", "type": "webhook", "config": map[string]any{"url": u}}, http.StatusBadRequest)
		if len(p.Errors) != 1 || !strings.Contains(p.Errors[0].Message, "GEDOUR_ALLOW_PRIVATE_NETS") {
			t.Errorf("%s: %+v", u, p)
		}
	}

	var tg channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Phone", "type": "telegram", "config": map[string]any{"chat_id": " -1001234 "}, "secret": botToken}, http.StatusCreated, &tg)
	if tg.Config["chat_id"] != "-1001234" || tg.Last4 != "K-ra" || tg.SigningSecret != "" {
		t.Fatalf("telegram: %+v", tg)
	}
	var hook channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Hook", "type": "webhook", "config": map[string]any{"url": "http://127.0.0.1:9/x"}}, http.StatusCreated, &hook)
	if len(hook.SigningSecret) < 40 || hook.Last4 != hook.SigningSecret[len(hook.SigningSecret)-4:] {
		t.Fatalf("webhook signing secret: %+v", hook)
	}
	// The signing secret is shown once: never in a read, a list, an update or the audit log.
	_, raw := a.call(t, alice, http.MethodGet, "/api/v1/channels/"+hook.ID, nil)
	_, list := a.call(t, alice, http.MethodGet, "/api/v1/channels", nil)
	_, upd := a.call(t, alice, http.MethodPatch, "/api/v1/channels/"+hook.ID, map[string]any{"name": "Hook 2"})
	var auditText string
	if err := a.pool.QueryRow(t.Context(), `SELECT string_agg(coalesce(before::text, '') || coalesce(after::text, ''), ' ') FROM audit_log WHERE target_type = 'channel'`).Scan(&auditText); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]string{"get": string(raw), "list": string(list), "update": string(upd), "audit": auditText} {
		if strings.Contains(s, hook.SigningSecret) || strings.Contains(s, botToken) || strings.Contains(s, "signing_secret") {
			t.Errorf("%s carries a secret: %s", name, s)
		}
	}
	if !strings.Contains(auditText, "Hook 2") {
		t.Fatalf("update not audited: %s", auditText)
	}
	var stored []byte
	if err := a.pool.QueryRow(t.Context(), `SELECT secret_ciphertext FROM channels WHERE id = $1`, hook.ID).Scan(&stored); err != nil || strings.Contains(string(stored), hook.SigningSecret) {
		t.Fatalf("secret stored in clear: %v", err)
	}

	// Email once SMTP is configured: recipients normalized, deduplicated, at most 10.
	a.sender.Mailer = &notify.Mailer{Addr: "127.0.0.1:1", From: &mail.Address{Address: "g@x.test"}, TLS: notify.TLSNone}
	var em channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Mail", "type": "email",
		"config": map[string]any{"to": []string{"Ops <ops@x.test>", "ops@x.test", "dev@x.test"}}}, http.StatusCreated, &em)
	if fmt.Sprint(em.Config["to"]) != "[ops@x.test dev@x.test]" {
		t.Fatalf("recipients: %v", em.Config["to"])
	}
	many := make([]string, 11)
	for i := range many {
		many[i] = fmt.Sprintf("u%d@x.test", i)
	}
	for _, to := range [][]string{many, {}, {"not an address"}, {`"a b"@x.test`}, {"a@"}} {
		a.problemOf(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "M2", "type": "email", "config": map[string]any{"to": to}}, http.StatusBadRequest)
	}
	a.problemOf(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "M3", "type": "email", "config": map[string]any{"to": []string{"a@x.test"}}, "secret": "x"}, http.StatusBadRequest)
	a.problemOf(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Mail", "type": "email", "config": map[string]any{"to": []string{"a@x.test"}}}, http.StatusConflict)

	// Update: config replaced and validated against the channel's type, secret rotated.
	var tg2 channelJSON
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/channels/"+tg.ID, map[string]any{"config": map[string]any{"chat_id": "@gedour_ops"}, "secret": "987:AAAAAAAAAAAAAAAAAAAAAAAAzzzz"}, http.StatusOK, &tg2)
	if tg2.Config["chat_id"] != "@gedour_ops" || tg2.Last4 != "zzzz" {
		t.Fatalf("telegram update: %+v", tg2)
	}
	a.problemOf(t, alice, http.MethodPatch, "/api/v1/channels/"+tg.ID, map[string]any{"config": map[string]any{"url": "https://x"}}, http.StatusBadRequest)
	a.problemOf(t, alice, http.MethodPatch, "/api/v1/channels/"+tg.ID, map[string]any{"secret": ""}, http.StatusBadRequest)
	a.problemOf(t, alice, http.MethodPatch, "/api/v1/channels/"+tg.ID, map[string]any{"name": "Mail"}, http.StatusConflict)

	// Private: bob sees none of them and gets 404 on every route.
	var bobList struct{ Items []channelJSON }
	a.mustCall(t, bob, http.MethodGet, "/api/v1/channels", nil, http.StatusOK, &bobList)
	if len(bobList.Items) != 0 {
		t.Fatal("bob sees alice's channels")
	}
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/channels/" + hook.ID}, {http.MethodPatch, "/api/v1/channels/" + hook.ID},
		{http.MethodDelete, "/api/v1/channels/" + hook.ID}, {http.MethodPost, "/api/v1/channels/" + hook.ID + "/test"},
	} {
		if code, _ := a.call(t, bob, r.method, r.path, map[string]any{}); code != http.StatusNotFound {
			t.Errorf("bob %s %s: %d", r.method, r.path, code)
		}
	}

	// A channel a rule uses cannot go; the 409 names the rule.
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "Releases", "channel_ids": []string{hook.ID}}, http.StatusCreated, nil)
	p = a.problemOf(t, alice, http.MethodDelete, "/api/v1/channels/"+hook.ID, nil, http.StatusConflict)
	if len(p.Errors) != 1 || p.Errors[0].Value != "Releases" {
		t.Fatalf("delete in use: %+v", p)
	}
	a.mustCall(t, alice, http.MethodDelete, "/api/v1/channels/"+tg.ID, nil, http.StatusNoContent, nil)
	a.mustCall(t, alice, http.MethodGet, "/api/v1/channels/"+tg.ID, nil, http.StatusNotFound, nil)
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE target_type = 'channel' AND action = 'channel.deleted'`); n != 1 {
		t.Fatalf("delete audited %d times", n)
	}
}

// 001-AC16: "Send test" signs like a real delivery, reports the receiver's failure, and is rate-limited.
func TestChannelTest(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	var mu sync.Mutex
	var body []byte
	var sig string
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ = io.ReadAll(r.Body)
		sig = r.Header.Get(notify.SignatureHeader)
		if fail {
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()
	var hook channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Hook", "type": "webhook", "config": map[string]any{"url": srv.URL}}, http.StatusCreated, &hook)

	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels/"+hook.ID+"/test", nil, http.StatusOK, &res)
	mu.Lock()
	if !res.OK || !hmac.Equal([]byte(sig), []byte(notify.Sign(hook.SigningSecret, body))) || !strings.Contains(string(body), notify.TestEventType) {
		t.Fatalf("test send: %+v sig=%s body=%s", res, sig, body)
	}
	fail = true
	mu.Unlock()
	code, raw := a.call(t, alice, http.MethodPost, "/api/v1/channels/"+hook.ID+"/test", nil)
	if code != http.StatusTooManyRequests || !strings.Contains(string(raw), "retry in") {
		t.Fatalf("second test within 10 s: %d %s", code, raw)
	}
	a.exec(t, `ALTER TABLE audit_log DISABLE TRIGGER audit_log_append_only`)
	a.exec(t, `UPDATE audit_log SET at = at - interval '1 minute' WHERE action = 'channel.tested'`)
	a.exec(t, `ALTER TABLE audit_log ENABLE TRIGGER audit_log_append_only`)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels/"+hook.ID+"/test", nil, http.StatusOK, &res)
	// The limit is per user: another channel of the same user waits too.
	var other channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Other", "type": "webhook", "config": map[string]any{"url": srv.URL}}, http.StatusCreated, &other)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels/"+other.ID+"/test", nil, http.StatusTooManyRequests, nil)
	if res.OK || !strings.Contains(res.Error, "HTTP 418") {
		t.Fatalf("failing receiver: %+v", res)
	}
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE action = 'channel.tested' AND target_id = $1`, hook.ID); n != 2 {
		t.Fatalf("tests audited %d times, want 2", n)
	}
}

// 001-AC13: filters compile on save (400 with the position), scope and channels are checked against the
// owner's access.
func TestRules(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var mine, bobs, shared watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &mine)
	a.mustCall(t, bob, http.MethodPost, "/api/v1/watches", sampleWatches[1], http.StatusCreated, &bobs)
	a.mustCall(t, bob, http.MethodPost, "/api/v1/watches", sampleWatches[2], http.StatusCreated, &shared)
	a.mustCall(t, bob, http.MethodPost, "/api/v1/watches/"+shared.ID+"/shares", map[string]any{"email": alice.email, "permission": "ro"}, http.StatusCreated, nil)
	var hook, bobHook channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Hook", "type": "webhook", "config": map[string]any{"url": "https://hooks.example/a"}}, http.StatusCreated, &hook)
	a.mustCall(t, bob, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Hook", "type": "webhook", "config": map[string]any{"url": "https://hooks.example/b"}}, http.StatusCreated, &bobHook)

	p := a.problemOf(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "r", "filter": "event.type ==\n  && watch.nme"}, http.StatusBadRequest)
	if len(p.Errors) != 1 || p.Errors[0].Location != "body.filter" || !strings.Contains(p.Errors[0].Message, "(line 2, column 3)") ||
		fmt.Sprint(p.Errors[0].Value) != "map[column:3 line:2]" {
		t.Fatalf("filter error: %+v", p)
	}
	p = a.problemOf(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "r", "filter": `event.nope == 1`}, http.StatusBadRequest)
	if !strings.Contains(p.Errors[0].Message, "event has no field nope (line 1, column 7)") {
		t.Fatalf("type error: %+v", p)
	}
	for _, body := range []map[string]any{
		{"name": "r", "watch_id": bobs.ID},
		{"name": "r", "watch_id": "nope"},
		{"name": "r", "channel_ids": []string{bobHook.ID}},
		{"name": "r", "channel_ids": []string{"nope"}},
	} {
		a.problemOf(t, alice, http.MethodPost, "/api/v1/rules", body, http.StatusBadRequest)
	}

	var r ruleJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "Shared releases", "watch_id": shared.ID,
		"filter": ` event.type == "release.published" `, "channel_ids": []string{hook.ID, hook.ID}}, http.StatusCreated, &r)
	if r.Filter != `event.type == "release.published"` || !r.Enabled || len(r.ChannelIDs) != 1 || r.WatchName == nil || *r.WatchName != shared.Name {
		t.Fatalf("created: %+v", r)
	}
	a.problemOf(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "Shared releases"}, http.StatusConflict)
	var upd ruleJSON
	a.mustCall(t, alice, http.MethodPatch, "/api/v1/rules/"+r.ID, map[string]any{"watch_id": "", "enabled": false, "channel_ids": []string{}}, http.StatusOK, &upd)
	if upd.WatchID != nil || upd.Enabled || len(upd.ChannelIDs) != 0 || upd.Filter != r.Filter {
		t.Fatalf("updated: %+v", upd)
	}
	a.problemOf(t, alice, http.MethodPatch, "/api/v1/rules/"+r.ID, map[string]any{"filter": "("}, http.StatusBadRequest)
	a.problemOf(t, alice, http.MethodPatch, "/api/v1/rules/"+r.ID, map[string]any{"watch_id": bobs.ID}, http.StatusBadRequest)
	a.problemOf(t, alice, http.MethodPatch, "/api/v1/rules/"+r.ID, map[string]any{"name": " "}, http.StatusBadRequest)

	var list struct{ Items []ruleJSON }
	a.mustCall(t, bob, http.MethodGet, "/api/v1/rules", nil, http.StatusOK, &list)
	if len(list.Items) != 0 {
		t.Fatal("bob lists alice's rules")
	}
	for _, m := range []string{http.MethodGet, http.MethodPatch, http.MethodDelete} {
		if code, _ := a.call(t, bob, m, "/api/v1/rules/"+r.ID, map[string]any{}); code != http.StatusNotFound {
			t.Errorf("bob %s rule: %d", m, code)
		}
	}
	a.mustCall(t, alice, http.MethodGet, "/api/v1/rules/"+r.ID, nil, http.StatusOK, nil)
	a.mustCall(t, alice, http.MethodDelete, "/api/v1/rules/"+r.ID, nil, http.StatusNoContent, nil)
	var actions []string
	rows, _ := a.pool.Query(t.Context(), `SELECT action FROM audit_log WHERE target_type = 'rule' ORDER BY id`)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		actions = append(actions, s)
	}
	if strings.Join(actions, ",") != "rule.created,rule.updated,rule.deleted" {
		t.Fatalf("rule audit: %v", actions)
	}
	// Rules, channels and deliveries are visible in the owner's audit view only.
	var page auditPage
	a.mustCall(t, bob, http.MethodGet, "/api/v1/audit?target_type=rule", nil, http.StatusOK, &page)
	if len(page.Items) != 0 {
		t.Fatal("bob sees alice's rule audit")
	}
}

type previewJSON struct {
	Items []struct {
		Event struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"event"`
		Watch   struct{ ID, Name string } `json:"watch"`
		Matched bool                      `json:"matched"`
		Error   string                    `json:"error"`
	} `json:"items"`
}

// 001-AC13: the preview evaluates the last 20 events of the watches the caller can read, never others'.
func TestRulePreview(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var mine, bobs watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &mine)
	a.mustCall(t, bob, http.MethodPost, "/api/v1/watches", sampleWatches[1], http.StatusCreated, &bobs)
	a.exec(t, `INSERT INTO events (watch_id, type, payload, dedup_key) SELECT $1, CASE WHEN g % 2 = 0 THEN 'tag.created' ELSE 'release.published' END,
		jsonb_build_object('tag', 'v' || g), g::text FROM generate_series(1, 25) g`, mine.ID)
	a.exec(t, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 'pr.state_changed', '{"to":"merged"}', 'x')`, bobs.ID)

	var pv previewJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules/preview", map[string]any{"filter": `event.type == "tag.created"`}, http.StatusOK, &pv)
	if len(pv.Items) != 20 || pv.Items[0].Event.Type != "release.published" || pv.Items[0].Matched || !pv.Items[1].Matched {
		t.Fatalf("preview: %+v", pv.Items[:2])
	}
	for _, it := range pv.Items {
		if it.Watch.ID != mine.ID {
			t.Fatal("another user's event leaked into the preview")
		}
	}
	pv = previewJSON{}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules/preview", map[string]any{"watch_id": bobs.ID, "filter": ""}, http.StatusOK, &pv)
	if len(pv.Items) != 0 {
		t.Fatal("preview scoped to an unreadable watch returned events")
	}
	pv = previewJSON{}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules/preview", map[string]any{"watch_id": mine.ID, "filter": `event.payload.tag > 3`}, http.StatusOK, &pv)
	if len(pv.Items) != 20 || pv.Items[0].Matched || pv.Items[0].Error == "" {
		t.Fatalf("eval errors: %+v", pv.Items[0])
	}
	p := a.problemOf(t, alice, http.MethodPost, "/api/v1/rules/preview", map[string]any{"filter": "event.type =="}, http.StatusBadRequest)
	if p.Errors[0].Location != "body.filter" {
		t.Fatalf("compile error: %+v", p)
	}
	a.problemOf(t, alice, http.MethodPost, "/api/v1/rules/preview", map[string]any{"filter": "", "watch_id": "x"}, http.StatusBadRequest)
}

type inboxJSON struct {
	Items []struct {
		ID     int64   `json:"id"`
		ReadAt *string `json:"read_at"`
		Event  struct {
			Type string `json:"type"`
		} `json:"event"`
		Watch struct{ Name string } `json:"watch"`
	} `json:"items"`
	NextCursor string `json:"next_cursor"`
}

func (a *devApp) inboxIDs(t *testing.T, u apiUser, state string) []int64 {
	t.Helper()
	var page inboxJSON
	a.mustCall(t, u, http.MethodGet, "/api/v1/inbox?state="+state, nil, http.StatusOK, &page)
	var ids []int64
	for _, it := range page.Items {
		ids = append(ids, it.ID)
	}
	return ids
}

// 001-AC20: inbox items are listed by state, bulk read/unread/archived, counted, and only one's own.
func TestInboxAPI(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	a.exec(t, `INSERT INTO events (watch_id, type, payload, dedup_key) SELECT $1, 'release.published', '{}', g::text FROM generate_series(1, 4) g`, w.ID)
	a.exec(t, `INSERT INTO inbox_items (user_id, event_id) SELECT $1, id FROM events ORDER BY id`, alice.id)
	// Bob reads the watch through a share, so his item on it shows.
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches/"+w.ID+"/shares", map[string]any{"email": bob.email, "permission": "ro"}, http.StatusCreated, nil)
	a.exec(t, `INSERT INTO inbox_items (user_id, event_id) SELECT $1, id FROM events ORDER BY id LIMIT 1`, bob.id)

	all := a.inboxIDs(t, alice, "all")
	if len(all) != 4 || all[0] < all[3] {
		t.Fatalf("all, newest first: %v", all)
	}
	bobItems := a.inboxIDs(t, bob, "all")
	type bulk struct{ Updated, Unread int }
	var res bulk
	// Bob's item among the ids is ignored: alice changes only hers.
	a.mustCall(t, alice, http.MethodPost, "/api/v1/inbox/read", map[string]any{"ids": append(all[:2:2], bobItems...)}, http.StatusOK, &res)
	if res.Updated != 2 || res.Unread != 2 {
		t.Fatalf("read: %+v", res)
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/inbox/read", map[string]any{"ids": all[:2]}, http.StatusOK, &res)
	if res.Updated != 0 {
		t.Fatal("re-reading counts as a change")
	}
	if got := a.inboxIDs(t, bob, "unread"); len(got) != 1 {
		t.Fatal("alice's bulk read touched bob's item")
	}
	if got := a.inboxIDs(t, alice, "unread"); !slices.Equal(got, all[2:]) {
		t.Fatalf("unread: %v", got)
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/inbox/unread", map[string]any{"ids": all[:1]}, http.StatusOK, &res)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/inbox/archive", map[string]any{"ids": all[:1]}, http.StatusOK, &res)
	if res.Updated != 1 || res.Unread != 2 {
		t.Fatalf("archive: %+v", res)
	}
	if got := a.inboxIDs(t, alice, "archived"); !slices.Equal(got, all[:1]) {
		t.Fatalf("archived: %v", got)
	}
	if got := a.inboxIDs(t, alice, "all"); len(got) != 3 {
		t.Fatalf("all hides archived: %v", got)
	}
	var count struct{ Unread int }
	a.mustCall(t, alice, http.MethodGet, "/api/v1/inbox/count", nil, http.StatusOK, &count)
	if count.Unread != 2 {
		t.Fatalf("count %d", count.Unread)
	}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/inbox/unarchive", map[string]any{"ids": all[:1]}, http.StatusOK, &res)
	if res.Unread != 3 {
		t.Fatalf("unarchive: %+v", res)
	}

	var page inboxJSON
	a.mustCall(t, alice, http.MethodGet, "/api/v1/inbox?limit=3", nil, http.StatusOK, &page)
	if len(page.Items) != 3 || page.NextCursor == "" || page.Items[0].Watch.Name != w.Name {
		t.Fatalf("page 1: %+v", page)
	}
	cursor := page.NextCursor
	page = inboxJSON{}
	a.mustCall(t, alice, http.MethodGet, "/api/v1/inbox?limit=3&cursor="+cursor, nil, http.StatusOK, &page)
	if len(page.Items) != 1 || page.NextCursor != "" {
		t.Fatalf("page 2: %+v", page)
	}
	for _, q := range []string{"state=new", "cursor=x", "limit=0"} {
		a.problemOf(t, alice, http.MethodGet, "/api/v1/inbox?"+q, nil, http.StatusBadRequest)
	}
	a.problemOf(t, alice, http.MethodPost, "/api/v1/inbox/read", map[string]any{"ids": []int64{}}, http.StatusBadRequest)
}

type deliveriesJSON struct {
	Items []struct {
		ID        int64   `json:"id"`
		Status    string  `json:"status"`
		Attempts  int     `json:"attempts"`
		LastError *string `json:"last_error"`
		Channel   struct{ ID, Name string }
		Rule      *struct{ Name string } `json:"rule"`
	} `json:"items"`
	NextCursor string `json:"next_cursor"`
}

// 001-AC15: the delivery list shows status, attempts and last error; failed and dead ones can be retried.
func TestDeliveriesAPI(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	var hook, other channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Hook", "type": "webhook", "config": map[string]any{"url": "https://hooks.example/a"}}, http.StatusCreated, &hook)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Other", "type": "webhook", "config": map[string]any{"url": "https://hooks.example/b"}}, http.StatusCreated, &other)
	var r ruleJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "All", "channel_ids": []string{hook.ID}}, http.StatusCreated, &r)
	a.exec(t, `INSERT INTO events (watch_id, type, payload, dedup_key) SELECT $1, 'release.published', '{}', g::text FROM generate_series(1, 3) g`, w.ID)
	a.exec(t, `INSERT INTO deliveries (event_id, rule_id, channel_id, status, attempts, last_error)
		SELECT e.id, $1, $2, (ARRAY['sent', 'failed', 'dead'])[row_number() OVER (ORDER BY e.id)], 2, 'HTTP 502' FROM events e`, r.ID, hook.ID)
	a.exec(t, `INSERT INTO deliveries (event_id, channel_id, status) SELECT min(id), $1, 'pending' FROM events`, other.ID)

	var page deliveriesJSON
	a.mustCall(t, alice, http.MethodGet, "/api/v1/deliveries?channel_id="+hook.ID, nil, http.StatusOK, &page)
	if len(page.Items) != 3 || page.Items[0].Status != "dead" || page.Items[0].Attempts != 2 || *page.Items[0].LastError != "HTTP 502" ||
		page.Items[0].Rule == nil || page.Items[0].Rule.Name != "All" || page.Items[0].Channel.Name != "Hook" {
		t.Fatalf("list: %+v", page)
	}
	page = deliveriesJSON{}
	a.mustCall(t, alice, http.MethodGet, "/api/v1/deliveries?status=pending", nil, http.StatusOK, &page)
	if len(page.Items) != 1 || page.Items[0].Channel.ID != other.ID || page.Items[0].Rule != nil {
		t.Fatalf("status filter: %+v", page)
	}
	a.mustCall(t, alice, http.MethodGet, "/api/v1/deliveries?limit=2", nil, http.StatusOK, &page)
	cursor := page.NextCursor
	page = deliveriesJSON{}
	a.mustCall(t, alice, http.MethodGet, "/api/v1/deliveries?limit=2&cursor="+cursor, nil, http.StatusOK, &page)
	if len(page.Items) != 2 || page.NextCursor != "" {
		t.Fatalf("page 2: %+v", page)
	}
	a.problemOf(t, alice, http.MethodGet, "/api/v1/deliveries?cursor=-1", nil, http.StatusBadRequest)

	byStatus := map[string]int64{}
	a.mustCall(t, alice, http.MethodGet, "/api/v1/deliveries", nil, http.StatusOK, &page)
	for _, d := range page.Items {
		byStatus[d.Status] = d.ID
	}
	var bobPage deliveriesJSON
	a.mustCall(t, bob, http.MethodGet, "/api/v1/deliveries", nil, http.StatusOK, &bobPage)
	if len(bobPage.Items) != 0 {
		t.Fatal("bob sees alice's deliveries")
	}
	a.mustCall(t, bob, http.MethodPost, fmt.Sprintf("/api/v1/deliveries/%d/retry", byStatus["dead"]), nil, http.StatusNotFound, nil)
	for _, s := range []string{"sent", "pending"} {
		a.mustCall(t, alice, http.MethodPost, fmt.Sprintf("/api/v1/deliveries/%d/retry", byStatus[s]), nil, http.StatusConflict, nil)
	}
	for _, s := range []string{"dead", "failed"} {
		var d struct {
			Status   string `json:"status"`
			Attempts int    `json:"attempts"`
		}
		a.mustCall(t, alice, http.MethodPost, fmt.Sprintf("/api/v1/deliveries/%d/retry", byStatus[s]), nil, http.StatusOK, &d)
		if d.Status != "pending" || d.Attempts != 2 {
			t.Fatalf("retry %s: %+v", s, d)
		}
	}
	if n := a.countRows(t, `SELECT count(*) FROM audit_log WHERE action = 'delivery.retried' AND after->>'status' = 'pending' AND before->>'channel_id' = $1`, hook.ID); n != 2 {
		t.Fatalf("retries audited %d times", n)
	}
	var page2 auditPage
	a.mustCall(t, alice, http.MethodGet, "/api/v1/audit?target_type=delivery", nil, http.StatusOK, &page2)
	if len(page2.Items) != 2 {
		t.Fatalf("alice's audit view: %d delivery entries", len(page2.Items))
	}
}

// A preview per pause in typing is fine; a loop of them is refused per user.
func TestRulePreviewRateLimit(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	codes := map[int]int{}
	for range 8 {
		code, _ := a.call(t, alice, http.MethodPost, "/api/v1/rules/preview", map[string]any{"filter": ""})
		codes[code]++
	}
	if codes[http.StatusOK] < 5 || codes[http.StatusTooManyRequests] == 0 {
		t.Fatalf("8 previews in a burst: %v", codes)
	}
	a.mustCall(t, bob, http.MethodPost, "/api/v1/rules/preview", map[string]any{"filter": ""}, http.StatusOK, nil)
}

// A revoked share hides what it brought: inbox items, deliveries and the rule's watch name.
func TestRevokedShareHides(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	var w watchJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches", sampleWatches[0], http.StatusCreated, &w)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/watches/"+w.ID+"/shares", map[string]any{"email": bob.email, "permission": "ro"}, http.StatusCreated, nil)
	var hook channelJSON
	a.mustCall(t, bob, http.MethodPost, "/api/v1/channels", map[string]any{"name": "Hook", "type": "webhook", "config": map[string]any{"url": "https://hooks.example/a"}}, http.StatusCreated, &hook)
	var r ruleJSON
	a.mustCall(t, bob, http.MethodPost, "/api/v1/rules", map[string]any{"name": "Theirs", "watch_id": w.ID, "channel_ids": []string{hook.ID}}, http.StatusCreated, &r)
	if r.WatchName == nil || *r.WatchName != w.Name {
		t.Fatalf("rule: %+v", r)
	}
	a.exec(t, `INSERT INTO events (watch_id, type, payload, dedup_key) VALUES ($1, 'release.published', '{}', 'k')`, w.ID)
	a.exec(t, `INSERT INTO inbox_items (user_id, event_id) SELECT $1, id FROM events`, bob.id)
	a.exec(t, `INSERT INTO deliveries (event_id, rule_id, channel_id) SELECT id, $1, $2 FROM events`, r.ID, hook.ID)
	visible := func() (int, int, int) {
		var inbox inboxJSON
		var deliveries deliveriesJSON
		var count struct{ Unread int }
		a.mustCall(t, bob, http.MethodGet, "/api/v1/inbox", nil, http.StatusOK, &inbox)
		a.mustCall(t, bob, http.MethodGet, "/api/v1/deliveries", nil, http.StatusOK, &deliveries)
		a.mustCall(t, bob, http.MethodGet, "/api/v1/inbox/count", nil, http.StatusOK, &count)
		return len(inbox.Items), len(deliveries.Items), count.Unread
	}
	if i, d, c := visible(); i != 1 || d != 1 || c != 1 {
		t.Fatalf("shared: inbox %d, deliveries %d, unread %d", i, d, c)
	}
	a.mustCall(t, alice, http.MethodDelete, "/api/v1/watches/"+w.ID+"/shares/"+bob.id, nil, http.StatusNoContent, nil)
	if i, d, c := visible(); i != 0 || d != 0 || c != 0 {
		t.Fatalf("revoked: inbox %d, deliveries %d, unread %d", i, d, c)
	}
	a.mustCall(t, bob, http.MethodGet, "/api/v1/rules/"+r.ID, nil, http.StatusOK, &r)
	if r.WatchID == nil || r.WatchName != nil {
		t.Fatalf("rule after revoke: %+v", r)
	}
}

// One user holds at most 50 channels and 100 rules; the next create is a 409, others are unaffected.
func TestPerUserCaps(t *testing.T) {
	a := newDevApp(t)
	alice, bob := a.signIn(t, "alice"), a.signIn(t, "bob")
	a.exec(t, `INSERT INTO channels (owner_id, name, type, config) SELECT $1, 'c' || g, 'webhook', '{"url":"https://h.example/"}' FROM generate_series(1, 49) g`, alice.id)
	a.exec(t, `INSERT INTO rules (owner_id, name) SELECT $1, 'r' || g FROM generate_series(1, 99) g`, alice.id)
	hook := map[string]any{"name": "last", "type": "webhook", "config": map[string]any{"url": "https://h.example/"}}
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", hook, http.StatusCreated, nil)
	a.mustCall(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "last"}, http.StatusCreated, nil)
	hook["name"] = "one too many"
	if p := a.problemOf(t, alice, http.MethodPost, "/api/v1/channels", hook, http.StatusConflict); !strings.Contains(p.Detail, "50 channels") {
		t.Fatalf("channel cap: %+v", p)
	}
	if p := a.problemOf(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": "one too many"}, http.StatusConflict); !strings.Contains(p.Detail, "100 rules") {
		t.Fatalf("rule cap: %+v", p)
	}
	a.mustCall(t, bob, http.MethodPost, "/api/v1/channels", hook, http.StatusCreated, nil)
	a.mustCall(t, bob, http.MethodPost, "/api/v1/rules", map[string]any{"name": "mine"}, http.StatusCreated, nil)
}

// Two tests at once on two channels of one user: the user lock lets exactly one through.
func TestChannelTestConcurrent(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	var ids [2]string
	for i := range ids {
		var c channelJSON
		a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": fmt.Sprint("h", i), "type": "webhook", "config": map[string]any{"url": srv.URL}}, http.StatusCreated, &c)
		ids[i] = c.ID
	}
	for round := range 5 {
		a.exec(t, `ALTER TABLE audit_log DISABLE TRIGGER audit_log_append_only`)
		a.exec(t, `DELETE FROM audit_log WHERE action = 'channel.tested'`)
		a.exec(t, `ALTER TABLE audit_log ENABLE TRIGGER audit_log_append_only`)
		codes := make([]int, 2)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, id := range ids {
			wg.Go(func() {
				<-start
				codes[i], _ = a.call(t, alice, http.MethodPost, "/api/v1/channels/"+id+"/test", nil)
			})
		}
		close(start)
		wg.Wait()
		slices.Sort(codes)
		if codes[0] != http.StatusOK || codes[1] != http.StatusTooManyRequests {
			t.Fatalf("round %d: %v, want one 200 and one 429", round, codes)
		}
	}
}

// The rule editor reads the allowlist from the server, the same one Compile enforces.
func TestFilterReference(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	var got filter.Ref
	a.mustCall(t, alice, http.MethodGet, "/api/v1/filters/reference", nil, http.StatusOK, &got)
	if !reflect.DeepEqual(got, filter.Reference()) {
		t.Fatalf("reference: %+v", got)
	}
}

// Channel tests and rule creates of one user run side by side: one lock order (user row first), so no
// deadlock and no 500, only 200, 201 or 429.
func TestChannelTestVersusRuleCreate(t *testing.T) {
	a := newDevApp(t)
	alice := a.signIn(t, "alice")
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	var hook channelJSON
	a.mustCall(t, alice, http.MethodPost, "/api/v1/channels", map[string]any{"name": "h", "type": "webhook", "config": map[string]any{"url": srv.URL}}, http.StatusCreated, &hook)
	var mu sync.Mutex
	codes := map[int]int{}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			var code int
			if i%2 == 0 {
				code, _ = a.call(t, alice, http.MethodPost, "/api/v1/channels/"+hook.ID+"/test", nil)
			} else {
				code, _ = a.call(t, alice, http.MethodPost, "/api/v1/rules", map[string]any{"name": fmt.Sprint("r", i), "channel_ids": []string{hook.ID}})
			}
			mu.Lock()
			codes[code]++
			mu.Unlock()
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("requests hung: a lock cycle")
	}
	if codes[http.StatusCreated] != 10 || codes[http.StatusOK]+codes[http.StatusTooManyRequests] != 10 || codes[http.StatusOK] < 1 {
		t.Fatalf("codes: %v", codes)
	}
}
