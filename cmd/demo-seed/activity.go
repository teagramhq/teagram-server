package main

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const (
	activityStateDir        = "/var/lib/teagram-demo-activity"
	activitySessionFileName = "session.json"
	activityRateFileName    = "rate.json"
	activityLockFileName    = "activity.lock"
)

var errActivitySessionInvalid = errors.New("activity session is invalid")

var activityCorpus = []string{
	"The river path is quiet where the willows bend toward the water.",
	"A pale strip of sunlight reached the footbridge this afternoon.",
	"The gravel loop feels especially calm after a light rain.",
	"I spotted a new patch of tiny blue flowers beside the trail.",
	"The clouds opened just long enough to brighten the hillside.",
	"A few leaves gathered in a neat line along the garden wall.",
	"The old map looks different when the evening light turns gold.",
	"There is a gentle breeze moving through the riverside reeds.",
}

type activityMessageSender interface {
	MessagesSendMessage(context.Context, *tg.MessagesSendMessageRequest) (tg.UpdatesClass, error)
}

type activityRateState struct {
	Actions      []time.Time `json:"actions"`
	BlockedUntil *time.Time  `json:"blocked_until,omitempty"`
}

type activityRateLimiter struct {
	path        string
	maxPerHour  int
	minInterval time.Duration
	now         func() time.Time
}

type activityTarget struct {
	slug       string
	username   string
	title      string
	channelID  int64
	accessHash int64
	megagroup  bool
}

func (target activityTarget) peer() tg.InputPeerClass {
	return &tg.InputPeerChannel{ChannelID: target.channelID, AccessHash: target.accessHash}
}

func activitySpaceBySlug(slug string) (spaceSpec, bool) {
	for _, spec := range publicSpaces {
		if spec.slug == slug && spec.megagroup {
			return spec, true
		}
	}
	return spaceSpec{}, false
}

func seededActivityAccount() (accountSpec, bool) {
	var found accountSpec
	count := 0
	for _, account := range seedAccounts {
		if account.activity {
			found = account
			count++
		}
	}
	return found, count == 1 && !found.owner
}

func resolveActivityTargetFromDialogs(slug string, dialogs tg.MessagesDialogsClass) (activityTarget, error) {
	spec, ok := activitySpaceBySlug(slug)
	if !ok {
		return activityTarget{}, errors.New("activity target is not a seeded megagroup")
	}

	var chats []tg.ChatClass
	switch result := dialogs.(type) {
	case *tg.MessagesDialogs:
		chats = result.Chats
	case *tg.MessagesDialogsSlice:
		chats = result.Chats
	default:
		return activityTarget{}, errors.New("activity target is missing from joined dialogs")
	}

	for _, chat := range chats {
		channel, ok := chat.(*tg.Channel)
		if !ok || channel.Username != spec.username {
			continue
		}
		if channel.ID <= 0 || channel.AccessHash == 0 || channel.Title != spec.title || !channel.Megagroup || channel.Broadcast || channel.Left {
			return activityTarget{}, errors.New("resolved activity target does not match the seeded public megagroup")
		}
		return activityTarget{
			slug:       spec.slug,
			username:   channel.Username,
			title:      channel.Title,
			channelID:  channel.ID,
			accessHash: channel.AccessHash,
			megagroup:  channel.Megagroup,
		}, nil
	}
	return activityTarget{}, errors.New("activity target is missing from joined dialogs")
}

func sendActivityMessage(ctx context.Context, api activityMessageSender, target activityTarget, peer tg.InputPeerClass, message string, randomID int64, limiter activityRateLimiter) error {
	if err := target.validate(); err != nil {
		return err
	}
	channel, ok := peer.(*tg.InputPeerChannel)
	if !ok || channel.ChannelID != target.channelID || channel.AccessHash != target.accessHash {
		return errors.New("activity peer is not the resolved seeded megagroup")
	}
	if !isActivityCorpusMessage(message) || strings.Contains(message, "@") {
		return errors.New("activity message is not fixed bundled content")
	}
	if randomID == 0 {
		return errors.New("activity message identifier is invalid")
	}
	if _, err := api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Peer:     channel,
		Message:  message,
		RandomID: randomID,
	}); err != nil {
		if isInvalidActivitySession(err) {
			return errActivitySessionInvalid
		}
		if waitErr, ok := waitActivityFlood(ctx, err, limiter); ok {
			return waitErr
		}
		return errors.New("activity API send failed")
	}
	return nil
}

func (target activityTarget) validate() error {
	spec, ok := activitySpaceBySlug(target.slug)
	if !ok || !target.megagroup || target.channelID <= 0 || target.accessHash == 0 || target.username != spec.username || target.title != spec.title {
		return errors.New("activity target is not a resolved seeded megagroup")
	}
	return nil
}

func isActivityCorpusMessage(message string) bool {
	return slices.Contains(activityCorpus, message)
}

func readActivityPasswordAt(path string, account accountSpec) (string, error) {
	if !account.activity || account.owner || account.loginID == "" {
		return "", errors.New("activity credential account is invalid")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.New("activity credential inventory is unavailable")
	}
	if !isOwnedByCurrentUser(info) || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || info.Size() > 64*1024 {
		return "", errors.New("activity credential inventory must be an owned regular mode 0400 file")
	}
	data, err := os.ReadFile(path) // #nosec G304 -- path is the fixed systemd credential file; tests use a private temporary file.
	if err != nil {
		return "", errors.New("activity credential inventory is unavailable")
	}
	var inventory map[string]string
	if err := json.Unmarshal(data, &inventory); err != nil {
		return "", errors.New("activity credential inventory is invalid")
	}
	password, ok := inventory[account.loginID]
	if !ok || len(password) != 64 {
		return "", errors.New("activity account credential is missing or invalid")
	}
	if _, err := hex.DecodeString(password); err != nil {
		return "", errors.New("activity account credential is missing or invalid")
	}
	return password, nil
}

func runDemoActivity(ctx context.Context, cfg apiConfig, options seedOptions, account accountSpec, password string) (result error) {
	if _, ok := activitySpaceBySlug(options.activityTarget); !ok || options.maxActionsHour < 1 || options.maxActionsHour > 6 || options.minActivityGap < 5*time.Minute {
		return errors.New("activity configuration is outside the safety bounds")
	}
	if !account.activity || account.owner {
		return errors.New("activity account is not the dedicated seeded account")
	}
	if err := validateActivityStateDir(activityStateDir); err != nil {
		return err
	}
	lock, err := acquireActivityLock(activityStateDir)
	if err != nil {
		return err
	}
	defer func() {
		result = errors.Join(result, releaseActivityLock(lock))
	}()

	limiter := activityRateLimiter{
		path:        filepath.Join(activityStateDir, activityRateFileName),
		maxPerHour:  options.maxActionsHour,
		minInterval: options.minActivityGap,
		now:         time.Now,
	}
	allowed, err := limiter.reserve()
	if err != nil {
		return err
	}
	if !allowed {
		fmt.Println("demo activity: rate limit held")
		return nil
	}
	if err := runActivityClient(ctx, cfg, options, account, password, limiter); err != nil {
		return err
	}
	fmt.Println("demo activity: posted")
	return nil
}

func validateActivityStateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("activity state directory is unavailable")
	}
	if !isOwnedByCurrentUser(info) || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("activity state directory must be an owned mode 0700 directory")
	}
	return nil
}

func acquireActivityLock(stateDir string) (*os.File, error) {
	path := filepath.Join(stateDir, activityLockFileName)
	info, err := os.Lstat(path)
	if err == nil && (!isOwnedByCurrentUser(info) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600) {
		return nil, errors.New("activity lock must be a regular mode 0600 file")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("activity lock is unavailable")
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600) // #nosec G304 -- path is under the validated private activity state directory.
	if err != nil {
		return nil, errors.New("activity lock is unavailable")
	}
	info, err = file.Stat()
	if err != nil || !isOwnedByCurrentUser(info) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		closeErr := file.Close()
		return nil, errors.Join(errors.New("activity lock must be an owned regular mode 0600 file"), closeErr)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		closeErr := file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errors.Join(errors.New("activity is already running"), closeErr)
		}
		return nil, errors.Join(errors.New("activity lock is unavailable"), closeErr)
	}
	return file, nil
}

func releaseActivityLock(file *os.File) error {
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}

func runActivityClient(ctx context.Context, cfg apiConfig, options seedOptions, account accountSpec, password string, limiter activityRateLimiter) error {
	sessionPath := filepath.Join(activityStateDir, activitySessionFileName)
	exists, err := validateActivitySessionFile(sessionPath)
	if err != nil {
		return err
	}
	if exists {
		err := runActivitySession(ctx, cfg, options, account, password, sessionPath, false, limiter)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errActivitySessionInvalid) {
			return err
		}
		if err := os.Remove(sessionPath); err != nil {
			return errors.New("could not clear invalid activity session")
		}
		if err := sleepActivity(ctx, 5*time.Second); err != nil {
			return errors.New("activity session recovery was interrupted")
		}
	}
	return runActivitySession(ctx, cfg, options, account, password, sessionPath, true, limiter)
}

func validateActivitySessionFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("activity session file is unavailable")
	}
	if !isOwnedByCurrentUser(info) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return false, errors.New("activity session must be an owned regular mode 0600 file")
	}
	return true, nil
}

func runActivitySession(ctx context.Context, cfg apiConfig, options seedOptions, account accountSpec, password, sessionPath string, login bool, limiter activityRateLimiter) error {
	return runActivitySessionUsingRunner(ctx, cfg, options, account, password, sessionPath, login, limiter, runTelegramClient)
}

func runActivitySessionUsingRunner(ctx context.Context, cfg apiConfig, options seedOptions, account accountSpec, password, sessionPath string, login bool, limiter activityRateLimiter, runner telegramClientRunner) error {
	clientOptions, err := accountClientOptions(cfg, options.endpoint, &session.FileStorage{Path: sessionPath})
	if err != nil {
		return fmt.Errorf("resolve activity endpoint: %w", err)
	}
	err = runner(ctx, clientOptions, func(runCtx context.Context, client *telegram.Client) error {
		api := client.API()
		if login {
			passwordCopy := password
			if err := authenticate(runCtx, api, account, &passwordCopy, nil); err != nil {
				if waitErr, ok := waitActivityFlood(runCtx, err, limiter); ok {
					return waitErr
				}
				return errors.New("activity sign-in failed")
			}
		} else {
			users, err := api.UsersGetUsers(runCtx, []tg.InputUserClass{&tg.InputUserSelf{}})
			if err != nil {
				if isInvalidActivitySession(err) {
					return errActivitySessionInvalid
				}
				if waitErr, ok := waitActivityFlood(runCtx, err, limiter); ok {
					return waitErr
				}
				return errors.New("activity session check failed")
			}
			if !isActivitySessionUser(users, account) {
				return errors.New("activity session does not belong to the seeded activity account")
			}
		}

		dialogs, err := api.MessagesGetDialogs(runCtx, &tg.MessagesGetDialogsRequest{Limit: 100})
		if err != nil {
			if isInvalidActivitySession(err) {
				return errActivitySessionInvalid
			}
			if waitErr, ok := waitActivityFlood(runCtx, err, limiter); ok {
				return waitErr
			}
			return errors.New("activity group lookup failed")
		}
		target, err := resolveActivityTargetFromDialogs(options.activityTarget, dialogs)
		if err != nil {
			return err
		}
		message, randomID, err := randomActivityPost()
		if err != nil {
			return errors.New("could not select bundled activity content")
		}
		return sendActivityMessage(runCtx, api, target, target.peer(), message, randomID, limiter)
	})
	if err == nil || errors.Is(err, errActivitySessionInvalid) {
		return err
	}
	if isInvalidActivitySession(err) {
		return errActivitySessionInvalid
	}
	if waitErr, ok := waitActivityFlood(ctx, err, limiter); ok {
		return waitErr
	}
	return err
}

func isActivitySessionUser(users []tg.UserClass, account accountSpec) bool {
	if !account.activity || account.owner || account.username == "" || len(users) != 1 {
		return false
	}
	user, ok := users[0].(*tg.User)
	return ok && user.Username == account.username
}

func waitActivityFlood(ctx context.Context, err error, limiter activityRateLimiter) (error, bool) {
	return waitActivityFloodWithSleep(ctx, err, limiter, sleepActivity)
}

func waitActivityFloodWithSleep(ctx context.Context, err error, limiter activityRateLimiter, sleep func(context.Context, time.Duration) error) (error, bool) {
	wait, ok := activityFloodWait(err)
	if !ok {
		return nil, false
	}
	persistErr := limiter.blockFor(wait)
	if sleepErr := sleep(context.WithoutCancel(ctx), wait); sleepErr != nil {
		return errors.New("activity flood wait was interrupted"), true
	}
	if persistErr != nil {
		return errors.New("activity flood wait could not be persisted"), true
	}
	return errors.New("activity API requested a flood wait; request was not retried"), true
}

func activityFloodWait(err error) (time.Duration, bool) {
	rpc, ok := tgerr.As(err)
	if !ok || rpc.Type != "FLOOD_WAIT" || rpc.Argument <= 0 {
		return 0, false
	}
	seconds := int64(rpc.Argument)
	maxSeconds := int64(math.MaxInt64) / int64(time.Second)
	wait := time.Duration(math.MaxInt64)
	if seconds <= maxSeconds {
		wait = time.Duration(seconds) * time.Second
	}
	return wait, true
}

func isInvalidActivitySession(err error) bool {
	return tgerr.Is(err, "AUTH_KEY_UNREGISTERED", "AUTH_KEY_INVALID", "SESSION_REVOKED", "SESSION_PASSWORD_NEEDED")
}

func randomActivityPost() (string, int64, error) {
	messageIndex, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(len(activityCorpus))))
	if err != nil {
		return "", 0, err
	}
	identifier, err := cryptorand.Int(cryptorand.Reader, big.NewInt(math.MaxInt64))
	if err != nil {
		return "", 0, err
	}
	randomID := identifier.Int64()
	if randomID == 0 {
		randomID = 1
	}
	return activityCorpus[messageIndex.Int64()], randomID, nil
}

func sleepActivity(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func isOwnedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(stat.Uid) == int64(os.Geteuid())
}

func (limiter activityRateLimiter) reserve() (bool, error) {
	if limiter.maxPerHour < 1 || limiter.maxPerHour > 6 || limiter.minInterval < 5*time.Minute {
		return false, errors.New("activity rate limit is outside the hard safety bounds")
	}
	if limiter.now == nil {
		return false, errors.New("activity rate limit clock is missing")
	}
	now := limiter.now().UTC()
	state, err := readActivityRateState(limiter.path)
	if err != nil {
		return false, err
	}
	if state.BlockedUntil != nil && now.Before(*state.BlockedUntil) {
		return false, nil
	}
	cutoff := now.Add(-time.Hour)
	kept := make([]time.Time, 0, len(state.Actions))
	for _, action := range state.Actions {
		if action.After(cutoff) {
			kept = append(kept, action)
		}
	}
	if len(kept) >= limiter.maxPerHour {
		return false, nil
	}
	if len(kept) > 0 && now.Sub(kept[len(kept)-1]) < limiter.minInterval {
		return false, nil
	}
	kept = append(kept, now)
	if err := writeActivityRateState(limiter.path, activityRateState{Actions: kept}); err != nil {
		return false, err
	}
	return true, nil
}

func (limiter activityRateLimiter) blockFor(wait time.Duration) error {
	if limiter.path == "" || limiter.now == nil || wait <= 0 {
		return errors.New("activity flood wait state is unavailable")
	}
	state, err := readActivityRateState(limiter.path)
	if err != nil {
		return err
	}
	until := limiter.now().UTC().Add(wait)
	if state.BlockedUntil == nil || until.After(*state.BlockedUntil) {
		state.BlockedUntil = &until
	}
	return writeActivityRateState(limiter.path, state)
}

func readActivityRateState(path string) (activityRateState, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return activityRateState{}, nil
	}
	if err != nil {
		return activityRateState{}, fmt.Errorf("inspect activity rate state: %w", err)
	}
	if !isOwnedByCurrentUser(info) || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return activityRateState{}, errors.New("activity rate state must be an owned regular mode 0600 file")
	}
	file, err := os.Open(path) // #nosec G304 -- rate state is inside the private activity state directory.
	if err != nil {
		return activityRateState{}, fmt.Errorf("open activity rate state: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	closeErr := file.Close()
	if err != nil {
		return activityRateState{}, errors.Join(fmt.Errorf("read activity rate state: %w", err), closeErr)
	}
	if closeErr != nil {
		return activityRateState{}, fmt.Errorf("close activity rate state: %w", closeErr)
	}
	if len(data) > 4096 {
		return activityRateState{}, errors.New("activity rate state is too large")
	}
	var state activityRateState
	if err := json.Unmarshal(data, &state); err != nil {
		return activityRateState{}, errors.New("activity rate state is invalid")
	}
	return state, nil
}

func writeActivityRateState(path string, state activityRateState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encode activity rate state: %w", err)
	}
	dir := filepath.Dir(path)
	temporary, err := os.CreateTemp(dir, ".activity-rate-*")
	if err != nil {
		return fmt.Errorf("create activity rate state: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() error {
		return os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0600); err != nil {
		closeErr := temporary.Close()
		return errors.Join(fmt.Errorf("protect activity rate state: %w", err), closeErr, cleanup())
	}
	if _, err := temporary.Write(data); err != nil {
		closeErr := temporary.Close()
		return errors.Join(fmt.Errorf("write activity rate state: %w", err), closeErr, cleanup())
	}
	if err := temporary.Sync(); err != nil {
		closeErr := temporary.Close()
		return errors.Join(fmt.Errorf("sync activity rate state: %w", err), closeErr, cleanup())
	}
	if err := temporary.Close(); err != nil {
		return errors.Join(fmt.Errorf("close activity rate state: %w", err), cleanup())
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return errors.Join(fmt.Errorf("publish activity rate state: %w", err), cleanup())
	}
	directory, err := os.Open(dir) // #nosec G304 -- dir is derived from the private activity rate state path.
	if err != nil {
		return fmt.Errorf("open activity state directory for sync: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}
