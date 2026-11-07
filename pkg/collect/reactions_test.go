package collect

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gedour/gedour/internal/testupstream"
)

// 011-AC12: with the box on, an entry from one inline or conversation comment carries its counts; a review body, and
// an inline comment standing for others of its review, carry none. With the box off, no entry does.
func TestNormalizeCommentsReactions(t *testing.T) {
	const heart = `"reactions":{"url":"x","total_count":1,"+1":0,"-1":0,"laugh":0,"hooray":0,"confused":0,"heart":1,"rocket":0,"eyes":0}`
	reviews := `[{"id":1,"user":{"login":"r"},"body":"overall fine","state":"COMMENTED","html_url":"r1","submitted_at":"2026-11-05T11:01:00Z",` + heart + `},
		{"id":2,"user":{"login":"q"},"body":"","state":"COMMENTED","html_url":"r2","submitted_at":"2026-11-05T12:05:00Z"},
		{"id":3,"user":{"login":"s"},"body":"","state":"COMMENTED","html_url":"r3","submitted_at":"2026-11-05T12:05:00Z"},
		{"id":4,"user":{"login":"t"},"body":"lgtm","state":"APPROVED","html_url":"r4","submitted_at":"2026-11-05T12:06:00Z",` + heart + `}]`
	inline := `[{"id":10,"pull_request_review_id":1,"user":{"login":"r"},"body":"under the body","html_url":"c10","created_at":"2026-11-05T11:00:30Z",` + heart + `},
		{"id":11,"pull_request_review_id":2,"user":{"login":"q"},"body":"first","html_url":"c11","created_at":"2026-11-05T12:01:00Z",` + heart + `},
		{"id":12,"pull_request_review_id":2,"user":{"login":"q"},"body":"second","html_url":"c12","created_at":"2026-11-05T12:02:00Z",` + heart + `},
		{"id":14,"pull_request_review_id":3,"user":{"login":"s"},"body":"a reply","html_url":"c14","created_at":"2026-11-05T12:04:00Z",` + heart + `}]`
	issue := `[{"user":{"login":"a"},"body":"hi","html_url":"i1","created_at":"2026-11-05T13:00:00Z",` + heart + `}]`
	for _, on := range []bool{true, false} {
		got, err := normalizeComments([]byte(reviews), []byte(inline), []byte(issue), on)
		if err != nil {
			t.Fatal(err)
		}
		var tracked []string
		for _, c := range got {
			if c.Reactions != nil {
				if c.Reactions.Heart != 1 {
					t.Fatalf("%s counts %+v", c.URL, c.Reactions)
				}
				tracked = append(tracked, c.URL)
			}
		}
		want := []string{"c14", "i1"}
		if !on {
			want = nil
		}
		if len(got) != 5 || !slices.Equal(tracked, want) {
			t.Fatalf("box %v: %d entries, tracked %q, want %q", on, len(got), tracked, want)
		}
	}
}

// 011-AC13: a rise per target emits once with the rises alone; a baseline, a new comment, a drop or no counts emit nothing.
func TestReactedEvents(t *testing.T) {
	r := func(plus, heart, eyes int) *Reactions { return &Reactions{PlusOne: plus, Heart: heart, Eyes: eyes} }
	c := func(u string, x *Reactions) PRComment { return PRComment{URL: u, Reactions: x} }
	const counts = `{"+1":%d,"-1":0,"laugh":0,"hooray":0,"confused":0,"heart":%d,"rocket":0,"eyes":%d}`
	for _, tt := range []struct {
		name       string
		old, cur   *Reactions
		oldC, curC []PRComment
		want       []string
	}{
		{"unchanged", r(1, 1, 0), r(1, 1, 0), []PRComment{c("u1", r(0, 0, 0))}, []PRComment{c("u1", r(0, 0, 0))}, []string{}},
		{"box just checked", nil, r(0, 2, 0), []PRComment{c("u1", nil)}, []PRComment{c("u1", r(1, 0, 0))}, []string{}},
		{"box unchecked", r(0, 2, 0), nil, []PRComment{c("u1", r(1, 0, 0))}, []PRComment{c("u1", nil)}, []string{}},
		{"new comment is a baseline", r(0, 0, 0), r(0, 0, 0), []PRComment{}, []PRComment{c("u1", r(3, 0, 0))}, []string{}},
		{"drop only", r(2, 2, 0), r(1, 0, 0), []PRComment{c("u1", r(1, 0, 0))}, []PRComment{c("u1", r(0, 0, 0))}, []string{}},
		{"rise and drop", r(2, 0, 1), r(1, 2, 3), nil, nil, []string{
			`issue.reacted x>` + fmt.Sprintf(counts, 1, 2, 3) + ` {"added":{"eyes":2,"heart":2},"counts":` + fmt.Sprintf(counts, 1, 2, 3) + `,"number":12,"target":"item","title":"T","url":"x"}`}},
		{"item and comment", r(0, 0, 0), r(0, 2, 0), []PRComment{c("u1", r(0, 0, 0)), c("u2", r(0, 0, 0))}, []PRComment{c("u1", r(1, 0, 0)), c("u2", r(0, 0, 0))}, []string{
			`issue.reacted x>` + fmt.Sprintf(counts, 0, 2, 0) + ` {"added":{"heart":2},"counts":` + fmt.Sprintf(counts, 0, 2, 0) + `,"number":12,"target":"item","title":"T","url":"x"}`,
			`issue.reacted u1>` + fmt.Sprintf(counts, 1, 0, 0) + ` {"added":{"+1":1},"counts":` + fmt.Sprintf(counts, 1, 0, 0) + `,"number":12,"target":"comment","title":"T","url":"u1"}`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			check(t, summary(t, reactedEvents("issue.reacted", 12, "T", "x", tt.old, tt.cur, tt.oldC, tt.curC)), tt.want)
		})
	}
}

// recorder counts the fake upstream's requests by path.
type recorder struct {
	mu    sync.Mutex
	paths []string
	h     http.Handler
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.paths = append(r.paths, req.URL.Path)
	r.mu.Unlock()
	r.h.ServeHTTP(w, req)
}

// stripReactions drops every "reactions" key of a state, to compare a box-on state with a box-off one.
func stripReactions(t *testing.T, b []byte) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "reactions")
	for _, c := range m["comments"].([]any) {
		delete(c.(map[string]any), "reactions")
	}
	out, _ := json.Marshal(m)
	return string(out)
}

// 011-AC11, 011-AC12, 011-AC14: per kind, a→b against the fake upstream. Box off: no counts stored, no reacted
// event, the requests of before 011. Box on: the same state plus counts, one more request on github_pr only, and
// the item heart +2 and comment +1 events.
func TestReactionsAgainstUpstream(t *testing.T) {
	const counts = `{"+1":%d,"-1":0,"laugh":0,"hooray":0,"confused":0,"heart":%d,"rocket":0,"eyes":0}`
	for _, tt := range []struct {
		kind, number, title, item, comment string
		heart, plus                        int // counts in b
		requests                           int // per box-off run, as before 011
		extra                              []string
	}{
		{"github_issue", "9400", "`gh run watch` exits 0 when the run is cancelled", "https://github.com/cli/cli/issues/9400",
			"https://github.com/cli/cli/issues/9400#issuecomment-3600001", 3, 3, 2, nil},
		{"github_pr", "14925", "Preserve existing SSH connection commands", "https://github.com/cli/cli/pull/14925",
			"https://github.com/cli/cli/pull/14925#issuecomment-3500001", 2, 1, 6, []string{"/repos/cli/cli/issues/14925"}},
		{"github_discussion", "9507", "How do I set a default repo per directory?", "https://github.com/cli/cli/discussions/9507",
			"https://github.com/cli/cli/discussions/9507#discussioncomment-4100001", 2, 1, 2, nil},
	} {
		t.Run(tt.kind, func(t *testing.T) {
			run := func(on bool) (b []byte, events []Event, paths [2][]string) {
				up := testupstream.New()
				rec := &recorder{h: up}
				srv := httptest.NewServer(rec)
				defer srv.Close()
				n, _ := strconv.Atoi(tt.number)
				cfg := map[string]any{"repo": "cli/cli", "number": n, "api_url": srv.URL}
				if on {
					cfg["reactions"] = true
				}
				c := Collectors()[tt.kind]
				a, err := c.Collect(t.Context(), fetcher(t), cfgJSON(t, cfg), nil)
				if err != nil {
					t.Fatal(err)
				}
				paths[0], rec.paths = rec.paths, nil
				up.SetFixture("b")
				if b, err = c.Collect(t.Context(), fetcher(t), cfgJSON(t, cfg), nil); err != nil {
					t.Fatal(err)
				}
				if events, err = c.Diff(a, b); err != nil {
					t.Fatal(err)
				}
				paths[1] = rec.paths
				return b, events, paths
			}
			offState, offEvents, offPaths := run(false)
			onState, onEvents, onPaths := run(true)
			if strings.Contains(string(offState), "reactions") || stripReactions(t, onState) != stripReactions(t, offState) {
				t.Fatalf("box-off state %s\nbox-on state %s", offState, onState)
			}
			var reacted []Event
			for _, e := range onEvents {
				if strings.HasSuffix(e.Type, ".reacted") {
					reacted = append(reacted, e)
				} else if !slices.ContainsFunc(offEvents, func(o Event) bool { return reflect.DeepEqual(o, e) }) {
					t.Fatalf("box on changed event %+v", e)
				}
			}
			if len(onEvents)-len(reacted) != len(offEvents) {
				t.Fatalf("box on: %d events, %d reacted, box off %d", len(onEvents), len(reacted), len(offEvents))
			}
			// Each run makes the box-off requests, plus the extra ones.
			for i := range onPaths {
				if len(offPaths[i]) != tt.requests || !slices.Equal(onPaths[i], append(slices.Clone(offPaths[i]), tt.extra...)) {
					t.Fatalf("run %d requests on %q, off %q", i, onPaths[i], offPaths[i])
				}
			}
			typ := map[string]string{"github_issue": "issue", "github_pr": "pr", "github_discussion": "discussion"}[tt.kind] + ".reacted"
			item, comment := fmt.Sprintf(counts, 0, tt.heart), fmt.Sprintf(counts, tt.plus, 0)
			base := `"number":` + tt.number + `,"target":"%s","title":` + string(must(json.Marshal(tt.title))) + `,"url":"%s"}`
			check(t, summary(t, reacted), []string{
				typ + " " + tt.item + ">" + item + ` {"added":{"heart":2},"counts":` + item + `,` + fmt.Sprintf(base, "item", tt.item),
				typ + " " + tt.comment + ">" + comment + ` {"added":{"+1":1},"counts":` + comment + `,` + fmt.Sprintf(base, "comment", tt.comment),
			})
		})
	}
}

func must(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}
