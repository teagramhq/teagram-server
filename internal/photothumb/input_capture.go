package photothumb

import "sync"

// InputCapture is the bounded, best-effort copy of the exact bytes sent to the
// original blob and structural validator. Its Write method never blocks on a
// worker or fails the original upload: once the worker branch is discarded,
// further bytes are accepted and dropped.
type InputCapture struct {
	mu     sync.Mutex
	buffer []byte
	closed bool
}

// NewInputCapture creates an empty capture for one admitted photo upload.
func NewInputCapture() *InputCapture {
	return &InputCapture{}
}

// Write appends bytes while the capture remains within the worker's input
// ceiling. Overflow discards the accumulated copy and switches the branch to
// io.Discard semantics without interrupting its caller.
func (capture *InputCapture) Write(data []byte) (int, error) {
	if capture == nil {
		return len(data), nil
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.closed {
		return len(data), nil
	}
	needed := len(capture.buffer) + len(data)
	if needed > maxEncodedBytes {
		clear(capture.buffer)
		capture.buffer = nil
		capture.closed = true
		return len(data), nil
	}
	if needed > cap(capture.buffer) {
		capacity := max(cap(capture.buffer)*2, 4096)
		capacity = min(capacity, maxEncodedBytes)
		capacity = max(capacity, needed)
		buffer := make([]byte, len(capture.buffer), capacity)
		copy(buffer, capture.buffer)
		capture.buffer = buffer
	}
	capture.buffer = append(capture.buffer, data...)
	return len(data), nil
}

// Discard releases the captured bytes and makes later writes behave like
// io.Discard. Use it when the derivative branch becomes ineligible or fails.
func (capture *InputCapture) Discard() {
	if capture == nil {
		return
	}
	capture.mu.Lock()
	clear(capture.buffer)
	capture.buffer = nil
	capture.closed = true
	capture.mu.Unlock()
}

// Finish transfers the exact captured bytes after the original Put and
// structural validation have succeeded. A short, oversized or discarded
// capture returns no data and permanently discards the branch.
func (capture *InputCapture) Finish(expectedBytes int64) ([]byte, bool) {
	if capture == nil {
		return nil, false
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.closed || expectedBytes < 1 || expectedBytes > maxEncodedBytes || int64(len(capture.buffer)) != expectedBytes {
		clear(capture.buffer)
		capture.buffer = nil
		capture.closed = true
		return nil, false
	}
	input := capture.buffer
	capture.buffer = nil
	capture.closed = true
	return input, true
}
