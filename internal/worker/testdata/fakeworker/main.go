// Command fakeworker is a test double for the C++ Aspose worker. It speaks the
// same CLI + JSON-stdio protocol so the Go orchestrator/worker/pool code can be
// integration-tested without the real Aspose binaries.
//
// Modes:
//   --mode probe   -> write a ProbeResult JSON to stdout, exit 0.
//   --mode render  -> write a tiny PDF to --output, exit 0 (one-shot).
//   --mode pool    -> read line-delimited JSON commands from stdin:
//                       {"cmd":"load",...[,"seq":N]}   -> {"status":"ok","page_count":P[,"seq":N]}
//                       {"cmd":"render","output":O,...} -> writes PDF to O, {"status":"ok","output":O[,"seq":N]}
//                       {"cmd":"quit"}                   -> exit 0
//
// Behavior is tunable via env vars so tests can assert specific shapes:
//   FAKE_PAGE_COUNT  page count reported by load/probe (default 5)
//   FAKE_EXIT_CODE   if set non-zero, render replies error with this code
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
)

const tinyPDF = "%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\ntrailer<</Root 1 0 R>>\n%%EOF\n"

func main() {
	args := parseArgs(os.Args[1:])
	switch args["mode"] {
	case "probe":
		emitProbe(args["format"])
	case "render":
		writePDF(args["output"])
	case "pool":
		poolLoop()
	default:
		fmt.Fprintln(os.Stderr, "unknown mode")
		os.Exit(1)
	}
}

func parseArgs(argv []string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(argv); i += 2 {
		key := argv[i]
		if len(key) > 2 && key[:2] == "--" {
			m[key[2:]] = argv[i+1]
		}
	}
	return m
}

func pageCount() int {
	if v := os.Getenv("FAKE_PAGE_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 5
}

func emitProbe(format string) {
	out := map[string]any{
		"page_count":    pageCount(),
		"format":        format,
		"size_bytes":    int64(len(tinyPDF)),
		"natural_seams": []any{},
	}
	b, _ := json.Marshal(out)
	os.Stdout.Write(b)
}

func writePDF(path string) {
	if path != "" {
		_ = os.WriteFile(path, []byte(tinyPDF), 0o644)
	}
}

func poolLoop() {
	// One heartbeat to exercise the stderr tailer + store.
	fmt.Fprintln(os.Stderr, `{"type":"heartbeat","phase":"load","pool_index":0,"elapsed_s":0,"rss_bytes":1000,"swap_bytes":0,"cpu_jiffies":1}`)

	exitCode := 0
	if v := os.Getenv("FAKE_EXIT_CODE"); v != "" {
		exitCode, _ = strconv.Atoi(v)
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	w := bufio.NewWriter(os.Stdout)
	for sc.Scan() {
		line := sc.Bytes()
		var cmd map[string]any
		if err := json.Unmarshal(line, &cmd); err != nil {
			continue
		}
		resp := map[string]any{}
		if seq, ok := cmd["seq"]; ok {
			resp["seq"] = seq
		}
		switch cmd["cmd"] {
		case "load":
			resp["status"] = "ok"
			resp["page_count"] = pageCount()
		case "render":
			// FAKE_CRASH_PAGE=N: die without replying when asked to render a
			// chunk starting at page N (a worker crash mid-render).
			if v := os.Getenv("FAKE_CRASH_PAGE"); v != "" {
				if ps, ok := cmd["page_start"].(float64); ok && fmt.Sprint(int(ps)) == v {
					os.Exit(3)
				}
			}
			if exitCode != 0 {
				resp["status"] = "error"
				resp["code"] = exitCode
				resp["detail"] = "fake render failure"
			} else {
				if out, _ := cmd["output"].(string); out != "" {
					_ = os.WriteFile(out, []byte(tinyPDF), 0o644)
					resp["output"] = out
				}
				resp["status"] = "ok"
			}
		case "quit":
			w.Flush()
			os.Exit(0)
		default:
			resp["status"] = "error"
			resp["detail"] = "unknown cmd"
		}
		b, _ := json.Marshal(resp)
		w.Write(b)
		w.WriteByte('\n')
		w.Flush()
	}
}
