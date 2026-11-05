package collect

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gedour/gedour/internal/testupstream"
)

// The stored JSON a payload goes through on its way to a filter.
func roundTrip(t *testing.T, p map[string]any) map[string]any {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// 004-AC1, 004-AC4: both kinds, from their fixtures: a release fires with its notes and ids, whatever
// mix of CVEs and breaking changes it states; one without notes has an empty body and an empty list.
func TestReleaseFixtureNotes(t *testing.T) {
	github, err := normalizeGitHubReleases(testupstream.Fixture("github_release", "b"), true)
	if err != nil {
		t.Fatal(err)
	}
	gitlab, err := normalizeGitLabReleases(testupstream.Fixture("gitlab_release", "b"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		state    ReleaseState
		tag      string
		cves     []string
		breaking bool
	}{
		{github, "v2.103.3", []string{"CVE-2099-1234", "CVE-2099-20871"}, false},
		{github, "v2.104.0-rc.1", []string{}, true},
		{github, "v2.105.0", []string{"CVE-2099-31337"}, true},
		{github, "v2.103.2", []string{}, false},
		{github, "v2.103.1", []string{}, false},
		{gitlab, "v19.3.5", []string{"CVE-2099-4242", "CVE-2099-50113"}, false},
		{gitlab, "v19.4.0", []string{}, true},
		{gitlab, "v19.3.8", []string{"CVE-2099-61205"}, true},
		{gitlab, "v19.3.6", []string{}, false},
		{gitlab, "v19.3.7", []string{}, false},
	} {
		// From nothing: every release of the state fires, those that stay put in the a to b diff too.
		events := diffReleases(ReleaseState{}, tt.state)
		WithNotes(events, tt.state.Notes)
		i := slices.IndexFunc(events, func(e Event) bool { return e.Subject == tt.tag })
		if i < 0 {
			t.Fatalf("%s: no event in %v", tt.tag, summary(t, events))
		}
		p := roundTrip(t, events[i].Payload)
		body, _ := p["body"].(string)
		got, ok := p["cves"].([]any)
		if !ok {
			t.Fatalf("%s: cves is %T after a JSON round trip", tt.tag, p["cves"])
		}
		ids := []string{}
		for _, id := range got {
			ids = append(ids, id.(string))
		}
		if !slices.Equal(ids, tt.cves) {
			t.Errorf("%s: cves %v, want %v", tt.tag, ids, tt.cves)
		}
		if b := strings.Contains(strings.ToLower(body), "breaking change"); b != tt.breaking {
			t.Errorf("%s: breaking change in body = %v", tt.tag, b)
		}
		if body != tt.state.Notes[tt.tag] {
			t.Errorf("%s: body %q", tt.tag, body)
		}
	}
	if github.Notes["v2.103.1"] != "" || gitlab.Notes["v19.3.7"] != "" {
		t.Fatal("the no-notes fixtures have notes")
	}
}

func TestParseNotes(t *testing.T) {
	for _, tt := range []struct {
		name, notes string
		cves        []string
	}{
		// 004-AC1
		{"order, case, duplicates", "cve-2026-2 then CVE-2026-1, Cve-2026-2 again (CVE-2026-3).", []string{"CVE-2026-2", "CVE-2026-1", "CVE-2026-3"}},
		// 004-AC3
		{"whole words only", "XCVE-2026-1 CVE-2026-1x CVE-2026 CVE-26-1 _CVE-2026-2 CVE-2026-3_", []string{}},
		{"word boundary on punctuation", "(CVE-2026-1), CVE-2026-2. [CVE-2026-3]\nCVE-2026-4-rc", []string{"CVE-2026-1", "CVE-2026-2", "CVE-2026-3", "CVE-2026-4"}},
		// The incremental scan reads boundaries as one pass would: next to the previous match, and past a refused one.
		{"adjacent", "CVE-2026-1,CVE-2026-2-CVE-2026-3 CVE-2026-4CVE-2026-5 CVE-2026-6", []string{"CVE-2026-1", "CVE-2026-2", "CVE-2026-3", "CVE-2026-6"}},
		{"after a word", "aCVE-2026-1 CVE-2026-1b CVE-2026-2", []string{"CVE-2026-2"}},
		{"after a long one", "CVE-2026-" + strings.Repeat("1", 40) + ",CVE-2026-2", []string{"CVE-2026-2"}},
		{"multibyte neighbours", "éCVE-2026-1é", []string{"CVE-2026-1"}},
		// 32 bytes pass, 33 do not.
		{"length bound", "CVE-2026-" + strings.Repeat("1", 23) + " CVE-2026-" + strings.Repeat("2", 24), []string{"CVE-2026-" + strings.Repeat("1", 23)}},
		// 004-AC4
		{"empty", "", []string{}},
		// PostgreSQL refuses NUL in jsonb: one stray byte would fail every run of the watch.
		{"NUL", "a\x00b CVE-2026-1", []string{"CVE-2026-1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body, cves := parseNotes(tt.notes)
			if !slices.Equal(cves, tt.cves) || cves == nil {
				t.Fatalf("cves %#v, want %v", cves, tt.cves)
			}
			if strings.Contains(body, "\x00") {
				t.Fatalf("body keeps a NUL: %q", body)
			}
		})
	}
}

// 004-AC2: the cut lands on a rune boundary, and an id past it still counts.
func TestParseNotesCut(t *testing.T) {
	for _, lead := range []string{"", "x", "xx"} {
		notes := lead + strings.Repeat("é€", 20_000) + " past the cut, cve-2026-99999"
		body, cves := parseNotes(notes)
		if len(body) > MaxNotes || len(body) < MaxNotes-3 || !utf8.ValidString(body) {
			t.Fatalf("lead %q: %d bytes, valid %v", lead, len(body), utf8.ValidString(body))
		}
		if !strings.HasPrefix(notes, body) || strings.Contains(body, "CVE") {
			t.Fatalf("lead %q: not a prefix of the notes", lead)
		}
		if !slices.Equal(cves, []string{"CVE-2026-99999"}) {
			t.Fatalf("lead %q: cves %v", lead, cves)
		}
	}
	short := strings.Repeat("a", MaxNotes)
	if body, _ := parseNotes(short); body != short {
		t.Fatal("notes of exactly the cap were cut")
	}
}

// 004-AC3: the first 200 distinct ids, duplicates not counted.
func TestParseNotesCap(t *testing.T) {
	var b strings.Builder
	for i := range 250 {
		id := fmt.Sprintf("CVE-2026-%d", i)
		b.WriteString(id + " " + id + " ")
	}
	_, cves := parseNotes(b.String())
	if len(cves) != maxCVEs || cves[0] != "CVE-2026-0" || cves[199] != "CVE-2026-199" {
		t.Fatalf("%d ids, last %.30s", len(cves), cves[len(cves)-1])
	}
}

// The scan restarts after each match: it finds what one pass over the whole text finds, on any neighbours.
func TestParseNotesScanMatchesOnePass(t *testing.T) {
	r := rand.New(rand.NewPCG(4, 2099)) //nolint:gosec // seeded on purpose: the same texts every run
	tokens := []string{"CVE-2026-1", "cve-2026-22", "CVE-2026-333", "CVE-", "2026-", "x", "_", "7", ",", " ", "-", "é", "\n", "."}
	for range 5000 {
		var b strings.Builder
		for range 1 + r.IntN(12) {
			b.WriteString(tokens[r.IntN(len(tokens))])
		}
		notes := b.String()
		want := []string{}
		for _, m := range cveID.FindAllString(notes, -1) {
			if id := strings.ToUpper(m); !slices.Contains(want, id) {
				want = append(want, id)
			}
		}
		if _, got := parseNotes(notes); !slices.Equal(got, want) {
			t.Fatalf("%q: %v, one pass finds %v", notes, got, want)
		}
	}
}

// An over-long id must not carry its size into the payload: one id of 9 MiB, or 20 of 5000 digits.
func TestLongIDsIgnored(t *testing.T) {
	var many strings.Builder
	for i := range 20 {
		fmt.Fprintf(&many, "CVE-2026-%d%s ", i, strings.Repeat("7", 5000))
	}
	for _, tt := range []struct{ name, notes string }{
		{"one huge", "CVE-2026-" + strings.Repeat("9", 9<<20) + " and CVE-2026-1"},
		{"many long", many.String() + "CVE-2026-1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := []Event{{Subject: "v1", Payload: map[string]any{"tag": "v1", "name": "One", "url": "https://x/v1"}}}
			WithNotes(e, map[string]string{"v1": tt.notes})
			if got := e[0].Payload["cves"].([]string); !slices.Equal(got, []string{"CVE-2026-1"}) {
				t.Fatalf("cves of %d", len(got))
			}
			b, err := json.Marshal(e[0].Payload)
			if err != nil {
				t.Fatal(err)
			}
			if len(b) > MaxNotes+1<<10 {
				t.Fatalf("a payload of %d bytes", len(b))
			}
		})
	}
}

// The scan stops at the cap without reading the rest, and a tail of duplicates costs little: the old
// FindAllString allocated 56 to 88 MB on these.
func TestParseNotesAllocations(t *testing.T) {
	const size = 10 << 20
	var capped strings.Builder
	for i := range 201 {
		fmt.Fprintf(&capped, "CVE-2026-%d ", i)
	}
	head := capped.String()
	for _, tt := range []struct {
		name, notes string
		limit       uint64
	}{
		{"past the cap", head + strings.Repeat("CVE-2026-1 ", (size-len(head))/11), 1 << 20},
		{"duplicates", strings.Repeat("CVE-2026-1 ", size/11), 40 << 20},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if raceEnabled && tt.name == "duplicates" {
				t.Skip("1.4 GB under the race detector; CI runs it without")
			}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, cves := parseNotes(tt.notes)
			runtime.ReadMemStats(&after)
			used := after.TotalAlloc - before.TotalAlloc
			t.Logf("%d ids, %d KiB allocated", len(cves), used>>10)
			if used > tt.limit {
				t.Fatalf("%d bytes allocated, past %d", used, tt.limit)
			}
		})
	}
}

// 004-AC6: the stored form holds no notes, so an edit of the notes alone leaves it, and its hash, as is.
func TestNotesStayOutOfTheState(t *testing.T) {
	for _, tt := range []struct {
		name string
		norm func(body string) (ReleaseState, error)
	}{
		{"github", func(body string) (ReleaseState, error) {
			return normalizeGitHubReleases([]byte(`[{"tag_name":"v1","published_at":"2026-01-01T00:00:00Z","body":`+body+`}]`), true)
		}},
		{"gitlab", func(body string) (ReleaseState, error) {
			return normalizeGitLabReleases([]byte(`[{"tag_name":"v1","released_at":"2026-01-01T00:00:00Z","description":` + body + `}]`))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, err := tt.norm(`"before, CVE-2026-1"`)
			if err != nil {
				t.Fatal(err)
			}
			b, err := tt.norm(`"after, a long edit"`)
			if err != nil {
				t.Fatal(err)
			}
			ja, _ := json.Marshal(a)
			jb, _ := json.Marshal(b)
			if string(ja) != string(jb) || strings.Contains(string(ja), "CVE") || strings.Contains(string(ja), "notes") {
				t.Fatalf("stored forms differ or hold the notes:\n%s\n%s", ja, jb)
			}
			if a.Notes["v1"] != "before, CVE-2026-1" {
				t.Fatalf("notes: %v", a.Notes)
			}
			if ev := diffReleases(a, b); len(ev) != 0 {
				t.Fatalf("an edit of the notes alone made %v", ev)
			}
		})
	}
}

// Other kinds' events stay as they are.
func TestWithNotesOnOtherKinds(t *testing.T) {
	e := []Event{{Subject: "x", Payload: map[string]any{}}}
	WithNotes(e, nil)
	if len(e[0].Payload) != 0 {
		t.Fatalf("payload %v", e[0].Payload)
	}
}
