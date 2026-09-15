package handlers

import (
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================
// LIVE SYSTEM STATS (per-core CPU, RAM, temp)
// ============================================

// cpuSample stores a snapshot of /proc/stat for all cores.
type cpuSample struct {
	ts    time.Time
	cores []cpuCoreSample // index = core id
}

type cpuCoreSample struct {
	idle  uint64
	total uint64
}

var (
	prevCPUSample *cpuSample
	cpuSampleMu   sync.Mutex
)

// cpuSysfsTempPaths are tried in order for CPU temperature.
// Value is in millidegrees Celsius; divide by 1000.
var cpuSysfsTempPaths = []string{
	"/sys/class/thermal/thermal_zone0/temp",
	"/sys/devices/virtual/thermal/thermal_zone0/temp",
	"/sys/class/hwmon/hwmon0/temp1_input",
}

// GetSystemStats returns live per-core CPU %, RAM, and CPU temperature.
// Admin-authed.
func (h *SystemHandler) GetSystemStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cpuCores, cpuAvg := computeCPUSamples()
	ramTotalMB, ramUsedMB, ramUsedPct := readRAMStats()
	cpuTemp := readCPUTemp()

	coreMaps := make([]map[string]interface{}, len(cpuCores))
	for i, c := range cpuCores {
		coreMaps[i] = map[string]interface{}{
			"id":    i,
			"usage": c,
		}
	}

	data := map[string]interface{}{
		"cpu_cores": coreMaps,
		"cpu_avg":   cpuAvg,
		"ram": map[string]interface{}{
			"total_mb": ramTotalMB,
			"used_mb":  ramUsedMB,
			"used_pct": ramUsedPct,
		},
		"cpu_temp_c": cpuTemp, // float64 or nil → JSON null
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
	}

	sendJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"data":    data,
	})
}

// computeCPUSamples reads /proc/stat, compares with the cached previous
// sample (if any, and ≥500ms old), and returns per-core usage percentages.
// First call (or calls <500ms apart) returns zeros — acceptable for a 2-3s
// poll interval on the dashboard.
func computeCPUSamples() (cores []float64, avg float64) {
	cur := readProcStat()
	if cur == nil {
		return []float64{0, 0, 0, 0}, 0
	}

	cpuSampleMu.Lock()
	prev := prevCPUSample
	prevCPUSample = cur
	cpuSampleMu.Unlock()

	n := len(cur.cores)
	cores = make([]float64, n)

	if prev == nil || time.Since(prev.ts) < 500*time.Millisecond || len(prev.cores) != n {
		// No usable previous sample — return zeros, caller will get real
		// numbers on the next poll.
		return cores, 0
	}

	var sum float64
	for i := 0; i < n; i++ {
		dTotal := cur.cores[i].total - prev.cores[i].total
		dIdle := cur.cores[i].idle - prev.cores[i].idle
		if dTotal > 0 {
			cores[i] = float64(dTotal-dIdle) / float64(dTotal) * 100
			// Round to 1 decimal place
			cores[i] = roundToOneDecimal(cores[i])
		}
		sum += cores[i]
	}
	if n > 0 {
		avg = roundToOneDecimal(sum / float64(n))
	}
	return cores, avg
}

// readProcStat parses /proc/stat for per-core CPU counters.
// Returns nil on read error.
func readProcStat() *cpuSample {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return nil
	}

	sample := &cpuSample{ts: time.Now()}

	for _, line := range strings.Split(string(data), "\n") {
		// Per-core lines: "cpu0 1234 56 789 ..."
		// Skip aggregate "cpu " line (has a space after "cpu").
		if !strings.HasPrefix(line, "cpu") || len(line) < 5 || line[3] == ' ' {
			continue
		}
		// line[3] should be a digit
		if line[3] < '0' || line[3] > '9' {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		// fields[0] = "cpuN", fields[1..] = counters
		var vals []uint64
		for _, f := range fields[1:] {
			v, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				vals = append(vals, 0)
			} else {
				vals = append(vals, v)
			}
		}
		// Sum all counters for total; idle is fields[4] (index 3 in vals)
		var total uint64
		for _, v := range vals {
			total += v
		}
		var idle uint64
		if len(vals) >= 4 {
			idle = vals[3] // idle field
		}
		sample.cores = append(sample.cores, cpuCoreSample{idle: idle, total: total})
	}

	return sample
}

// readRAMStats parses /proc/meminfo and returns (totalMB, usedMB, usedPct).
// Reuses parseMemInfoValue from system.go.
func readRAMStats() (totalMB int64, usedMB int64, usedPct float64) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, 0
	}

	var memTotal, memAvailable int64
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			memTotal = parseMemInfoValue(line)
		} else if strings.HasPrefix(line, "MemAvailable:") {
			memAvailable = parseMemInfoValue(line)
		}
	}

	used := memTotal - memAvailable
	totalMB = memTotal / 1024
	usedMB = used / 1024
	if memTotal > 0 {
		usedPct = roundToOneDecimal(float64(used) / float64(memTotal) * 100)
	}
	return totalMB, usedMB, usedPct
}

// readCPUTemp reads CPU temperature from sysfs. Returns nil (→ JSON null)
// if all paths fail.
func readCPUTemp() interface{} {
	for _, path := range cpuSysfsTempPaths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		s := strings.TrimSpace(string(data))
		milli, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			continue
		}
		temp := float64(milli) / 1000.0
		return roundToOneDecimal(temp)
	}
	// All paths failed — return nil so JSON encodes as null.
	return nil
}

func roundToOneDecimal(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}
