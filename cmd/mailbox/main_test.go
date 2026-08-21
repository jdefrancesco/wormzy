package main

import "testing"

// TestConfiguredMailboxRedisURL verifies credentials are read inside the process instead of argv.
func TestConfiguredMailboxRedisURL(t *testing.T) {
	t.Setenv("WORMZY_MAILBOX_REDIS", "rediss://env-secret@redis.example:6379")
	if got := configuredMailboxRedisURL("rediss://flag-secret@redis.example:6379"); got != "rediss://flag-secret@redis.example:6379" {
		t.Fatalf("explicit Redis URL = %q", got)
	}
	if got := configuredMailboxRedisURL(""); got != "rediss://env-secret@redis.example:6379" {
		t.Fatalf("environment Redis URL = %q", got)
	}
	t.Setenv("WORMZY_MAILBOX_REDIS", "")
	if got := configuredMailboxRedisURL(""); got != defaultMailboxRedisURL {
		t.Fatalf("default Redis URL = %q; want %q", got, defaultMailboxRedisURL)
	}
}
