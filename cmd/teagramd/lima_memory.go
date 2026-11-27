package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"runtime/metrics"
	"time"
)

const (
	limaMemoryDiagnosticsEnv  = "TG_LIMA_MEMORY_DIAGNOSTICS"
	limaMemoryDiagnosticsAddr = "127.0.0.1:2447"
	// Leave headroom for non-Go and cgroup-charged memory in the 46 MiB Lima probe.
	limaGoMemoryLimitBytes = 16 << 20
)

type limaRuntimeMemorySample struct {
	SampleUnixNano           int64  `json:"sample_unix_nano"`
	RuntimeTotalBytes        uint64 `json:"runtime_total_bytes"`
	HeapLiveBytes            uint64 `json:"heap_live_bytes"`
	HeapObjectsBytes         uint64 `json:"heap_objects_bytes"`
	HeapFreeBytes            uint64 `json:"heap_free_bytes"`
	HeapUnusedBytes          uint64 `json:"heap_unused_bytes"`
	HeapReleasedBytes        uint64 `json:"heap_released_bytes"`
	HeapStacksBytes          uint64 `json:"heap_stacks_bytes"`
	MetadataMcacheInuseBytes uint64 `json:"metadata_mcache_inuse_bytes"`
	MetadataMspanInuseBytes  uint64 `json:"metadata_mspan_inuse_bytes"`
	MetadataOtherBytes       uint64 `json:"metadata_other_bytes"`
	OtherBytes               uint64 `json:"other_bytes"`
	Goroutines               uint64 `json:"goroutines"`
}

var limaRuntimeMemoryMetricNames = [...]string{
	"/memory/classes/total:bytes",
	"/gc/heap/live:bytes",
	"/memory/classes/heap/objects:bytes",
	"/memory/classes/heap/free:bytes",
	"/memory/classes/heap/unused:bytes",
	"/memory/classes/heap/released:bytes",
	"/memory/classes/heap/stacks:bytes",
	"/memory/classes/metadata/mcache/inuse:bytes",
	"/memory/classes/metadata/mspan/inuse:bytes",
	"/memory/classes/metadata/other:bytes",
	"/memory/classes/other:bytes",
	"/sched/goroutines:goroutines",
}

func serveLimaRuntimeMemory(w http.ResponseWriter, r *http.Request) {
	if os.Getenv(limaMemoryDiagnosticsEnv) != "1" {
		http.NotFound(w, r)
		return
	}
	sample, err := readLimaRuntimeMemorySample()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(sample); err != nil {
		slog.Error("write Lima runtime memory sample", "err", err)
	}
}

func newLimaRuntimeMemoryDiagnosticsMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/runtime-memory", serveLimaRuntimeMemory)
	return mux
}

func readLimaRuntimeMemorySample() (limaRuntimeMemorySample, error) {
	samples := make([]metrics.Sample, len(limaRuntimeMemoryMetricNames))
	for index, name := range limaRuntimeMemoryMetricNames {
		samples[index].Name = name
	}
	metrics.Read(samples)
	value := func(index int) (uint64, error) {
		if samples[index].Value.Kind() != metrics.KindUint64 {
			return 0, fmt.Errorf("runtime metric %q is unavailable", samples[index].Name)
		}
		return samples[index].Value.Uint64(), nil
	}
	values := make([]uint64, len(samples))
	for index := range samples {
		var err error
		values[index], err = value(index)
		if err != nil {
			return limaRuntimeMemorySample{}, err
		}
	}
	return limaRuntimeMemorySample{
		SampleUnixNano:           time.Now().UnixNano(),
		RuntimeTotalBytes:        values[0],
		HeapLiveBytes:            values[1],
		HeapObjectsBytes:         values[2],
		HeapFreeBytes:            values[3],
		HeapUnusedBytes:          values[4],
		HeapReleasedBytes:        values[5],
		HeapStacksBytes:          values[6],
		MetadataMcacheInuseBytes: values[7],
		MetadataMspanInuseBytes:  values[8],
		MetadataOtherBytes:       values[9],
		OtherBytes:               values[10],
		Goroutines:               values[11],
	}, nil
}

func applyLimaGoMemoryLimit() func() {
	previousMemoryLimit := debug.SetMemoryLimit(-1)
	if previousMemoryLimit < 0 || previousMemoryLimit > limaGoMemoryLimitBytes {
		debug.SetMemoryLimit(limaGoMemoryLimitBytes)
	}
	return func() { debug.SetMemoryLimit(previousMemoryLimit) }
}

func startLimaRuntimeMemoryDiagnostics(log *slog.Logger) (func(), error) {
	if os.Getenv(limaMemoryDiagnosticsEnv) != "1" {
		return func() {}, nil
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(context.Background(), "tcp", limaMemoryDiagnosticsAddr)
	if err != nil {
		return nil, fmt.Errorf("listen for Lima runtime memory diagnostics: %w", err)
	}
	server := &http.Server{
		Addr:              limaMemoryDiagnosticsAddr,
		Handler:           newLimaRuntimeMemoryDiagnosticsMux(),
		ReadHeaderTimeout: time.Second,
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(listener)
	}()
	log.Info("Lima runtime memory diagnostics listening", "addr", limaMemoryDiagnosticsAddr)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Error("Lima runtime memory diagnostics shutdown", "err", err)
		}
		if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("Lima runtime memory diagnostics server", "err", err)
		}
	}, nil
}
