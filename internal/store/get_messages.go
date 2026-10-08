package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/teagramhq/teagram-server/internal/store/db"
)

// MessageLookup is one messages.getMessages input after stable request
// deduplication. ReplyTo asks the store to follow the source row's trusted
// owner-local reply link; otherwise ID addresses a direct local message.
type MessageLookup struct {
	ID      int64
	ReplyTo bool
}

// MessageLookupResult retains the requested ID even when the lookup is hidden
// or absent, so the API can return the same messageEmpty shape for both.
type MessageLookupResult struct {
	RequestedID int64
	Message     *Message
}

// MessagesReadSnapshot contains authorized results and only the related rows
// needed to render them, all observed in one read-only repeatable-read snapshot.
type MessagesReadSnapshot struct {
	Results               []MessageLookupResult
	Users                 map[int64]User
	EntitledUserIDs       map[int64]bool
	Contacts              map[int64]Contact
	Chats                 map[int64]Chat
	ChatParticipantCounts map[int64]int
	CreateUsersByLocalID  map[int64][]int64
	Files                 map[int64]File
	Polls                 map[int64]Poll
	ReactionsByLocalID    map[int64][]Reaction
}

// MessagesForLookupSnapshot resolves direct IDs in the caller's message space
// and reply references only through a trusted link on an active, accessible
// source row. Authorization and all response hydration share one database
// snapshot, so a concurrent chat removal cannot split the decision from the
// data returned with it.
func (s *Store) MessagesForLookupSnapshot(ctx context.Context, ownerID int64, lookups []MessageLookup) (MessagesReadSnapshot, error) {
	snapshot := emptyMessagesReadSnapshot(len(lookups))
	if ownerID <= 0 {
		return snapshot, fmt.Errorf("messages lookup: invalid owner %d", ownerID)
	}

	sourceIDs := make([]int64, 0, len(lookups))
	seenSourceIDs := make(map[int64]bool, len(lookups))
	for i, lookup := range lookups {
		snapshot.Results[i].RequestedID = lookup.ID
		if lookup.ID > 0 && !seenSourceIDs[lookup.ID] {
			sourceIDs = append(sourceIDs, lookup.ID)
			seenSourceIDs[lookup.ID] = true
		}
	}
	if len(sourceIDs) == 0 {
		return snapshot, nil
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return snapshot, fmt.Errorf("begin messages read snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // no-op after commit
	qtx := s.q.WithTx(tx)

	sourceRows, err := qtx.MessagesByOwnerLocalIDs(ctx, db.MessagesByOwnerLocalIDsParams{
		OwnerID:  ownerID,
		LocalIds: sourceIDs,
	})
	if err != nil {
		return snapshot, fmt.Errorf("select messages lookup sources: %w", err)
	}
	if hook := s.getMessagesSnapshotHook; hook != nil {
		hook()
	}
	sources := make(map[int64]Message, len(sourceRows))
	for _, row := range sourceRows {
		message := messageFromRow(row)
		sources[message.LocalID] = message
	}

	memberByChat := make(map[int64]bool)
	checkedChats := make(map[int64]bool)
	isAccessible := func(message Message) (bool, error) {
		if message.Deleted {
			return false, nil
		}
		switch message.PeerType {
		case PeerTypeUser:
			return true, nil
		case PeerTypeChat:
			if !checkedChats[message.PeerID] {
				member, memberErr := qtx.IsChatMember(ctx, db.IsChatMemberParams{
					ChatID: message.PeerID,
					UserID: ownerID,
				})
				if memberErr != nil {
					return false, fmt.Errorf("check messages lookup chat membership: %w", memberErr)
				}
				memberByChat[message.PeerID] = member
				checkedChats[message.PeerID] = true
			}
			return memberByChat[message.PeerID], nil
		default:
			return false, nil
		}
	}

	targetIDs := make([]int64, 0, len(lookups))
	seenTargetIDs := make(map[int64]bool, len(lookups))
	targetSourceByLookup := make(map[int]int64, len(lookups))
	for i, lookup := range lookups {
		source, ok := sources[lookup.ID]
		if !ok {
			continue
		}
		accessible, accessErr := isAccessible(source)
		if accessErr != nil {
			return snapshot, accessErr
		}
		if !accessible {
			continue
		}
		if !lookup.ReplyTo {
			message := source
			snapshot.Results[i].Message = &message
			continue
		}
		if !source.ReplyToTrusted || source.ReplyToMsgID <= 0 {
			continue
		}
		targetID := int64(source.ReplyToMsgID)
		targetSourceByLookup[i] = source.LocalID
		if !seenTargetIDs[targetID] {
			targetIDs = append(targetIDs, targetID)
			seenTargetIDs[targetID] = true
		}
	}

	if len(targetIDs) > 0 {
		targetRows, targetErr := qtx.MessagesByOwnerLocalIDs(ctx, db.MessagesByOwnerLocalIDsParams{
			OwnerID:  ownerID,
			LocalIds: targetIDs,
		})
		if targetErr != nil {
			return snapshot, fmt.Errorf("select messages lookup targets: %w", targetErr)
		}
		targets := make(map[int64]Message, len(targetRows))
		for _, row := range targetRows {
			message := messageFromRow(row)
			targets[message.LocalID] = message
		}
		for lookupIndex, sourceID := range targetSourceByLookup {
			source := sources[sourceID]
			target, ok := targets[int64(source.ReplyToMsgID)]
			if !ok || target.Deleted || target.PeerType != source.PeerType || target.PeerID != source.PeerID {
				continue
			}
			accessible, accessErr := isAccessible(target)
			if accessErr != nil {
				return snapshot, accessErr
			}
			if accessible {
				message := target
				snapshot.Results[lookupIndex].Message = &message
			}
		}
	}

	messages := make([]Message, 0, len(snapshot.Results))
	messageByLocalID := make(map[int64]Message, len(snapshot.Results))
	for _, result := range snapshot.Results {
		if result.Message == nil {
			continue
		}
		messageByLocalID[result.Message.LocalID] = *result.Message
	}
	for _, result := range snapshot.Results {
		if result.Message == nil {
			continue
		}
		if _, seen := messageByLocalID[result.Message.LocalID]; !seen {
			continue
		}
		messages = append(messages, messageByLocalID[result.Message.LocalID])
		delete(messageByLocalID, result.Message.LocalID)
	}
	if err = s.hydrateMessagesReadSnapshot(ctx, qtx, ownerID, messages, &snapshot); err != nil {
		return snapshot, err
	}
	if err = tx.Commit(ctx); err != nil {
		return snapshot, fmt.Errorf("commit messages read snapshot: %w", err)
	}
	return snapshot, nil
}

func emptyMessagesReadSnapshot(resultCount int) MessagesReadSnapshot {
	return MessagesReadSnapshot{
		Results:               make([]MessageLookupResult, resultCount),
		Users:                 map[int64]User{},
		EntitledUserIDs:       map[int64]bool{},
		Contacts:              map[int64]Contact{},
		Chats:                 map[int64]Chat{},
		ChatParticipantCounts: map[int64]int{},
		CreateUsersByLocalID:  map[int64][]int64{},
		Files:                 map[int64]File{},
		Polls:                 map[int64]Poll{},
		ReactionsByLocalID:    map[int64][]Reaction{},
	}
}

func (s *Store) hydrateMessagesReadSnapshot(ctx context.Context, qtx *db.Queries, ownerID int64, messages []Message, snapshot *MessagesReadSnapshot) error {
	if len(messages) == 0 {
		return nil
	}

	localIDs := make([]int64, 0, len(messages))
	userIDs := map[int64]bool{ownerID: true}
	chatIDs := make(map[int64]bool)
	createChatIDs := make(map[int64]bool)
	fileIDs := make([]int64, 0, len(messages))
	for _, message := range messages {
		localIDs = append(localIDs, message.LocalID)
		userIDs[message.FromID] = true
		switch message.PeerType {
		case PeerTypeUser:
			userIDs[message.PeerID] = true
		case PeerTypeChat:
			chatIDs[message.PeerID] = true
		}
		switch message.Action {
		case ChatActionAddUser, ChatActionDeleteUser:
			userIDs[message.ActionUserID] = true
		case ChatActionCreate:
			createChatIDs[message.PeerID] = true
		}
		if message.FileID > 0 {
			fileIDs = append(fileIDs, message.FileID)
		}
	}
	participantsByChat := make(map[int64][]int64, len(createChatIDs))
	if len(createChatIDs) > 0 {
		participantRows, err := qtx.ChatParticipantsByChatIDs(ctx, boolKeys(createChatIDs))
		if err != nil {
			return fmt.Errorf("hydrate messages create participants: %w", err)
		}
		for _, row := range participantRows {
			participantsByChat[row.ChatID] = append(participantsByChat[row.ChatID], row.UserID)
			userIDs[row.UserID] = true
		}
		for _, message := range messages {
			if message.Action == ChatActionCreate {
				snapshot.CreateUsersByLocalID[message.LocalID] = participantsByChat[message.PeerID]
			}
		}
	}

	userIDList := boolKeys(userIDs)
	userRows, err := qtx.UsersByID(ctx, userIDList)
	if err != nil {
		return fmt.Errorf("hydrate messages users: %w", err)
	}
	for _, row := range userRows {
		snapshot.Users[row.ID] = UserFromDB(db.UserByIDRow(row))
	}
	entitledRows, err := qtx.EntitledUserIDs(ctx, db.EntitledUserIDsParams{ViewerID: ownerID, Ids: userIDList})
	if err != nil {
		return fmt.Errorf("hydrate messages user entitlements: %w", err)
	}
	for _, row := range entitledRows {
		id, ok := row.(int64)
		if !ok {
			return fmt.Errorf("hydrate messages user entitlements: unexpected id type %T", row)
		}
		snapshot.EntitledUserIDs[id] = true
	}
	contactIDs := make([]int64, 0, len(snapshot.Users))
	for id := range snapshot.Users {
		if id != ownerID && snapshot.EntitledUserIDs[id] {
			contactIDs = append(contactIDs, id)
		}
	}
	if len(contactIDs) > 0 {
		contactRows, contactErr := qtx.ContactStates(ctx, db.ContactStatesParams{OwnerID: ownerID, ContactIds: contactIDs})
		if contactErr != nil {
			return fmt.Errorf("hydrate messages contact states: %w", contactErr)
		}
		for _, row := range contactRows {
			snapshot.Contacts[row.ContactID] = Contact{UserID: row.ContactID, Mutual: row.Mutual}
		}
	}

	chatIDList := boolKeys(chatIDs)
	if len(chatIDList) > 0 {
		chatRows, chatErr := qtx.ChatsByIDsForMember(ctx, db.ChatsByIDsForMemberParams{UserID: ownerID, ChatIds: chatIDList})
		if chatErr != nil {
			return fmt.Errorf("hydrate messages chats: %w", chatErr)
		}
		for _, row := range chatRows {
			snapshot.Chats[row.ID] = chatFromRow(row)
		}
		countRows, countErr := qtx.ChatParticipantCountsByChatIDs(ctx, chatIDList)
		if countErr != nil {
			return fmt.Errorf("hydrate messages chat participant counts: %w", countErr)
		}
		for _, row := range countRows {
			snapshot.ChatParticipantCounts[row.ChatID] = int(row.ParticipantCount)
		}
	}

	if len(fileIDs) > 0 {
		fileRows, fileErr := qtx.FilesByIDs(ctx, fileIDs)
		if fileErr != nil {
			return fmt.Errorf("hydrate messages files: %w", fileErr)
		}
		for _, row := range fileRows {
			file := fileFromRow(row)
			snapshot.Files[file.ID] = file
		}
	}

	reactionRows, err := qtx.ReactionsByMessages(ctx, db.ReactionsByMessagesParams{OwnerID: ownerID, LocalIds: localIDs})
	if err != nil {
		return fmt.Errorf("hydrate messages reactions: %w", err)
	}
	for _, row := range reactionRows {
		snapshot.ReactionsByLocalID[row.LocalID] = append(snapshot.ReactionsByLocalID[row.LocalID], reactionFromRow(row))
	}

	pollLocalIDs, err := qtx.PollMessageCopiesByOwnerLocalIDs(ctx, db.PollMessageCopiesByOwnerLocalIDsParams{OwnerID: ownerID, LocalIds: localIDs})
	if err != nil {
		return fmt.Errorf("hydrate messages poll copies: %w", err)
	}
	for _, localID := range pollLocalIDs {
		message, ok := findMessageByLocalID(messages, localID)
		if !ok {
			continue
		}
		pollRow, pollErr := qtx.PollByMessage(ctx, db.PollByMessageParams{
			OwnerID: ownerID, LocalID: localID, PeerType: int16(message.PeerType), PeerID: message.PeerID,
		})
		if errors.Is(pollErr, pgx.ErrNoRows) {
			continue
		}
		if pollErr != nil {
			return fmt.Errorf("hydrate messages poll: %w", pollErr)
		}
		poll, viewErr := pollView(ctx, qtx, pollRow, ownerID)
		if viewErr != nil {
			return fmt.Errorf("hydrate messages poll view: %w", viewErr)
		}
		snapshot.Polls[localID] = poll
	}
	return nil
}

func findMessageByLocalID(messages []Message, localID int64) (Message, bool) {
	for _, message := range messages {
		if message.LocalID == localID {
			return message, true
		}
	}
	return Message{}, false
}

func boolKeys(values map[int64]bool) []int64 {
	keys := make([]int64, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	return keys
}
