package server

import (
	"strings"
	"testing"
	"time"
)

func TestTimingsReportIncludesRuntimeAndPlayerStats(t *testing.T) {
	timings := newTickTimings(func() (int, int) { return 3, 20 })
	timings.commit(10 * time.Millisecond)
	report := timings.Report()
	for _, expected := range []string{`RAM:`, `CPU:`, `Players:`, `3/20`} {
		if !strings.Contains(report, expected) {
			t.Errorf(`timings report does not contain %q: %s`, expected, report)
		}
	}
	if plain := stripMinecraftFormatting(report); strings.ContainsRune(plain, '§') {
		t.Fatalf(`plain console report still contains formatting: %q`, plain)
	}
}
