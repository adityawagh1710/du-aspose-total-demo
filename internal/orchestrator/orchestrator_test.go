package orchestrator

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"pgregory.net/rapid"

	"github.com/opus2/office-convert-orchestrator/internal/cache"
	"github.com/opus2/office-convert-orchestrator/internal/config"
	"github.com/opus2/office-convert-orchestrator/internal/obs"
	"github.com/opus2/office-convert-orchestrator/internal/oerrors"
	"github.com/opus2/office-convert-orchestrator/internal/planner"
	"github.com/opus2/office-convert-orchestrator/internal/qpdf"
	"github.com/opus2/office-convert-orchestrator/internal/types"
	"github.com/opus2/office-convert-orchestrator/internal/worker"
)

// fakeQpdf stands in for qpdf: `--empty --pages a b ... -- out` concatenates
// the input files, one per line, so tests can read back the page order.
func fakeQpdf(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qpdf")
	script := "#!/bin/sh\nshift 2\nfor f in \"$@\"; do [ \"$f\" = -- ] && break; cat \"$f\"; done\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	orig := qpdf.Binary
	qpdf.Binary = path
	t.Cleanup(func() { qpdf.Binary = orig })
}

func testDeps(t *testing.T, dir string) Deps {
	t.Helper()
	c, err := cache.NewManager("", "test")
	require.NoError(t, err)
	return Deps{
		Settings: &config.Settings{
			Parallel: 2, MaxPagesPerChunk: 10000, MaxMBPerChunk: 50, PoolMinChunks: 2,
			WorkerRAMBytes: 6 << 30, ChunkTimeoutSeconds: 30, LicensePath: filepath.Join(dir, "x.lic"),
			WorkerBinaryPrefix: filepath.Join(dir, "worker"),
		},
		Cache:  c,
		Stores: worker.Stores{Heartbeats: obs.HeartbeatStore(), Timings: obs.TimingStore(), Progress: obs.NewJobProgressStore()},
	}
}

// pagesOf lists the pages a fake chunk output (one "p<start>-<end>" line per
// chunk) covers, in order.
func pagesOf(out string) []int {
	var pages []int
	for _, line := range strings.Fields(out) {
		var a, b int
		if _, err := fmt.Sscanf(line, "p%d-%d", &a, &b); err == nil {
			for p := a; p <= b; p++ {
				pages = append(pages, p)
			}
		}
	}
	return pages
}

func seq(n int) []int {
	s := make([]int, n)
	for i := range s {
		s[i] = i + 1
	}
	return s
}

// The OOM retry shared by one-shot AND pool mode must still yield every page
// exactly once, in order, when chunks OOM repeatedly.
func TestRenderWithRetrySubdividesOnOOM(t *testing.T) {
	fakeQpdf(t)
	dir := t.TempDir()
	deps := testDeps(t, dir)
	renderOne := func(ctx context.Context, c types.Chunk) (string, error) {
		if c.Pages() > 2 {
			return "", oerrors.NewOOM(c)
		}
		path := filepath.Join(dir, worker.ChunkFileStem(c)+".pdf")
		return path, os.WriteFile(path, []byte(fmt.Sprintf("p%d-%d\n", c.PageStart, c.PageEnd)), 0o644)
	}
	got, err := renderWithRetry(context.Background(), types.Chunk{Index: 0, PageStart: 1, PageEnd: 11},
		renderOne, types.FormatDOCX, deps, dir, "rid", "", false, &counters{}, 0)
	require.NoError(t, err)
	out, err := os.ReadFile(got)
	require.NoError(t, err)
	require.Equal(t, seq(11), pagesOf(string(out)))
}

// A DOCX whose docProps/app.xml claims 3 pages but which really has 9 must
// render all 9 in one-shot mode (previously pages 4-9 were dropped).
func TestOneShotUsesExactPageCount(t *testing.T) {
	fakeQpdf(t)
	t.Setenv("OFFICE_CONVERT_POOL_MODE", "0")
	dir := t.TempDir()
	deps := testDeps(t, dir)

	worker := "#!/bin/sh\nmode=; out=; range=\nwhile [ $# -gt 0 ]; do case \"$1\" in --mode) mode=$2;; --output) out=$2;; --page-range) range=$2;; esac; shift; done\n" +
		"if [ \"$mode\" = probe ]; then echo '{\"page_count\":9,\"format\":\"docx\",\"size_bytes\":100,\"natural_seams\":[]}'; exit 0; fi\n" +
		"echo \"p$range\" > \"$out\"\n"
	require.NoError(t, os.WriteFile(deps.Settings.WorkerBinaryPrefix+"-docx", []byte(worker), 0o755))

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, err := zw.Create("docProps/app.xml")
	require.NoError(t, err)
	_, _ = f.Write([]byte(`<Properties><Pages>3</Pages></Properties>`))
	require.NoError(t, zw.Close())
	input := filepath.Join(dir, "input.docx")
	require.NoError(t, os.WriteFile(input, buf.Bytes(), 0o644))

	var out bytes.Buffer
	_, err = ConvertJob(context.Background(), "rid", input, types.FormatDOCX,
		types.ConversionOptions{}, deps, dir, &out, nil)
	require.NoError(t, err)
	pages := pagesOf(out.String())
	sort.Ints(pages)
	require.Equal(t, seq(9), pages)
}

// Every file a request can write during OOM subdivision (each chunk's render
// output and each parent's merged output) must have a distinct name, and a
// merge must never write over one of its own inputs. Float-index naming broke
// this: subdividing 3.5 produced 3.5 and 4.0, overwriting top-level chunk 4.
func TestSubdivisionFileNamesNeverCollide(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		pages := rapid.IntRange(1, 400).Draw(t, "pages")
		nChunks := rapid.IntRange(1, min(pages, 12)).Draw(t, "chunks")

		var plan []types.Chunk
		start := 1
		for i := 0; i < nChunks; i++ {
			end := start + (pages-start+1)/(nChunks-i) - 1
			plan = append(plan, types.Chunk{Index: float64(i), PageStart: start, PageEnd: end})
			start = end + 1
		}

		seen := map[string]bool{}
		claim := func(name string) {
			if seen[name] {
				t.Fatalf("file name %q written twice", name)
			}
			seen[name] = true
		}
		// Worst case: every chunk OOMs at every level down to the floor.
		var walk func(c types.Chunk)
		walk = func(c types.Chunk) {
			claim(worker.ChunkFileStem(c) + ".pdf")
			subs := planner.Subdivide(c)
			if len(subs) == 0 {
				return
			}
			for _, sc := range subs {
				walk(sc)
			}
			claim(worker.ChunkFileStem(c) + "-merged.pdf")
		}
		for _, c := range plan {
			walk(c)
		}
	})
}
