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
	"math"
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
	rounds    = flag.Int("rounds", 5, "measured rounds; every driver runs once per round, in round-robin order")
	seedRows  = flag.Int("seed", 200000, "events pre-seeded into the log before each run")
	only      = flag.String("only", "", "substring filter on driver name")
	jsonOut   = flag.String("json", "", "also write results as JSON to this path")
	sampleCap = flag.Int("samples", 100000, "reservoir size for latency samples, per reader")
	writeRate = flag.Int("writerate", 0, "cap the writer at this many appends/s; 0 means as fast as it can go")
)

// RunResult is one driver's numbers for one run.
type RunResult struct {
	Driver          string    `json:"driver"`
	Round           int       `json:"round"`
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
	LatencySamples  int       `json:"latency_samples"`
	Errors          []string  `json:"errors,omitempty"`
	StartedAt       time.Time `json:"started_at"`
}

type Report struct {
	Invocation   string                 `json:"invocation"`
	Machine      map[string]string      `json:"machine"`
	Config       map[string]any         `json:"config"`
	Capabilities []map[string]string    `json:"capabilities"`
	RoundStates  []bakeoff.MachineState `json:"round_states"`
	Results      []RunResult            `json:"results"`
}

// Stat summarises one metric across rounds. A single run is an anecdote; the
// spread is what says whether the difference between two drivers is real.
type Stat struct {
	Mean   float64   `json:"mean"`
	StdDev float64   `json:"stddev"`
	Min    float64   `json:"min"`
	Max    float64   `json:"max"`
	N      int       `json:"n"`
	Values []float64 `json:"values"`
}

func summarise(values []float64) Stat {
	st := Stat{N: len(values), Values: append([]float64(nil), values...)}
	if st.N == 0 {
		return st
	}
	sum := 0.0
	st.Min, st.Max = values[0], values[0]
	for _, v := range values {
		sum += v
		if v < st.Min {
			st.Min = v
		}
		if v > st.Max {
			st.Max = v
		}
	}
	st.Mean = sum / float64(st.N)
	if st.N > 1 {
		ss := 0.0
		for _, v := range values {
			d := v - st.Mean
			ss += d * d
		}
		st.StdDev = math.Sqrt(ss / float64(st.N-1)) // sample stddev
	}
	return st
}

// RelSD is the standard deviation as a fraction of the mean. Above 0.10 the
// directive says stop and look rather than conclude.
func (s Stat) RelSD() float64 {
	if s.Mean == 0 {
		return 0
	}
	return s.StdDev / s.Mean
}

func (s Stat) Format(unit string) string {
	flag := ""
	if s.RelSD() > 0.10 {
		flag = " !"
	}
	return fmt.Sprintf("%.0f +/- %.0f %s (min %.0f, max %.0f, n=%d)%s", s.Mean, s.StdDev, unit, s.Min, s.Max, s.N, flag)
}

func main() {
	flag.Parse()

	invocation := strings.Join(os.Args, " ")

	report := Report{
		Invocation: invocation,
		Machine:    machineFacts(),
		Config: map[string]any{
			"duration":     duration.String(),
			"warmup":       warmup.String(),
			"readers":      *readers,
			"rounds":       *rounds,
			"seed_rows":    *seedRows,
			"payload_size": bakeoff.DefaultPayloadSize,
			"tail_limit":   bakeoff.TailLimit,
			"write_rate":   *writeRate,
			"pragmas":      bakeoff.Pragmas,
			"append_sql":   bakeoff.AppendSQL,
			"tail_sql":     bakeoff.TailSQL,
			"batch_env":    os.Getenv("PUREGO_SQLITE_BATCH"),
		},
	}

	fmt.Println("== invocation (re-run this verbatim to reproduce)")
	fmt.Printf("   PUREGO_SQLITE_BATCH=%q %s\n", os.Getenv("PUREGO_SQLITE_BATCH"), invocation)

	fmt.Println("\n== machine and toolchain")
	for _, k := range sortedKeys(report.Machine) {
		fmt.Printf("   %-18s %s\n", k, report.Machine[k])
	}

	drivers := selectedDrivers(*only)
	if len(drivers) == 0 {
		fmt.Fprintf(os.Stderr, "no drivers matched %q\n", *only)
		os.Exit(1)
	}
	fmt.Printf("\n== %d drivers under test, interleaved: every round runs each once, in this order\n", len(drivers))
	for i, d := range drivers {
		fmt.Printf("   %d. %s\n", i+1, d.Name())
	}

	fmt.Println("\n== build capabilities (reported, not asserted: a build without FTS5 is a different build, not a wrong one)")
	report.Capabilities = probeCapabilities(drivers)
	printCapabilities(report.Capabilities)

	fmt.Printf("\n== workload: 1 writer + %d readers, %s measured per run after %s warmup, %d rounds, %d events pre-seeded\n",
		*readers, *duration, *warmup, *rounds, *seedRows)
	if *writeRate > 0 {
		fmt.Printf("   writer capped at %d appends/s so readers are compared under identical write pressure\n", *writeRate)
	} else {
		fmt.Println("   writer unthrottled: each driver picks its own write rate, so reader numbers are NOT comparable across drivers")
	}
	fmt.Printf("   append: %s\n   tail:   %s\n", bakeoff.AppendSQL, bakeoff.TailSQL)

	// Round-robin rather than all-of-A-then-all-of-B. Thermal drift, page
	// cache warmth and whatever else the machine is doing move slowly; running
	// a driver's rounds consecutively would fold that drift into that driver's
	// numbers and nobody else's.
	for round := 1; round <= *rounds; round++ {
		state := bakeoff.CaptureMachineState()
		report.RoundStates = append(report.RoundStates, state)
		fmt.Printf("\n########## ROUND %d/%d\n   machine: %s\n", round, *rounds, state)

		for _, d := range drivers {
			res, err := runOne(d, round)
			if err != nil {
				fmt.Fprintf(os.Stderr, "%s round %d: %v\n", d.Name(), round, err)
				os.Exit(1)
			}
			report.Results = append(report.Results, res)
			printRun(res)
		}
	}

	fmt.Printf("\n== summary over %d rounds: mean +/- sample stddev (min, max, n)\n", *rounds)
	fmt.Println("   a trailing ! marks a metric whose stddev exceeds 10% of its mean")
	printSummary(report.Results, *rounds)

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

func runOne(d bakeoff.Driver, round int) (RunResult, error) {
	dir, err := os.MkdirTemp("", "bakeoff-run-")
	if err != nil {
		return RunResult{}, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "events.db")

	if err := seed(path, *seedRows); err != nil {
		return RunResult{}, fmt.Errorf("seeding: %w", err)
	}

	res := RunResult{Driver: d.Name(), Round: round, StartedAt: time.Now()}

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
	res.LatencySamples = len(all)

	// A run that read nothing is a failed run, not a fast one. Assertions that
	// can pass without subjects are worse than no assertions.
	if res.LatencySamples == 0 {
		return res, fmt.Errorf("%s round %d: no latency samples -- readers did no measured work", d.Name(), round)
	}
	if sumInt64(res.ReaderRows) == 0 {
		return res, fmt.Errorf("%s round %d: readers returned zero rows -- the workload measured nothing", d.Name(), round)
	}
	if res.Appends == 0 {
		return res, fmt.Errorf("%s round %d: writer committed nothing", d.Name(), round)
	}

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
	fmt.Printf("\n-- %s  round %d  (%.1fs measured)\n", r.Driver, r.Round, r.Seconds)
	fmt.Printf("   writer      %10.0f appends/s  (%d appends committed)\n", r.AppendsPerSec, r.Appends)
	fmt.Printf("   readers     min %8.0f   median %8.0f   max %8.0f  polls/s   (max/min %.2f)\n",
		r.PollsPerSecMin, r.PollsPerSecMed, r.PollsPerSecMax, r.FairnessRatio)
	fmt.Printf("   aggregate   %10.0f polls/s   %12.0f rows/s   over %d readers\n", r.PollsPerSecAgg, r.RowsPerSecAgg, len(r.ReaderPolls))
	fmt.Printf("   latency     p50 %7.1fus   p99 %8.1fus   p99.9 %9.1fus   (%d samples)\n",
		r.LatencyP50Us, r.LatencyP99Us, r.LatencyP999Us, r.LatencySamples)
	fmt.Printf("   subjects    %d reader polls, %d rows read -- a run with zero of either is a failed run, not a fast one\n",
		sumInt64(r.ReaderPolls), sumInt64(r.ReaderRows))
	for _, e := range r.Errors {
		fmt.Printf("   ERROR       %s\n", e)
	}
}

func sumInt64(v []int64) int64 {
	var t int64
	for _, x := range v {
		t += x
	}
	return t
}

func printSummary(results []RunResult, wantRounds int) {
	byDriver := map[string][]RunResult{}
	var order []string
	for _, r := range results {
		if _, ok := byDriver[r.Driver]; !ok {
			order = append(order, r.Driver)
		}
		byDriver[r.Driver] = append(byDriver[r.Driver], r)
	}

	for _, name := range order {
		rs := byDriver[name]
		pick := func(f func(RunResult) float64) Stat {
			v := make([]float64, 0, len(rs))
			for _, r := range rs {
				v = append(v, f(r))
			}
			return summarise(v)
		}
		appends := pick(func(r RunResult) float64 { return r.AppendsPerSec })
		minPolls := pick(func(r RunResult) float64 { return r.PollsPerSecMin })
		medPolls := pick(func(r RunResult) float64 { return r.PollsPerSecMed })
		fairness := pick(func(r RunResult) float64 { return r.FairnessRatio })
		p50 := pick(func(r RunResult) float64 { return r.LatencyP50Us })
		p99 := pick(func(r RunResult) float64 { return r.LatencyP99Us })
		rows := pick(func(r RunResult) float64 { return r.RowsPerSecAgg })

		fmt.Printf("\n   %s\n", name)
		if len(rs) != wantRounds {
			fmt.Printf("      INCOMPLETE: %d rounds recorded, %d expected\n", len(rs), wantRounds)
		}
		fmt.Printf("      appends/s        %s\n", appends.Format(""))
		fmt.Printf("      min polls/s      %s\n", minPolls.Format(""))
		fmt.Printf("      median polls/s   %s\n", medPolls.Format(""))
		fmt.Printf("      rows/s aggregate %s\n", rows.Format(""))
		fmt.Printf("      fairness max/min %.3f +/- %.3f (min %.3f, max %.3f)\n", fairness.Mean, fairness.StdDev, fairness.Min, fairness.Max)
		fmt.Printf("      p50 latency      %s us\n", p50.Format(""))
		fmt.Printf("      p99 latency      %s us\n", p99.Format(""))
		var errs int
		for _, r := range rs {
			errs += len(r.Errors)
		}
		fmt.Printf("      errors           %d\n", errs)
		if medPolls.RelSD() > 0.10 {
			fmt.Printf("      ** median polls/s stddev is %.1f%% of mean -- above the 10%% bar; do not conclude from this without investigating\n", medPolls.RelSD()*100)
		}
	}

	fmt.Printf("\n   one-line table (mean median-polls/s +/- stddev)\n")
	fmt.Printf("   %-36s %14s %16s %16s %12s\n", "driver", "appends/s", "min polls/s", "med polls/s", "p99 us")
	for _, name := range order {
		rs := byDriver[name]
		pick := func(f func(RunResult) float64) Stat {
			v := make([]float64, 0, len(rs))
			for _, r := range rs {
				v = append(v, f(r))
			}
			return summarise(v)
		}
		a := pick(func(r RunResult) float64 { return r.AppendsPerSec })
		mn := pick(func(r RunResult) float64 { return r.PollsPerSecMin })
		md := pick(func(r RunResult) float64 { return r.PollsPerSecMed })
		p9 := pick(func(r RunResult) float64 { return r.LatencyP99Us })
		fmt.Printf("   %-36s %7.0f+/-%-5.0f %8.0f+/-%-6.0f %8.0f+/-%-6.0f %6.0f+/-%-4.0f\n",
			name, a.Mean, a.StdDev, mn.Mean, mn.StdDev, md.Mean, md.StdDev, p9.Mean, p9.StdDev)
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
