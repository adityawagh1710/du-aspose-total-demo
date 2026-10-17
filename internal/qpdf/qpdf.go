// Package qpdf is the qpdf streaming-concat wrapper.
//
// Ported from office_convert/qpdf.py. Implements FR-3 (merge step) and NFR-1
// (no full output buffering). Spawns `qpdf --empty --pages <list> -- -` and
// copies stdout to the caller's writer in 64 KB blocks, with an optional
// tee-to-disk for the cache write.
//
// Python's async generator yielding 64 KB blocks becomes io.Copy semantics:
// the caller passes an io.Writer (the http.Flusher-backed response writer or a
// cache file), and we stream into it block by block.
package qpdf

import (
	"context"
	"io"
	"os"
	"os/exec"

	"github.com/opus2/office-convert-orchestrator/internal/oerrors"
)

const readBlockSize = 65536 // 64 KB; matches typical Linux pipe-buffer behavior.

// Binary is the qpdf executable name/path.
var Binary = "qpdf"

// ConcatStreaming streams the concatenation of chunkPaths into dst in 64 KB
// blocks. If cacheTempPath != "", streamed bytes are also tee'd to that file
// (the caller renames it into the cache atomically on success). Mirrors
// concat_streaming. Returns a MergeError if qpdf exits non-zero.
func ConcatStreaming(ctx context.Context, chunkPaths []string, dst io.Writer, cacheTempPath string) error {
	if len(chunkPaths) == 0 {
		return oerrors.NewMerge(-1, "no chunks to concatenate")
	}

	argv := append([]string{"--empty", "--pages"}, chunkPaths...)
	argv = append(argv, "--", "-")
	// CommandContext: a cancelled request (client gone) kills qpdf.
	cmd := exec.CommandContext(ctx, Binary, argv...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return oerrors.NewMerge(-1, "stdout pipe: "+err.Error())
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return oerrors.NewMerge(-1, "stderr pipe: "+err.Error())
	}
	if err := cmd.Start(); err != nil {
		return oerrors.NewMerge(-1, "spawn failed: "+err.Error())
	}

	// Drain stderr concurrently so qpdf never blocks on a full stderr pipe.
	var stderrBytes []byte
	stderrDone := make(chan struct{})
	go func() { stderrBytes, _ = io.ReadAll(stderr); close(stderrDone) }()

	var tee *os.File
	if cacheTempPath != "" {
		tee, err = os.Create(cacheTempPath)
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			<-stderrDone
			return oerrors.NewMerge(-1, "cache temp open: "+err.Error())
		}
	}

	var w io.Writer = dst
	if tee != nil {
		w = io.MultiWriter(dst, tee)
	}

	buf := make([]byte, readBlockSize)
	_, copyErr := io.CopyBuffer(w, stdout, buf)
	if copyErr != nil {
		// Nothing reads qpdf's stdout any more, so it would block on a full
		// pipe forever (and stderr never hit EOF). Kill it before waiting.
		_ = cmd.Process.Kill()
	}
	if tee != nil {
		_ = tee.Sync()
		_ = tee.Close()
	}

	<-stderrDone
	waitErr := cmd.Wait()
	rc := 0
	if cmd.ProcessState != nil {
		rc = cmd.ProcessState.ExitCode()
	}
	if rc != 0 || waitErr != nil || copyErr != nil {
		if cacheTempPath != "" {
			_ = os.Remove(cacheTempPath)
		}
		return oerrors.NewMerge(rc, tailStr(stderrBytes, 1024))
	}
	return nil
}

// ConcatToFile concatenates chunkPaths into a single file at outPath (no
// streaming to a caller needed). Used for sub-chunk merge after OOM
// subdivision. Mirrors orchestrator._merge_to_file.
func ConcatToFile(ctx context.Context, chunkPaths []string, outPath string) error {
	out, err := os.Create(outPath)
	if err != nil {
		return oerrors.NewMerge(-1, "open merge target: "+err.Error())
	}
	defer out.Close()
	return ConcatStreaming(ctx, chunkPaths, out, "")
}

func tailStr(b []byte, n int) string {
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return string(b)
}
