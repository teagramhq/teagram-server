package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestApplyLimaGoMemoryLimitReadsCgroupMemoryMax(t *testing.T) {
	t.Setenv("TG_LIMA_MEMORY_DIAGNOSTICS", "")
	tests := []struct {
		name            string
		memoryMax       string
		writeMemoryMax  bool
		previousLimit   int64
		wantMemoryLimit int64
		wantLogLevel    string
		wantLogMessage  string
	}{
		{name: "Lima cgroup", memoryMax: "48234496", writeMemoryMax: true, previousLimit: 20 << 20, wantMemoryLimit: 14 << 20, wantLogLevel: "level=INFO", wantLogMessage: "Lima Go memory cap applied"},
		{name: "demo operator limit", memoryMax: "48234496", writeMemoryMax: true, previousLimit: 18 << 20, wantMemoryLimit: 14 << 20, wantLogLevel: "level=INFO", wantLogMessage: "Lima Go memory cap applied"},
		{name: "unset Go memory target", memoryMax: "48234496", writeMemoryMax: true, previousLimit: math.MaxInt64, wantMemoryLimit: 14 << 20, wantLogLevel: "level=INFO", wantLogMessage: "Lima Go memory cap applied"},
		{name: "threshold cgroup", memoryMax: "67108864", writeMemoryMax: true, previousLimit: 20 << 20, wantMemoryLimit: 14 << 20, wantLogLevel: "level=INFO", wantLogMessage: "Lima Go memory cap applied"},
		{name: "higher existing limit", memoryMax: "48234496", writeMemoryMax: true, previousLimit: 32 << 20, wantMemoryLimit: 14 << 20, wantLogLevel: "level=INFO", wantLogMessage: "Lima Go memory cap applied"},
		{name: "production cgroup", memoryMax: "1073741824", writeMemoryMax: true, previousLimit: math.MaxInt64, wantMemoryLimit: math.MaxInt64, wantLogLevel: "level=INFO", wantLogMessage: "Lima Go memory cap not applied"},
		{name: "unlimited cgroup", memoryMax: "max", writeMemoryMax: true, previousLimit: math.MaxInt64, wantMemoryLimit: math.MaxInt64, wantLogLevel: "level=WARN", wantLogMessage: "parse cgroup memory.max"},
		{name: "missing cgroup file", previousLimit: 20 << 20, wantMemoryLimit: 20 << 20, wantLogLevel: "level=WARN", wantLogMessage: "read cgroup memory.max"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			previousLimit := debug.SetMemoryLimit(test.previousLimit)
			t.Cleanup(func() { debug.SetMemoryLimit(previousLimit) })
			memoryMaxPath := filepath.Join(t.TempDir(), "memory.max")
			if test.writeMemoryMax {
				if err := os.WriteFile(memoryMaxPath, []byte(test.memoryMax), 0o600); err != nil {
					t.Fatalf("write memory.max: %v", err)
				}
			}
			var logOutput bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logOutput, nil))

			restore := applyLimaGoMemoryLimit(logger, memoryMaxPath)
			if got := currentLimaTestMemoryLimit(); got != test.wantMemoryLimit {
				t.Fatalf("Go memory limit = %d bytes, want %d", got, test.wantMemoryLimit)
			}
			restore()
			if got := currentLimaTestMemoryLimit(); got != test.previousLimit {
				t.Fatalf("Go memory limit after restore = %d bytes, want previous limit %d", got, test.previousLimit)
			}
			if got := logOutput.String(); !strings.Contains(got, test.wantLogLevel) || !strings.Contains(got, test.wantLogMessage) {
				t.Fatalf("memory limit log = %q, want %q and %q", got, test.wantLogLevel, test.wantLogMessage)
			}
		})
	}
}

func TestApplyLimaGoMemoryLimitPreservesTargetBelowCgroupCeiling(t *testing.T) {
	previousLimit := debug.SetMemoryLimit(10 << 20)
	t.Cleanup(func() { debug.SetMemoryLimit(previousLimit) })

	memoryMaxPath := writeLimaTestCgroupMemoryMax(t, "48234496")
	var logOutput bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logOutput, nil))
	restore := applyLimaGoMemoryLimit(logger, memoryMaxPath)
	if got := currentLimaTestMemoryLimit(); got != 10<<20 {
		t.Fatalf("Go memory limit = %d bytes, want explicit target %d", got, 10<<20)
	}
	restore()
}

func writeLimaTestCgroupMemoryMax(t *testing.T, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "memory.max")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatalf("write memory.max: %v", err)
	}
	return path
}

func currentLimaTestMemoryLimit() int64 {
	limit := debug.SetMemoryLimit(-1)
	debug.SetMemoryLimit(limit)
	return limit
}

func TestLimaRuntimeMemoryDiagnosticsDoesNotChangeLimitWhenDisabled(t *testing.T) {
	previousLimit := debug.SetMemoryLimit(20 << 20)
	t.Cleanup(func() { debug.SetMemoryLimit(previousLimit) })
	t.Setenv("TG_LIMA_MEMORY_DIAGNOSTICS", "")

	stop, err := startLimaRuntimeMemoryDiagnostics(slog.Default())
	if err != nil {
		t.Fatalf("start disabled Lima runtime memory diagnostics: %v", err)
	}
	stop()
	if got := currentLimaTestMemoryLimit(); got != 20<<20 {
		t.Fatalf("Go memory limit with diagnostics disabled = %d bytes, want %d", got, 20<<20)
	}
}

func TestLimaRuntimeMemoryEndpointReportsGoHeapAndRuntimeAttribution(t *testing.T) {
	t.Setenv("TG_LIMA_MEMORY_DIAGNOSTICS", "1")

	server := httptest.NewServer(newLimaRuntimeMemoryDiagnosticsMux())
	t.Cleanup(server.Close)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/debug/runtime-memory", nil)
	if err != nil {
		t.Fatalf("create runtime memory diagnostics request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("get runtime memory diagnostics: %v", err)
	}
	t.Cleanup(func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close runtime memory diagnostics response: %v", err)
		}
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("runtime memory diagnostics status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	var got struct {
		SampleUnixNano     int64    `json:"sample_unix_nano"`
		RuntimeTotalBytes  uint64   `json:"runtime_total_bytes"`
		HeapLiveBytes      uint64   `json:"heap_live_bytes"`
		HeapObjectsBytes   uint64   `json:"heap_objects_bytes"`
		HeapStacksBytes    uint64   `json:"heap_stacks_bytes"`
		MetadataOtherBytes uint64   `json:"metadata_other_bytes"`
		Goroutines         uint64   `json:"goroutines"`
		GCCPUFraction      *float64 `json:"gc_cpu_fraction"`
	}
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode runtime memory diagnostics: %v", err)
	}
	if got.SampleUnixNano == 0 || got.RuntimeTotalBytes == 0 || got.HeapLiveBytes == 0 ||
		got.HeapObjectsBytes == 0 || got.HeapStacksBytes == 0 || got.MetadataOtherBytes == 0 || got.Goroutines == 0 {
		t.Fatalf("runtime memory diagnostics = %+v, want sampled heap and runtime values", got)
	}
	if got.HeapLiveBytes > got.RuntimeTotalBytes || got.HeapObjectsBytes > got.RuntimeTotalBytes {
		t.Fatalf("runtime memory diagnostics = %+v, heap values exceed total runtime memory", got)
	}
	if got.GCCPUFraction == nil || *got.GCCPUFraction < 0 || *got.GCCPUFraction > 1 {
		t.Fatalf("runtime memory diagnostics GC CPU fraction = %v, want a value in [0, 1]", got.GCCPUFraction)
	}
}

func TestReadLimaRuntimeMemoryMappingsRanksRSSAndAnonymousBytes(t *testing.T) {
	smapsPath := filepath.Join(t.TempDir(), "smaps")
	smaps := "10000000-10002000 rw-p 00000000 00:00 0 [heap]\n" +
		"Size: 8 kB\nRss: 8 kB\nAnonymous: 4 kB\n" +
		"20000000-20001000 r--p 00000000 00:00 0 /fixture/map.dat\n" +
		"Size: 4 kB\nRss: 20 kB\nAnonymous: 12 kB\n"
	if err := os.WriteFile(smapsPath, []byte(smaps), 0o600); err != nil {
		t.Fatalf("write smaps fixture: %v", err)
	}

	sample, err := readLimaRuntimeMemoryMappings(smapsPath)
	if err != nil {
		t.Fatalf("read runtime memory mappings: %v", err)
	}
	if sample.SampleUnixNano <= 0 || len(sample.Mappings) != 2 {
		t.Fatalf("runtime memory mappings = %+v, want timestamped two-mapping sample", sample)
	}
	if sample.TotalAnonymousBytes != 16*1024 {
		t.Fatalf("total anonymous mapping bytes = %d, want 16 KiB", sample.TotalAnonymousBytes)
	}
	if got := sample.Mappings[0]; got.Mapping != "20000000-20001000 r--p 00000000 00:00 0 /fixture/map.dat" ||
		got.RSSBytes != 20*1024 || got.AnonymousBytes != 12*1024 {
		t.Fatalf("largest runtime memory mapping = %+v, want 20 KiB RSS and 12 KiB anonymous", got)
	}
	if got := sample.Mappings[1]; got.RSSBytes != 8*1024 || got.AnonymousBytes != 4*1024 {
		t.Fatalf("second runtime memory mapping = %+v, want 8 KiB RSS and 4 KiB anonymous", got)
	}
}

func TestReadLimaRuntimeMemoryMappingsTotalsAnonymousBytesBeforeTruncating(t *testing.T) {
	smapsPath := filepath.Join(t.TempDir(), "smaps")
	var smaps strings.Builder
	for index := range 21 {
		address := 0x10000000 + index*0x2000
		if _, err := fmt.Fprintf(&smaps, "%08x-%08x rw-p 00000000 00:00 0 /fixture/%d\nRss: %d kB\nAnonymous: 2 kB\n",
			address, address+0x1000, index, index+1); err != nil {
			t.Fatalf("write smaps fixture: %v", err)
		}
	}
	if err := os.WriteFile(smapsPath, []byte(smaps.String()), 0o600); err != nil {
		t.Fatalf("write smaps fixture: %v", err)
	}

	sample, err := readLimaRuntimeMemoryMappings(smapsPath)
	if err != nil {
		t.Fatalf("read runtime memory mappings: %v", err)
	}
	if len(sample.Mappings) != 20 {
		t.Fatalf("runtime memory mapping rows = %d, want top 20", len(sample.Mappings))
	}
	if sample.TotalAnonymousBytes != 42*1024 {
		t.Fatalf("total anonymous mapping bytes = %d, want 42 KiB across all 21 mappings", sample.TotalAnonymousBytes)
	}
	if got := sample.Mappings[len(sample.Mappings)-1].RSSBytes; got != 2*1024 {
		t.Fatalf("smallest returned mapping RSS = %d, want second-smallest 2 KiB after truncation", got)
	}
}

func TestLimaRuntimeMemoryMappingsEndpointReportsProcessMappings(t *testing.T) {
	t.Setenv("TG_LIMA_MEMORY_DIAGNOSTICS", "1")

	server := httptest.NewServer(newLimaRuntimeMemoryDiagnosticsMux())
	t.Cleanup(server.Close)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/debug/runtime-memory-mappings", nil)
	if err != nil {
		t.Fatalf("create runtime memory mappings request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("get runtime memory mappings: %v", err)
	}
	t.Cleanup(func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close runtime memory mappings response: %v", err)
		}
	})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("runtime memory mappings status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	var got struct {
		SampleUnixNano      int64  `json:"sample_unix_nano"`
		TotalAnonymousBytes uint64 `json:"total_anonymous_bytes"`
		Mappings            []struct {
			RSSBytes       uint64 `json:"rss_bytes"`
			AnonymousBytes uint64 `json:"anonymous_bytes"`
			Mapping        string `json:"mapping"`
		} `json:"mappings"`
	}
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode runtime memory mappings: %v", err)
	}
	if got.SampleUnixNano <= 0 || got.TotalAnonymousBytes == 0 || len(got.Mappings) == 0 {
		t.Fatalf("runtime memory mappings = %+v, want a timestamped non-empty sample", got)
	}
	for _, mapping := range got.Mappings {
		if mapping.Mapping == "" {
			t.Fatalf("runtime memory mapping has no header: %+v", mapping)
		}
	}
}

func TestLimaRuntimeMemoryEndpointIsDisabledOutsideProbe(t *testing.T) {
	t.Setenv("TG_LIMA_MEMORY_DIAGNOSTICS", "")

	server := httptest.NewServer(newLimaRuntimeMemoryDiagnosticsMux())
	t.Cleanup(server.Close)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/debug/runtime-memory", nil)
	if err != nil {
		t.Fatalf("create runtime memory diagnostics request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("get runtime memory diagnostics: %v", err)
	}
	t.Cleanup(func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close runtime memory diagnostics response: %v", err)
		}
	})
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("runtime memory diagnostics status = %d, want %d when disabled", response.StatusCode, http.StatusNotFound)
	}

	mappingRequest, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/debug/runtime-memory-mappings", nil)
	if err != nil {
		t.Fatalf("create runtime memory mappings request: %v", err)
	}
	mappingResponse, err := http.DefaultClient.Do(mappingRequest)
	if err != nil {
		t.Fatalf("get runtime memory mappings: %v", err)
	}
	t.Cleanup(func() {
		if err := mappingResponse.Body.Close(); err != nil {
			t.Errorf("close runtime memory mappings response: %v", err)
		}
	})
	if mappingResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("runtime memory mappings status = %d, want %d when disabled", mappingResponse.StatusCode, http.StatusNotFound)
	}
}
