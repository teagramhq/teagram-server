package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const (
	defaultEndpointHost      = "teagramd"
	defaultEndpoint          = defaultEndpointHost + ":2443"
	candidateEndpointHost    = "teagramd-next"
	candidateEndpoint        = candidateEndpointHost + ":2443"
	defaultClientConfig      = "/run/teagram-demo/client.json"
	defaultOwnerPasswordFile = "/run/secrets/seed-owner-password"
	defaultAccountInventory  = "/run/secrets/demo-accounts/inventory.json"
	defaultActivityInventory = "/run/secrets/demo-activity-inventory.json"
	trustedProxyHost         = "haproxy"
	trustedProxyEndpoint     = trustedProxyHost + ":2443"
	appID                    = 1
	appHash                  = "hash"
)

type clientDocument struct {
	Version int `json:"version"`
	MTProto struct {
		DCID    int    `json:"dc_id"`
		RSASPKI string `json:"rsa_spki"`
	} `json:"mtproto"`
}

type seedOptions struct {
	check           bool
	seed            bool
	verifySeed      bool
	verifyTurnstile bool
	activity        bool
	endpoint        string
	ownerUsername   string
	activityTarget  string
	maxActionsHour  int
	minActivityGap  time.Duration
}

type apiConfig struct {
	dcID int
	key  *rsa.PublicKey
}

type apiAction func(context.Context, *tg.Client) error

type telegramClientAction func(context.Context, *telegram.Client) error

type telegramClientRunner func(context.Context, telegram.Options, telegramClientAction) error

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "demo-seed: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	options, err := parseOptions(args)
	if err != nil {
		return err
	}
	if err := validateEndpointForMode(options.endpoint, options.verifyTurnstile); err != nil {
		return err
	}
	if options.check {
		return nil
	}
	if options.activity {
		if err := requireRoot(); err != nil {
			return err
		}
	}

	cfg, err := readClientConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	switch {
	case options.seed:
		if err := requireRoot(); err != nil {
			return err
		}
		ownerPassword, err := readSecretPassword()
		if err != nil {
			return fmt.Errorf("read seed-owner password file: %w", err)
		}
		inventory, err := readCredentialInventory()
		if err != nil {
			return err
		}
		return seedDemo(ctx, cfg, options, ownerPassword, inventory)
	case options.verifySeed:
		if err := requireRoot(); err != nil {
			return err
		}
		ownerPassword, err := readSecretPassword()
		if err != nil {
			return fmt.Errorf("read seed-owner password file: %w", err)
		}
		inventory, err := readCredentialInventory()
		if err != nil {
			return err
		}
		return verifySeed(ctx, cfg, options, ownerPassword, inventory)
	case options.verifyTurnstile:
		return verifyTurnstile(ctx, cfg)
	case options.activity:
		account, ok := seededActivityAccount()
		if !ok {
			return errors.New("seeded activity account is missing")
		}
		password, err := readActivityPasswordAt(defaultActivityInventory, account)
		if err != nil {
			return err
		}
		return runDemoActivity(ctx, cfg, options, account, password)
	default:
		return errors.New("select one of --check, --seed, --verify-seed, --verify-turnstile, or --activity")
	}
}

func parseOptions(args []string) (seedOptions, error) {
	var options seedOptions
	flags := flag.NewFlagSet("demo-seed", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&options.check, "check", false, "validate the internal endpoint")
	flags.BoolVar(&options.seed, "seed", false, "create demo accounts and content")
	flags.BoolVar(&options.verifySeed, "verify-seed", false, "verify the seeded public content")
	flags.BoolVar(&options.verifyTurnstile, "verify-turnstile", false, "verify production sign-up challenge enforcement")
	flags.BoolVar(&options.activity, "activity", false, "post one fixed demo activity message")
	flags.StringVar(&options.endpoint, "endpoint", defaultEndpoint, "internal MTProto service endpoint")
	flags.StringVar(&options.ownerUsername, "owner-username", "demo_seed_owner", "seed-owner login name")
	flags.StringVar(&options.activityTarget, "target", "river", "seeded megagroup slug for scheduled activity")
	flags.IntVar(&options.maxActionsHour, "max-actions-per-hour", 6, "activity ceiling, from 1 through 6 actions per hour")
	flags.DurationVar(&options.minActivityGap, "min-interval", 5*time.Minute, "minimum time between activity actions (at least 5m)")
	if err := flags.Parse(args); err != nil {
		return seedOptions{}, err
	}
	if flags.NArg() != 0 {
		return seedOptions{}, errors.New("unexpected positional arguments")
	}
	selected := 0
	for _, value := range []bool{options.check, options.seed, options.verifySeed, options.verifyTurnstile, options.activity} {
		if value {
			selected++
		}
	}
	if selected != 1 {
		return seedOptions{}, errors.New("select exactly one of --check, --seed, --verify-seed, --verify-turnstile, or --activity")
	}
	if (options.seed || options.verifySeed) && options.ownerUsername != "demo_seed_owner" {
		return seedOptions{}, errors.New("seed owner must use the demo_seed_owner account")
	}
	if options.activity {
		switch options.activityTarget {
		case "river", "garden", "workshop":
		default:
			return seedOptions{}, errors.New("activity target must be river, garden, or workshop")
		}
		if options.maxActionsHour < 1 || options.maxActionsHour > 6 {
			return seedOptions{}, errors.New("activity limit must be between 1 and 6 actions per hour")
		}
		if options.minActivityGap < 5*time.Minute {
			return seedOptions{}, errors.New("activity minimum interval must be at least 5m")
		}
	}
	return options, nil
}

func validateEndpoint(endpoint string) error {
	_, err := endpointDialHost(endpoint)
	return err
}

func endpointDialHost(endpoint string) (string, error) {
	switch endpoint {
	case defaultEndpoint:
		return defaultEndpointHost, nil
	case candidateEndpoint:
		return candidateEndpointHost, nil
	default:
		return "", errors.New("endpoint must be an internal teagramd service")
	}
}

func validateEndpointForMode(endpoint string, verifyTurnstile bool) error {
	if verifyTurnstile {
		if endpoint != trustedProxyEndpoint {
			return errors.New("turnstile verification must use the trusted internal haproxy:2443 service")
		}
		return nil
	}
	return validateEndpoint(endpoint)
}

func readClientConfig() (apiConfig, error) {
	data, err := os.ReadFile(defaultClientConfig)
	if err != nil {
		return apiConfig{}, fmt.Errorf("read public client config: %w", err)
	}
	var document clientDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return apiConfig{}, fmt.Errorf("parse public client config: %w", err)
	}
	if document.Version != 1 || document.MTProto.DCID < 1 || document.MTProto.DCID > 5 {
		return apiConfig{}, errors.New("public client config has an unsupported version or DC id")
	}
	der, err := base64.StdEncoding.DecodeString(document.MTProto.RSASPKI)
	if err != nil || len(der) == 0 {
		return apiConfig{}, errors.New("public client config has an invalid RSA key")
	}
	publicKey, err := x509ParsePublicKey(der)
	if err != nil {
		return apiConfig{}, fmt.Errorf("parse public client RSA key: %w", err)
	}
	return apiConfig{dcID: document.MTProto.DCID, key: publicKey}, nil
}

func x509ParsePublicKey(der []byte) (*rsa.PublicKey, error) {
	key, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	publicKey, ok := key.(*rsa.PublicKey)
	if !ok || publicKey.N == nil || publicKey.N.BitLen() < 2048 {
		return nil, errors.New("RSA public key is missing or too small")
	}
	return publicKey, nil
}

func requireRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("must run as root so generated account credentials stay private")
	}
	return nil
}

func readSecretPassword() (string, error) {
	info, err := os.Lstat(defaultOwnerPasswordFile)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return "", errors.New("password file must be a regular mode 0600 file")
	}
	data, err := os.ReadFile(defaultOwnerPasswordFile)
	if err != nil {
		return "", err
	}
	password := strings.TrimSpace(string(data))
	if len(password) != 64 {
		return "", errors.New("password file has an invalid value")
	}
	if _, err := hex.DecodeString(password); err != nil {
		return "", errors.New("password file has an invalid value")
	}
	return password, nil
}

func readCredentialInventory() (map[string]string, error) {
	parent := filepath.Dir(defaultAccountInventory)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return nil, fmt.Errorf("inspect account password directory: %w", err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode().Perm() != 0700 {
		return nil, errors.New("account password directory must be a real mode 0700 directory")
	}
	info, err := os.Lstat(defaultAccountInventory)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect account password inventory: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("account password inventory must be a regular mode 0600 file")
	}
	data, err := os.ReadFile(defaultAccountInventory)
	if err != nil {
		return nil, fmt.Errorf("read account password inventory: %w", err)
	}
	var inventory map[string]string
	if err := json.Unmarshal(data, &inventory); err != nil {
		return nil, errors.New("account password inventory is not valid JSON")
	}
	for _, password := range inventory {
		if len(password) != 64 {
			return nil, errors.New("account password inventory contains an invalid password")
		}
		if _, err := hex.DecodeString(password); err != nil {
			return nil, errors.New("account password inventory contains an invalid password")
		}
	}
	if inventory == nil {
		inventory = map[string]string{}
	}
	return inventory, nil
}

func writeCredentialInventory(inventory map[string]string) error {
	return writeCredentialInventoryAt(defaultAccountInventory, inventory)
}

func writeCredentialInventoryAt(path string, inventory map[string]string) error {
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect account password directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("account password directory must be a real mode 0700 directory")
	}
	if existing, err := os.Lstat(path); err == nil {
		if !existing.Mode().IsRegular() || existing.Mode().Perm() != 0600 {
			return errors.New("existing account password inventory has unsafe permissions")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect account password inventory: %w", err)
	}
	data, err := json.Marshal(inventory)
	if err != nil {
		return fmt.Errorf("encode account password inventory: %w", err)
	}
	temporary, err := os.CreateTemp(dir, ".account-passwords-*")
	if err != nil {
		return fmt.Errorf("create account password inventory: %w", err)
	}
	temporaryName := temporary.Name()
	closed := false
	cleanup := func() error {
		var cleanupErr error
		if !closed {
			if err := temporary.Close(); err != nil {
				cleanupErr = errors.Join(cleanupErr, err)
			}
			closed = true
		}
		if err := os.Remove(temporaryName); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, err)
		}
		return cleanupErr
	}
	if err := temporary.Chmod(0600); err != nil {
		return errors.Join(fmt.Errorf("set account password permissions: %w", err), cleanup())
	}
	if _, err := temporary.Write(data); err != nil {
		return errors.Join(fmt.Errorf("write account password inventory: %w", err), cleanup())
	}
	if err := temporary.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync account password inventory: %w", err), cleanup())
	}
	if err := temporary.Close(); err != nil {
		closed = true
		return errors.Join(fmt.Errorf("close account password inventory: %w", err), cleanup())
	}
	closed = true
	if err := os.Rename(temporaryName, path); err != nil {
		return errors.Join(fmt.Errorf("publish account password inventory: %w", err), cleanup())
	}
	if err := os.Chmod(path, 0600); err != nil {
		return fmt.Errorf("set account password permissions: %w", err)
	}
	return nil
}

func randomPassword() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate account password: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func newTelegramClientAt(cfg apiConfig, host string) *telegram.Client {
	return newTelegramClientWithStorageAt(cfg, host, &session.StorageMemory{})
}

func newTelegramClientWithStorageAt(cfg apiConfig, host string, storage session.Storage) *telegram.Client {
	return telegram.NewClient(appID, appHash, telegramOptionsAt(cfg, host, storage))
}

func telegramOptionsAt(cfg apiConfig, host string, storage session.Storage) telegram.Options {
	return telegram.Options{
		DC:             cfg.dcID,
		DCList:         dcs.List{Options: []tg.DCOption{{ID: cfg.dcID, IPAddress: host, Port: 2443}}},
		PublicKeys:     []telegram.PublicKey{{RSA: cfg.key}},
		Resolver:       dcs.Plain(dcs.PlainOptions{}),
		SessionStorage: storage,
	}
}

func accountClientOptions(cfg apiConfig, endpoint string, storage session.Storage) (telegram.Options, error) {
	host, err := endpointDialHost(endpoint)
	if err != nil {
		return telegram.Options{}, fmt.Errorf("resolve demo account endpoint: %w", err)
	}
	return telegramOptionsAt(cfg, host, storage), nil
}

func withAccount(ctx context.Context, cfg apiConfig, endpoint string, account accountSpec, password string, savePassword func(string) error, action apiAction) error {
	return withAccountUsingRunner(ctx, cfg, endpoint, account, password, savePassword, action, runTelegramClient)
}

func withAccountUsingRunner(ctx context.Context, cfg apiConfig, endpoint string, account accountSpec, password string, savePassword func(string) error, action apiAction, runner telegramClientRunner) error {
	options, err := accountClientOptions(cfg, endpoint, &session.StorageMemory{})
	if err != nil {
		return err
	}
	return runner(ctx, options, func(runCtx context.Context, client *telegram.Client) error {
		api := client.API()
		if err := authenticate(runCtx, api, account, &password, savePassword); err != nil {
			return err
		}
		return action(runCtx, api)
	})
}

func runTelegramClient(ctx context.Context, options telegram.Options, action telegramClientAction) error {
	client := telegram.NewClient(appID, appHash, options)
	return client.Run(ctx, func(runCtx context.Context) error {
		return action(runCtx, client)
	})
}

func authenticate(ctx context.Context, api *tg.Client, account accountSpec, password *string, savePassword func(string) error) error {
	sent, err := api.AuthSendCode(ctx, &tg.AuthSendCodeRequest{
		PhoneNumber: account.loginID,
		APIID:       appID,
		APIHash:     appHash,
	})
	if err != nil {
		return fmt.Errorf("request demo account sign-in code: %w", err)
	}
	sentCode, ok := sent.(*tg.AuthSentCode)
	if !ok {
		return fmt.Errorf("unexpected sign-in response %T", sent)
	}
	signIn, signInErr := api.AuthSignIn(ctx, &tg.AuthSignInRequest{
		PhoneNumber:   account.loginID,
		PhoneCodeHash: sentCode.PhoneCodeHash,
		PhoneCode:     "00000",
	})
	_, responseRequiresSignup := signIn.(*tg.AuthAuthorizationSignUpRequired)
	if isSignUpRequired(signInErr) || (signInErr == nil && responseRequiresSignup) {
		if account.owner {
			return errors.New("seed-owner account is missing")
		}
		if password == nil {
			return errors.New("new demo account password cannot be stored")
		}
		generated, err := randomPassword()
		if err != nil {
			return err
		}
		if savePassword == nil {
			return errors.New("new demo account password has no secure destination")
		}
		if err := savePassword(generated); err != nil {
			return fmt.Errorf("persist generated demo account password: %w", err)
		}
		*password = generated
		result, err := api.AuthSignUp(ctx, &tg.AuthSignUpRequest{
			PhoneNumber:   account.loginID,
			PhoneCodeHash: sentCode.PhoneCodeHash,
			FirstName:     account.firstName,
			LastName:      account.lastName,
		})
		if err != nil {
			return fmt.Errorf("create demo account through auth.signUp: %w", err)
		}
		if _, ok := result.(*tg.AuthAuthorization); !ok {
			return fmt.Errorf("unexpected account creation response %T", result)
		}
		if err := setNewPassword(ctx, api, *password); err != nil {
			return fmt.Errorf("set demo account password: %w", err)
		}
		return nil
	}
	if isSessionPasswordNeeded(signInErr) {
		if password == nil || *password == "" {
			return errors.New("existing demo account is missing its generated password")
		}
		if err := checkPassword(ctx, api, *password); err != nil {
			return fmt.Errorf("verify demo account password: %w", err)
		}
		return nil
	}
	if signInErr != nil {
		return fmt.Errorf("sign in to demo account: %w", signInErr)
	}
	if _, ok := signIn.(*tg.AuthAuthorization); !ok {
		return fmt.Errorf("unexpected sign-in response %T", signIn)
	}
	settings, err := api.AccountGetPassword(ctx)
	if err != nil {
		return fmt.Errorf("inspect demo account password settings: %w", err)
	}
	if !settings.HasPassword {
		if account.owner {
			return errors.New("seed-owner account has no password configured")
		}
		if password == nil || *password == "" {
			return errors.New("existing demo account is missing its generated password")
		}
		if err := setNewPassword(ctx, api, *password); err != nil {
			return fmt.Errorf("complete demo account password setup: %w", err)
		}
	}
	return nil
}

func setNewPassword(ctx context.Context, api *tg.Client, password string) error {
	settings, err := api.AccountGetPassword(ctx)
	if err != nil {
		return err
	}
	algo, ok := settings.NewAlgo.(*tg.PasswordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow)
	if !ok {
		return fmt.Errorf("unsupported password setup algorithm %T", settings.NewAlgo)
	}
	newHash, err := auth.NewPasswordHash([]byte(password), algo)
	if err != nil {
		return err
	}
	_, err = api.AccountUpdatePasswordSettings(ctx, &tg.AccountUpdatePasswordSettingsRequest{
		Password: &tg.InputCheckPasswordEmpty{},
		NewSettings: tg.AccountPasswordInputSettings{
			NewAlgo:         algo,
			NewPasswordHash: newHash,
		},
	})
	return err
}

func checkPassword(ctx context.Context, api *tg.Client, password string) error {
	settings, err := api.AccountGetPassword(ctx)
	if err != nil {
		return err
	}
	proof, err := auth.PasswordHash([]byte(password), settings.SRPID, settings.SRPB, settings.SecureRandom, settings.CurrentAlgo)
	if err != nil {
		return err
	}
	_, err = api.AuthCheckPassword(ctx, proof)
	return err
}

func isSignUpRequired(err error) bool {
	return isRPCMessage(err, "SIGN_UP_REQUIRED")
}

func isSessionPasswordNeeded(err error) bool {
	return isRPCMessage(err, "SESSION_PASSWORD_NEEDED")
}

func isRPCMessage(err error, message string) bool {
	var rpcErr *tgerr.Error
	return errors.As(err, &rpcErr) && rpcErr.Message == message
}

func randomID(label string) int64 {
	sum := sha256.Sum256([]byte(label))
	value := binary.BigEndian.Uint64(sum[:8]) & (uint64(1)<<63 - 1)
	if value == 0 {
		return 1
	}
	return int64(value)
}

func randomBytes(size int) ([]byte, error) {
	result := make([]byte, size)
	if _, err := rand.Read(result); err != nil {
		return nil, err
	}
	return result, nil
}

func randomInt64() (int64, error) {
	data, err := randomBytes(8)
	if err != nil {
		return 0, err
	}
	value := binary.BigEndian.Uint64(data) & (uint64(1)<<63 - 1)
	if value == 0 {
		value = 1
	}
	return int64(value), nil
}

func newProbeUsername() (string, error) {
	value, err := randomBytes(8)
	if err != nil {
		return "", fmt.Errorf("generate sign-up probe username: %w", err)
	}
	return "demo_probe_" + hex.EncodeToString(value), nil
}

type turnstileProbeAPI interface {
	AuthSendCode(context.Context, *tg.AuthSendCodeRequest) (tg.AuthSentCodeClass, error)
	AuthSignIn(context.Context, *tg.AuthSignInRequest) (tg.AuthAuthorizationClass, error)
	AuthSignUp(context.Context, *tg.AuthSignUpRequest) (tg.AuthAuthorizationClass, error)
}

func verifyTurnstile(ctx context.Context, cfg apiConfig) error {
	username, err := newProbeUsername()
	if err != nil {
		return err
	}
	client := newTelegramClientAt(cfg, trustedProxyHost)
	return client.Run(ctx, func(runCtx context.Context) error {
		return verifyTurnstileWithAPI(runCtx, client.API(), username)
	})
}

func verifyTurnstileWithAPI(ctx context.Context, api turnstileProbeAPI, username string) error {
	sent, err := api.AuthSendCode(ctx, &tg.AuthSendCodeRequest{PhoneNumber: username, APIID: appID, APIHash: appHash})
	if err != nil {
		return fmt.Errorf("request sign-up probe code: %w", err)
	}
	sentCode, ok := sent.(*tg.AuthSentCode)
	if !ok {
		return fmt.Errorf("unexpected sign-up probe response %T", sent)
	}
	signIn, err := api.AuthSignIn(ctx, &tg.AuthSignInRequest{
		PhoneNumber: username, PhoneCodeHash: sentCode.PhoneCodeHash, PhoneCode: "00000",
	})
	_, responseRequiresSignup := signIn.(*tg.AuthAuthorizationSignUpRequired)
	if err == nil {
		if !responseRequiresSignup {
			return errors.New("sign-up probe unexpectedly authenticated")
		}
	} else if !isSignUpRequired(err) {
		return fmt.Errorf("sign-up probe did not reach registration: %w", err)
	}
	// Keep the probe non-creating if the CAPTCHA gate is ever missing.
	_, err = api.AuthSignUp(ctx, &tg.AuthSignUpRequest{
		PhoneNumber:   username,
		PhoneCodeHash: sentCode.PhoneCodeHash,
		FirstName:     strings.Repeat("x", 65),
		LastName:      "Probe",
	})
	if !isSignupCaptchaRequired(err) {
		if err == nil {
			return errors.New("production sign-up gate accepted a probe account")
		}
		return fmt.Errorf("production sign-up gate returned an unexpected result: %w", err)
	}
	return nil
}

func isSignupCaptchaRequired(err error) bool {
	var rpcErr *tgerr.Error
	return errors.As(err, &rpcErr) && strings.HasPrefix(rpcErr.Message, "RECAPTCHA_CHECK_signup__")
}
