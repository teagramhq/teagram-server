package botapi

import (
	"testing"
	"time"
)

func TestForwardedAddrLogSamplerAllowsOneWarningPerMinute(t *testing.T) {
	var sampler forwardedAddrLogSampler
	now := time.Now()

	if _, ok := sampler.allow(now); !ok {
		t.Fatal("first ignored address was suppressed")
	}
	if _, ok := sampler.allow(now.Add(30 * time.Second)); ok {
		t.Fatal("ignored address was logged again within a minute")
	}
	if dropped, ok := sampler.allow(now.Add(time.Minute)); !ok || dropped != 1 {
		t.Fatalf("ignored address after a minute = (dropped %d, allowed %t), want (1, true)", dropped, ok)
	}
}
