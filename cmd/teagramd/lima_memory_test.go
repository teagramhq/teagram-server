package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime/debug"
	"testing"
)

func TestLimaRuntimeMemoryDiagnosticsCapsAndRestoresGoMemoryLimit(t *testing.T) {
	previousLimit := debug.SetMemoryLimit(20 << 20)
	t.Cleanup(func() { debug.SetMemoryLimit(previousLimit) })
	t.Setenv("TG_LIMA_MEMORY_DIAGNOSTICS", "1")

	stop, err := startLimaRuntimeMemoryDiagnostics(slog.Default())
	if err != nil {
		t.Fatalf("start Lima runtime memory diagnostics: %v", err)
	}
	effectiveLimit := currentLimaTestMemoryLimit()
	stop()
	if effectiveLimit != 16<<20 {
		t.Fatalf("diagnostic Go memory limit = %d bytes, want %d", effectiveLimit, 16<<20)
	}
	if got := currentLimaTestMemoryLimit(); got != 20<<20 {
		t.Fatalf("Go memory limit after diagnostics stop = %d bytes, want restored limit %d", got, 20<<20)
	}
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
		SampleUnixNano     int64  `json:"sample_unix_nano"`
		RuntimeTotalBytes  uint64 `json:"runtime_total_bytes"`
		HeapLiveBytes      uint64 `json:"heap_live_bytes"`
		HeapObjectsBytes   uint64 `json:"heap_objects_bytes"`
		HeapStacksBytes    uint64 `json:"heap_stacks_bytes"`
		MetadataOtherBytes uint64 `json:"metadata_other_bytes"`
		Goroutines         uint64 `json:"goroutines"`
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
}
