// Package server is the HTTP layer: POST /v1/convert + the GET dashboards/health.
//
// Ported from office_convert/server.py. net/http (Go 1.22 method+wildcard
// routing) replaces FastAPI; the StreamingResponse becomes direct writes to an
// http.Flusher-backed ResponseWriter whose status line is deferred until the
// first body byte (so a pre-stream error still returns a JSON Diagnostic).
package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/opus2/office-convert-orchestrator/internal/cache"
	"github.com/opus2/office-convert-orchestrator/internal/config"
	"github.com/opus2/office-convert-orchestrator/internal/containerstats"
	"github.com/opus2/office-convert-orchestrator/internal/obs"
	"github.com/opus2/office-convert-orchestrator/internal/oclog"
	"github.com/opus2/office-convert-orchestrator/internal/oerrors"
	"github.com/opus2/office-convert-orchestrator/internal/orchestrator"
	"github.com/opus2/office-convert-orchestrator/internal/ratelimit"
	"github.com/opus2/office-convert-orchestrator/internal/s3"
	"github.com/opus2/office-convert-orchestrator/internal/types"
	"github.com/opus2/office-convert-orchestrator/internal/worker"
)

const version = "0.1.0"

var libreofficeFormats = map[types.DispatchFormat]bool{
	types.DispatchODG: true, types.DispatchPNG: true, types.DispatchJPG: true,
	types.DispatchTIFF: true, types.DispatchGIF: true, types.DispatchBMP: true,
	types.DispatchWEBP: true, types.DispatchSVG: true,
}

// extHintFormats: formats whose original extension must be preserved so the
// downstream loader (Aspose / soffice) picks the right importer.
var extHintFormats = map[string]bool{
	"odt": true, "ods": true, "odp": true, "odg": true, "rtf": true,
	"jpg": true, "jpeg": true, "tif": true, "tiff": true,
	"html": true, "htm": true,
}

// Server holds the wired dependencies. One per process.
type Server struct {
	settings   *config.Settings
	license    *licenseHealth
	cache      *cache.Manager
	stores     worker.Stores
	recent     *obs.RecentStore
	rl         *ratelimit.Limiter
	s3ops      s3.Ops
	sem        chan struct{}
	activeJobs atomic.Int64
	dashHTML   string
	landHTML   string
}

// New builds a Server from settings. Mirrors create_app's wiring.
func New(s *config.Settings, lic *licenseHealth, c *cache.Manager, stores worker.Stores, recent *obs.RecentStore, s3ops s3.Ops, dashHTML, landHTML string) *Server {
	srv := &Server{
		settings: s, license: lic, cache: c, stores: stores, recent: recent,
		s3ops: s3ops, sem: make(chan struct{}, s.MaxJobs), dashHTML: dashHTML, landHTML: landHTML,
	}
	if s.RateLimitEnabled {
		srv.rl = ratelimit.New(s.RateLimitPerIPRPM, s.RateLimitBurst, s.RateLimitMaxKeys)
	}
	return srv
}

// Handler returns the http.Handler with all routes + request-id middleware.
// Routing uses go-chi (the recommended Go HTTP router); the route table, methods,
// and path-param names are unchanged from the prior net/http ServeMux, so the
// wire contract is identical (verified by the golden-fixture parity gate).
func (srv *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(srv.requestIDMiddleware)
	r.Post("/v1/convert", srv.convert)
	r.Post("/v1/convert/html/gotenberg", srv.convertHTMLGotenberg)
	r.Post("/v1/convert/html/aspose", srv.convertHTMLAspose)
	r.Get("/health", srv.health)
	r.Get("/", srv.landing)
	r.Get("/v1/jobs/{request_id}/heartbeats", srv.getHeartbeats)
	r.Get("/v1/jobs/{request_id}/timings", srv.getTimings)
	r.Get("/v1/jobs/{request_id}/progress", srv.getProgress)
	r.Get("/v1/jobs/active", srv.listActiveJobs)
	r.Get("/v1/stats", srv.containerStats)
	r.Get("/v1/workers", srv.containerWorkers)
	r.Delete("/v1/cache", srv.clearCache)
	r.Get("/v1/conversions", srv.listConversions)
	r.Get("/v1/conversions/stats", srv.conversionsStats)
	r.Get("/v1/dashboard", srv.dashboard)
	r.Get("/v1/downloads/presign", srv.presign)
	return r
}

// requestIDMiddleware binds X-Request-ID (or a fresh one) to the context and
// echoes it on the response. Mirrors request_id_middleware. Shape matches
// chi's middleware contract (func(http.Handler) http.Handler).
func (srv *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rid := r.Header.Get("X-Request-ID")
		if rid == "" {
			rid = newRequestID()
		}
		w.Header().Set("X-Request-ID", rid)
		next.ServeHTTP(w, r.WithContext(oclog.WithRequestID(r.Context(), rid)))
	})
}

// --- error mapping ---

// writeError maps a typed error to the canonical JSON Diagnostic response.
// Mirrors conversion_error_handler. Unknown errors map to a 500 render_failed.
func (srv *Server) writeError(w http.ResponseWriter, rid string, err error) {
	oe, ok := err.(*oerrors.Error)
	if !ok {
		oe = &oerrors.Error{FailureClass: types.RenderFailed, HTTPStatus: 500, Msg: err.Error()}
	}
	switch oe.FailureClass {
	case types.Busy:
		if v, ok := oe.Detail["retry_after_seconds"]; ok {
			w.Header().Set("Retry-After", toStr(v))
		}
	case types.RateLimited:
		w.Header().Set("Retry-After", toStr(oe.Detail["retry_after_seconds"]))
		w.Header().Set("X-RateLimit-Limit", toStr(oe.Detail["limit"]))
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Unix()+toInt64(oe.Detail["retry_after_seconds"]), 10))
	}
	oclog.EmitEvent("error", "request_failed", mergeMap(map[string]any{"failure_class": string(oe.FailureClass)}, oe.DetailDict()))
	diag := map[string]any{"request_id": rid, "failure_class": string(oe.FailureClass), "detail": oe.DetailDict()}
	writeJSON(w, oe.HTTPStatus, diag)
}

// --- GET endpoints ---

func (srv *Server) health(w http.ResponseWriter, r *http.Request) {
	snap := srv.license.snapshot(int(srv.activeJobs.Load()))
	code := 200
	if !snap["ready"].(bool) {
		code = 503
	}
	writeJSON(w, code, snap)
}

func (srv *Server) landing(w http.ResponseWriter, r *http.Request) {
	ready, _ := srv.license.snapshot(int(srv.activeJobs.Load()))["ready"].(bool)
	label, class := "NOT READY", "err"
	if ready {
		label, class = "READY", "ok"
	}
	html := strings.NewReplacer(
		"{{STATUS_LABEL}}", label, "{{STATUS_CLASS}}", class, "{{VERSION}}", version,
	).Replace(srv.landHTML)
	writeHTML(w, html)
}

func (srv *Server) dashboard(w http.ResponseWriter, r *http.Request) { writeHTML(w, srv.dashHTML) }

func (srv *Server) getHeartbeats(w http.ResponseWriter, r *http.Request) {
	rid := chi.URLParam(r, "request_id")
	writeJSON(w, 200, map[string]any{"request_id": rid, "heartbeats": srv.stores.Heartbeats.Get(rid)})
}

func (srv *Server) getTimings(w http.ResponseWriter, r *http.Request) {
	rid := chi.URLParam(r, "request_id")
	writeJSON(w, 200, map[string]any{"request_id": rid, "timings": srv.stores.Timings.Get(rid)})
}

func (srv *Server) getProgress(w http.ResponseWriter, r *http.Request) {
	rid := chi.URLParam(r, "request_id")
	jp := srv.stores.Progress.Get(rid)
	if jp == nil {
		writeJSON(w, 200, map[string]any{
			"request_id": rid, "phase": "unknown", "total_chunks": 0, "chunks_rendered": 0,
			"load_progress": 0.0, "merge_done": 0.0, "weighted_percent": 0.0, "elapsed_s": 0.0,
		})
		return
	}
	writeJSON(w, 200, mergeMap(map[string]any{"request_id": rid}, jp.ToDict()))
}

func (srv *Server) listActiveJobs(w http.ResponseWriter, r *http.Request) {
	jobs := []map[string]any{}
	for _, e := range srv.stores.Progress.Active() {
		jobs = append(jobs, mergeMap(map[string]any{"request_id": e.RequestID}, e.Progress.ToDict()))
	}
	writeJSON(w, 200, map[string]any{"jobs": jobs})
}

func (srv *Server) containerStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, containerstats.ReadContainerStats())
}

func (srv *Server) containerWorkers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"workers": containerstats.ListWorkers("office-convert-worker")})
}

func (srv *Server) clearCache(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, srv.cache.Clear())
}

func (srv *Server) listConversions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 20
	if v, err := strconv.Atoi(q.Get("limit")); err == nil {
		limit = v
	}
	if limit < 1 {
		limit = 1
	} else if limit > 100 {
		limit = 100
	}
	filter := q.Get("filter")
	if filter == "" {
		filter = "all"
	}
	if filter != "all" && filter != "ui" && filter != "cross" && filter != "failed" {
		filter = "all"
	}
	snap := srv.recent.Snapshot()
	var filtered []obs.ConversionRecord
	for _, rec := range snap {
		if obs.Matches(rec, filter) {
			filtered = append(filtered, rec)
		}
	}
	var cur *obs.Cursor
	if c := q.Get("cursor"); c != "" {
		cur = obs.DecodeCursor(c)
	}
	page := obs.Paginate(filtered, cur, limit, srv.recent.Size())
	entries := make([]map[string]any, 0, len(page.Entries))
	for _, e := range page.Entries {
		entries = append(entries, recordToDict(e))
	}
	writeJSON(w, 200, map[string]any{
		"entries": entries, "next_cursor": page.NextCursor, "has_more": page.HasMore,
		"stale_cursor": page.StaleCursor, "buffer_size": page.BufferSize,
	})
}

func (srv *Server) conversionsStats(w http.ResponseWriter, r *http.Request) {
	snap := srv.recent.Snapshot()
	totals := map[string]int{"count": 0, "successes": 0, "failures": 0}
	perFmtTimes := map[string][]int{}
	perEngineTimes := map[string][]int{} // HTML conversions, keyed by engine
	for _, rec := range snap {
		totals["count"]++
		if rec.Status == "failed" {
			totals["failures"]++
			continue
		}
		totals["successes"]++
		if rec.DurationMS == 0 {
			continue
		}
		perFmtTimes[rec.Format] = append(perFmtTimes[rec.Format], rec.DurationMS)
		if rec.Engine != "" {
			perEngineTimes[rec.Engine] = append(perEngineTimes[rec.Engine], rec.DurationMS)
		}
	}
	perFormat := map[string]map[string]int{}
	for fmt, times := range perFmtTimes {
		perFormat[fmt] = summarizeTimes(times)
	}
	body := map[string]any{"per_format": perFormat, "totals": totals}
	// Additive (FR-4): present only once an HTML conversion has been recorded,
	// so the payload stays parity-identical to Python until the feature is used.
	if len(perEngineTimes) > 0 {
		perEngine := map[string]map[string]int{}
		for engine, times := range perEngineTimes {
			perEngine[engine] = summarizeTimes(times)
		}
		body["per_engine_html"] = perEngine
	}
	writeJSON(w, 200, body)
}

// summarizeTimes computes the count/avg/p95 stats triple over durations (ms).
func summarizeTimes(times []int) map[string]int {
	sort.Ints(times)
	sum := 0
	for _, t := range times {
		sum += t
	}
	n := len(times)
	avg := 0
	if n > 0 {
		avg = sum / n
	}
	p95 := 0
	if n > 0 {
		p95idx := int(math.Ceil(0.95*float64(n))) - 1
		if p95idx < 0 {
			p95idx = 0
		}
		if p95idx > n-1 {
			p95idx = n - 1
		}
		p95 = times[p95idx]
	}
	return map[string]int{"count": n, "avg_ms": avg, "p95_ms": p95}
}

func (srv *Server) presign(w http.ResponseWriter, r *http.Request) {
	rid := oclog.RequestID(r.Context())
	if !srv.settings.S3Enabled {
		srv.writeError(w, rid, oerrors.NewS3Disabled())
		return
	}
	bucket := r.URL.Query().Get("bucket")
	key := r.URL.Query().Get("key")
	url, err := srv.s3ops.PresignGetURL(bucket, key, srv.settings)
	if err != nil {
		srv.writeError(w, rid, err)
		return
	}
	expiresAt := time.Now().UTC().Add(time.Duration(srv.settings.S3PresignTTLSeconds) * time.Second)
	oclog.EmitEvent("info", "s3_presign", map[string]any{"bucket": bucket, "key": key})
	writeJSON(w, 200, map[string]any{
		"download_url": url, "bucket": bucket, "key": key,
		"expires_in_seconds": srv.settings.S3PresignTTLSeconds,
		"expires_at":         expiresAt.Format(time.RFC3339),
	})
}

// --- helpers ---

func newRequestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeHTML(w http.ResponseWriter, html string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(200)
	_, _ = io.WriteString(w, html)
}

func recordToDict(r obs.ConversionRecord) map[string]any {
	d := map[string]any{
		"request_id": r.RequestID, "completion_ts": r.CompletionTS, "source": r.Source,
		"input_filename": r.InputFilename, "format": r.Format, "page_count": r.PageCount,
		"duration_ms": r.DurationMS, "status": r.Status, "error_code": r.ErrorCode,
		"output_s3_uri": r.OutputS3URI, "output_size_bytes": r.OutputSizeBytes,
	}
	// Additive: only HTML conversions carry an engine; omitting the key for
	// legacy records keeps the wire shape parity-identical to Python.
	if r.Engine != "" {
		d["engine"] = r.Engine
	}
	return d
}

func mergeMap(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func toStr(v any) string {
	switch n := v.(type) {
	case int:
		return strconv.Itoa(n)
	case int64:
		return strconv.FormatInt(n, 10)
	case string:
		return n
	default:
		return ""
	}
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	}
	return 0
}

// dispatchInfo is shared state threaded through the convert pipeline for the
// recent-conversions record.
type dispatchInfo struct {
	rid           string
	t0            time.Time
	source        string
	inputFilename string
	format        string
	engine        string // "gotenberg" | "aspose" for HTML conversions; "" otherwise
	s3OutBucket   string
	s3OutKey      string
	hasS3Out      bool
}

// orchestratorDeps builds the Deps for ConvertJob.
func (srv *Server) orchestratorDeps() orchestrator.Deps {
	return orchestrator.Deps{Settings: srv.settings, Cache: srv.cache, Stores: srv.stores}
}

// elapsedMS returns whole milliseconds since t0, rounding sub-millisecond
// durations up to 1. conversionsStats treats 0 as "no duration recorded", so
// truncating a fast conversion to 0 would silently drop it from the stats.
func elapsedMS(t0 time.Time) int {
	if ms := int(time.Since(t0).Milliseconds()); ms > 0 {
		return ms
	}
	return 1
}

func (srv *Server) recordConversion(d dispatchInfo, status, errorCode string, outSize int64) {
	rec := obs.ConversionRecord{
		RequestID:    d.rid,
		CompletionTS: float64(time.Now().UnixNano()) / 1e9,
		Source:       d.source,
		Format:       d.format,
		Engine:       d.engine,
		DurationMS:   elapsedMS(d.t0),
		Status:       status,
	}
	if d.inputFilename != "" {
		f := d.inputFilename
		rec.InputFilename = &f
	}
	if errorCode != "" {
		rec.ErrorCode = &errorCode
	}
	if status == "success" {
		rec.OutputSizeBytes = &outSize
	}
	if d.hasS3Out {
		uri := "s3://" + d.s3OutBucket + "/" + d.s3OutKey
		rec.OutputS3URI = &uri
	}
	srv.recent.Record(rec)
}

func basename(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func extOf(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return strings.ToLower(name[i+1:])
	}
	return ""
}

func fileSize(path string) int64 {
	if info, err := os.Stat(path); err == nil {
		return info.Size()
	}
	return 0
}

func ensureDir(p string) error { return os.MkdirAll(p, 0o755) }

func joinScratch(base, rid string) string { return filepath.Join(base, rid) }
