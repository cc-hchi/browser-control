package main

import (
	"testing"

	"github.com/cc-hchi/browser-control/internal/bridgeauth"
)

func TestValidateInvocationOrigin(t *testing.T) {
	if err := validateInvocationOrigin([]string{bridgeauth.StableExtensionOrigin}); err != nil {
		t.Fatalf("stable extension origin rejected: %v", err)
	}
	for _, args := range [][]string{
		nil,
		{"chrome-extension://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/"},
		{bridgeauth.StableExtensionOrigin, "extra"},
	} {
		if err := validateInvocationOrigin(args); err == nil {
			t.Fatalf("validateInvocationOrigin(%q) unexpectedly succeeded", args)
		}
	}
}
