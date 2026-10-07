package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/teagramhq/teagram-server/internal/rsakey"
)

func main() {
	endpoint := flag.String("endpoint", "wss://telegramd.test/apiws", "fixed fixture MTProto WebSocket endpoint")
	publicKeyPath := flag.String("public-key", "/run/secrets/server.pub.pem", "fixture server RSA public key")
	tlsRootPath := flag.String("tls-root-file", "/run/secrets/tls.crt", "per-run TLS leaf certificate")
	username := flag.String("username", "", "synthetic username")
	passwordPath := flag.String("password-file", "", "protected synthetic password file")
	closedSignup := flag.Bool("closed-signup", false, "assert registration is closed")
	sendTo := flag.String("send-message-to", "", "synthetic recipient username")
	message := flag.String("message", "", "synthetic isolation probe text")
	fingerprintOnly := flag.Bool("fingerprint-only", false, "print the public key fingerprint")
	flag.Parse()

	key, err := loadPublicKey(*publicKeyPath)
	fatalIf(err)
	if *fingerprintOnly {
		//nolint:gosec // The protocol fingerprint is a 64-bit bit pattern stored in a signed integer.
		fmt.Printf("%016x\n", uint64(rsakey.Fingerprint(key)))
		return
	}
	if *endpoint != "wss://telegramd.test/apiws" {
		fatalIf(errors.New("fixture endpoint must be wss://telegramd.test/apiws"))
	}
	rootPEM, err := os.ReadFile(*tlsRootPath)
	fatalIf(err)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		fatalIf(errors.New("per-run TLS leaf certificate could not be loaded"))
	}
	resolver := dcs.Websocket(dcs.WebsocketOptions{DialOptions: &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		}}},
		HTTPHeader:   http.Header{"Origin": []string{"https://telegramd.test"}},
		Subprotocols: []string{"binary"},
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client := telegram.NewClient(1, "fixture", telegram.Options{
		DC: 2,
		DCList: dcs.List{
			Options: []tg.DCOption{{ID: 2, IPAddress: "telegramd.test", Port: 443}},
			Domains: map[int]string{2: *endpoint},
		},
		PublicKeys:     []telegram.PublicKey{{RSA: key}},
		Resolver:       resolver,
		SessionStorage: &session.StorageMemory{},
	})
	if *closedSignup {
		err := client.Run(ctx, func(ctx context.Context) error {
			_, err := client.API().AuthSignUp(ctx, &tg.AuthSignUpRequest{
				PhoneNumber: "closedfixture1",
				FirstName:   "Closed",
			})
			if !rpcMessage(err, "INPUT_REQUEST_INVALID") {
				if err == nil {
					return errors.New("closed registration unexpectedly succeeded, want INPUT_REQUEST_INVALID")
				}
				return fmt.Errorf("closed registration response = %w, want INPUT_REQUEST_INVALID", err)
			}
			return nil
		})
		fatalIf(err)
		fmt.Println("registration=closed")
		return
	}
	if *username == "" || *passwordPath == "" {
		fatalIf(errors.New("username and protected password file are required"))
	}
	password, err := os.ReadFile(*passwordPath)
	fatalIf(err)
	password = []byte(strings.TrimSuffix(strings.TrimSuffix(string(password), "\n"), "\r"))
	if len(password) == 0 {
		fatalIf(errors.New("empty synthetic password"))
	}

	err = client.Run(ctx, func(ctx context.Context) error {
		api := client.API()
		sent, err := api.AuthSendCode(ctx, &tg.AuthSendCodeRequest{
			PhoneNumber: *username,
			APIID:       1,
			APIHash:     "fixture",
		})
		if err != nil {
			return fmt.Errorf("auth.sendCode: %w", err)
		}
		code, ok := sent.(*tg.AuthSentCode)
		if !ok {
			return fmt.Errorf("auth.sendCode returned %T, want *tg.AuthSentCode", sent)
		}
		_, err = api.AuthSignIn(ctx, &tg.AuthSignInRequest{
			PhoneNumber:   *username,
			PhoneCodeHash: code.PhoneCodeHash,
		})
		if !rpcMessage(err, "SESSION_PASSWORD_NEEDED") {
			if err == nil {
				return errors.New("auth.signIn unexpectedly succeeded, want SESSION_PASSWORD_NEEDED")
			}
			return fmt.Errorf("auth.signIn response = %w, want SESSION_PASSWORD_NEEDED", err)
		}
		settings, err := api.AccountGetPassword(ctx)
		if err != nil {
			return fmt.Errorf("account.getPassword: %w", err)
		}
		proof, err := auth.PasswordHash(password, settings.SRPID, settings.SRPB, settings.SecureRandom, settings.CurrentAlgo)
		if err != nil {
			return fmt.Errorf("compute SRP proof: %w", err)
		}
		response, err := api.AuthCheckPassword(ctx, proof)
		if err != nil {
			return fmt.Errorf("auth.checkPassword: %w", err)
		}
		if _, ok := response.(*tg.AuthAuthorization); !ok {
			return fmt.Errorf("auth.checkPassword returned %T, want authorization", response)
		}
		status, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("authorization status: %w", err)
		}
		if !status.Authorized {
			return errors.New("authorization status is false after SRP login")
		}
		if *sendTo != "" {
			if *message == "" {
				return errors.New("message is required with recipient")
			}
			peer, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: *sendTo})
			if err != nil {
				return fmt.Errorf("resolve synthetic recipient: %w", err)
			}
			if len(peer.Users) != 1 {
				return fmt.Errorf("resolve synthetic recipient returned %d users, want one", len(peer.Users))
			}
			user, ok := peer.Users[0].(*tg.User)
			if !ok {
				return fmt.Errorf("resolved synthetic recipient has type %T, want *tg.User", peer.Users[0])
			}
			randomID, err := randomID()
			if err != nil {
				return fmt.Errorf("generate message random ID: %w", err)
			}
			if _, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
				Peer: &tg.InputPeerUser{UserID: user.ID, AccessHash: user.AccessHash}, Message: *message, RandomID: randomID,
			}); err != nil {
				return fmt.Errorf("send synthetic isolation message: %w", err)
			}
		}
		return nil
	})
	fatalIf(err)
	fmt.Println("authentication=authorized")
}

func randomID() (int64, error) {
	var data [8]byte
	if _, err := rand.Read(data[:]); err != nil {
		return 0, err
	}
	//nolint:gosec // MTProto random IDs are signed 64-bit bit patterns; retain all random bits.
	value := int64(binary.LittleEndian.Uint64(data[:]))
	if value == 0 {
		return 1, nil
	}
	return value, nil
}

func loadPublicKey(path string) (*rsa.PublicKey, error) {
	//nolint:gosec // Fixture callers pass only the public-key path mounted inside the isolated client container.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(data)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("public key is not a single PEM block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse RSA public key: %w", err)
	}
	key, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key type is %T, want RSA", parsed)
	}
	return key, nil
}

func rpcMessage(err error, want string) bool {
	var rpcErr *tgerr.Error
	return errors.As(err, &rpcErr) && rpcErr.Message == want
}

func fatalIf(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
