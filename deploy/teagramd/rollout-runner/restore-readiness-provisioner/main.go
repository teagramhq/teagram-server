package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	serverPath   = "/run/app/teagramd"
	dsnPath      = "/run/secrets/postgres.dsn"
	authKeyPath  = "/run/secrets/authkey.hex"
	usernamePath = "/run/secrets/username"
	passwordPath = "/run/secrets/password"
)

var (
	dsnPattern          = regexp.MustCompile(`^postgres://postgres:[0-9a-f]{64}@127\.0\.0\.1:5432/teagram\?sslmode=disable$`)
	usernamePattern     = regexp.MustCompile(`^fixture_[a-f0-9]{16}$`)
	passwordPattern     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	createUserPattern   = regexp.MustCompile(`^User created: (fixture_[a-f0-9]{16}) \(user id: ([1-9][0-9]*)\)\n$`)
	authKeyPattern      = regexp.MustCompile(`^[a-f0-9]{64}$`)
	maxAdminOutputBytes = 64 * 1024
)

type provisionResult struct {
	SchemaVersion int    `json:"schema_version"`
	Category      string `json:"category"`
	UserID        int64  `json:"user_id,omitempty"`
	Username      string `json:"username,omitempty"`
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result := run(ctx)
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		os.Exit(1)
	}
	if result.Category != "provisioned" {
		os.Exit(1)
	}
}

func run(ctx context.Context) provisionResult {
	result := provisionResult{SchemaVersion: 1, Category: "provisioning_failure"}
	if os.Geteuid() != 0 || len(os.Environ()) != 0 {
		return result
	}
	dsn, err := readProtected(dsnPath, 0o400, 256)
	if err != nil || !dsnPattern.Match(bytes.TrimSuffix(dsn, []byte("\n"))) {
		clear(dsn)
		return result
	}
	defer clear(dsn)
	dsn = bytes.TrimSuffix(dsn, []byte("\n"))
	authKey, err := readProtected(authKeyPath, 0o400, 128)
	if err != nil || !authKeyPattern.Match(bytes.TrimSpace(authKey)) {
		clear(authKey)
		return result
	}
	defer clear(authKey)
	keyBytes := make([]byte, hex.DecodedLen(len(bytes.TrimSpace(authKey))))
	decoded, decodeErr := hex.Decode(keyBytes, bytes.TrimSpace(authKey))
	if decodeErr != nil || decoded != 32 {
		clear(keyBytes)
		return result
	}
	clear(keyBytes)
	username, err := readProtected(usernamePath, 0o400, 64)
	if err != nil || !usernamePattern.Match(bytes.TrimSuffix(username, []byte("\n"))) {
		clear(username)
		return result
	}
	defer clear(username)
	username = bytes.TrimSuffix(username, []byte("\n"))
	password, err := readProtected(passwordPath, 0o400, 128)
	passwordValue := bytes.TrimSuffix(password, []byte("\n"))
	if err != nil || !passwordPattern.Match(passwordValue) {
		clear(password)
		return result
	}
	defer clear(password)
	passwordLine := append(append([]byte(nil), passwordValue...), '\n')
	defer clear(passwordLine)

	command := exec.CommandContext(ctx, serverPath, "admin", "create-user", "--username", string(username)) // #nosec G204 -- fixed teagramd command with a validated synthetic username.
	command.Env = []string{
		"TG_POSTGRES_DSN=" + string(dsn),
		"TG_AUTHKEY_ENC_KEY_FILE=" + authKeyPath,
		"TG_AUTHKEY_ENC_KEY=",
		"TG_REGISTRATION=closed",
	}
	command.Stdin = bytes.NewReader(passwordLine)
	var stdout, stderr boundedBuffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	runErr := command.Run()
	if runErr != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
			result.Category = "timeout"
		} else if bytes.Contains(stderr.Bytes(), []byte("username occupied")) {
			result.Category = "identity_collision"
		}
		return result
	}
	if stdout.Len() != 0 {
		return result
	}
	userID, ok := parseCreateUserConfirmation(stderr.Bytes(), username)
	if !ok {
		return result
	}
	return provisionResult{SchemaVersion: 1, Category: "provisioned", UserID: userID, Username: string(username)}
}

func parseCreateUserConfirmation(stderr, username []byte) (int64, bool) {
	match := createUserPattern.FindSubmatch(stderr)
	if len(match) != 3 || !bytes.Equal(match[1], username) {
		return 0, false
	}
	userID, err := strconv.ParseInt(string(match[2]), 10, 64)
	return userID, err == nil && userID > 0
}

type boundedBuffer struct {
	bytes.Buffer
	tooLarge bool
}

func (buffer *boundedBuffer) Write(data []byte) (int, error) {
	if buffer.Len()+len(data) > maxAdminOutputBytes {
		buffer.tooLarge = true
		return 0, errors.New("admin output too large")
	}
	return buffer.Buffer.Write(data)
}

func readProtected(path string, mode os.FileMode, maxBytes int) (contents []byte, resultErr error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("open protected input")
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() {
		if err := file.Close(); err != nil {
			clear(contents)
			contents = nil
			resultErr = errors.New("close protected input")
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != mode || info.Size() > int64(maxBytes) {
		return nil, errors.New("invalid protected input")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("invalid protected input owner")
	}
	contents, err = io.ReadAll(io.LimitReader(file, int64(maxBytes+1)))
	if err != nil {
		return nil, errors.New("read protected input")
	}
	if len(contents) > maxBytes || strings.ContainsAny(string(contents), "\x00\r") {
		clear(contents)
		return nil, errors.New("protected input too large")
	}
	return contents, nil
}
