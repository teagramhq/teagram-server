//go:build performance

package perf_test

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
)

const dispatchWaitProbeTimeout = 20 * time.Second

func searchCountersDispatchWait(ctx context.Context, e *env) (call, string, error) {
	peer := userPeer(e.f.Owner, e.f.Partner)
	request := &tg.MessagesGetSearchCountersRequest{
		Peer: peer, Filters: []tg.MessagesFilterClass{&tg.InputMessagesFilterPhotos{}},
	}
	return func(ctx context.Context) error {
		probeCtx, cancel := context.WithTimeout(ctx, dispatchWaitProbeTimeout)
		queryStarted := e.srv.tracer.watchQueryStart("CountAllFilteredMessages")
		counterDone := make(chan error, 1)
		go func() {
			_, err := e.owner.api.MessagesGetSearchCounters(probeCtx, request)
			counterDone <- err
		}()
		counterFinished := false
		defer func() {
			cancel()
			if !counterFinished {
				<-counterDone
			}
		}()

		if err := waitDispatchProbe(probeCtx, queryStarted, "search counters query"); err != nil {
			return err
		}
		started := time.Now()
		_, err := e.owner.api.HelpGetConfig(probeCtx)
		e.elapsed = time.Since(started)
		e.maxConcurrentRequestLatency = e.elapsed
		e.concurrentRequestSamples = 1
		if err != nil {
			return fmt.Errorf("help.getConfig during search counters: %w", err)
		}
		select {
		case err := <-counterDone:
			counterFinished = true
			if err != nil {
				return fmt.Errorf("messages.getSearchCounters: %w", err)
			}
		case <-probeCtx.Done():
			return fmt.Errorf("messages.getSearchCounters did not finish: %w", probeCtx.Err())
		}
		return nil
	}, "", nil
}

func waitDispatchProbe(ctx context.Context, ready <-chan struct{}, what string) error {
	select {
	case <-ready:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait for %s: %w", what, ctx.Err())
	}
}
