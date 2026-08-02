package server

import (
	"fmt"
	"runtime"
	"runtime/metrics"
	"strings"
	"sync"
	"time"
)

// tickSection identifies a named subsystem measured within a single game tick.
type tickSection int

const (
	sectionDamage       tickSection = iota // DrainEntityDamage + health application
	sectionAI                              // mob wander / golem / panic AI
	sectionPhysics                         // gravity, position integration, ground detection
	sectionBroadcast                       // DispatchTickBroadcast (packet build, not net I/O)
	sectionTime                            // time-of-day, crop ticks, time-skip drain
	sectionSpawnPassive                    // passive mob natural spawn loop
	sectionSpawnHostile                    // hostile mob natural spawn loop
	sectionBlockPhysics                    // falling blocks + fluid spreading
	sectionCount
)

var sectionNames = [sectionCount]string{
	"damage", "mob-ai", "physics", "broadcast", "time/crops", "spawn-passive", "spawn-hostile", "block-physics",
}

// windowSize is the number of ticks kept in the rolling timing window.
// 600 ticks = 30 seconds at 20 TPS.
const windowSize = 600

// tickTimings records per-subsystem durations over a rolling window of ticks.
// It is written only by the entity tick goroutine (commit, cur) and read
// concurrently by command handlers (Report, TPS) under mu.
type tickTimings struct {
	mu sync.Mutex

	// rolling window of per-tick total durations (nanoseconds)
	window     [windowSize]int64
	windowPos  int
	windowFill int // how many slots are populated

	// per-section rolling totals — same window layout as window[]
	sections [sectionCount][windowSize]int64

	// all-time peak tick duration (nanoseconds), updated atomically by commit
	peakTickNS int64

	// cur accumulates per-section nanoseconds for the tick in progress.
	// Written only by the tick goroutine; reset in commit.
	cur [sectionCount]int64

	// Runtime samples power the RAM/CPU/player summary shown by /timings.
	lastCPUTotal float64
	lastCPUIdle  float64
	cpuReady     bool
	playerCounts func() (online, maximum int)
}

func newTickTimings(playerCounters ...func() (online, maximum int)) *tickTimings {
	total, idle, ready := readRuntimeCPU()
	t := &tickTimings{
		lastCPUTotal: total,
		lastCPUIdle:  idle,
		cpuReady:     ready,
	}
	if len(playerCounters) > 0 {
		t.playerCounts = playerCounters[0]
	}
	return t
}

// measure starts timing section s.  The caller must invoke the returned
// function when the section ends; the elapsed time is added to cur[s].
// Safe to call multiple times per tick for the same section — durations
// accumulate (e.g. AI measured once per entity across the entity loop).
func (t *tickTimings) measure(s tickSection) func() {
	start := time.Now()
	return func() {
		t.cur[s] += time.Since(start).Nanoseconds()
	}
}

// commit records the completed tick into the rolling window and resets cur.
// Must be called once per tick by the tick goroutine.
func (t *tickTimings) commit(total time.Duration) {
	ns := total.Nanoseconds()

	t.mu.Lock()
	pos := t.windowPos
	t.window[pos] = ns
	for i := tickSection(0); i < sectionCount; i++ {
		t.sections[i][pos] = t.cur[i]
		t.cur[i] = 0
	}
	t.windowPos = (pos + 1) % windowSize
	if t.windowFill < windowSize {
		t.windowFill++
	}
	if ns > t.peakTickNS {
		t.peakTickNS = ns
	}
	t.mu.Unlock()
}

// TPS returns the current TPS (capped at 20) and average tick duration in ms
// based on the rolling window.
func (t *tickTimings) TPS() (tps float64, avgMs float64) {
	t.mu.Lock()
	fill := t.windowFill
	window := t.window
	t.mu.Unlock()

	if fill == 0 {
		return 20, 0
	}
	var totalNS int64
	for i := 0; i < fill; i++ {
		totalNS += window[i]
	}
	avgMs = float64(totalNS) / float64(fill) / 1e6
	tps = 1000.0 / avgMs
	if tps > 20 {
		tps = 20
	}
	return tps, avgMs
}

// Report returns a multi-line Spark-style timings report (Minecraft colour codes).
func (t *tickTimings) Report() string {
	t.mu.Lock()
	fill := t.windowFill
	window := t.window
	sections := t.sections
	peakNS := t.peakTickNS
	playerCounts := t.playerCounts
	t.mu.Unlock()
	stats := t.runtimeSnapshot()
	online, maximum := 0, 0
	if playerCounts != nil {
		online, maximum = playerCounts()
	}

	if fill == 0 {
		return strings.Join([]string{
			`§e━━━ GoCraft Timings ━━━`,
			formatRuntimeStats(stats, online, maximum),
			`§cNo timing data collected yet — wait a few seconds after startup.`,
			`§e━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━`,
		}, string('\n'))
	}

	var totalNS, maxNS int64
	for i := 0; i < fill; i++ {
		totalNS += window[i]
		if window[i] > maxNS {
			maxNS = window[i]
		}
	}
	avgMs := float64(totalNS) / float64(fill) / 1e6
	maxMs := float64(maxNS) / 1e6
	peakMs := float64(peakNS) / 1e6

	tps := 1000.0 / avgMs
	if tps > 20 {
		tps = 20
	}

	tpsColor := "§a"
	switch {
	case tps < 15:
		tpsColor = "§c"
	case tps < 18:
		tpsColor = "§e"
	}

	secs := fill / 20
	if secs < 1 {
		secs = 1
	}

	var sectionAvgMs [sectionCount]float64
	for s := tickSection(0); s < sectionCount; s++ {
		var sum int64
		for i := 0; i < fill; i++ {
			sum += sections[s][i]
		}
		sectionAvgMs[s] = float64(sum) / float64(fill) / 1e6
	}

	lines := make([]string, 0, 14)
	lines = append(lines, fmt.Sprintf("§e━━━ GoCraft Timings (last %ds, %d ticks) ━━━", secs, fill))
	lines = append(lines, formatRuntimeStats(stats, online, maximum))
	lines = append(lines, fmt.Sprintf(
		"§7TPS: %s%.1f§r  §7Avg: §f%.2fms  §7Max(window): §f%.2fms  §7Peak(all-time): §f%.2fms",
		tpsColor, tps, avgMs, maxMs, peakMs,
	))
	lines = append(lines, "§7Per-subsystem average (ms / %% of tick):")

	for s := tickSection(0); s < sectionCount; s++ {
		pct := 0.0
		if avgMs > 0 {
			pct = sectionAvgMs[s] / avgMs * 100
		}
		bar := progressBar(pct, 20, sectionAvgMs[s])
		lines = append(lines, fmt.Sprintf(
			"  §7%-15s §f%7.3f ms  %s §8%.1f%%",
			sectionNames[s], sectionAvgMs[s], bar, pct,
		))
	}

	// "other" = time unaccounted for by measured sections
	var sectionTotal float64
	for _, ms := range sectionAvgMs {
		sectionTotal += ms
	}
	other := avgMs - sectionTotal
	if other < 0 {
		other = 0
	}
	otherPct := 0.0
	if avgMs > 0 {
		otherPct = other / avgMs * 100
	}
	lines = append(lines, fmt.Sprintf(
		"  §7%-15s §f%7.3f ms  %s §8%.1f%%",
		"other/overhead", other, progressBar(otherPct, 20, other), otherPct,
	))

	lines = append(lines, "§e━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	return strings.Join(lines, "\n")
}

type runtimeStats struct {
	memoryBytes uint64
	heapBytes   uint64
	cpuPercent  float64
	cpuReady    bool
}

// runtimeSnapshot reports Go runtime memory and CPU utilisation normalised to
// the process GOMAXPROCS capacity. CPU is measured since the previous report.
func (t *tickTimings) runtimeSnapshot() runtimeStats {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	total, idle, ready := readRuntimeCPU()

	t.mu.Lock()
	cpuPercent := 0.0
	if ready && t.cpuReady {
		totalDelta := total - t.lastCPUTotal
		idleDelta := idle - t.lastCPUIdle
		if totalDelta > 0 {
			busyDelta := totalDelta - idleDelta
			if busyDelta < 0 {
				busyDelta = 0
			}
			cpuPercent = busyDelta / totalDelta * 100
			if cpuPercent > 100 {
				cpuPercent = 100
			}
		}
	}
	t.lastCPUTotal = total
	t.lastCPUIdle = idle
	t.cpuReady = ready
	t.mu.Unlock()

	return runtimeStats{
		memoryBytes: memory.Sys,
		heapBytes:   memory.HeapAlloc,
		cpuPercent:  cpuPercent,
		cpuReady:    ready,
	}
}

func readRuntimeCPU() (total, idle float64, ready bool) {
	samples := []metrics.Sample{
		{Name: `/cpu/classes/total:cpu-seconds`},
		{Name: `/cpu/classes/idle:cpu-seconds`},
	}
	metrics.Read(samples)
	if samples[0].Value.Kind() != metrics.KindFloat64 || samples[1].Value.Kind() != metrics.KindFloat64 {
		return 0, 0, false
	}
	return samples[0].Value.Float64(), samples[1].Value.Float64(), true
}

func formatRuntimeStats(stats runtimeStats, online, maximum int) string {
	cpu := `n/a`
	if stats.cpuReady {
		cpu = fmt.Sprintf(`%.1f%%`, stats.cpuPercent)
	}
	players := fmt.Sprintf(`%d`, online)
	if maximum > 0 {
		players = fmt.Sprintf(`%d/%d`, online, maximum)
	}
	return fmt.Sprintf(
		`§7RAM: §f%.1f MB §8(heap %.1f MB)  §7CPU: §f%s  §7Players: §f%s`,
		float64(stats.memoryBytes)/(1024*1024),
		float64(stats.heapBytes)/(1024*1024),
		cpu,
		players,
	)
}

func stripMinecraftFormatting(text string) string {
	runes := []rune(text)
	plain := make([]rune, 0, len(runes))
	for i := 0; i < len(runes); i++ {
		if runes[i] == '§' && i+1 < len(runes) {
			i++
			continue
		}
		plain = append(plain, runes[i])
	}
	return string(plain)
}

// progressBar returns a coloured ASCII bar.
// Color is based on the absolute millisecond value of the section, not its
// percentage share, so a dominant-but-fast section stays green.
//   - green  : absMs < 5 ms   (healthy)
//   - yellow : absMs < 15 ms  (worth watching)
//   - red    : absMs >= 15 ms (genuinely slow)
func progressBar(pct float64, width int, absMs float64) string {
	filled := int(pct / 100 * float64(width))
	if filled > width {
		filled = width
	}
	color := "§a"
	switch {
	case absMs >= 15:
		color = "§c"
	case absMs >= 5:
		color = "§e"
	}
	return color + strings.Repeat("|", filled) + "§8" + strings.Repeat("|", width-filled)
}
