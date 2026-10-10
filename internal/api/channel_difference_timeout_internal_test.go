package api

import (
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
)

func TestSetDefaultChannelDifferenceTimeoutCoversEveryConstructor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		response bin.Encoder
	}{
		{name: "empty", response: &tg.UpdatesChannelDifferenceEmpty{}},
		{name: "too long", response: &tg.UpdatesChannelDifferenceTooLong{}},
		{name: "difference", response: &tg.UpdatesChannelDifference{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setDefaultChannelDifferenceTimeout(tc.response)
			assertDefaultChannelDifferenceTimeout(t, tc.response)
		})
	}
}

func assertDefaultChannelDifferenceTimeout(t *testing.T, response bin.Encoder) {
	t.Helper()
	var timeout int
	var ok bool
	var flags bin.Fields
	switch difference := response.(type) {
	case *tg.UpdatesChannelDifferenceEmpty:
		timeout, ok = difference.GetTimeout()
		flags = difference.Flags
	case *tg.UpdatesChannelDifferenceTooLong:
		timeout, ok = difference.GetTimeout()
		flags = difference.Flags
	case *tg.UpdatesChannelDifference:
		timeout, ok = difference.GetTimeout()
		flags = difference.Flags
	default:
		t.Fatalf("response type = %T, want a channel difference", response)
	}
	if !ok || timeout != 30 {
		t.Fatalf("timeout = %d, present = %v, want 30 seconds", timeout, ok)
	}
	if !flags.Has(1) {
		t.Fatal("timeout flag is unset")
	}
}
