// Package netpolicy is the canonical BR-4 external-resource deny policy for
// the HTML conversion engines (functional-design business-rules.md §BR-4).
//
// One normative table, two enforcement points:
//   - Denied() — consulted in Go tests and documentation; the C++ worker's
//     IResourceLoadingCallback (worker_cpp/formats/docx.cpp) reimplements the
//     same table for the Aspose engine.
//   - ChromiumDenyListRegex — the value wired into Gotenberg's
//     --chromium-deny-list flag in compose.go.yaml for the Chromium engine.
//
// The PBT suite asserts Denied() and ChromiumDenyListRegex agree on generated
// URLs, so the two enforcement points cannot silently drift.
//
// Known limitation (documented in BR-4): both enforcement points match the
// URL text without DNS resolution, so a public hostname that resolves to a
// private IP (DNS rebinding) is out of scope for this iteration.
package netpolicy

import (
	"net"
	"net/url"
	"strings"
)

// ChromiumDenyListRegex is the Gotenberg --chromium-deny-list value: it must
// deny exactly what Denied() denies for http(s) URLs. Gotenberg matches it
// with Go's regexp (RE2) against the full request URL, which Chromium has
// already canonicalized (IPv4 dotted-quad, compressed lowercase IPv6).
//
// Setting the flag replaces Gotenberg's default `^file:(?!//\/tmp/).*`, so the
// final alternative re-adds it: file: URLs are denied except under /tmp/,
// where Gotenberg stages the uploaded page itself (RE2 has no lookahead).
const ChromiumDenyListRegex = `^https?://(?:[^@/]*@)?(?:` +
	`localhost|` + // loopback by name
	`[^/:.]+|` + // single-label host (in-cluster service names)
	`[^/:]*\.localhost|` + // *.localhost
	`127\.[0-9.]+|` + // 127.0.0.0/8
	`0\.[0-9.]+|` + // 0.0.0.0/8
	`10\.[0-9.]+|` + // 10.0.0.0/8
	`192\.168\.[0-9.]+|` + // 192.168.0.0/16
	`172\.(?:1[6-9]|2[0-9]|3[01])\.[0-9.]+|` + // 172.16.0.0/12
	`169\.254\.[0-9.]+|` + // link-local + metadata
	`\[::[^\]]*\]|` + // ::/96-ish: ::, ::1, IPv4-mapped ::ffff:a.b.c.d
	`\[[fF][cdCD][0-9a-fA-F]{2}:[^\]]*\]|` + // fc00::/7 (ULA)
	`\[[fF][eE][89abAB][0-9a-fA-F]:[^\]]*\]` + // fe80::/10 (link-local)
	`)\.?(?::[0-9]+)?(?:[/?#]|$)` + // optional FQDN trailing dot
	`|^file:(?:[^/]|/[^/]|//[^/]|///(?:[^t]|t[^m]|tm[^p]|tmp[^/]))`

// Denied reports whether the BR-4 policy blocks a fetch of rawURL, with a
// short human-readable reason for the SSRF audit log. data: URIs are allowed
// (no network fetch); every non-http(s) scheme is denied.
func Denied(rawURL string) (bool, string) {
	lower := strings.ToLower(strings.TrimSpace(rawURL))
	if strings.HasPrefix(lower, "data:") {
		return false, ""
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return true, "unparseable URL"
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return true, "scheme " + u.Scheme
	}
	// A trailing dot is the same host to a resolver ("localhost." == "localhost").
	host := strings.TrimRight(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return true, "empty host"
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true, "loopback host"
	}
	if ip := net.ParseIP(host); ip != nil {
		if strings.HasPrefix(host, "::") || (ip.To4() != nil && strings.Contains(host, ":")) {
			return true, "reserved IPv6 " + host // ::, ::1, IPv4-mapped/compatible
		}
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return true, "private address " + host
		}
		if ip4 := ip.To4(); ip4 != nil && ip4[0] == 0 {
			return true, "private address " + host // 0.0.0.0/8
		}
		return false, ""
	}
	if !strings.Contains(host, ".") {
		return true, "single-label host " + host
	}
	// No real hostname ends in a numeric label; this is an IPv4 literal in a
	// legacy spelling (127.1, 0x7f.0.0.1, 0177.0.0.1) that resolvers accept.
	if isNumericLabel(host[strings.LastIndexByte(host, '.')+1:]) {
		return true, "non-canonical IP literal " + host
	}
	return false, ""
}

func isNumericLabel(label string) bool {
	if strings.HasPrefix(label, "0x") {
		return true
	}
	if label == "" {
		return false
	}
	for _, c := range label {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
