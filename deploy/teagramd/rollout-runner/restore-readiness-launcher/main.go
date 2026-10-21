package main

import (
	"errors"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	dsnFile = "/run/secrets/postgres.dsn"
	server  = "/run/app/teagramd"
)

var databasePassword = regexp.MustCompile(`^[0-9a-f]{64}$`)

var allowedEnvironment = map[string]struct{}{
	"TG_POSTGRES_DSN_FILE":             {},
	"TG_AUTHKEY_ENC_KEY_FILE":          {},
	"TG_RSA_KEY_PATH":                  {},
	"TG_LISTEN_ADDR":                   {},
	"TG_ADVERTISE_ADDR":                {},
	"TG_WEBSOCKET_LISTEN_ADDR":         {},
	"TG_REGISTRATION":                  {},
	"TG_PUBLIC_REGISTRATION":           {},
	"TG_LOG_LOGIN_CODES":               {},
	"TG_PUBLIC_LINK_PREFIX":            {},
	"TG_BLOB_DIR":                      {},
	"TG_MEDIA_ERASURE_DESTRUCTIVE":     {},
	"TG_MEDIA_ERASURE_INTERVAL_MIN":    {},
	"TG_MEDIA_ERASURE_INTERVAL_MAX":    {},
	"TG_MEDIA_ERASURE_REPORT_INTERVAL": {},
	"TG_BLOB_SCAN_REPORT_INTERVAL":     {},
}

func main() {
	if err := run(); err != nil {
		os.Exit(1)
	}
}

func run() error {
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			return errors.New("invalid environment")
		}
		if _, allowed := allowedEnvironment[name]; !allowed {
			return errors.New("unexpected environment")
		}
	}

	dsn, err := readDatabaseDSN(dsnFile)
	if err != nil {
		return err
	}
	if err := os.Setenv("TG_POSTGRES_DSN", dsn); err != nil {
		return errors.New("set database configuration")
	}
	if err := os.Unsetenv("TG_POSTGRES_DSN_FILE"); err != nil {
		return errors.New("clear database configuration path")
	}
	return unix.Exec(server, []string{server, "serve"}, os.Environ())
}

func readDatabaseDSN(path string) (dsn string, resultErr error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", errors.New("open database configuration")
	}
	file := os.NewFile(uintptr(fd), path)
	defer func() {
		if err := file.Close(); err != nil {
			dsn = ""
			resultErr = errors.New("close database configuration")
		}
	}()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > 256 {
		return "", errors.New("invalid database configuration file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) {
		return "", errors.New("invalid database configuration owner")
	}
	contents, err := io.ReadAll(io.LimitReader(file, 257))
	if err != nil {
		return "", errors.New("read database configuration")
	}
	if len(contents) > 256 {
		return "", errors.New("invalid database configuration size")
	}
	dsn = strings.TrimSuffix(string(contents), "\n")
	if strings.ContainsAny(dsn, "\r\n\x00") || !validDatabaseDSN(dsn) {
		return "", errors.New("invalid database configuration")
	}
	return dsn, nil
}

func validDatabaseDSN(dsn string) bool {
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Scheme != "postgres" || parsed.User == nil ||
		parsed.User.Username() != "postgres" || parsed.Path != "/teagram" ||
		parsed.Host != "" || parsed.Fragment != "" || parsed.RawQuery == "" {
		return false
	}
	password, ok := parsed.User.Password()
	if !ok || !databasePassword.MatchString(password) {
		return false
	}
	query := parsed.Query()
	return len(query) == 2 && query.Get("host") == "/run/postgres" &&
		query.Get("sslmode") == "disable"
}
