// Unit test for HandleGetPlaybookSignatures
// (Wave ai-analysis-staleness-tracking).
//
// The endpoint returns the in-binary playbook content digests so
// the dashboard can detect when a cached AI analysis was anchored
// on a now-stale playbook. Tests pin:
//
//   - Auth: missing project context returns 401 (no signatures
//     leak; though the content itself is identical across projects,
//     the endpoint still enforces the standard /me/* auth chain).
//   - Happy path: returns a non-empty `signatures` map with hex
//     digests for every registered playbook.
//   - Consistency: each entry value matches what playbooks.Signature
//     would return for that failure_class.
package api

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mesedi/backend/internal/playbooks"
)

func Test_HandleGetPlaybookSignatures_NoProjectContext_401(t *testing.T) {
	h := &Handlers{Logger: quietLogger()}
	r := httptest.NewRequest(http.MethodGet, "/me/playbook-signatures", nil)
	w := httptest.NewRecorder()

	h.HandleGetPlaybookSignatures(w, r)

	if w.Code == http.StatusOK {
		t.Fatalf("expected non-200 for missing project context, got 200")
	}
}

func Test_HandleGetPlaybookSignatures_HappyPath(t *testing.T) {
	h := &Handlers{Logger: quietLogger()}
	r := httptest.NewRequest(http.MethodGet, "/me/playbook-signatures", nil)
	r = r.WithContext(withProjectID(r.Context(), "proj-test"))
	w := httptest.NewRecorder()

	h.HandleGetPlaybookSignatures(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	var body struct {
		Signatures map[string]string `json:"signatures"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Signatures) == 0 {
		t.Fatal("expected non-empty signatures map")
	}
	// Each value MUST be a 64-character hex digest.
	for key, sig := range body.Signatures {
		if len(sig) != 64 {
			t.Errorf("entry %q: signature length %d, want 64", key, len(sig))
		}
		if _, err := hex.DecodeString(sig); err != nil {
			t.Errorf("entry %q: not valid hex: %v", key, err)
		}
	}
}

func Test_HandleGetPlaybookSignatures_ConsistentWithLibrary(t *testing.T) {
	// The endpoint's output MUST equal playbooks.AllSignatures() so
	// the dashboard's signature comparison stays correct.
	h := &Handlers{Logger: quietLogger()}
	r := httptest.NewRequest(http.MethodGet, "/me/playbook-signatures", nil)
	r = r.WithContext(withProjectID(r.Context(), "proj-test"))
	w := httptest.NewRecorder()
	h.HandleGetPlaybookSignatures(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body struct {
		Signatures map[string]string `json:"signatures"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	libSignatures := playbooks.AllSignatures()
	if len(body.Signatures) != len(libSignatures) {
		t.Errorf("endpoint returned %d entries, library has %d; sync broken",
			len(body.Signatures), len(libSignatures))
	}
	for key, libSig := range libSignatures {
		gotSig, ok := body.Signatures[key]
		if !ok {
			t.Errorf("endpoint missing key %q from library", key)
			continue
		}
		if gotSig != libSig {
			t.Errorf("endpoint mismatched key %q: got %q, library %q", key, gotSig, libSig)
		}
	}
}

func Test_HandleGetPlaybookSignatures_KnownDetectorsPresent(t *testing.T) {
	// Spot-check that representative detectors from each category
	// (catch-all, variant-prefix, security) appear in the response.
	h := &Handlers{Logger: quietLogger()}
	r := httptest.NewRequest(http.MethodGet, "/me/playbook-signatures", nil)
	r = r.WithContext(withProjectID(r.Context(), "proj-test"))
	w := httptest.NewRecorder()
	h.HandleGetPlaybookSignatures(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	required := []string{
		`"data_leakage"`,
		`"context_overflow"`,
		`"hitl_timeout"`,
		`"loops:identical_call_"`,
		`"drift:lexical_drift_"`,
		`"prompt_injection:jailbreak_dan"`,
	}
	for _, want := range required {
		if !strings.Contains(body, want) {
			t.Errorf("expected response body to contain %s; got %s", want, body)
		}
	}
}
