package bakeoff

import (
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MachineState is what else the machine was doing when a round ran.
//
// It is captured per round rather than once per session because the point is
// not to describe the machine, it is to make contamination visible at the
// granularity the numbers are compared at. This machine hosts other agent
// lanes; a round that ran beside a Swift build is not comparable to one that
// did not, and the only way to know afterwards is to have written it down.
type MachineState struct {
	At          time.Time `json:"at"`
	Load1       float64   `json:"load_1min"`
	Load5       float64   `json:"load_5min"`
	Load15      float64   `json:"load_15min"`
	BuildProcs  []string  `json:"build_processes"`
	BusyProcs   []string  `json:"busy_processes"` // anything over BusyCPUThreshold
	TotalCPUPct float64   `json:"total_cpu_percent"`
	PowerSource string    `json:"power_source"`
	Thermal     string    `json:"thermal"`
}

// BusyCPUThreshold is the percentage above which another process is worth
// naming in the record.
const BusyCPUThreshold = 15.0

// buildToolNames are matched against the process *name*, not its command line.
// Matching command lines catches every Electron app, because Chrome and VS Code
// mention "clang" and "link" in their flags -- a false positive that made the
// first attempt at this check useless.
var buildToolNames = map[string]bool{
	"go": true, "compile": true, "link": true, "asm": true, "cgo": true,
	"clang": true, "clang++": true, "cc1": true, "cc1plus": true, "gcc": true, "ld": true,
	"swift-frontend": true, "swiftc": true, "swift": true, "xcodebuild": true,
	"cargo": true, "rustc": true, "tsc": true, "node": true, "ninja": true, "make": true,
}

// CaptureMachineState reads what else is running. Every probe degrades to a
// note rather than an error: a missing reading must not stop a benchmark, but
// it must also not be silently recorded as "nothing running".
func CaptureMachineState() MachineState {
	st := MachineState{At: time.Now()}

	if out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output(); err == nil {
		f := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{} "))
		vals := make([]float64, 0, 3)
		for _, s := range f {
			if v, err := strconv.ParseFloat(s, 64); err == nil {
				vals = append(vals, v)
			}
		}
		if len(vals) >= 3 {
			st.Load1, st.Load5, st.Load15 = vals[0], vals[1], vals[2]
		}
	}

	if out, err := exec.Command("ps", "-Ao", "pcpu=,comm=").Output(); err == nil {
		type proc struct {
			cpu  float64
			name string
		}
		var builds, busy []proc
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			sp := strings.IndexByte(line, ' ')
			if sp < 0 {
				continue
			}
			cpu, err := strconv.ParseFloat(line[:sp], 64)
			if err != nil {
				continue
			}
			st.TotalCPUPct += cpu

			full := strings.TrimSpace(line[sp:])
			name := full
			if i := strings.LastIndexByte(name, '/'); i >= 0 {
				name = name[i+1:]
			}
			if buildToolNames[name] && cpu > 1.0 {
				builds = append(builds, proc{cpu, name})
			}
			if cpu >= BusyCPUThreshold {
				busy = append(busy, proc{cpu, name})
			}
		}
		render := func(ps []proc) []string {
			sort.Slice(ps, func(i, j int) bool { return ps[i].cpu > ps[j].cpu })
			out := make([]string, 0, len(ps))
			for _, p := range ps {
				out = append(out, fmt.Sprintf("%s %.0f%%", p.name, p.cpu))
			}
			return out
		}
		st.BuildProcs = render(builds)
		st.BusyProcs = render(busy)
	}

	if out, err := exec.Command("pmset", "-g", "ps").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "drawing from") {
				st.PowerSource = strings.TrimSpace(line)
				break
			}
		}
	}

	if out, err := exec.Command("pmset", "-g", "therm").Output(); err == nil {
		s := string(out)
		switch {
		case strings.Contains(s, "No thermal warning level has been recorded"):
			st.Thermal = "no thermal warning recorded"
		default:
			st.Thermal = strings.TrimSpace(strings.ReplaceAll(s, "\n", "; "))
		}
	}

	return st
}

// Quiet reports whether the machine looked idle enough for the round to be
// trusted: no build tools burning CPU, and a one-minute load average that
// leaves most cores to the benchmark.
func (s MachineState) Quiet() bool {
	return len(s.BuildProcs) == 0 && s.Load1 < float64(runtime.NumCPU())/3
}

func (s MachineState) String() string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "load %.2f/%.2f/%.2f  cpu %.0f%%", s.Load1, s.Load5, s.Load15, s.TotalCPUPct)
	if len(s.BuildProcs) > 0 {
		fmt.Fprintf(b, "  BUILDS RUNNING: %s", strings.Join(s.BuildProcs, ", "))
	} else {
		b.WriteString("  no build tools running")
	}
	if len(s.BusyProcs) > 0 {
		fmt.Fprintf(b, "  busy: %s", strings.Join(s.BusyProcs, ", "))
	}
	if !s.Quiet() {
		b.WriteString("  [NOT QUIET -- results from this round are suspect]")
	}
	return b.String()
}
