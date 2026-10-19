package main

import (
	"log/slog"
	"net"

	"github.com/gotd/td/exchange"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func newListenerScopedServer(
	cfg config.Config,
	key exchange.PrivateKey,
	st *store.Store,
	log *slog.Logger,
	newHandler func(config.RegistrationMode) mtproto.Handler,
) (*mtproto.Server, mtproto.Handler) {
	tailnetHandler := newHandler(cfg.RegistrationMode)
	var publicHandler mtproto.Handler
	if cfg.PublicWebSocketListenAddr != "" {
		publicHandler = newHandler(cfg.PublicRegistrationMode)
	}
	server := mtproto.New(key, cfg.DCID, mtproto.NewPgAuthKeyStore(st), tailnetHandler, log)
	return server, publicHandler
}

func newPublicWebSocketListener(cfg config.Config, listener net.Listener, handler mtproto.Handler) *publicWebSocketListener {
	return &publicWebSocketListener{
		listener:       listener,
		handler:        handler,
		originPatterns: cfg.PublicWebSocketOriginPatterns,
	}
}
