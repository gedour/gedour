package route

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/gedour/gedour/pkg/notify"
)

var signedRe = regexp.MustCompile(`https://gedour\.test/r#([^"]+)"`)

// openKey is a read-on-open key for these tests.
func openKey(t *testing.T) []byte {
	t.Helper()
	k, err := notify.DeriveOpenKey(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// signedEvents is the event ids text links to through signed links of owner, failing on a link that does not verify.
func signedEvents(t *testing.T, s *notify.Sender, text, owner string) []int64 {
	t.Helper()
	var ids []int64
	for _, m := range signedRe.FindAllStringSubmatch(text, -1) {
		id, o, ok := s.VerifyOpen(m[1])
		if !ok || o != owner {
			t.Fatalf("link %q: %d %q %v, want owner %s", m[1], id, o, ok, owner)
		}
		ids = append(ids, id)
	}
	return ids
}

func (e *env) withURLs(t *testing.T) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE events SET payload = payload || jsonb_build_object('url', 'https://x.test/e/' || id)`); err != nil {
		t.Fatal(err)
	}
}

// 021-AC2, 021-AC1: single, grouped and digest messages of a channel with read_on_open link each event through
// a signed link bound to the channel owner (loadChannel's owner_id); the same messages without it are unchanged.
func TestReadOnOpenDeliveries(t *testing.T) {
	e := newDigestEnv(t)
	on, off := e.tgChannel(t, e.alice), e.tgChannel(t, e.alice)
	if _, err := e.pool.Exec(context.Background(), `UPDATE channels SET config = config || '{"chat_id": "-2", "read_on_open": true}' WHERE id = $1`, on); err != nil {
		t.Fatal(err)
	}
	w := e.namedWatch(t, "org/alpha")
	e.rule(t, e.alice, &w, "", on, off)
	d := e.deliverer(t, e.pool)
	d.sender.OpenKey = openKey(t)
	check := func(name string, events []int64) {
		t.Helper()
		got := map[string]string{}
		for _, m := range e.messages() {
			got[m.ChatID] = m.Text
		}
		e.mu.Lock()
		e.got = nil
		e.mu.Unlock()
		ids := signedEvents(t, d.sender, got["-2"], e.alice)
		if len(ids) != len(events) || strings.Contains(got["-2"], "x.test") {
			t.Fatalf("%s on: %v in %q, want %v", name, ids, got["-2"], events)
		}
		for i, id := range events {
			if ids[i] != id {
				t.Fatalf("%s on: %v, want %v", name, ids, events)
			}
		}
		// The same text with the upstream URLs in place of the signed links: only the link string differs.
		if back := signedRe.ReplaceAllStringFunc(got["-2"], func(s string) string {
			id, _, _ := d.sender.VerifyOpen(strings.TrimSuffix(s[len("https://gedour.test/r#"):], `"`))
			return "https://x.test/e/" + strconv.FormatInt(id, 10) + `"`
		}); back != got["-100123"] || strings.Contains(got["-100123"], "/r#") {
			t.Fatalf("%s: on %q, off %q", name, got["-2"], got["-100123"])
		}
	}

	single := e.poll(t, w, "v1")
	e.withURLs(t)
	routeAll(t, e.router())
	e.round(t, d)
	check("single", single)

	group := e.poll(t, w, "v2", "v3")
	e.withURLs(t)
	routeAll(t, e.router())
	e.round(t, d)
	check("group", group)

	w2 := e.namedWatch(t, "org/beta")
	e.digestRule(t, e.alice, &w2, "", on, off)
	digest := []int64{e.event(t, w2, "release.published", "v4"), e.event(t, w2, "release.published", "v5")}
	e.withURLs(t)
	routeAll(t, e.router())
	e.round(t, d)
	check("digest", digest)
}

// 021-AC2: a rule's live test carries the signed link of the real event, bound to the channel owner.
func TestReadOnOpenLiveTest(t *testing.T) {
	r := newRuleTestEnv(t)
	alice := r.user(t, "alice")
	w := r.watch(t, alice)
	tg := r.tgChannel(t, alice)
	if _, err := r.pool.Exec(context.Background(), `UPDATE channels SET config = config || '{"read_on_open": true}' WHERE id = $1`, tg); err != nil {
		t.Fatal(err)
	}
	rule := r.rule(t, alice, &w, `event.type == "none"`, tg)
	ev := r.event(t, w, "release.published", "v1.2.3")
	r.withURLs(t)
	routeAll(t, r.router())
	r.sender.PublicURL, r.sender.OpenKey = "https://gedour.test", openKey(t)
	if got := r.items(t, alice, rule, ev); !got[tg].OK {
		t.Fatalf("items %+v", got)
	}
	tgs := r.received("telegram")
	if len(tgs) != 1 {
		t.Fatalf("%d telegram messages", len(tgs))
	}
	text := tgText(t, tgs[0])
	if ids := signedEvents(t, r.sender, text, alice); len(ids) != 1 || ids[0] != ev || !strings.Contains(text, "[test] ") {
		t.Fatalf("live test %q", text)
	}
}
