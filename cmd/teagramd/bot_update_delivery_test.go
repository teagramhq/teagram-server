package main

import (
	"context"
	"reflect"
	"testing"
)

type botUpdateNotifierFunc func(int64)

func (f botUpdateNotifierFunc) NotifyUpdates(ownerID int64) {
	f(ownerID)
}

func TestBotUpdateDeliveryNotifiesPollsBeforeDelivering(t *testing.T) {
	const ownerID int64 = 42
	var calls []string
	notifier := botUpdateNotifierFunc(func(got int64) {
		if got != ownerID {
			t.Errorf("notified owner = %d, want %d", got, ownerID)
		}
		calls = append(calls, "notify")
	})
	deliver := func(_ context.Context, got int64) {
		if got != ownerID {
			t.Errorf("delivered owner = %d, want %d", got, ownerID)
		}
		calls = append(calls, "deliver")
	}

	deliverWithBotAPINotify(context.Background(), notifier, deliver, ownerID)
	if want := []string{"notify", "deliver"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("delivery calls = %v, want %v", calls, want)
	}
}
