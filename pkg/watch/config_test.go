package watch

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
)

func obj(s string) map[string]any {
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		panic(err)
	}
	return m
}

func locations(errs []error) []string {
	var out []string
	for _, err := range errs {
		var d *huma.ErrorDetail
		if errors.As(err, &d) {
			out = append(out, d.Location)
		}
	}
	return out
}

// 001-AC4: each kind's config is checked field by field; bad shapes give errors located on the field.
func TestConfigValidation(t *testing.T) {
	v := validator{httpHosts: []string{"127.0.0.1"}}
	long := strings.Repeat("a", 257)
	tests := []struct {
		name, kind, in string
		want           string   // normalized JSON when valid
		errAt          []string // error locations when invalid
	}{
		{"github release defaults", KindGitHubRepo, `{"repo":"golang/go"}`,
			`{"api_url":"https://api.github.com","include_prereleases":false,"repo":"golang/go"}`, nil},
		{"github release enterprise", KindGitHubRepo, `{"repo":"o/r.js","include_prereleases":true,"api_url":"https://ghe.example/api/v3/"}`,
			`{"api_url":"https://ghe.example/api/v3","include_prereleases":true,"repo":"o/r.js"}`, nil},
		{"github repo without owner", KindGitHubRepo, `{"repo":"go"}`, "", []string{"body.config.repo"}},
		{"github repo with three parts", KindGitHubRepo, `{"repo":"a/b/c"}`, "", []string{"body.config.repo"}},
		{"github repo dotdot", KindGitHubRepo, `{"repo":"a/.."}`, "", []string{"body.config.repo"}},
		{"github missing repo", KindGitHubRepo, `{}`, "", []string{"body.config.repo"}},
		{"github unknown field", KindGitHubRepo, `{"repo":"a/b","token":"x","zz":1}`, "", []string{"body.config.token", "body.config.zz"}},
		// 008-AC4: the boxes default to off and an off box is left out, so a boxes-off config is stored as before 008.
		{"github repo boxes off", KindGitHubRepo, `{"repo":"a/b","new_issues":false,"new_prs":false}`,
			`{"api_url":"https://api.github.com","include_prereleases":false,"repo":"a/b"}`, nil},
		{"github repo new issues", KindGitHubRepo, `{"repo":"a/b","new_issues":true}`,
			`{"api_url":"https://api.github.com","include_prereleases":false,"new_issues":true,"repo":"a/b"}`, nil},
		{"github repo both boxes", KindGitHubRepo, `{"repo":"a/b","new_issues":true,"new_prs":true}`,
			`{"api_url":"https://api.github.com","include_prereleases":false,"new_issues":true,"new_prs":true,"repo":"a/b"}`, nil},
		{"github repo boxes not bool", KindGitHubRepo, `{"repo":"a/b","new_prs":"yes"}`, "", []string{"body.config.new_prs"}},
		{"github repo box number", KindGitHubRepo, `{"repo":"a/b","new_issues":1}`, "", []string{"body.config.new_issues"}},
		{"github pr has no boxes", KindGitHubPR, `{"repo":"a/b","number":1,"new_prs":true}`, "", []string{"body.config.new_prs"}},
		{"github prerelease not bool", KindGitHubRepo, `{"repo":"a/b","include_prereleases":"yes"}`, "", []string{"body.config.include_prereleases"}},
		{"github http api", KindGitHubRepo, `{"repo":"a/b","api_url":"http://ghe.example"}`, "", []string{"body.config.api_url"}},
		{"github http allowlisted", KindGitHubRepo, `{"repo":"a/b","api_url":"http://127.0.0.1:9999"}`,
			`{"api_url":"http://127.0.0.1:9999","include_prereleases":false,"repo":"a/b"}`, nil},
		{"github userinfo", KindGitHubRepo, `{"repo":"a/b","api_url":"https://u:p@ghe.example"}`, "", []string{"body.config.api_url"}},
		{"docker hub default port", KindOCITags, `{"registry":"docker.io:443","repository":"a/b"}`, `{"registry":"docker.io","repository":"a/b","semver_only":false}`, nil},
		{"docker hub alias default port", KindOCITags, `{"registry":"INDEX.docker.io:443","repository":"a/b"}`, `{"registry":"docker.io","repository":"a/b","semver_only":false}`, nil},
		{"docker hub other port", KindOCITags, `{"registry":"docker.io:8443","repository":"a/b"}`, `{"registry":"docker.io:8443","repository":"a/b","semver_only":false}`, nil},
		{"bare port", KindOCITags, `{"registry":":443","repository":"a/b"}`, "", []string{"body.config.registry"}},
		{"github empty query", KindGitHubRepo, `{"repo":"a/b","api_url":"https://ghe.example/x?"}`, "", []string{"body.config.api_url"}},
		{"gitlab empty fragment", KindGitLabRelease, `{"project":"a/b","base_url":"https://git.example/x#"}`, "", []string{"body.config.base_url"}},
		{"github query", KindGitHubRepo, `{"repo":"a/b","api_url":"https://ghe.example/?x=1"}`, "", []string{"body.config.api_url"}},
		{"github relative url", KindGitHubRepo, `{"repo":"a/b","api_url":"/api"}`, "", []string{"body.config.api_url"}},
		{"github ftp url", KindGitHubRepo, `{"repo":"a/b","api_url":"ftp://ghe.example"}`, "", []string{"body.config.api_url"}},
		{"github pr", KindGitHubPR, `{"repo":"golang/go","number":123}`,
			`{"api_url":"https://api.github.com","number":123,"repo":"golang/go"}`, nil},
		{"github pr zero", KindGitHubPR, `{"repo":"golang/go","number":0}`, "", []string{"body.config.number"}},
		{"github pr fraction", KindGitHubPR, `{"repo":"golang/go","number":1.5}`, "", []string{"body.config.number"}},
		{"github pr missing number and bad repo", KindGitHubPR, `{"repo":"x"}`, "", []string{"body.config.repo", "body.config.number"}},
		{"gitlab release defaults", KindGitLabRelease, `{"project":"gitlab-org/gitlab"}`,
			`{"base_url":"https://gitlab.com","project":"gitlab-org/gitlab"}`, nil},
		{"gitlab nested self-hosted", KindGitLabRelease, `{"project":"/grp/sub/proj/","base_url":"https://git.example"}`,
			`{"base_url":"https://git.example","project":"grp/sub/proj"}`, nil},
		{"gitlab single segment", KindGitLabRelease, `{"project":"proj"}`, "", []string{"body.config.project"}},
		{"gitlab route marker", KindGitLabRelease, `{"project":"grp/-/proj"}`, "", []string{"body.config.project"}},
		{"gitlab http base", KindGitLabRelease, `{"project":"a/b","base_url":"http://git.example"}`, "", []string{"body.config.base_url"}},
		{"gitlab mr", KindGitLabMR, `{"project":"a/b","iid":7}`, `{"base_url":"https://gitlab.com","iid":7,"project":"a/b"}`, nil},
		{"gitlab mr negative", KindGitLabMR, `{"project":"a/b","iid":-1}`, "", []string{"body.config.iid"}},
		{"gitlab mr iid string", KindGitLabMR, `{"project":"a/b","iid":"7"}`, "", []string{"body.config.iid"}},
		{"oci docker hub short", KindOCITags, `{"repository":"postgres"}`,
			`{"registry":"docker.io","repository":"library/postgres","semver_only":false}`, nil},
		{"oci docker hub alias", KindOCITags, `{"registry":"index.docker.io","repository":"Bitnami/redis"}`, "", []string{"body.config.repository"}},
		{"oci ghcr regex", KindOCITags, `{"registry":"GHCR.io","repository":"org/img","tag_regex":"^v\\d+","semver_only":true}`,
			`{"registry":"ghcr.io","repository":"org/img","semver_only":true,"tag_regex":"^v\\d+"}`, nil},
		{"oci ghcr single segment kept", KindOCITags, `{"registry":"ghcr.io","repository":"img"}`,
			`{"registry":"ghcr.io","repository":"img","semver_only":false}`, nil},
		{"oci registry with port", KindOCITags, `{"registry":"registry.example:5000","repository":"a/b"}`,
			`{"registry":"registry.example:5000","repository":"a/b","semver_only":false}`, nil},
		{"oci bad regex", KindOCITags, `{"repository":"postgres","tag_regex":"(["}`, "", []string{"body.config.tag_regex"}},
		{"oci long regex", KindOCITags, `{"repository":"postgres","tag_regex":"` + long + `"}`, "", []string{"body.config.tag_regex"}},
		{"oci bad registry", KindOCITags, `{"registry":"https://ghcr.io","repository":"a/b"}`, "", []string{"body.config.registry"}},
		{"oci empty repository", KindOCITags, `{}`, "", []string{"body.config.repository"}},
		{"oci uppercase repository", KindOCITags, `{"registry":"ghcr.io","repository":"Org/Img"}`, "", []string{"body.config.repository"}},
		{"unknown kind", "rss", `{}`, "", []string{"body.kind"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, errs := v.Config(tt.kind, obj(tt.in))
			if tt.errAt != nil {
				if got != nil || !reflect.DeepEqual(locations(errs), tt.errAt) {
					t.Fatalf("want errors at %v, got %v (%v)", tt.errAt, locations(errs), errs)
				}
				return
			}
			if len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			b, _ := json.Marshal(got)
			if string(b) != tt.want {
				t.Fatalf("normalized:\n got %s\nwant %s", b, tt.want)
			}
		})
	}
}

// 001-AC4: durations within 1m..7d or 5-field cron; everything else is refused with a reason.
func TestParseSchedule(t *testing.T) {
	for in, want := range map[string]string{
		"15m": "15m", " 1h30m ": "1h30m", "1m": "1m", "168h": "168h",
		"0 9 * * 1-5": "0 9 * * 1-5", "*/5  *  * * *": "*/5 * * * *", "0 0 1 jan *": "0 0 1 jan *",
	} {
		if got, msg := parseSchedule(in); got != want || msg != "" {
			t.Errorf("%q: got %q %q", in, got, msg)
		}
	}
	for in, reason := range map[string]string{
		"30s": "between", "169h": "between", "-5m": "between",
		"@daily": "expected", "0 0 * * * *": "expected", "": "expected", "soon": "expected",
		"CRON_TZ=UTC 0 9 * * *": "expected", "61 * * * *": "invalid cron", "0 9 * * 8": "invalid cron",
	} {
		if got, msg := parseSchedule(in); got != "" || !strings.Contains(msg, reason) {
			t.Errorf("%q: want rejection with %q, got %q %q", in, reason, got, msg)
		}
	}
}

func TestHelpers(t *testing.T) {
	if got := likePattern(`50%_a\b`); got != `%50\%\_a\\b%` {
		t.Errorf("likePattern: %s", got)
	}
	for in, want := range map[string]string{"short-secret": "", "ghp_0123456789abcdef": "cdef", "ééééééééééééééé€": "ééé€"} {
		if got := last4(in); got != want {
			t.Errorf("last4(%q) = %q", in, got)
		}
	}
	for kind, want := range map[string]string{KindGitHubRepo: CredGitHubToken, KindGitHubPR: CredGitHubToken,
		KindGitLabRelease: CredGitLabToken, KindGitLabMR: CredGitLabToken, KindOCITags: CredRegistryBasic} {
		if CredentialKindFor(kind) != want {
			t.Errorf("%s: %s", kind, CredentialKindFor(kind))
		}
	}
	for p, want := range map[string]level{"owner": levelOwner, "rw": levelWrite, "ro": levelRead, "": levelNone} {
		if levelOf(p) != want {
			t.Errorf("levelOf(%q)", p)
		}
	}
}

// 007-AC36: a target reduces to one exact string: host case, a trailing dot, a default port and a trailing slash fold; nothing else does, and non-ASCII never does.
func TestComparableTarget(t *testing.T) {
	for raw, want := range map[string]string{
		"https://gitlab.com":            "gitlab.com",
		"https://GitLab.COM":            "gitlab.com",
		"HTTPS://gitlab.com":            "gitlab.com",
		"https://gitlab.com.":           "gitlab.com",
		"https://gitlab.com:443":        "gitlab.com",
		"https://gitlab.com.:443/x":     "gitlab.com/x",
		"http://h.example:80":           "h.example",
		"http://h.example:443":          "h.example:443",
		"https://gitlab.com:8443":       "gitlab.com:8443",
		"https://gitlab.com:80":         "gitlab.com:80",
		"https://gitlab.com..":          "gitlab.com.",
		"ghcr.io":                       "ghcr.io",
		"ghcr.io:443":                   "ghcr.io",
		"reg.example:5000":              "reg.example:5000",
		"https://[::1]:8443":            "[::1]:8443",
		"https://g\u0456tlab.com":       "g\u0456tlab.com", // Cyrillic i
		"https://g\u0130tlab.com":       "g\u0130tlab.com", // U+0130: strings.ToLower gives gitlab.com
		"https://doc\u212Aer.io":        "doc\u212Aer.io",  // U+212A: strings.ToLower gives docker.io
		"https://B\u00fcCHER.example":   "b\u00fccher.example",
		"https://xn--bcher-kva.example": "xn--bcher-kva.example",
		"https://gitlab.com/":           "gitlab.com",
		"https://gitlab.com/DATA":       "gitlab.com/DATA",
		"https://gitlab.com:443/a/b/":   "gitlab.com/a/b",
		"https://ghe.example/API/v3":    "ghe.example/API/v3",
		"https://ghe.example/%61pi":     "ghe.example/%61pi",
		"https://%zz":                   "",
	} {
		if got := comparableTarget(raw); got != want {
			t.Errorf("%q: %q, want %q", raw, got, want)
		}
	}
}
