// Command mixed-trust-probe exercises real Compose listeners during disposable
// mixed-trust validation. It is not included in the telegramd image.
package main

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/discovery"
)

const rpcTimeout = 5 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usageError()
	}
	switch args[0] {
	case "hold":
		return runHold(args[1:])
	case "origin":
		return runOrigin(args[1:])
	case "forged":
		return runForged(args[1:])
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New("usage: mixed-trust-probe hold --transport tcp|websocket --address host:port --config file [--origin https://example.test] | origin --address host:port --origin URL --status code | forged --address host:port")
}

func runHold(args []string) error {
	flags := flag.NewFlagSet("hold", flag.ContinueOnError)
	transportName := flags.String("transport", "tcp", "transport to keep open")
	address := flags.String("address", "", "listener host:port")
	configPath := flags.String("config", "", "public telegramd client-config JSON")
	origin := flags.String("origin", "https://web.example.test", "WebSocket Origin header")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*transportName != "tcp" && *transportName != "websocket") || *address == "" || *configPath == "" {
		return usageError()
	}

	doc, publicKey, err := readClientConfig(*configPath)
	if err != nil {
		return err
	}
	client, dials, err := newClient(*transportName, *address, *origin, doc.MTProto.DCID, publicKey)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = client.Run(ctx, func(ctx context.Context) error {
		if err := withRPCDeadline(ctx, rpcTimeout, func(rpcCtx context.Context) error {
			_, err := client.API().HelpGetConfig(rpcCtx)
			return err
		}); err != nil {
			return fmt.Errorf("initial application RPC: %w", err)
		}
		initialDials := dials.Load()
		if initialDials == 0 {
			return errors.New("client completed an RPC without opening a transport")
		}
		fmt.Printf("stream-ready transport=%s dials=%d\n", *transportName, initialDials)

		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-ticker.C:
				if err := withRPCDeadline(ctx, rpcTimeout, func(rpcCtx context.Context) error {
					_, err := client.API().HelpGetConfig(rpcCtx)
					return err
				}); err != nil {
					return fmt.Errorf("application RPC on established stream: %w", err)
				}
				if current := dials.Load(); current != initialDials {
					return fmt.Errorf("transport reconnected during hold: dials changed from %d to %d", initialDials, current)
				}
				fmt.Printf("stream-heartbeat transport=%s\n", *transportName)
			}
		}
	})
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func withRPCDeadline(ctx context.Context, timeout time.Duration, call func(context.Context) error) error {
	rpcCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return call(rpcCtx)
}

func newClient(transportName, address, origin string, dcID int, publicKey *rsa.PublicKey) (*telegram.Client, *atomic.Int64, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, nil, fmt.Errorf("parse listener address: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, nil, fmt.Errorf("invalid listener port %q", portText)
	}

	var dialCount atomic.Int64
	var resolver dcs.Resolver
	switch transportName {
	case "tcp":
		dialer := &net.Dialer{}
		resolver = dcs.Plain(dcs.PlainOptions{
			Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
				dialCount.Add(1)
				return dialer.DialContext(ctx, network, addr)
			},
		})
	case "websocket":
		dialer := &net.Dialer{}
		httpTransport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialCount.Add(1)
			return dialer.DialContext(ctx, network, addr)
		}}
		resolver = dcs.Websocket(dcs.WebsocketOptions{DialOptions: &websocket.DialOptions{
			HTTPClient:   &http.Client{Transport: httpTransport},
			HTTPHeader:   http.Header{"Origin": []string{origin}},
			Host:         "example.test",
			Subprotocols: []string{"binary"},
		}})
	}

	client := telegram.NewClient(1, "hash", telegram.Options{
		DC: dcID,
		DCList: dcs.List{
			Options: []tg.DCOption{{ID: dcID, IPAddress: host, Port: port}},
			Domains: map[int]string{dcID: "ws://" + address + "/apiws"},
		},
		PublicKeys:     []telegram.PublicKey{{RSA: publicKey}},
		Resolver:       resolver,
		SessionStorage: &session.StorageMemory{},
	})
	return client, &dialCount, nil
}

func readClientConfig(path string) (discovery.Document, *rsa.PublicKey, error) {
	const maxClientConfigBytes = 1 << 20
	info, err := os.Lstat(path)
	if err != nil {
		return discovery.Document{}, nil, fmt.Errorf("inspect client config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxClientConfigBytes {
		return discovery.Document{}, nil, errors.New("client config must be a non-empty regular file no larger than 1 MiB")
	}
	file, err := os.Open(filepath.Clean(path)) // #nosec G304 -- the isolated smoke script supplies this temporary public discovery file; it is checked and bounded below.
	if err != nil {
		return discovery.Document{}, nil, fmt.Errorf("open client config: %w", err)
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil {
		closeErr := file.Close()
		return discovery.Document{}, nil, errors.Join(fmt.Errorf("stat client config: %w", statErr), closeErr)
	}
	if !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() || openedInfo.Size() <= 0 || openedInfo.Size() > maxClientConfigBytes {
		if closeErr := file.Close(); closeErr != nil {
			return discovery.Document{}, nil, fmt.Errorf("close invalid client config: %w", closeErr)
		}
		return discovery.Document{}, nil, errors.New("client config changed or exceeded its size limit while opening")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxClientConfigBytes+1))
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return discovery.Document{}, nil, fmt.Errorf("read client config: %w", err)
	}
	if len(data) == 0 || len(data) > maxClientConfigBytes {
		return discovery.Document{}, nil, errors.New("client config is empty or exceeds its size limit")
	}
	var doc discovery.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return discovery.Document{}, nil, fmt.Errorf("parse client config: %w", err)
	}
	if _, err := discovery.MarshalDocument(doc); err != nil {
		return discovery.Document{}, nil, fmt.Errorf("validate client config: %w", err)
	}
	der, err := base64.StdEncoding.DecodeString(doc.MTProto.RSASPKI)
	if err != nil {
		return discovery.Document{}, nil, fmt.Errorf("decode client RSA key: %w", err)
	}
	parsed, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return discovery.Document{}, nil, fmt.Errorf("parse client RSA key: %w", err)
	}
	publicKey, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return discovery.Document{}, nil, errors.New("client config RSA key has the wrong type")
	}
	return doc, publicKey, nil
}

func runOrigin(args []string) error {
	flags := flag.NewFlagSet("origin", flag.ContinueOnError)
	address := flags.String("address", "", "listener host:port")
	origin := flags.String("origin", "", "Origin header")
	status := flags.Int("status", 403, "expected HTTP response status")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *address == "" || *origin == "" {
		return usageError()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, "ws://"+*address+"/apiws", &websocket.DialOptions{
		HTTPHeader:   http.Header{"Origin": []string{*origin}},
		Host:         "example.test",
		Subprotocols: []string{"binary"},
	})
	gotStatus := 0
	if response != nil {
		gotStatus = response.StatusCode
		if response.Body != nil {
			if closeErr := response.Body.Close(); closeErr != nil {
				return fmt.Errorf("close WebSocket rejection response: %w", closeErr)
			}
		}
	}
	if err == nil && conn != nil {
		if closeErr := conn.CloseNow(); closeErr != nil {
			return fmt.Errorf("close unexpected WebSocket connection: %w", closeErr)
		}
	}
	if gotStatus != *status {
		if err != nil {
			return fmt.Errorf("WebSocket Origin %q returned HTTP %d, want %d: %w", *origin, gotStatus, *status, err)
		}
		return fmt.Errorf("WebSocket Origin %q returned HTTP %d, want %d", *origin, gotStatus, *status)
	}
	fmt.Printf("websocket-origin status=%d\n", gotStatus)
	return nil
}

func runForged(args []string) (resultErr error) {
	flags := flag.NewFlagSet("forged", flag.ContinueOnError)
	address := flags.String("address", "", "proxy-trust replica host:port")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *address == "" {
		return usageError()
	}

	dialer := net.Dialer{Timeout: 2 * time.Second}
	conn, err := dialer.DialContext(context.Background(), "tcp", *address)
	if err != nil {
		return fmt.Errorf("dial proxy-trust replica: %w", err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close forged-header connection: %w", closeErr))
		}
	}()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		return fmt.Errorf("set forged-header deadline: %w", err)
	}
	if _, err := conn.Write(forgedProxyV2Header()); err != nil {
		return fmt.Errorf("write forged PROXY-v2 header: %w", err)
	}
	var response [1]byte
	n, err := conn.Read(response[:])
	if n != 0 {
		return errors.New("untrusted peer received application bytes after a forged PROXY-v2 header")
	}
	if err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		return errors.New("untrusted peer was not promptly refused after a forged PROXY-v2 header")
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) && networkErr.Timeout() {
		return errors.New("untrusted peer was not promptly refused after a forged PROXY-v2 header")
	}
	fmt.Println("forged-proxy-v2-refused")
	return nil
}

func forgedProxyV2Header() []byte {
	header := []byte{
		0x0d, 0x0a, 0x0d, 0x0a, 0x00, 0x0d, 0x0a, 0x51,
		0x55, 0x49, 0x54, 0x0a, 0x21, 0x11, 0x00, 0x0c,
	}
	// Claimed source and destination are documentation-only addresses. The
	// test succeeds only when the real socket peer, not this claim, is refused.
	header = append(header, 198, 51, 100, 123, 198, 18, 129, 21)
	header = binary.BigEndian.AppendUint16(header, 51000)
	header = binary.BigEndian.AppendUint16(header, 2443)
	return header
}
