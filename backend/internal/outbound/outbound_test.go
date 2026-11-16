package outbound

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// declared maps every outside host named in backend code to who it is.
// "subprocessor: <name>" means the host receives customer data and <name>
// must appear on mesedi.ai/privacy (mesedi-web dashboard/app/privacy,
// SubprocessorList); mesedi-web's claims check enforces that side.
var declared = map[string]string{
	"api.anthropic.com": "subprocessor: Anthropic (AI root-cause analysis, on request)",
	"api.resend.com":    "subprocessor: Resend (transactional email)",

	"mesedi.ai":     "own",
	"app.mesedi.ai": "own",

	// Public anchors (since 2026-11-16). Each receives only the hourly
	// checkpoint hash, a fingerprint over every project's leaf that reveals
	// nothing about any customer, and publishes it. Not subprocessors.
	"rekor.sigstore.dev":                    "public anchor: Sigstore Rekor log",
	"alice.btc.calendar.opentimestamps.org": "public anchor: OpenTimestamps calendar",
	"bob.btc.calendar.opentimestamps.org":   "public anchor: OpenTimestamps calendar",
	"finney.calendar.eternitywall.com":      "public anchor: OpenTimestamps calendar",

	// Webhook destinations the customer configures; recognised only to
	// format the payload for that service.
	"hooks.slack.com":         "customer destination",
	"discord.com":             "customer destination",
	"discordapp.com":          "customer destination",
	"canary.discord.com":      "customer destination",
	"ptb.discord.com":         "customer destination",
	"events.pagerduty.com":    "customer destination",
	"events.eu.pagerduty.com": "customer destination",

	// A placeholder in a struct field comment written after code.
	"mcp.example.com": "example",
}

var hostRe = regexp.MustCompile(`"https://([a-zA-Z0-9.-]+\.[a-zA-Z]{2,})`)

// backendRoot is two levels up from this package.
func backendRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected the backend root at %s: %v", root, err)
	}
	return root
}

// hostsInCode returns each outside host named in a non-test Go file, with
// the files that name it. Whole-line comments are skipped: a URL in prose
// is not a call.
func hostsInCode(t *testing.T, root string) map[string][]string {
	t.Helper()
	found := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", ".git", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			for _, m := range hostRe.FindAllStringSubmatch(line, -1) {
				host := strings.ToLower(m[1])
				found[host] = append(found[host], rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func TestEveryOutsideHostIsDeclared(t *testing.T) {
	found := hostsInCode(t, backendRoot(t))
	if len(found) == 0 {
		t.Fatal("found no hosts at all; the scan is broken and would pass vacuously")
	}
	var hosts []string
	for h := range found {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		if _, ok := declared[h]; !ok {
			t.Errorf("%s is named in %v but not declared in internal/outbound. "+
				"If it receives customer data, add it to mesedi.ai/privacy and declare it "+
				"as a subprocessor here; otherwise declare what it is.", h, found[h])
		}
	}
}

func TestEveryDeclarationIsStillUsed(t *testing.T) {
	found := hostsInCode(t, backendRoot(t))
	for h := range declared {
		if _, ok := found[h]; !ok {
			t.Errorf("%s is declared but no longer named in backend code; remove it so the list stays true", h)
		}
	}
}
