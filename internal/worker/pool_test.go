package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opus2/office-convert-orchestrator/internal/config"
	"github.com/opus2/office-convert-orchestrator/internal/obs"
	"github.com/opus2/office-convert-orchestrator/internal/oerrors"
	"github.com/opus2/office-convert-orchestrator/internal/types"
)

// buildFakeWorker compiles the fake worker binary to <dir>/worker-<fmt> for the
// requested formats and returns the prefix path. Skips the test if `go` or the
// build is unavailable.
func buildFakeWorker(t *testing.T, formats ...types.FormatName) string {
	t.Helper()
	dir := t.TempDir()
	base := filepath.Join(dir, "worker")
	// Build once, then copy to each per-format name.
	first := base + "-" + string(formats[0])
	cmd := exec.Command("go", "build", "-o", first, "./testdata/fakeworker")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot build fake worker: %v", err)
	}
	for _, f := range formats[1:] {
		b, err := os.ReadFile(first)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(base+"-"+string(f), b, 0o755))
	}
	return base
}

func testSettings(prefix string) *config.Settings {
	return &config.Settings{
		WorkerBinaryPrefix:  prefix,
		WorkerRAMBytes:      16 << 30, // generous so prlimit --as doesn't choke the Go fake worker
		ChunkTimeoutSeconds: 30,
		LicensePath:         "/tmp/fake.lic",
		Parallel:            2,
	}
}

func testStores() Stores {
	return Stores{
		Heartbeats: obs.HeartbeatStore(),
		Timings:    obs.TimingStore(),
		Progress:   obs.NewJobProgressStore(),
	}
}

func mkInput(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "input.docx")
	require.NoError(t, os.WriteFile(p, []byte("fake document bytes"), 0o644))
	return p
}

func renderChunks(t *testing.T, pool Pool, scratch string, n int) []string {
	t.Helper()
	paths := make([]string, n)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := types.Chunk{Index: float64(i), PageStart: i*10 + 1, PageEnd: i*10 + 10}
			p, err := pool.RenderChunk(context.Background(), c, scratch)
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			paths[i] = p
		}(i)
	}
	wg.Wait()
	require.NoError(t, firstErr, "render error")
	return paths
}

func TestForkedWorkerPoolRendersConcurrently(t *testing.T) {
	prefix := buildFakeWorker(t, types.FormatDOCX)
	s := testSettings(prefix)
	stores := testStores()
	scratch := t.TempDir()

	pool, err := NewForkedWorkerPool(s, types.FormatDOCX, mkInput(t), "rid-fork", stores, 2)
	require.NoError(t, err, "spawn")
	defer pool.Close(context.Background())

	pc, ok := pool.ActualPageCount()
	require.True(t, ok, "page count should be known")
	require.Equal(t, 5, pc)

	paths := renderChunks(t, pool, scratch, 3)
	for i, p := range paths {
		require.NotEmptyf(t, p, "chunk %d returned empty path", i)
		b, err := os.ReadFile(p)
		require.NoErrorf(t, err, "chunk %d output", i)
		require.NotEmptyf(t, b, "chunk %d output missing", i)
	}

	// The fake worker emits one load heartbeat; the stderr tailer must have
	// recorded it under the request id (the GIL->mutex store path).
	assert.NotEmpty(t, stores.Heartbeats.Get("rid-fork"), "expected at least one heartbeat recorded for rid-fork")
}

func TestWorkerPoolLegacyRenders(t *testing.T) {
	prefix := buildFakeWorker(t, types.FormatXLSX)
	s := testSettings(prefix)
	scratch := t.TempDir()

	pool, err := NewWorkerPool(s, types.FormatXLSX, mkInput(t), "rid-legacy", testStores(), 2)
	require.NoError(t, err, "spawn")
	defer pool.Close(context.Background())

	pc, ok := pool.ActualPageCount()
	require.True(t, ok, "page count should be known")
	require.Equal(t, 5, pc)
	paths := renderChunks(t, pool, scratch, 4)
	for i, p := range paths {
		b, err := os.ReadFile(p)
		require.NoErrorf(t, err, "chunk %d output", i)
		require.NotEmptyf(t, b, "chunk %d output missing", i)
	}
}

// A legacy pool worker that dies mid-render must not go back into rotation:
// it is replaced, so the next chunk renders instead of failing on a dead pipe.
func TestWorkerPoolReplacesDeadWorker(t *testing.T) {
	prefix := buildFakeWorker(t, types.FormatXLSX)
	t.Setenv("FAKE_CRASH_PAGE", "1")
	s := testSettings(prefix)
	scratch := t.TempDir()

	pool, err := NewWorkerPool(s, types.FormatXLSX, mkInput(t), "rid-dead", testStores(), 1)
	require.NoError(t, err, "spawn")
	defer pool.Close(context.Background())

	_, err = pool.RenderChunk(context.Background(), types.Chunk{PageStart: 1, PageEnd: 2}, scratch)
	require.Error(t, err, "crashing chunk must fail")

	p, err := pool.RenderChunk(context.Background(), types.Chunk{Index: 1, PageStart: 3, PageEnd: 4}, scratch)
	require.NoError(t, err, "next chunk must get a live (replacement) worker")
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	require.NotEmpty(t, b)
}

func TestLegacyPoolHeartbeatPoolIndexDistinct(t *testing.T) {
	// The fake worker emits pool_index=0 in every heartbeat (each legacy worker
	// is its own process). The legacy pool MUST override with each worker's
	// assigned index, so the dashboard shows N distinct workers — not all
	// collapsed onto pool_index 0 (the bug the UI surfaced).
	prefix := buildFakeWorker(t, types.FormatXLSX)
	s := testSettings(prefix)
	stores := testStores()
	scratch := t.TempDir()

	pool, err := NewWorkerPool(s, types.FormatXLSX, mkInput(t), "rid-idx", stores, 3)
	require.NoError(t, err, "spawn")
	defer pool.Close(context.Background())
	renderChunks(t, pool, scratch, 3)

	idxs := map[int]bool{}
	for _, hb := range stores.Heartbeats.Get("rid-idx") {
		if v, ok := hb["pool_index"]; ok {
			idxs[toInt(v, -1)] = true
		}
	}
	require.GreaterOrEqualf(t, len(idxs), 2, "legacy workers collapsed onto pool_index %v — expected distinct indices", idxs)
}

func TestForkedPoolMapsOOMError(t *testing.T) {
	prefix := buildFakeWorker(t, types.FormatDOCX)
	t.Setenv("FAKE_EXIT_CODE", "137") // worker replies error code 137 on render
	s := testSettings(prefix)
	scratch := t.TempDir()

	pool, err := NewForkedWorkerPool(s, types.FormatDOCX, mkInput(t), "rid-oom", testStores(), 1)
	require.NoError(t, err, "spawn")
	defer pool.Close(context.Background())

	_, rerr := pool.RenderChunk(context.Background(), types.Chunk{Index: 0, PageStart: 1, PageEnd: 10}, scratch)
	var oe *oerrors.Error
	require.ErrorAs(t, rerr, &oe, "expected render error")
	require.True(t, oe.OOM, "expected OOM error")
}
