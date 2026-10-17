// Package worker is the orchestrator-side wrapper around the C++ worker
// subprocess and the persistent worker pools.
//
// Ported from aspose_worker.py (one-shot exec + prlimit + exit-code mapping)
// and worker_pool.py (WorkerPool, ForkedPoolLeader seq-demux, ForkedWorkerPool).
// Implements FR-6 (subprocess isolation) and FR-4 (OOM -> subdivide signal).
//
// The C++ workers, the JSON-stdio protocol, and the prlimit RLIMIT_AS wrapper
// are unchanged from the Python orchestrator — only the orchestrator side is Go.
//
// Concurrency mapping (the load-bearing part): Python's ForkedPoolLeader uses a
// dict[int, asyncio.Future] + a stdout-reader task; here it is a
// map[int]chan response under a sync.Mutex + a reader goroutine, and
// asyncio.wait_for(fut, timeout) becomes a select on the channel vs a timer.
package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/opus2/office-convert-orchestrator/internal/config"
	"github.com/opus2/office-convert-orchestrator/internal/obs"
	"github.com/opus2/office-convert-orchestrator/internal/oclog"
	"github.com/opus2/office-convert-orchestrator/internal/oerrors"
	"github.com/opus2/office-convert-orchestrator/internal/types"
)

// Worker exit codes — the one-shot + pool contract.
const (
	ExitOK                 = 0
	ExitRenderFailure      = 1
	ExitLicenseInvalid     = 2
	ExitInputUnprocessable = 3
	ExitOOM                = 137
)

const loadTimeout = 600 * time.Second

// Stores bundles the observability sinks the worker stderr tailer feeds.
type Stores struct {
	Heartbeats *obs.RingStore
	Timings    *obs.RingStore
	Progress   *obs.JobProgressStore
}

// Pool is the common interface the orchestrator uses to render chunks,
// implemented by both WorkerPool and ForkedWorkerPool.
type Pool interface {
	RenderChunk(ctx context.Context, chunk types.Chunk, scratchDir string) (string, error)
	ActualPageCount() (int, bool)
	Close(ctx context.Context) error
}

func workerBinary(s *config.Settings, format types.FormatName) string {
	return fmt.Sprintf("%s-%s", s.WorkerBinaryPrefix, format)
}

func prlimitArgs(s *config.Settings, binary string, rest ...string) []string {
	argv := []string{"prlimit", fmt.Sprintf("--as=%d", s.WorkerRAMBytes), "--", binary}
	return append(argv, rest...)
}

// --- one-shot worker (probe + non-pool fallback) ---

// RunWorker runs the worker binary once under prlimit and returns its stdout.
// Mirrors aspose_worker._run_worker. mode is "render" or "probe". For render,
// outputPath and pageRange (1-based inclusive [start,end]) must be set.
func RunWorker(
	ctx context.Context,
	s *config.Settings,
	mode string,
	inputPath string,
	format types.FormatName,
	outputPath string,
	pageRange *[2]int,
	requestID string,
	chunk *types.Chunk,
) (stdout []byte, stderr []byte, err error) {
	return runWorkerBin(ctx, s, mode, inputPath, workerBinary(s, format), string(format), outputPath, pageRange, requestID, chunk)
}

// RenderHTMLOneShot renders an HTML input to PDF via the worker-docx binary
// (Aspose.Words loads HTML natively; the BR-4 resource deny callback and the
// BR-7 page geometry live in the worker). Full-document render: the worker
// deliberately ignores the page range for format=html, so a 1-1 placeholder
// satisfies the argv contract.
func RenderHTMLOneShot(ctx context.Context, s *config.Settings, inputPath, outputPath, requestID string) error {
	pr := [2]int{1, 1}
	_, _, err := runWorkerBin(ctx, s, "render", inputPath,
		workerBinary(s, types.FormatDOCX), "html", outputPath, &pr, requestID, nil)
	return err
}

// runWorkerBin is the shared one-shot subprocess core: bin is the worker
// binary path, formatArg the --format value (they differ for HTML, which runs
// on the docx binary).
func runWorkerBin(
	ctx context.Context,
	s *config.Settings,
	mode string,
	inputPath string,
	bin string,
	formatArg string,
	outputPath string,
	pageRange *[2]int,
	requestID string,
	chunk *types.Chunk,
) (stdout []byte, stderr []byte, err error) {
	format := types.FormatName(formatArg) // label for logging/diagnostics only
	rest := []string{
		"--mode", mode,
		"--input", inputPath,
		"--format", formatArg,
		"--license-path", s.LicensePath,
	}
	if mode == "render" {
		rest = append(rest, "--output", outputPath,
			"--page-range", fmt.Sprintf("%d-%d", pageRange[0], pageRange[1]))
	}
	argv := prlimitArgs(s, bin, rest...)

	cmd := exec.Command(argv[0], argv[1:]...)
	outPipe, _ := cmd.StdoutPipe()
	errPipe, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return nil, nil, oerrors.NewRender(chunk, -1, "spawn failed: "+err.Error())
	}

	var pageRangeList []int
	if pageRange != nil {
		pageRangeList = []int{pageRange[0], pageRange[1]}
	}
	oclog.EmitEvent("info", "worker_spawn", map[string]any{
		"worker": format, "mode": mode, "pid": cmd.Process.Pid,
		"page_range": pageRangeList, "chunk_index": chunkIndex(chunk),
	})
	spawnedAt := time.Now()

	// Read both pipes concurrently while the process runs.
	var stdoutBytes, stderrBytes []byte
	var rwg sync.WaitGroup
	rwg.Add(2)
	go func() { defer rwg.Done(); stdoutBytes, _ = io.ReadAll(outPipe) }()
	go func() { defer rwg.Done(); stderrBytes, _ = io.ReadAll(errPipe) }()

	timeout := time.Duration(s.ChunkTimeoutSeconds) * time.Second
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		rwg.Wait()
		oclog.EmitEvent("warn", "worker_exit", map[string]any{
			"worker": format, "mode": mode, "pid": cmd.Process.Pid, "exit_code": -1,
			"outcome": "timeout", "duration_s": round3(time.Since(spawnedAt)),
			"chunk_index": chunkIndex(chunk),
		})
		return nil, nil, oerrors.NewRender(chunk, -1, "timeout exceeded")
	case <-done:
		rwg.Wait()
	}

	rc := cmd.ProcessState.ExitCode()
	outcome := "ok"
	level := "info"
	if rc != ExitOK {
		outcome, level = "error", "warn"
	}
	oclog.EmitEvent(level, "worker_exit", map[string]any{
		"worker": format, "mode": mode, "pid": cmd.Process.Pid, "exit_code": rc,
		"outcome": outcome, "duration_s": round3(time.Since(spawnedAt)),
		"stderr_bytes": len(stderrBytes), "chunk_index": chunkIndex(chunk),
	})
	if err := mapExitCode(rc, stderrBytes, chunk); err != nil {
		return stdoutBytes, stderrBytes, err
	}
	return stdoutBytes, stderrBytes, nil
}

// RenderChunkOneShot spawns a worker subprocess to render one chunk (non-pool
// path). Mirrors aspose_worker.render_chunk. Returns the chunk PDF path.
func RenderChunkOneShot(ctx context.Context, s *config.Settings, chunk types.Chunk, inputPath string, format types.FormatName, scratchDir, requestID string) (string, error) {
	outputPath := fmt.Sprintf("%s/%s.pdf", scratchDir, ChunkFileStem(chunk))
	pr := [2]int{chunk.PageStart, chunk.PageEnd}
	_, _, err := RunWorker(ctx, s, "render", inputPath, format, outputPath, &pr, requestID, &chunk)
	if err != nil {
		return "", err
	}
	return outputPath, nil
}

// mapExitCode translates a worker exit code to a typed error (nil on rc=0).
// Mirrors aspose_worker._map_exit_code.
func mapExitCode(rc int, stderrBytes []byte, chunk *types.Chunk) error {
	if rc == ExitOK {
		return nil
	}
	tail := tailStr(stderrBytes, 1024)
	switch rc {
	case ExitOOM:
		if chunk != nil {
			return oerrors.NewOOM(*chunk)
		}
		return oerrors.NewRender(chunk, rc, tail)
	case ExitLicenseInvalid:
		return oerrors.NewLicenseExpired("")
	case ExitInputUnprocessable:
		if tail == "" {
			tail = "input unprocessable"
		}
		return oerrors.NewInputUnprocessable(tail)
	default:
		return oerrors.NewRender(chunk, rc, tail)
	}
}

// raiseError maps a pool protocol error response to a typed error. Mirrors
// the _raise_error helpers in worker_pool.py.
func raiseError(result map[string]any, chunk *types.Chunk) error {
	code := ExitRenderFailure
	if c, ok := result["code"]; ok {
		code = toInt(c, ExitRenderFailure)
	}
	detail, _ := result["detail"].(string)
	if detail == "" {
		detail = "unknown error"
	}
	switch code {
	case ExitOOM:
		if chunk != nil {
			return oerrors.NewOOM(*chunk)
		}
		return oerrors.NewRender(chunk, code, detail)
	case ExitLicenseInvalid:
		return oerrors.NewLicenseExpired("")
	case ExitInputUnprocessable:
		return oerrors.NewInputUnprocessable(detail)
	default:
		return oerrors.NewRender(chunk, code, detail)
	}
}

// --- shared stderr tailer ---

// handleStderrLine parses one stderr line from a pool worker and routes
// heartbeat / timing / load_progress events to the stores.
//
// pool_index source differs by pool model (matching the Python originals):
//   - Legacy WorkerPool (useMsgIndex=false): each worker is its OWN process and
//     the C++ side emits pool_index=0 for all of them, so we MUST override with
//     the orchestrator-assigned index (poolIndexDefault = 0..N-1). Without this
//     all legacy workers collapse onto pool_index 0 in the dashboard.
//   - ForkedPoolLeader (useMsgIndex=true): one process tags leader=0 / children
//     1..N-1 itself, so we trust the message's pool_index.
func handleStderrLine(format types.FormatName, pid, poolIndexDefault int, requestID string, stores Stores, useMsgIndex bool, raw string) {
	if raw == "" {
		return
	}
	var msg map[string]any
	if len(raw) > 8 && raw[:8] == `{"type":` {
		_ = json.Unmarshal([]byte(raw), &msg)
	}
	if msg == nil {
		oclog.EmitEvent("warn", "pool_worker_stderr", map[string]any{
			"worker": format, "pool_index": poolIndexDefault, "pid": pid, "text": raw,
		})
		return
	}
	switch msg["type"] {
	case "load_progress":
		if v, ok := asFloat(msg["value"]); ok && requestID != "" && requestID != "-" {
			lp := v
			stores.Progress.Update(requestID, obs.ProgressUpdate{LoadProgress: &lp})
		}
	case "heartbeat":
		poolIdx := poolIndexDefault
		if useMsgIndex {
			poolIdx = toInt(msg["pool_index"], poolIndexDefault)
		}
		rec := obs.Event{
			"worker": format, "pool_index": poolIdx, "pid": pid,
			"phase": msg["phase"], "elapsed_s": msg["elapsed_s"],
			"rss_bytes": msg["rss_bytes"], "swap_bytes": msg["swap_bytes"],
			"cpu_jiffies": msg["cpu_jiffies"], "wall_ts": nowEpoch(),
		}
		oclog.EmitEvent("debug", "pool_worker_heartbeat", rec)
		if requestID != "" && requestID != "-" {
			stores.Heartbeats.Record(requestID, rec)
		}
	case "timing":
		poolIdx := poolIndexDefault
		if useMsgIndex {
			poolIdx = toInt(msg["pool_index"], poolIndexDefault)
		}
		rec := obs.Event{"worker": format, "pool_index": poolIdx, "pid": pid, "wall_ts": nowEpoch()}
		for k, v := range msg {
			if k != "type" && k != "pool_index" {
				rec[k] = v
			}
		}
		oclog.EmitEvent("info", "pool_worker_timing", rec)
		if requestID != "" && requestID != "-" {
			stores.Timings.Record(requestID, rec)
		}
	default:
		oclog.EmitEvent("warn", "pool_worker_stderr", map[string]any{
			"worker": format, "pool_index": poolIndexDefault, "pid": pid, "text": raw,
		})
	}
}

// --- helpers ---

func chunkIndex(c *types.Chunk) any {
	if c == nil {
		return nil
	}
	return c.Index
}

// ChunkFileStem names a chunk's scratch files by its page range. Page ranges
// are unique within a request, including across OOM subdivision levels, unlike
// the float Index (subdividing 3.5 yields 3.5 and 4.0, colliding with chunk 4).
func ChunkFileStem(c types.Chunk) string {
	return fmt.Sprintf("chunk-p%d-%d", c.PageStart, c.PageEnd)
}

func tailStr(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}

func round3(d time.Duration) float64 {
	return float64(int64(d.Seconds()*1000+0.5)) / 1000
}

func nowEpoch() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func toInt(v any, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return int(i)
		}
	}
	return def
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

// readLine reads a single newline-delimited line from r (without the newline).
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return line, err
	}
	// Trim trailing newline + carriage return.
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line, nil
}
