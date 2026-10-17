package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sampleHTML = "<!doctype html><html><body><h1>hello</h1></body></html>"

// postMultipart builds a multipart POST with a file part and optional extra
// form fields, and runs it through the server handler.
func postMultipart(t *testing.T, srv *Server, target, filename, content string, fields map[string]string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if filename != "" {
		part, err := mw.CreateFormFile("file", filename)
		require.NoError(t, err)
		_, err = io.WriteString(part, content)
		require.NoError(t, err)
	}
	for k, v := range fields {
		require.NoError(t, mw.WriteField(k, v))
	}
	require.NoError(t, mw.Close())

	r := httptest.NewRequest("POST", target, &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w.Result()
}

func decodeDiag(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

// installFakeDocxWorker writes an executable stand-in for
// office-convert-worker-docx that emits a minimal PDF to --output.
func installFakeDocxWorker(t *testing.T, srv *Server) {
	t.Helper()
	script := `#!/bin/sh
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--output" ]; then out="$a"; fi
  prev="$a"
done
printf '%%PDF-1.7 fake-aspose-html-pdf' > "$out"
exit 0
`
	bin := srv.settings.WorkerBinaryPrefix + "-docx"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
}

// --- Gotenberg endpoint ---

func TestHTMLGotenbergSuccessAndTelemetry(t *testing.T) {
	srv := buildTestServer(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "%PDF-1.7 fake-chromium-pdf")
	}))
	defer fake.Close()
	srv.settings.GotenbergURL = fake.URL

	resp := postMultipart(t, srv, "/v1/convert/html/gotenberg", "page.html", sampleHTML, nil)
	require.Equal(t, 200, resp.StatusCode)
	assert.Equal(t, "application/pdf", resp.Header.Get("Content-Type"))
	b, _ := io.ReadAll(resp.Body)
	assert.True(t, strings.HasPrefix(string(b), "%PDF-"))

	// FR-4: recent feed carries engine; stats expose per_engine_html.
	lr := do(t, srv, "GET", "/v1/conversions", "")
	feed := decodeDiag(t, lr)
	entries := feed["entries"].([]any)
	require.NotEmpty(t, entries)
	assert.Equal(t, "gotenberg", entries[0].(map[string]any)["engine"])
	assert.Equal(t, "html", entries[0].(map[string]any)["format"])

	sr := do(t, srv, "GET", "/v1/conversions/stats", "")
	stats := decodeDiag(t, sr)
	require.Contains(t, stats, "per_engine_html")
	perEngine := stats["per_engine_html"].(map[string]any)
	assert.Contains(t, perEngine, "gotenberg")
}

// A hostile X-Request-ID must not steer the per-request scratch dir: with
// ScratchDir=<dir>/scratch, "X-Request-ID: .." used to make cleanup run
// RemoveAll(<dir>), deleting everything beside the scratch root.
func TestHostileRequestIDCannotEscapeScratch(t *testing.T) {
	srv := buildTestServer(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "%PDF-1.7 fake-chromium-pdf")
	}))
	defer fake.Close()
	srv.settings.GotenbergURL = fake.URL
	parent := filepath.Dir(srv.settings.LicensePath)
	srv.settings.ScratchDir = filepath.Join(parent, "scratch")

	for _, rid := range []string{"..", ".", "../..", "a/../..", strings.Repeat("x", 65)} {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, err := mw.CreateFormFile("file", "page.html")
		require.NoError(t, err)
		_, _ = io.WriteString(part, sampleHTML)
		require.NoError(t, mw.Close())
		r := httptest.NewRequest("POST", "/v1/convert/html/gotenberg", &buf)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		r.Header.Set("X-Request-ID", rid)
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, r)

		assert.Equal(t, 200, w.Code, "rid %q", rid)
		assert.NotEqual(t, rid, w.Header().Get("X-Request-ID"), "invalid rid %q must be replaced", rid)
		_, err = os.Stat(srv.settings.LicensePath)
		require.NoError(t, err, "rid %q deleted files outside the scratch dir", rid)
	}
}

// An upload far past the size cap is rejected as input_too_large without the
// whole body being spooled first (MaxBytesReader stops the multipart parse).
func TestOversizedUploadRejectedBeforeFullParse(t *testing.T) {
	srv := buildTestServer(t)
	srv.settings.HTMLMaxBytes = 1024
	big := "<!doctype html><html><body>" + strings.Repeat("x", 3<<20) + "</body></html>"
	resp := postMultipart(t, srv, "/v1/convert/html/aspose", "page.html", big, nil)
	require.Equal(t, 400, resp.StatusCode)
	assert.Equal(t, "input_too_large", decodeDiag(t, resp)["failure_class"])
}

// A failure after the first PDF byte must abort the connection: ending the
// chunked body cleanly handed clients a truncated PDF with status 200.
func TestMidStreamFailureAbortsResponse(t *testing.T) {
	srv := buildTestServer(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &streamWriter{w: w, headers: map[string]string{"Content-Type": "application/pdf"}}
		if f, ok := w.(http.Flusher); ok {
			sw.flusher = f
		}
		di := dispatchInfo{rid: "midstream", t0: time.Now(), format: "docx"}
		srv.finishStream(sw, di, t.TempDir(), func(dst io.Writer) error {
			_, _ = io.WriteString(dst, "%PDF-1.7 partial")
			return errors.New("qpdf died mid-merge")
		})
	}))
	defer ts.Close()

	resp, err := http.Get(ts.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	require.Error(t, err, "truncated stream must not read as a complete body")
}

func TestValidRequestID(t *testing.T) {
	for _, ok := range []string{"rid-health", "abc_DEF-123", strings.Repeat("a", 64)} {
		assert.True(t, validRequestID(ok), ok)
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "a b", "ü", strings.Repeat("a", 65)} {
		assert.False(t, validRequestID(bad), bad)
	}
}

// A conversion that completes in under 1ms must still count toward stats.
// Truncating its duration to 0 made stats drop it, so per_engine_html went
// missing on fast CI runners (flaky TestHTMLGotenbergSuccessAndTelemetry).
func TestSubMillisecondConversionCountsInStats(t *testing.T) {
	srv := buildTestServer(t)
	srv.recordConversion(dispatchInfo{
		rid: "sub-ms", t0: time.Now(), source: "upload", format: "html", engine: "gotenberg",
	}, "success", "", 10)

	stats := decodeDiag(t, do(t, srv, "GET", "/v1/conversions/stats", ""))
	require.Contains(t, stats, "per_engine_html")
	assert.Contains(t, stats["per_engine_html"].(map[string]any), "gotenberg")
	assert.Contains(t, stats["per_format"].(map[string]any), "html")
}

func TestHTMLGotenbergEngineDown(t *testing.T) {
	srv := buildTestServer(t)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.settings.GotenbergURL = fake.URL
	fake.Close() // refuse connections

	resp := postMultipart(t, srv, "/v1/convert/html/gotenberg", "page.html", sampleHTML, nil)
	require.Equal(t, 503, resp.StatusCode)
	assert.Equal(t, "engine_unavailable", decodeDiag(t, resp)["failure_class"])
}

func TestHTMLGotenbergNotConfigured(t *testing.T) {
	srv := buildTestServer(t) // GotenbergURL == ""
	resp := postMultipart(t, srv, "/v1/convert/html/gotenberg", "page.html", sampleHTML, nil)
	require.Equal(t, 503, resp.StatusCode)
	assert.Equal(t, "engine_unavailable", decodeDiag(t, resp)["failure_class"])
}

// BR-3 wait-control validation.
func TestHTMLGotenbergWaitValidation(t *testing.T) {
	srv := buildTestServer(t)
	cases := []struct {
		name   string
		fields map[string]string
	}{
		{"bad syntax", map[string]string{"waitDelay": "soon"}},
		{"over 30s", map[string]string{"waitDelay": "31s"}},
		{"expression too long", map[string]string{"waitForExpression": strings.Repeat("x", 1025)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postMultipart(t, srv, "/v1/convert/html/gotenberg", "p.html", sampleHTML, tc.fields)
			require.Equal(t, 422, resp.StatusCode)
			assert.Equal(t, "input_unprocessable", decodeDiag(t, resp)["failure_class"])
		})
	}
}

// --- Aspose endpoint ---

func TestHTMLAsposeSuccessViaFakeWorker(t *testing.T) {
	srv := buildTestServer(t)
	installFakeDocxWorker(t, srv)

	resp := postMultipart(t, srv, "/v1/convert/html/aspose", "page.html", sampleHTML, nil)
	require.Equal(t, 200, resp.StatusCode)
	b, _ := io.ReadAll(resp.Body)
	assert.True(t, strings.HasPrefix(string(b), "%PDF-"))

	lr := do(t, srv, "GET", "/v1/conversions", "")
	entries := decodeDiag(t, lr)["entries"].([]any)
	require.NotEmpty(t, entries)
	assert.Equal(t, "aspose", entries[0].(map[string]any)["engine"])
}

// D4: wait controls on the Aspose endpoint are an explicit error.
func TestHTMLAsposeRejectsWaitControls(t *testing.T) {
	srv := buildTestServer(t)
	resp := postMultipart(t, srv, "/v1/convert/html/aspose", "p.html", sampleHTML,
		map[string]string{"waitDelay": "2s"})
	require.Equal(t, 422, resp.StatusCode)
	body := decodeDiag(t, resp)
	assert.Equal(t, "input_unprocessable", body["failure_class"])
}

// --- shared validation ---

func TestHTMLEndpointsRejectNonHTML(t *testing.T) {
	srv := buildTestServer(t)
	resp := postMultipart(t, srv, "/v1/convert/html/gotenberg", "doc.bin", "\x00\x01binary", nil)
	require.Equal(t, 422, resp.StatusCode)
	assert.Equal(t, "input_unprocessable", decodeDiag(t, resp)["failure_class"])
}

func TestHTMLEndpointsSizeCap(t *testing.T) {
	srv := buildTestServer(t)
	srv.settings.HTMLMaxBytes = 64
	big := sampleHTML + strings.Repeat("x", 256)
	resp := postMultipart(t, srv, "/v1/convert/html/aspose", "big.html", big, nil)
	require.Equal(t, 400, resp.StatusCode)
	assert.Equal(t, "input_too_large", decodeDiag(t, resp)["failure_class"])
}

func TestHTMLEndpointsMissingFile(t *testing.T) {
	srv := buildTestServer(t)
	resp := postMultipart(t, srv, "/v1/convert/html/gotenberg", "", "", map[string]string{"k": "v"})
	require.Equal(t, 400, resp.StatusCode)
	assert.Equal(t, "missing_file", decodeDiag(t, resp)["failure_class"])
}

// D1: the generic endpoint refuses HTML with a pointer to the engine routes.
func TestGenericConvertRejectsHTML(t *testing.T) {
	srv := buildTestServer(t)
	resp := postMultipart(t, srv, "/v1/convert", "page.html", sampleHTML, nil)
	require.Equal(t, 400, resp.StatusCode)
	body := decodeDiag(t, resp)
	assert.Equal(t, "unsupported_format", body["failure_class"])
	detail := body["detail"].(map[string]any)
	assert.Contains(t, detail["reason"], "/v1/convert/html/")
}
