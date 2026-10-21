package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
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
	// #nosec G101 -- a protected input path, not a credential.
	passwordPath = "/run/secrets/password"
	rsaKeyPath   = "/run/secrets/server-key.pem"
)

var (
	dsnPassword = regexp.MustCompile(`^[0-9a-f]{64}$`)
	usernameRE  = regexp.MustCompile(`^fixture_[a-f0-9]{16}$`)
	authKeyRE   = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

func main() {
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	if os.Geteuid() != 0 || len(os.Environ()) != 0 {
		return errors.New("unexpected environment")
	}
	dsnBytes, err := readProtected(dsnPath, 0o400, 256)
	if err != nil || !validDSN(string(dsnBytes)) {
		clear(dsnBytes)
		return errors.New("invalid database configuration")
	}
	defer clear(dsnBytes)
	key, err := readProtected(authKeyPath, 0o400, 128)
	if err != nil || !authKeyRE.MatchString(strings.TrimSuffix(string(key), "\n")) {
		clear(key)
		return errors.New("invalid encryption key")
	}
	defer clear(key)
	username, err := readProtected(usernamePath, 0o400, 64)
	if err != nil || !usernameRE.MatchString(strings.TrimSuffix(string(username), "\n")) {
		clear(username)
		return errors.New("invalid synthetic identity")
	}
	defer clear(username)
	password, err := readProtected(passwordPath, 0o400, 128)
	if err != nil || strings.TrimSpace(string(password)) == "" || bytes.ContainsAny(password, "\x00\r\n") {
		clear(password)
		return errors.New("invalid synthetic credential")
	}
	defer clear(password)
	input := bytes.TrimSuffix(password, []byte("\n"))
	input = append(input, '\n')
	defer clear(input)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, serverPath, "admin", "create-user", "--username", strings.TrimSuffix(string(username), "\n")) // #nosec G204 -- fixed binary; the generated synthetic handle is validated above.
	command.Env = []string{
		"TG_POSTGRES_DSN=" + string(dsnBytes),
		"TG_AUTHKEY_ENC_KEY_FILE=" + authKeyPath,
		"TG_RSA_KEY_PATH=" + rsaKeyPath,
		"TG_REGISTRATION=closed",
		"TG_LOG_LOGIN_CODES=false",
	}
	command.Stdin = bytes.NewReader(input)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return errors.New("synthetic fixture account creation failed")
	}
	return nil
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
	uid, uidOK := effectiveUID()
	if !ok || !uidOK || stat.Nlink != 1 || stat.Uid != uid {
		return nil, errors.New("invalid protected input owner")
	}
	contents, err = io.ReadAll(io.LimitReader(file, int64(maxBytes+1)))
	if err != nil {
		return nil, errors.New("read protected input")
	}
	if len(contents) > maxBytes {
		clear(contents)
		return nil, errors.New("protected input too large")
	}
	return contents, nil
}

func effectiveUID() (uint32, bool) {
	uid := os.Geteuid()
	if uid < 0 {
		return 0, false
	}
	value := uint64(uid)
	if value > uint64(^uint32(0)) {
		return 0, false
	}
	return uint32(value), true
}

func validDSN(dsn string) bool {
	const prefix = "postgres://postgres:"
	if !strings.HasPrefix(dsn, prefix) || len(dsn) <= len(prefix)+64 {
		return false
	}
	password := dsn[len(prefix) : len(prefix)+64]
	return dsnPassword.MatchString(password) &&
		dsn[len(prefix)+64:] == "@/teagram?host=/run/postgres&sslmode=disable"
}
