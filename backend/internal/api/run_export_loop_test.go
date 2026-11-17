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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest"
	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/dlp"
	"mesedi/backend/internal/store"
)

// The whole loop for a signed run: reports chained and signed as the
// SDKs send them, ingested (a secret redacted on arrival), the hour
// sealed and anchored, then the hourly file and the single-run file
// produced by their endpoints and the run checked against the anchored
// hour with the reader's own key. Set MESEDI_RUN_LOOP_DIR to keep the
// two files for running the mesedi-verify binary on them.
func TestSignedRunFromIngestToIndependentCheck(t *testing.T) {
	f := newChainFixture(t, false)
	scanner, err := dlp.NewScanner([]dlp.Rule{{ID: "loop_secret", Label: "test", Severity: dlp.SeverityCritical,
		Pattern: `LOOPSECRET_[A-Z0-9]{8}`}})
	if err != nil {
		t.Fatal(err)
	}
	f.h.DLPScanner = scanner
	_, priv, _ := ed25519.GenerateKey(nil)
	pub := base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	if err := f.st.InsertReportSigningKey(f.ctx, "proj", "harness-1", pub, "owner"); err != nil {
		t.Fatal(err)
	}
	const exec = "exec-signed"
	end := f.t0.Add(time.Minute)
	if err := f.st.CreateExecution(f.ctx, &events.Execution{ExecutionID: exec, ProjectID: "proj", Status: "completed",
		StartedAt: f.t0, EndedAt: &end}); err != nil {
		t.Fatal(err)
	}

	prev := attest.ReportChainGenesis(exec)
	var batch []events.Event
	for i, p := range []string{
		`{"tool_name":"lookup","arguments":{"token":"LOOPSECRET_ABCD1234"}}`,
		`{"tool_name":"reply","note":"<ok> & done"}`,
	} {
		e := events.Event{EventID: fmt.Sprintf("%s-e%d", exec, i+1), ExecutionID: exec, EventType: events.EventTypeToolCall,
			Sequence: i + 1, Timestamp: f.t0.Add(time.Duration(i+1) * time.Second), Payload: json.RawMessage(p), ChainPrev: prev}
		h := attest.ReportChainHash(&e, "")
		msg, _ := hex.DecodeString(h)
		e.Signature = &events.ReportSignature{KeyID: "harness-1", Sig: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))}
		batch = append(batch, e)
		prev = h
	}
	// Sent without HTML escaping: Go's default encoder rewrites "<", ">"
	// and "&", so the bytes received would not be the bytes signed and
	// the signature would rightly fail. The SDKs' encoders do not.
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(batch); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/events", &body)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, "proj"))
	rec := httptest.NewRecorder()
	f.h.HandleIngestEvents(rec, req)
	if rec.Code != 200 || bytes.Contains(rec.Body.Bytes(), []byte(`"rejected":1`)) {
		t.Fatalf("ingest %d: %s", rec.Code, rec.Body.String())
	}
	if err := f.st.SetReportChainStatus(f.ctx, store.ReportChainStatusRow{ExecutionID: exec, ProjectID: "proj",
		Status: "signed", EndHash: prev, EndKeyID: "harness-1",
		EndSignature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, attest.ReportChainEndMessage(exec, prev)))}); err != nil {
		t.Fatal(err)
	}

	f.now = f.t0.Add(30 * time.Minute)
	if err := f.sch.tick(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.now = f.t0.Add(2*time.Hour + 5*time.Minute)
	if err := f.sch.tick(f.ctx); err != nil {
		t.Fatal(err)
	}
	hourly, hv := f.export()
	if !hv.OK {
		t.Fatalf("hourly export does not verify: %s", failures(hv))
	}
	root, status := "", ""
	for _, iv := range hourly.Intervals {
		for _, x := range iv.Executions {
			if x.ExecutionID == exec {
				root, status = x.DigestRoot, x.ReportChain
			}
		}
	}
	if root == "" || status != "signed" {
		t.Fatalf("the hourly file lists the run with root %q and chain %q", root, status)
	}

	req = httptest.NewRequest("GET", "/executions/"+exec+"/export", nil)
	req.SetPathValue("id", exec)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, "proj"))
	rec = httptest.NewRecorder()
	f.h.HandleRunExport(rec, req)
	if rec.Code != 200 {
		t.Fatalf("run export %d: %s", rec.Code, rec.Body.String())
	}
	var run attest.RunExport
	if err := json.Unmarshal(rec.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	v := attest.VerifyRunExport(run, root, attest.RunKeys{
		Keys: map[string]ed25519.PublicKey{"harness-1": priv.Public().(ed25519.PublicKey)}, FromReader: true})
	if len(v.Failures)+len(v.Gaps) != 0 || v.ChainStatus != "signed" {
		t.Fatalf("run check: status %s, failures %v, gaps %v", v.ChainStatus, v.Failures, v.Gaps)
	}

	if dir := os.Getenv("MESEDI_RUN_LOOP_DIR"); dir != "" {
		hb, _ := json.MarshalIndent(hourly, "", "  ")
		_ = os.WriteFile(filepath.Join(dir, "hourly.json"), hb, 0o644)
		_ = os.WriteFile(filepath.Join(dir, "run.json"), rec.Body.Bytes(), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "report-key.pub"), []byte(pub), 0o644)
	}
}
