package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest"
	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/dlp"
	"mesedi/backend/internal/store"
)

// chainHarness drives the real ingest and completion handlers on an
// in-memory database, building chained reports the way an SDK does.
type chainHarness struct {
	t       *testing.T
	st      *store.SQLiteStore
	h       *Handlers
	project string
	exec    string
	prev    string
	seq     int
	clock   time.Time
	priv    ed25519.PrivateKey
	// signed is set once a signed report is built; the declared end is
	// then signed too, as the SDKs do.
	signed bool
	// noEnd makes complete() send no declared end, as an older SDK.
	noEnd bool
}

func newChainHarness(t *testing.T) *chainHarness {
	t.Helper()
	logger := quietLogger()
	st, err := store.OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const project = "proj_chain"
	if err := st.CreateProject(ctx, &store.Project{ProjectID: project, Name: "chain", Tier: "hobby",
		CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	exec := fmt.Sprintf("exec_chain_%d", time.Now().UnixNano())
	if err := st.CreateExecution(ctx, &events.Execution{ExecutionID: exec, ProjectID: project,
		Status: events.StatusStarted, StartedAt: time.Now().UTC().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	scanner, err := dlp.NewScanner([]dlp.Rule{{ID: "test_secret", Label: "test", Severity: dlp.SeverityCritical,
		Pattern: `CHAINSECRET_[A-Z0-9]{8}`}})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{Logger: logger, Store: st, HaltSubs: NewHaltSubscribers(), DLPScanner: scanner}
	t.Cleanup(func() { h.DrainDispatches(); _ = st.Close() })
	_, priv, _ := ed25519.GenerateKey(nil)
	return &chainHarness{t: t, st: st, h: h, project: project, exec: exec,
		prev: attest.ReportChainGenesis(exec), clock: time.Now().UTC().Add(-30 * time.Second), priv: priv}
}

// next builds the next chained report; sign adds a signature.
func (c *chainHarness) next(payload string, sign bool) events.Event {
	c.seq++
	c.clock = c.clock.Add(time.Second)
	e := events.Event{EventID: fmt.Sprintf("%s_e%d", c.exec, c.seq), ExecutionID: c.exec,
		EventType: events.EventTypeToolCall, Sequence: c.seq, Timestamp: c.clock,
		Payload: json.RawMessage(payload), ChainPrev: c.prev}
	h := attest.ReportChainHash(&e, "")
	if sign {
		c.signed = true
		msg, _ := hex.DecodeString(h)
		e.Signature = &events.ReportSignature{KeyID: "harness-1",
			Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(c.priv, msg))}
	}
	c.prev = h
	return e
}

func (c *chainHarness) registerKey() {
	c.t.Helper()
	pub := c.priv.Public().(ed25519.PublicKey)
	if err := c.st.InsertReportSigningKey(context.Background(), c.project, "harness-1",
		base64.StdEncoding.EncodeToString(pub), "owner@example.invalid"); err != nil {
		c.t.Fatal(err)
	}
}

// ingest posts a batch and returns the rejected count.
func (c *chainHarness) ingest(batch ...events.Event) int {
	c.t.Helper()
	// Without HTML escaping, as the SDKs send: Go's default encoder
	// rewrites "<", ">" and "&", so the bytes received would not be the
	// bytes fingerprinted, and a signed report would rightly be refused.
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(batch); err != nil {
		c.t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/events", &body)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, c.project))
	rec := httptest.NewRecorder()
	c.h.HandleIngestEvents(rec, req)
	if rec.Code != 200 {
		c.t.Fatalf("ingest returned %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Rejected int `json:"rejected"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return resp.Rejected
}

// complete closes the run and returns its chain verdict and the
// record_integrity signatures raised.
func (c *chainHarness) complete() (status, detail string, sigs []string) {
	c.t.Helper()
	body := map[string]any{"status": "completed"}
	if !c.noEnd && c.seq > 0 {
		// The declared end: the last report built, as the SDK's run-
		// finished message carries it.
		end := map[string]any{"hash": c.prev}
		if c.signed {
			end["signature"] = map[string]string{"key_id": "harness-1",
				"sig": base64.StdEncoding.EncodeToString(ed25519.Sign(c.priv, attest.ReportChainEndMessage(c.exec, c.prev)))}
		}
		body["report_chain_end"] = end
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("PATCH", "/executions/"+c.exec, bytes.NewReader(raw))
	req.SetPathValue("id", c.exec)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, c.project))
	rec := httptest.NewRecorder()
	c.h.HandleUpdateExecution(rec, req)
	if rec.Code != 200 {
		c.t.Fatalf("PATCH returned %d: %s", rec.Code, rec.Body.String())
	}
	if row, err := c.st.GetReportChainStatus(context.Background(), c.exec); err == nil {
		status, detail = row.Status, row.Detail
	}
	groups, _ := c.st.ListFailureGroups(context.Background(), c.project, store.ListFailureGroupsOpts{Limit: 50})
	for _, g := range groups {
		if g.FailureClass == store.FailureClassRecordIntegrity && strings.Contains(g.Signature, "report_") {
			sigs = append(sigs, g.Signature)
		}
	}
	return status, detail, sigs
}

func TestReportChainIntactAcrossBatches(t *testing.T) {
	c := newChainHarness(t)
	e1, e2, e3 := c.next(`{"tool_name":"a"}`, false), c.next(`{"tool_name":"b"}`, false), c.next(`{"tool_name":"c"}`, false)
	// Sent out of order and in two batches, as a busy SDK may.
	if n := c.ingest(e2, e1); n != 0 {
		t.Fatalf("rejected %d", n)
	}
	c.ingest(e3)
	if status, detail, sigs := c.complete(); status != store.ReportChainChained || len(sigs) != 0 {
		t.Fatalf("status %q (%s), signals %v; want chained and quiet", status, detail, sigs)
	}
}

// Secret redaction rewrites the stored payload; the chain was taken
// from the bytes as received, so it still holds.
func TestReportChainSurvivesSecretRedaction(t *testing.T) {
	c := newChainHarness(t)
	c.ingest(c.next(`{"tool_name":"a","arguments":{"token":"CHAINSECRET_ABCD1234"}}`, false),
		c.next(`{"tool_name":"b"}`, false))
	if status, detail, sigs := c.complete(); status != store.ReportChainChained || len(sigs) != 0 {
		t.Fatalf("status %q (%s), signals %v", status, detail, sigs)
	}
}

func TestReportChainBreaks(t *testing.T) {
	cases := map[string]func(c *chainHarness){
		"a report never arrives": func(c *chainHarness) {
			e1 := c.next(`{"tool_name":"a"}`, false)
			_ = c.next(`{"tool_name":"b"}`, false)
			c.ingest(e1, c.next(`{"tool_name":"c"}`, false))
		},
		"a report slipped in later, claiming an existing predecessor": func(c *chainHarness) {
			e1 := c.next(`{"tool_name":"a"}`, false)
			e2 := c.next(`{"tool_name":"b"}`, false)
			c.ingest(e1, e2)
			forged := events.Event{EventID: c.exec + "_forged", ExecutionID: c.exec, EventType: "tool_call",
				Sequence: 9, Timestamp: time.Now().UTC(), Payload: json.RawMessage(`{"tool_name":"approve_refund"}`),
				ChainPrev: attest.ReportChainHash(&e1, "")}
			c.ingest(forged)
		},
		"an unchained report in a chained run": func(c *chainHarness) {
			c.ingest(c.next(`{"tool_name":"a"}`, false), events.Event{EventID: c.exec + "_old", ExecutionID: c.exec,
				EventType: "tool_call", Sequence: 5, Timestamp: time.Now().UTC(), Payload: json.RawMessage(`{"tool_name":"x"}`)})
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			c := newChainHarness(t)
			setup(c)
			status, detail, sigs := c.complete()
			if status != store.ReportChainBroken || len(sigs) != 1 || sigs[0] != RecordIntegritySigChainBroken {
				t.Fatalf("status %q (%s), signals %v; want broken", status, detail, sigs)
			}
		})
	}
}

// Reports from SDKs that predate chaining are accepted as before.
func TestUnchainedRunsAreAcceptedAndMarked(t *testing.T) {
	c := newChainHarness(t)
	n := c.ingest(events.Event{EventID: "u1", ExecutionID: c.exec, EventType: "tool_call", Sequence: 1,
		Timestamp: time.Now().UTC(), Payload: json.RawMessage(`{"tool_name":"a"}`)})
	if n != 0 {
		t.Fatalf("an unchained report was refused")
	}
	if status, _, sigs := c.complete(); status != store.ReportChainUnchained || len(sigs) != 0 {
		t.Fatalf("status %q, signals %v", status, sigs)
	}
}

func TestSignedRuns(t *testing.T) {
	t.Run("signed throughout", func(t *testing.T) {
		c := newChainHarness(t)
		c.registerKey()
		if n := c.ingest(c.next(`{"tool_name":"a"}`, true), c.next(`{"tool_name":"b"}`, true)); n != 0 {
			t.Fatalf("rejected %d genuine signed reports", n)
		}
		if status, detail, sigs := c.complete(); status != store.ReportChainSigned || len(sigs) != 0 {
			t.Fatalf("status %q (%s), signals %v", status, detail, sigs)
		}
	})
	t.Run("an unsigned report for a signed run is refused", func(t *testing.T) {
		c := newChainHarness(t)
		c.registerKey()
		c.ingest(c.next(`{"tool_name":"a"}`, true))
		if n := c.ingest(c.next(`{"tool_name":"forged"}`, false)); n != 1 {
			t.Fatalf("rejected %d, want the unsigned report refused", n)
		}
		unchained := events.Event{EventID: c.exec + "_u", ExecutionID: c.exec, EventType: "tool_call",
			Sequence: 7, Timestamp: time.Now().UTC(), Payload: json.RawMessage(`{"tool_name":"forged"}`)}
		if n := c.ingest(unchained); n != 1 {
			t.Fatalf("an unchained report for a signed run was accepted")
		}
	})
	t.Run("bad, unknown and revoked signatures are refused", func(t *testing.T) {
		c := newChainHarness(t)
		c.registerKey()
		bad := c.next(`{"tool_name":"a"}`, true)
		bad.Signature.Sig = base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
		unknown := c.next(`{"tool_name":"b"}`, true)
		unknown.Signature.KeyID = "never-registered"
		if n := c.ingest(bad, unknown); n != 2 {
			t.Fatalf("rejected %d of 2 badly signed reports", n)
		}
		if err := c.st.RevokeReportSigningKey(context.Background(), c.project, "harness-1"); err != nil {
			t.Fatal(err)
		}
		if n := c.ingest(c.next(`{"tool_name":"c"}`, true)); n != 1 {
			t.Fatalf("a report signed with a revoked key was accepted")
		}
	})
	t.Run("an unsigned report that raced ahead is caught at completion", func(t *testing.T) {
		c := newChainHarness(t)
		c.registerKey()
		c.ingest(c.next(`{"tool_name":"a"}`, false))
		c.ingest(c.next(`{"tool_name":"b"}`, true))
		status, _, sigs := c.complete()
		if status != store.ReportChainBroken || len(sigs) != 1 || sigs[0] != RecordIntegritySigSignatureMissing {
			t.Fatalf("status %q, signals %v", status, sigs)
		}
	})
}

// Only a signed-in admin may add or revoke a key: an API key is
// refused whatever its role, because the agent may hold one.
func TestReportSigningKeyEndpoints(t *testing.T) {
	c := newChainHarness(t)
	pub := base64.StdEncoding.EncodeToString(c.priv.Public().(ed25519.PublicKey))
	call := func(method, path, body string, apiKey bool) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		ctx := context.WithValue(req.Context(), ctxKeyProjectID, c.project)
		// An API key carries the user it was minted for (auth.go), so
		// the API-key request below has both, as a real one does.
		ctx = context.WithValue(ctx, ctxKeyUserID, "owner@example.invalid")
		if apiKey {
			ctx = context.WithValue(ctx, ctxKeyAPIKeyID, "key-agent")
		}
		req = req.WithContext(ctx)
		if strings.HasPrefix(path, "/me/report-signing-keys/") {
			req.SetPathValue("key_id", strings.TrimPrefix(path, "/me/report-signing-keys/"))
		}
		rec := httptest.NewRecorder()
		switch method {
		case "POST":
			c.h.HandleCreateReportSigningKey(rec, req)
		case "DELETE":
			c.h.HandleRevokeReportSigningKey(rec, req)
		default:
			c.h.HandleListReportSigningKeys(rec, req)
		}
		return rec.Code
	}
	body := `{"key_id":"harness-1","public_key":"` + pub + `"}`
	if code := call("POST", "/me/report-signing-keys", body, true); code != 403 {
		t.Errorf("an API key registered a signing key (%d)", code)
	}
	if code := call("POST", "/me/report-signing-keys", body, false); code != 201 {
		t.Fatalf("signed-in admin could not register a key (%d)", code)
	}
	if code := call("POST", "/me/report-signing-keys", body, false); code != 200 {
		t.Errorf("a retry of the same key was not idempotent (%d)", code)
	}
	_, other, _ := ed25519.GenerateKey(nil)
	otherBody := `{"key_id":"harness-1","public_key":"` + base64.StdEncoding.EncodeToString(other.Public().(ed25519.PublicKey)) + `"}`
	if code := call("POST", "/me/report-signing-keys", otherBody, false); code != 409 {
		t.Errorf("a different key was registered under a used key_id (%d)", code)
	}
	if code := call("POST", "/me/report-signing-keys", `{"key_id":"k2","public_key":"bm90IGEga2V5"}`, false); code != 400 {
		t.Errorf("a malformed public key was accepted (%d)", code)
	}
	if code := call("GET", "/me/report-signing-keys", "", true); code != 200 {
		t.Errorf("listing keys failed (%d)", code)
	}
	if code := call("DELETE", "/me/report-signing-keys/harness-1", "", true); code != 403 {
		t.Errorf("an API key revoked a signing key (%d)", code)
	}
	if code := call("DELETE", "/me/report-signing-keys/harness-1", "", false); code != 204 {
		t.Errorf("signed-in admin could not revoke (%d)", code)
	}
	if code := call("DELETE", "/me/report-signing-keys/harness-1", "", false); code != 404 {
		t.Errorf("a revoked key was revoked again (%d)", code)
	}
}

// The single-run file, end to end: a signed run with a secret in it goes
// through real ingest and completion, comes out of the export endpoint,
// and the evidence module's checker finds it intact and signed against
// the reader's own copy of the key.
func TestRunExportOfASignedRunVerifies(t *testing.T) {
	c := newChainHarness(t)
	c.registerKey()
	c.ingest(c.next(`{"tool_name":"a","arguments":{"token":"CHAINSECRET_ABCD1234"}}`, true),
		c.next(`{"tool_name":"b","note":"<b>&</b>"}`, true))
	if status, detail, _ := c.complete(); status != store.ReportChainSigned {
		t.Fatalf("status %q (%s)", status, detail)
	}
	covered, err := c.st.ListDigestEventsForExecution(context.Background(), c.exec)
	if err != nil {
		t.Fatal(err)
	}
	d, err := attest.Compute(c.exec, covered)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/executions/"+c.exec+"/export", nil)
	req.SetPathValue("id", c.exec)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, c.project))
	rec := httptest.NewRecorder()
	c.h.HandleRunExport(rec, req)
	if rec.Code != 200 {
		t.Fatalf("export returned %d: %s", rec.Code, rec.Body.String())
	}
	var run attest.RunExport
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	v := attest.VerifyRunExport(run, d.Root, attest.RunKeys{
		Keys: map[string]ed25519.PublicKey{"harness-1": c.priv.Public().(ed25519.PublicKey)}, FromReader: true})
	if len(v.Failures)+len(v.Gaps) != 0 || v.ChainStatus != "signed" {
		t.Fatalf("status %s, failures %v, gaps %v", v.ChainStatus, v.Failures, v.Gaps)
	}
	redactionNoted := false
	for _, u := range v.Unverified {
		redactionNoted = redactionNoted || strings.Contains(u, "to remove a secret")
	}
	if !redactionNoted {
		t.Errorf("the redacted report is not reported: %v", v.Unverified)
	}

	// Another project cannot read it.
	req = httptest.NewRequest("GET", "/executions/"+c.exec+"/export", nil)
	req.SetPathValue("id", c.exec)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, "proj_other"))
	rec = httptest.NewRecorder()
	c.h.HandleRunExport(rec, req)
	if rec.Code != 404 {
		t.Errorf("another project got %d", rec.Code)
	}
}

// The ending (owner decision, 17 November 2026): the run's last report
// going missing is caught when the SDK declared the end, and only then.
func TestAMissingLastReportIsCaught(t *testing.T) {
	c := newChainHarness(t)
	e1, e2 := c.next(`{"tool_name":"a"}`, false), c.next(`{"tool_name":"approve_refund"}`, false)
	_ = e2 // sent by the SDK, never stored
	c.ingest(e1)
	if status, detail, sigs := c.complete(); status != store.ReportChainBroken || len(sigs) != 1 ||
		!strings.Contains(detail, "last report is missing") {
		t.Fatalf("status %q (%s), signals %v", status, detail, sigs)
	}

	older := newChainHarness(t)
	older.noEnd = true
	f1 := older.next(`{"tool_name":"a"}`, false)
	_ = older.next(`{"tool_name":"approve_refund"}`, false)
	older.ingest(f1)
	if status, _, sigs := older.complete(); status != store.ReportChainChained || len(sigs) != 0 {
		t.Fatalf("an SDK that declares no end cannot be judged on its ending: %q %v", status, sigs)
	}
	// The run's page says the end was not confirmed, and confirms it
	// for a run that declared one.
	endConfirmed := func(h *chainHarness) any {
		req := httptest.NewRequest("GET", "/executions/"+h.exec, nil)
		req.SetPathValue("id", h.exec)
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, h.project))
		rec := httptest.NewRecorder()
		h.h.HandleGetExecution(rec, req)
		var body struct {
			ReportChain map[string]any `json:"report_chain"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body.ReportChain["end_confirmed"]
	}
	if got := endConfirmed(older); got != false {
		t.Errorf("a run with no declared end shows end_confirmed %v", got)
	}
	if got := endConfirmed(c); got != true {
		t.Errorf("a run with a declared end shows end_confirmed %v", got)
	}
}

func TestASignedRunNeedsASignedEnd(t *testing.T) {
	c := newChainHarness(t)
	c.registerKey()
	c.ingest(c.next(`{"tool_name":"a"}`, true))
	c.signed = false // the end goes out unsigned
	status, detail, sigs := c.complete()
	if status != store.ReportChainBroken || len(sigs) != 1 || sigs[0] != RecordIntegritySigSignatureMissing ||
		!strings.Contains(detail, "declared end is unsigned") {
		t.Fatalf("status %q (%s), signals %v", status, detail, sigs)
	}
}
