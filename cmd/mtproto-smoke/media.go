package main

import (
	"context"
	"crypto/md5" // #nosec G501 -- Telegram requires this legacy checksum for InputFile uploads.
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/gotd/td/tg"
)

const (
	mediaPartSize = 512 * 1024
	maxSmokeMedia = 64 * 1024 * 1024
	// Keep profile photo uploads within the server's accepted encoded-JPEG cap.
	maxSmokePhoto = 10 * 1024 * 1024
)

type mediaReference struct {
	ID            int64  `json:"id"`
	AccessHash    int64  `json:"access_hash"`
	FileReference []byte `json:"file_reference"`
	Size          int64  `json:"size"`
}

func uploadMediaSmoke(ctx context.Context, api *tg.Client, payloadPath, manifestPath string) error {
	payload, err := readSmokeMedia(payloadPath)
	if err != nil {
		return err
	}
	fileID, err := randomPositiveInt64()
	if err != nil {
		return fmt.Errorf("create media upload id: %w", err)
	}
	parts := 0
	for offset := 0; offset < len(payload); offset += mediaPartSize {
		end := min(offset+mediaPartSize, len(payload))
		saved, err := api.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
			FileID: fileID, FilePart: parts, Bytes: payload[offset:end],
		})
		if err != nil {
			return fmt.Errorf("save media upload part %d: %w", parts, err)
		}
		if !saved {
			return fmt.Errorf("save media upload part %d returned false", parts)
		}
		parts++
	}
	checksum := md5.Sum(payload) // #nosec G401 -- Telegram requires the legacy checksum for InputFile uploads.
	name := "cutover-preserved-media.bin"
	updates, err := api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer: &tg.InputPeerSelf{},
		Media: &tg.InputMediaUploadedDocument{
			File: &tg.InputFile{
				ID: fileID, Parts: parts, Name: name, MD5Checksum: hex.EncodeToString(checksum[:]),
			},
			MimeType:   "application/octet-stream",
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: name}},
		},
		Message:  "cutover media preservation smoke",
		RandomID: fileID,
	})
	if err != nil {
		return fmt.Errorf("send uploaded media: %w", err)
	}
	reference, err := mediaReferenceFromUpdates(updates, int64(len(payload)))
	if err != nil {
		return err
	}
	if err := writeMediaReference(manifestPath, reference); err != nil {
		return err
	}
	return nil
}

func uploadProfilePhotoSmoke(ctx context.Context, api *tg.Client, username, payloadPath, startGateFile string) error {
	payload, err := readSmokeProfilePhoto(payloadPath)
	if err != nil {
		return err
	}
	fileID, parts, checksum, err := uploadPhotoParts(ctx, api, payload)
	if err != nil {
		return fmt.Errorf("upload profile photo parts: %w", err)
	}
	if startGateFile != "" {
		if _, err := fmt.Fprintln(os.Stdout, "Profile photo upload ready"); err != nil {
			return fmt.Errorf("write profile photo readiness: %w", err)
		}
		if err := waitForSmokeGate(ctx, startGateFile); err != nil {
			return err
		}
	}
	request := &tg.PhotosUploadProfilePhotoRequest{}
	request.SetFile(&tg.InputFile{
		ID: fileID, Parts: parts, Name: "lima-memory-profile-photo.jpg", MD5Checksum: checksum,
	})
	result, err := api.PhotosUploadProfilePhoto(ctx, request)
	if err != nil {
		return fmt.Errorf("upload profile photo: %w", err)
	}
	if result == nil {
		return errors.New("upload profile photo returned no result")
	}
	photo, ok := result.Photo.(*tg.Photo)
	if !ok || photo == nil || photo.ID <= 0 {
		return fmt.Errorf("upload profile photo returned %T/%+v, want stored photo", result.Photo, result.Photo)
	}
	for _, candidate := range result.Users {
		user, ok := candidate.(*tg.User)
		if !ok || user == nil || user.Username != username {
			continue
		}
		profile, hasPhoto := user.GetPhoto()
		selected, selectedOK := profile.(*tg.UserProfilePhoto)
		if hasPhoto && selectedOK && selected != nil && selected.PhotoID == photo.ID {
			return nil
		}
		return fmt.Errorf("profile photo response for %s did not select uploaded photo %d", username, photo.ID)
	}
	return fmt.Errorf("profile photo response omitted user %s", username)
}

func uploadMediaPhotoSmoke(ctx context.Context, api *tg.Client, payloadPath, startGateFile string) error {
	payload, err := readSmokeMediaPhoto(payloadPath)
	if err != nil {
		return err
	}
	fileID, parts, checksum, err := uploadPhotoParts(ctx, api, payload)
	if err != nil {
		return fmt.Errorf("upload media photo parts: %w", err)
	}
	if startGateFile != "" {
		if _, err := fmt.Fprintln(os.Stdout, "Media photo upload ready"); err != nil {
			return fmt.Errorf("write media photo readiness: %w", err)
		}
		if err := waitForSmokeGate(ctx, startGateFile); err != nil {
			return err
		}
	}
	updates, err := api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Peer: &tg.InputPeerSelf{},
		Media: &tg.InputMediaUploadedPhoto{File: &tg.InputFile{
			ID: fileID, Parts: parts, Name: "lima-memory-media-photo.jpg", MD5Checksum: checksum,
		}},
		Message:  "lima media photo memory probe",
		RandomID: fileID,
	})
	if err != nil {
		return fmt.Errorf("send uploaded media photo: %w", err)
	}
	if err := validateMediaPhotoUpdates(updates); err != nil {
		return err
	}
	return nil
}

func uploadPhotoParts(ctx context.Context, api *tg.Client, payload []byte) (int64, int, string, error) {
	fileID, err := randomPositiveInt64()
	if err != nil {
		return 0, 0, "", fmt.Errorf("create photo upload id: %w", err)
	}
	parts := 0
	for offset := 0; offset < len(payload); offset += mediaPartSize {
		end := min(offset+mediaPartSize, len(payload))
		saved, err := api.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{
			FileID: fileID, FilePart: parts, Bytes: payload[offset:end],
		})
		if err != nil {
			return 0, 0, "", fmt.Errorf("save photo upload part %d: %w", parts, err)
		}
		if !saved {
			return 0, 0, "", fmt.Errorf("save photo upload part %d returned false", parts)
		}
		parts++
	}
	checksum := md5.Sum(payload) // #nosec G401 -- Telegram requires the legacy checksum for InputFile uploads.
	return fileID, parts, hex.EncodeToString(checksum[:]), nil
}

func validateMediaPhotoUpdates(updates tg.UpdatesClass) error {
	var entries []tg.UpdateClass
	switch result := updates.(type) {
	case *tg.Updates:
		entries = result.Updates
	case *tg.UpdatesCombined:
		entries = result.Updates
	default:
		return fmt.Errorf("send uploaded media photo returned %T, want updates", updates)
	}
	for _, update := range entries {
		newMessage, ok := update.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		message, ok := newMessage.Message.(*tg.Message)
		if !ok {
			continue
		}
		media, ok := message.Media.(*tg.MessageMediaPhoto)
		if !ok {
			continue
		}
		photo, ok := media.Photo.(*tg.Photo)
		if !ok || photo.ID <= 0 {
			return fmt.Errorf("send uploaded media photo returned invalid photo %T", media.Photo)
		}
		return nil
	}
	return errors.New("send uploaded media photo returned no photo message")
}

func waitForSmokeGate(ctx context.Context, path string) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := os.Stat(path)
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect smoke start gate: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for smoke start gate: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func downloadMediaSmoke(ctx context.Context, api *tg.Client, manifestPath, outputPath string) error {
	reference, err := readMediaReference(manifestPath)
	if err != nil {
		return err
	}
	var payload []byte
	for offset := int64(0); ; {
		result, err := api.UploadGetFile(ctx, &tg.UploadGetFileRequest{
			Location: &tg.InputDocumentFileLocation{
				ID: reference.ID, AccessHash: reference.AccessHash, FileReference: reference.FileReference,
			},
			Offset: offset,
			Limit:  mediaPartSize,
		})
		if err != nil {
			return fmt.Errorf("download preserved media at offset %d: %w", offset, err)
		}
		file, ok := result.(*tg.UploadFile)
		if !ok {
			return fmt.Errorf("download preserved media returned %T, want *tg.UploadFile", result)
		}
		payload = append(payload, file.Bytes...)
		if len(payload) > maxSmokeMedia || int64(len(payload)) > reference.Size {
			return errors.New("downloaded preserved media exceeds its recorded size")
		}
		offset += int64(len(file.Bytes))
		if len(file.Bytes) < mediaPartSize {
			break
		}
	}
	if int64(len(payload)) != reference.Size {
		return fmt.Errorf("downloaded preserved media has %d bytes, want %d", len(payload), reference.Size)
	}
	file, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- caller supplies a run-owned output path.
	if err != nil {
		return fmt.Errorf("create preserved media output: %w", err)
	}
	if _, err := file.Write(payload); err != nil {
		writeErr := fmt.Errorf("write preserved media output: %w", err)
		if closeErr := file.Close(); closeErr != nil {
			return errors.Join(writeErr, fmt.Errorf("close preserved media output: %w", closeErr))
		}
		return writeErr
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close preserved media output: %w", err)
	}
	return nil
}

func readSmokeMedia(path string) (payload []byte, returnErr error) {
	return readSmokePayload(path, maxSmokeMedia, "media upload")
}

func readSmokeProfilePhoto(path string) (payload []byte, returnErr error) {
	return readSmokePayload(path, maxSmokePhoto, "profile photo")
}

func readSmokeMediaPhoto(path string) (payload []byte, returnErr error) {
	return readSmokePayload(path, maxSmokePhoto, "media photo")
}

func readSmokePayload(path string, maxBytes int64, kind string) (payload []byte, returnErr error) {
	file, err := os.Open(path) // #nosec G304 -- CI supplies a run-owned fixture path.
	if err != nil {
		return nil, fmt.Errorf("open %s fixture: %w", kind, err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close %s fixture: %w", kind, err))
		}
	}()
	payload, err = io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s fixture: %w", kind, err)
	}
	if len(payload) == 0 || int64(len(payload)) > maxBytes {
		return nil, fmt.Errorf("%s fixture size must be between 1 and %d bytes", kind, maxBytes)
	}
	return payload, nil
}

func mediaReferenceFromUpdates(updates tg.UpdatesClass, size int64) (mediaReference, error) {
	var entries []tg.UpdateClass
	switch result := updates.(type) {
	case *tg.Updates:
		entries = result.Updates
	case *tg.UpdatesCombined:
		entries = result.Updates
	default:
		return mediaReference{}, fmt.Errorf("send uploaded media returned %T, want updates", updates)
	}
	for _, update := range entries {
		newMessage, ok := update.(*tg.UpdateNewMessage)
		if !ok {
			continue
		}
		message, ok := newMessage.Message.(*tg.Message)
		if !ok {
			continue
		}
		media, ok := message.Media.(*tg.MessageMediaDocument)
		if !ok {
			continue
		}
		document, ok := media.Document.(*tg.Document)
		if !ok || document.Size != size || document.ID <= 0 || len(document.FileReference) == 0 {
			return mediaReference{}, errors.New("send uploaded media returned an invalid document reference")
		}
		return mediaReference{
			ID: document.ID, AccessHash: document.AccessHash,
			FileReference: append([]byte(nil), document.FileReference...), Size: document.Size,
		}, nil
	}
	return mediaReference{}, errors.New("send uploaded media returned no document message")
}

func writeMediaReference(path string, reference mediaReference) (returnErr error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- caller supplies a run-owned manifest path.
	if err != nil {
		return fmt.Errorf("create media reference manifest: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("close media reference manifest: %w", err)
		}
	}()
	if err := json.NewEncoder(file).Encode(reference); err != nil {
		return fmt.Errorf("write media reference manifest: %w", err)
	}
	return nil
}

func readMediaReference(path string) (reference mediaReference, returnErr error) {
	file, err := os.Open(path) // #nosec G304 -- caller supplies a run-owned manifest path.
	if err != nil {
		return mediaReference{}, fmt.Errorf("open media reference manifest: %w", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close media reference manifest: %w", err))
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return mediaReference{}, fmt.Errorf("inspect media reference manifest: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return mediaReference{}, errors.New("media reference manifest must be a private regular file")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4096))
	if err := decoder.Decode(&reference); err != nil {
		return mediaReference{}, fmt.Errorf("decode media reference manifest: %w", err)
	}
	if reference.ID <= 0 || reference.Size <= 0 || reference.Size > maxSmokeMedia || len(reference.FileReference) == 0 {
		return mediaReference{}, errors.New("media reference manifest has invalid document fields")
	}
	return reference, nil
}

func randomPositiveInt64() (int64, error) {
	var value [8]byte
	if _, err := rand.Read(value[:]); err != nil {
		return 0, err
	}
	result := int64(binary.LittleEndian.Uint64(value[:]) & (^uint64(0) >> 1))
	if result == 0 {
		return randomPositiveInt64()
	}
	return result, nil
}
