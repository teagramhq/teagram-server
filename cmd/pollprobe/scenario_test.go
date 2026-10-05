package main

import (
	"errors"
	"testing"

	"github.com/gotd/td/tg"
)

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
			}, 42)
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
	}, 42)
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
	}, 42)
	if !found {
		t.Fatal("recovery status did not find the edited poll")
	}
	probeErr, ok := errors.AsType[*probeError](err)
	if !ok || probeErr.errorCode != "POLL_RESULT_MISMATCH" {
		t.Fatalf("error = %v, want POLL_RESULT_MISMATCH", err)
	}
}

func TestAnonymousGetPollResultsStatusRejectsChosenAnswerForNonVoter(t *testing.T) {
	err := anonymousPollResultsStatus(&tg.PollResults{
		TotalVoters: 1,
		Results: []tg.PollAnswerVoters{
			{Option: []byte("A"), Voters: 0},
			{Option: []byte("B"), Voters: 1, Chosen: true},
		},
	})
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
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
