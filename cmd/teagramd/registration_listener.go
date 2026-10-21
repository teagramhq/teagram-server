package main

import (
	"log/slog"
	"net"

	"github.com/gotd/td/exchange"

	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/discovery"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

func newListenerScopedServer(
	cfg config.Config,
	key exchange.PrivateKey,
	st *store.Store,
	log *slog.Logger,
	newHandler func(config.RegistrationMode, bool, bool) mtproto.Handler,
) (*mtproto.Server, mtproto.Handler) {
	turnstileOnDefaultListener := cfg.Turnstile.Enabled() && cfg.PublicWebSocketListenAddr == ""
	signInOnDefaultListener := turnstileOnDefaultListener && cfg.Turnstile.SignInEnabled
	tailnetHandler := newHandler(cfg.RegistrationMode, turnstileOnDefaultListener, signInOnDefaultListener)
	var publicHandler mtproto.Handler
	if cfg.PublicWebSocketListenAddr != "" {
		publicTurnstileEnabled := cfg.Turnstile.Enabled()
		publicHandler = newHandler(cfg.PublicRegistrationMode, publicTurnstileEnabled, publicTurnstileEnabled && cfg.Turnstile.SignInEnabled)
	}
	server := mtproto.New(key, cfg.DCID, mtproto.NewPgAuthKeyStore(st), tailnetHandler, log)
	return server, publicHandler
}

func newPublicWebSocketListener(cfg config.Config, listener net.Listener, handler mtproto.Handler, doc discovery.Document) *publicWebSocketListener {
	return &publicWebSocketListener{
		listener:       listener,
		handler:        handler,
		originPatterns: cfg.PublicWebSocketOriginPatterns,
		discovery:      doc,
	}
}
