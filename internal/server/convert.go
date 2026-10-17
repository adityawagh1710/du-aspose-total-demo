package server

import (
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/opus2/office-convert-orchestrator/internal/csvinput"
	"github.com/opus2/office-convert-orchestrator/internal/email"
	"github.com/opus2/office-convert-orchestrator/internal/libreoffice"
	"github.com/opus2/office-convert-orchestrator/internal/oclog"
	"github.com/opus2/office-convert-orchestrator/internal/oerrors"
	"github.com/opus2/office-convert-orchestrator/internal/orchestrator"
	"github.com/opus2/office-convert-orchestrator/internal/probe"
	"github.com/opus2/office-convert-orchestrator/internal/ratelimit"
	"github.com/opus2/office-convert-orchestrator/internal/s3"
	"github.com/opus2/office-convert-orchestrator/internal/types"
)

// streamWriter defers the 200 status + headers until the first body byte, so a
// pre-stream error can still be turned into a JSON Diagnostic. Counts bytes and
// flushes after each write. Mirrors the "materialize first chunk before
// constructing StreamingResponse" trick in server.py.
type streamWriter struct {
	w       http.ResponseWriter
	headers map[string]string
	flusher http.Flusher
	wrote   bool
	n       int64
}

func (s *streamWriter) Write(p []byte) (int, error) {
	if !s.wrote {
		for k, v := range s.headers {
			s.w.Header().Set(k, v)
		}
		s.w.WriteHeader(http.StatusOK)
		s.wrote = true
	}
	n, err := s.w.Write(p)
	s.n += int64(n)
	if s.flusher != nil {
		s.flusher.Flush()
	}
	return n, err
}

func (srv *Server) convert(w http.ResponseWriter, r *http.Request) {
	rid := oclog.RequestID(r.Context())
	oclog.EmitEvent("info", "request_received", nil)
	t0 := time.Now()
	s := srv.settings

	// Per-IP rate limit (before semaphore so 429s don't hold a slot).
	rateHeaders := map[string]string{}
	if srv.rl != nil {
		cid := ratelimit.ClientIDFor(r, s.RateLimitTrustXFF)
		dec := srv.rl.Check(cid)
		rateHeaders["X-RateLimit-Limit"] = strconv.Itoa(dec.Limit)
		rateHeaders["X-RateLimit-Remaining"] = strconv.Itoa(dec.Remaining)
		rateHeaders["X-RateLimit-Reset"] = strconv.FormatInt(dec.ResetEpochSeconds, 10)
		if !dec.Allowed {
			oclog.EmitEvent("warn", "rate_limited", map[string]any{"limit": dec.Limit, "retry_after_seconds": dec.RetryAfterSeconds})
			srv.writeError(w, rid, oerrors.NewRateLimited(dec.RetryAfterSeconds, dec.Limit))
			return
		}
	}

	// Non-blocking semaphore acquire — 503 Busy on contention.
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

	// Parse the upload only once the client has passed the rate limit and got
	// a slot, and never past the size cap.
	if err := parseUpload(w, r, s.MaxInputBytes); err != nil {
		srv.writeError(w, rid, err)
		return
	}
	s3in := strings.TrimSpace(r.FormValue("s3_input"))
	s3out := strings.TrimSpace(r.FormValue("s3_output"))
	optionsStr := r.FormValue("options")
	if optionsStr == "" {
		optionsStr = "{}"
	}
	file, fh, ferr := r.FormFile("file")
	if ferr == nil {
		defer file.Close()
	}
	hasFile := ferr == nil && fh != nil && (fh.Filename != "" || fh.Size > 0)

	di := dispatchInfo{rid: rid, t0: t0, source: "ui"}
	if s3in != "" {
		di.source = "cross"
	}

	// Options (tolerant).
	opts := parseOptions(optionsStr)

	// License pre-check (fast fail before disk I/O).
	if expired, err := srv.license.licenseMgr.IsExpired(); err != nil {
		srv.writeError(w, rid, oerrors.NewLicenseExpired(""))
		return
	} else if expired {
		exp := ""
		if d, has, _ := srv.license.licenseMgr.ExpiryDate(); has {
			exp = d.Format("2006-01-02")
		}
		srv.writeError(w, rid, oerrors.NewLicenseExpired(exp))
		return
	}

	// Input-source selection.
	if (s3in != "" || s3out != "") && !s.S3Enabled {
		srv.writeError(w, rid, oerrors.NewS3Disabled())
		return
	}
	if hasFile && s3in != "" {
		srv.writeError(w, rid, oerrors.NewInputSourceConflict())
		return
	}
	if !hasFile && s3in == "" {
		srv.writeError(w, rid, oerrors.NewMissingFile("provide exactly one input source: 'file' or 's3_input'"))
		return
	}

	// Resolve + allowlist-check the output target before rendering.
	if s3out != "" {
		bucket, key, err := s3.ResolveOutputTarget(s3out, rid, s)
		if err != nil {
			srv.writeError(w, rid, err)
			return
		}
		if !s3.IsOutputBucketAllowed(bucket, s) {
			srv.writeError(w, rid, oerrors.NewS3OutputForbidden(bucket))
			return
		}
		di.s3OutBucket, di.s3OutKey, di.hasS3Out = bucket, key, true
	}

	scratchDir, err := newScratchDir(s.ScratchDir, rid)
	if err != nil {
		srv.writeError(w, rid, oerrors.NewRender(nil, -1, "scratch mkdir: "+err.Error()))
		return
	}
	cleanupScratch := func() { _ = os.RemoveAll(scratchDir) }
	inputTmp := scratchDir + "/input.tmp"

	// Acquire the input into inputTmp (size-bounded).
	var size int64
	var sourceFilename string
	if s3in != "" {
		if _, err := srv.s3ops.DownloadToPath(s3in, inputTmp, s); err != nil {
			cleanupScratch()
			srv.writeError(w, rid, err)
			return
		}
		size = fileSize(inputTmp)
		if size > s.MaxInputBytes {
			cleanupScratch()
			srv.writeError(w, rid, oerrors.NewInputTooLarge(size, s.MaxInputBytes))
			return
		}
		if _, key, perr := s3.ParseURL(s3in); perr == nil {
			sourceFilename = basename(key)
		}
	} else {
		sourceFilename = fh.Filename
		n, err := streamUploadToFile(file, inputTmp, s.MaxInputBytes)
		if err != nil {
			cleanupScratch()
			srv.writeError(w, rid, err)
			return
		}
		size = n
	}
	if size == 0 {
		cleanupScratch()
		srv.writeError(w, rid, oerrors.NewMissingFile("file is empty"))
		return
	}

	// CSV normalization to XLSX.
	if csvinput.IsCSVFilename(sourceFilename) {
		csvBytes, err := os.ReadFile(inputTmp)
		if err == nil {
			xlsxBytes, cerr := csvinput.CSVBytesToXLSXBytes(csvBytes)
			if cerr == nil {
				if int64(len(xlsxBytes)) > s.MaxInputBytes {
					cleanupScratch()
					srv.writeError(w, rid, oerrors.NewInputTooLarge(int64(len(xlsxBytes)), s.MaxInputBytes))
					return
				}
				_ = os.WriteFile(inputTmp, xlsxBytes, 0o644)
				size = int64(len(xlsxBytes))
			}
		}
	}

	// Detect format from the buffered file.
	head, _ := readHead(inputTmp, minI64(size, 512))
	fmtName, err := probe.DetectFormat(head, inputTmp, sourceFilename)
	if err != nil {
		cleanupScratch()
		srv.writeError(w, rid, err)
		return
	}
	di.format = string(fmtName)
	di.inputFilename = sourceFilename
	oclog.EmitEvent("info", "format_detected", map[string]any{
		"source_filename": sourceFilename, "size_bytes": size, "format": fmtName,
	})

	// Preserve the original extension for ODF/RTF/image inputs.
	suffix := string(fmtName)
	if strings.Contains(sourceFilename, ".") {
		if ext := extOf(sourceFilename); extHintFormats[ext] {
			suffix = ext
		}
	}
	inputPath := scratchDir + "/input." + suffix
	if err := os.Rename(inputTmp, inputPath); err != nil {
		cleanupScratch()
		srv.writeError(w, rid, oerrors.NewRender(nil, -1, "rename input: "+err.Error()))
		return
	}

	// Build the streaming writer (status deferred to first byte).
	headers := map[string]string{"Content-Type": "application/pdf"}
	for k, v := range rateHeaders {
		headers[k] = v
	}
	if di.hasS3Out {
		headers["X-S3-Output-Bucket"] = di.s3OutBucket
		headers["X-S3-Output-Key"] = di.s3OutKey
	}
	sw := &streamWriter{w: w, headers: headers}
	if f, ok := w.(http.Flusher); ok {
		sw.flusher = f
	}

	ctx := r.Context()

	// Route to the correct producer.
	switch {
	case libreofficeFormats[fmtName]:
		pdfPath, lerr := libreoffice.ConvertToPDF(ctx, inputPath, scratchDir+"/lo_out", s.ChunkTimeoutSeconds)
		if lerr != nil {
			cleanupScratch()
			srv.recordConversion(di, "failed", errCode(lerr), 0)
			srv.writeError(w, rid, lerr)
			return
		}
		srv.finishStream(sw, di, scratchDir, func(dst io.Writer) error { return copyPDF(pdfPath, dst) })

	case fmtName == types.DispatchEML:
		pdfPath, eerr := email.ConvertToPDF(ctx, s, inputPath, scratchDir+"/email_out", rid)
		if eerr != nil {
			cleanupScratch()
			srv.recordConversion(di, "failed", errCode(eerr), 0)
			srv.writeError(w, rid, eerr)
			return
		}
		srv.finishStream(sw, di, scratchDir, func(dst io.Writer) error { return copyPDF(pdfPath, dst) })

	case fmtName == types.DispatchHTML:
		// D1: never silently pick an engine for HTML — direct callers to the
		// explicit engine endpoints so the comparison stays intentional.
		cleanupScratch()
		herr := oerrors.NewUnsupportedFormat("html", probe.AcceptedUploadFormats,
			"use /v1/convert/html/gotenberg or /v1/convert/html/aspose for HTML inputs")
		srv.recordConversion(di, "failed", errCode(herr), 0)
		srv.writeError(w, rid, herr)
		return

	default:
		// Aspose orchestrator path (docx/pptx/xlsx/pdf).
		af := types.FormatName(fmtName)
		srv.finishStream(sw, di, scratchDir, func(dst io.Writer) error {
			_, e := orchestrator.ConvertJob(ctx, rid, inputPath, af, opts, srv.orchestratorDeps(), scratchDir, dst, nil)
			return e
		})
	}
	// finishStream owns scratch cleanup + recording from here.
	release() // release the slot now that streaming is done (defer is a no-op after this)
}

// finishStream runs produce(dst) — where dst tees to S3 if configured — then
// records the conversion, uploads to S3 on success, and cleans up scratch.
// A pre-stream error (nothing written yet) becomes a JSON Diagnostic.
func (srv *Server) finishStream(sw *streamWriter, di dispatchInfo, scratchDir string, produce func(io.Writer) error) {
	defer os.RemoveAll(scratchDir)

	var s3tmp *os.File
	var dst io.Writer = sw
	if di.hasS3Out {
		s3tmp, _ = os.CreateTemp("", "s3out-*.pdf")
		if s3tmp != nil {
			dst = io.MultiWriter(sw, s3tmp)
		}
	}

	err := produce(dst)
	if err != nil {
		if s3tmp != nil {
			s3tmp.Close()
			_ = os.Remove(s3tmp.Name())
		}
		if !sw.wrote {
			srv.recordConversion(di, "failed", errCode(err), 0)
			srv.writeError(sw.w, di.rid, err)
			return
		}
		// Already streaming, so a JSON error is impossible. Returning would
		// end the chunked body cleanly and the client would accept a
		// truncated PDF as a 200; abort the connection instead.
		srv.recordConversion(di, "failed", errCode(err), sw.n)
		panic(http.ErrAbortHandler)
	}

	if s3tmp != nil {
		s3tmp.Close()
		if upErr := srv.s3ops.UploadFile(s3tmp.Name(), di.s3OutBucket, di.s3OutKey, srv.settings); upErr != nil {
			oclog.EmitEvent("error", "s3_output_upload_failed", map[string]any{"bucket": di.s3OutBucket, "key": di.s3OutKey, "error": upErr.Error()})
		} else {
			oclog.EmitEvent("info", "s3_output_uploaded", map[string]any{"bucket": di.s3OutBucket, "key": di.s3OutKey})
		}
		_ = os.Remove(s3tmp.Name())
	}
	srv.recordConversion(di, "success", "", sw.n)
}

// --- small helpers ---

func parseOptions(raw string) types.ConversionOptions {
	opts := types.ConversionOptions{Cache: true}
	var data map[string]any
	if err := json.Unmarshal([]byte(raw), &data); err != nil || data == nil {
		return opts
	}
	if v, present := data["cache"]; present {
		if b, ok := v.(bool); ok {
			opts.Cache = b
		} else {
			opts.Cache = truthy(v)
		}
	}
	if v, ok := data["log_level"].(string); ok {
		opts.LogLevel = v
	}
	return opts
}

func truthy(v any) bool {
	switch n := v.(type) {
	case nil:
		return false
	case bool:
		return n
	case float64:
		return n != 0
	case string:
		return n != ""
	default:
		return true
	}
}

func errCode(err error) string {
	if oe, ok := err.(*oerrors.Error); ok {
		return string(oe.FailureClass)
	}
	return "render_failed"
}

// multipartSlack covers multipart boundaries and the small non-file fields.
const multipartSlack = 1 << 20

// parseUpload caps the request body at maxBytes (plus multipart overhead)
// before parsing, so an oversized upload is rejected instead of being spooled
// to disk in full. Other parse errors are left for the field checks to report.
func parseUpload(w http.ResponseWriter, r *http.Request, maxBytes int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+multipartSlack)
	var mbe *http.MaxBytesError
	if err := r.ParseMultipartForm(16 << 20); errors.As(err, &mbe) {
		return oerrors.NewInputTooLarge(max(r.ContentLength, mbe.Limit+1), maxBytes)
	}
	return nil
}

// streamUploadToFile copies an uploaded multipart file to dest, enforcing the
// size cap mid-stream (mirrors the server.py while-read loop).
func streamUploadToFile(src multipart.File, dest string, maxBytes int64) (int64, error) {
	out, err := os.Create(dest)
	if err != nil {
		return 0, oerrors.NewRender(nil, -1, "open input tmp: "+err.Error())
	}
	defer out.Close()
	buf := make([]byte, 1<<20)
	var total int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			total += int64(n)
			if total > maxBytes {
				return total, oerrors.NewInputTooLarge(total, maxBytes)
			}
			if _, werr := out.Write(buf[:n]); werr != nil {
				return total, oerrors.NewRender(nil, -1, "write input tmp: "+werr.Error())
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return total, oerrors.NewRender(nil, -1, "read upload: "+rerr.Error())
		}
	}
	return total, nil
}

func copyPDF(path string, dst io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return oerrors.NewRender(nil, -1, "open pdf: "+err.Error())
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	_, err = io.CopyBuffer(dst, f, buf)
	return err
}

func readHead(path string, n int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if n <= 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	read, _ := io.ReadFull(f, buf)
	return buf[:read], nil
}

func minI64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
