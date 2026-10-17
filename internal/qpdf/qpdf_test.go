package qpdf

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opus2/office-convert-orchestrator/internal/oerrors"
	"github.com/opus2/office-convert-orchestrator/internal/types"
)

// writeFakeQpdf writes a shell script standing in for qpdf and points the
// package Binary at it. The script writes `body` to stdout and exits `code`.
func writeFakeQpdf(t *testing.T, body string, code int) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeqpdf")
	script := "#!/bin/sh\nprintf '" + body + "'\nexit " + itoa(code) + "\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	orig := Binary
	Binary = path
	t.Cleanup(func() { Binary = orig })
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	return string(rune('0' + i))
}

func TestConcatStreamingStreamsAndTees(t *testing.T) {
	writeFakeQpdf(t, "MERGEDPDFBYTES", 0)
	teeDir := t.TempDir()
	teePath := filepath.Join(teeDir, "cache.tmp")

	var buf bytes.Buffer
	err := ConcatStreaming(context.Background(), []string{"a.pdf", "b.pdf"}, &buf, teePath)
	require.NoError(t, err)
	require.Equal(t, "MERGEDPDFBYTES", buf.String())
	teed, err := os.ReadFile(teePath)
	require.NoError(t, err)
	require.Equal(t, "MERGEDPDFBYTES", string(teed))
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }

// When the destination stops accepting bytes (client disconnected), qpdf must
// be killed rather than left blocked on a full stdout pipe, which hung the
// handler and leaked its job slot forever.
func TestConcatStreamingDestWriteFailureDoesNotHang(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeqpdf")
	// One process (like real qpdf) that writes to stdout forever.
	script := "#!/bin/sh\nexec yes\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
	orig := Binary
	Binary = path
	t.Cleanup(func() { Binary = orig })

	done := make(chan error, 1)
	go func() { done <- ConcatStreaming(context.Background(), []string{"a.pdf"}, failingWriter{}, "") }()
	select {
	case err := <-done:
		var oe *oerrors.Error
		require.ErrorAs(t, err, &oe)
		require.Equal(t, types.MergeFailed, oe.FailureClass)
	case <-time.After(10 * time.Second):
		t.Fatal("ConcatStreaming hung after the destination write failed")
	}
}

func TestConcatStreamingNoChunks(t *testing.T) {
	var buf bytes.Buffer
	err := ConcatStreaming(context.Background(), nil, &buf, "")
	var oe *oerrors.Error
	require.ErrorAs(t, err, &oe)
	require.Equal(t, types.MergeFailed, oe.FailureClass)
}

func TestConcatStreamingQpdfFailureMapsToMergeError(t *testing.T) {
	writeFakeQpdf(t, "boom", 2)
	teePath := filepath.Join(t.TempDir(), "cache.tmp")
	var buf bytes.Buffer
	err := ConcatStreaming(context.Background(), []string{"a.pdf"}, &buf, teePath)
	var oe *oerrors.Error
	require.ErrorAs(t, err, &oe)
	require.Equal(t, types.MergeFailed, oe.FailureClass)
	// Partial tee file must be cleaned up on failure.
	_, statErr := os.Stat(teePath)
	assert.True(t, os.IsNotExist(statErr), "tee file should be removed on qpdf failure")
}
