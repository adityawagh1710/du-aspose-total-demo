package orchestrator

import (
	"testing"

	"pgregory.net/rapid"

	"github.com/opus2/office-convert-orchestrator/internal/planner"
	"github.com/opus2/office-convert-orchestrator/internal/types"
	"github.com/opus2/office-convert-orchestrator/internal/worker"
)

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
