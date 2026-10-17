package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opus2/office-convert-orchestrator/internal/config"
	"github.com/opus2/office-convert-orchestrator/internal/oclog"
	"github.com/opus2/office-convert-orchestrator/internal/oerrors"
	"github.com/opus2/office-convert-orchestrator/internal/types"
)

// forkUnsafeFormats — Aspose.Cells does not survive fork() (explicit Startup()
// lifecycle + OpenSSL + internal worker threads). Mirrors _FORK_UNSAFE_FORMATS.
var forkUnsafeFormats = map[types.FormatName]bool{types.FormatXLSX: true}

// PoolModeAvailable reports whether the worker should use --mode=pool. Mirrors
// pool_mode_available: enabled unless OFFICE_CONVERT_POOL_MODE=0.
func PoolModeAvailable() bool {
	return os.Getenv("OFFICE_CONVERT_POOL_MODE") != "0"
}

// ForkAfterLoadEnabled reports whether to use the ForkedWorkerPool for a format.
// XLSX is always opted out (fork-unsafe) regardless of the global flag.
// Mirrors fork_after_load_enabled.
func ForkAfterLoadEnabled(s *config.Settings, format types.FormatName) bool {
	if forkUnsafeFormats[format] {
		return false
	}
	return s.ForkAfterLoad
}

// =========================== PooledWorker / WorkerPool ===========================

// PooledWorker is a single persistent worker process holding a loaded document.
// Ported from worker_pool.PooledWorker. A persistent goroutine drains stdout
// into respCh (command responses) and another drains stderr (heartbeats/timings).
type PooledWorker struct {
	cmd       *exec.Cmd
	stdin     interface{ Write([]byte) (int, error) }
	format    types.FormatName
	pid       int
	poolIndex int
	requestID string
	stores    Stores

	respCh chan string
	loaded bool
	// dead: the process exited, was killed on timeout, or desynced the
	// protocol. A dead worker must not take more chunks.
	dead atomic.Bool
}

func spawnPooledWorker(s *config.Settings, format types.FormatName, poolIndex int, requestID string, stores Stores, extraArgs ...string) (*PooledWorker, error) {
	bin := workerBinary(s, format)
	rest := append([]string{"--mode", "pool", "--format", string(format), "--license-path", s.LicensePath}, extraArgs...)
	argv := prlimitArgs(s, bin, rest...)

	cmd := exec.Command(argv[0], argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w := &PooledWorker{
		cmd: cmd, stdin: stdin, format: format, pid: cmd.Process.Pid,
		poolIndex: poolIndex, requestID: requestID, stores: stores,
		respCh: make(chan string, 8),
	}
	go w.readStdout(bufio.NewReader(stdout))
	go w.readStderr(bufio.NewReader(stderr))
	return w, nil
}

func (w *PooledWorker) readStdout(r *bufio.Reader) {
	for {
		line, err := readLine(r)
		if line != "" {
			w.respCh <- line
		}
		if err != nil {
			close(w.respCh)
			return
		}
	}
}

func (w *PooledWorker) readStderr(r *bufio.Reader) {
	for {
		line, err := readLine(r)
		// Legacy pool: each worker is its own process emitting pool_index=0,
		// so force the orchestrator-assigned index (useMsgIndex=false).
		handleStderrLine(w.format, w.pid, w.poolIndex, w.requestID, w.stores, false, line)
		if err != nil {
			return
		}
	}
}

func (w *PooledWorker) alive() bool {
	return w.cmd.ProcessState == nil
}

// send writes a JSON command line to the worker's stdin.
func (w *PooledWorker) send(cmd map[string]any) error {
	b, _ := json.Marshal(cmd)
	b = append(b, '\n')
	_, err := w.stdin.Write(b)
	return err
}

// await reads one response with a timeout; kills the worker on timeout.
func (w *PooledWorker) await(timeout time.Duration, chunk *types.Chunk) (map[string]any, error) {
	select {
	case line, ok := <-w.respCh:
		if !ok {
			w.dead.Store(true)
			return nil, oerrors.NewRender(chunk, ExitRenderFailure, "worker stdout EOF")
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(line), &result); err != nil {
			// The reply stream is out of step: a later line would be taken
			// as the next chunk's reply. Retire the worker.
			w.dead.Store(true)
			_ = w.cmd.Process.Kill()
			return nil, oerrors.NewRender(chunk, ExitRenderFailure, "invalid worker JSON: "+err.Error())
		}
		return result, nil
	case <-time.After(timeout):
		w.dead.Store(true)
		_ = w.cmd.Process.Kill()
		return nil, oerrors.NewRender(chunk, -1, "pool worker timeout")
	}
}

// LoadDocument sends load and returns the page count.
func (w *PooledWorker) LoadDocument(inputPath string) (int, error) {
	if err := w.send(map[string]any{"cmd": "load", "input": inputPath}); err != nil {
		return 0, err
	}
	result, err := w.await(loadTimeout, nil)
	if err != nil {
		return 0, err
	}
	if result["status"] != "ok" {
		return 0, raiseError(result, nil)
	}
	w.loaded = true
	return toInt(result["page_count"], 0), nil
}

// RenderChunk sends render for a page range and returns the output path.
func (w *PooledWorker) RenderChunk(chunk types.Chunk, outputPath string, timeout time.Duration) (string, error) {
	if err := w.send(map[string]any{
		"cmd": "render", "page_start": chunk.PageStart, "page_end": chunk.PageEnd, "output": outputPath,
	}); err != nil {
		return "", err
	}
	result, err := w.await(timeout, &chunk)
	if err != nil {
		return "", err
	}
	if result["status"] != "ok" {
		return "", raiseError(result, &chunk)
	}
	return outputPath, nil
}

func (w *PooledWorker) quit() {
	if w.alive() {
		_ = w.send(map[string]any{"cmd": "quit"})
		done := make(chan struct{})
		go func() { _ = w.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = w.cmd.Process.Kill()
			<-done
		}
	}
}

func (w *PooledWorker) kill() {
	if w.alive() {
		_ = w.cmd.Process.Kill()
	}
	_ = w.cmd.Wait()
}

// WorkerPool is a per-request pool of N independent persistent workers sharing
// the same loaded document via independent loads. Ported from WorkerPool.
type WorkerPool struct {
	settings  *config.Settings
	format    types.FormatName
	input     string
	requestID string
	stores    Stores
	poolSize  int

	mu        sync.Mutex // guards workers (replace vs Close)
	workers   []*PooledWorker
	available chan *PooledWorker
	pageCount int
	havePages bool
}

// NewWorkerPool spawns poolSize workers and loads the document in each (in
// parallel). On any failure all live workers are killed and the error returned.
func NewWorkerPool(s *config.Settings, format types.FormatName, input, requestID string, stores Stores, poolSize int) (*WorkerPool, error) {
	p := &WorkerPool{settings: s, format: format, input: input, requestID: requestID, stores: stores, poolSize: poolSize}

	type res struct {
		w     *PooledWorker
		pages int
		err   error
	}
	results := make([]res, poolSize)
	var wg sync.WaitGroup
	for i := 0; i < poolSize; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w, err := spawnPooledWorker(s, format, i, requestID, stores)
			if err != nil {
				results[i] = res{err: err}
				return
			}
			oclog.EmitEvent("info", "pool_worker_spawn", map[string]any{"worker": format, "pool_index": i, "pid": w.pid})
			pages, err := w.LoadDocument(input)
			if err != nil {
				w.kill()
				results[i] = res{err: err}
				return
			}
			oclog.EmitEvent("info", "pool_worker_loaded", map[string]any{"worker": format, "pool_index": i, "page_count": pages})
			results[i] = res{w: w, pages: pages}
		}(i)
	}
	wg.Wait()

	var firstErr error
	var successes []*PooledWorker
	var successPages []int
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			oclog.EmitEvent("warn", "pool_worker_load_failed", map[string]any{"error": r.err.Error()})
		} else {
			successes = append(successes, r.w)
			successPages = append(successPages, r.pages)
		}
	}
	if firstErr != nil {
		for _, w := range successes {
			w.kill()
		}
		return nil, firstErr
	}

	p.available = make(chan *PooledWorker, len(successes))
	for i, w := range successes {
		if !p.havePages {
			p.pageCount, p.havePages = successPages[i], true
		}
		p.workers = append(p.workers, w)
		p.available <- w
	}
	return p, nil
}

// RenderChunk checks out the next available worker and renders one chunk.
func (p *WorkerPool) RenderChunk(ctx context.Context, chunk types.Chunk, scratchDir string) (string, error) {
	var w *PooledWorker
	select {
	case w = <-p.available:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	outputPath := fmt.Sprintf("%s/%s.pdf", scratchDir, ChunkFileStem(chunk))
	path, err := w.RenderChunk(chunk, outputPath, time.Duration(p.settings.ChunkTimeoutSeconds)*time.Second)
	if w.dead.Load() {
		w = p.replace(w)
	}
	p.available <- w
	return path, err
}

// replace reaps a dead worker and swaps in a freshly loaded one. If the
// respawn fails the dead worker stays in rotation: its chunks then fail fast
// (stdin EPIPE / closed stdout) instead of blocking on an empty pool.
func (p *WorkerPool) replace(dead *PooledWorker) *PooledWorker {
	dead.kill()
	nw, err := spawnPooledWorker(p.settings, p.format, dead.poolIndex, p.requestID, p.stores)
	if err == nil {
		if _, err = nw.LoadDocument(p.input); err != nil {
			nw.kill()
		}
	}
	if err != nil {
		oclog.EmitEvent("warn", "pool_worker_respawn_failed", map[string]any{"pool_index": dead.poolIndex, "error": err.Error()})
		return dead
	}
	oclog.EmitEvent("info", "pool_worker_respawned", map[string]any{"pool_index": dead.poolIndex, "pid": nw.pid})
	p.mu.Lock()
	for i, w := range p.workers {
		if w == dead {
			p.workers[i] = nw
		}
	}
	p.mu.Unlock()
	return nw
}

// ActualPageCount returns the page count reported by the first loaded worker.
func (p *WorkerPool) ActualPageCount() (int, bool) { return p.pageCount, p.havePages }

// Close gracefully shuts down all workers.
func (p *WorkerPool) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, w := range p.workers {
		w.quit()
	}
	p.workers = nil
	return nil
}

// =========================== ForkedPoolLeader / ForkedWorkerPool ===========================

// ForkedPoolLeader is a single leader process that loads the document once and
// forks pool_size-1 children sharing it via copy-on-write. The orchestrator
// sees one stdin/stdout/stderr triple; each command carries a seq id so
// concurrent render responses are demuxed back to the waiting caller.
//
// Ported from worker_pool.ForkedPoolLeader. The dict[int, asyncio.Future] +
// stdout-reader task becomes map[int]chan + a reader goroutine under a mutex.
type ForkedPoolLeader struct {
	cmd       *exec.Cmd
	stdin     interface{ Write([]byte) (int, error) }
	format    types.FormatName
	pid       int
	poolSize  int
	requestID string
	stores    Stores

	writeMu sync.Mutex // serializes stdin writes

	mu      sync.Mutex
	seq     int
	pending map[int]chan map[string]any
	loaded  bool
}

func spawnForkedLeader(s *config.Settings, format types.FormatName, input, requestID string, stores Stores, poolSize int) (*ForkedPoolLeader, error) {
	w, err := spawnRaw(s, format, requestID, "--pool-size", fmt.Sprintf("%d", poolSize))
	if err != nil {
		return nil, err
	}
	l := &ForkedPoolLeader{
		cmd: w.cmd, stdin: w.stdin, format: format, pid: w.pid, poolSize: poolSize,
		requestID: requestID, stores: stores, pending: make(map[int]chan map[string]any),
	}
	go l.readStdout(w.stdout)
	go l.readStderr(w.stderr)
	return l, nil
}

// rawProc is the bundle of a started process + its pipes.
type rawProc struct {
	cmd    *exec.Cmd
	stdin  interface{ Write([]byte) (int, error) }
	stdout *bufio.Reader
	stderr *bufio.Reader
	pid    int
}

func spawnRaw(s *config.Settings, format types.FormatName, requestID string, extraArgs ...string) (*rawProc, error) {
	bin := workerBinary(s, format)
	rest := append([]string{"--mode", "pool", "--format", string(format), "--license-path", s.LicensePath}, extraArgs...)
	argv := prlimitArgs(s, bin, rest...)
	cmd := exec.Command(argv[0], argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &rawProc{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), stderr: bufio.NewReader(stderr), pid: cmd.Process.Pid}, nil
}

func (l *ForkedPoolLeader) readStdout(r *bufio.Reader) {
	for {
		line, err := readLine(r)
		if line != "" {
			var data map[string]any
			if json.Unmarshal([]byte(line), &data) == nil {
				seq := toInt(data["seq"], 0)
				l.mu.Lock()
				ch := l.pending[seq]
				delete(l.pending, seq)
				l.mu.Unlock()
				if ch != nil {
					ch <- data
				} else {
					oclog.EmitEvent("warn", "fork_leader_unknown_seq", map[string]any{"seq": seq})
				}
			} else {
				oclog.EmitEvent("warn", "fork_leader_non_json", map[string]any{"line": tailStr([]byte(line), 200)})
			}
		}
		if err != nil {
			l.failAllPending("worker stdout EOF")
			return
		}
	}
}

func (l *ForkedPoolLeader) failAllPending(reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for seq, ch := range l.pending {
		ch <- map[string]any{"seq": seq, "status": "error", "code": float64(ExitRenderFailure), "detail": reason}
		delete(l.pending, seq)
	}
}

func (l *ForkedPoolLeader) readStderr(r *bufio.Reader) {
	for {
		line, err := readLine(r)
		// Leader=0, children=1..N-1; pool_index is read from each message.
		// Forked leader: one process tags leader=0 / children 1..N-1 itself,
		// so trust the message's pool_index (useMsgIndex=true).
		handleStderrLine(l.format, l.pid, 0, l.requestID, l.stores, true, line)
		if err != nil {
			return
		}
	}
}

func (l *ForkedPoolLeader) register(seq int) chan map[string]any {
	ch := make(chan map[string]any, 1)
	l.mu.Lock()
	l.pending[seq] = ch
	l.mu.Unlock()
	return ch
}

func (l *ForkedPoolLeader) unregister(seq int) {
	l.mu.Lock()
	delete(l.pending, seq)
	l.mu.Unlock()
}

func (l *ForkedPoolLeader) write(cmd map[string]any) error {
	b, _ := json.Marshal(cmd)
	b = append(b, '\n')
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	_, err := l.stdin.Write(b)
	return err
}

// LoadDocument sends the seq=0 load command and returns the page count.
func (l *ForkedPoolLeader) LoadDocument(inputPath string) (int, error) {
	ch := l.register(0) // seq 0 reserved for load
	defer l.unregister(0)
	if err := l.write(map[string]any{"cmd": "load", "seq": 0, "input": inputPath}); err != nil {
		return 0, err
	}
	result, err := awaitSeq(ch, loadTimeout, nil)
	if err != nil {
		return 0, err
	}
	if result["status"] != "ok" {
		return 0, raiseError(result, nil)
	}
	l.loaded = true
	return toInt(result["page_count"], 0), nil
}

// RenderChunk sends a render command with a fresh seq and awaits its response.
func (l *ForkedPoolLeader) RenderChunk(chunk types.Chunk, outputPath string, timeout time.Duration) (string, error) {
	l.mu.Lock()
	l.seq++
	seq := l.seq
	l.mu.Unlock()
	ch := l.register(seq)
	defer l.unregister(seq)

	if err := l.write(map[string]any{
		"cmd": "render", "seq": seq,
		"page_start": chunk.PageStart, "page_end": chunk.PageEnd, "output": outputPath,
	}); err != nil {
		return "", err
	}
	result, err := awaitSeq(ch, timeout, &chunk)
	if err != nil {
		return "", err
	}
	if result["status"] != "ok" {
		return "", raiseError(result, &chunk)
	}
	return outputPath, nil
}

func awaitSeq(ch chan map[string]any, timeout time.Duration, chunk *types.Chunk) (map[string]any, error) {
	select {
	case result := <-ch:
		return result, nil
	case <-time.After(timeout):
		return nil, oerrors.NewRender(chunk, -1, "fork pool timeout")
	}
}

func (l *ForkedPoolLeader) quit() {
	if l.cmd.ProcessState == nil {
		_ = l.write(map[string]any{"cmd": "quit"})
		done := make(chan struct{})
		go func() { _ = l.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = l.cmd.Process.Kill()
			<-done
		}
	}
}

func (l *ForkedPoolLeader) kill() {
	if l.cmd.ProcessState == nil {
		_ = l.cmd.Process.Kill()
	}
	_ = l.cmd.Wait()
}

// ForkedWorkerPool has the same interface as WorkerPool, backed by a single
// ForkedPoolLeader. Ported from ForkedWorkerPool.
type ForkedWorkerPool struct {
	settings  *config.Settings
	format    types.FormatName
	leader    *ForkedPoolLeader
	pageCount int
	havePages bool
}

// NewForkedWorkerPool spawns the leader and loads the document.
func NewForkedWorkerPool(s *config.Settings, format types.FormatName, input, requestID string, stores Stores, poolSize int) (*ForkedWorkerPool, error) {
	leader, err := spawnForkedLeader(s, format, input, requestID, stores, poolSize)
	if err != nil {
		return nil, err
	}
	oclog.EmitEvent("info", "fork_pool_spawn", map[string]any{"worker": format, "pool_size": poolSize, "pid": leader.pid})
	pages, err := leader.LoadDocument(input)
	if err != nil {
		leader.kill()
		return nil, err
	}
	oclog.EmitEvent("info", "fork_pool_loaded", map[string]any{"worker": format, "page_count": pages})
	return &ForkedWorkerPool{settings: s, format: format, leader: leader, pageCount: pages, havePages: true}, nil
}

// RenderChunk renders one chunk via the leader.
func (p *ForkedWorkerPool) RenderChunk(ctx context.Context, chunk types.Chunk, scratchDir string) (string, error) {
	outputPath := fmt.Sprintf("%s/%s.pdf", scratchDir, ChunkFileStem(chunk))
	return p.leader.RenderChunk(chunk, outputPath, time.Duration(p.settings.ChunkTimeoutSeconds)*time.Second)
}

// ActualPageCount returns the leader's reported page count.
func (p *ForkedWorkerPool) ActualPageCount() (int, bool) { return p.pageCount, p.havePages }

// Close shuts the leader down.
func (p *ForkedWorkerPool) Close(ctx context.Context) error {
	if p.leader != nil {
		p.leader.quit()
	}
	return nil
}
