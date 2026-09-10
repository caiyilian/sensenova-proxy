package main

import (
	"net/http"
	"testing"
	"time"
)

func testAccounts() []Account {
	return []Account{
		{ID: "account-1", Fingerprint: "first", KeyRef: "11111111"},
		{ID: "account-2", Fingerprint: "second", KeyRef: "22222222"},
	}
}

func TestAccountPoolRotatesAndSkipsCoolingAccount(t *testing.T) {
	pool := NewAccountPool()
	accounts := testAccounts()
	first, ok := pool.Pick(accounts, "model-a", map[string]struct{}{})
	if !ok || first.ID != "account-1" {
		t.Fatalf("first pick = %+v, %v", first, ok)
	}
	second, ok := pool.Pick(accounts, "model-a", map[string]struct{}{})
	if !ok || second.ID != "account-2" {
		t.Fatalf("second pick = %+v, %v", second, ok)
	}

	pool.MarkFailure(first, "model-a", failureClass{Category: "rate_limit", Retryable: true, Scope: "model"}, 0)
	picked, ok := pool.Pick(accounts, "model-a", map[string]struct{}{})
	if !ok || picked.ID != "account-2" {
		t.Fatalf("pick with first account cooling = %+v, %v", picked, ok)
	}
	otherModel, ok := pool.Pick(accounts, "model-b", map[string]struct{}{})
	if !ok || otherModel.ID != "account-1" {
		t.Fatalf("per-model cooldown leaked into another model: %+v, %v", otherModel, ok)
	}
}

func TestAccountPoolQuarantinesAuthenticationFailure(t *testing.T) {
	pool := NewAccountPool()
	accounts := testAccounts()
	pool.MarkFailure(accounts[0], "model-a", failureClass{Category: "auth", Retryable: true, Scope: "account"}, 0)
	if got := pool.AuthQuarantinedCount(accounts); got != 1 {
		t.Fatalf("quarantined count = %d, want 1", got)
	}
	picked, ok := pool.Pick(accounts, "another-model", map[string]struct{}{})
	if !ok || picked.ID != "account-2" {
		t.Fatalf("auth-quarantined key was not skipped: %+v, %v", picked, ok)
	}
}

func TestFailureClassificationAndRetryAfter(t *testing.T) {
	if class := classifyFailure(http.StatusTooManyRequests, `{"error":"inference exceeds tpm/rpm limit"}`); class.Category != "rate_limit" || !class.Retryable {
		t.Fatalf("unexpected rate-limit class: %+v", class)
	}
	if class := classifyFailure(http.StatusUnauthorized, `{"error":"invalid api key"}`); class.Category != "auth" || class.Scope != "account" {
		t.Fatalf("unexpected auth class: %+v", class)
	}
	if got := parseRetryAfter("12"); got != 12*time.Second {
		t.Fatalf("Retry-After duration = %s, want 12s", got)
	}
}
