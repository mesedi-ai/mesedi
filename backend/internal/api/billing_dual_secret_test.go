package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stripe/stripe-go/v82"
)

// signStripePayload mints a valid Stripe-Signature header value for
// the test. Mirrors the format Stripe itself sends:
//
//	t=<unix_seconds>,v1=<hex_hmac_sha256(<t>.<body>, secret)>
//
// stripe-go's webhook.ConstructEventWithOptions enforces the same
// shape on the receive side, so signatures minted by this helper are
// indistinguishable from real Stripe deliveries.
func signStripePayload(body []byte, secret string, ts time.Time) string {
	tsSec := ts.Unix()
	signed := fmt.Sprintf("%d.%s", tsSec, string(body))
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(signed))
	return fmt.Sprintf("t=%d,v1=%s", tsSec, hex.EncodeToString(mac.Sum(nil)))
}

// minimalEventJSON is a stub Stripe event body shaped enough that
// stripe-go's webhook.ConstructEventWithOptions parses it without
// complaining about missing fields. The id and type are stable
// markers the test can later assert on.
var minimalEventJSON = []byte(
	`{"id":"evt_test_dispute","type":"charge.dispute.created","data":{"object":{}}}`,
)

// TestConstructStripeEvent_LiveSecretMatches covers the common case:
// a live-mode webhook signed with the live secret validates against
// the live secret on the first attempt; the test secret is never
// consulted.
func TestConstructStripeEvent_LiveSecretMatches(t *testing.T) {
	liveSecret := "whsec_live_test_value"
	testSecret := "whsec_test_test_value"
	sig := signStripePayload(minimalEventJSON, liveSecret, time.Now())

	evt, matched, err := constructStripeEvent(minimalEventJSON, sig, liveSecret, testSecret)
	if err != nil {
		t.Fatalf("expected success on live secret, got: %v", err)
	}
	if matched != "live" {
		t.Fatalf("expected matched=\"live\", got %q", matched)
	}
	if evt.ID != "evt_test_dispute" {
		t.Fatalf("expected event id evt_test_dispute, got %q", evt.ID)
	}
}

// TestConstructStripeEvent_FallsBackToTestSecret covers the 
// fallback: a test-mode event signed with the test secret fails
// against the live secret on the first try, then succeeds against
// the test secret. The matched label should reflect "test" so the
// log line later shows the operator which mode this delivery came
// from.
func TestConstructStripeEvent_FallsBackToTestSecret(t *testing.T) {
	liveSecret := "whsec_live_test_value"
	testSecret := "whsec_test_test_value"
	sig := signStripePayload(minimalEventJSON, testSecret, time.Now())

	evt, matched, err := constructStripeEvent(minimalEventJSON, sig, liveSecret, testSecret)
	if err != nil {
		t.Fatalf("expected success on test secret fallback, got: %v", err)
	}
	if matched != "test" {
		t.Fatalf("expected matched=\"test\", got %q", matched)
	}
	if evt.ID != "evt_test_dispute" {
		t.Fatalf("expected event id evt_test_dispute, got %q", evt.ID)
	}
}

// TestConstructStripeEvent_BothFail covers the genuine signature
// failure path: the signature was minted with a third unknown secret
// (e.g. an attacker forging a payload, or a misconfigured endpoint
// that received traffic from the wrong Stripe account). Neither live
// nor test should accept it; the helper returns the live error as
// the canonical message so existing log scraping continues to work.
func TestConstructStripeEvent_BothFail(t *testing.T) {
	liveSecret := "whsec_live_test_value"
	testSecret := "whsec_test_test_value"
	bogusSecret := "whsec_attacker_value"
	sig := signStripePayload(minimalEventJSON, bogusSecret, time.Now())

	_, matched, err := constructStripeEvent(minimalEventJSON, sig, liveSecret, testSecret)
	if err == nil {
		t.Fatal("expected signature verification failure when neither secret matches")
	}
	if matched != "" {
		t.Fatalf("expected empty matched label on failure, got %q", matched)
	}
	// stripe-go surfaces "no valid signature" on the v1 scheme; sanity
	// check we got a recognizable signature error rather than a JSON
	// parse error.
	if !strings.Contains(strings.ToLower(err.Error()), "signature") {
		t.Fatalf("expected a signature-related error, got: %v", err)
	}
}

// TestConstructStripeEvent_TestSecretUnsetIsSilentFallback covers
// backwards compatibility: when the test secret is empty (the default
// state), the helper must behave identically to a single-
// secret validator. No bonus attempts, no panic, error matches what
// stripe-go returns directly.
func TestConstructStripeEvent_TestSecretUnsetIsSilentFallback(t *testing.T) {
	liveSecret := "whsec_live_test_value"
	wrongSecret := "whsec_other_value"
	sig := signStripePayload(minimalEventJSON, wrongSecret, time.Now())

	_, matched, err := constructStripeEvent(minimalEventJSON, sig, liveSecret, "")
	if err == nil {
		t.Fatal("expected signature failure when test secret is unset and live does not match")
	}
	if matched != "" {
		t.Fatalf("expected empty matched label on failure, got %q", matched)
	}
}

// TestApplyKeyForLivemode_LivemodeTrueUsesLiveSecret covers the
// dominant case: a live-mode event picks the live API key even when a
// test key is configured. The webhook handler can callback to Stripe
// for live objects (charges, customers) with the right credential.
func TestApplyKeyForLivemode_LivemodeTrueUsesLiveSecret(t *testing.T) {
	prev := stripe.Key
	defer func() { stripe.Key = prev }()

	cfg := StripeConfig{
		SecretKey:     "sk_live_unit_test",
		SecretKeyTest: "sk_test_unit_test",
	}
	cfg.applyKeyForLivemode(true)
	if stripe.Key != "sk_live_unit_test" {
		t.Fatalf("expected live key, got %q", stripe.Key)
	}
}

// TestApplyKeyForLivemode_LivemodeFalseUsesTestSecret covers the
// follow-on case: a test-mode event with a configured test API
// key picks the test key so callbacks like charge.Get can read
// test-mode objects.
func TestApplyKeyForLivemode_LivemodeFalseUsesTestSecret(t *testing.T) {
	prev := stripe.Key
	defer func() { stripe.Key = prev }()

	cfg := StripeConfig{
		SecretKey:     "sk_live_unit_test",
		SecretKeyTest: "sk_test_unit_test",
	}
	cfg.applyKeyForLivemode(false)
	if stripe.Key != "sk_test_unit_test" {
		t.Fatalf("expected test key, got %q", stripe.Key)
	}
}

// TestApplyKeyForLivemode_TestSecretUnsetFallsBackToLive covers
// backwards compatibility: when no test API key is configured, a
// test-mode event still ships using the live key. Signature
// validation already succeeded, so the receive log is useful; only
// the Stripe API callbacks 401 (handled gracefully by existing error
// paths).
func TestApplyKeyForLivemode_TestSecretUnsetFallsBackToLive(t *testing.T) {
	prev := stripe.Key
	defer func() { stripe.Key = prev }()

	cfg := StripeConfig{
		SecretKey:     "sk_live_unit_test",
		SecretKeyTest: "",
	}
	cfg.applyKeyForLivemode(false)
	if stripe.Key != "sk_live_unit_test" {
		t.Fatalf("expected live key fallback when test key unset, got %q", stripe.Key)
	}
}
