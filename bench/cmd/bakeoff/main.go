// Command bakeoff runs the contended coordination-plane workload through every
// driver under test and reports the numbers.
//
// The workload is one sustained writer appending events while N readers, each
// on its own connection, walk the log in pages. What it is built to expose is
// reader fairness: in a coordination plane the readers are subscribers waiting
// on state, so a driver that keeps aggregate throughput up while starving one
// reader has failed at the thing that matters. Per-reader minimum and median
// are reported alongside the aggregate for exactly that reason.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sqlite3 "github.com/pjstein/purego-sqlite"
	bakeoff "github.com/pjstein/purego-sqlite/bench"
	"github.com/pjstein/purego-sqlite/conformance"
)

var (
	duration  = flag.Duration("duration", 30*time.Second, "measured duration of each run")
	warmup    = flag.Duration("warmup", 3*time.Second, "unmeasured warmup before each run")
	readers   = flag.Int("readers", 8, "number of subscriber-shaped readers")
	runs      = flag.Int("runs", 3, "runs per driver")
	seedRows  = flag.Int("seed", 200000, "events pre-seeded into the log before each run")
	only      = flag.String("only", "", "substring filter on driver name")
	jsonOut   = flag.String("json", "", "also write results as JSON to this path")
	sampleCap = flag.Int("samples", 100000, "reservoir size for latency samples, per reader")
	writeRate = flag.Int("writerate", 0, "cap the writer at this many appends/s; 0 means as fast as it can go")
)

// RunResult is one driver's numbers for one run.
type RunResult struct {
	Driver          string    `json:"driver"`
	Run             int       `json:"run"`
	Seconds         float64   `json:"seconds"`
	WriteRateTarget int64     `json:"write_rate_target"`
	Appends         int64     `json:"appends"`
	AppendsPerSec   float64   `json:"appends_per_sec"`
	ReaderPolls     []int64   `json:"reader_polls"`
	ReaderRows      []int64   `json:"reader_rows"`
	PollsPerSecMin  float64   `json:"polls_per_sec_min"`
	PollsPerSecMed  float64   `json:"polls_per_sec_median"`
	PollsPerSecMax  float64   `json:"polls_per_sec_max"`
	PollsPerSecAgg  float64   `json:"polls_per_sec_aggregate"`
	RowsPerSecAgg   float64   `json:"rows_per_sec_aggregate"`
	FairnessRatio   float64   `json:"fairness_ratio_max_over_min"`
	LatencyP50Us    float64   `json:"latency_p50_us"`
	LatencyP99Us    float64   `json:"latency_p99_us"`
	LatencyP999Us   float64   `json:"latency_p999_us"`
	Errors          []string  `json:"errors,omitempty"`
	StartedAt       time.Time `json:"started_at"`
}

type Report struct {
	Machine      map[string]string   `json:"machine"`
	Config       map[string]any      `json:"config"`
	Capabilities []map[string]string `json:"capabilities"`
	Results      []RunResult         `json:"results"`
}

func main() {
	flag.Parse()

	report := Report{
		Machine: machineFacts(),
		Config: map[string]any{
			"duration":     duration.String(),
			"warmup":       warmup.String(),
			"readers":      *readers,
			"runs":         *runs,
			"seed_rows":    *seedRows,
			"payload_size": bakeoff.DefaultPayloadSize,
			"tail_limit":   bakeoff.TailLimit,
			"write_rate":   *writeRate,
			"pragmas":      bakeoff.Pragmas,
			"append_sql":   bakeoff.AppendSQL,
			"tail_sql":     bakeoff.TailSQL,
		},
	}

	fmt.Println("== machine and toolchain")
	for _, k := range sortedKeys(report.Machine) {
		fmt.Printf("   %-18s %s\n", k, report.Machine[k])
	}

	drivers := selectedDrivers(*only)
	if len(drivers) == 0 {
		fmt.Fprintf(os.Stderr, "no drivers matched %q\n", *only)
		os.Exit(1)
	}

	fmt.Println("\n== build capabilities (reported, not asserted: a build without FTS5 is a different build, not a wrong one)")
	report.Capabilities = probeCapabilities(drivers)
	printCapabilities(report.Capabilities)

	fmt.Printf("\n== workload: 1 writer + %d readers, %s per run, %d runs, %d events pre-seeded\n",
		*readers, *duration, *runs, *seedRows)
	fmt.Printf("   append: %s\n   tail:   %s\n", bakeoff.AppendSQL, bakeoff.TailSQL)

	for _, d := range drivers {
		for run := 1; run <= *runs; run++ {
			res, err := runOne(d, run)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s run %d: %v\n", d.Name(), run, err)
				continue
			}
			report.Results = append(report.Results, res)
			printRun(res)
		}
	}

	fmt.Println("\n== summary (median run per driver)")
	printSummary(report.Results)

	if *jsonOut != "" {
		b, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, "encoding results:", err)
			os.Exit(1)
		}
		if err := os.WriteFile(*jsonOut, b, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "writing results:", err)
			os.Exit(1)
		}
		fmt.Printf("\nwrote %s\n", *jsonOut)
	}
}

func selectedDrivers(filter string) []bakeoff.Driver {
	var out []bakeoff.Driver
	for _, d := range bakeoff.Drivers() {
		if filter == "" || strings.Contains(strings.ToLower(d.Name()), strings.ToLower(filter)) {
			out = append(out, d)
		}
	}
	return out
}

func machineFacts() map[string]string {
	version, err := sqlite3.Version()
	if err != nil {
		version = "unavailable: " + err.Error()
	}
	source, _ := sqlite3.SourceID()
	return map[string]string{
		"goos":            runtime.GOOS,
		"goarch":          runtime.GOARCH,
		"go version":      runtime.Version(),
		"cpus":            fmt.Sprint(runtime.NumCPU()),
		"pinned sqlite":   version,
		"pinned sourceid": source,
		"measured at":     time.Now().Format(time.RFC3339),
	}
}

// probeCapabilities records how each driver's SQLite was built. The
// performance table is not readable without it: a driver missing a module, or
// compiled in a different threading mode, is not running the same program.
func probeCapabilities(drivers []bakeoff.Driver) []map[string]string {
	var out []map[string]string
	dir, err := os.MkdirTemp("", "bakeoff-caps-")
	if err != nil {
		return out
	}
	defer os.RemoveAll(dir)

	for i, d := range drivers {
		row := map[string]string{"driver": d.Name()}
		db, err := d.Open(filepath.Join(dir, fmt.Sprintf("caps-%d.db", i)))
		if err != nil {
			row["error"] = err.Error()
			out = append(out, row)
			continue
		}
		for _, c := range conformance.Capabilities() {
			vals, err := db.QueryText(c.SQL)
			switch {
			case err != nil:
				row[c.Name] = "error: " + err.Error()
			case len(vals) == 0:
				row[c.Name] = "(no rows)"
			default:
				row[c.Name] = vals[0]
			}
		}
		// A compile-option flag can be present while the feature is not, and
		// absent while it is. Ask the module itself.
		if err := db.Exec(`CREATE VIRTUAL TABLE fts_probe USING fts5(body)`); err != nil {
			row["fts5-usable"] = "no"
		} else {
			row["fts5-usable"] = "yes"
		}
		db.Close()
		out = append(out, row)
	}
	return out
}

func printCapabilities(rows []map[string]string) {
	if len(rows) == 0 {
		return
	}
	cols := []string{"sqlite-version", "threading-mode", "fts5-usable", "stat4", "math-functions", "double-quoted-strings-rejected"}
	fmt.Printf("   %-34s", "driver")
	for _, c := range cols {
		fmt.Printf(" %-16s", short(c))
	}
	fmt.Println()
	for _, r := range rows {
		fmt.Printf("   %-34s", r["driver"])
		for _, c := range cols {
			fmt.Printf(" %-16s", r[c])
		}
		fmt.Println()
	}
}

func short(s string) string {
	switch s {
	case "sqlite-version":
		return "sqlite"
	case "threading-mode":
		return "threadsafe"
	case "math-functions":
		return "math"
	case "double-quoted-strings-rejected":
		return "DQS=0"
	}
	return s
}

// seed builds the pre-existing event log. It always uses this repository's
// binding so every driver starts from a byte-identical database file.
func seed(path string, rows int) error {
	db, err := (bakeoff.PuregoDriver{}).Open(path)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Exec(bakeoff.Schema); err != nil {
		return err
	}
	app, err := db.NewAppender()
	if err != nil {
		return err
	}
	defer app.Close()

	payload := bakeoff.SeedPayload(bakeoff.DefaultPayloadSize)
	if err := db.Exec(`BEGIN`); err != nil {
		return err
	}
	for i := 0; i < rows; i++ {
		if err := app.Append("stream-a", "event.appended", int64(i), payload); err != nil {
			return err
		}
	}
	if err := db.Exec(`COMMIT`); err != nil {
		return err
	}
	// Start every run from a checkpointed database so no driver inherits
	// another's WAL.
	return db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
}

func runOne(d bakeoff.Driver, run int) (RunResult, error) {
	dir, err := os.MkdirTemp("", "bakeoff-run-")
	if err != nil {
		return RunResult{}, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "events.db")

	if err := seed(path, *seedRows); err != nil {
		return RunResult{}, fmt.Errorf("seeding: %w", err)
	}

	res := RunResult{Driver: d.Name(), Run: run, StartedAt: time.Now()}

	var (
		stop        = make(chan struct{})
		measure     = make(chan struct{})
		wg          sync.WaitGroup
		appends     atomic.Int64
		errMu       sync.Mutex
		errs        []string
		readerPolls = make([]int64, *readers)
		readerRows  = make([]int64, *readers)
		latencies   = make([][]time.Duration, *readers)
	)
	fail := func(who string, err error) {
		errMu.Lock()
		if len(errs) < 20 {
			errs = append(errs, who+": "+err.Error())
		}
		errMu.Unlock()
	}

	// Every connection is opened and every statement prepared before any
	// measurement starts. Opening under load is itself contended, and a
	// connection that fails to open would otherwise report zero throughput --
	// indistinguishable from a starved reader, which is the exact thing this
	// harness exists to detect.
	writerDB, err := d.Open(path)
	if err != nil {
		return RunResult{}, fmt.Errorf("opening writer: %w", err)
	}
	defer writerDB.Close()
	appender, err := writerDB.NewAppender()
	if err != nil {
		return RunResult{}, fmt.Errorf("preparing appender: %w", err)
	}
	defer appender.Close()

	readerDBs := make([]bakeoff.DB, *readers)
	tailers := make([]bakeoff.Tailer, *readers)
	for i := 0; i < *readers; i++ {
		rdb, err := d.Open(path)
		if err != nil {
			return RunResult{}, fmt.Errorf("opening reader %d: %w", i, err)
		}
		readerDBs[i] = rdb
		defer rdb.Close()
		t, err := rdb.NewTailer()
		if err != nil {
			return RunResult{}, fmt.Errorf("preparing tailer %d: %w", i, err)
		}
		tailers[i] = t
		defer t.Close()
	}

	// Writer.
	wg.Add(1)
	go func() {
		defer wg.Done()
		payload := bakeoff.SeedPayload(bakeoff.DefaultPayloadSize)
		counting := false
		var local int64

		// A rate-limited writer is how readers get compared at equal write
		// load. Without it the drivers pick their own write rates and the
		// reader numbers are partly a measurement of how much WAL churn each
		// writer happened to generate.
		var interval time.Duration
		if *writeRate > 0 {
			interval = time.Second / time.Duration(*writeRate)
		}
		var paceStart time.Time
		var paced int64

		for {
			select {
			case <-stop:
				appends.Add(local)
				return
			default:
			}
			if !counting {
				select {
				case <-measure:
					counting = true
				default:
				}
			}
			if interval > 0 {
				if paceStart.IsZero() {
					paceStart = time.Now()
				}
				if wait := time.Until(paceStart.Add(time.Duration(paced) * interval)); wait > 0 {
					time.Sleep(wait)
				}
				paced++
			}
			if err := appender.Append("stream-a", "event.appended", bakeoff.Now(), payload); err != nil {
				fail("append", err)
				appends.Add(local)
				return
			}
			if counting {
				local++
			}
		}
	}()

	// Readers. Each walks the log in pages from its own random starting point,
	// restarting when it reaches the end. Every poll therefore does real work:
	// a subscriber that has caught up would be measuring the cost of an empty
	// query, which is a different question.
	for i := 0; i < *readers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tail := tailers[i]

			rng := rand.New(rand.NewPCG(uint64(i)+1, 0x5eed))
			samples := make([]time.Duration, 0, *sampleCap)
			cursor := int64(rng.IntN(*seedRows))
			var polls, rows, seen int64
			counting := false

			for {
				select {
				case <-stop:
					readerPolls[i], readerRows[i] = polls, rows
					latencies[i] = samples
					return
				default:
				}
				if !counting {
					select {
					case <-measure:
						counting = true
						polls, rows = 0, 0
						samples = samples[:0]
						seen = 0
					default:
					}
				}

				start := time.Now()
				last, n, err := tail.Poll(cursor)
				elapsed := time.Since(start)
				if err != nil {
					fail(fmt.Sprintf("reader %d poll", i), err)
					readerPolls[i], readerRows[i] = polls, rows
					latencies[i] = samples
					return
				}
				if n < bakeoff.TailLimit {
					cursor = int64(rng.IntN(*seedRows))
				} else {
					cursor = last
				}
				if !counting {
					continue
				}
				polls++
				rows += int64(n)
				// Reservoir sampling: an unbiased picture of the whole run
				// rather than of whichever part fit in a buffer.
				seen++
				if len(samples) < *sampleCap {
					samples = append(samples, elapsed)
				} else if j := rng.IntN(int(seen)); j < *sampleCap {
					samples[j] = elapsed
				}
			}
		}(i)
	}

	time.Sleep(*warmup)
	close(measure)
	measuredStart := time.Now()
	time.Sleep(*duration)
	seconds := time.Since(measuredStart).Seconds()
	close(stop)
	wg.Wait()

	res.Seconds = seconds
	res.WriteRateTarget = int64(*writeRate)
	res.Appends = appends.Load()
	res.AppendsPerSec = float64(res.Appends) / seconds
	res.ReaderPolls = readerPolls
	res.ReaderRows = readerRows
	res.Errors = errs

	rates := make([]float64, len(readerPolls))
	var aggPolls, aggRows int64
	for i, p := range readerPolls {
		rates[i] = float64(p) / seconds
		aggPolls += p
		aggRows += readerRows[i]
	}
	sorted := append([]float64(nil), rates...)
	sort.Float64s(sorted)
	res.PollsPerSecMin = sorted[0]
	res.PollsPerSecMax = sorted[len(sorted)-1]
	res.PollsPerSecMed = median(sorted)
	res.PollsPerSecAgg = float64(aggPolls) / seconds
	res.RowsPerSecAgg = float64(aggRows) / seconds
	if res.PollsPerSecMin > 0 {
		res.FairnessRatio = res.PollsPerSecMax / res.PollsPerSecMin
	}

	var all []time.Duration
	for _, s := range latencies {
		all = append(all, s...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	res.LatencyP50Us = percentileUs(all, 0.50)
	res.LatencyP99Us = percentileUs(all, 0.99)
	res.LatencyP999Us = percentileUs(all, 0.999)

	return res, nil
}

func median(sorted []float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

func percentileUs(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p * float64(len(sorted)))
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return float64(sorted[i].Nanoseconds()) / 1000
}

func printRun(r RunResult) {
	fmt.Printf("\n-- %s  run %d  (%.1fs)\n", r.Driver, r.Run, r.Seconds)
	fmt.Printf("   writer      %10.0f appends/s  (%d total)\n", r.AppendsPerSec, r.Appends)
	fmt.Printf("   readers     min %8.0f   median %8.0f   max %8.0f  polls/s   (max/min %.2f)\n",
		r.PollsPerSecMin, r.PollsPerSecMed, r.PollsPerSecMax, r.FairnessRatio)
	fmt.Printf("   aggregate   %10.0f polls/s   %12.0f rows/s\n", r.PollsPerSecAgg, r.RowsPerSecAgg)
	fmt.Printf("   latency     p50 %7.1fus   p99 %8.1fus   p99.9 %9.1fus\n", r.LatencyP50Us, r.LatencyP99Us, r.LatencyP999Us)
	for _, e := range r.Errors {
		fmt.Printf("   ERROR       %s\n", e)
	}
}

func printSummary(results []RunResult) {
	byDriver := map[string][]RunResult{}
	var order []string
	for _, r := range results {
		if _, ok := byDriver[r.Driver]; !ok {
			order = append(order, r.Driver)
		}
		byDriver[r.Driver] = append(byDriver[r.Driver], r)
	}

	fmt.Printf("   %-34s %12s %12s %12s %10s %10s %10s\n",
		"driver", "appends/s", "min polls/s", "med polls/s", "max/min", "p99 us", "rows/s")
	for _, name := range order {
		rs := byDriver[name]
		sort.Slice(rs, func(i, j int) bool { return rs[i].PollsPerSecMed < rs[j].PollsPerSecMed })
		m := rs[len(rs)/2]
		fmt.Printf("   %-34s %12.0f %12.0f %12.0f %10.2f %10.1f %10.0f\n",
			name, m.AppendsPerSec, m.PollsPerSecMin, m.PollsPerSecMed, m.FairnessRatio, m.LatencyP99Us, m.RowsPerSecAgg)
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
