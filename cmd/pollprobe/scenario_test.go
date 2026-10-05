package main

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
)

func TestChannelUpdateCaptureDropsUnrelatedTrafficAndCapsEntries(t *testing.T) {
	probe := newProbe(probeConfig{
		scenario: probeScenarioChannels,
		creds: [4]accountCredential{
			{username: "synthpoll_a"},
			{username: "synthpoll_b"},
			{username: "synthpoll_c"},
			{username: "synthpoll_d"},
		},
	}, io.Discard)
	access, ok := any(probe).(interface {
		addChannelPollCapture(string, int64) error
		channelUpdateHandler(string) (telegram.UpdateHandler, error)
		channelUpdateSnapshot(string) []tg.UpdateClass
	})
	if !ok {
		t.Fatal("probe does not provide bounded channel update capture")
	}
	if err := access.addChannelPollCapture("synthpoll_c", 42); err != nil {
		t.Fatalf("register run-created poll: %v", err)
	}
	handler, err := access.channelUpdateHandler("synthpoll_c")
	if err != nil {
		t.Fatalf("get C update handler: %v", err)
	}
	updates := make([]tg.UpdateClass, 0, 67)
	updates = append(updates,
		&tg.UpdateNewMessage{},
		&tg.UpdateMessagePoll{PollID: 43},
		&tg.UpdateEditChannelMessage{},
	)
	for range 65 {
		updates = append(updates, &tg.UpdateMessagePoll{PollID: 42})
	}
	if err := handler.Handle(context.Background(), &tg.Updates{Updates: updates}); err != nil {
		t.Fatalf("capture matching poll updates: %v", err)
	}
	captured := access.channelUpdateSnapshot("synthpoll_c")
	if len(captured) != 64 {
		t.Fatalf("captured entries = %d, want bounded maximum 64", len(captured))
	}
	for index, update := range captured {
		poll, ok := update.(*tg.UpdateMessagePoll)
		if !ok || poll.PollID != 42 {
			t.Fatalf("captured update %d = %#v, want only registered poll 42 result", index, update)
		}
	}
}

func TestChannelUpdateCaptureUnwrapsShortPollPush(t *testing.T) {
	probe := newProbe(probeConfig{
		scenario: probeScenarioChannels,
		creds: [4]accountCredential{
			{username: "synthpoll_a"},
			{username: "synthpoll_b"},
			{username: "synthpoll_c"},
			{username: "synthpoll_d"},
		},
	}, io.Discard)
	access, ok := any(probe).(interface {
		addChannelPollCapture(string, int64) error
		channelUpdateHandler(string) (telegram.UpdateHandler, error)
		channelUpdateSnapshot(string) []tg.UpdateClass
	})
	if !ok {
		t.Fatal("probe does not provide bounded channel update capture")
	}
	if err := access.addChannelPollCapture("synthpoll_c", 42); err != nil {
		t.Fatalf("register run-created poll: %v", err)
	}
	handler, err := access.channelUpdateHandler("synthpoll_c")
	if err != nil {
		t.Fatalf("get C update handler: %v", err)
	}
	if err := handler.Handle(context.Background(), &tg.UpdateShort{Update: &tg.UpdateMessagePoll{PollID: 42}}); err != nil {
		t.Fatalf("capture UpdateShort poll result: %v", err)
	}
	if got := len(access.channelUpdateSnapshot("synthpoll_c")); got != 1 {
		t.Fatalf("captured entries = %d, want 1 matching poll result", got)
	}
}

func TestChannelUpdateCaptureKeepsMatchingPollEdits(t *testing.T) {
	probe := newProbe(probeConfig{
		scenario: probeScenarioChannels,
		creds: [4]accountCredential{
			{username: "synthpoll_a"},
			{username: "synthpoll_b"},
			{username: "synthpoll_c"},
			{username: "synthpoll_d"},
		},
	}, io.Discard)
	if err := probe.addChannelPollCapture("synthpoll_c", 42); err != nil {
		t.Fatalf("register run-created poll: %v", err)
	}
	edit := func(pollID int64) tg.UpdateClass {
		return &tg.UpdateEditChannelMessage{Message: &tg.Message{
			Media: &tg.MessageMediaPoll{Poll: tg.Poll{ID: pollID}},
		}}
	}
	if err := probe.account("synthpoll_c").capture.Handle(context.Background(), &tg.Updates{
		Updates: []tg.UpdateClass{edit(43), edit(42)},
	}); err != nil {
		t.Fatalf("capture matching poll edit: %v", err)
	}
	captured := probe.channelUpdateSnapshot("synthpoll_c")
	if len(captured) != 1 {
		t.Fatalf("captured edits = %d, want one matching run poll", len(captured))
	}
	if pollID, ok := capturedChannelPollID(captured[0]); !ok || pollID != 42 {
		t.Fatalf("captured edit poll id = %d, matched = %t; want run poll 42", pollID, ok)
	}
}

func TestAwaitChannelPollAfterIgnoresPriorUpdates(t *testing.T) {
	probe := newProbe(probeConfig{
		scenario: probeScenarioChannels,
		creds: [4]accountCredential{
			{username: "synthpoll_a"},
			{username: "synthpoll_b"},
			{username: "synthpoll_c"},
			{username: "synthpoll_d"},
		},
	}, io.Discard)
	if err := probe.addChannelPollCapture("synthpoll_c", 42); err != nil {
		t.Fatalf("register run-created poll: %v", err)
	}
	account := probe.account("synthpoll_c")
	if err := account.capture.Handle(context.Background(), &tg.Updates{Updates: []tg.UpdateClass{
		&tg.UpdateMessagePoll{PollID: 42, Results: tg.PollResults{TotalVoters: 1}},
	}}); err != nil {
		t.Fatalf("capture prior poll update: %v", err)
	}
	baseline := len(probe.channelUpdateSnapshot(account.username))
	fresh := &tg.UpdateMessagePoll{PollID: 42, Results: tg.PollResults{TotalVoters: 2}}
	if err := account.capture.Handle(context.Background(), &tg.Updates{Updates: []tg.UpdateClass{fresh}}); err != nil {
		t.Fatalf("capture poll update after baseline: %v", err)
	}
	got, err := probe.awaitChannelPollAfter(context.Background(), account, 42, baseline, "channel_reconnect_live_capture", func(update *tg.UpdateMessagePoll) error {
		if update.Results.TotalVoters != 2 {
			return failure("channel_reconnect_live_capture", "RECONNECTED_POLL_UPDATE_MISMATCH")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("wait for poll update after baseline: %v", err)
	}
	if got != fresh {
		t.Fatal("await returned an update captured before the baseline")
	}
}

func TestAnonymousVoteRecoveryStatusValidatesViewerResults(t *testing.T) {
	tests := []struct {
		name      string
		results   tg.PollResults
		wantFound bool
		wantError string
	}{
		{
			name: "one vote for B without viewer or voter identity",
			results: tg.PollResults{
				TotalVoters: 1,
				Results: []tg.PollAnswerVoters{
					{Option: []byte("A"), Voters: 0},
					{Option: []byte("B"), Voters: 1},
				},
			},
			wantFound: true,
		},
		{
			name: "stale zero-vote counts",
			results: tg.PollResults{
				Results: []tg.PollAnswerVoters{
					{Option: []byte("A"), Voters: 0},
					{Option: []byte("B"), Voters: 0},
				},
			},
			wantFound: true,
			wantError: "POLL_RESULT_MISMATCH",
		},
		{
			name: "wrong answer count",
			results: tg.PollResults{
				TotalVoters: 1,
				Results: []tg.PollAnswerVoters{
					{Option: []byte("A"), Voters: 1},
					{Option: []byte("B"), Voters: 0},
				},
			},
			wantFound: true,
			wantError: "POLL_RESULT_MISMATCH",
		},
		{
			name: "chosen answer exposed",
			results: tg.PollResults{
				TotalVoters: 1,
				Results: []tg.PollAnswerVoters{
					{Option: []byte("A"), Voters: 0},
					{Option: []byte("B"), Voters: 1, Chosen: true},
				},
			},
			wantFound: true,
			wantError: "VOTER_CHOICE_EXPOSED",
		},
		{
			name: "global voter identity exposed",
			results: tg.PollResults{
				TotalVoters: 1,
				Results: []tg.PollAnswerVoters{
					{Option: []byte("A"), Voters: 0},
					{Option: []byte("B"), Voters: 1},
				},
				RecentVoters: []tg.PeerClass{&tg.PeerUser{UserID: 7}},
			},
			wantFound: true,
			wantError: "VOTER_IDENTITY_EXPOSED",
		},
		{
			name: "answer voter identity exposed",
			results: tg.PollResults{
				TotalVoters: 1,
				Results: []tg.PollAnswerVoters{
					{Option: []byte("A"), Voters: 0},
					{Option: []byte("B"), Voters: 1, RecentVoters: []tg.PeerClass{&tg.PeerUser{UserID: 7}}},
				},
			},
			wantFound: true,
			wantError: "VOTER_IDENTITY_EXPOSED",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			found, err := anonymousVoteRecoveryStatus([]tg.UpdateClass{
				&tg.UpdateMessagePoll{PollID: 42, Results: test.results},
			}, 42, []byte("A"), []byte("B"))
			if found != test.wantFound {
				t.Fatalf("found = %t, want %t", found, test.wantFound)
			}
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			probeErr, ok := errors.AsType[*probeError](err)
			if !ok || probeErr.errorCode != test.wantError {
				t.Fatalf("error = %v, want error code %q", err, test.wantError)
			}
		})
	}
}

func TestAnonymousVoteRecoveryStatusIgnoresOtherPolls(t *testing.T) {
	found, err := anonymousVoteRecoveryStatus([]tg.UpdateClass{
		&tg.UpdateMessagePoll{PollID: 41, Results: tg.PollResults{TotalVoters: 1}},
	}, 42, []byte("A"), []byte("B"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatal("recovery status found an update for a different poll")
	}
}

func TestAnonymousVoteRecoveryStatusValidatesEditedPollResults(t *testing.T) {
	found, err := anonymousVoteRecoveryStatus([]tg.UpdateClass{
		&tg.UpdateEditMessage{Message: &tg.Message{Media: &tg.MessageMediaPoll{
			Poll: tg.Poll{ID: 42},
			Results: tg.PollResults{
				TotalVoters: 1,
				Results: []tg.PollAnswerVoters{
					{Option: []byte("A"), Voters: 1},
					{Option: []byte("B"), Voters: 0},
				},
			},
		}}},
	}, 42, []byte("A"), []byte("B"))
	if !found {
		t.Fatal("recovery status did not find the edited poll")
	}
	probeErr, ok := errors.AsType[*probeError](err)
	if !ok || probeErr.errorCode != "POLL_RESULT_MISMATCH" {
		t.Fatalf("error = %v, want POLL_RESULT_MISMATCH", err)
	}
}

func TestChannelAnonymousVoteResponseKeepsChosenAnswerViewerScoped(t *testing.T) {
	update := &tg.UpdateMessagePoll{
		PollID: 42,
		Results: tg.PollResults{
			TotalVoters: 1,
			Results: []tg.PollAnswerVoters{
				{Option: []byte("A"), Voters: 0},
				{Option: []byte("B"), Voters: 1, Chosen: true},
			},
		},
	}
	if !anonymousChannelVoteMatches(update, 42, 1, map[string]int{"A": 0, "B": 1}) {
		t.Fatal("anonymous vote response rejected the voter's own chosen answer")
	}
}

func TestChannelQuizResultsKeepVoterIdentityViewerScoped(t *testing.T) {
	nonVoter := tg.PollResults{
		TotalVoters: 1,
		Results: []tg.PollAnswerVoters{
			{Option: []byte("A"), Voters: 0},
			{Option: []byte("B"), Voters: 1},
		},
	}
	if !nonVoterQuizResults(nonVoter) {
		t.Fatal("quiz results exposed the solution to a non-voter")
	}
	nonVoter.RecentVoters = []tg.PeerClass{&tg.PeerUser{UserID: 7}}
	if nonVoterQuizResults(nonVoter) {
		t.Fatal("quiz results exposed voter identities to a non-voter")
	}

	voter := &tg.PollResults{
		TotalVoters: 1,
		Solution:    "correct answer",
		Results: []tg.PollAnswerVoters{
			{Option: []byte("A"), Voters: 0},
			{Option: []byte("B"), Voters: 1, Correct: true, Chosen: true},
		},
	}
	if !voterQuizResultsMatch(voter, "correct answer") {
		t.Fatal("quiz voter did not receive the solution and selected correct answer")
	}
	voter.RecentVoters = []tg.PeerClass{&tg.PeerUser{UserID: 7}}
	if voterQuizResultsMatch(voter, "correct answer") {
		t.Fatal("quiz results exposed voter identities to the voter")
	}
}

func TestAnonymousGetPollResultsStatusRejectsChosenAnswerForNonVoter(t *testing.T) {
	err := anonymousPollResultsStatus(&tg.PollResults{
		TotalVoters: 1,
		Results: []tg.PollAnswerVoters{
			{Option: []byte("A"), Voters: 0},
			{Option: []byte("B"), Voters: 1, Chosen: true},
		},
	}, []byte("A"), []byte("B"))
	probeErr, ok := errors.AsType[*probeError](err)
	if !ok || probeErr.assertion != "anonymous_vote_privacy" || probeErr.errorCode != "VOTER_CHOICE_EXPOSED" {
		t.Fatalf("error = %v, want anonymous_vote_privacy/VOTER_CHOICE_EXPOSED", err)
	}
}

func TestAnonymousGetPollResultsStatusAllowsUnchosenAnswersForNonVoter(t *testing.T) {
	err := anonymousPollResultsStatus(&tg.PollResults{
		TotalVoters: 1,
		Results: []tg.PollAnswerVoters{
			{Option: []byte("A"), Voters: 0},
			{Option: []byte("B"), Voters: 1},
		},
	}, []byte("A"), []byte("B"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
