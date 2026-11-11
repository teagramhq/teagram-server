//go:build !fixtureauthprogress

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

const rpcTraceFileEnv = "TG_RPC_TRACE_FILE"

type rpcTraceFileRecord struct {
	Method     string                 `json:"method"`
	DurationUS int64                  `json:"duration_us"`
	Result     mtproto.RPCResultClass `json:"result"`
	EndUnixMS  int64                  `json:"end_unix_ms"`
}

type rpcTraceFileExporter struct {
	mu     sync.Mutex
	file   *os.File
	closed bool
}

func newRPCTraceFileTracerFromEnv() (*mtproto.RPCTracer, *rpcTraceFileExporter, error) {
	path, supplied := os.LookupEnv(rpcTraceFileEnv)
	if !supplied || path == "" {
		return nil, nil, nil
	}
	if err := validateRPCTraceOutputPath(path); err != nil {
		return nil, nil, err
	}
	// This path is an explicit process-owner output setting, validated above.
	// #nosec G304 -- the performance harness chooses the trace artifact path.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", rpcTraceFileEnv, err)
	}
	info, err := file.Stat()
	if err != nil {
		return nil, nil, errors.Join(fmt.Errorf("stat %s: %w", rpcTraceFileEnv, err), file.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, nil, errors.Join(fmt.Errorf("%s must name a regular file", rpcTraceFileEnv), file.Close())
	}
	if err := file.Chmod(0o600); err != nil {
		return nil, nil, errors.Join(fmt.Errorf("set permissions on %s: %w", rpcTraceFileEnv, err), file.Close())
	}

	exporter := &rpcTraceFileExporter{file: file}
	tracer := mtproto.NewRPCTracer(mtproto.RPCTracerConfig{
		Exporter:            exporter,
		SynchronousExporter: true,
	})
	return tracer, exporter, nil
}

func validateRPCTraceOutputPath(path string) error {
	if filepath.Clean(path) != path || (!filepath.IsAbs(path) && !filepath.IsLocal(path)) {
		return fmt.Errorf("%s must be a clean path without parent traversal", rpcTraceFileEnv)
	}
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s must name a regular file", rpcTraceFileEnv)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", rpcTraceFileEnv, err)
	}
	return nil
}

func (e *rpcTraceFileExporter) Export(ctx context.Context, span mtproto.RPCSpan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	record := rpcTraceFileRecord{
		Method:     span.Method,
		DurationUS: span.Duration.Microseconds(),
		Result:     span.Result,
		EndUnixMS:  span.EndTime.UnixMilli(),
	}
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode RPC trace record: %w", err)
	}
	line = append(line, '\n')

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.file == nil {
		return errors.New("RPC trace file is closed")
	}
	for len(line) > 0 {
		written, err := e.file.Write(line)
		line = line[written:]
		if err != nil {
			return fmt.Errorf("append RPC trace record: %w", err)
		}
		if written == 0 {
			return fmt.Errorf("append RPC trace record: %w", io.ErrShortWrite)
		}
	}
	return nil
}

func (e *rpcTraceFileExporter) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	if e.file == nil {
		return nil
	}
	return e.file.Close()
}

func closeRPCTraceFile(tracer *mtproto.RPCTracer, exporter *rpcTraceFileExporter) error {
	if tracer == nil {
		if exporter == nil {
			return nil
		}
		return exporter.Close()
	}
	traceCloseErr := tracer.Close()
	snapshot := tracer.Snapshot()
	var exportErr error
	if snapshot.Dropped != 0 || snapshot.ExporterErrors != 0 || snapshot.ExporterPanics != 0 ||
		snapshot.ExportOutstanding != 0 || snapshot.Overflow || snapshot.Inconsistent {
		exportErr = fmt.Errorf(
			"RPC trace output incomplete: dropped=%d exporter_errors=%d exporter_panics=%d outstanding=%d overflow=%t inconsistent=%t",
			snapshot.Dropped,
			snapshot.ExporterErrors,
			snapshot.ExporterPanics,
			snapshot.ExportOutstanding,
			snapshot.Overflow,
			snapshot.Inconsistent,
		)
	}
	var fileCloseErr error
	if exporter != nil {
		fileCloseErr = exporter.Close()
	}
	return errors.Join(traceCloseErr, exportErr, fileCloseErr)
}
