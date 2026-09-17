//go:build linux

package alerts

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// linuxSampler reads CPU/memory usage from /proc and disk usage via Statfs.
type linuxSampler struct {
	mu        sync.Mutex
	prevTotal uint64
	prevIdle  uint64
}

func newPlatformSampler() Sampler { return &linuxSampler{} }

// Sample gathers CPU, memory and disk usage in one pass.
func (s *linuxSampler) Sample() Metrics {
	return Metrics{
		CPU:  s.cpuPercent(),
		Mem:  memPercent(),
		Disk: diskPercent(),
	}
}

// cpuPercent computes aggregate CPU utilisation between two reads of
// /proc/stat. The first call primes the baseline and returns 0.
func (s *linuxSampler) cpuPercent() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0
	}

	var total, idle uint64
	for i, fld := range fields[1:] {
		n, err := strconv.ParseUint(fld, 10, 64)
		if err != nil {
			return 0
		}
		total += n
		// /proc/stat order: user nice system idle iowait irq softirq steal ...
		if i == 3 || i == 4 { // idle, iowait
			idle += n
		}
	}

	if s.prevTotal == 0 {
		s.prevTotal = total
		s.prevIdle = idle
		return 0
	}
	totalDelta := total - s.prevTotal
	idleDelta := idle - s.prevIdle
	s.prevTotal = total
	s.prevIdle = idle

	if totalDelta == 0 {
		return 0
	}
	used := float64(totalDelta-idleDelta) / float64(totalDelta) * 100
	return clampPct(used)
}

// memPercent derives used memory from /proc/meminfo (MemTotal - MemAvailable).
func memPercent() float64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	var total, avail uint64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = memValue(line)
		case strings.HasPrefix(line, "MemAvailable:"):
			avail = memValue(line)
		}
		if total != 0 && avail != 0 {
			break
		}
	}
	if total == 0 {
		return 0
	}
	used := float64(total-avail) / float64(total) * 100
	return clampPct(used)
}

// memValue extracts the numeric value (in kB) from a /proc/meminfo line.
func memValue(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	n, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// diskPercent derives used capacity of the root filesystem via Statfs.
func diskPercent() float64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return 0
	}
	if st.Blocks == 0 {
		return 0
	}
	used := float64(st.Blocks-st.Bfree) / float64(st.Blocks) * 100
	return clampPct(used)
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
