// Package orchestrator runs the per-request pipeline:
// probe -> plan -> dispatch -> merge -> stream.
//
// Ported from office_convert/orchestrator.py. Implements FR-3..FR-7, FR-9,
// FR-10 — the end-to-end pipeline from business-logic-model.md §1.
//
// The Python original is an async generator yielding PDF bytes and stashes the
// ConversionResult via a per-task module slot. In Go, ConvertJob writes blocks
// to the caller's io.Writer (the http.Flusher-backed response) and simply
// RETURNS the ConversionResult — no contextvar/task-slot trick needed.
package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/opus2/office-convert-orchestrator/internal/cache"
	"github.com/opus2/office-convert-orchestrator/internal/config"
	"github.com/opus2/office-convert-orchestrator/internal/obs"
	"github.com/opus2/office-convert-orchestrator/internal/oclog"
	"github.com/opus2/office-convert-orchestrator/internal/oerrors"
	"github.com/opus2/office-convert-orchestrator/internal/planner"
	"github.com/opus2/office-convert-orchestrator/internal/probe"
	"github.com/opus2/office-convert-orchestrator/internal/qpdf"
	"github.com/opus2/office-convert-orchestrator/internal/types"
	"github.com/opus2/office-convert-orchestrator/internal/worker"
)

// Deps bundles the shared services a conversion needs.
type Deps struct {
	Settings *config.Settings
	Cache    *cache.Manager
	Stores   worker.Stores
}

// ConvertJob runs the full conversion pipeline for one request, streaming the
// merged PDF into dst (flushing after each block via flush, if non-nil) and
// returning the ConversionResult for the caller's response headers.
//
// Mirrors orchestrator.convert_job.
func ConvertJob(
	ctx context.Context,
	requestID, inputPath string,
	format types.FormatName,
	opts types.ConversionOptions,
	deps Deps,
	scratchDir string,
	dst io.Writer,
	flush func(),
) (*types.ConversionResult, error) {
	s := deps.Settings
	progress := deps.Stores.Progress
	started := time.Now()
	counters := &counters{}

	cacheActive := opts.Cache && deps.Cache.Enabled()
	sourceSHA := ""
	if cacheActive {
		var err error
		sourceSHA, err = fileSHA256(inputPath)
		if err != nil {
			return nil, oerrors.NewInputUnprocessable("hash failed: " + err.Error())
		}
	}

	out := &countingWriter{w: dst, flush: flush}

	// Final-cache lookup.
	if cacheActive {
		if final := deps.Cache.GetFinal(sourceSHA); final != "" {
			oclog.EmitEvent("info", "cache_hit", map[string]any{"layer": "final", "source_sha256": truncate(sourceSHA, 16)})
			progress.Update(requestID, obs.ProgressUpdate{
				TotalChunks: intp(1), LoadProgress: f64p(1.0), MergeDone: f64p(1.0), Phase: strp("complete"),
			})
			if err := streamFile(final, out); err != nil {
				return nil, err
			}
			return &types.ConversionResult{CacheHits: 1, DurationSeconds: time.Since(started).Seconds()}, nil
		}
	}

	// Probe.
	oclog.EmitEvent("info", "probe_start", map[string]any{"format": format})
	probeStarted := time.Now()
	pr, err := probe.Probe(ctx, s, inputPath, format, requestID)
	if err != nil {
		return nil, err
	}
	format = pr.Format // probe may correct the format
	oclog.EmitEvent("info", "probe_complete", map[string]any{
		"page_count": pr.PageCount, "natural_seams": len(pr.NaturalSeams),
		"size_bytes": pr.SizeBytes, "duration_s": round3(time.Since(probeStarted)),
	})

	// Plan with adaptive chunk sizing.
	adaptivePages := planner.AdaptiveMaxPages(pr, s.WorkerRAMBytes, s.Parallel, planner.AdaptiveMaxPagesOptions{})
	effMaxPages := minInt(adaptivePages, s.MaxPagesPerChunk)
	switch format {
	case types.FormatXLSX:
		effMaxPages = maxInt(effMaxPages, s.XLSXMinPagesPerChunk)
	case types.FormatPPTX:
		effMaxPages = maxInt(effMaxPages, s.PPTXMinPagesPerChunk)
	}
	oclog.EmitEvent("info", "adaptive_chunk_sizing", map[string]any{
		"adaptive_pages": adaptivePages, "effective_max_pages": effMaxPages, "format": format,
	})
	plan := planner.PlanChunks(pr, effMaxPages, s.MaxMBPerChunk)
	oclog.EmitEvent("info", "plan_complete", map[string]any{
		"chunks": len(plan.Chunks), "total_pages": plan.TotalPages,
		"strategy": planStrategy(plan), "parallel": s.Parallel,
	})
	progress.Update(requestID, obs.ProgressUpdate{TotalChunks: intp(len(plan.Chunks)), Phase: strp("loading")})

	usePool := worker.PoolModeAvailable() && len(plan.Chunks) >= s.PoolMinChunks

	var rendered []chunkPath
	if usePool {
		rendered, plan, err = dispatchPool(ctx, requestID, inputPath, format, deps, scratchDir, plan, effMaxPages, pr, sourceSHA, cacheActive, counters)
	} else {
		rendered, err = dispatchOneShot(ctx, requestID, inputPath, format, deps, scratchDir, plan, sourceSHA, cacheActive, counters)
	}
	if err != nil {
		return nil, err
	}

	// Order chunk PDFs by chunk index.
	sort.Slice(rendered, func(i, j int) bool { return rendered[i].chunk.Index < rendered[j].chunk.Index })
	chunkPaths := make([]string, len(rendered))
	for i, r := range rendered {
		chunkPaths[i] = r.path
	}

	// Stream qpdf concat into the response (and optionally tee to cache).
	cacheTemp := ""
	if cacheActive {
		cacheTemp, _ = deps.Cache.FinalTempPath(sourceSHA)
	}
	oclog.EmitEvent("info", "merge_start", map[string]any{"chunk_count": len(chunkPaths)})
	progress.Update(requestID, obs.ProgressUpdate{Phase: strp("merging")})
	mergeStarted := time.Now()
	if err := qpdf.ConcatStreaming(ctx, chunkPaths, out, cacheTemp); err != nil {
		if cacheTemp != "" {
			_ = os.Remove(cacheTemp)
		}
		return nil, err
	}
	oclog.EmitEvent("info", "merge_complete", map[string]any{
		"chunk_count": len(chunkPaths), "output_bytes": out.n, "duration_s": round3(time.Since(mergeStarted)),
	})
	progress.Update(requestID, obs.ProgressUpdate{MergeDone: f64p(1.0), Phase: strp("complete")})

	if cacheTemp != "" {
		_ = deps.Cache.FinalizeFinal(sourceSHA, cacheTemp)
	}

	dur := time.Since(started).Seconds()
	r, sub, ch := counters.snapshot()
	oclog.EmitEvent("info", "request_complete", map[string]any{
		"chunks_rendered": r, "subdivision_retries": sub, "cache_hits": ch,
		"output_bytes": out.n, "duration_seconds": round3time(dur),
	})
	return &types.ConversionResult{
		ChunksRendered: r, SubdivisionRetries: sub, CacheHits: ch, DurationSeconds: dur,
	}, nil
}

type chunkPath struct {
	chunk types.Chunk
	path  string
}

// dispatchPool renders all chunks via a persistent pool (forked or legacy).
// Returns the rendered chunk paths and the (possibly re-planned) plan.
func dispatchPool(
	ctx context.Context, requestID, inputPath string, format types.FormatName, deps Deps,
	scratchDir string, plan types.ChunkPlan, effMaxPages int, pr types.ProbeResult,
	sourceSHA string, cacheActive bool, counters *counters,
) ([]chunkPath, types.ChunkPlan, error) {
	s := deps.Settings
	progress := deps.Stores.Progress

	poolSize := minInt(s.Parallel, len(plan.Chunks))
	forked := worker.ForkAfterLoadEnabled(s, format)
	if format == types.FormatXLSX && !forked {
		poolSize = minInt(poolSize, s.XLSXMaxPoolSize)
	}

	var pool worker.Pool
	var err error
	if forked {
		oclog.EmitEvent("info", "dispatch_mode", map[string]any{"mode": "pool_fork", "workers": s.Parallel, "pool_size": poolSize})
		pool, err = worker.NewForkedWorkerPool(s, format, inputPath, requestID, deps.Stores, poolSize)
	} else {
		oclog.EmitEvent("info", "dispatch_mode", map[string]any{"mode": "pool", "workers": s.Parallel})
		pool, err = worker.NewWorkerPool(s, format, inputPath, requestID, deps.Stores, poolSize)
	}
	if err != nil {
		return nil, plan, err
	}
	defer pool.Close(context.Background())

	progress.Update(requestID, obs.ProgressUpdate{LoadProgress: f64p(1.0), Phase: strp("rendering")})

	// Re-plan if the actual page count differs from the estimate.
	if actual, ok := pool.ActualPageCount(); ok && actual != plan.TotalPages {
		oclog.EmitEvent("info", "replan_from_pool", map[string]any{"estimated_pages": plan.TotalPages, "actual_pages": actual})
		actualProbe := types.ProbeResult{PageCount: actual, Format: pr.Format, SizeBytes: pr.SizeBytes}
		plan = planner.PlanChunks(actualProbe, effMaxPages, s.MaxMBPerChunk)
		progress.Update(requestID, obs.ProgressUpdate{TotalChunks: intp(len(plan.Chunks))})
	}

	render := func(ctx context.Context, chunk types.Chunk) (string, error) {
		if cacheActive {
			key := planner.ChunkSHA256(chunk, sourceSHA, format)
			if cached := deps.Cache.GetChunk(key); cached != "" {
				counters.addCacheHit()
				progress.Update(requestID, obs.ProgressUpdate{IncrementChunks: 1})
				return cached, nil
			}
			path, err := pool.RenderChunk(ctx, chunk, scratchDir)
			if err != nil {
				return "", err
			}
			counters.addRendered()
			_ = deps.Cache.PutChunk(key, path)
			progress.Update(requestID, obs.ProgressUpdate{IncrementChunks: 1})
			return path, nil
		}
		path, err := pool.RenderChunk(ctx, chunk, scratchDir)
		if err != nil {
			return "", err
		}
		counters.addRendered()
		progress.Update(requestID, obs.ProgressUpdate{IncrementChunks: 1})
		return path, nil
	}

	rendered, err := renderAll(ctx, plan.Chunks, s.Parallel, render)
	return rendered, plan, err
}

// dispatchOneShot renders all chunks via per-chunk subprocesses with OOM
// subdivision retry.
func dispatchOneShot(
	ctx context.Context, requestID, inputPath string, format types.FormatName, deps Deps,
	scratchDir string, plan types.ChunkPlan, sourceSHA string, cacheActive bool, counters *counters,
) ([]chunkPath, error) {
	s := deps.Settings
	oclog.EmitEvent("info", "dispatch_mode", map[string]any{"mode": "one_shot", "workers": s.Parallel})
	deps.Stores.Progress.Update(requestID, obs.ProgressUpdate{LoadProgress: f64p(1.0), Phase: strp("rendering")})

	render := func(ctx context.Context, chunk types.Chunk) (string, error) {
		return renderWithRetry(ctx, chunk, inputPath, format, deps, scratchDir, requestID, sourceSHA, cacheActive, counters, 0)
	}
	return renderAll(ctx, plan.Chunks, s.Parallel, render)
}

// renderWithRetry renders a chunk; on OOM it subdivides and recurses, then
// concatenates the sub-PDFs into one. Mirrors orchestrator._render_with_retry.
func renderWithRetry(
	ctx context.Context, chunk types.Chunk, inputPath string, format types.FormatName, deps Deps,
	scratchDir, requestID, sourceSHA string, cacheActive bool, counters *counters, depth int,
) (string, error) {
	s := deps.Settings
	if cacheActive {
		key := planner.ChunkSHA256(chunk, sourceSHA, format)
		if cached := deps.Cache.GetChunk(key); cached != "" {
			counters.addCacheHit()
			oclog.EmitEvent("info", "cache_hit", map[string]any{"layer": "chunk", "chunk_index": chunk.Index})
			return cached, nil
		}
	}

	oclog.EmitEvent("info", "chunk_render_start", map[string]any{
		"chunk_index": chunk.Index, "page_range": []int{chunk.PageStart, chunk.PageEnd},
		"page_count": chunk.Pages(), "depth": depth, "worker": format,
	})
	chunkStarted := time.Now()
	path, err := worker.RenderChunkOneShot(ctx, s, chunk, inputPath, format, scratchDir, requestID)
	if err != nil {
		if oe, ok := err.(*oerrors.Error); ok && oe.OOM {
			counters.addSubdivision()
			subs := planner.Subdivide(chunk)
			if len(subs) == 0 {
				return "", oerrors.NewSubdivisionFloor(chunk, depth+1)
			}
			oclog.EmitEvent("warn", "subdivision_retry", map[string]any{
				"chunk_index": chunk.Index, "page_range_before": []int{chunk.PageStart, chunk.PageEnd},
				"sub_count": len(subs), "depth": depth,
			})
			subPaths, serr := renderAll(ctx, subs, len(subs), func(ctx context.Context, sc types.Chunk) (string, error) {
				return renderWithRetry(ctx, sc, inputPath, format, deps, scratchDir, requestID, sourceSHA, cacheActive, counters, depth+1)
			})
			if serr != nil {
				return "", serr
			}
			merged := fmt.Sprintf("%s/%s-merged.pdf", scratchDir, worker.ChunkFileStem(chunk))
			paths := make([]string, len(subPaths))
			for i, sp := range subPaths {
				paths[i] = sp.path
			}
			if err := qpdf.ConcatToFile(ctx, paths, merged); err != nil {
				return "", err
			}
			return merged, nil
		}
		return "", err
	}

	counters.addRendered()
	deps.Stores.Progress.Update(requestID, obs.ProgressUpdate{IncrementChunks: 1})
	outBytes := int64(-1)
	if info, err := os.Stat(path); err == nil {
		outBytes = info.Size()
	}
	oclog.EmitEvent("info", "chunk_complete", map[string]any{
		"chunk_index": chunk.Index, "page_range": []int{chunk.PageStart, chunk.PageEnd},
		"depth": depth, "duration_s": round3(time.Since(chunkStarted)), "output_bytes": outBytes,
	})
	if cacheActive {
		_ = deps.Cache.PutChunk(planner.ChunkSHA256(chunk, sourceSHA, format), path)
	}
	return path, nil
}

// renderAll runs renderFn over chunks with a bounded concurrency of `parallel`,
// preserving input order in the result and failing fast on the first error.
// Mirrors the asyncio.Semaphore(parallel) + gather pattern.
func renderAll(ctx context.Context, chunks []types.Chunk, parallel int, renderFn func(context.Context, types.Chunk) (string, error)) ([]chunkPath, error) {
	if parallel < 1 {
		parallel = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	out := make([]chunkPath, len(chunks))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i, c := range chunks {
		wg.Add(1)
		go func(i int, c types.Chunk) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			p, err := renderFn(ctx, c)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
				return
			}
			out[i] = chunkPath{chunk: c, path: p}
		}(i, c)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

// --- counters (concurrent-safe; Python relied on the GIL) ---

type counters struct {
	mu                       sync.Mutex
	rendered, subs, cacheHit int
}

func (c *counters) addRendered()    { c.mu.Lock(); c.rendered++; c.mu.Unlock() }
func (c *counters) addSubdivision() { c.mu.Lock(); c.subs++; c.mu.Unlock() }
func (c *counters) addCacheHit()    { c.mu.Lock(); c.cacheHit++; c.mu.Unlock() }
func (c *counters) snapshot() (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rendered, c.subs, c.cacheHit
}

// --- helpers ---

func planStrategy(plan types.ChunkPlan) string {
	if len(plan.Chunks) == 1 {
		return "single_chunk"
	}
	for _, c := range plan.Chunks {
		if c.NaturalSeam {
			return "natural_seams"
		}
	}
	return "page_range_split"
}

// countingWriter counts bytes and flushes after each write.
type countingWriter struct {
	w     io.Writer
	flush func()
	n     int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	if c.flush != nil {
		c.flush()
	}
	return n, err
}

func streamFile(path string, dst io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 65536)
	_, err = io.CopyBuffer(dst, f, buf)
	return err
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}


func round3(d time.Duration) float64 { return round3time(d.Seconds()) }
func round3time(sec float64) float64 { return float64(int64(sec*1000+0.5)) / 1000 }
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func intp(i int) *int         { return &i }
func f64p(f float64) *float64 { return &f }
func strp(s string) *string   { return &s }
