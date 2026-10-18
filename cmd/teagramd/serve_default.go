//go:build !fixtureauthprogress

package main

import (
	"context"
	"errors"
	"net"
	"os"

	"github.com/gotd/td/tdsync"

	"github.com/teagramhq/teagram-server/internal/mtproto"
)

func validateServeEnvironment() error {
	if _, supplied := os.LookupEnv("TG_FIXTURE_AUTH_PROGRESS"); supplied {
		return errors.New("TG_FIXTURE_AUTH_PROGRESS is only supported by fixtureauthprogress builds")
	}
	return nil
}

func serveMTProto(ctx context.Context, server *mtproto.Server, tcp net.Listener, websocket net.Listener) error {
	if websocket == nil {
		return server.Serve(ctx, tcp)
	}
	group := tdsync.NewCancellableGroup(ctx)
	group.Go(func(ctx context.Context) error { return server.Serve(ctx, tcp) })
	group.Go(func(ctx context.Context) error { return server.ServeWebSocket(ctx, websocket) })
	return group.Wait()
}
