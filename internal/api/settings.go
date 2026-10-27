package api

import (
	"encoding/binary"
	"hash/crc32"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"

	"github.com/teagramhq/teagram-server/internal/mtproto"
	"github.com/teagramhq/teagram-server/internal/store"
)

const (
	appConfigHash       = 7
	emptyWallPapersHash = 1
	emptyChatThemesHash = 1
)

func (h *handlers) handleGetContentSettings(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetContentSettingsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	return &tg.AccountContentSettings{}, nil
}

func (h *handlers) handleGetGlobalPrivacySettings(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetGlobalPrivacySettingsRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	return &tg.GlobalPrivacySettings{}, nil
}

func (h *handlers) handleGetThemes(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetThemesRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	return &tg.AccountThemes{Themes: []tg.Theme{}}, nil
}

func (h *handlers) handleGetWallPapers(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetWallPapersRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if req.Hash == emptyWallPapersHash {
		return &tg.AccountWallPapersNotModified{}, nil
	}
	return &tg.AccountWallPapers{
		Hash:       emptyWallPapersHash,
		Wallpapers: []tg.WallPaperClass{},
	}, nil
}

func (h *handlers) handleGetChatThemes(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.AccountGetChatThemesRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	if r.UserID == 0 {
		return nil, errAuthKeyUnreg
	}
	if req.Hash == emptyChatThemesHash {
		return &tg.AccountThemesNotModified{}, nil
	}
	return &tg.AccountThemes{
		Hash:   emptyChatThemesHash,
		Themes: []tg.Theme{},
	}, nil
}

func (h *handlers) handleGetAppConfig(r *mtproto.Request) (bin.Encoder, error) {
	var req tg.HelpGetAppConfigRequest
	if err := req.Decode(r.Buf); err != nil {
		return nil, errMethodNotImpl
	}
	// help.appConfig carries an extensible JSON object, so the registration mode
	// does not change the fixed TL response shape that stock clients decode.
	values := []tg.JSONObjectValue{{
		Key:   "chat_read_mark_expire_period",
		Value: &tg.JSONNumber{Value: store.ChatReadMarkExpirePeriod},
	}, {
		Key:   "chat_read_mark_size_threshold",
		Value: &tg.JSONNumber{Value: store.ChatReadMarkSizeThreshold},
	}, {
		Key:   "upload_max_fileparts_default",
		Value: &tg.JSONNumber{Value: float64(h.maxFileParts())},
	}, {
		Key:   "upload_max_fileparts_premium",
		Value: &tg.JSONNumber{Value: float64(h.maxFileParts())},
	}, {
		Key:   "dialog_filters_chats_limit_default",
		Value: &tg.JSONNumber{Value: 100},
	}, {
		Key:   "dialog_filters_enabled",
		Value: &tg.JSONBool{Value: true},
	}, {
		Key:   "dialog_filters_limit_default",
		Value: &tg.JSONNumber{Value: 10},
	}, {
		Key:   "dialog_filters_tooltip",
		Value: &tg.JSONBool{Value: false},
	}, {
		Key:   "dialogs_folder_pinned_limit_default",
		Value: &tg.JSONNumber{Value: 100},
	}, {
		Key:   "dialogs_pinned_limit_default",
		Value: &tg.JSONNumber{Value: 5},
	}, {
		Key:   "registration_mode",
		Value: &tg.JSONString{Value: string(h.registrationMode)},
	}, {
		Key:   "sticker_import_mode",
		Value: &tg.JSONString{Value: string(h.stickerImportMode)},
	}, {
		Key:   "reactions_uniq_max",
		Value: &tg.JSONNumber{Value: 11},
	}, {
		Key:   "reactions_user_max_default",
		Value: &tg.JSONNumber{Value: 1},
	}, {
		Key:   "reactions_user_max_premium",
		Value: &tg.JSONNumber{Value: 1},
	}}
	if h.appConfigUploadMaxFileBytes > 0 {
		values = append(values, tg.JSONObjectValue{
			Key:   "upload_max_file_bytes",
			Value: &tg.JSONNumber{Value: float64(h.appConfigUploadMaxFileBytes)},
		})
	}
	if h.turnstileSiteKey != "" && h.turnstileVerifier != nil {
		values = append(values, tg.JSONObjectValue{
			Key: "turnstile",
			Value: &tg.JSONObject{Value: []tg.JSONObjectValue{{
				Key:   "site_key",
				Value: &tg.JSONString{Value: h.turnstileSiteKey},
			}}},
		})
	}
	return &tg.HelpAppConfig{
		Hash:   appConfigHashForUploadLimit(h.appConfigUploadMaxFileBytes),
		Config: &tg.JSONObject{Value: values},
	}, nil
}

func appConfigHashForUploadLimit(maxFileBytes int64) int {
	if maxFileBytes <= 0 {
		return appConfigHash
	}
	var encoded [8]byte
	binary.LittleEndian.PutUint64(encoded[:], uint64(maxFileBytes))
	hash := crc32.ChecksumIEEE(encoded[:]) & 0x7fffffff
	if int(hash) == appConfigHash {
		hash++
	}
	return int(hash)
}
