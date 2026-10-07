// Package timing records wall time per named stage of one CLI invocation.
// It is diagnostic only: a disabled recorder does no work, and no command
// result depends on a recorded value.
package timing

import (
	"encoding/json"
	"io"
	"sort"
	"sync"
	"time"
)

// EnvVar enables the stage report when set to a non-empty value other than 0.
const EnvVar = "ONWARDPG_TIMINGS"

type stage struct {
	count int
	total time.Duration
	first int
}

var (
	mu      sync.Mutex
	enabled bool
	started time.Time
	stages  map[string]*stage
)

// Enable starts recording. It is called once, before any stage runs.
func Enable() {
	mu.Lock()
	defer mu.Unlock()
	enabled = true
	started = time.Now()
	stages = make(map[string]*stage)
}

// Enabled reports whether stages are recorded.
func Enabled() bool {
	mu.Lock()
	defer mu.Unlock()
	return enabled
}

// Start opens one span of the named stage and returns the function that
// closes it. Spans of one name are summed; nested names are independent, so a
// parent stage includes the time of the stages it contains.
func Start(name string) func() {
	if !Enabled() {
		return func() {}
	}
	begin := time.Now()
	return func() {
		elapsed := time.Since(begin)
		mu.Lock()
		defer mu.Unlock()
		entry := stages[name]
		if entry == nil {
			entry = &stage{first: len(stages)}
			stages[name] = entry
		}
		entry.count++
		entry.total += elapsed
	}
}

// Stage is one line of the report.
type Stage struct {
	Name         string  `json:"name"`
	Count        int     `json:"count"`
	Milliseconds float64 `json:"ms"`
}

// Report is the JSON document written to standard error.
type Report struct {
	TotalMilliseconds float64 `json:"total_ms"`
	Stages            []Stage `json:"stages"`
}

// Snapshot returns the stages in the order each name first completed.
func Snapshot() Report {
	mu.Lock()
	defer mu.Unlock()
	report := Report{Stages: []Stage{}}
	if !enabled {
		return report
	}
	report.TotalMilliseconds = milliseconds(time.Since(started))
	names := make([]string, 0, len(stages))
	for name := range stages {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return stages[names[i]].first < stages[names[j]].first })
	for _, name := range names {
		entry := stages[name]
		report.Stages = append(report.Stages, Stage{Name: name, Count: entry.count, Milliseconds: milliseconds(entry.total)})
	}
	return report
}

// Write emits the report as one JSON line. It writes nothing when disabled.
func Write(writer io.Writer) {
	if !Enabled() {
		return
	}
	_ = json.NewEncoder(writer).Encode(struct {
		Timings Report `json:"timings"`
	}{Timings: Snapshot()})
}

func milliseconds(value time.Duration) float64 {
	return float64(value.Microseconds()) / 1000
}
