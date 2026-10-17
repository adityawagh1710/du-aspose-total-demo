package netpolicy

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// URLs as Chromium hands them to the deny-list (already canonicalized).
func TestChromiumDenyListRegexTable(t *testing.T) {
	re := regexp.MustCompile(ChromiumDenyListRegex)
	for _, u := range []string{
		"http://[::ffff:7f00:1]/",
		"http://[::ffff:a9fe:a9fe]/latest/meta-data/",
		"http://[::]/",
		"http://[::1]:8080/",
		"http://localhost./",
		"http://orchestrator./x",
		"http://169.254.169.254./",
		"http://0.1.2.3/",
		"file:///etc/passwd",
		"file:///proc/self/environ",
		"file://host/share",
		"file:///tm/x",
	} {
		assert.Truef(t, re.MatchString(u), "regex must DENY %s", u)
	}
	for _, u := range []string{
		"https://example.com/a.css",
		"https://[2606:4700::1111]/",
		"file:///tmp/abc/index.html", // Gotenberg's staged input page
		"data:image/png;base64,AAAA",
	} {
		assert.Falsef(t, re.MatchString(u), "regex must ALLOW %s", u)
	}
}

// compose.yaml passes the deny-list to Gotenberg as a literal string; it must
// be exactly ChromiumDenyListRegex (YAML double-quoted, so backslashes doubled).
func TestComposeDenyListMatchesConstant(t *testing.T) {
	compose, err := os.ReadFile("../../compose.yaml")
	require.NoError(t, err)
	want := `"--chromium-deny-list=` + strings.ReplaceAll(ChromiumDenyListRegex, `\`, `\\`) + `"`
	assert.Contains(t, string(compose), want)
}

func TestDeniedTable(t *testing.T) {
	denied := []string{
		"http://localhost/x.png",
		"http://sub.localhost/x.png",
		"http://localhost:8080/x.png",
		"http://127.0.0.1/style.css",
		"http://127.8.9.10/style.css",
		"http://0.0.0.0/",
		"http://10.0.0.5/a",
		"http://172.16.0.1/a",
		"http://172.31.255.255/a",
		"http://192.168.8.24/a",
		"http://169.254.169.254/latest/meta-data/", // the metadata endpoint
		"http://[::1]/a",
		"http://[fd00::1]/a",
		"http://[fe80::1]/a",
		"http://localstack:4566/bucket/key", // single-label in-cluster name
		"http://gotenberg:3000/health",
		"ftp://example.com/file",
		"file:///etc/passwd",
		// Review bypasses: IPv4-mapped/unspecified IPv6, trailing dots,
		// 0.0.0.0/8, legacy IPv4 spellings.
		"http://[::ffff:7f00:1]/",
		"http://[::ffff:a9fe:a9fe]/latest/meta-data/",
		"http://[::]/",
		"http://localhost./",
		"http://orchestrator./",
		"http://169.254.169.254./",
		"http://0.1.2.3/",
		"http://127.1/",
		"http://0x7f.0.0.1/",
	}
	for _, u := range denied {
		got, reason := Denied(u)
		assert.Truef(t, got, "expected DENY for %s", u)
		assert.NotEmptyf(t, reason, "deny reason missing for %s", u)
	}

	allowed := []string{
		"https://cdn.jsdelivr.net/npm/chart.js",
		"http://fonts.googleapis.com/css?family=Roboto",
		"https://example.com/logo.png",
		"https://8.8.8.8/x.png", // public IP literal
		"http://172.32.0.1/a",   // just outside 172.16/12
		"http://192.169.0.1/a",  // just outside 192.168/16
		"data:image/png;base64,iVBORw0KGgo=",
	}
	for _, u := range allowed {
		got, _ := Denied(u)
		assert.Falsef(t, got, "expected ALLOW for %s", u)
	}
}
