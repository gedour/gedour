package route

import (
	"context"
	"encoding/json"
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

// 021-AC16, 021-AC17, 021-AC18: on Web Push, a single event, its live test and a one-event digest open the signed
// /r link bound to the channel owner, relative and without a public URL; title, body, tag and urgency stay as
// the channel without the option gets them. An event without a URL, a poll of two and a digest of two do not.
func TestReadOnOpenWebPush(t *testing.T) {
	r := newRuleTestEnv(t)
	r.sender.OpenKey = openKey(t)
	alice := r.user(t, "alice")
	on, off := r.channel(t, alice, false, r.device(t, alice, "on", false)), r.channel(t, alice, false, r.device(t, alice, "off", false))
	if _, err := r.pool.Exec(context.Background(), `UPDATE channels SET config = config || '{"read_on_open": true}' WHERE id = $1`, on); err != nil {
		t.Fatal(err)
	}
	ch, err := loadChannel(context.Background(), r.pool, r.box, on)
	if err != nil || ch.OwnerID != alice {
		t.Fatalf("channel %+v %v", ch, err)
	}
	last := func(dev string) (notify.PushMessage, string) {
		t.Helper()
		got := r.received("push-" + dev)
		var m notify.PushMessage
		if len(got) == 0 || json.Unmarshal([]byte(got[len(got)-1].Body), &m) != nil {
			t.Fatalf("%s: %+v", dev, got)
		}
		return m, got[len(got)-1].Header["Urgency"]
	}
	// check compares the last pushes: off opens offURL; on the signed link of event, offURL for event 0.
	check := func(name string, event int64, offURL string) {
		t.Helper()
		m, mu := last("on")
		o, ou := last("off")
		if event == 0 {
			if m.URL != offURL {
				t.Fatalf("%s: on opens %q, want %q", name, m.URL, offURL)
			}
		} else if id, owner, ok := r.sender.VerifyOpen(strings.TrimPrefix(m.URL, "/r#")); !strings.HasPrefix(m.URL, "/r#") || !ok || id != event || owner != alice {
			t.Fatalf("%s: on opens %q: %d %q %v", name, m.URL, id, owner, ok)
		}
		// Poll and digest tags name their channel or lead delivery: only an event's tag is shared.
		sameTag := m.Tag == o.Tag || !strings.HasPrefix(m.Tag, "gedour-event-")
		if o.URL != offURL || m.Title != o.Title || m.Body != o.Body || m.Silent != o.Silent || !sameTag || mu != ou {
			t.Fatalf("%s: on %+v %s, off %+v %s", name, m, mu, o, ou)
		}
	}
	w := r.watch(t, alice)
	rule := r.rule(t, alice, &w, "", on, off)
	d := r.deliverer()

	ev := r.event(t, w, "release.published", "v1")
	r.withURLs(t)
	routeAll(t, r.router())
	r.once(t, d)
	check("single", ev, "/inbox?event="+strconv.FormatInt(ev, 10))

	bare := r.event(t, w, "release.published", "v2")
	routeAll(t, r.router())
	r.once(t, d)
	check("no url", 0, "/inbox?event="+strconv.FormatInt(bare, 10))

	r.poll(t, w, "v3", "v4")
	r.withURLs(t)
	routeAll(t, r.router())
	r.once(t, d)
	check("poll of two", 0, "/inbox")

	if got := r.items(t, alice, rule, ev); !got[on].OK || !got[off].OK {
		t.Fatalf("live test %+v", got)
	}
	check("live test", ev, "/inbox?event="+strconv.FormatInt(ev, 10))

	w2 := r.watch(t, alice)
	r.digestRule(t, alice, &w2, "", on, off)
	one := r.event(t, w2, "release.published", "d1")
	r.withURLs(t)
	routeAll(t, r.router())
	r.makeDue(t)
	r.once(t, d)
	check("digest of one", one, "/inbox")

	r.event(t, w2, "release.published", "d2")
	r.event(t, w2, "release.published", "d3")
	r.withURLs(t)
	routeAll(t, r.router())
	r.makeDue(t)
	r.once(t, d)
	check("digest of two", 0, "/inbox")
}
