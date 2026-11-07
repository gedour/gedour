package server_test

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Names only watch sharing used: PostgreSQL FOR SHARE / KEY SHARE and "shared secret" never match.
var shareNames = regexp.MustCompile(`(?i)watch_shares|sharee|shared_with|addShare|SharedBadge|with_me|levelWrite|shares_received|SharedWatch|dropped_share`)

// Sharing's history: the migrations that create and drop it, the test of the drop, and this guard.
var shareAllowed = map[string]bool{
	"pkg/db/migrations/00003_watches.sql":           true,
	"pkg/db/migrations/00017_drop_watch_shares.sql": true,
	"pkg/db/share_drop_test.go":                     true,
	"pkg/server/noshare_test.go":                    true,
}

// 009-AC13: no live code, test, page or doc names watch sharing again.
func TestNoWatchSharingLeft(t *testing.T) {
	root := os.DirFS("../..")
	var hits []string
	for _, dir := range []string{"pkg", "cmd", "web/src", "web/e2e", "README.md", "AGENTS.md"} {
		err := fs.WalkDir(root, dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || shareAllowed[path] {
				return nil
			}
			raw, err := fs.ReadFile(root, path)
			if err != nil {
				return err
			}
			if m := shareNames.Find(raw); m != nil {
				hits = append(hits, path+": "+string(m))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(hits) > 0 {
		t.Fatalf("watch sharing names left:\n%s", strings.Join(hits, "\n"))
	}
}

// Sharing in prose: the lock modes, the audit target type of old entries, unrelated wording and the upgrade
// note are the only allowed contexts.
var (
	shareProse      = regexp.MustCompile(`(?i)\bshar(e|es|ing)\b|shared with`)
	shareProseAllow = strings.NewReplacer("FOR SHARE", "", "KEY SHARE", "", "ACCESS SHARE", "", "`SHARE`", "", "watch_share", "",
		"share the load", "")
	upgradeSection = regexp.MustCompile(`(?s)\n## Upgrading\n.*?\n## `)
)

// 009-AC14: README and AGENTS.md describe no sharing outside the allowed contexts.
func TestDocsDescribeNoSharing(t *testing.T) {
	root := os.DirFS("../..")
	for _, doc := range []string{"README.md", "AGENTS.md"} {
		raw, err := fs.ReadFile(root, doc)
		if err != nil {
			t.Fatal(err)
		}
		text := shareProseAllow.Replace(upgradeSection.ReplaceAllString(string(raw), "\n## "))
		for _, line := range strings.Split(text, "\n") {
			if shareProse.MatchString(line) {
				t.Errorf("%s describes sharing: %q", doc, line)
			}
		}
	}
}
