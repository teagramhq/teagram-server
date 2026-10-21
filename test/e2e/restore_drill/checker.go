package restoredrill

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/gotd/td/exchange"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/dcs"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"golang.org/x/sys/unix"

	"github.com/teagramhq/teagram-server/internal/rsakey"
)

const (
	checkTimeout       = 45 * time.Second
	checkerRunTimeout  = 2 * checkTimeout
	maxInvocationBytes = 4096
	maxPublicKeyBytes  = 4096
	maxPasswordBytes   = 1024
	maxUsernameBytes   = 32
)

type Phase string

const (
	PhaseInvocation     Phase = "invocation"
	PhaseConfiguration  Phase = "help_get_config"
	PhaseProvisioning   Phase = "provisioning"
	PhaseSignIn         Phase = "synthetic_signin"
	PhaseApplication    Phase = "application_assertions"
	PhaseCheckerCleanup Phase = "checker_cleanup"
	PhaseArtifact       Phase = "result_artifact"
)

type Category string

const (
	CategoryAssertionsPassed      Category = "assertions_passed"
	CategoryInvocationInvalid     Category = "invocation_invalid"
	CategoryIdentityCollision     Category = "identity_collision"
	CategoryAuthFailure           Category = "auth_failure"
	CategoryKeyMismatch           Category = "key_mismatch"
	CategoryConfigurationInvalid  Category = "configuration_invalid"
	CategoryIdentityMismatch      Category = "identity_mismatch"
	CategoryConnectionFailure     Category = "connection_failure"
	CategoryProvisioningFailure   Category = "provisioning_failure"
	CategoryTimeout               Category = "timeout"
	CategoryCheckerCleanupFailure Category = "checker_cleanup_failure"
	CategoryArtifactFailure       Category = "artifact_failure"
)

type Assertion string

const (
	AssertionPass   Assertion = "pass"
	AssertionFail   Assertion = "fail"
	AssertionNotRun Assertion = "not_run"
)

// Invocation carries only attestations and a loopback endpoint. Secrets are
// kept in sibling protected files and are never accepted in this JSON object.
type Invocation struct {
	SchemaVersion            int    `json:"schema_version"`
	Mode                     string `json:"mode"`
	Endpoint                 string `json:"endpoint"`
	ScratchTargetOwned       bool   `json:"scratch_target_owned"`
	NetworkNamespaceIsolated bool   `json:"network_namespace_isolated"`
	DaemonIdentityPinned     bool   `json:"daemon_identity_pinned"`
	RegistrationClosed       bool   `json:"registration_closed"`
}

// Result is the checker-owned assertion contract. It intentionally contains
// no account, endpoint, secret, resource, timing, or outer-cleanup fields.
type Result struct {
	SchemaVersion   int       `json:"schema_version"`
	Phase           Phase     `json:"phase"`
	Category        Category  `json:"category"`
	HelpGetConfig   Assertion `json:"help_getConfig"`
	SyntheticSignin Assertion `json:"synthetic_signin"`
}

type Identity struct {
	ID       int64
	Username string
}

type invocationFiles struct {
	directory    string
	dir          *os.File
	invocation   Invocation
	endpoint     string
	publicKey    *rsa.PublicKey
	publicPEM    []byte
	username     string
	password     []byte
	passwordLine []byte
}

type stageFailureError struct {
	category Category
}

type connectionErrorCapture struct {
	mu  sync.Mutex
	err error
}

func (capture *connectionErrorCapture) record(err error) {
	if err == nil {
		return
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.err == nil || errors.Is(err, exchange.ErrKeyFingerprintNotFound) {
		capture.err = err
	}
}

func (capture *connectionErrorCapture) get() error {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.err
}

func (failure *stageFailureError) Error() string {
	return string(failure.category)
}

var (
	usernamePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	dcHostPattern   = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)
)

// ValidateInvocation rejects any mode that is not an attested synthetic
// fixture targeting a literal loopback WebSocket endpoint.
func ValidateInvocation(invocation Invocation) Category {
	if invocation.SchemaVersion != 1 || invocation.Mode != "synthetic-only" ||
		!invocation.ScratchTargetOwned || !invocation.NetworkNamespaceIsolated ||
		!invocation.DaemonIdentityPinned || !invocation.RegistrationClosed ||
		!validLoopbackEndpoint(invocation.Endpoint) {
		return CategoryInvocationInvalid
	}
	return CategoryAssertionsPassed
}

// ValidConfig validates the actual daemon's response shape without using any
// advertised address to select or change the client's endpoint.
func ValidConfig(config *tg.Config) bool {
	now := int(time.Now().Unix())
	const clockSkew = int((5 * time.Minute) / time.Second)
	if config == nil || config.ThisDC != 2 || config.TestMode || config.WebfileDCID != 2 ||
		config.Date < now-clockSkew || config.Date > now+clockSkew || config.Expires <= now ||
		config.Expires <= config.Date || config.Expires-config.Date > int((2*time.Hour)/time.Second) ||
		config.ChatSizeMax <= 0 || config.MegagroupSizeMax <= 0 || config.ForwardedCountMax <= 0 ||
		config.CaptionLengthMax <= 0 || config.MessageLengthMax <= 0 || len(config.DCOptions) != 1 {
		return false
	}
	option := config.DCOptions[0]
	return option.ID == config.ThisDC && option.Port > 0 && option.Port <= 65535 && validDCAddress(option.IPAddress)
}

// ValidIdentity requires both authorization responses to bind to the account
// returned by the insert-only create-user command.
func ValidIdentity(user tg.UserClass, expectedID int64, expectedUsername string) bool {
	if expectedID <= 0 || !usernamePattern.MatchString(expectedUsername) {
		return false
	}
	account, ok := user.(*tg.User)
	return ok && account.ID == expectedID && account.Username == expectedUsername
}

// CheckConfig connects only to a literal loopback endpoint, pins the supplied
// generated RSA key, and validates help.getConfig from the running daemon.
func CheckConfig(parent context.Context, endpoint string, publicKeyPEM []byte) Result {
	result := newResult(PhaseConfiguration, CategoryInvocationInvalid)
	if !validLoopbackEndpoint(endpoint) {
		return result
	}
	key, ok := parsePublicKey(publicKeyPEM)
	if !ok {
		result.Category = CategoryKeyMismatch
		return result
	}
	ctx, cancel := boundedContext(parent)
	defer cancel()
	if ctx.Err() != nil {
		result.Category = CategoryTimeout
		return result
	}
	if err := probeLoopback(ctx, endpoint); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.Category = CategoryTimeout
		} else {
			result.Category = CategoryConnectionFailure
		}
		return result
	}

	connectionError := &connectionErrorCapture{}
	client, sessionStore, err := newPinnedClient(endpoint, key, connectionError.record)
	if err != nil {
		result.Category = CategoryConnectionFailure
		return result
	}
	defer sessionStore.clear()
	entered := false
	err = client.Run(ctx, func(ctx context.Context) error {
		entered = true
		config, err := client.API().HelpGetConfig(ctx)
		if err != nil || !ValidConfig(config) {
			return &stageFailureError{category: CategoryConfigurationInvalid}
		}
		return nil
	})
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && (entered || connectionError.get() == nil) {
			result.Category = CategoryTimeout
			return result
		}
		if failure, ok := errors.AsType[*stageFailureError](err); ok {
			result.Category = failure.category
			result.HelpGetConfig = AssertionFail
			return result
		}
		if entered {
			result.Category = CategoryConfigurationInvalid
		} else {
			result.Category = connectionFailureCategory(connectionError.get())
		}
		result.HelpGetConfig = AssertionFail
		return result
	}
	result.Category = CategoryAssertionsPassed
	result.HelpGetConfig = AssertionPass
	return result
}

// CheckSignIn performs ordinary username/password MTProto authentication and
// checks both authorization identity responses against the new account.
func CheckSignIn(parent context.Context, endpoint string, publicKeyPEM []byte, username string, password []byte, expected Identity) Result {
	result := newResult(PhaseSignIn, CategoryInvocationInvalid)
	if !validLoopbackEndpoint(endpoint) || !usernamePattern.MatchString(username) ||
		expected.Username != username || expected.ID <= 0 || len(password) == 0 || len(password) > maxPasswordBytes {
		return result
	}
	key, ok := parsePublicKey(publicKeyPEM)
	if !ok {
		result.Category = CategoryKeyMismatch
		return result
	}
	ctx, cancel := boundedContext(parent)
	defer cancel()
	if ctx.Err() != nil {
		result.Category = CategoryTimeout
		return result
	}
	if err := probeLoopback(ctx, endpoint); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.Category = CategoryTimeout
		} else {
			result.Category = CategoryConnectionFailure
		}
		return result
	}

	secret := append([]byte(nil), password...)
	defer clear(secret)
	connectionError := &connectionErrorCapture{}
	client, sessionStore, err := newPinnedClient(endpoint, key, connectionError.record)
	if err != nil {
		result.Category = CategoryConnectionFailure
		return result
	}
	defer sessionStore.clear()
	entered := false
	err = client.Run(ctx, func(ctx context.Context) error {
		entered = true
		return signIn(ctx, client, username, secret, expected)
	})
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && (entered || connectionError.get() == nil) {
			result.Category = CategoryTimeout
			result.SyntheticSignin = AssertionFail
			return result
		}
		switch failure, ok := errors.AsType[*stageFailureError](err); {
		case ok:
			result.Category = failure.category
		case entered:
			result.Category = CategoryAuthFailure
		default:
			result.Category = connectionFailureCategory(connectionError.get())
		}
		result.SyntheticSignin = AssertionFail
		return result
	}
	result.Category = CategoryAssertionsPassed
	result.SyntheticSignin = AssertionPass
	return result
}

// Run checks the actual daemon configuration and signs in as the synthetic
// identity created by the separate one-shot provisioner.
func Run(parent context.Context, invocationPath string) (result Result) {
	result = newResult(PhaseInvocation, CategoryInvocationInvalid)
	loaded, category := loadInvocation(invocationPath)
	if loaded == nil {
		return result
	}
	var artifact *os.File
	defer func() {
		clear(loaded.password)
		clear(loaded.passwordLine)
		clear(loaded.publicPEM)
		if err := loaded.dir.Close(); err != nil && result.Category == CategoryAssertionsPassed {
			result = newResult(PhaseArtifact, CategoryArtifactFailure)
		}
		if artifact != nil {
			if err := finishResultArtifact(artifact, result); err != nil {
				result = newResult(PhaseArtifact, CategoryArtifactFailure)
			}
		}
	}()

	var err error
	artifact, err = createResultArtifact(loaded.dir)
	if err != nil {
		return newResult(PhaseArtifact, CategoryArtifactFailure)
	}
	if category != CategoryAssertionsPassed {
		return newResult(PhaseInvocation, category)
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, checkerRunTimeout)
	defer cancel()
	identity, category := readProvisionedIdentity(loaded.dir, loaded.username)
	if category != CategoryAssertionsPassed {
		return newResult(PhaseProvisioning, category)
	}
	configuration := CheckConfig(ctx, loaded.endpoint, loaded.publicPEM)
	if configuration.Category != CategoryAssertionsPassed {
		return configuration
	}

	result = CheckSignIn(ctx, loaded.endpoint, loaded.publicPEM, loaded.username, loaded.password, identity)
	result.HelpGetConfig = AssertionPass
	if result.Category == CategoryAssertionsPassed {
		result.Phase = PhaseApplication
	}
	return result
}

// RunCLI emits the typed result on stdout and never emits raw diagnostics. The
// caller decides how to publish or clean up the protected invocation directory.
func RunCLI(ctx context.Context, invocationPath string, stdout, stderr io.Writer) int {
	result := Run(ctx, invocationPath)
	return writeCLIResult(result, stdout)
}

func writeCLIResult(result Result, stdout io.Writer) int {
	encoder := json.NewEncoder(stdout)
	if err := encoder.Encode(result); err != nil {
		return 1
	}
	if result.Category != CategoryAssertionsPassed {
		return 1
	}
	return 0
}

func newResult(phase Phase, category Category) Result {
	return Result{
		SchemaVersion:   1,
		Phase:           phase,
		Category:        category,
		HelpGetConfig:   AssertionNotRun,
		SyntheticSignin: AssertionNotRun,
	}
}

func boundedContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, checkTimeout)
}

func validLoopbackEndpoint(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "ws" || parsed.User != nil || parsed.Opaque != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Path != "/apiws" || parsed.RawPath != "" {
		return false
	}
	host := parsed.Hostname()
	portText := parsed.Port()
	if host == "" || portText == "" {
		return false
	}
	address, err := netip.ParseAddr(host)
	if err != nil || !address.IsLoopback() || address.Zone() != "" {
		return false
	}
	port, err := strconv.Atoi(portText)
	return err == nil && port > 0 && port <= 65535
}

func validDCAddress(host string) bool {
	if host == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, "/\\?#@\x00") {
		return false
	}
	if address, err := netip.ParseAddr(host); err == nil {
		return !address.IsUnspecified() && !address.IsMulticast() && address.Zone() == ""
	}
	return len(host) <= 253 && dcHostPattern.MatchString(host)
}

func parsePublicKey(data []byte) (*rsa.PublicKey, bool) {
	if len(data) == 0 || len(data) > maxPublicKeyBytes {
		return nil, false
	}
	block, rest := pem.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, false
	}
	var key *rsa.PublicKey
	switch block.Type {
	case "PUBLIC KEY":
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, false
		}
		var ok bool
		key, ok = parsed.(*rsa.PublicKey)
		if !ok {
			return nil, false
		}
	case "RSA PUBLIC KEY":
		parsed, err := x509.ParsePKCS1PublicKey(block.Bytes)
		if err != nil {
			return nil, false
		}
		key = parsed
	default:
		return nil, false
	}
	if key.N.BitLen() != 2048 || key.E != 65537 || rsakey.Fingerprint(key) == -0x449d8e0f8515d6 {
		return nil, false
	}
	return key, true
}

func newPinnedClient(endpoint string, key *rsa.PublicKey, onDead func(error)) (*telegram.Client, *memorySession, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, nil, errors.New("endpoint invalid")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		return nil, nil, errors.New("endpoint invalid")
	}
	storage := &memorySession{}
	client := telegram.NewClient(1, "restore-drill", telegram.Options{
		DC: 2,
		DCList: dcs.List{
			Options: []tg.DCOption{{ID: 2, IPAddress: parsed.Hostname(), Port: port}},
			Domains: map[int]string{2: endpoint},
		},
		PublicKeys: []telegram.PublicKey{{RSA: key}},
		OnDead:     onDead,
		Resolver: dcs.Websocket(dcs.WebsocketOptions{
			DialOptions: &websocket.DialOptions{
				HTTPClient: &http.Client{
					CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
						return http.ErrUseLastResponse
					},
				},
			},
		}),
		SessionStorage: storage,
	})
	return client, storage, nil
}

func connectionFailureCategory(err error) Category {
	if errors.Is(err, exchange.ErrKeyFingerprintNotFound) {
		return CategoryKeyMismatch
	}
	return CategoryConnectionFailure
}

func probeLoopback(ctx context.Context, endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return errors.New("endpoint unavailable")
	}
	connection, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(parsed.Hostname(), parsed.Port()))
	if err != nil {
		return err
	}
	if err := connection.Close(); err != nil {
		return errors.New("endpoint unavailable")
	}
	return nil
}

func signIn(ctx context.Context, client *telegram.Client, username string, password []byte, expected Identity) error {
	api := client.API()
	sent, err := api.AuthSendCode(ctx, &tg.AuthSendCodeRequest{
		PhoneNumber: username,
		APIID:       1,
		APIHash:     "fixture",
	})
	if err != nil {
		return &stageFailureError{category: CategoryAuthFailure}
	}
	code, ok := sent.(*tg.AuthSentCode)
	if !ok {
		return &stageFailureError{category: CategoryAuthFailure}
	}
	_, err = api.AuthSignIn(ctx, &tg.AuthSignInRequest{PhoneNumber: username, PhoneCodeHash: code.PhoneCodeHash})
	if !rpcMessage(err, "SESSION_PASSWORD_NEEDED") {
		return &stageFailureError{category: CategoryAuthFailure}
	}
	settings, err := api.AccountGetPassword(ctx)
	if err != nil {
		return &stageFailureError{category: CategoryAuthFailure}
	}
	proof, err := auth.PasswordHash(password, settings.SRPID, settings.SRPB, settings.SecureRandom, settings.CurrentAlgo)
	if err != nil {
		return &stageFailureError{category: CategoryAuthFailure}
	}
	authorization, err := api.AuthCheckPassword(ctx, proof)
	clear(proof.A)
	clear(proof.M1)
	if err != nil {
		return &stageFailureError{category: CategoryAuthFailure}
	}
	authenticated, ok := authorization.(*tg.AuthAuthorization)
	if !ok || !ValidIdentity(authenticated.User, expected.ID, expected.Username) {
		return &stageFailureError{category: CategoryIdentityMismatch}
	}
	status, err := client.Auth().Status(ctx)
	if err != nil || !status.Authorized {
		return &stageFailureError{category: CategoryAuthFailure}
	}
	users, err := api.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
	if err != nil || len(users) != 1 || !ValidIdentity(users[0], expected.ID, expected.Username) {
		return &stageFailureError{category: CategoryIdentityMismatch}
	}
	return nil
}

func rpcMessage(err error, want string) bool {
	var rpcErr *tgerr.Error
	return errors.As(err, &rpcErr) && rpcErr.Message == want
}

func loadInvocation(path string) (*invocationFiles, Category) {
	if path == "" {
		return nil, CategoryInvocationInvalid
	}
	absPath, err := filepath.Abs(path)
	if err != nil || filepath.Base(absPath) != "invocation.json" {
		return nil, CategoryInvocationInvalid
	}
	directory := filepath.Dir(absPath)
	dir, err := openProtectedDirectory(directory)
	if err != nil {
		return nil, CategoryInvocationInvalid
	}
	loaded := &invocationFiles{directory: directory, dir: dir}
	raw, ok := readRecordAt(dir.Fd(), "invocation.json", 0o600, maxInvocationBytes)
	if !ok {
		return loaded, CategoryInvocationInvalid
	}
	defer clear(raw)
	if !decodeInvocation(raw, &loaded.invocation) {
		return loaded, CategoryInvocationInvalid
	}
	if category := ValidateInvocation(loaded.invocation); category != CategoryAssertionsPassed {
		return loaded, category
	}
	loaded.endpoint = loaded.invocation.Endpoint
	if artifactExists(dir.Fd(), "result.json") {
		return loaded, CategoryInvocationInvalid
	}
	usernameData, ok := readRecordAt(dir.Fd(), "username", 0o400, maxUsernameBytes+1)
	if !ok {
		return loaded, CategoryInvocationInvalid
	}
	usernameBytes := trimOneNewline(usernameData)
	loaded.username = string(usernameBytes)
	clear(usernameData)
	if !usernamePattern.MatchString(loaded.username) {
		return loaded, CategoryInvocationInvalid
	}
	loaded.passwordLine, ok = readRecordAt(dir.Fd(), "password", 0o400, maxPasswordBytes+1)
	if !ok {
		return loaded, CategoryInvocationInvalid
	}
	loaded.password = append([]byte(nil), trimOneNewline(loaded.passwordLine)...)
	if len(loaded.password) == 0 || len(loaded.password) > maxPasswordBytes || bytes.ContainsAny(loaded.password, "\r\n\x00") {
		clear(loaded.password)
		return loaded, CategoryInvocationInvalid
	}
	loaded.publicPEM, ok = readRecordAt(dir.Fd(), "server.pub.pem", 0o400, maxPublicKeyBytes)
	if !ok {
		return loaded, CategoryInvocationInvalid
	}
	loaded.publicKey, ok = parsePublicKey(loaded.publicPEM)
	if !ok {
		return loaded, CategoryKeyMismatch
	}
	return loaded, CategoryAssertionsPassed
}

type provisionedIdentityDocument struct {
	SchemaVersion int    `json:"schema_version"`
	UserID        int64  `json:"user_id"`
	Username      string `json:"username"`
}

func readProvisionedIdentity(directory *os.File, expectedUsername string) (Identity, Category) {
	data, ok := readRecordAt(directory.Fd(), "provisioned.json", 0o400, 1024)
	if !ok {
		return Identity{}, CategoryProvisioningFailure
	}
	defer clear(data)
	var document provisionedIdentityDocument
	if !decodeProvisionedIdentity(data, &document) || document.SchemaVersion != 1 ||
		document.UserID <= 0 || document.Username != expectedUsername || !usernamePattern.MatchString(document.Username) {
		return Identity{}, CategoryProvisioningFailure
	}
	return Identity{ID: document.UserID, Username: document.Username}, CategoryAssertionsPassed
}

func decodeProvisionedIdentity(data []byte, document *provisionedIdentityDocument) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return false
	}
	seen := make(map[string]struct{}, 3)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return false
		}
		key, ok := keyToken.(string)
		if !ok {
			return false
		}
		if _, exists := seen[key]; exists {
			return false
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return false
		}
		var valid bool
		switch key {
		case "schema_version":
			valid = json.Unmarshal(value, &document.SchemaVersion) == nil
		case "user_id":
			valid = json.Unmarshal(value, &document.UserID) == nil
		case "username":
			valid = json.Unmarshal(value, &document.Username) == nil
		default:
			return false
		}
		clear(value)
		if !valid {
			return false
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || len(seen) != 3 {
		return false
	}
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}

func decodeInvocation(data []byte, invocation *Invocation) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return false
	}
	seen := make(map[string]struct{}, 7)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return false
		}
		key, ok := keyToken.(string)
		if !ok {
			return false
		}
		if _, exists := seen[key]; exists {
			return false
		}
		seen[key] = struct{}{}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return false
		}
		valid := decodeInvocationField(key, value, invocation)
		clear(value)
		if !valid {
			return false
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') {
		return false
	}
	var trailing any
	return errors.Is(decoder.Decode(&trailing), io.EOF)
}

func decodeInvocationField(key string, value []byte, invocation *Invocation) bool {
	switch key {
	case "schema_version":
		return json.Unmarshal(value, &invocation.SchemaVersion) == nil
	case "mode":
		return json.Unmarshal(value, &invocation.Mode) == nil
	case "endpoint":
		return json.Unmarshal(value, &invocation.Endpoint) == nil
	case "scratch_target_owned":
		return json.Unmarshal(value, &invocation.ScratchTargetOwned) == nil
	case "network_namespace_isolated":
		return json.Unmarshal(value, &invocation.NetworkNamespaceIsolated) == nil
	case "daemon_identity_pinned":
		return json.Unmarshal(value, &invocation.DaemonIdentityPinned) == nil
	case "registration_closed":
		return json.Unmarshal(value, &invocation.RegistrationClosed) == nil
	default:
		return false
	}
}

func trimOneNewline(data []byte) []byte {
	if len(data) > 0 && data[len(data)-1] == '\n' {
		data = data[:len(data)-1]
	}
	return data
}

func openProtectedDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, errors.New("invocation directory invalid")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		if closeErr := unix.Close(fd); closeErr != nil {
			return nil, errors.New("invocation directory unavailable")
		}
		return nil, errors.New("invocation directory unavailable")
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR || !ownedByCurrentUID(stat.Uid) || stat.Mode&0o777 != 0o700 {
		if closeErr := unix.Close(fd); closeErr != nil {
			return nil, errors.New("invocation directory unavailable")
		}
		return nil, errors.New("invocation directory invalid")
	}
	return os.NewFile(uintptr(fd), "restore-drill-invocation"), nil
}

func readRecordAt(dirFD uintptr, name string, permissions os.FileMode, maxBytes int) ([]byte, bool) {
	fd, err := unix.Openat(int(dirFD), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	file := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return nil, false
		}
		return nil, false
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || !ownedByCurrentUID(stat.Uid) ||
		os.FileMode(stat.Mode&0o777) != permissions || stat.Nlink != 1 || stat.Size < 0 || stat.Size > int64(maxBytes) {
		if closeErr := file.Close(); closeErr != nil {
			return nil, false
		}
		return nil, false
	}
	data, readErr := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > maxBytes {
		clear(data)
		return nil, false
	}
	return data, true
}

func ownedByCurrentUID(uid uint32) bool {
	return strconv.FormatUint(uint64(uid), 10) == strconv.Itoa(os.Geteuid())
}

func artifactExists(dirFD uintptr, name string) bool {
	var stat unix.Stat_t
	err := unix.Fstatat(int(dirFD), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	return err == nil || !errors.Is(err, unix.ENOENT)
}

func createResultArtifact(dir *os.File) (*os.File, error) {
	fd, err := unix.Openat(int(dir.Fd()), "result.json", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, errors.New("result artifact unavailable")
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		closeErr := unix.Close(fd)
		unlinkErr := unix.Unlinkat(int(dir.Fd()), "result.json", 0)
		if closeErr != nil || unlinkErr != nil {
			return nil, errors.New("result artifact unavailable")
		}
		return nil, errors.New("result artifact unavailable")
	}
	return os.NewFile(uintptr(fd), "restore-drill-result"), nil
}

func finishResultArtifact(file *os.File, result Result) error {
	data, err := json.Marshal(result)
	if err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return errors.New("result artifact unavailable")
		}
		return errors.New("result artifact unavailable")
	}
	data = append(data, '\n')
	if _, err := file.WriteAt(data, 0); err != nil {
		clear(data)
		if closeErr := file.Close(); closeErr != nil {
			return errors.New("result artifact unavailable")
		}
		return errors.New("result artifact unavailable")
	}
	if err := file.Truncate(int64(len(data))); err != nil {
		clear(data)
		if closeErr := file.Close(); closeErr != nil {
			return errors.New("result artifact unavailable")
		}
		return errors.New("result artifact unavailable")
	}
	clear(data)
	if err := file.Sync(); err != nil {
		if closeErr := file.Close(); closeErr != nil {
			return errors.New("result artifact unavailable")
		}
		return errors.New("result artifact unavailable")
	}
	if err := file.Close(); err != nil {
		return errors.New("result artifact unavailable")
	}
	return nil
}

type memorySession struct {
	mutex sync.Mutex
	data  []byte
}

func (storage *memorySession) LoadSession(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	storage.mutex.Lock()
	defer storage.mutex.Unlock()
	if len(storage.data) == 0 {
		return nil, session.ErrNotFound
	}
	return append([]byte(nil), storage.data...), nil
}

func (storage *memorySession) StoreSession(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	storage.mutex.Lock()
	defer storage.mutex.Unlock()
	clear(storage.data)
	storage.data = append(storage.data[:0], data...)
	return nil
}

func (storage *memorySession) clear() {
	storage.mutex.Lock()
	defer storage.mutex.Unlock()
	clear(storage.data)
	storage.data = nil
}
