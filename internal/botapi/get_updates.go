package botapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	maxGetUpdatesLimit     = 100
	maxGetUpdatesTimeout   = 50
	maxOpenGetUpdates      = 128
	maxGetUpdatesPages     = 10 // Bound a poll to 1,000 raw events, including reads after wakeups.
	getUpdatesWriteTimeout = 60 * time.Second
	// Keep one bounded query window after long-poll expiry for updates committed during the wait.
	getUpdatesFinalScanTimeout = 5 * time.Second
)

const getUpdatesConflictBody = "{\"ok\":false,\"error_code\":409,\"description\":\"Conflict: terminated by other getUpdates request\"}\n"

type getUpdatesStore interface {
	BotMessageEvents(context.Context, int64, int64, int) (store.BotMessageEventPage, error)
}

type getUpdatesParams struct {
	offset  *int64
	limit   int
	timeout int
}

type getUpdatesResponse struct {
	OK     bool           `json:"ok"`
	Result []botAPIUpdate `json:"result"`
}

type botAPIUpdate struct {
	UpdateID int64               `json:"update_id"`
	Message  botAPIUpdateMessage `json:"message"`
}

type botAPIUpdateMessage struct {
	MessageID int64            `json:"message_id"`
	From      botAPIUpdateUser `json:"from"`
	Chat      botAPIUpdateChat `json:"chat"`
	Date      int64            `json:"date"`
	Text      string           `json:"text"`
}

type botAPIUpdateUser struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type botAPIUpdateChat struct {
	ID        int64  `json:"id"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
	Type      string `json:"type"`
}

type updatePoll struct {
	ctx        context.Context
	cancel     context.CancelFunc
	superseded chan struct{}
	wake       chan struct{}
}

func parseGetUpdatesRequest(r *http.Request, body []byte) (getUpdatesParams, error) {
	if r.Method == http.MethodGet && len(body) != 0 {
		return getUpdatesParams{}, errors.New("getUpdates GET body is not supported")
	}
	if len(body) == 0 {
		return parseGetUpdatesValues(r.URL.Query())
	}
	if r.URL.RawQuery != "" {
		return getUpdatesParams{}, errors.New("getUpdates cannot mix query and body parameters")
	}
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return getUpdatesParams{}, errors.New("invalid getUpdates content type")
	}
	switch contentType {
	case "application/json":
		return parseJSONGetUpdates(body)
	case "application/x-www-form-urlencoded":
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return getUpdatesParams{}, errors.New("invalid getUpdates form")
		}
		return parseGetUpdatesValues(values)
	default:
		return getUpdatesParams{}, errors.New("unsupported getUpdates content type")
	}
}

func parseJSONGetUpdates(body []byte) (getUpdatesParams, error) {
	if !utf8.Valid(body) || !json.Valid(body) {
		return getUpdatesParams{}, errors.New("invalid getUpdates JSON")
	}
	var request struct {
		Offset         json.RawMessage `json:"offset"`
		Limit          json.RawMessage `json:"limit"`
		Timeout        json.RawMessage `json:"timeout"`
		AllowedUpdates json.RawMessage `json:"allowed_updates"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return getUpdatesParams{}, errors.New("invalid getUpdates JSON")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return getUpdatesParams{}, errors.New("trailing getUpdates JSON")
	}
	if len(request.AllowedUpdates) != 0 && string(request.AllowedUpdates) != "null" {
		var allowed []string
		if err := json.Unmarshal(request.AllowedUpdates, &allowed); err != nil || (len(allowed) != 0 && (len(allowed) != 1 || allowed[0] != "message")) {
			return getUpdatesParams{}, errors.New("getUpdates only supports message updates")
		}
	}
	params := defaultGetUpdatesParams()
	if len(request.Offset) != 0 {
		offset, err := parseJSONInteger(request.Offset)
		if err != nil || offset < 0 {
			return getUpdatesParams{}, errors.New("invalid getUpdates offset")
		}
		params.offset = &offset
	}
	if len(request.Limit) != 0 {
		limit, err := parseJSONInteger(request.Limit)
		if err != nil {
			return getUpdatesParams{}, errors.New("invalid getUpdates limit")
		}
		params.limit = clampGetUpdatesLimit(limit)
	}
	if len(request.Timeout) != 0 {
		timeout, err := parseJSONInteger(request.Timeout)
		if err != nil || timeout < 0 {
			return getUpdatesParams{}, errors.New("invalid getUpdates timeout")
		}
		params.timeout = clampGetUpdatesTimeout(timeout)
	}
	return params, nil
}

func parseJSONInteger(raw json.RawMessage) (int64, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value json.Number
	if err := decoder.Decode(&value); err != nil {
		return 0, err
	}
	return strconv.ParseInt(string(value), 10, 64)
}

func parseGetUpdatesValues(values url.Values) (getUpdatesParams, error) {
	for key := range values {
		if key != "offset" && key != "limit" && key != "timeout" && key != "allowed_updates" {
			return getUpdatesParams{}, fmt.Errorf("unsupported getUpdates field %q", key)
		}
		if len(values[key]) != 1 {
			return getUpdatesParams{}, fmt.Errorf("getUpdates field %q must occur once", key)
		}
	}
	params := defaultGetUpdatesParams()
	if value, ok := values["offset"]; ok {
		offset, err := strconv.ParseInt(value[0], 10, 64)
		if err != nil || offset < 0 {
			return getUpdatesParams{}, errors.New("invalid getUpdates offset")
		}
		params.offset = &offset
	}
	if value, ok := values["limit"]; ok {
		limit, err := strconv.ParseInt(value[0], 10, 64)
		if err != nil {
			return getUpdatesParams{}, errors.New("invalid getUpdates limit")
		}
		params.limit = clampGetUpdatesLimit(limit)
	}
	if value, ok := values["timeout"]; ok {
		timeout, err := strconv.ParseInt(value[0], 10, 64)
		if err != nil || timeout < 0 {
			return getUpdatesParams{}, errors.New("invalid getUpdates timeout")
		}
		params.timeout = clampGetUpdatesTimeout(timeout)
	}
	if value, ok := values["allowed_updates"]; ok && !validAllowedUpdates(value[0]) {
		return getUpdatesParams{}, errors.New("getUpdates only supports message updates")
	}
	return params, nil
}

func validAllowedUpdates(value string) bool {
	var allowed []string
	if err := json.Unmarshal([]byte(value), &allowed); err != nil {
		return false
	}
	return len(allowed) == 0 || (len(allowed) == 1 && allowed[0] == "message")
}

func defaultGetUpdatesParams() getUpdatesParams {
	return getUpdatesParams{limit: maxGetUpdatesLimit}
}

func clampGetUpdatesLimit(limit int64) int {
	switch {
	case limit < 1:
		return 1
	case limit > maxGetUpdatesLimit:
		return maxGetUpdatesLimit
	default:
		return int(limit)
	}
}

func clampGetUpdatesTimeout(timeout int64) int {
	if timeout > maxGetUpdatesTimeout {
		return maxGetUpdatesTimeout
	}
	return int(timeout)
}

// NotifyUpdates wakes the active poll for an owner after the update listener
// receives the existing tg_updates notification.
func (h *handler) NotifyUpdates(ownerID int64) {
	h.limiter.wakeUpdatePoll(ownerID)
}

func (h *handler) getUpdates(w http.ResponseWriter, r *http.Request, botID int64, method, token string, params getUpdatesParams) {
	st := h.store
	_, err := st.BotUpdateOffset(r.Context(), botID, token)
	if errors.Is(err, store.ErrBotTokenNotFound) {
		h.unauthorized(w, botID, method)
		return
	}
	if err != nil {
		h.log.Error("Bot API update offset lookup failed", "bot_id", botID, "method", method, "err", err)
		h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
		return
	}
	poll, ok := h.limiter.beginUpdatePoll(r.Context(), botID)
	if !ok {
		h.rateLimited(w, botID, method)
		return
	}
	defer h.limiter.endUpdatePoll(botID, poll)
	if poll.wasSuperseded() {
		h.updatePollConflict(w, botID, method)
		return
	}
	claimed, err := st.StartBotUpdatePoll(poll.ctx, botID, token)
	if errors.Is(err, store.ErrBotTokenNotFound) {
		h.unauthorized(w, botID, method)
		return
	}
	if err != nil {
		if poll.wasSuperseded() || errors.Is(err, store.ErrBotPollSuperseded) {
			h.updatePollConflict(w, botID, method)
			return
		}
		if r.Context().Err() != nil || poll.ctx.Err() != nil {
			return
		}
		h.log.Error("Bot API update poll claim failed", "bot_id", botID, "method", method, "err", err)
		h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
		return
	}
	if poll.wasSuperseded() {
		h.updatePollConflict(w, botID, method)
		return
	}
	effectiveOffset := claimed.Offset
	if params.offset != nil && *params.offset > effectiveOffset {
		effectiveOffset = *params.offset
	}

	readTimeout := time.Duration(maxGetUpdatesTimeout) * time.Second
	if params.timeout > 0 {
		readTimeout = time.Duration(params.timeout) * time.Second
	}
	readCtx, cancelRead := context.WithTimeout(poll.ctx, readTimeout)
	defer cancelRead()
	timedOut := false
	var updates []botAPIUpdate
	var nextOffset int64
	var readErr error
	pagesRemaining := maxGetUpdatesPages
pollLoop:
	for {
		if poll.wasSuperseded() {
			h.updatePollConflict(w, botID, method)
			return
		}
		if err := st.CheckBotUpdatePoll(poll.ctx, botID, token, claimed.Generation); err != nil {
			if errors.Is(err, store.ErrBotPollSuperseded) || poll.wasSuperseded() {
				h.updatePollConflict(w, botID, method)
				return
			}
			if errors.Is(err, store.ErrBotTokenNotFound) {
				h.unauthorized(w, botID, method)
				return
			}
			if r.Context().Err() != nil {
				return
			}
			h.log.Error("Bot API update poll ownership check failed", "bot_id", botID, "method", method, "err", err)
			h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
			return
		}
		var pagesRead int
		updates, nextOffset, pagesRead, readErr = h.readBotUpdates(readCtx, st, botID, effectiveOffset, params.limit, pagesRemaining)
		pagesRemaining -= pagesRead
		if readErr != nil {
			if errors.Is(readErr, context.DeadlineExceeded) && errors.Is(readCtx.Err(), context.DeadlineExceeded) && poll.ctx.Err() == nil && r.Context().Err() == nil {
				if nextOffset > effectiveOffset {
					effectiveOffset = nextOffset
				}
				break
			}
			if poll.wasSuperseded() {
				h.updatePollConflict(w, botID, method)
				return
			}
			if r.Context().Err() != nil {
				return
			}
			h.log.Error("Bot API update log read failed", "bot_id", botID, "method", method, "err", readErr)
			h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
			return
		}
		if nextOffset > effectiveOffset {
			effectiveOffset = nextOffset
		}
		if len(updates) != 0 || params.timeout == 0 || pagesRemaining == 0 {
			break
		}
		select {
		case <-poll.superseded:
			h.updatePollConflict(w, botID, method)
			return
		case <-r.Context().Done():
			return
		case <-poll.ctx.Done():
			if poll.wasSuperseded() {
				h.updatePollConflict(w, botID, method)
			}
			return
		case <-poll.wake:
		case <-readCtx.Done():
			if poll.wasSuperseded() {
				h.updatePollConflict(w, botID, method)
				return
			}
			if r.Context().Err() != nil || poll.ctx.Err() != nil {
				return
			}
			timedOut = true
			break pollLoop
		}
	}
	if timedOut && len(updates) == 0 {
		if poll.wasSuperseded() {
			h.updatePollConflict(w, botID, method)
			return
		}
		if err := st.CheckBotUpdatePoll(poll.ctx, botID, token, claimed.Generation); err != nil {
			if errors.Is(err, store.ErrBotPollSuperseded) || poll.wasSuperseded() {
				h.updatePollConflict(w, botID, method)
				return
			}
			if errors.Is(err, store.ErrBotTokenNotFound) {
				h.unauthorized(w, botID, method)
				return
			}
			if r.Context().Err() != nil {
				return
			}
			h.log.Error("Bot API update poll ownership check failed", "bot_id", botID, "method", method, "err", err)
			h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
			return
		}
		if pagesRemaining > 0 {
			finalReadCtx, cancelFinalRead := context.WithTimeout(poll.ctx, getUpdatesFinalScanTimeout)
			updates, nextOffset, _, readErr = h.readBotUpdates(finalReadCtx, st, botID, effectiveOffset, params.limit, pagesRemaining)
			cancelFinalRead()
			if nextOffset > effectiveOffset {
				effectiveOffset = nextOffset
			}
			if readErr != nil && !errors.Is(readErr, context.DeadlineExceeded) {
				if poll.wasSuperseded() {
					h.updatePollConflict(w, botID, method)
					return
				}
				if r.Context().Err() != nil {
					return
				}
				h.log.Error("Bot API update log read failed", "bot_id", botID, "method", method, "err", readErr)
				h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
				return
			}
		}
	}
	if poll.wasSuperseded() {
		h.updatePollConflict(w, botID, method)
		return
	}

	_, err = st.FinishBotUpdatePoll(poll.ctx, botID, token, claimed.Generation, effectiveOffset)
	if errors.Is(err, store.ErrBotPollSuperseded) || (err != nil && poll.wasSuperseded()) {
		h.updatePollConflict(w, botID, method)
		return
	}
	if errors.Is(err, store.ErrBotTokenNotFound) {
		h.unauthorized(w, botID, method)
		return
	}
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		h.log.Error("Bot API update offset confirmation failed", "bot_id", botID, "method", method, "err", err)
		h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
		return
	}
	encoded, err := json.Marshal(getUpdatesResponse{OK: true, Result: updates})
	if err != nil {
		h.log.Error("Bot API updates response encoding failed", "bot_id", botID, "method", method, "err", err)
		h.write(w, http.StatusInternalServerError, []byte(serverErrorBody), botID, method)
		return
	}
	encoded = append(encoded, '\n')
	h.log.Info("Bot API request", "bot_id", botID, "method", method)
	h.write(w, http.StatusOK, encoded, botID, method)
}

func (h *handler) readBotUpdates(ctx context.Context, st getUpdatesStore, botID, offset int64, limit, maxPages int) ([]botAPIUpdate, int64, int, error) {
	if limit < 1 {
		limit = 1
	} else if limit > maxGetUpdatesLimit {
		limit = maxGetUpdatesLimit
	}
	updates := make([]botAPIUpdate, 0, limit)
	scanOffset := offset
	pagesRead := 0
	safeOffset := func() int64 {
		if len(updates) != 0 && scanOffset > updates[0].UpdateID {
			// Keep the first returned update in the inclusive offset window until acknowledged.
			return updates[0].UpdateID
		}
		return scanOffset
	}
	if maxPages < 1 {
		return updates, safeOffset(), pagesRead, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return updates, safeOffset(), pagesRead, err
		}
		pagesRead++
		page, err := st.BotMessageEvents(ctx, botID, scanOffset, maxGetUpdatesLimit)
		if err != nil {
			return updates, safeOffset(), pagesRead, err
		}
		remaining := limit - len(updates)
		updates = append(updates, h.hydrateBotUpdates(botID, page.Events, scanOffset, remaining)...)
		nextOffset := max(page.NextOffset, scanOffset)
		if nextOffset <= scanOffset || len(updates) >= limit || pagesRead >= maxPages {
			scanOffset = nextOffset
			return updates, safeOffset(), pagesRead, nil
		}
		scanOffset = nextOffset
	}
}

func (h *handler) hydrateBotUpdates(botID int64, events []store.BotMessageEvent, offset int64, limit int) []botAPIUpdate {
	updates := make([]botAPIUpdate, 0, len(events))
	for _, event := range events {
		if len(updates) >= limit {
			break
		}
		if event.Type != store.EventNewMessage || event.Pts < offset {
			continue
		}
		message, user := event.Message, event.Sender
		if message.OwnerID != botID || message.LocalID != event.LocalID || message.PeerType != store.PeerTypeUser || message.Out || message.PeerID != message.FromID || message.FromID <= 0 || message.Deleted || message.Action != 0 || message.Text == "" || message.FileID != 0 || message.StickerDocumentID != nil {
			continue
		}
		if user.ID != message.FromID || user.Kind != store.UserKindHuman {
			continue
		}
		username := ""
		if user.Username != nil {
			username = *user.Username
		}
		updates = append(updates, botAPIUpdate{
			UpdateID: event.Pts,
			Message: botAPIUpdateMessage{
				MessageID: message.LocalID,
				From:      botAPIUpdateUser{ID: user.ID, FirstName: user.FirstName, LastName: user.LastName, Username: username},
				Chat:      botAPIUpdateChat{ID: user.ID, FirstName: user.FirstName, LastName: user.LastName, Username: username, Type: "private"},
				Date:      message.Date.Unix(),
				Text:      message.Text,
			},
		})
	}
	return updates
}

func (h *handler) updatePollConflict(w http.ResponseWriter, botID int64, method string) {
	h.write(w, http.StatusConflict, []byte(getUpdatesConflictBody), botID, method)
}

func (p *updatePoll) wasSuperseded() bool {
	select {
	case <-p.superseded:
		return true
	default:
		return false
	}
}

func (l *authLimiter) beginUpdatePoll(parent context.Context, botID int64) (*updatePoll, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.polls == nil {
		l.polls = make(map[int64]*updatePoll)
	}
	if previous := l.polls[botID]; previous != nil {
		close(previous.superseded)
		previous.cancel()
	} else if len(l.polls) >= maxOpenGetUpdates {
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	poll := &updatePoll{ctx: ctx, cancel: cancel, superseded: make(chan struct{}), wake: make(chan struct{}, 1)}
	l.polls[botID] = poll
	return poll, true
}

func (l *authLimiter) endUpdatePoll(botID int64, poll *updatePoll) {
	l.mu.Lock()
	if l.polls[botID] == poll {
		delete(l.polls, botID)
	}
	l.mu.Unlock()
	poll.cancel()
}

func (l *authLimiter) wakeUpdatePoll(botID int64) {
	l.mu.Lock()
	poll := l.polls[botID]
	if poll != nil {
		select {
		case poll.wake <- struct{}{}:
		default:
		}
	}
	l.mu.Unlock()
}
