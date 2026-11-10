package main

import "testing"

func TestValidateServerUpstream(t *testing.T) {
	t.Parallel()

	if err := validateServerUpstream(""); err == nil {
		t.Fatal("validateServerUpstream accepted an empty upstream")
	}
	if err := validateServerUpstream("http://teagramd:2446"); err != nil {
		t.Fatalf("validateServerUpstream: %v", err)
	}
}
