// Command teagramd runs the MTProto server.
package main

import (
	"bufio"
	"context"
	cryptorand "crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	gotdsrp "github.com/gotd/td/crypto/srp"
	"github.com/gotd/td/exchange"

	"github.com/teagramhq/teagram-server/internal/admin"
	"github.com/teagramhq/teagram-server/internal/api"
	"github.com/teagramhq/teagram-server/internal/blob"
	"github.com/teagramhq/teagram-server/internal/blobmode"
	"github.com/teagramhq/teagram-server/internal/blobscan"
	"github.com/teagramhq/teagram-server/internal/botapi"
	"github.com/teagramhq/teagram-server/internal/config"
	"github.com/teagramhq/teagram-server/internal/discovery"
	"github.com/teagramhq/teagram-server/internal/fleet"
	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/peerhash"
	"github.com/teagramhq/teagram-server/internal/photohash"
	"github.com/teagramhq/teagram-server/internal/photothumb"
	"github.com/teagramhq/teagram-server/internal/rsakey"
	tsrp "github.com/teagramhq/teagram-server/internal/srp"
	"github.com/teagramhq/teagram-server/internal/store"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := runCommand(os.Args[1:], log, os.Stdout, os.Stderr); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// sweepInterval is how often the background sweep deletes expired login codes.
const sweepInterval = 5 * time.Minute

const blobModeDirectory = "/run/teagramd/blob-mode"

// adminShutdownTimeout bounds how long the admin server waits for in-flight
// requests to finish before it gives up.
const adminShutdownTimeout = 5 * time.Second

const botAPIShutdownTimeout = 5 * time.Second

type publicWebSocketListener struct {
	listener       net.Listener
	handler        mtproto.Handler
	originPatterns []string
	discovery      discovery.Document
}

func runCommand(args []string, log *slog.Logger, stdout, stderr io.Writer) error {
	switch {
	case len(args) == 0:
		return run(log)
	case args[0] == "serve":
		if len(args) != 1 {
			return errors.New("serve takes no arguments")
		}
		return run(log)
	case args[0] == "invite":
		return runInviteCommand(args[1:], log, stdout, stderr)
	case args[0] == "bot":
		return runBotCommand(args[1:], log, stdout, stderr)
	case args[0] == "maintenance":
		return runMaintenanceCommand(args[1:], log, stdout, stderr)
	case args[0] == "admin":
		return runAdminCommand(args[1:], os.Stdin, log, stderr)
	case args[0] == "client-config":
		if len(args) == 2 && slices.Contains(args[1:], "--help") {
			return writeClientConfigUsage(stdout)
		}
		if len(args) != 1 {
			return errors.New("client-config takes no arguments")
		}
		return runClientConfigCommand(stdout)
	case args[0] == "bootstrap-identity":
		if len(args) == 2 && slices.Contains(args[1:], "--help") {
			return writeBootstrapIdentityUsage(stdout)
		}
		if len(args) != 1 {
			return errors.New("bootstrap-identity takes no arguments")
		}
		return runBootstrapIdentityCommand(stdout)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

var adminUsernameRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{1,31}$`)

func runAdminCommand(args []string, stdin io.Reader, _ *slog.Logger, stderr io.Writer) (err error) {
	return runAdminCommandWithEUID(args, stdin, stderr, os.Geteuid())
}

func runAdminCommandWithEUID(args []string, stdin io.Reader, stderr io.Writer, euid int) (err error) {
	if len(args) > 0 && args[0] == "create-user" {
		if euid != 0 {
			return errors.New("admin create-user requires root")
		}
		return runAdminCreateUserCommand(args[1:], stdin, stderr)
	}
	if len(args) == 0 || args[0] != "set-password" {
		return adminUsageError()
	}
	handle, err := parseAdminSetPasswordArgs(args[1:])
	if err != nil {
		return err
	}
	handle = strings.TrimPrefix(handle, "@")
	if !adminUsernameRE.MatchString(handle) {
		return errors.New("invalid username handle")
	}
	handle = strings.ToLower(handle)

	password, err := readAdminPassword(stdin)
	if err != nil {
		return err
	}
	defer clear(password)

	salt1 := make([]byte, 32)
	if _, err := io.ReadFull(cryptorand.Reader, salt1); err != nil {
		return fmt.Errorf("generate password salt: %w", err)
	}
	salt2 := make([]byte, 32)
	if _, err := io.ReadFull(cryptorand.Reader, salt2); err != nil {
		return fmt.Errorf("generate password salt: %w", err)
	}
	verifier, augmentedSalt1, err := gotdsrp.NewSRP(cryptorand.Reader).NewHash(password, gotdsrp.Input{
		Salt1: salt1,
		Salt2: salt2,
		G:     tsrp.G,
		P:     tsrp.PBytes(),
	})
	if err != nil {
		return fmt.Errorf("generate SRP verifier: %w", err)
	}
	defer clear(verifier)
	if len(verifier) != tsrp.PadLen || !tsrp.ValidVerifier(verifier) {
		return errors.New("generate SRP verifier: invalid verifier")
	}

	// Config and store startup logs can add paths and other details to stderr;
	// the operator command exposes only its explicit confirmation or error.
	quietLog := slog.New(slog.DiscardHandler)
	cfg, err := config.Load(quietLog)
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN, cfg.AuthKeyEncKey,
		store.WithLogger(quietLog),
		store.WithPoolConfig(cfg.PostgresPool),
		store.WithStatementTimeout(cfg.StatementTimeout),
		store.WithSessionIP(cfg.SessionIP),
		store.WithSessionDeviceLabels(cfg.SessionDeviceLabels),
		store.WithoutBlobStore(),
	)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close store: %w", closeErr))
		}
	}()

	reset, err := st.ResetUsernamePassword(ctx, handle, augmentedSalt1, salt2, verifier)
	if err != nil {
		return fmt.Errorf("reset username password: %w", err)
	}
	if _, err := fmt.Fprintf(stderr, "Password reset: %s (user id: %d)\n", reset.Handle, reset.UserID); err != nil {
		return fmt.Errorf("write password reset confirmation: %w", err)
	}
	return nil
}

func parseAdminSetPasswordArgs(args []string) (string, error) {
	var flag, username string
	for index, arg := range args {
		switch index {
		case 0:
			flag = arg
		case 1:
			username = arg
		default:
			return "", adminUsageError()
		}
	}
	switch len(args) {
	case 2:
		if flag == "--username" {
			return username, nil
		}
	case 1:
		if username, ok := strings.CutPrefix(flag, "--username="); ok {
			return username, nil
		}
	}
	return "", adminUsageError()
}

func adminUsageError() error {
	return errors.New("usage: teagramd admin set-password --username <handle>")
}

func runAdminCreateUserCommand(args []string, stdin io.Reader, stderr io.Writer) (err error) {
	handle, err := parseAdminCreateUserArgs(args)
	if err != nil {
		return err
	}
	handle = strings.TrimPrefix(handle, "@")
	if !adminUsernameRE.MatchString(handle) {
		return errors.New("invalid username handle")
	}
	handle = strings.ToLower(handle)
	if api.IsReservedUsername(handle) {
		return errors.New("reserved username handle")
	}

	password, err := readAdminPassword(stdin)
	if err != nil {
		return err
	}
	defer clear(password)

	salt1 := make([]byte, 32)
	if _, err := io.ReadFull(cryptorand.Reader, salt1); err != nil {
		return fmt.Errorf("generate password salt: %w", err)
	}
	salt2 := make([]byte, 32)
	if _, err := io.ReadFull(cryptorand.Reader, salt2); err != nil {
		return fmt.Errorf("generate password salt: %w", err)
	}
	verifier, augmentedSalt1, err := gotdsrp.NewSRP(cryptorand.Reader).NewHash(password, gotdsrp.Input{
		Salt1: salt1,
		Salt2: salt2,
		G:     tsrp.G,
		P:     tsrp.PBytes(),
	})
	if err != nil {
		return fmt.Errorf("generate SRP verifier: %w", err)
	}
	defer clear(verifier)
	if len(verifier) != tsrp.PadLen || !tsrp.ValidVerifier(verifier) {
		return errors.New("generate SRP verifier: invalid verifier")
	}

	quietLog := slog.New(slog.DiscardHandler)
	cfg, err := config.Load(quietLog)
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN, cfg.AuthKeyEncKey,
		store.WithLogger(quietLog),
		store.WithPoolConfig(cfg.PostgresPool),
		store.WithStatementTimeout(cfg.StatementTimeout),
		store.WithSessionIP(cfg.SessionIP),
		store.WithSessionDeviceLabels(cfg.SessionDeviceLabels),
		store.WithoutBlobStore(),
	)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close store: %w", closeErr))
		}
	}()

	user, err := st.CreateUsernameAccountWithPassword(ctx, handle, augmentedSalt1, salt2, verifier)
	if err != nil {
		return fmt.Errorf("create username account: %w", err)
	}
	if _, err := fmt.Fprintf(stderr, "User created: %s (user id: %d)\n", handle, user.ID); err != nil {
		return fmt.Errorf("write account creation confirmation: %w", err)
	}
	return nil
}

func parseAdminCreateUserArgs(args []string) (string, error) {
	if len(args) == 2 && args[0] == "--username" {
		return args[1], nil
	}
	if len(args) == 1 {
		if handle, ok := strings.CutPrefix(args[0], "--username="); ok {
			return handle, nil
		}
	}
	return "", errors.New("usage: teagramd admin create-user --username <handle>")
}

func readAdminPassword(stdin io.Reader) ([]byte, error) {
	if file, ok := stdin.(*os.File); ok {
		info, err := file.Stat()
		if err != nil {
			return nil, errors.New("read password input from stdin")
		}
		if info.Mode()&os.ModeCharDevice != 0 {
			return nil, errors.New("interactive password input is unsupported; provide protected stdin")
		}
	}
	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, errors.New("read password input from stdin")
	}
	if err == nil {
		if _, err := reader.ReadByte(); err == nil || !errors.Is(err, io.EOF) {
			return nil, errors.New("password input must contain exactly one line")
		}
	}
	line, hasLF := strings.CutSuffix(line, "\n")
	if hasLF {
		line = strings.TrimSuffix(line, "\r")
	}
	if line == "" {
		return nil, errors.New("password input must be nonempty")
	}
	return []byte(line), nil
}

func runMaintenanceCommand(args []string, log *slog.Logger, _ io.Writer, stderr io.Writer) (err error) {
	var channelSummaryID int64
	if len(args) == 0 {
		return maintenanceUsageError()
	}
	switch args[0] {
	case "assign-operator":
		if len(args) != 1 {
			return maintenanceUsageError()
		}
	case "initialize-channel-post-summaries":
		if len(args) != 3 {
			return maintenanceUsageError()
		}
		if args[1] != "--channel-id" {
			return maintenanceUsageError()
		}
		channelSummaryID, err = strconv.ParseInt(args[2], 10, 64)
		if err != nil || channelSummaryID <= 0 {
			return maintenanceUsageError()
		}
	default:
		return maintenanceUsageError()
	}

	cfg, err := config.Load(log)
	if err != nil {
		return err
	}

	// Maintenance commands deliberately do not validate TG_REGISTRATION or
	// initialize any of the server's listeners, keys, blob backends, or sweeps.
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN, cfg.AuthKeyEncKey,
		store.WithLogger(log),
		store.WithPoolConfig(cfg.PostgresPool),
		store.WithStatementTimeout(cfg.StatementTimeout),
		store.WithSessionIP(cfg.SessionIP),
		store.WithSessionDeviceLabels(cfg.SessionDeviceLabels),
		store.WithoutBlobStore(),
	)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close store: %w", closeErr))
		}
	}()

	if channelSummaryID > 0 {
		if err := st.InitializeChannelPostSummaries(ctx, channelSummaryID); err != nil {
			return fmt.Errorf("initialize channel post summaries: %w", err)
		}
		if _, err := fmt.Fprintf(stderr, "Channel post summaries initialized: %d\n", channelSummaryID); err != nil {
			return fmt.Errorf("write channel post summary confirmation: %w", err)
		}
		return nil
	}
	if err := st.AssignOperatorServerAdministrator(ctx); err != nil {
		return fmt.Errorf("assign operator server administrator: %w", err)
	}
	if _, err := fmt.Fprintln(stderr, "Operator server administrator assigned"); err != nil {
		return fmt.Errorf("write maintenance confirmation: %w", err)
	}
	return nil
}

func maintenanceUsageError() error {
	return errors.New("usage: teagramd maintenance assign-operator | initialize-channel-post-summaries --channel-id <positive-id>")
}

func writeClientConfigUsage(w io.Writer) error {
	if _, err := fmt.Fprintln(w, "usage: teagramd client-config"); err != nil {
		return fmt.Errorf("write client-config usage: %w", err)
	}
	return nil
}

func runClientConfigCommand(stdout io.Writer) error {
	cfg, err := config.LoadClientConfig()
	if err != nil {
		return err
	}
	key, err := rsakey.Load(cfg.RSAKeyPath)
	if err != nil {
		return err
	}
	endpoint := net.JoinHostPort(cfg.AdvertiseHost, strconv.Itoa(cfg.AdvertisePort))
	wipeGeneration := ""
	if cfg.AdvertiseDemoGuestSignUp {
		wipeGeneration, err = store.ReadDemoWipeGeneration(context.Background(), os.Getenv("TG_POSTGRES_DSN"))
		if err != nil {
			return fmt.Errorf("read demo wipe generation for client-config: %w", err)
		}
	}
	doc, err := discovery.NewDocument(endpoint, cfg.DCID, &key.PublicKey, cfg.AdvertiseDemoGuestSignUp, wipeGeneration)
	if err != nil {
		return err
	}
	if cfg.TurnstileEnabled && cfg.PublicWebSocketListenAddr == "" {
		doc.Turnstile = &discovery.Turnstile{SiteKey: cfg.TurnstileSiteKey}
	}
	return discovery.WriteDocument(stdout, doc)
}

func writeBootstrapIdentityUsage(w io.Writer) error {
	if _, err := fmt.Fprintln(w, "usage: teagramd bootstrap-identity"); err != nil {
		return fmt.Errorf("write bootstrap-identity usage: %w", err)
	}
	return nil
}

func runBootstrapIdentityCommand(stdout io.Writer) error {
	path := config.RSAKeyPath()
	key, err := rsakey.Bootstrap(path)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(stdout, "RSA identity bootstrapped: fingerprint=%d path=%s\n", rsakey.Fingerprint(&key.PublicKey), path); err != nil {
		return fmt.Errorf("write identity bootstrap confirmation: %w", err)
	}
	return nil
}

func runInviteCommand(args []string, log *slog.Logger, stdout, stderr io.Writer) (err error) {
	if len(args) == 0 {
		return inviteUsageError()
	}

	var (
		handle   string
		lifetime time.Duration
		id       int64
		userID   int64
	)
	switch args[0] {
	case "issue":
		handle, lifetime, err = parseInviteIssueArgs(args[1:])
		if err != nil {
			return err
		}
	case "list":
		if len(args) != 1 {
			return inviteUsageError()
		}
	case "revoke":
		if len(args) != 2 {
			return inviteUsageError()
		}
		id, err = strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return fmt.Errorf("invalid invite id %q: %w", args[1], err)
		}
	case "revoke-user", "invitees", "inviter":
		if len(args) != 2 {
			return inviteUsageError()
		}
		userID, err = strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return fmt.Errorf("invalid user id %q: %w", args[1], err)
		}
		if userID <= 0 {
			return fmt.Errorf("invalid user id %q", args[1])
		}
	default:
		return inviteUsageError()
	}

	cfg, err := config.Load(log)
	if err != nil {
		return err
	}

	// Invite commands deliberately do not validate TG_REGISTRATION or initialize
	// any of the server's listeners, keys, blob backends, or sweeps.
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN, cfg.AuthKeyEncKey,
		store.WithLogger(log),
		store.WithPoolConfig(cfg.PostgresPool),
		store.WithStatementTimeout(cfg.StatementTimeout),
		store.WithSessionIP(cfg.SessionIP),
		store.WithSessionDeviceLabels(cfg.SessionDeviceLabels),
		store.WithoutBlobStore(),
	)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close store: %w", closeErr))
		}
	}()

	switch args[0] {
	case "issue":
		var (
			invite store.RegistrationInvite
			secret string
		)
		if lifetime == 0 {
			invite, secret, err = st.IssueInvite(ctx, handle)
		} else {
			invite, secret, err = st.IssueInvite(ctx, handle, lifetime)
		}
		if err != nil {
			return fmt.Errorf("issue invite for %q: %w", strings.ToLower(handle), err)
		}
		if _, err := fmt.Fprintln(stdout, secret); err != nil {
			return fmt.Errorf("write invite secret: %w", err)
		}
		return writeInviteIssued(stderr, invite)
	case "list":
		invites, err := st.ListInvites(ctx)
		if err != nil {
			return err
		}
		return writeInviteList(stdout, invites)
	case "revoke":
		if err := st.RevokeInvite(ctx, id); err != nil {
			return fmt.Errorf("revoke invite %d: %w", id, err)
		}
		if _, err := fmt.Fprintf(stderr, "Revoked: id=%d\n", id); err != nil {
			return fmt.Errorf("write revoke confirmation: %w", err)
		}
		return nil
	case "revoke-user", "invitees", "inviter":
		if _, exists, err := st.UserByID(ctx, userID); err != nil {
			return err
		} else if !exists {
			return fmt.Errorf("unknown user %d", userID)
		}
		switch args[0] {
		case "revoke-user":
			count, err := st.RevokeUserInvites(ctx, userID)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(stderr, "Revoked: user_id=%d count=%d\n", userID, count); err != nil {
				return fmt.Errorf("write user invite revocation count: %w", err)
			}
			return nil
		case "invitees":
			invitees, err := st.ListInviteesForUser(ctx, userID)
			if err != nil {
				return err
			}
			return writeInvitees(stdout, invitees)
		case "inviter":
			inviterID, found, err := st.InviterForUser(ctx, userID)
			if err != nil {
				return err
			}
			return writeInviteAttribution(stdout, userID, inviterID, found)
		default:
			return inviteUsageError()
		}
	default:
		return inviteUsageError()
	}
}

func runBotCommand(args []string, log *slog.Logger, stdout, stderr io.Writer) (err error) {
	return runBotCommandWithEUID(args, log, stdout, stderr, os.Geteuid())
}

func runBotCommandWithEUID(args []string, log *slog.Logger, stdout, stderr io.Writer, euid int) (err error) {
	if len(args) == 0 {
		return botUsageError()
	}
	switch args[0] {
	case "create", "rotate", "revoke":
		if euid != 0 {
			return fmt.Errorf("bot %s requires root", args[0])
		}
	default:
		return botUsageError()
	}

	var (
		username  string
		firstName string
		botID     int64
	)
	switch args[0] {
	case "create":
		if len(args) != 3 {
			return botUsageError()
		}
		username, firstName = args[1], args[2]
	case "rotate", "revoke":
		if len(args) != 2 {
			return botUsageError()
		}
		botID, err = strconv.ParseInt(args[1], 10, 64)
		if err != nil || botID <= 0 {
			return errors.New("invalid bot id")
		}
	default:
		return botUsageError()
	}

	cfg, err := config.Load(log)
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.PostgresDSN, cfg.AuthKeyEncKey,
		store.WithLogger(log),
		store.WithPoolConfig(cfg.PostgresPool),
		store.WithStatementTimeout(cfg.StatementTimeout),
		store.WithSessionIP(cfg.SessionIP),
		store.WithSessionDeviceLabels(cfg.SessionDeviceLabels),
		store.WithoutBlobStore(),
	)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		if closeErr := st.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close store: %w", closeErr))
		}
	}()

	switch args[0] {
	case "create":
		_, token, err := st.CreateBot(ctx, username, firstName)
		if err != nil {
			return fmt.Errorf("create bot: %w", err)
		}
		if _, err := fmt.Fprintln(stdout, token); err != nil {
			return fmt.Errorf("write bot token: %w", err)
		}
		return nil
	case "rotate":
		token, err := st.RotateBotToken(ctx, botID)
		if err != nil {
			return fmt.Errorf("rotate bot token: %w", err)
		}
		if _, err := fmt.Fprintln(stdout, token); err != nil {
			return fmt.Errorf("write bot token: %w", err)
		}
		return nil
	case "revoke":
		if err := st.RevokeBotToken(ctx, botID); err != nil {
			return fmt.Errorf("revoke bot token: %w", err)
		}
		if _, err := fmt.Fprintf(stderr, "Revoked bot token: id=%d\n", botID); err != nil {
			return fmt.Errorf("write bot token revocation: %w", err)
		}
		return nil
	default:
		return botUsageError()
	}
}

func botUsageError() error {
	return errors.New("usage: teagramd bot create <username> <first_name> | teagramd bot rotate <bot_id> | teagramd bot revoke <bot_id>")
}

func parseInviteIssueArgs(args []string) (string, time.Duration, error) {
	if len(args) == 0 {
		return "", 0, inviteUsageError()
	}
	if len(args) == 1 {
		return args[0], 0, nil
	}
	if len(args) != 3 || args[1] != "--lifetime" {
		return "", 0, inviteUsageError()
	}
	handle := args[0]
	lifetime, err := time.ParseDuration(args[2])
	if err != nil {
		return "", 0, fmt.Errorf("invalid invite lifetime %q: %w", args[2], err)
	}
	return handle, lifetime, nil
}

func inviteUsageError() error {
	return errors.New("usage: teagramd invite issue <handle> [--lifetime <duration>] | teagramd invite list | teagramd invite revoke <id> | teagramd invite revoke-user <user-id> | teagramd invite invitees <user-id> | teagramd invite inviter <user-id>")
}

func writeInviteIssued(w io.Writer, invite store.RegistrationInvite) error {
	if _, err := fmt.Fprintf(w, "Invite issued: id=%d handle=%s expires=%s\n", invite.ID, invite.Handle, invite.ExpiresAt.Format(time.RFC3339)); err != nil {
		return fmt.Errorf("write invite metadata: %w", err)
	}
	return nil
}

func writeInviteList(w io.Writer, invites []store.RegistrationInvite) error {
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "ID\tHANDLE\tINVITER\tSTATE\tISSUED\tEXPIRES"); err != nil {
		return fmt.Errorf("write invite list: %w", err)
	}
	for _, invite := range invites {
		inviter := ""
		if invite.InviterUserID != nil {
			inviter = strconv.FormatInt(*invite.InviterUserID, 10)
		}
		if _, err := fmt.Fprintf(table, "%d\t%s\t%s\t%s\t%s\t%s\n",
			invite.ID,
			invite.Handle,
			inviter,
			invite.State,
			invite.IssuedAt.Format(time.RFC3339),
			invite.ExpiresAt.Format(time.RFC3339),
		); err != nil {
			return fmt.Errorf("write invite list: %w", err)
		}
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("write invite list: %w", err)
	}
	return nil
}

func writeInvitees(w io.Writer, invitees []store.RegistrationInvitee) error {
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "INVITEE_USER_ID\tREDEEMED_AT"); err != nil {
		return fmt.Errorf("write invitees: %w", err)
	}
	for _, invitee := range invitees {
		if _, err := fmt.Fprintf(table, "%d\t%s\n", invitee.UserID, invitee.RedeemedAt.Format(time.RFC3339)); err != nil {
			return fmt.Errorf("write invitees: %w", err)
		}
	}
	if err := table.Flush(); err != nil {
		return fmt.Errorf("write invitees: %w", err)
	}
	return nil
}

func writeInviteAttribution(w io.Writer, userID, inviterID int64, found bool) error {
	if !found {
		if _, err := fmt.Fprintf(w, "User %d was created without a user invite\n", userID); err != nil {
			return fmt.Errorf("write invite attribution: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintf(w, "User %d was invited by user %d\n", userID, inviterID); err != nil {
		return fmt.Errorf("write invite attribution: %w", err)
	}
	return nil
}

func run(log *slog.Logger) error {
	return runAtBlobModePath(log, blobModeDirectory)
}

func photoThumbSupervisorForStartup(
	log *slog.Logger,
	newSupervisor func() (*photothumb.Supervisor, error),
) *photothumb.Supervisor {
	supervisor, err := newSupervisor()
	if err != nil {
		log.Warn("photo thumbnail worker unavailable; photo uploads will continue without derivatives", "err", err)
		return nil
	}
	return supervisor
}

func runAtBlobModePath(log *slog.Logger, blobModePath string) error {
	if err := validateServeEnvironment(); err != nil {
		return err
	}
	cfg, err := config.LoadServerConfig(log)
	if err != nil {
		return err
	}
	stickerSets, err := api.LoadStickerSetCatalog(cfg.StickerBundle)
	if err != nil {
		return fmt.Errorf("load sticker catalogue: %w", err)
	}
	blobAuthority, err := blobmode.Read(blobModePath, blobmode.EffectiveConfig{BlobDir: cfg.BlobDir, BlobS3: cfg.BlobS3})
	if err != nil {
		return err
	}

	// Validate registration modes immediately after config load, before any
	// resource-intensive init (RSA/DB/blob). A bad value fails fast with a
	// message naming the variable.
	if err := cfg.ValidateRegistrationMode(); err != nil {
		return err
	}
	processIdentity, err := admin.NewProcessIdentity(cfg.ReplicaID)
	if err != nil {
		return fmt.Errorf("process identity: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	stopShutdownWatchdog := startProcessShutdownWatchdog(ctx, processShutdownTimeout)
	defer stopShutdownWatchdog()
	serviceCtx, cancelService := context.WithCancel(context.Background())
	defer cancelService()

	key, err := rsakey.Load(cfg.RSAKeyPath)
	if err != nil {
		return err
	}
	fingerprint := rsakey.Fingerprint(&key.PublicKey)
	if cfg.ExpectedRSAFingerprint != nil && fingerprint != *cfg.ExpectedRSAFingerprint {
		return errors.New("server RSA key fingerprint does not match TG_RSA_KEY_FINGERPRINT")
	}
	keyID, err := rsakey.KeyID(&key.PublicKey)
	if err != nil {
		return err
	}
	advertise := net.JoinHostPort(cfg.AdvertiseHost, strconv.Itoa(cfg.AdvertisePort))
	log.Info("server RSA key", "key_id", keyID, "fingerprint", fingerprint, "path", cfg.RSAKeyPath)

	blobs, err := newBlobStoreWithAuthority(ctx, cfg, log, blobAuthority)
	if err != nil {
		return err
	}

	st, err := store.Open(ctx, cfg.PostgresDSN, cfg.AuthKeyEncKey, store.WithLogger(log),
		store.WithAuthKeyUnbindLogging(cfg.LogAuthKeyUnbinds), store.WithBlobStore(blobs),
		store.WithPoolConfig(cfg.PostgresPool),
		store.WithStatementTimeout(cfg.StatementTimeout),
		store.WithSessionIP(cfg.SessionIP),
		store.WithSessionDeviceLabels(cfg.SessionDeviceLabels))
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			log.Error("store close", "err", cerr)
		}
	}()
	wipeGeneration := ""
	if cfg.AdvertiseDemoGuestSignUp {
		wipeGeneration, err = st.DemoWipeGeneration(ctx)
		if err != nil {
			return fmt.Errorf("read demo wipe generation: %w", err)
		}
	}
	clientDiscovery, publicClientDiscovery, err := listenerDiscoveryDocuments(cfg, &key.PublicKey, wipeGeneration)
	if err != nil {
		return err
	}
	hasStoredAuthKeys, err := st.ValidateAuthKeyEncryption(ctx)
	if err != nil {
		return fmt.Errorf("validate auth-key encryption identity: %w", err)
	}
	if !hasStoredAuthKeys {
		log.Info("auth-key encryption readiness: first-bootstrap database has no stored auth keys")
	}
	if err := st.ValidateChannelPostSummariesReady(ctx); err != nil {
		return fmt.Errorf("validate channel post summaries before startup: %w", err)
	}
	if err := st.RefreshCatalogSnapshot(ctx); err != nil {
		// Catalog data is optional for the built-in English behavior. A failed
		// refresh must not prevent the core server from starting.
		log.Warn("language catalog refresh failed", "err", err)
	}
	log.Info("file assembly concurrency bound",
		"limit", st.AssemblyConcurrencyLimit(),
		"reserved_pool_connections", st.AssemblyPoolHeadroom())

	// Run the sweep under a cancelable child context and wait for it to exit
	// before the store pool closes, so shutdown never closes the pool out from
	// under an in-flight sweep. This defer is registered after st.Close so it
	// runs first (LIFO): cancel the sweep, wait it out, then Close runs.
	sweepCtx, cancelSweep := context.WithCancel(serviceCtx)
	var sweepWG sync.WaitGroup
	sweepWG.Go(func() {
		sweepExpiredCodes(sweepCtx, st, log)
	})
	sweepWG.Go(func() {
		sweepExpiredAuthorizations(sweepCtx, st, log)
	})
	if cfg.AuthKeySweepIdleWindow > 0 && cfg.AuthKeySweepMaxAge > 0 {
		sweepWG.Go(func() {
			sweepNeverBoundAuthKeys(sweepCtx, st, cfg.AuthKeySweepIdleWindow, cfg.AuthKeySweepMaxAge, mtproto.DefaultPendingLoginLifetime, log)
		})
	}
	sweepWG.Go(func() {
		sweepExpiredUploadParts(sweepCtx, st, cfg.UploadPartTTL, log)
	})
	sweepWG.Go(func() {
		sweepExpiredChatReadReceipts(sweepCtx, st, log)
	})
	sweepWG.Go(func() {
		reclaimOrphanedPartBytes(sweepCtx, st, cfg.UploadPartTTL, log)
	})
	sweepWG.Go(func() {
		sweepExpiredRateLimits(sweepCtx, st, log)
	})
	sweepWG.Go(func() {
		sweepExpiredSendCodeIPLimits(sweepCtx, st, log)
	})
	sweepWG.Go(func() {
		sweepExpiredSignInFailLimits(sweepCtx, st, log)
	})
	sweepWG.Go(func() {
		sweepExpiredAdminSessions(sweepCtx, st, log)
	})
	sweepWG.Go(func() {
		sweepExpiredSRPChallenges(sweepCtx, st, log)
	})
	sweepWG.Go(func() {
		sweepExpiredHistoryMessages(sweepCtx, st, log)
	})
	sweepWG.Go(func() {
		sweepDeletedChannelPosts(sweepCtx, st, log)
	})
	if cfg.MediaErasureReportInterval > 0 {
		sweepWG.Go(func() {
			reportMediaErasureCandidates(sweepCtx, st, cfg.MediaErasureMinAge, cfg.BlobScanTempMinAge, cfg.MediaErasureReportInterval, log)
		})
	}
	if cfg.MediaErasureIntervalMin > 0 {
		sweepWG.Go(func() {
			sweepMediaErasure(sweepCtx, st, cfg, log)
		})
	}
	defer func() {
		cancelSweep()
		sweepWG.Wait()
	}()

	if cfg.BlobScanReportInterval > 0 {
		sweepWG.Go(func() {
			reportBlobDisk(sweepCtx, blobs, st, cfg.BlobScanTempMinAge, cfg.BlobScanReportInterval, log)
		})
	}

	// Derive the peer-hash subkey here, at process start, and hand only the
	// subkey to the RPC layer. cfg.AuthKeyEncKey itself must not travel past
	// store.Open: its reach today is storage, and this must not widen it.
	peerSubkey, err := peerhash.Subkey(cfg.AuthKeyEncKey)
	if err != nil {
		return err
	}
	peers, err := peerhash.New(peerSubkey)
	if err != nil {
		return err
	}

	// The profile-photo hash is its own capability domain and gets its own
	// subkey from the same master, derived at process start for the same reason:
	// only the subkey reaches the RPC layer, and the gallery lane holds nothing
	// but that.
	photoSubkey, err := photohash.Subkey(cfg.AuthKeyEncKey)
	if err != nil {
		return err
	}
	photos, err := photohash.New(photoSubkey)
	if err != nil {
		return err
	}
	photoThumbs := photoThumbSupervisorForStartup(log, photothumb.New)

	tgcfg := api.DefaultConfig(cfg.DCID, cfg.AdvertiseHost, cfg.AdvertisePort)
	tgcfg.MeURLPrefix = cfg.PublicLinkPrefix
	notifyMetrics := store.NewNotificationMetrics()
	dialogFilterSync := api.NewDialogFilterSync()
	var turnstileVerifier api.SignUpTurnstileVerifier
	if cfg.Turnstile.Enabled() {
		turnstileVerifier = api.NewTurnstileVerifierWithConfig(
			cfg.Turnstile.SecretKey, cfg.Turnstile.Hostnames, cfg.Turnstile.QABypassToken, log,
		)
	}
	newHandler := func(registrationMode config.RegistrationMode, turnstileEnabled, turnstileSignInEnabled bool) mtproto.Handler {
		turnstileSiteKey := ""
		var verifier api.SignUpTurnstileVerifier
		if turnstileEnabled && cfg.Turnstile.Enabled() {
			turnstileSiteKey = cfg.Turnstile.SiteKey
			verifier = turnstileVerifier
		}
		return api.NewWithPhotoThumbsAndDialogFilterSyncAndUploadPartTTLAndTurnstileAndWelcomeChannelAndSignInAndStickerCatalogAndUserInviteQuota(
			st, cfg.DCID, tgcfg, log, cfg.LogLoginCodes, cfg.MaxFileBytes, blobs,
			cfg.MaxUserStorageBytes, peers, photos, cfg.RateLimits, registrationMode,
			dialogFilterSync, photoThumbs, cfg.UploadPartTTL, turnstileSiteKey,
			verifier, cfg.WelcomeChannel, turnstileSignInEnabled, cfg.RecoveryEmailMode,
			cfg.SignUpTermsOfService, stickerSets, cfg.StickerImportMode, cfg.UserInviteQuota, cfg.UserInviteMinAccountAge, cfg.UploadsDisabled, notifyMetrics,
		)
	}
	if cfg.LogLoginCodes {
		log.Warn("TG_LOG_LOGIN_CODES is on: login codes are written to the log in cleartext")
	}
	cfg.WarnClientAddrTrust(log)

	server, publicHandler := newListenerScopedServer(cfg, exchange.PrivateKey{RSA: key}, st, log, newHandler)
	server.SetWebSocketOriginPatterns(cfg.WebSocketOriginPatterns)
	if err := server.SetClientDiscoveryDocument(clientDiscovery); err != nil {
		return fmt.Errorf("set client discovery document: %w", err)
	}
	if err := trustClientAddr(server, cfg, log); err != nil {
		return err
	}
	if err := server.SetPreAuthLimits(cfg.PreAuth); err != nil {
		return err
	}
	if err := server.SetExchangeLimits(cfg.KeyExchangeLimits); err != nil {
		return err
	}
	if err := server.SetDiscoveryLimits(cfg.DiscoveryLimits); err != nil {
		return err
	}
	if err := server.SetMaxConnsPerUnboundKey(cfg.MaxConnsPerUnboundKey); err != nil {
		return err
	}
	if err := server.SetMaxPendingLoginConns(cfg.MaxPendingLoginConns); err != nil {
		return err
	}
	if err := server.SetReplicaCount(cfg.ReplicaCount); err != nil {
		return err
	}
	if err := server.SetRPCDeadline(cfg.RPCDeadline); err != nil {
		return err
	}
	cfg.WarnPreAuthLifetime(log)

	// Connection lifecycle callback: when a session binds or closes, record the
	// status change and notify other replicas.
	server.OnStatusChange(func(ctx context.Context, userID int64, online bool) {
		if err := st.SetUserStatus(ctx, userID, online); err != nil {
			log.Error("set user status", "user_id", userID, "online", online, "err", err)
			return
		}
		if err := st.Notify(ctx, store.ChannelStatus, store.StatusPayload(userID, online)); err != nil {
			log.Error("notify status", "user_id", userID, "err", err)
		}
	})

	// Cross-replica real-time delivery: the listener wakes on NOTIFY and pushes
	// each user's pending updates to their live conns in this process. Drained
	// before the store pool closes (defer registered after st.Close, runs first).
	updater := api.NewUpdaterWithDialogFilterSyncAndStickerCatalog(st, cfg.DCID, server.Registry(), log, peers, dialogFilterSync, stickerSets, notifyMetrics)
	stopDialogFilterRecovery := updater.StartDialogFilterRecovery(serviceCtx)
	defer stopDialogFilterRecovery()
	_, stopListener, err := store.StartListenerWithDialogPinsAndStickerSets(serviceCtx, cfg.PostgresDSN, updater.Deliver, updater.DeliverTyping, updater.Evict, updater.DeliverChannelPost, updater.DeliverEncryption, updater.DeliverStatus, updater.DeliverEncryptedMsg, updater.DeliverReactions, updater.DeliverPinned, updater.MarkDialogFilters, updater.DeliverDialogPins, updater.DeliverStickerSets, updater.DialogFilterListenerReconnected, log, notifyMetrics)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := stopListener(); cerr != nil {
			log.Error("listener stop", "err", cerr)
		}
	}()
	fleetPublisher := fleet.NewPublisher(
		st, server.Registry(), processIdentity.Generation, processIdentity.ReplicaID, fleet.BuildVersion(),
	)
	stopFleetPublisher := fleetPublisher.Start(ctx)
	defer func() {
		if err := stopFleetPublisher(); err != nil {
			log.Error("fleet telemetry shutdown", "err", err)
		}
	}()

	// Start the admin HTTP server on a separate listener with login, logout,
	// CSRF protection, rate limiting, and security headers.
	if cfg.AdminListenAddr != "" {
		// One shared sampler feeds every dashboard stream; it idles while
		// nobody is connected. Delivery lag state is shared with JSON and the
		// dashboard so all authenticated surfaces retain the same complete
		// sample after a partial attempt.
		deliveryLag := admin.NewDeliveryLagSampler()
		metricsCache := admin.NewMetricsSnapshotCache(server.Registry(), st, processIdentity, deliveryLag, notifyMetrics)
		events := admin.NewBroadcaster(admin.BroadcasterConfig{
			Store:  st,
			Sample: metricsCache.Snapshot,
			Logger: log,
			Render: admin.DashboardFragmentRenderer,
		})
		var eventsWG sync.WaitGroup
		eventsCtx, stopEvents := context.WithCancel(serviceCtx)
		defer stopEvents()
		eventsWG.Go(func() { events.Run(eventsCtx) })

		adminRouter := admin.AdminRouter(admin.LoginHandlerConfig{
			Store:         st,
			TokenHash:     cfg.AdminTokenHash,
			Logger:        log,
			AdminOrigin:   cfg.AdminOrigin,
			Events:        events,
			NotifyMetrics: notifyMetrics,
			DeliveryLag:   deliveryLag,
			Metrics:       metricsCache,
		}, server.Registry())
		adminSrv := &http.Server{
			Addr:              cfg.AdminListenAddr,
			Handler:           adminRouter,
			ReadHeaderTimeout: 5 * time.Second,
			MaxHeaderBytes:    8192,
		}
		var adminLC net.ListenConfig
		adminLn, err := adminLC.Listen(ctx, "tcp", cfg.AdminListenAddr)
		if err != nil {
			return fmt.Errorf("admin listen: %w", err)
		}
		log.Info("admin server listening", "addr", cfg.AdminListenAddr)
		go func() {
			if err := adminSrv.Serve(adminLn); err != nil && err != http.ErrServerClosed {
				log.Error("admin server", "err", err)
			}
		}()
		defer func() {
			// Stop the broadcaster first: it closes every open SSE stream, and
			// Shutdown waits on in-flight requests, which a live stream is.
			stopEvents()
			eventsWG.Wait()

			shutdownCtx, cancel := context.WithTimeout(context.Background(), adminShutdownTimeout)
			defer cancel()
			if err := adminSrv.Shutdown(shutdownCtx); err != nil {
				log.Error("admin server shutdown", "err", err)
			}
		}()
	}

	if cfg.BotAPIListenAddr != "" {
		botAPIServer := botapi.NewServer(cfg.BotAPIListenAddr, st, log, cfg.RateLimits.MessageSend)
		var botAPILC net.ListenConfig
		botAPILn, err := botAPILC.Listen(ctx, "tcp", cfg.BotAPIListenAddr)
		if err != nil {
			return fmt.Errorf("bot API listen: %w", err)
		}
		botAPILn = botapi.LimitConnectionsPerIP(botAPILn, botapi.DefaultMaxConnectionsPerIP)
		log.Info("Bot API server listening", "addr", cfg.BotAPIListenAddr)
		go func() {
			if err := botAPIServer.Serve(botAPILn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("Bot API server", "err", err)
			}
		}()
		defer func() {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), botAPIShutdownTimeout)
			defer cancel()
			if err := botAPIServer.Shutdown(shutdownCtx); err != nil {
				log.Error("Bot API server shutdown", "err", err)
			}
		}()
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	log.Info("listening", "addr", cfg.ListenAddr, "advertise", advertise, "dc", cfg.DCID)
	var websocketLn net.Listener
	if cfg.WebSocketListenAddr != "" {
		websocketLn, err = lc.Listen(ctx, "tcp", cfg.WebSocketListenAddr)
		if err != nil {
			return errors.Join(fmt.Errorf("websocket listen: %w", err), closeListener(ln))
		}
		log.Info("WebSocket MTProto listening", "addr", cfg.WebSocketListenAddr)
	}

	var publicWebSocket *publicWebSocketListener
	if cfg.PublicWebSocketListenAddr != "" {
		publicLn, listenErr := lc.Listen(ctx, "tcp", cfg.PublicWebSocketListenAddr)
		if listenErr != nil {
			closeErr := errors.Join(closeListener(websocketLn), closeListener(ln))
			return errors.Join(fmt.Errorf("public websocket listen: %w", listenErr), closeErr)
		}
		log.Info("Public WebSocket MTProto listening", "addr", cfg.PublicWebSocketListenAddr)
		publicWebSocket = newPublicWebSocketListener(cfg, publicLn, publicHandler, publicClientDiscovery)
	}

	return serveMTProto(ctx, server, ln, websocketLn, publicWebSocket)
}

func listenerDiscoveryDocuments(cfg config.Config, pub *rsa.PublicKey, wipeGeneration string) (discovery.Document, discovery.Document, error) {
	advertise := net.JoinHostPort(cfg.AdvertiseHost, strconv.Itoa(cfg.AdvertisePort))
	clientDiscovery, err := discovery.NewDocument(advertise, cfg.DCID, pub, cfg.AdvertiseDemoGuestSignUp, wipeGeneration)
	if err != nil {
		return discovery.Document{}, discovery.Document{}, fmt.Errorf("validate discovery identity: %w", err)
	}
	if cfg.PublicWebSocketListenAddr == "" {
		if cfg.Turnstile.Enabled() {
			clientDiscovery.Turnstile = &discovery.Turnstile{SiteKey: cfg.Turnstile.SiteKey}
		}
		return clientDiscovery, clientDiscovery, nil
	}

	publicClientDiscovery := clientDiscovery
	if cfg.PublicWebSocketAdvertiseHost != "" {
		publicAdvertise := net.JoinHostPort(cfg.PublicWebSocketAdvertiseHost, strconv.Itoa(cfg.PublicWebSocketAdvertisePort))
		publicClientDiscovery, err = discovery.NewDocumentWithWebSocketAdvertiseAddr(
			advertise, publicAdvertise, cfg.DCID, pub, cfg.AdvertiseDemoGuestSignUp, wipeGeneration,
		)
		if err != nil {
			return discovery.Document{}, discovery.Document{}, fmt.Errorf("validate public WebSocket discovery identity: %w", err)
		}
	}
	if cfg.Turnstile.Enabled() {
		publicClientDiscovery.Turnstile = &discovery.Turnstile{SiteKey: cfg.Turnstile.SiteKey}
	}
	return clientDiscovery, publicClientDiscovery, nil
}

func closeListener(listener net.Listener) error {
	if listener == nil {
		return nil
	}
	if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

func newBlobStore(ctx context.Context, cfg config.Config, log *slog.Logger) (blob.Store, error) {
	return newBlobStoreWithAuthority(ctx, cfg, log, blobmode.Authority{})
}

func newBlobStoreWithAuthority(ctx context.Context, cfg config.Config, log *slog.Logger, authority blobmode.Authority) (blob.Store, error) {
	if cfg.BlobS3 == nil {
		return blob.NewLocal(cfg.BlobDir)
	}
	acceptedPlaintext := authority.Present && authority.Outcome == blobmode.OutcomeS3Accepted
	encryptedMigration := authority.Present && authority.Outcome == blobmode.OutcomeEncrypted
	if authority.Present && !acceptedPlaintext && !encryptedMigration {
		return nil, errors.New("blob-mode authority is not valid for the configured S3 backend")
	}
	if acceptedPlaintext {
		log.Warn("S3 media encryption remains disabled for accepted plaintext cutover", "mode", authority.Outcome)
	} else if cfg.MediaKeys == nil {
		return nil, errors.New("S3 media encryption requires TG_MEDIA_KEY_DIR and TG_MEDIA_KEY_CURRENT")
	}
	s3cfg := *cfg.BlobS3
	// NewS3 emits the plaintext warning while it constructs the client, so
	// the logger must be selected before construction rather than after it.
	s3cfg.Logger = log
	if encryptedMigration {
		return blob.NewMigratedS3Store(ctx, s3cfg, cfg.MediaKeys)
	}
	blobs, _, err := blob.NewS3Store(ctx, s3cfg, cfg.MediaKeys, !acceptedPlaintext)
	if err != nil {
		return nil, err
	}
	return blobs, nil
}

// trustClientAddr applies the configured client-address source to the server.
//
// The switch is exhaustive on purpose and has no permissive default: config
// validation already rejects any other value, and a fallback to socket keying
// added here would be the silent collapse into one global bucket that the mode
// exists to prevent.
func trustClientAddr(server *mtproto.Server, cfg config.Config, log *slog.Logger) error {
	switch cfg.ClientAddrTrust {
	case config.ClientAddrSocket:
		return nil
	case config.ClientAddrProxyV2:
		log.Info("client addresses are read from PROXY protocol v2 headers",
			"trust", string(cfg.ClientAddrTrust),
			"balancers", cfg.ClientAddrProxies)
		server.TrustProxyV2Headers(cfg.ClientAddrProxies)
		return nil
	default:
		return fmt.Errorf("unsupported client address trust mode %q", cfg.ClientAddrTrust)
	}
}

// sweepExpiredCodes periodically deletes expired login codes until ctx is
// canceled.
//
// ponytail: naive full-table DELETE on a plain ticker. Fine at login-code
// volumes; if phone_codes gets hot, add an index on expires_at, partition by
// time, or move the sweep to an external cron/job scheduler.
func sweepExpiredCodes(ctx context.Context, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := st.DeleteExpiredCodes(ctx)
			if err != nil {
				log.Error("sweep expired codes", "err", err)
				continue
			}
			log.Info("swept expired codes", "deleted", n)
		}
	}
}

// sweepExpiredAuthorizations periodically revokes sessions whose last
// authenticated activity is older than their account's configured TTL.
func sweepExpiredAuthorizations(ctx context.Context, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			revoked, err := st.SweepExpiredAuthorizations(ctx, store.ExpiredAuthorizationSweepBatch)
			if err != nil {
				log.Error("sweep expired authorizations", "deleted", len(revoked), "err", err)
				continue
			}
			log.Info("swept expired authorizations", "deleted", len(revoked))
		}
	}
}

// sweepNeverBoundAuthKeys periodically removes only keys that have never been
// bound to an account and have no live pending login.
func sweepNeverBoundAuthKeys(ctx context.Context, st *store.Store, idleWindow, maxAge, pendingLifetime time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deleted, err := st.SweepNeverBoundAuthKeys(ctx, idleWindow, maxAge, pendingLifetime, store.NeverBoundAuthKeySweepBatch)
			if err != nil {
				log.Error("sweep never-bound auth keys", "deleted", deleted, "err", err)
				continue
			}
			log.Info("swept never-bound auth keys", "deleted", deleted)
		}
	}
}

// sweepExpiredUploadParts periodically deletes unassembled upload parts older
// than ttl until ctx is canceled.
//
// The interval is derived from the TTL rather than being a constant: this sweep
// is what bounds worst-case retained bytes, and an interval no greater than a
// quarter of the TTL keeps the overshoot past expiry small relative to the
// window itself.
func sweepExpiredUploadParts(ctx context.Context, st *store.Store, ttl time.Duration, log *slog.Logger) {
	// Floored at a second: NewTicker panics on a non-positive interval, and the
	// config only rejects a non-positive TTL, so a tiny-but-valid TTL would
	// otherwise crash the server at boot rather than sweep aggressively.
	ticker := time.NewTicker(max(ttl/4, time.Second))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// One sweep is as many bounded passes as it takes: the batch bounds
			// each statement, not how much a tick may retire, so a backlog left
			// by a long outage clears here rather than one batch per tick.
			n, err := st.SweepExpiredUploadParts(ctx, time.Now().Add(-ttl), store.ExpiredPartSweepBatch)
			if err != nil {
				log.Error("sweep expired upload parts", "deleted", n, "err", err)
				continue
			}
			log.Info("swept expired upload parts", "deleted", n)
		}
	}
}

// sweepExpiredChatReadReceipts periodically removes only expired derived
// receipt rows. Store performs each delete in bounded batches and drains the
// expired backlog before the next tick.
func sweepExpiredChatReadReceipts(ctx context.Context, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := st.SweepExpiredChatReadReceipts(ctx)
			if err != nil {
				log.Error("sweep expired chat read receipts", "deleted", n, "err", err)
				continue
			}
			log.Info("swept expired chat read receipts", "deleted", n)
		}
	}
}

// reclaimOrphanedPartBytes periodically walks the parts prefix and reclaims
// objects no row names, older than the part TTL plus a margin. It is the
// mechanism that bounds the MAIN-341 crash window: a row committed and its
// bytes lost, or a best-effort cleanup that dropped rows and failed on
// objects. Without it those bytes are permanent, uncapped, and on a
// deployment with no backup.
//
// The interval matches the row-driven sweep's: the two passes complement
// each other (rows vs objects) and share the same cadence, so an operator
// tuning one tunes both.
func reclaimOrphanedPartBytes(ctx context.Context, st *store.Store, ttl time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(max(ttl/4, time.Second))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			margin := store.PartOrphanMargin(ttl)
			cutoff := time.Now().Add(-(ttl + margin))
			res, err := st.ReclaimOrphanedPartBytes(ctx, cutoff, ttl)
			if err != nil {
				log.Error("reclaim orphaned part bytes", "objects", res.Objects, "bytes", res.Bytes, "err", err)
				continue
			}
			if res.Objects > 0 {
				log.Info("reclaimed orphaned part bytes", "objects", res.Objects, "bytes", res.Bytes)
			}
		}
	}
}

// reportMediaErasureCandidates periodically counts what a media erase could
// reclaim and what is holding the rest back, until ctx is canceled.
//
// It is named a report rather than a sweep because it removes nothing: no row,
// no blob, no quota. The eraser is a later stage of M17 and lands with its own
// human decision to enable destruction; this pass exists so an operator can see
// the size of the reclaim before anything is allowed to perform it, and it is
// safe to leave running indefinitely.
//
// It logs aggregates and never a file's access hash. That value is the
// unguessable half of a download credential, so it must not reach log
// aggregation for every file a report names — which is why store's candidate
// type does not carry it at all rather than this line choosing not to print it.
//
// Off unless an operator sets an interval, and that default is why: the
// reference predicate is a SubPlan the planner cannot lift into a semi-join,
// and past roughly 300k media messages it stops being hashable and runs once
// per files row. See MediaErasureReportInterval for the measurement. An index
// on messages (file_id) is what makes it cheap, and that belongs with the
// ticket that decides how often a reclaim runs — it buys a write on every send,
// which is not a cost a report should be quietly incurring.
func reportMediaErasureCandidates(ctx context.Context, st *store.Store, minAge, tempMinAge, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// The cutoff is read per pass, not per batch: what a report holds
			// back is decided once, at its start, the same way a sweep's is.
			now := time.Now()
			olderThan := now.Add(-minAge)
			tempOlderThan := now.Add(-tempMinAge)
			c, err := st.MediaErasureSummary(ctx, olderThan, store.ErasureScanBatch)
			if err != nil {
				log.Error("media erasure report", "err", err)
			}
			b, blobErr := st.BlobErasureSummary(ctx, tempOlderThan)
			if blobErr != nil {
				log.Error("blob erasure report", "err", blobErr)
			}
			if err != nil || blobErr != nil {
				continue
			}
			log.Info("media erasure candidates",
				"scanned", c.Scanned,
				"unreferenced", c.Unreferenced,
				"unreferenced_bytes", c.UnreferencedBytes,
				"unassembled", c.Unassembled,
				"unassembled_bytes", c.UnassembledBytes,
				"skipped_message_ref", c.SkippedMessageRef,
				"skipped_channel_ref", c.SkippedChannelRef,
				"skipped_gallery_ref", c.SkippedGalleryRef,
				"skipped_chat_ref", c.SkippedChatRef,
				"skipped_too_new", c.SkippedTooNew,
				"blob_orphans", b.Orphans,
				"blob_orphan_bytes", b.OrphanBytes,
				"abandoned_temp", b.AbandonedTemps,
				"abandoned_temp_bytes", b.AbandonedTempBytes,
				"blob_accounted", b.Accounted,
				"blob_above_snapshot", b.AboveSnapshot,
				"blob_temp_in_flight", b.TempsInFlight,
				"blob_unexplained", b.Unexplained,
				"blob_unexplained_bytes", b.UnexplainedBytes)
			for _, p := range b.UnexplainedPaths {
				log.Warn("path under the assembled blob tree the layout does not explain", "path", p)
			}
		}
	}
}

// reportBlobDisk periodically classifies what is on the blob store against what
// the database accounts for, until ctx is canceled.
//
// A report, not a sweep: it removes nothing, and the pass it runs takes no lock
// and opens no transaction, so it is safe to leave running on a live server.
// The unaccounted bytes it finds are the completion mechanism a later stage
// needs — an eraser deletes a files row and unlinks afterwards, so a crash
// between the two leaves bytes nothing else will ever name — and an operator
// wants to see the size of that before anything is enabled to act on it.
//
// Paths the blob layout does not explain are logged one by one, at warn, and
// deliberately not counted alongside the reclaimable classes. Something is
// under the blob root that this server did not put there; that is a question
// for a person, and a pass that guessed at it would be how an unrelated file
// gets destroyed. Upload parts, the layout's other keyspace, are reported as
// their own class and never as unexplained.
func reportBlobDisk(ctx context.Context, blobs blob.Store, st *store.Store, tempMinAge, interval time.Duration, log *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// The cutoff is read per pass, the same way a sweep's is: what a
			// report holds back is decided once, at its start.
			rep, err := blobscan.ScanStore(ctx, blobs, st, time.Now().Add(-tempMinAge))
			if err != nil {
				log.Error("blob disk report", "err", err)
				continue
			}
			log.Info("blob disk classification",
				"through_file_id", rep.Through,
				"walked", rep.Walked,
				"orphans", rep.Orphans.Count,
				"orphan_bytes", rep.Orphans.Bytes,
				"abandoned_temp", rep.Temps.Count,
				"abandoned_temp_bytes", rep.Temps.Bytes,
				"upload_parts", rep.Parts.Count,
				"upload_part_bytes", rep.Parts.Bytes,
				"unexplained", rep.Unexplained.Count,
				"unexplained_bytes", rep.Unexplained.Bytes,
				"accounted", rep.Accounted,
				"above_snapshot", rep.AboveSnapshot,
				"temp_in_flight", rep.TempsInFlight)
			for _, p := range rep.Unexplained.Paths {
				log.Warn("path under the blob root the layout does not explain", "path", p.Key, "bytes", p.Size)
			}
		}
	}
}

// sweepMediaErasure periodically reclaims the media nothing live references,
// until ctx is canceled. It is the one pass in this process that destroys user
// data, and the blob volume it unlinks from has no backup and no restore path.
//
// Two things keep that from being a default. The pass does not run at all
// unless an operator configures an interval, and even then it removes nothing
// unless they separately set TG_MEDIA_ERASURE_DESTRUCTIVE: the reporting branch
// runs the same read-only summary the candidate report runs and logs what a
// destructive pass would have freed. Enabling destruction on a real deployment
// is a decision made against that report, not a flag flipped at install.
//
// The interval is drawn fresh from the configured range before every wait,
// never a ticker. A fixed period is what makes another account's deletion
// observable to the uploader at a known time: usage is summed off the files
// table, so an uploader sitting at their quota learns that a recipient deleted
// their copy by retrying an upload, and on a fixed tick they learn when to
// within a second. The draw does not remove that channel — nothing can, short
// of not freeing quota — it blunts it to "eventually".
//
// It logs aggregates and never a file's access hash, which is the unguessable
// half of a download credential; the store's counts carry no file id and no
// hash at all, so there is nothing here to choose not to print.
func sweepMediaErasure(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) {
	for {
		// The wait comes first, so a restart loop cannot turn into a burst of
		// passes and so the first pass after a deploy is not synchronised
		// across replicas.
		wait := cfg.MediaErasureIntervalMin +
			rand.N(cfg.MediaErasureIntervalMax-cfg.MediaErasureIntervalMin) //nolint:gosec // scheduling jitter, not a secret
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		sweepMediaErasurePass(ctx, st, cfg, log)
	}
}

// sweepMediaErasurePass runs one reporting or destructive pass. Keeping the
// deployment decision at this command boundary makes it testable without
// waiting on the background interval and keeps the off-switch ahead of every
// operation that can unlink bytes.
func sweepMediaErasurePass(ctx context.Context, st *store.Store, cfg config.Config, log *slog.Logger) {
	// The cutoff is read per pass, not per batch: what one sweep reclaims is
	// decided once at its start, the same rule the upload-part sweep keeps.
	now := time.Now()
	olderThan := now.Add(-cfg.MediaErasureMinAge)
	tempOlderThan := now.Add(-cfg.BlobScanTempMinAge)
	if !cfg.MediaErasureDestructive {
		c, err := st.MediaErasureSummary(ctx, olderThan, store.ErasureScanBatch)
		if err != nil {
			log.Error("media erasure sweep report", "err", err)
		}
		b, blobErr := st.BlobErasureSummary(ctx, tempOlderThan)
		if blobErr != nil {
			log.Error("blob erasure sweep report", "err", blobErr)
		}
		if err != nil || blobErr != nil {
			return
		}
		log.Info("media erasure sweep (reporting; destruction disabled)",
			"scanned", c.Scanned,
			"would_erase", c.Unreferenced,
			"would_free_bytes", c.UnreferencedBytes,
			"would_erase_unassembled", c.Unassembled,
			"would_free_unassembled_bytes", c.UnassembledBytes,
			"skipped_message_ref", c.SkippedMessageRef,
			"skipped_channel_ref", c.SkippedChannelRef,
			"skipped_gallery_ref", c.SkippedGalleryRef,
			"skipped_chat_ref", c.SkippedChatRef,
			"skipped_too_new", c.SkippedTooNew,
			"would_erase_blob_orphans", b.Orphans,
			"would_free_blob_orphan_bytes", b.OrphanBytes,
			"would_erase_abandoned_temp", b.AbandonedTemps,
			"would_free_abandoned_temp_bytes", b.AbandonedTempBytes,
			"blob_accounted", b.Accounted,
			"blob_above_snapshot", b.AboveSnapshot,
			"blob_temp_in_flight", b.TempsInFlight,
			"blob_unexplained", b.Unexplained,
			"blob_unexplained_bytes", b.UnexplainedBytes)
		for _, p := range b.UnexplainedPaths {
			log.Warn("path under the assembled blob tree the layout does not explain", "path", p)
		}
		return
	}

	c, err := st.SweepMediaErasure(ctx, olderThan, store.ErasureScanBatch)
	// The blob sweep uses tempOlderThan only for temporary mtime; its row
	// deletion receives the media cutoff independently.
	b, blobErr := st.SweepBlobErasure(ctx, tempOlderThan, olderThan)
	// Logged before the error is handled rather than after: a sweep that
	// failed part way through has still committed everything it erased, and
	// the operator-facing question after a failure is how much went.
	log.Info("media erasure sweep",
		"considered", c.Considered,
		"erased", c.Erased,
		"erased_bytes", c.ErasedBytes,
		"contended", c.Contended,
		"retained", c.Retained,
		"unlink_failed", c.UnlinkFailed,
		"unassembled_considered", c.UnassembledConsidered,
		"unassembled_erased", c.UnassembledErased,
		"unassembled_erased_bytes", c.UnassembledErasedBytes,
		"unassembled_contended", c.UnassembledContended,
		"unassembled_retained", c.UnassembledRetained,
		"unassembled_unlink_failed", c.UnassembledUnlinkFailed,
		"blob_orphan_considered", b.OrphanConsidered,
		"blob_orphan_unlink_attempts", b.OrphanUnlinkAttempts,
		"blob_orphan_unlink_attempted_bytes", b.OrphanUnlinkAttemptedBytes,
		"blob_orphan_retained", b.OrphanRetained,
		"blob_orphan_unlink_failed", b.OrphanUnlinkFailed,
		"temp_considered", b.TempConsidered,
		"temp_unlink_attempts", b.TempUnlinkAttempts,
		"temp_unlink_attempted_bytes", b.TempUnlinkAttemptedBytes,
		"temp_in_flight", b.TempInFlight,
		"temp_contended", b.TempContended,
		"temp_retained", b.TempRetained,
		"temp_unlink_failed", b.TempUnlinkFailed,
		"above_snapshot", b.AboveSnapshot,
		"unexplained", b.Unexplained,
		"unexplained_bytes", b.UnexplainedBytes)
	if err != nil {
		log.Error("media erasure sweep", "err", err)
	}
	if blobErr != nil {
		log.Error("blob erasure sweep", "err", blobErr)
	}
	for _, p := range b.UnexplainedPaths {
		log.Warn("path under the assembled blob tree the layout does not explain", "path", p)
	}
}

// sweepExpiredRateLimits periodically deletes expired rate-limit counters and
// concurrent-limit leases. Rate-limit rows are only created by the limiter,
// not on every request; leases are reclaimed here after their owners disappear.
func sweepExpiredRateLimits(ctx context.Context, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := st.SweepExpiredRateLimits(ctx)
			if err != nil {
				log.Error("sweep expired server limits", "err", err)
				continue
			}
			log.Info("swept expired server limits", "deleted", n)
		}
	}
}

// sweepExpiredSendCodeIPLimits periodically deletes per-IP sendCode rows past
// their deadline. A network that keeps calling prunes its own rows on write;
// this is what clears the ones that go quiet, and it is what holds retention of
// the network-to-number rows to the limit window rather than forever.
func sweepExpiredSendCodeIPLimits(ctx context.Context, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := st.SweepExpiredSendCodeIPLimits(ctx)
			if err != nil {
				log.Error("sweep expired send code ip limits", "err", err)
				continue
			}
			log.Info("swept expired send code ip limits", "deleted", n)
		}
	}
}

// sweepExpiredSignInFailLimits periodically deletes per-IP signIn-failure rows
// past their deadline. Unlike sendCode IP limits there is no prune-on-write
// (signIn is not idempotent), so the sweep is the sole cleanup path.
func sweepExpiredSignInFailLimits(ctx context.Context, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := st.SweepExpiredSignInFailCalls(ctx)
			if err != nil {
				log.Error("sweep expired sign in fail limits", "err", err)
				continue
			}
			log.Info("swept expired sign in fail limits", "deleted", n)
		}
	}
}

// sweepExpiredAdminSessions periodically deletes admin session rows whose
// absolute expiry deadline has passed. This is what bounds the admin_sessions
// table: sessions are only created by the login handler and only deleted by
// this sweep.
func sweepExpiredAdminSessions(ctx context.Context, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := st.SweepExpiredAdminSessions(ctx)
			if err != nil {
				log.Error("sweep expired admin sessions", "err", err)
				continue
			}
			log.Info("swept expired admin sessions", "deleted", n)
		}
	}
}

// sweepExpiredSRPChallenges periodically deletes expired two-factor login
// challenges so abandoned logins do not retain encrypted secrets indefinitely.
func sweepExpiredSRPChallenges(ctx context.Context, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := st.SweepExpiredSRPChallenges(ctx)
			if err != nil {
				log.Error("sweep expired SRP challenges", "err", err)
				continue
			}
			log.Info("swept expired SRP challenges", "deleted", n)
		}
	}
}

// sweepExpiredHistoryMessages deletes stamped message copies and channel posts
// in bounded transactions. The row stamps are the resume point, so a process
// restart simply discovers any remaining expired rows on its next interval.
func sweepExpiredHistoryMessages(ctx context.Context, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var copies, channelPosts int
			for {
				deleted, more, err := st.SweepExpiredMessageCopies(ctx, store.HistoryTTLSweepBatchSize)
				if err != nil {
					if ctx.Err() == nil {
						log.Error("sweep expired message copies", "deleted", copies, "err", err)
					}
					break
				}
				copies += deleted
				if !more {
					break
				}
			}
			for {
				deleted, more, err := st.SweepExpiredChannelPosts(ctx, store.HistoryTTLSweepBatchSize)
				if err != nil {
					if ctx.Err() == nil {
						log.Error("sweep expired channel posts", "deleted", channelPosts, "err", err)
					}
					break
				}
				channelPosts += deleted
				if !more {
					break
				}
			}
			if ctx.Err() != nil {
				return
			}
			if copies > 0 || channelPosts > 0 {
				log.Info("swept expired history", "message_copies", copies, "channel_posts", channelPosts)
			}
		}
	}
}

// sweepDeletedChannelPosts drains resumable batches of already-inaccessible
// channel posts. Each batch commits its cursor, so shutdown or a failed
// transaction resumes safely on the next process start.
func sweepDeletedChannelPosts(ctx context.Context, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			var total int
			for {
				n, worked, err := st.SweepDeletedChannelPosts(ctx)
				if err != nil {
					if ctx.Err() == nil {
						log.Error("sweep deleted channel posts", "tombstoned", total, "err", err)
					}
					break
				}
				total += n
				if !worked {
					if total > 0 {
						log.Info("swept deleted channel posts", "tombstoned", total)
					}
					break
				}
			}
		}
	}
}
