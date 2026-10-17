package probe

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/opus2/office-convert-orchestrator/internal/config"
	"github.com/opus2/office-convert-orchestrator/internal/oerrors"
	"github.com/opus2/office-convert-orchestrator/internal/types"
	"github.com/opus2/office-convert-orchestrator/internal/worker"
)

// staleMetadataSizeThreshold: a DOCX over this size claiming 1 page in app.xml
// is treated as stale and gets a size-based estimate instead.
const staleMetadataSizeThreshold = 200_000

// bytesPerPageEstimate — conservative (LOW) per-format byte-per-page figures
// for size-based fallback. Low => over-estimate pages => over-chunk (safe).
var bytesPerPageEstimate = map[types.FormatName]int64{
	types.FormatDOCX: 20_000,
	types.FormatPPTX: 100_000,
	types.FormatXLSX: 50_000,
	types.FormatPDF:  30_000,
}

var qpdfNPagesRE = regexp.MustCompile(`^\s*(\d+)\s*$`)

// ProbeLite is a best-effort metadata-only probe. Returns nil on any failure
// (caller falls through to the C++ Aspose worker). Mirrors probe_lite.probe_lite.
func ProbeLite(ctx context.Context, inputPath string, format types.FormatName) *types.ProbeResult {
	info, err := os.Stat(inputPath)
	if err != nil {
		return nil
	}
	size := info.Size()

	var n int
	switch format {
	case types.FormatDOCX:
		c := ooxmlCount(inputPath, "Pages")
		switch {
		case c <= 0:
			n = estimatePagesFromSize(size, format)
		case c == 1 && size > staleMetadataSizeThreshold:
			n = estimatePagesFromSize(size, format)
		default:
			n = c
		}
	case types.FormatPPTX:
		n = ooxmlCount(inputPath, "Slides")
		if n <= 0 {
			n = pptxSlideCountFromZip(inputPath)
		}
		if n <= 0 {
			n = estimatePagesFromSize(size, format)
		}
	case types.FormatXLSX:
		// XLSX always falls through to the C++ probe (rendered page count, not
		// worksheet count — see probe_lite.py).
		return nil
	case types.FormatPDF:
		n = qpdfPageCount(ctx, inputPath)
	default:
		return nil
	}

	if n <= 0 {
		return nil
	}
	return &types.ProbeResult{PageCount: n, Format: format, NaturalSeams: nil, SizeBytes: size}
}

// Probe is the two-tier probe: metadata-only first, full Aspose probe as
// fallback, with a format-mismatch retry. Mirrors probe.probe.
func Probe(ctx context.Context, s *config.Settings, inputPath string, format types.FormatName, requestID string) (types.ProbeResult, error) {
	if lite := ProbeLite(ctx, inputPath, format); lite != nil {
		return *lite, nil
	}
	return ProbeWorker(ctx, s, inputPath, format, requestID)
}

// ProbeWorker runs the full Aspose probe (the document is loaded and
// paginated), so its page count is exact, unlike ProbeLite's metadata.
func ProbeWorker(ctx context.Context, s *config.Settings, inputPath string, format types.FormatName, requestID string) (types.ProbeResult, error) {
	stdout, _, err := worker.RunWorker(ctx, s, "probe", inputPath, format, "", nil, requestID, nil)
	if err == nil {
		return ParseProbeJSON(stdout)
	}

	// Format-mismatch retry: if the worker rejected the file with
	// input_unprocessable and the message hints a different format, retry once.
	if oe, ok := err.(*oerrors.Error); ok && oe.FailureClass == types.InputUnprocessable {
		if retry := mismatchRetryFormat(strings.ToLower(err.Error()), format); retry != "" {
			if stdout2, _, err2 := worker.RunWorker(ctx, s, "probe", inputPath, retry, "", nil, requestID, nil); err2 == nil {
				if pr, perr := ParseProbeJSON(stdout2); perr == nil {
					pr.Format = retry
					return pr, nil
				}
			}
		}
	}
	return types.ProbeResult{}, err
}

// mismatchRetryFormat reproduces probe.py's hint logic, including its Python
// operator-precedence behavior: the "excel"/"powerpoint" branches OR the
// keyword test with the (keyword AND format-differs) test.
func mismatchRetryFormat(errMsg string, format types.FormatName) types.FormatName {
	switch {
	case strings.Contains(errMsg, "word doc") && format != types.FormatDOCX:
		return types.FormatDOCX
	case strings.Contains(errMsg, "excel") || (strings.Contains(errMsg, "workbook") && format != types.FormatXLSX):
		return types.FormatXLSX
	case strings.Contains(errMsg, "powerpoint") || (strings.Contains(errMsg, "presentation") && format != types.FormatPPTX):
		return types.FormatPPTX
	}
	return ""
}

// ooxmlCount reads <Pages> or <Slides> from docProps/app.xml. Returns 0/-1 on
// any failure (treated as "no count").
func ooxmlCount(path, element string) int {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return 0
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != "docProps/app.xml" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return 0
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return 0
		}
		var props struct {
			Pages  *int `xml:"Pages"`
			Slides *int `xml:"Slides"`
		}
		if err := xml.Unmarshal(data, &props); err != nil {
			return 0
		}
		if element == "Pages" && props.Pages != nil {
			return *props.Pages
		}
		if element == "Slides" && props.Slides != nil {
			return *props.Slides
		}
		return 0
	}
	return 0
}

// pptxSlideCountFromZip counts ppt/slides/slideN.xml entries.
func pptxSlideCountFromZip(path string) int {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return 0
	}
	defer zr.Close()
	count := 0
	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "ppt/slides/slide") && strings.HasSuffix(f.Name, ".xml") {
			count++
		}
	}
	return count
}

func estimatePagesFromSize(size int64, format types.FormatName) int {
	bpp, ok := bytesPerPageEstimate[format]
	if !ok {
		bpp = 20_000
	}
	est := size / bpp
	if est < 1 {
		est = 1
	}
	return int(est)
}

// qpdfPageCount shells out to `qpdf --show-npages`. Returns 0 on any failure.
func qpdfPageCount(ctx context.Context, path string) int {
	out, err := exec.Command("qpdf", "--show-npages", path).Output()
	if err != nil {
		return 0
	}
	m := qpdfNPagesRE.FindSubmatch(out)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(string(m[1]))
	if err != nil {
		return 0
	}
	return n
}
