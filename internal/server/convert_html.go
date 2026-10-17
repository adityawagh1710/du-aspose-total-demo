// convert_html.go — the two engine-specific HTML→PDF endpoints (HTML feature
// FR-1/FR-3, functional-design business-logic-model.md Flows A and B).
//
// Both routes use the bypass pattern (no probe→plan→render→merge pipeline):
// shared pre-processing → engine render → %PDF- validation → stream + record.
// The streamWriter/finishStream plumbing is shared with the office paths.
package server

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/opus2/office-convert-orchestrator/internal/gotenberg"
	"github.com/opus2/office-convert-orchestrator/internal/oclog"
	"github.com/opus2/office-convert-orchestrator/internal/oerrors"
	"github.com/opus2/office-convert-orchestrator/internal/probe"
	"github.com/opus2/office-convert-orchestrator/internal/ratelimit"
	"github.com/opus2/office-convert-orchestrator/internal/worker"
)

// BR-3 bounds for the JS wait controls (Gotenberg endpoint only).
const (
	maxWaitDelay         = 30 * time.Second
	maxWaitExpressionLen = 1024
)

func (srv *Server) convertHTMLGotenberg(w http.ResponseWriter, r *http.Request) {
	srv.convertHTML(w, r, "gotenberg")
}

func (srv *Server) convertHTMLAspose(w http.ResponseWriter, r *http.Request) {
	srv.convertHTML(w, r, "aspose")
}

func (srv *Server) convertHTML(w http.ResponseWriter, r *http.Request, engine string) {
	rid := oclog.RequestID(r.Context())
	oclog.EmitEvent("info", "html_convert_start", map[string]any{"engine": engine})
	s := srv.settings
	di := dispatchInfo{rid: rid, t0: time.Now(), source: "ui", format: "html", engine: engine}

	// Per-IP rate limit (shared budget with /v1/convert).
	rateHeaders := map[string]string{}
	if srv.rl != nil {
		cid := ratelimit.ClientIDFor(r, s.RateLimitTrustXFF)
		dec := srv.rl.Check(cid)
		rateHeaders["X-RateLimit-Limit"] = strconv.Itoa(dec.Limit)
		rateHeaders["X-RateLimit-Remaining"] = strconv.Itoa(dec.Remaining)
		rateHeaders["X-RateLimit-Reset"] = strconv.FormatInt(dec.ResetEpochSeconds, 10)
		if !dec.Allowed {
			srv.writeError(w, rid, oerrors.NewRateLimited(dec.RetryAfterSeconds, dec.Limit))
			return
		}
	}

	// Concurrency semaphore (shared with the office paths).
	select {
	case srv.sem <- struct{}{}:
	default:
		srv.writeError(w, rid, oerrors.NewBusy(60))
		return
	}
	srv.activeJobs.Add(1)
	released := false
	release := func() {
		if !released {
			released = true
			<-srv.sem
			srv.activeJobs.Add(-1)
		}
	}
	defer release()

	// Parse the upload only after the rate limit and slot checks, capped at
	// the HTML size limit.
	if err := parseUpload(w, r, s.HTMLMaxBytes); err != nil {
		srv.writeError(w, rid, err)
		return
	}

	// BR-3: wait controls are Gotenberg-only (Aspose has no JS engine — D4).
	waitOpts, werr := parseWaitOptions(r, engine)
	if werr != nil {
		srv.writeError(w, rid, werr)
		return
	}

	// The Aspose engine needs a valid license; Gotenberg does not (fair
	// benchmark: a lapsed Aspose license must not block the Chromium engine).
	if engine == "aspose" {
		if expired, err := srv.license.licenseMgr.IsExpired(); err != nil || expired {
			exp := ""
			if d, has, _ := srv.license.licenseMgr.ExpiryDate(); has {
				exp = d.Format("2006-01-02")
			}
			srv.writeError(w, rid, oerrors.NewLicenseExpired(exp))
			return
		}
	}

	// Upload acquisition: these endpoints accept a multipart file only.
	file, fh, ferr := r.FormFile("file")
	if ferr != nil || fh == nil || (fh.Filename == "" && fh.Size == 0) {
		srv.writeError(w, rid, oerrors.NewMissingFile("provide an HTML upload in the 'file' field"))
		return
	}
	defer file.Close()
	di.inputFilename = fh.Filename

	scratchDir, err := newScratchDir(s.ScratchDir, rid)
	if err != nil {
		srv.writeError(w, rid, oerrors.NewRender(nil, -1, "scratch mkdir: "+err.Error()))
		return
	}
	cleanupScratch := func() { _ = os.RemoveAll(scratchDir) }
	inputPath := scratchDir + "/input.html"

	// NFR-2: HTML-specific cap, enforced mid-stream.
	size, err := streamUploadToFile(file, inputPath, s.HTMLMaxBytes)
	if err != nil {
		cleanupScratch()
		srv.writeError(w, rid, err)
		return
	}
	if size == 0 {
		cleanupScratch()
		srv.writeError(w, rid, oerrors.NewMissingFile("file is empty"))
		return
	}

	// BR-2: these endpoints convert ONLY HTML (sniff + extension fallback).
	head, _ := readHead(inputPath, minI64(size, 1024))
	if !probe.IsHTMLUpload(head, fh.Filename) {
		cleanupScratch()
		srv.writeError(w, rid, oerrors.NewInputUnprocessable(
			"input is not HTML (expected <!doctype html>/<html> content or a .html/.htm file)"))
		return
	}

	oclog.EmitEvent("info", "html_convert_dispatch", map[string]any{
		"engine": engine, "size_bytes": size, "source_filename": fh.Filename,
	})

	headers := map[string]string{"Content-Type": "application/pdf"}
	for k, v := range rateHeaders {
		headers[k] = v
	}
	sw := &streamWriter{w: w, headers: headers}
	if f, ok := w.(http.Flusher); ok {
		sw.flusher = f
	}

	ctx := r.Context()
	outPath := scratchDir + "/output.pdf"

	srv.finishStream(sw, di, scratchDir, func(dst io.Writer) error {
		var rerr error
		switch engine {
		case "gotenberg":
			client := gotenberg.New(s.GotenbergURL, s.GotenbergTimeoutSeconds)
			rerr = client.ConvertHTML(ctx, inputPath, waitOpts, outPath)
		default: // "aspose"
			rerr = worker.RenderHTMLOneShot(ctx, s, inputPath, outPath, rid)
			if rerr == nil {
				rerr = validatePDFMagic(outPath)
			}
		}
		if rerr != nil {
			return rerr
		}
		return copyPDF(outPath, dst)
	})
	release()
	oclog.EmitEvent("info", "html_convert_complete", map[string]any{
		"engine": engine, "duration_ms": time.Since(di.t0).Milliseconds(),
	})
}

// parseWaitOptions validates the BR-3 wait-control form fields. On the Aspose
// endpoint any wait field is an explicit 400 (D4): silently ignoring a JS wait
// on an engine with no JS would corrupt the comparison.
func parseWaitOptions(r *http.Request, engine string) (gotenberg.WaitOptions, *oerrors.Error) {
	delay := strings.TrimSpace(r.FormValue("waitDelay"))
	expr := strings.TrimSpace(r.FormValue("waitForExpression"))

	if engine != "gotenberg" {
		if delay != "" || expr != "" {
			return gotenberg.WaitOptions{}, oerrors.NewInputUnprocessable(
				"wait controls are not supported by the aspose engine (no JavaScript); use /v1/convert/html/gotenberg")
		}
		return gotenberg.WaitOptions{}, nil
	}
	if delay != "" {
		d, err := time.ParseDuration(delay)
		if err != nil || d <= 0 {
			return gotenberg.WaitOptions{}, oerrors.NewInputUnprocessable(
				"waitDelay must be a positive duration (e.g. 2s, 1500ms)")
		}
		if d > maxWaitDelay {
			return gotenberg.WaitOptions{}, oerrors.NewInputUnprocessable(
				"waitDelay must be <= 30s")
		}
	}
	if len(expr) > maxWaitExpressionLen {
		return gotenberg.WaitOptions{}, oerrors.NewInputUnprocessable(
			"waitForExpression must be <= 1024 characters")
	}
	return gotenberg.WaitOptions{WaitDelay: delay, WaitForExpression: expr}, nil
}

// validatePDFMagic enforces BR-8 on worker output before streaming starts.
func validatePDFMagic(path string) error {
	head, err := readHead(path, 5)
	if err != nil || string(head) != "%PDF-" {
		return oerrors.NewRender(nil, -1, "engine produced non-PDF output")
	}
	return nil
}
