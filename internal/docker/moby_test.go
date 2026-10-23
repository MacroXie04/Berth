package docker

import (
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
)

func sample(totalNs, preNs, sys, preSys uint64, cpus uint32, mem, limit uint64, memStats map[string]uint64) container.StatsResponse {
	var s container.StatsResponse
	s.CPUStats.CPUUsage.TotalUsage = totalNs
	s.PreCPUStats.CPUUsage.TotalUsage = preNs
	s.CPUStats.SystemUsage = sys
	s.PreCPUStats.SystemUsage = preSys
	s.CPUStats.OnlineCPUs = cpus
	s.MemoryStats.Usage = mem
	s.MemoryStats.Limit = limit
	s.MemoryStats.Stats = memStats
	return s
}

// CPU follows `docker stats`: container CPU time over host CPU time, scaled by
// the number of CPUs, so a container saturating two of four cores reads 200.
func TestStatsFromCPU(t *testing.T) {
	cases := []struct {
		name string
		s    container.StatsResponse
		want float64
	}{
		{"two of four cores busy", sample(2_000_000_000, 0, 4_000_000_000, 0, 4, 0, 0, nil), 200},
		{"deltas, not totals", sample(7_000_000_000, 5_000_000_000, 14_000_000_000, 10_000_000_000, 4, 0, 0, nil), 200},
		{"idle", sample(100, 100, 4_000_000_000, 0, 4, 0, 0, nil), 0},
		{"no previous sample", sample(500, 0, 1000, 0, 1, 0, 0, nil), 50},
		{"host clock did not advance", sample(100, 0, 500, 500, 4, 0, 0, nil), 0},
		{"cpu counter went backwards", sample(100, 200, 1000, 0, 4, 0, 0, nil), 0},
	}
	for _, tc := range cases {
		if got := statsFrom(tc.s).CPUPercent; got != tc.want {
			t.Errorf("%s: CPUPercent = %v, want %v", tc.name, got, tc.want)
		}
	}

	// Without OnlineCPUs the per-CPU list gives the count.
	s := sample(1_000_000_000, 0, 4_000_000_000, 0, 0, 0, 0, nil)
	s.CPUStats.CPUUsage.PercpuUsage = []uint64{1, 2, 3, 4}
	if got := statsFrom(s).CPUPercent; got != 100 {
		t.Errorf("per-cpu fallback: CPUPercent = %v, want 100", got)
	}
	// No CPU count at all must not divide by zero or invent a number.
	if got := statsFrom(sample(1000, 0, 4000, 0, 0, 0, 0, nil)).CPUPercent; got != 0 {
		t.Errorf("unknown cpu count: CPUPercent = %v, want 0", got)
	}
}

// Memory is the working set: usage minus the page cache the kernel can reclaim.
func TestStatsFromMemory(t *testing.T) {
	cases := []struct {
		name  string
		stats map[string]uint64
		usage uint64
		want  uint64
	}{
		{"cgroup v2 inactive_file", map[string]uint64{"inactive_file": 300}, 1000, 700},
		{"cgroup v1 total_inactive_file", map[string]uint64{"total_inactive_file": 250}, 1000, 750},
		{"v2 key wins over v1", map[string]uint64{"inactive_file": 100, "total_inactive_file": 900}, 1000, 900},
		{"no cache info", map[string]uint64{"anon": 5}, 1000, 1000},
		{"nil map", nil, 1000, 1000},
		{"cache larger than usage is ignored", map[string]uint64{"inactive_file": 5000}, 1000, 1000},
		{"cache equal to usage", map[string]uint64{"inactive_file": 1000}, 1000, 1000},
	}
	for _, tc := range cases {
		got := statsFrom(sample(0, 0, 0, 0, 0, tc.usage, 4096, tc.stats))
		if got.MemUsageBytes != tc.want {
			t.Errorf("%s: MemUsageBytes = %d, want %d", tc.name, got.MemUsageBytes, tc.want)
		}
		if got.MemLimitBytes != 4096 {
			t.Errorf("%s: MemLimitBytes = %d", tc.name, got.MemLimitBytes)
		}
	}
	s := sample(0, 0, 0, 0, 0, 1, 1, nil)
	s.PidsStats.Current = 42
	if got := statsFrom(s).Pids; got != 42 {
		t.Errorf("Pids = %d", got)
	}
}

func TestParseDockerTime(t *testing.T) {
	want := time.Date(2026, 10, 22, 12, 30, 45, 123456789, time.UTC)
	if got := parseDockerTime("2026-10-22T12:30:45.123456789Z"); !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("parsed %v", got)
	}
	// Offsets are normalised to UTC.
	if got := parseDockerTime("2026-10-22T14:30:45+02:00"); !got.Equal(time.Date(2026, 10, 22, 12, 30, 45, 0, time.UTC)) || got.Location() != time.UTC {
		t.Errorf("offset parsed as %v", got)
	}
	// Docker spells "never" as the year-1 zero time; callers need a real zero.
	for _, in := range []string{"0001-01-01T00:00:00Z", "", "garbage", "2026-13-45T00:00:00Z"} {
		if got := parseDockerTime(in); !got.IsZero() {
			t.Errorf("parseDockerTime(%q) = %v, want the zero time", in, got)
		}
	}
}

func TestTrimSlash(t *testing.T) {
	for in, want := range map[string]string{"/berth-ws-alice": "berth-ws-alice", "berth-ws-alice": "berth-ws-alice", "": "", "/": ""} {
		if got := trimSlash(in); got != want {
			t.Errorf("trimSlash(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsUnavailable(t *testing.T) {
	if IsUnavailable(nil) || IsUnavailable(ErrNotFound) {
		t.Error("unrelated errors reported as unavailable")
	}
	if !IsUnavailable(ErrUnavailable) {
		t.Error("ErrUnavailable not recognised")
	}
}
