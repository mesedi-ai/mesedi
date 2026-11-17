package attest

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

// runFixture builds a run of n reports chained (and optionally signed)
// the way the SDKs and ingest do, and returns the file, the digest the
// hour anchored, and the signing key.
func runFixture(t *testing.T, n int, sign bool) (RunExport, string, ed25519.PrivateKey) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(nil)
	exec := "exec-run-1"
	r := RunExport{Format: RunExportFormatV1, ProjectID: "p", ExecutionID: exec, GeneratedAt: time.Now().UTC(),
		ReportChainStatus: "chained"}
	if sign {
		r.ReportChainStatus = "signed"
		r.SigningKeys = []RunExportKey{{KeyID: "k1", PublicKey: base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))}}
	}
	prev := ReportChainGenesis(exec)
	var anchored []*events.Event
	base := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= n; i++ {
		payload := []byte(fmt.Sprintf(`{"tool_name":"t%d","note":"<b>&</b>"}`, i))
		e := &events.Event{EventID: fmt.Sprintf("e%d", i), ExecutionID: exec, EventType: "tool_call",
			Sequence: i, Timestamp: base.Add(time.Duration(i) * time.Second), Payload: payload, ChainPrev: prev}
		h := ReportChainHash(e, "")
		sum := sha256.Sum256(payload)
		link := RunExportLink{EventID: e.EventID, Prev: prev, Hash: h, PayloadSHA256: hex.EncodeToString(sum[:])}
		if sign {
			msg, _ := hex.DecodeString(h)
			link.KeyID, link.Signature = "k1", base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))
		}
		r.ReportChain = append(r.ReportChain, link)
		r.Events = append(r.Events, RunExportEvent{EventID: e.EventID, EventType: "tool_call", Sequence: i,
			Timestamp: e.Timestamp, PayloadB64: base64.StdEncoding.EncodeToString(payload)})
		anchored = append(anchored, e)
		prev = h
	}
	// The end the SDK declares when the run finishes, signed for a
	// signed run over its own message.
	r.ReportChainEnd = &RunExportEnd{Hash: prev}
	if sign {
		r.ReportChainEnd.KeyID = "k1"
		r.ReportChainEnd.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, ReportChainEndMessage(exec, prev)))
	}
	d, err := Compute(exec, anchored)
	if err != nil {
		t.Fatal(err)
	}
	return r, d.Root, priv
}

func readerKeys(priv ed25519.PrivateKey) RunKeys {
	return RunKeys{Keys: map[string]ed25519.PublicKey{"k1": priv.Public().(ed25519.PublicKey)}, FromReader: true}
}

func has(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func TestAnIntactSignedRunVerifies(t *testing.T) {
	r, root, priv := runFixture(t, 3, true)
	v := VerifyRunExport(r, root, readerKeys(priv))
	if len(v.Failures)+len(v.Gaps) != 0 || v.ChainStatus != "signed" {
		t.Fatalf("status %s, failures %v, gaps %v", v.ChainStatus, v.Failures, v.Gaps)
	}
	if has(v.Unverified, "keys in this file") {
		t.Error("reader keys were used, but the report says the file's keys were")
	}
	fileKeys, err := KeysFromRunExport(r)
	if err != nil {
		t.Fatal(err)
	}
	v = VerifyRunExport(r, root, fileKeys)
	if len(v.Failures) != 0 || !has(v.Unverified, "keys in this file") {
		t.Errorf("file keys: failures %v, unverified %v", v.Failures, v.Unverified)
	}
}

func TestRunChecksCatchEachChange(t *testing.T) {
	cases := []struct {
		name    string
		change  func(r *RunExport, root *string)
		failure string
		gap     string
	}{
		{"a report's content changed", func(r *RunExport, _ *string) {
			r.Events[1].PayloadB64 = base64.StdEncoding.EncodeToString([]byte(`{"tool_name":"approve_refund"}`))
		}, "differs from what was sent", ""},
		{"a report removed", func(r *RunExport, _ *string) {
			r.Events = append(r.Events[:1], r.Events[2:]...)
		}, "do not rebuild the digest", "not in the run file"},
		{"the anchor disagrees", func(_ *RunExport, root *string) { *root = strings.Repeat("0", 64) }, "do not rebuild the digest", ""},
		{"the run is not in the hourly file", func(_ *RunExport, root *string) { *root = "" }, "", "not in the hourly file"},
		{"a bad signature", func(r *RunExport, _ *string) {
			r.ReportChain[0].Signature = base64.StdEncoding.EncodeToString(make([]byte, 64))
		}, "does not verify", ""},
		{"an unknown signing key", func(r *RunExport, _ *string) { r.ReportChain[2].KeyID = "k9" }, "", "no public key was available for signing key k9"},
		{"a chain record rewritten", func(r *RunExport, _ *string) { r.ReportChain[1].Prev = strings.Repeat("a", 64) }, "do not match their reports", "chain is broken"},
		{"the last report removed", func(r *RunExport, _ *string) {
			r.Events = r.Events[:2]
			r.ReportChain = r.ReportChain[:2]
		}, "do not rebuild the digest", "last report is missing"},
		{"an end signed with a report's own signature", func(r *RunExport, _ *string) {
			r.ReportChainEnd.Hash = r.ReportChain[1].Hash
			r.ReportChainEnd.Signature = r.ReportChain[1].Signature
		}, "declared end does not verify", "continue past"},
		{"a signed run with no declared end", func(r *RunExport, _ *string) { r.ReportChainEnd = nil }, "Mesedi recorded this run's chain as", "declares no end"},
		{"Mesedi's status contradicted", func(r *RunExport, _ *string) { r.ReportChainStatus = "broken" }, `Mesedi recorded this run's chain as "broken"`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, root, priv := runFixture(t, 3, true)
			c.change(&r, &root)
			v := VerifyRunExport(r, root, readerKeys(priv))
			if c.failure != "" && !has(v.Failures, c.failure) {
				t.Errorf("failures %v lack %q", v.Failures, c.failure)
			}
			if c.gap != "" && !has(v.Gaps, c.gap) {
				t.Errorf("gaps %v lack %q", v.Gaps, c.gap)
			}
			if c.failure == "" && len(v.Failures) != 0 {
				t.Errorf("unexpected failures %v", v.Failures)
			}
		})
	}
}

// Secret redaction on arrival changes the stored payload; the anchored
// digest covers the stored bytes, and the chain covers what was sent, so
// a redacted report is reported, not failed.
func TestARedactedReportIsNotAFailure(t *testing.T) {
	r, _, priv := runFixture(t, 2, true)
	redacted := []byte(`{"tool_name":"t1","token":"[REDACTED:aws_key]"}`)
	r.Events[0].PayloadB64 = base64.StdEncoding.EncodeToString(redacted)
	var anchored []*events.Event
	for _, re := range r.Events {
		raw, _ := base64.StdEncoding.DecodeString(re.PayloadB64)
		anchored = append(anchored, &events.Event{EventID: re.EventID, ExecutionID: r.ExecutionID, EventType: "tool_call",
			Sequence: re.Sequence, Timestamp: re.Timestamp, Payload: raw})
	}
	d, _ := Compute(r.ExecutionID, anchored)
	v := VerifyRunExport(r, d.Root, readerKeys(priv))
	if len(v.Failures) != 0 || !has(v.Unverified, "changed by Mesedi on arrival to remove a secret") {
		t.Fatalf("failures %v, unverified %v", v.Failures, v.Unverified)
	}
}

func TestAfterAnchoringReportsAreChainedButNotAnchored(t *testing.T) {
	r, _, priv := runFixture(t, 3, true)
	r.Events[2].AfterAnchoring = true
	var anchored []*events.Event
	for _, re := range r.Events[:2] {
		raw, _ := base64.StdEncoding.DecodeString(re.PayloadB64)
		anchored = append(anchored, &events.Event{EventID: re.EventID, ExecutionID: r.ExecutionID, EventType: "tool_call",
			Sequence: re.Sequence, Timestamp: re.Timestamp, Payload: raw})
	}
	d, _ := Compute(r.ExecutionID, anchored)
	v := VerifyRunExport(r, d.Root, readerKeys(priv))
	if len(v.Failures)+len(v.Gaps) != 0 || !has(v.Unverified, "1 record arrived after this run was anchored") {
		t.Fatalf("failures %v, gaps %v, unverified %v", v.Failures, v.Gaps, v.Unverified)
	}
}

func TestAnUnchainedRunIsSaidToBeUnchained(t *testing.T) {
	r, root, _ := runFixture(t, 2, false)
	r.ReportChain = nil
	r.ReportChainEnd = nil
	r.ReportChainStatus = "unchained"
	v := VerifyRunExport(r, root, RunKeys{})
	if v.ChainStatus != "unchained" || len(v.Failures)+len(v.Gaps) != 0 || !has(v.Unverified, "predates report chaining") {
		t.Fatalf("%+v", v)
	}
}
