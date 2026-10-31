// Command public-websocket-probe exercises real Compose listeners in
// disposable public WebSocket validation. It is not included in the teagramd image.
package main

import (
	"bufio"
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
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
const cfConnectingIPHeader = "Cf-Connecting-Ip"

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
	case "proxy-v2-echo":
		return runProxyV2Echo(args[1:])
	case "keepalive-attribution":
		return runKeepaliveAttribution(args[1:])
	case "public-signup":
		return runPublicSignup(args[1:])
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New("usage: public-websocket-probe hold --transport tcp|websocket --address host:port --config file [--origin https://example.test] [--cf-connecting-ip address] | origin --address host:port --origin URL --status code [--cf-connecting-ip address] | forged --address host:port | proxy-v2-echo --listen host:port --requests count | keepalive-attribution --address host:port --first-ip address --second-ip address | public-signup --address host:port --config file --cf-connecting-ip address")
}

func runHold(args []string) error {
	flags := flag.NewFlagSet("hold", flag.ContinueOnError)
	transportName := flags.String("transport", "tcp", "transport to keep open")
	address := flags.String("address", "", "listener host:port")
	configPath := flags.String("config", "", "public teagramd client-config JSON")
	origin := flags.String("origin", "https://web.example.test", "WebSocket Origin header")
	cfConnectingIP := flags.String("cf-connecting-ip", "", "CF-Connecting-IP header value")
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
	client, dials, err := newClient(*transportName, *address, *origin, *cfConnectingIP, doc.MTProto.DCID, publicKey)
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
			probeErr := fmt.Errorf("initial application RPC: %w", err)
			fmt.Fprintln(os.Stderr, probeErr)
			return probeErr
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
					probeErr := fmt.Errorf("application RPC on established stream: %w", err)
					fmt.Fprintln(os.Stderr, probeErr)
					return probeErr
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

func newClient(transportName, address, origin, cfConnectingIP string, dcID int, publicKey *rsa.PublicKey) (*telegram.Client, *atomic.Int64, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, nil, fmt.Errorf("parse listener address: %w", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, nil, fmt.Errorf("invalid listener port %q", portText)
	}

	var dialCount atomic.Int64
	header := http.Header{"Origin": []string{origin}}
	if cfConnectingIP != "" {
		header.Set(cfConnectingIPHeader, cfConnectingIP)
	}
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
			HTTPHeader:   header,
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
	cfConnectingIP := flags.String("cf-connecting-ip", "", "CF-Connecting-IP header value")
	status := flags.Int("status", 403, "expected HTTP response status")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *address == "" || *origin == "" {
		return usageError()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	header := http.Header{"Origin": []string{*origin}}
	if *cfConnectingIP != "" {
		header.Set(cfConnectingIPHeader, *cfConnectingIP)
	}
	conn, response, err := websocket.Dial(ctx, "ws://"+*address+"/apiws", &websocket.DialOptions{
		HTTPHeader:   header,
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

func runKeepaliveAttribution(args []string) (resultErr error) {
	flags := flag.NewFlagSet("keepalive-attribution", flag.ContinueOnError)
	address := flags.String("address", "", "test HAProxy host:port")
	firstIP := flags.String("first-ip", "", "first valid CF-Connecting-IP value")
	secondIP := flags.String("second-ip", "", "second valid CF-Connecting-IP value")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *address == "" || net.ParseIP(*firstIP) == nil || net.ParseIP(*secondIP) == nil || *firstIP == *secondIP {
		return usageError()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", *address)
	if err != nil {
		return fmt.Errorf("dial test HAProxy: %w", err)
	}
	defer func() {
		if closeErr := conn.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close test HAProxy connection: %w", closeErr))
		}
	}()
	if err := conn.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
		return fmt.Errorf("set test connection deadline: %w", err)
	}
	reader := bufio.NewReader(conn)
	// Exercise per-request attribution on one persistent HTTP connection. A
	// successful WebSocket 101 turns its connection into a tunnel, so it cannot
	// carry the next visitor's HTTP request.
	for index, clientIP := range []string{*firstIP, *secondIP} {
		request := fmt.Sprintf("GET /apiws HTTP/1.1\r\nHost: example.test\r\nOrigin: https://web.example.test\r\nConnection: keep-alive\r\nCF-Connecting-IP: %s\r\n\r\n", clientIP)
		if _, err := io.WriteString(conn, request); err != nil {
			return fmt.Errorf("write keepalive request %d: %w", index+1, err)
		}
		response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
		if err != nil {
			return fmt.Errorf("read keepalive response %d: %w", index+1, err)
		}
		status := response.StatusCode
		if err := response.Body.Close(); err != nil {
			return fmt.Errorf("close keepalive response %d: %w", index+1, err)
		}
		if status != http.StatusOK {
			return fmt.Errorf("keepalive request %d returned HTTP %d, want 200", index+1, status)
		}
	}
	fmt.Println("keepalive-attribution requests=2 status=200")
	return nil
}

func runPublicSignup(args []string) error {
	flags := flag.NewFlagSet("public-signup", flag.ContinueOnError)
	address := flags.String("address", "", "public WebSocket host:port")
	configPath := flags.String("config", "", "public teagramd client-config JSON")
	cfConnectingIP := flags.String("cf-connecting-ip", "", "CF-Connecting-IP header value")
	origin := flags.String("origin", "https://web.example.test", "WebSocket Origin header")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *address == "" || *configPath == "" || net.ParseIP(*cfConnectingIP) == nil {
		return usageError()
	}
	doc, publicKey, err := readClientConfig(*configPath)
	if err != nil {
		return err
	}
	client, _, err := newClient("websocket", *address, *origin, *cfConnectingIP, doc.MTProto.DCID, publicKey)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	if err := client.Run(ctx, func(ctx context.Context) error {
		identifier := fmt.Sprintf("publicsmoke%d", time.Now().UnixNano())
		sent, err := client.API().AuthSendCode(ctx, &tg.AuthSendCodeRequest{
			PhoneNumber: identifier,
			APIID:       1,
			APIHash:     "hash",
		})
		if err != nil {
			return fmt.Errorf("public auth.sendCode: %w", err)
		}
		code, ok := sent.(*tg.AuthSentCode)
		if !ok {
			return fmt.Errorf("public auth.sendCode response = %T, want *tg.AuthSentCode", sent)
		}
		_, err = client.API().AuthSignUp(ctx, &tg.AuthSignUpRequest{
			PhoneNumber:   identifier,
			PhoneCodeHash: code.PhoneCodeHash,
			FirstName:     "Public",
		})
		if err == nil {
			return errors.New("public auth.signUp succeeded without an invite; want INVITE_HASH_INVALID")
		}
		if !strings.Contains(err.Error(), "INVITE_HASH_INVALID") {
			return fmt.Errorf("public auth.signUp error, want INVITE_HASH_INVALID without an invite: %w", err)
		}
		fmt.Println("public-signup refused=invite-required")
		return nil
	}); err != nil {
		return fmt.Errorf("public signup policy probe: %w", err)
	}
	return nil
}

func runProxyV2Echo(args []string) (resultErr error) {
	flags := flag.NewFlagSet("proxy-v2-echo", flag.ContinueOnError)
	listenAddress := flags.String("listen", ":2446", "PROXY-v2 echo listen address")
	requestCount := flags.Int("requests", 2, "number of HTTP requests to accept")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *requestCount < 1 || *requestCount > 10 {
		return usageError()
	}
	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", *listenAddress)
	if err != nil {
		return fmt.Errorf("listen for PROXY-v2 test traffic: %w", err)
	}
	defer func() {
		if closeErr := listener.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			resultErr = errors.Join(resultErr, fmt.Errorf("close PROXY-v2 test listener: %w", closeErr))
		}
	}()
	fmt.Println("proxy-v2-echo-ready")
	observed, err := serveProxyV2Echo(listener, *requestCount)
	if err != nil {
		return err
	}
	fmt.Printf("proxy-v2-attribution=%s\n", strings.Join(observed, ","))
	return nil
}

// serveProxyV2Echo records the PROXY-v2 source of each proxied request, in
// arrival order. Connections are served concurrently: HAProxy keeps an earlier
// backend connection idle on the client session and opens a new one when the
// next request carries a different source address.
func serveProxyV2Echo(listener net.Listener, requestCount int) ([]string, error) {
	sources := make(chan string)
	failures := make(chan error, 1)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	defer wg.Wait()
	defer close(stop)

	fail := func(err error) {
		select {
		case failures <- err:
		default:
		}
	}
	serve := func(conn net.Conn) {
		defer func() {
			if err := closeProbeConn(conn, "close PROXY-v2 test backend connection"); err != nil && !errors.Is(err, net.ErrClosed) {
				fail(err)
			}
		}()
		go func() {
			<-stop
			if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				fail(fmt.Errorf("close PROXY-v2 test backend connection: %w", err))
			}
		}()
		if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
			fail(fmt.Errorf("set PROXY-v2 test backend deadline: %w", err))
			return
		}
		reader := bufio.NewReader(conn)
		clientIP, err := readProxyV2Source(reader)
		if err != nil {
			fail(err)
			return
		}
		for {
			request, err := http.ReadRequest(reader)
			if err != nil {
				select {
				case <-stop:
				default:
					if !errors.Is(err, io.EOF) {
						fail(fmt.Errorf("read proxied HTTP request: %w", err))
					}
				}
				return
			}
			if err := request.Body.Close(); err != nil {
				fail(fmt.Errorf("close proxied HTTP request body: %w", err))
				return
			}
			if _, err := io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: keep-alive\r\n\r\n"); err != nil {
				fail(fmt.Errorf("write test backend response: %w", err))
				return
			}
			select {
			case sources <- clientIP.String():
			case <-stop:
				return
			}
		}
	}
	wg.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-stop:
				default:
					fail(fmt.Errorf("accept PROXY-v2 test backend connection: %w", err))
				}
				return
			}
			wg.Go(func() { serve(conn) })
		}
	})
	go func() {
		<-stop
		if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			fail(fmt.Errorf("close PROXY-v2 test listener: %w", err))
		}
	}()

	var observed []string
	for len(observed) < requestCount {
		select {
		case source := <-sources:
			observed = append(observed, source)
		case err := <-failures:
			return nil, err
		}
	}
	return observed, nil
}

func closeProbeConn(conn net.Conn, description string) error {
	if err := conn.Close(); err != nil {
		return fmt.Errorf("%s: %w", description, err)
	}
	return nil
}

func readProxyV2Source(reader io.Reader) (netip.Addr, error) {
	var header [16]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return netip.Addr{}, fmt.Errorf("read PROXY-v2 header: %w", err)
	}
	if string(header[:12]) != "\r\n\r\n\x00\r\nQUIT\n" || header[12] != 0x21 || header[13] != 0x11 || binary.BigEndian.Uint16(header[14:]) != 12 {
		return netip.Addr{}, errors.New("invalid PROXY-v2 IPv4 stream header")
	}
	var addressBytes [4]byte
	var addressTail [8]byte
	if _, err := io.ReadFull(reader, addressBytes[:]); err != nil {
		return netip.Addr{}, fmt.Errorf("read PROXY-v2 source address: %w", err)
	}
	if _, err := io.ReadFull(reader, addressTail[:]); err != nil {
		return netip.Addr{}, fmt.Errorf("read PROXY-v2 address tail: %w", err)
	}
	return netip.AddrFrom4(addressBytes), nil
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
