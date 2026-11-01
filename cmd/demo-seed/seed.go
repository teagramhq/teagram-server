package main

import (
	"context"
	"crypto/md5" //nolint:gosec // MTProto requires MD5 as its legacy upload checksum.
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/gotd/td/tg"
)

type accountSpec struct {
	loginID   string
	username  string
	firstName string
	lastName  string
	about     string
	avatar    int
	owner     bool
	activity  bool
}

type spaceSpec struct {
	slug      string
	username  string
	title     string
	about     string
	megagroup bool
}

type remoteSpace struct {
	spec       spaceSpec
	channelID  int64
	accessHash int64
	creator    bool
}

var seedAccounts = []accountSpec{
	{loginID: "demo_seed_owner", username: "demo_seed_owner", firstName: "Mira", lastName: "Wells", about: "A fictional field naturalist collecting quiet paths and cloud maps.", avatar: 0, owner: true},
	{loginID: "demo_iona", username: "demo_iona", firstName: "Iona", lastName: "Vale", about: "Sketching riverside paths and small changes in the weather.", avatar: 1},
	{loginID: "demo_ren", username: "demo_ren", firstName: "Ren", lastName: "Marlow", about: "Keeping a notebook of garden birds and good walking routes.", avatar: 2, activity: true},
	{loginID: "demo_sasha", username: "demo_sasha", firstName: "Sasha", lastName: "North", about: "Making paper maps for slow walks and curious detours.", avatar: 3},
	{loginID: "demo_elio", username: "demo_elio", firstName: "Elio", lastName: "Park", about: "Collecting color studies from parks, bridges, and open sky.", avatar: 4},
	{loginID: "demo_tavi", username: "demo_tavi", firstName: "Tavi", lastName: "Linden", about: "A patient observer of trees, paths, and changing light.", avatar: 5},
	{loginID: "demo_nori", username: "demo_nori", firstName: "Nori", lastName: "Wells", about: "Saving useful notes from small trips around the valley.", avatar: 6},
	{loginID: "demo_ari", username: "demo_ari", firstName: "Ari", lastName: "Sloan", about: "Drawing imaginary gardens and sharing practical route notes.", avatar: 7},
	{loginID: "demo_kit", username: "demo_kit", firstName: "Kit", lastName: "Rowan", about: "Trading trail observations, sketch prompts, and picnic ideas.", avatar: 8},
	{loginID: "demo_searcher", username: "demo_searcher", firstName: "Sol", lastName: "Piper", about: "A new neighbor looking for public notes and open groups.", avatar: 9},
}

var publicSpaces = []spaceSpec{
	{slug: "welcome", username: "teagramdemowelcome", title: "Welcome to the Teagram demo", about: "A fictional starter channel for exploring the Teagram demo."},
	{slug: "maps", username: "teagramdemomaps", title: "Teagram Demo Field Maps", about: "Fictional route sketches, landscape notes, and mapmaking ideas."},
	{slug: "notes", username: "teagramdemonotes", title: "Teagram Demo Pocket Notes", about: "Fictional observations gathered on unhurried walks."},
	{slug: "studio", username: "teagramdemostudio", title: "Teagram Demo Sketch Studio", about: "Fictional prompts and color studies from an imaginary valley."},
	{slug: "river", username: "teagramdemoriver", title: "Teagram Demo River Walkers", about: "A public group for fictional river path observations and route ideas.", megagroup: true},
	{slug: "garden", username: "teagramdemogarden", title: "Teagram Demo Garden Circle", about: "A public group for fictional garden questions and seasonal notes.", megagroup: true},
	{slug: "workshop", username: "teagramdemoworkshop", title: "Teagram Demo Map Workshop", about: "A public group for fictional mapmaking prompts and helpful feedback.", megagroup: true},
}

var conversationLines = []string{
	"I found a shaded bend just past the old footbridge; it is a calm place to pause.",
	"That sounds lovely. I marked the bend with a blue dot on my paper route.",
	"The north path is dry today, though the stones near the water are still slick.",
	"Thanks for the note. I will add a small bridge symbol beside that stretch.",
	"I brought the folded map from last weekend and found two useful shortcuts.",
	"Could you share the longer one? I am collecting accessible loop ideas.",
	"The meadow loop is gentle if you start beside the lower garden gate.",
	"I tried it this morning. The path is clear, and there are benches by the pond.",
	"I am bringing colored pencils next time so we can compare the route sketches.",
	"I can bring a spare notebook and note the water stops along the way.",
	"The clouds opened around noon, which made the whole hillside look brighter.",
	"I added that light change to my notes. Thanks for making the route clearer.",
}

const seedReactionEmoji = "❤"

func seedDemo(ctx context.Context, cfg apiConfig, options seedOptions, ownerPassword string, inventory map[string]string) error {
	for _, account := range seedAccounts {
		if err := withSeedAccount(ctx, cfg, options, account, ownerPassword, inventory, func(runCtx context.Context, api *tg.Client) error {
			return configureAccount(runCtx, api, account)
		}); err != nil {
			return fmt.Errorf("prepare fictional demo account: %w", err)
		}
	}

	var remotes []remoteSpace
	if err := withSeedOwner(ctx, cfg, options, ownerPassword, func(runCtx context.Context, api *tg.Client) error {
		for _, spec := range publicSpaces {
			remote, err := ensurePublicSpace(runCtx, api, spec)
			if err != nil {
				return err
			}
			if !remote.creator {
				return fmt.Errorf("seed owner does not own public space %s", spec.username)
			}
			remotes = append(remotes, remote)
		}
		for _, remote := range remotes {
			if remote.spec.megagroup {
				if err := inviteConversationAccounts(runCtx, api, remote); err != nil {
					return err
				}
				continue
			}
			if remote.spec.slug == "welcome" {
				if err := seedWelcome(runCtx, api, remote); err != nil {
					return err
				}
				continue
			}
			if err := seedBroadcastChannel(runCtx, api, remote); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("seed public channels and groups: %w", err)
	}

	if err := seedGroupConversations(ctx, cfg, options, ownerPassword, inventory, remotes); err != nil {
		return fmt.Errorf("seed public group conversations: %w", err)
	}
	if err := pinPublicGroups(ctx, cfg, options, ownerPassword, remotes); err != nil {
		return fmt.Errorf("pin public group messages: %w", err)
	}
	if err := seedReactionExample(ctx, cfg, options, inventory); err != nil {
		return fmt.Errorf("seed a sample reaction: %w", err)
	}
	return verifySeed(ctx, cfg, options, ownerPassword, inventory)
}

func verifySeed(ctx context.Context, cfg apiConfig, options seedOptions, ownerPassword string, inventory map[string]string) error {
	verifiedMarkers := 0
	if err := withSeedOwner(ctx, cfg, options, ownerPassword, func(runCtx context.Context, api *tg.Client) error {
		for _, spec := range publicSpaces {
			remote, err := resolvePublicSpace(runCtx, api, spec)
			if err != nil {
				return err
			}
			if !remote.creator {
				return fmt.Errorf("seed owner does not own public space %s", spec.username)
			}
			markers, err := historyMarkers(runCtx, api, remote.peer())
			if err != nil {
				return err
			}
			if got, want := countSeedMarkers(markers, spec), expectedSeedMarkerCount(spec); got != want {
				return fmt.Errorf("space %s has %d seeded content markers, want %d", spec.username, got, want)
			}
			verifiedMarkers += countSeedMarkers(markers, spec)
			if remote.spec.megagroup {
				for turn := range conversationLines {
					if _, ok := markers[conversationMarker(spec, turn)]; !ok {
						return fmt.Errorf("group %s is missing a conversation post", spec.username)
					}
				}
				if messageIsPinned(runCtx, api, remote.peer(), conversationMarker(spec, 1)) != nil {
					return fmt.Errorf("group %s is missing its pinned conversation post", spec.username)
				}
				continue
			}
			if spec.slug == "welcome" {
				marker := welcomeMarker()
				if _, ok := markers[marker]; !ok {
					return errors.New("welcome channel is missing its weekly-wipe note")
				}
				if messageIsPinned(runCtx, api, remote.peer(), marker) != nil {
					return errors.New("welcome weekly-wipe note is not pinned")
				}
				continue
			}
			for _, marker := range broadcastMarkers(spec) {
				if _, ok := markers[marker]; !ok {
					return fmt.Errorf("channel %s is missing seeded content", spec.username)
				}
			}
			if messageIsPinned(runCtx, api, remote.peer(), textMarker(spec, 0)) != nil {
				return fmt.Errorf("channel %s is missing its pinned post", spec.username)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("verify owner-visible demo content: %w", err)
	}

	searcher := seedAccounts[len(seedAccounts)-1]
	if err := withAccount(ctx, cfg, options.endpoint, searcher, inventory[searcher.loginID], nil, func(runCtx context.Context, api *tg.Client) error {
		for _, account := range seedAccounts {
			if err := verifyAccountProfile(runCtx, api, account); err != nil {
				return err
			}
		}
		for _, spec := range publicSpaces {
			if err := verifySearchResult(runCtx, api, spec); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	fmt.Printf("demo seed verified accounts=%d welcome_posts=1 broadcast_channels=3 posts_per_channel=32 public_groups=3 posts_per_group=%d searchable_spaces=%d markers=%d search_account=%s reaction_examples=1\n",
		len(seedAccounts), len(conversationLines), len(publicSpaces), verifiedMarkers, searcher.username)
	return nil
}

func verifyAccountProfile(ctx context.Context, api *tg.Client, account accountSpec) error {
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: account.username})
	if err != nil {
		return fmt.Errorf("resolve demo account %s during verification: %w", account.username, err)
	}
	var user *tg.User
	for _, candidate := range resolved.Users {
		if found, ok := candidate.(*tg.User); ok && found.Username == account.username {
			user = found
			break
		}
	}
	if user == nil || user.AccessHash == 0 {
		return fmt.Errorf("demo account %s is missing its public profile", account.username)
	}
	if user.FirstName != account.firstName || user.LastName != account.lastName {
		return fmt.Errorf("demo account %s has an unexpected display name", account.username)
	}
	profile, err := api.UsersGetFullUser(ctx, &tg.InputUser{UserID: user.ID, AccessHash: user.AccessHash})
	if err != nil {
		return fmt.Errorf("read demo account %s during verification: %w", account.username, err)
	}
	about, hasAbout := profile.FullUser.GetAbout()
	if !hasAbout || about != account.about {
		return fmt.Errorf("demo account %s has an unexpected biography", account.username)
	}
	if len(profile.Users) != 1 {
		return fmt.Errorf("demo account %s profile returned an unexpected user count", account.username)
	}
	profileUser, ok := profile.Users[0].(*tg.User)
	if !ok {
		return fmt.Errorf("demo account %s profile returned %T", account.username, profile.Users[0])
	}
	photo, ok := profileUser.GetPhoto()
	if !ok || isEmptyProfilePhoto(photo) {
		return fmt.Errorf("demo account %s has no uploaded avatar", account.username)
	}
	selected, ok := photo.(*tg.UserProfilePhoto)
	if !ok || selected.PhotoID <= 0 {
		return fmt.Errorf("demo account %s has an invalid avatar selection", account.username)
	}
	return nil
}

func withSeedOwner(ctx context.Context, cfg apiConfig, options seedOptions, password string, action apiAction) error {
	account := seedAccounts[0]
	if account.username != options.ownerUsername {
		return errors.New("configured seed-owner username does not match the fixed demo owner")
	}
	return withAccount(ctx, cfg, options.endpoint, account, password, nil, action)
}

func withSeedAccount(ctx context.Context, cfg apiConfig, options seedOptions, account accountSpec, ownerPassword string, inventory map[string]string, action apiAction) error {
	if account.owner {
		return withSeedOwner(ctx, cfg, options, ownerPassword, action)
	}
	password := inventory[account.loginID]
	save := func(value string) error {
		inventory[account.loginID] = value
		return writeCredentialInventory(inventory)
	}
	return withAccount(ctx, cfg, options.endpoint, account, password, save, action)
}

func configureAccount(ctx context.Context, api *tg.Client, account accountSpec) error {
	full, err := api.UsersGetFullUser(ctx, &tg.InputUserSelf{})
	if err != nil {
		return fmt.Errorf("read demo account profile: %w", err)
	}
	if len(full.Users) != 1 {
		return fmt.Errorf("unexpected demo account profile users count %d", len(full.Users))
	}
	user, ok := full.Users[0].(*tg.User)
	if !ok {
		return fmt.Errorf("unexpected demo account user type %T", full.Users[0])
	}
	if user.Username != account.username {
		if _, err := api.AccountUpdateUsername(ctx, account.username); err != nil {
			return fmt.Errorf("set demo account username: %w", err)
		}
	}
	about, hasAbout := full.FullUser.GetAbout()
	if user.FirstName != account.firstName || user.LastName != account.lastName || !hasAbout || about != account.about {
		request := &tg.AccountUpdateProfileRequest{}
		request.SetFirstName(account.firstName)
		request.SetLastName(account.lastName)
		request.SetAbout(account.about)
		if _, err := api.AccountUpdateProfile(ctx, request); err != nil {
			return fmt.Errorf("set demo account biography: %w", err)
		}
	}
	photo, hasPhoto := user.GetPhoto()
	if !hasPhoto || isEmptyProfilePhoto(photo) {
		payload, err := avatarJPEG(account.avatar)
		if err != nil {
			return err
		}
		file, err := uploadInputFile(ctx, api, payload, "fictional-avatar.jpg")
		if err != nil {
			return fmt.Errorf("upload generated demo avatar: %w", err)
		}
		request := &tg.PhotosUploadProfilePhotoRequest{}
		request.SetFile(file)
		if _, err := api.PhotosUploadProfilePhoto(ctx, request); err != nil {
			return fmt.Errorf("set generated demo avatar: %w", err)
		}
	}
	return nil
}

func isEmptyProfilePhoto(photo tg.UserProfilePhotoClass) bool {
	_, empty := photo.(*tg.UserProfilePhotoEmpty)
	return empty || photo == nil
}

func ensurePublicSpace(ctx context.Context, api *tg.Client, spec spaceSpec) (remoteSpace, error) {
	remote, err := resolvePublicSpace(ctx, api, spec)
	if err == nil {
		if !remote.creator {
			return remoteSpace{}, fmt.Errorf("seed owner does not own public space %s", spec.username)
		}
		return remote, nil
	}
	if !isRPCMessage(err, "USERNAME_NOT_OCCUPIED") {
		return remoteSpace{}, err
	}
	created, err := api.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
		Broadcast: !spec.megagroup,
		Megagroup: spec.megagroup,
		Title:     spec.title,
		About:     spec.about,
	})
	if err != nil {
		return remoteSpace{}, fmt.Errorf("create public demo space: %w", err)
	}
	channel, ok := channelFromUpdates(created)
	if !ok {
		return remoteSpace{}, fmt.Errorf("channel creation returned no channel for %s", spec.username)
	}
	input := &tg.InputChannel{ChannelID: channel.ID, AccessHash: channel.AccessHash}
	if _, err := api.ChannelsUpdateUsername(ctx, &tg.ChannelsUpdateUsernameRequest{Channel: input, Username: spec.username}); err != nil {
		return remoteSpace{}, fmt.Errorf("set public demo handle %s: %w", spec.username, err)
	}
	return remoteSpace{spec: spec, channelID: channel.ID, accessHash: channel.AccessHash, creator: true}, nil
}

func resolvePublicSpace(ctx context.Context, api *tg.Client, spec spaceSpec) (remoteSpace, error) {
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: spec.username})
	if err != nil {
		return remoteSpace{}, fmt.Errorf("resolve public handle %s: %w", spec.username, err)
	}
	channel, ok := resolvedChannel(resolved)
	if !ok {
		return remoteSpace{}, fmt.Errorf("public handle %s belongs to a non-channel peer", spec.username)
	}
	if channel.Username != spec.username || channel.Title != spec.title || channel.Megagroup != spec.megagroup || channel.Broadcast == spec.megagroup {
		return remoteSpace{}, fmt.Errorf("public handle %s does not match the expected demo channel", spec.username)
	}
	participant, err := api.ChannelsGetParticipant(ctx, &tg.ChannelsGetParticipantRequest{
		Channel:     &tg.InputChannel{ChannelID: channel.ID, AccessHash: channel.AccessHash},
		Participant: &tg.InputPeerSelf{},
	})
	if err != nil {
		return remoteSpace{}, fmt.Errorf("inspect seed owner membership in public space %s: %w", spec.username, err)
	}
	return remoteSpace{
		spec:       spec,
		channelID:  channel.ID,
		accessHash: channel.AccessHash,
		creator:    isChannelCreator(participant.Participant),
	}, nil
}

func isChannelCreator(participant tg.ChannelParticipantClass) bool {
	_, ok := participant.(*tg.ChannelParticipantCreator)
	return ok
}

func resolvedChannel(resolved *tg.ContactsResolvedPeer) (*tg.Channel, bool) {
	if resolved == nil {
		return nil, false
	}
	for _, chat := range resolved.Chats {
		if channel, ok := chat.(*tg.Channel); ok {
			return channel, true
		}
	}
	return nil, false
}

func channelFromUpdates(result tg.UpdatesClass) (*tg.Channel, bool) {
	var chats []tg.ChatClass
	switch update := result.(type) {
	case *tg.Updates:
		chats = update.Chats
	case *tg.UpdatesCombined:
		chats = update.Chats
	}
	for _, chat := range chats {
		if channel, ok := chat.(*tg.Channel); ok {
			return channel, true
		}
	}
	return nil, false
}

func (space remoteSpace) peer() tg.InputPeerClass {
	return &tg.InputPeerChannel{ChannelID: space.channelID, AccessHash: space.accessHash}
}

func (space remoteSpace) inputChannel() tg.InputChannelClass {
	return &tg.InputChannel{ChannelID: space.channelID, AccessHash: space.accessHash}
}

func welcomeMarker() string {
	return "[[teagram-demo:welcome:weekly-wipe]]"
}

func textMarker(spec spaceSpec, index int) string {
	return fmt.Sprintf("[[teagram-demo:%s:text-%02d]]", spec.slug, index)
}

func photoMarker(spec spaceSpec) string {
	return fmt.Sprintf("[[teagram-demo:%s:photo]]", spec.slug)
}

func pollMarker(spec spaceSpec) string {
	return fmt.Sprintf("[[teagram-demo:%s:poll]]", spec.slug)
}

func linkMarker(spec spaceSpec) string {
	return fmt.Sprintf("[[teagram-demo:%s:link]]", spec.slug)
}

func conversationMarker(spec spaceSpec, turn int) string {
	return fmt.Sprintf("[[teagram-demo:%s:conversation-%02d]]", spec.slug, turn)
}

func broadcastMarkers(spec spaceSpec) []string {
	markers := make([]string, 0, 32)
	for index := range 28 {
		markers = append(markers, textMarker(spec, index))
	}
	markers = append(markers, conversationMarker(spec, 0), photoMarker(spec), pollMarker(spec), linkMarker(spec))
	return markers
}

func seedWelcome(ctx context.Context, api *tg.Client, remote remoteSpace) error {
	markers, err := historyMarkers(ctx, api, remote.peer())
	if err != nil {
		return err
	}
	marker := welcomeMarker()
	if _, ok := markers[marker]; !ok {
		if _, err := sendText(ctx, api, remote.peer(), marker, "This fictional demo is refreshed by a weekly data wipe. Explore public notes and groups, then come back after the next reset.", 0); err != nil {
			return fmt.Errorf("post weekly-wipe welcome note: %w", err)
		}
	}
	if err := pinMarker(ctx, api, remote.peer(), marker); err != nil {
		return fmt.Errorf("pin weekly-wipe welcome note: %w", err)
	}
	return nil
}

func seedBroadcastChannel(ctx context.Context, api *tg.Client, remote remoteSpace) error {
	markers, err := historyMarkers(ctx, api, remote.peer())
	if err != nil {
		return err
	}
	rootMarker := textMarker(remote.spec, 0)
	if _, ok := markers[rootMarker]; !ok {
		id, err := sendText(ctx, api, remote.peer(), rootMarker, channelText(remote.spec, 0), 0)
		if err != nil {
			return fmt.Errorf("post channel introduction: %w", err)
		}
		markers[rootMarker] = id
	}
	for index := 1; index < 28; index++ {
		marker := textMarker(remote.spec, index)
		if _, ok := markers[marker]; ok {
			continue
		}
		if _, err := sendText(ctx, api, remote.peer(), marker, channelText(remote.spec, index), 0); err != nil {
			return fmt.Errorf("post fictional channel note: %w", err)
		}
	}
	replyMarker := conversationMarker(remote.spec, 0)
	if _, ok := markers[replyMarker]; !ok {
		if _, err := sendText(ctx, api, remote.peer(), replyMarker, "I saved this first route note; the next sketch adds a quieter path by the water.", markers[rootMarker]); err != nil {
			return fmt.Errorf("post channel reply: %w", err)
		}
	}
	if _, ok := markers[photoMarker(remote.spec)]; !ok {
		payload, err := avatarJPEG(20 + int(remote.channelID%10))
		if err != nil {
			return err
		}
		file, err := uploadInputFile(ctx, api, payload, "fictional-landscape.jpg")
		if err != nil {
			return fmt.Errorf("upload generated channel photo: %w", err)
		}
		marker := photoMarker(remote.spec)
		_, err = api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer:     remote.peer(),
			Media:    &tg.InputMediaUploadedPhoto{File: file},
			Message:  marker + " A locally generated study of a quiet imaginary landscape.",
			RandomID: randomID(marker),
		})
		if err != nil {
			return fmt.Errorf("post generated channel photo: %w", err)
		}
	}
	if _, ok := markers[pollMarker(remote.spec)]; !ok {
		marker := pollMarker(remote.spec)
		_, err := api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
			Peer:     remote.peer(),
			Media:    demoPoll(),
			Message:  marker + " Choose a detail for the next imaginary route sketch.",
			RandomID: randomID(marker),
		})
		if err != nil {
			return fmt.Errorf("post channel poll: %w", err)
		}
	}
	if _, ok := markers[linkMarker(remote.spec)]; !ok {
		marker := linkMarker(remote.spec)
		link := fmt.Sprintf("https://%s.field-notes.invalid/walk-%02d", remote.spec.slug, 1)
		if _, err := sendText(ctx, api, remote.peer(), marker, "A reserved fictional link for the route notes: "+link, 0); err != nil {
			return fmt.Errorf("post fictional channel link: %w", err)
		}
	}
	if err := pinMarker(ctx, api, remote.peer(), rootMarker); err != nil {
		return fmt.Errorf("pin channel introduction: %w", err)
	}
	return nil
}

func channelText(spec spaceSpec, index int) string {
	observations := []string{
		"the lower path stayed cool beneath the willow shade",
		"a pair of paper markers made the fork easier to follow",
		"the little footbridge was dry before the afternoon clouds",
		"the north-facing stones held a soft silver color",
		"a quiet bench sits just beyond the bend in the trail",
		"the garden gate makes a useful start for a short loop",
		"the blue path ribbon is visible from the meadow edge",
		"the light changed quickly over the hill after lunch",
		"the old route sketch needs one more stream crossing",
		"a narrow track follows the edge of the open field",
		"the pond path is gentle and has room to stop safely",
		"a small grove offers shelter when the breeze picks up",
		"the wide gravel section is easier to mark on a map",
		"the clouds left a pale strip of light along the ridge",
	}
	return fmt.Sprintf("%s note %02d: %s.", spec.title, index+1, observations[index%len(observations)])
}

func demoPoll() *tg.InputMediaPoll {
	return &tg.InputMediaPoll{Poll: tg.Poll{
		Question: tg.TextWithEntities{Text: "Which imaginary landmark should the next route sketch include?"},
		Answers: []tg.PollAnswerClass{
			&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "A small footbridge"}},
			&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "A shaded garden gate"}},
			&tg.InputPollAnswer{Text: tg.TextWithEntities{Text: "A hilltop bench"}},
		},
	}}
}

func inviteConversationAccounts(ctx context.Context, api *tg.Client, remote remoteSpace) error {
	for _, account := range seedAccounts[1:4] {
		user, err := findInputUser(ctx, api, account.username)
		if err != nil {
			return fmt.Errorf("find public conversation account: %w", err)
		}
		_, err = api.ChannelsInviteToChannel(ctx, &tg.ChannelsInviteToChannelRequest{
			Channel: remote.inputChannel(),
			Users:   []tg.InputUserClass{user},
		})
		if err != nil && !isRPCMessage(err, "USER_ALREADY_PARTICIPANT") && !isRPCMessage(err, "USER_ALREADY_IN_CHANNEL") {
			return fmt.Errorf("invite demo account to public group: %w", err)
		}
	}
	return nil
}

func findInputUser(ctx context.Context, api *tg.Client, username string) (*tg.InputUser, error) {
	found, err := api.ContactsSearch(ctx, &tg.ContactsSearchRequest{Q: username, Limit: 50})
	if err != nil {
		return nil, err
	}
	for _, candidate := range found.Users {
		user, ok := candidate.(*tg.User)
		if ok && user.Username == username && user.AccessHash != 0 {
			return &tg.InputUser{UserID: user.ID, AccessHash: user.AccessHash}, nil
		}
	}
	return nil, fmt.Errorf("public account %s was not found in search", username)
}

func seedGroupConversations(ctx context.Context, cfg apiConfig, options seedOptions, ownerPassword string, inventory map[string]string, remotes []remoteSpace) error {
	groups := make([]remoteSpace, 0, 3)
	for _, remote := range remotes {
		if remote.spec.megagroup {
			groups = append(groups, remote)
		}
	}
	current := make([]map[string]int64, len(groups))
	if err := withSeedOwner(ctx, cfg, options, ownerPassword, func(runCtx context.Context, api *tg.Client) error {
		for index, group := range groups {
			markers, err := historyMarkers(runCtx, api, group.peer())
			if err != nil {
				return err
			}
			current[index] = markers
		}
		return nil
	}); err != nil {
		return err
	}
	for turn := range conversationLines {
		account := seedAccounts[1+turn%3]
		if err := withSeedAccount(ctx, cfg, options, account, ownerPassword, inventory, func(runCtx context.Context, api *tg.Client) error {
			for groupIndex, original := range groups {
				remote, err := resolvePublicSpace(runCtx, api, original.spec)
				if err != nil {
					return err
				}
				marker := conversationMarker(remote.spec, turn)
				if _, ok := current[groupIndex][marker]; ok {
					continue
				}
				replyTo := int64(0)
				if turn > 0 {
					replyTo = current[groupIndex][conversationMarker(remote.spec, turn-1)]
					if replyTo == 0 {
						return fmt.Errorf("group %s reply parent is missing", remote.spec.username)
					}
				}
				id, err := sendText(runCtx, api, remote.peer(), marker, conversationLines[turn], replyTo)
				if err != nil {
					return fmt.Errorf("post group conversation message: %w", err)
				}
				current[groupIndex][marker] = id
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func pinPublicGroups(ctx context.Context, cfg apiConfig, options seedOptions, ownerPassword string, remotes []remoteSpace) error {
	return withSeedOwner(ctx, cfg, options, ownerPassword, func(runCtx context.Context, api *tg.Client) error {
		for _, remote := range remotes {
			if !remote.spec.megagroup {
				continue
			}
			if err := pinMarker(runCtx, api, remote.peer(), conversationMarker(remote.spec, 1)); err != nil {
				return fmt.Errorf("pin group conversation in %s: %w", remote.spec.username, err)
			}
		}
		return nil
	})
}

func seedReactionExample(ctx context.Context, cfg apiConfig, options seedOptions, inventory map[string]string) error {
	sender := seedAccounts[1]
	receiver := seedAccounts[2]
	var messageID int
	if err := withSeedAccount(ctx, cfg, options, sender, "", inventory, func(runCtx context.Context, api *tg.Client) error {
		peer, err := resolveUserPeer(runCtx, api, receiver.username)
		if err != nil {
			return err
		}
		marker := "[[teagram-demo:conversation:reaction-example]]"
		markers, err := historyMarkers(runCtx, api, peer)
		if err != nil {
			return err
		}
		if id, ok := markers[marker]; ok {
			messageID = int(id)
			return nil
		}
		id, err := sendText(runCtx, api, peer, marker, "I saved the public route summary here too, in case it helps with the next walk.", 0)
		if err != nil {
			return err
		}
		messageID = int(id)
		return nil
	}); err != nil {
		return err
	}
	return withSeedAccount(ctx, cfg, options, receiver, "", inventory, func(runCtx context.Context, api *tg.Client) error {
		peer, err := resolveUserPeer(runCtx, api, sender.username)
		if err != nil {
			return err
		}
		markers, err := historyMarkers(runCtx, api, peer)
		if err != nil {
			return err
		}
		if id, ok := markers["[[teagram-demo:conversation:reaction-example]]"]; ok {
			messageID = int(id)
		}
		if messageID <= 0 {
			return errors.New("reaction example message was not found")
		}
		_, err = api.MessagesSendReaction(runCtx, seedReactionRequest(peer, messageID))
		if err != nil {
			return fmt.Errorf("react to fictional message: %w", err)
		}
		return nil
	})
}

func seedReactionRequest(peer tg.InputPeerClass, messageID int) *tg.MessagesSendReactionRequest {
	return &tg.MessagesSendReactionRequest{
		Peer:     peer,
		MsgID:    messageID,
		Reaction: []tg.ReactionClass{&tg.ReactionEmoji{Emoticon: seedReactionEmoji}},
	}
}

func resolveUserPeer(ctx context.Context, api *tg.Client, username string) (tg.InputPeerClass, error) {
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return nil, err
	}
	for _, candidate := range resolved.Users {
		user, ok := candidate.(*tg.User)
		if ok && user.Username == username && user.AccessHash != 0 {
			return &tg.InputPeerUser{UserID: user.ID, AccessHash: user.AccessHash}, nil
		}
	}
	return nil, fmt.Errorf("public account %s was not resolved", username)
}

func verifySearchResult(ctx context.Context, api *tg.Client, spec spaceSpec) error {
	found, err := api.ContactsSearch(ctx, &tg.ContactsSearchRequest{Q: spec.username, Limit: 100})
	if err != nil {
		return fmt.Errorf("search for public demo space %s: %w", spec.username, err)
	}
	for _, chat := range found.Chats {
		channel, ok := chat.(*tg.Channel)
		if ok && channel.Username == spec.username && channel.Title == spec.title && channel.Megagroup == spec.megagroup {
			return nil
		}
	}
	return fmt.Errorf("fresh-account search did not find public space %s", spec.username)
}

func historyMarkers(ctx context.Context, api *tg.Client, peer tg.InputPeerClass) (map[string]int64, error) {
	history, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer, Limit: 100})
	if err != nil {
		return nil, fmt.Errorf("read seeded message history: %w", err)
	}
	var messages []tg.MessageClass
	switch value := history.(type) {
	case *tg.MessagesChannelMessages:
		messages = value.Messages
	case *tg.MessagesMessagesSlice:
		messages = value.Messages
	case *tg.MessagesMessages:
		messages = value.Messages
	default:
		return nil, fmt.Errorf("unexpected message history response %T", history)
	}
	markers := make(map[string]int64, len(messages))
	for _, candidate := range messages {
		message, ok := candidate.(*tg.Message)
		if !ok {
			continue
		}
		marker := markerFromText(message.Message)
		if marker != "" {
			if previous, exists := markers[marker]; exists && previous != int64(message.ID) {
				return nil, fmt.Errorf("duplicate seeded message marker %s", marker)
			}
			markers[marker] = int64(message.ID)
		}
	}
	return markers, nil
}

func expectedSeedMarkerCount(spec spaceSpec) int {
	switch {
	case spec.slug == "welcome":
		return 1
	case spec.megagroup:
		return len(conversationLines)
	default:
		return len(broadcastMarkers(spec))
	}
}

func countSeedMarkers(markers map[string]int64, spec spaceSpec) int {
	prefix := fmt.Sprintf("[[teagram-demo:%s:", spec.slug)
	count := 0
	for marker := range markers {
		if strings.HasPrefix(marker, prefix) {
			count++
		}
	}
	return count
}

func markerFromText(value string) string {
	start := strings.Index(value, "[[teagram-demo:")
	if start < 0 {
		return ""
	}
	end := strings.Index(value[start:], "]]")
	if end < 0 {
		return ""
	}
	return value[start : start+end+2]
}

func sendText(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, marker, body string, replyTo int64) (int64, error) {
	request := &tg.MessagesSendMessageRequest{
		Peer:      peer,
		Message:   marker + " " + body,
		RandomID:  randomID(marker),
		NoWebpage: true,
	}
	if replyTo > 0 {
		request.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: int(replyTo)})
	}
	result, err := api.MessagesSendMessage(ctx, request)
	if err != nil {
		return 0, err
	}
	id := messageIDFromUpdates(result, request.RandomID)
	if id <= 0 {
		return 0, errors.New("message send response omitted its message id")
	}
	return int64(id), nil
}

func messageIDFromUpdates(result tg.UpdatesClass, random int64) int {
	var updates []tg.UpdateClass
	switch value := result.(type) {
	case *tg.Updates:
		updates = value.Updates
	case *tg.UpdatesCombined:
		updates = value.Updates
	}
	for _, update := range updates {
		if messageID, ok := update.(*tg.UpdateMessageID); ok && messageID.RandomID == random {
			return messageID.ID
		}
	}
	return 0
}

func uploadInputFile(ctx context.Context, api *tg.Client, payload []byte, name string) (*tg.InputFile, error) {
	if len(payload) == 0 {
		return nil, errors.New("cannot upload an empty generated image")
	}
	const partSize = 512 * 1024
	parts := (len(payload) + partSize - 1) / partSize
	fileID, err := randomInt64()
	if err != nil {
		return nil, fmt.Errorf("generate upload id: %w", err)
	}
	for part := range parts {
		start := part * partSize
		end := min(len(payload), start+partSize)
		accepted, err := api.UploadSaveFilePart(ctx, &tg.UploadSaveFilePartRequest{FileID: fileID, FilePart: part, Bytes: payload[start:end]})
		if err != nil {
			return nil, fmt.Errorf("upload generated image part: %w", err)
		}
		if !accepted {
			return nil, errors.New("server did not accept a generated image part")
		}
	}
	checksum := md5.Sum(payload) //nolint:gosec // MTProto requires this upload integrity checksum.
	return &tg.InputFile{ID: fileID, Parts: parts, Name: name, MD5Checksum: hex.EncodeToString(checksum[:])}, nil
}

func pinMarker(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, marker string) error {
	markers, err := historyMarkers(ctx, api, peer)
	if err != nil {
		return err
	}
	messageID, ok := markers[marker]
	if !ok {
		return errors.New("cannot pin a missing seeded message")
	}
	_, err = api.MessagesUpdatePinnedMessage(ctx, &tg.MessagesUpdatePinnedMessageRequest{
		Peer: peer,
		ID:   int(messageID),
	})
	return err
}

func messageIsPinned(ctx context.Context, api *tg.Client, peer tg.InputPeerClass, marker string) error {
	markers, err := historyMarkers(ctx, api, peer)
	if err != nil {
		return err
	}
	messageID, ok := markers[marker]
	if !ok {
		return errors.New("seeded message is missing")
	}
	channel, ok := peer.(*tg.InputPeerChannel)
	if !ok {
		return errors.New("pinned demo message belongs to a non-channel peer")
	}
	full, err := api.ChannelsGetFullChannel(ctx, &tg.InputChannel{ChannelID: channel.ChannelID, AccessHash: channel.AccessHash})
	if err != nil {
		return fmt.Errorf("read pinned demo message state: %w", err)
	}
	pinnedID, ok := channelPinnedMessageID(full.FullChat)
	if !ok || int64(pinnedID) != messageID {
		return errors.New("seeded message is not pinned")
	}
	return nil
}

func channelPinnedMessageID(full tg.ChatFullClass) (int, bool) {
	channel, ok := full.(*tg.ChannelFull)
	if !ok {
		return 0, false
	}
	return channel.GetPinnedMsgID()
}
