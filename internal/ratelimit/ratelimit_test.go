package ratelimit

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClientIDFor(t *testing.T) {
	cases := []struct {
		name     string
		xff      []string
		trustXFF bool
		want     string
	}{
		{"no xff falls back to peer", nil, true, "192.0.2.1"},
		{"single hop", []string{"203.0.113.7"}, true, "203.0.113.7"},
		// The client sent a forged value; the ALB appended the real peer.
		{"spoofed prefix ignored", []string{"1.2.3.4, 203.0.113.7"}, true, "203.0.113.7"},
		{"repeated header lines", []string{"1.2.3.4", "203.0.113.7"}, true, "203.0.113.7"},
		{"trailing empty entry falls back", []string{"1.2.3.4, "}, true, "192.0.2.1"},
		{"xff ignored when untrusted", []string{"1.2.3.4"}, false, "192.0.2.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/convert", nil)
			r.RemoteAddr = "192.0.2.1:5555"
			for _, v := range c.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			assert.Equal(t, c.want, ClientIDFor(r, c.trustXFF))
		})
	}
}

// Rotating a forged X-Forwarded-For prefix must not escape the limit.
func TestSpoofedXFFStillLimited(t *testing.T) {
	l := New(60, 2, 100)
	allowed := 0
	for i := 0; i < 10; i++ {
		r := httptest.NewRequest("POST", "/v1/convert", nil)
		r.Header.Set("X-Forwarded-For", "10.0.0."+string(rune('0'+i))+", 203.0.113.7")
		if l.Check(ClientIDFor(r, true)).Allowed {
			allowed++
		}
	}
	assert.LessOrEqual(t, allowed, 3)
}
