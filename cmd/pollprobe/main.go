// Command pollprobe runs the fixed, synthetic-only poll lifecycle scenario.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gotd/td/tgerr"
)

const (
	probeDCID             = 2
	probeAppID            = 1
	probeAppHash          = "hash"
	probeDeadline         = 3 * time.Minute
	logoutDeadline        = 8 * time.Second
	maxCredentialBytes    = 4096
	maxPublicKeyFileBytes = 16 * 1024
	maxDifferencePages    = 8
	maxHistoryMessages    = 100
	maxPollVotesPage      = 1
)

var probeUsernames = [4]string{"synthpoll_a", "synthpoll_b", "synthpoll_c", "synthpoll_d"}

type endpoint struct {
	host string
	port int
}

type accountCredential struct {
	username string
	password []byte
}

type probeConfig struct {
	endpoint  endpoint
	publicKey *rsa.PublicKey
	creds     [4]accountCredential
}

type probeError struct {
	assertion string
	errorCode string
	rpcError  string
	rpcCode   int
	details   []string
}

func (f *probeError) Error() string { return "poll probe failed" }

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	cfg, err := parseOptions(args)
	if err != nil {
		failure := &probeError{assertion: "configuration_validated", errorCode: "LOCAL_INPUT_INVALID"}
		if writeErr := writeFailure(stderr, failure); writeErr != nil {
			return &probeError{assertion: "probe_output", errorCode: "LOCAL_OUTPUT_ERROR"}
		}
		return failure
	}
	defer clearCredentials(&cfg.creds)

	ctx, cancel := context.WithTimeout(context.Background(), probeDeadline)
	defer cancel()
	p := newProbe(cfg, stdout)
	if err := p.pass("configuration_validated"); err != nil {
		return err
	}
	scenarioErr := p.execute(ctx)
	logoutErr := p.logoutAll()
	shutdownErr := p.stopClients()
	if scenarioErr != nil {
		if writeErr := writeFailure(stderr, asProbeFailure("scenario_complete", scenarioErr)); writeErr != nil {
			scenarioErr = failure("probe_output", "LOCAL_OUTPUT_ERROR")
		}
	}
	if logoutErr != nil {
		if writeErr := writeFailure(stderr, asProbeFailure("logout_sessions", logoutErr)); writeErr != nil {
			logoutErr = failure("probe_output", "LOCAL_OUTPUT_ERROR")
		}
	}
	if shutdownErr != nil {
		if writeErr := writeFailure(stderr, asProbeFailure("client_shutdown", shutdownErr)); writeErr != nil {
			shutdownErr = failure("probe_output", "LOCAL_OUTPUT_ERROR")
		}
	}
	return errors.Join(scenarioErr, logoutErr, shutdownErr)
}

func parseOptions(args []string) (probeConfig, error) {
	var cfg probeConfig
	var endpointValue, publicKeyPath, keyID, credentialsDir string
	flags := flag.NewFlagSet("pollprobe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&endpointValue, "endpoint", "", "remote host:port")
	flags.StringVar(&publicKeyPath, "rsa-public-key", "", "trusted server RSA public key PEM")
	flags.StringVar(&keyID, "rsa-key-id", "", "trusted SHA-256 SPKI key ID")
	flags.StringVar(&credentialsDir, "credentials-dir", "", "protected directory containing the four fixed password files")
	if err := flags.Parse(args); err != nil || len(flags.Args()) != 0 || endpointValue == "" || publicKeyPath == "" || keyID == "" || credentialsDir == "" {
		return cfg, errors.New("invalid options")
	}

	parsedEndpoint, err := parseEndpoint(endpointValue)
	if err != nil {
		return cfg, err
	}
	publicKey, err := loadPublicKey(publicKeyPath, keyID)
	if err != nil {
		return cfg, err
	}
	credentials, err := loadCredentialFiles(credentialsDir)
	if err != nil {
		return cfg, err
	}
	cfg.endpoint = parsedEndpoint
	cfg.publicKey = publicKey
	cfg.creds = credentials
	return cfg, nil
}

func parseEndpoint(value string) (endpoint, error) {
	host, portText, err := net.SplitHostPort(value)
	if err != nil || host == "" {
		return endpoint{}, errors.New("invalid endpoint")
	}
	if strings.ContainsAny(host, " /\\%\t\r\n") {
		return endpoint{}, errors.New("invalid endpoint")
	}
	if net.ParseIP(host) == nil {
		if len(host) > 253 {
			return endpoint{}, errors.New("invalid endpoint")
		}
		for label := range strings.SplitSeq(host, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return endpoint{}, errors.New("invalid endpoint")
			}
			for _, char := range label {
				if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' {
					return endpoint{}, errors.New("invalid endpoint")
				}
			}
		}
	}
	port := 0
	for _, char := range portText {
		if char < '0' || char > '9' {
			return endpoint{}, errors.New("invalid endpoint")
		}
		port = port*10 + int(char-'0')
		if port > 65535 {
			return endpoint{}, errors.New("invalid endpoint")
		}
	}
	if port == 0 {
		return endpoint{}, errors.New("invalid endpoint")
	}
	return endpoint{host: host, port: port}, nil
}

func loadPublicKey(path, keyID string) (*rsa.PublicKey, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxPublicKeyFileBytes {
		return nil, errors.New("invalid trusted server key input")
	}
	file, err := os.Open(path) // #nosec G304 -- the operator selects the pinned public-key file; the file is checked and bounded below.
	if err != nil {
		return nil, errors.New("invalid trusted server key input")
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() || openedInfo.Size() <= 0 || openedInfo.Size() > maxPublicKeyFileBytes {
		if closeErr := file.Close(); closeErr != nil {
			return nil, errors.New("invalid trusted server key input")
		}
		return nil, errors.New("invalid trusted server key input")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxPublicKeyFileBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) == 0 || len(data) > maxPublicKeyFileBytes {
		clear(data)
		return nil, errors.New("invalid trusted server key input")
	}
	return parsePublicKey(data, keyID)
}

func parsePublicKey(data []byte, keyID string) (*rsa.PublicKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("invalid trusted server key input")
	}
	var publicKey *rsa.PublicKey
	switch block.Type {
	case "PUBLIC KEY":
		parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid trusted server key input")
		}
		var ok bool
		publicKey, ok = parsed.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("invalid trusted server key input")
		}
	case "RSA PUBLIC KEY":
		parsed, err := x509.ParsePKCS1PublicKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid trusted server key input")
		}
		publicKey = parsed
	default:
		return nil, errors.New("invalid trusted server key input")
	}
	if publicKey.N == nil || publicKey.N.BitLen() != 2048 || publicKey.E < 3 || publicKey.E%2 == 0 {
		return nil, errors.New("invalid trusted server key input")
	}
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return nil, errors.New("invalid trusted server key input")
	}
	want, err := parseKeyID(keyID)
	if err != nil {
		return nil, errors.New("invalid trusted server key ID")
	}
	digest := sha256.Sum256(der)
	if subtle.ConstantTimeCompare(want, digest[:]) != 1 {
		return nil, errors.New("trusted server key ID mismatch")
	}
	return publicKey, nil
}

func parseKeyID(value string) ([]byte, error) {
	compact := value
	if strings.Contains(value, "-") {
		parts := strings.Split(value, "-")
		if len(parts) != 16 {
			return nil, errors.New("invalid key ID")
		}
		for _, part := range parts {
			if len(part) != 4 {
				return nil, errors.New("invalid key ID")
			}
		}
		compact = strings.Join(parts, "")
	}
	decoded, err := hex.DecodeString(compact)
	if err != nil || len(decoded) != sha256.Size {
		return nil, errors.New("invalid key ID")
	}
	return decoded, nil
}

func loadCredentialFiles(dir string) (_ [4]accountCredential, err error) {
	var credentials [4]accountCredential
	info, statErr := os.Lstat(dir)
	if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !ownedByEffectiveUser(info) {
		return credentials, errors.New("invalid credential directory")
	}
	perms := info.Mode().Perm()
	if perms&0o077 != 0 || perms&0o500 != 0o500 {
		return credentials, errors.New("unsafe credential directory permissions")
	}
	entries, readDirErr := os.ReadDir(dir)
	if readDirErr != nil || len(entries) != len(probeUsernames) {
		return credentials, errors.New("invalid credential directory contents")
	}
	for _, entry := range entries {
		if !knownCredentialFile(entry.Name()) {
			return credentials, errors.New("invalid credential directory contents")
		}
	}
	defer func() {
		if err != nil {
			clearCredentials(&credentials)
		}
	}()
	for index, username := range probeUsernames {
		password, fileErr := readPasswordFile(filepath.Join(dir, username+".password"))
		if fileErr != nil {
			return credentials, fileErr
		}
		credentials[index] = accountCredential{username: username, password: password}
	}
	return credentials, nil
}

func knownCredentialFile(name string) bool {
	for _, username := range probeUsernames {
		if name == username+".password" {
			return true
		}
	}
	return false
}

func readPasswordFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || !ownedByEffectiveUser(info) || info.Size() <= 0 || info.Size() > maxCredentialBytes {
		return nil, errors.New("invalid credential file")
	}
	perms := info.Mode().Perm()
	if perms&0o077 != 0 || perms&^0o600 != 0 || perms&0o400 == 0 {
		return nil, errors.New("unsafe credential file permissions")
	}
	file, err := os.Open(path) // #nosec G304 -- path is a fixed credential name under the validated owner-only directory; identity and permissions are rechecked after open.
	if err != nil {
		return nil, errors.New("invalid credential file")
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() || !ownedByEffectiveUser(openedInfo) || openedInfo.Size() <= 0 || openedInfo.Size() > maxCredentialBytes || openedInfo.Mode().Perm()&0o077 != 0 || openedInfo.Mode().Perm()&^0o600 != 0 || openedInfo.Mode().Perm()&0o400 == 0 {
		closeErr := file.Close()
		if closeErr != nil {
			return nil, errors.New("invalid credential file")
		}
		return nil, errors.New("invalid credential file")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxCredentialBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) == 0 || len(data) > maxCredentialBytes {
		clear(data)
		return nil, errors.New("invalid credential file")
	}
	if bytes.HasSuffix(data, []byte("\r\n")) {
		data = data[:len(data)-2]
	} else if bytes.HasSuffix(data, []byte("\n")) {
		data = data[:len(data)-1]
	}
	if len(data) == 0 || bytes.ContainsAny(data, "\r\n\x00") {
		clear(data)
		return nil, errors.New("invalid credential file")
	}
	password := bytes.Clone(data)
	clear(data)
	return password, nil
}

func ownedByEffectiveUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}

func clearCredentials(credentials *[4]accountCredential) {
	for index := range credentials {
		clear(credentials[index].password)
		credentials[index].password = nil
	}
}

func writeFailure(writer io.Writer, failure *probeError) error {
	if failure == nil {
		return nil
	}
	if _, err := fmt.Fprintf(writer, "assertion=%s result=fail error_code=%s", safeAssertionName(failure.assertion), safeCode(failure.errorCode)); err != nil {
		return err
	}
	if failure.rpcError != "" {
		if _, err := fmt.Fprintf(writer, " rpc_error=%s rpc_code=%d", safeCode(failure.rpcError), failure.rpcCode); err != nil {
			return err
		}
	}
	for _, field := range failure.details {
		if !safeField(field) {
			continue
		}
		if _, err := fmt.Fprintf(writer, " %s", field); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(writer)
	return err
}

func safeAssertionName(value string) string {
	if value == "" {
		return "probe"
	}
	for _, char := range value {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return "probe"
		}
	}
	return value
}

func safeCode(value string) string {
	if value == "" {
		return "UNKNOWN"
	}
	for _, char := range value {
		if (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '_' {
			return "UNKNOWN"
		}
	}
	return value
}

func asProbeFailure(assertion string, err error) *probeError {
	if failure, ok := errors.AsType[*probeError](err); ok {
		return failure
	}
	if rpcErr, ok := errors.AsType[*tgerr.Error](err); ok {
		return &probeError{assertion: assertion, errorCode: "RPC_FAILED", rpcError: safeCode(rpcErr.Message), rpcCode: rpcErr.Code}
	}
	return &probeError{assertion: assertion, errorCode: "OPERATION_FAILED"}
}

func failure(assertion, errorCode string) *probeError {
	return &probeError{assertion: assertion, errorCode: errorCode}
}

func failureWithFields(assertion, errorCode string, fields ...string) *probeError {
	for _, field := range fields {
		if !safeField(field) {
			return failure(assertion, "INVALID_OUTPUT_FIELD")
		}
	}
	return &probeError{assertion: assertion, errorCode: errorCode, details: fields}
}

func rpcFailure(assertion string, err error) *probeError {
	return asProbeFailure(assertion, err)
}

func expectRPCError(assertion, wantName string, wantCode int, err error) *probeError {
	if err == nil {
		return failure(assertion, "EXPECTED_RPC_ERROR_MISSING")
	}
	rpcErr, ok := errors.AsType[*tgerr.Error](err)
	if !ok {
		return failure(assertion, "EXPECTED_RPC_ERROR_MISSING")
	}
	if rpcErr.Message != wantName || rpcErr.Code != wantCode {
		return rpcFailure(assertion, err)
	}
	return nil
}

func randomID() (int64, error) {
	var raw [8]byte
	if _, err := io.ReadFull(rand.Reader, raw[:]); err != nil {
		return 0, errors.New("random request ID unavailable")
	}
	id := int64(binary.LittleEndian.Uint64(raw[:]) & (1<<63 - 1))
	if id == 0 {
		id = 1
	}
	return id, nil
}
