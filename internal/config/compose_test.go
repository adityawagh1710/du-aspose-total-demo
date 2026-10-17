package config

import (
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// composeInt reads an OFFICE_CONVERT_* value from compose.yaml, accepting both
// "N" and "${VAR:-N}" forms.
func composeInt(t *testing.T, compose, name string) int64 {
	t.Helper()
	m := regexp.MustCompile(`OFFICE_CONVERT_` + name + `: "(?:\$\{[A-Z_]+:-)?(\d+)`).FindStringSubmatch(compose)
	require.NotNilf(t, m, "compose.yaml has no OFFICE_CONVERT_%s", name)
	n, err := strconv.ParseInt(m[1], 10, 64)
	require.NoError(t, err)
	return n
}

// The orchestrator's worst-case worker memory must fit inside the container's
// RAM+swap, or the cgroup OOM killer fires before any worker hits its
// RLIMIT_AS and the OOM -> subdivide retry can never run.
func TestComposeMemoryBudget(t *testing.T) {
	b, err := os.ReadFile("../../compose.yaml")
	require.NoError(t, err)
	compose := string(b)

	m := regexp.MustCompile(`memswap_limit: (\d+)g`).FindStringSubmatch(compose)
	require.NotNil(t, m, "office-convert service needs memswap_limit")
	gb, _ := strconv.ParseInt(m[1], 10, 64)
	budget := gb << 30

	jobs := composeInt(t, compose, "MAX_JOBS")
	ram := composeInt(t, compose, "WORKER_RAM_BYTES")
	for _, workers := range []string{"PARALLEL", "XLSX_MAX_POOL_SIZE"} {
		need := jobs * composeInt(t, compose, workers) * ram
		require.LessOrEqualf(t, need, budget,
			"MAX_JOBS x %s x WORKER_RAM_BYTES = %d exceeds memswap_limit %d", workers, need, budget)
	}
}
