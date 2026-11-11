package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/gedour/gedour/internal/testupstream"
	"github.com/gedour/gedour/pkg/audit"
	"github.com/gedour/gedour/pkg/netguard"
	"github.com/gedour/gedour/pkg/notify"
	"github.com/gedour/gedour/pkg/script"
	"github.com/gedour/gedour/pkg/testpg"
	"github.com/gedour/gedour/pkg/wake"
)

// ruleTestEnv is a push env whose fake upstream also takes Telegram and webhook messages, with a service and
// a deliverer sending through it.
type ruleTestEnv struct {
	*pushEnv
	sender *notify.Sender
	runner *script.Runner
}

func newRuleTestEnv(t *testing.T) *ruleTestEnv {
	t.Helper()
	r := &ruleTestEnv{pushEnv: newPushEnv(t), sender: pushSender(t)}
	r.sender.TelegramURL = r.url + "/tg"
	g, _ := netguard.New([]string{"127.0.0.0/8"})
	r.runner = testRunner(t, r.sender.Client, g)
	return r
}

func (r *ruleTestEnv) service(viewer string) *Service {
	g, _ := netguard.New([]string{"127.0.0.0/8"})
	s := New(r.pool, zap.NewNop(), r.box, r.sender, r.runner, g, []string{"127.0.0.1"}, r.m)
	s.viewer = func(context.Context) (string, bool) { return viewer, true }
	return s
}

func (r *ruleTestEnv) deliverer() *Deliverer {
	return NewDeliverer(r.pool, zap.NewNop(), r.box, r.sender, r.runner, r.m, wake.NewLoop("deliveries", 10*time.Millisecond, nil, nil), 4, time.Second)
}

// hook is a webhook channel posting to the fake's /_hooks/<name>.
func (r *ruleTestEnv) hook(t *testing.T, owner, name string) string {
	t.Helper()
	return r.webhook(t, owner, r.url+"/_hooks/"+name)
}

// received lists the bodies the fake got at endpoint.
func (r *ruleTestEnv) received(endpoint string) []testupstream.Received {
	var out []testupstream.Received
	for _, x := range r.up.Received() {
		if x.Endpoint == endpoint {
			out = append(out, x)
		}
	}
	return out
}

// pastTests moves every test out of the rate limit window.
func (r *ruleTestEnv) pastTests(t *testing.T) {
	t.Helper()
	for _, sql := range []string{`ALTER TABLE audit_log DISABLE TRIGGER audit_log_append_only`,
		`UPDATE audit_log SET at = at - interval '1 minute' WHERE action IN ('channel.tested', 'rule.tested')`,
		`ALTER TABLE audit_log ENABLE TRIGGER audit_log_append_only`} {
		if _, err := r.pool.Exec(context.Background(), sql); err != nil {
			t.Fatal(err)
		}
	}
}

func testRuleCall(ctx context.Context, s *Service, rule string, event int64) (*testRuleOutput, error) {
	in := &testRuleInput{ID: rule}
	in.Body.EventID = event
	return s.testRule(ctx, in)
}

func status(err error) int {
	var se huma.StatusError
	if errors.As(err, &se) {
		return se.GetStatus()
	}
	return 0
}

// items runs viewer's test of rule on event, which must succeed, and returns its items by channel.
func (r *ruleTestEnv) items(t *testing.T, viewer, rule string, event int64) map[string]RuleTestItem {
	t.Helper()
	out, err := testRuleCall(context.Background(), r.service(viewer), rule, event)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]RuleTestItem{}
	for _, it := range out.Body.Items {
		m[it.ChannelID] = it
	}
	return m
}

func tgText(t *testing.T, x testupstream.Received) string {
	t.Helper()
	var b struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(x.Body), &b); err != nil {
		t.Fatal(err)
	}
	return b.Text
}

// 013-AC1, 013-AC5, 013-AC11: both channels get the real message of the event with a "[test] " summary, the
// webhook body says test: true; nothing is queued, the inbox is untouched and one rule.tested row is written.
// A real delivery afterwards carries neither the prefix nor a test key.
func TestRuleTestSends(t *testing.T) {
	r := newRuleTestEnv(t)
	alice := r.user(t, "alice")
	w := r.watch(t, alice)
	hook, tg := r.hook(t, alice, "a"), r.tgChannel(t, alice)
	rule := r.rule(t, alice, &w, `event.type == "none"`, hook, tg)
	var name string
	r.scalar(t, &name, `SELECT name FROM rules WHERE id = $1`, rule)
	ev := r.event(t, w, "release.published", "v1.2.3")
	// An unrelated routed event leaves a delivery and an inbox item the test must not touch (013-AC5).
	r.rule(t, alice, &w, "", hook)
	routeAll(t, r.router())
	inboxBefore, deliveriesBefore := r.inboxState(t), r.count(t, `SELECT count(*) FROM deliveries`)

	got := r.items(t, alice, rule, ev)
	if len(got) != 2 || !got[hook].OK || !got[tg].OK || got[hook].Error != "" || got[tg].Accepted != nil {
		t.Fatalf("items %+v", got)
	}
	hooks, tgs := r.received("a"), r.received("telegram")
	if len(hooks) != 1 || len(tgs) != 1 {
		t.Fatalf("received %d webhooks, %d telegram", len(hooks), len(tgs))
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(hooks[0].Body), &body)
	e, _ := body["event"].(map[string]any)
	ru, _ := body["rule"].(map[string]any)
	if body["test"] != true || e["id"] != float64(ev) || ru["id"] != rule || ru["name"] != name || hooks[0].Header["X-Gedour-Event"] != "release.published" {
		t.Fatalf("webhook body %s", hooks[0].Body)
	}
	if text := tgText(t, tgs[0]); !strings.Contains(text, "cli/cli: [test] v1.2.3 published") {
		t.Fatalf("telegram %q", text)
	}
	if n := r.count(t, `SELECT count(*) FROM deliveries`); n != deliveriesBefore {
		t.Fatalf("deliveries %d, was %d", n, deliveriesBefore)
	}
	if after := r.inboxState(t); after != inboxBefore {
		t.Fatalf("inbox %s, was %s", after, inboxBefore)
	}
	if n := r.count(t, `SELECT count(*) FROM audit_log WHERE action = 'rule.tested' AND target_type = 'rule' AND target_id = $1
		AND actor_user_id = $2 AND (after->>'event_id')::bigint = $3`, rule, alice, ev); n != 1 || r.count(t, `SELECT count(*) FROM audit_log`) != 1 {
		t.Fatalf("%d rule.tested rows", n)
	}

	// 013-AC11: the routed delivery of the other rule.
	if _, err := r.deliverer().DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hooks = r.received("a"); len(hooks) != 2 || strings.Contains(hooks[1].Body, `"test"`) {
		t.Fatalf("real webhook %v", hooks)
	}
}

// inboxState is every inbox item and its state, to compare before and after.
func (r *ruleTestEnv) inboxState(t *testing.T) string {
	t.Helper()
	var s string
	r.scalar(t, &s, `SELECT COALESCE(json_agg(i ORDER BY i.id)::text, '[]') FROM inbox_items i`)
	if s == "[]" {
		t.Fatal("no inbox item to compare")
	}
	return s
}

// 013-AC2, 013-AC11: a script channel runs on the real event with the rule, a "[test] " summary and test: true,
// and the item carries its logs; the routed delivery's run sees no prefix and no test.
func TestRuleTestScript(t *testing.T) {
	r := newRuleTestEnv(t)
	alice := r.user(t, "alice")
	w := r.watch(t, alice)
	ch := r.scriptChannel(t, alice, `function main(event, ctx) {
		log(event.summary, event.payload.tag, event.type, watch.name, rule.id, rule.name, String(test), String(ctx.test), delivery_id);
	}`)
	rule := r.rule(t, alice, nil, "", ch)
	var name string
	r.scalar(t, &name, `SELECT name FROM rules WHERE id = $1`, rule)
	ev := r.event(t, w, "release.published", "v2.0.0")
	got := r.items(t, alice, rule, ev)
	want := fmt.Sprintf("[test] v2.0.0 published v2.0.0 release.published cli/cli %s %s true true 0", rule, name)
	if it := got[ch]; !it.OK || len(it.Logs) != 1 || it.Logs[0] != want {
		t.Fatalf("item %+v, want log %q", it, want)
	}
	routeAll(t, r.router())
	if _, err := r.deliverer().DeliverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var logs string
	r.scalar(t, &logs, `SELECT logs FROM deliveries WHERE channel_id = $1`, ch)
	if !strings.HasPrefix(logs, "v2.0.0 published v2.0.0 ") || !strings.Contains(logs, " undefined undefined ") {
		t.Fatalf("real run logs %q", logs)
	}
}

// 013-AC3: a Web Push channel gets the urgency a real delivery of the rule gets, and the item says how many
// devices accepted.
func TestRuleTestPushUrgency(t *testing.T) {
	r := newRuleTestEnv(t)
	for _, tt := range []struct{ priority, want string }{
		{priorityHigh, notify.UrgencyHigh},
		{priorityNormal, notify.UrgencyNormal},
	} {
		u := r.user(t, "u-"+tt.priority)
		w := r.watch(t, u)
		r.device(t, u, "dev-"+tt.priority, false)
		ch := r.channel(t, u, false)
		rule := r.rule(t, u, &w, "", ch)
		r.setPriority(t, rule, tt.priority)
		ev := r.event(t, w, "release.published", "v3")
		got := r.items(t, u, rule, ev)
		if it := got[ch]; !it.OK || it.Accepted == nil || *it.Accepted != 1 {
			t.Fatalf("%s: item %+v", tt.priority, it)
		}
		pushes := r.received("push-dev-" + tt.priority)
		var m notify.PushMessage
		if len(pushes) == 1 {
			_ = json.Unmarshal([]byte(pushes[0].Body), &m)
		}
		if len(pushes) != 1 || pushes[0].Header["Urgency"] != tt.want || m.Title != "cli/cli: [test] v3 published" || m.URL != fmt.Sprintf("/inbox?event=%d", ev) {
			t.Fatalf("%s: pushes %+v", tt.priority, pushes)
		}
		if n := r.count(t, `SELECT count(*) FROM delivery_push_sent`); n != 0 {
			t.Fatalf("%s: %d ledger rows", tt.priority, n)
		}
	}
}

// 013-AC4: a digest rule's test sends the single-event message now and queues nothing.
func TestRuleTestDigest(t *testing.T) {
	r := newRuleTestEnv(t)
	alice := r.user(t, "alice")
	w := r.watch(t, alice)
	tg := r.tgChannel(t, alice)
	rule := r.digestRule(t, alice, &w, "", tg)
	ev := r.event(t, w, "release.published", "v4")
	got := r.items(t, alice, rule, ev)
	tgs := r.received("telegram")
	if !got[tg].OK || len(tgs) != 1 || !strings.HasPrefix(tgText(t, tgs[0]), "<b>cli/cli: [test] v4 published</b>") {
		t.Fatalf("item %+v, telegram %v", got[tg], tgs)
	}
	if n := r.count(t, `SELECT count(*) FROM deliveries`); n != 0 {
		t.Fatalf("%d deliveries queued", n)
	}
}

// 013-AC6: a rule of another user, a missing event, an event the caller cannot read and one outside the
// rule's watch are all 404, and none is audited.
func TestRuleTestNotFound(t *testing.T) {
	r := newRuleTestEnv(t)
	alice, bob := r.user(t, "alice"), r.user(t, "bob")
	w, w2, bw := r.watch(t, alice), r.watch(t, alice), r.watch(t, bob)
	hook := r.hook(t, alice, "nf")
	onW, all := r.rule(t, alice, &w, "", hook), r.rule(t, alice, nil, "", hook)
	bobs := r.rule(t, bob, &bw, "", r.hook(t, bob, "nf-bob"))
	mine, other, bobEvent := r.event(t, w, "release.published", "v1"), r.event(t, w2, "release.published", "v1"), r.event(t, bw, "release.published", "v1")
	for _, tt := range []struct {
		name  string
		rule  string
		event int64
	}{
		{"foreign rule", bobs, bobEvent},
		{"foreign rule, own event", bobs, mine},
		{"missing event", onW, bobEvent + 1000},
		{"unreadable event", all, bobEvent},
		{"out of scope", onW, other},
	} {
		_, err := testRuleCall(context.Background(), r.service(alice), tt.rule, tt.event)
		if status(err) != http.StatusNotFound {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
	if n := r.count(t, `SELECT count(*) FROM audit_log`); n != 0 || len(r.received("nf")) != 0 {
		t.Fatalf("%d audit rows, %d sends", n, len(r.received("nf")))
	}
	// The same calls in scope succeed: the 404s above come from the scope, not the setup.
	for _, ev := range []int64{mine, other} {
		r.pastTests(t)
		if _, err := testRuleCall(context.Background(), r.service(alice), all, ev); err != nil {
			t.Fatalf("all-watches rule, event %d: %v", ev, err)
		}
	}
}

// 013-AC6, 014-AC1: a rule on [A, B] tests an event of A or B; one of C, an unlisted watch of the owner, is a 404,
// as is any event once the rule's list is emptied without all_watches.
func TestRuleTestMultiWatchScope(t *testing.T) {
	r := newRuleTestEnv(t)
	alice := r.user(t, "alice")
	a, b, c := r.watch(t, alice), r.watch(t, alice), r.watch(t, alice)
	rule := testpg.InsertRule(t, r.pool, testpg.Rule{Owner: alice, Watches: []string{a, b}})
	if _, err := r.pool.Exec(context.Background(), `INSERT INTO rule_channels (rule_id, channel_id) VALUES ($1, $2)`, rule, r.hook(t, alice, "mw")); err != nil {
		t.Fatal(err)
	}
	evA, evB, evC := r.event(t, a, "release.published", "v1"), r.event(t, b, "release.published", "v1"), r.event(t, c, "release.published", "v1")
	if _, err := testRuleCall(context.Background(), r.service(alice), rule, evC); status(err) != http.StatusNotFound {
		t.Fatalf("event of unlisted watch C: %v", err)
	}
	for _, ev := range []int64{evA, evB} {
		r.pastTests(t)
		if _, err := testRuleCall(context.Background(), r.service(alice), rule, ev); err != nil {
			t.Fatalf("listed event %d: %v", ev, err)
		}
	}
	if _, err := r.pool.Exec(context.Background(), `DELETE FROM rule_watches WHERE rule_id = $1`, rule); err != nil {
		t.Fatal(err)
	}
	r.pastTests(t)
	if _, err := testRuleCall(context.Background(), r.service(alice), rule, evA); status(err) != http.StatusNotFound {
		t.Fatalf("emptied list: %v", err)
	}
}

// 013-AC7: rule and channel tests share one 10 s interval per user, in both orders and when an admin acts as
// the user.
func TestRuleTestRateLimit(t *testing.T) {
	r := newRuleTestEnv(t)
	alice, admin := r.user(t, "alice"), r.user(t, "admin")
	w := r.watch(t, alice)
	hook := r.hook(t, alice, "rl")
	rule := r.rule(t, alice, &w, "", hook)
	ev := r.event(t, w, "release.published", "v1")
	s := r.service(alice)
	ruleTest := func(ctx context.Context) error { _, err := testRuleCall(ctx, s, rule, ev); return err }
	channelTest := func(ctx context.Context) error { _, err := s.testChannel(ctx, &idPath{ID: hook}); return err }
	acting := audit.WithActing(context.Background(), admin, alice)
	for _, tt := range []struct {
		name          string
		first, second func(context.Context) error
		firstCtx      context.Context
	}{
		{"rule then channel", ruleTest, channelTest, context.Background()},
		{"channel then rule", channelTest, ruleTest, context.Background()},
		{"rule then rule", ruleTest, ruleTest, context.Background()},
		{"acting rule then channel", ruleTest, channelTest, acting},
	} {
		r.pastTests(t)
		if err := tt.first(tt.firstCtx); err != nil {
			t.Fatalf("%s: first: %v", tt.name, err)
		}
		err := tt.second(context.Background())
		var he huma.HeadersError
		if status(err) != http.StatusTooManyRequests || !errors.As(err, &he) || he.GetHeaders().Get("Retry-After") == "" {
			t.Fatalf("%s: second: %v", tt.name, err)
		}
	}
}

// 013-AC8: a failing webhook and a script with no free sandbox fail their items; the other channel is sent.
func TestRuleTestPartialFailure(t *testing.T) {
	r := newRuleTestEnv(t)
	alice := r.user(t, "alice")
	w := r.watch(t, alice)
	down, ok := r.hook(t, alice, "fail-down"), r.hook(t, alice, "up")
	sc := r.scriptChannel(t, alice, `function main() { log("ran") }`)
	rule := r.rule(t, alice, &w, "", down, sc, ok)
	ev := r.event(t, w, "release.published", "v1")
	for {
		release, free := r.runner.TryReserve()
		if !free {
			break
		}
		t.Cleanup(release)
	}
	got := r.items(t, alice, rule, ev)
	if it := got[down]; it.OK || !strings.Contains(it.Error, "HTTP 500") {
		t.Errorf("down: %+v", it)
	}
	if it := got[sc]; it.OK || it.Error != script.ErrBusy.Error() || it.Logs != nil {
		t.Errorf("script: %+v", it)
	}
	if it := got[ok]; !it.OK || len(r.received("up")) != 1 {
		t.Errorf("up: %+v", it)
	}
}

// 013-AC9: a rule with no channel answers an empty list. Past its time bound, the channels left are not sent.
func TestRuleTestNoChannelAndTimeout(t *testing.T) {
	r := newRuleTestEnv(t)
	alice := r.user(t, "alice")
	w := r.watch(t, alice)
	ev := r.event(t, w, "release.published", "v1")
	out, err := testRuleCall(context.Background(), r.service(alice), r.rule(t, alice, &w, ""), ev)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(out.Body); string(b) != `{"items":[]}` {
		t.Fatalf("body %s", b)
	}
	r.pastTests(t)
	saved := pushTestTimeout
	pushTestTimeout = time.Nanosecond
	t.Cleanup(func() { pushTestTimeout = saved })
	hook := r.hook(t, alice, "late")
	got := r.items(t, alice, r.rule(t, alice, &w, "", hook), ev)
	if it := got[hook]; it.OK || it.Error != ruleTestLate || len(r.received("late")) != 0 {
		t.Fatalf("late: %+v", it)
	}
}

// 013-AC8: a channel gone between the commit and its send fails its item by name; a load error stays generic.
func TestRuleTestChannelLoadErrors(t *testing.T) {
	r := newRuleTestEnv(t)
	alice := r.user(t, "alice")
	s := r.service(alice)
	for id, want := range map[string]string{uuid.NewString(): ruleTestGone, "not-a-uuid": "internal error"} {
		if _, _, err := s.testSend(context.Background(), id, notify.Delivery{Test: true}); err == nil || err.Error() != want {
			t.Errorf("%s: %v, want %q", id, err, want)
		}
	}
}
