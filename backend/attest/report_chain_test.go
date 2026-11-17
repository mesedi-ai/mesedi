package attest

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

// Test vectors for mesedi-report-chain-v1. The same three values are
// asserted by the Python and TypeScript SDK tests; they were computed
// from the written field list by an independent implementation, not by
// this code. If one implementation changes, its test fails here or there.
const (
	vectorGenesis = "aa5e34801b6470b5c0c1b2dd18940faa69c6a78d3cc0b8e8b22147b67ac8ca38"
	vectorHash1   = "ab93fdf9da9815714c444157c23face182b1374657894ce6495e8ce478f4de80"
	vectorHash2   = "794d6cd3b0003ee7b0db118645c7e77c28bd7687b89eb1e908584462a1735a6c"
)

// The first payload holds the six ASCII characters of the JSON escape
// for e-acute, as Python's encoder sends them, not the character: the
// fingerprint is over bytes as sent, never over a decoded value.
func vectorEvents(t *testing.T) (*events.Event, *events.Event) {
	t.Helper()
	ts1, _ := time.Parse(time.RFC3339Nano, "2026-11-17T12:00:00.123456Z")
	// Sent with a +02:00 offset: the fingerprint uses the UTC instant.
	ts2, _ := time.Parse(time.RFC3339Nano, "2026-11-17T14:00:01.5+02:00")
	e1 := &events.Event{EventID: "evt-1", ExecutionID: "exec-vector-1", EventType: "tool_call",
		Sequence: 1, Timestamp: ts1,
		Payload: json.RawMessage("{\"tool_name\":\"search\",\"return_value\":\"caf\\u00e9\"}")}
	e2 := &events.Event{EventID: "evt-2", ExecutionID: "exec-vector-1", EventType: "llm_call",
		Sequence: 2, Timestamp: ts2, DurationMs: 42,
		Payload: json.RawMessage(`{"model":"m","user_message":"hi"}`)}
	return e1, e2
}

func TestReportChainVectors(t *testing.T) {
	if got := ReportChainGenesis("exec-vector-1"); got != vectorGenesis {
		t.Fatalf("genesis = %s", got)
	}
	e1, e2 := vectorEvents(t)
	e1.ChainPrev = vectorGenesis
	if got := ReportChainHash(e1, ""); got != vectorHash1 {
		t.Fatalf("hash 1 = %s", got)
	}
	e2.ChainPrev = vectorHash1
	if got := ReportChainHash(e2, ""); got != vectorHash2 {
		t.Fatalf("hash 2 = %s", got)
	}
}

// Every field moves the fingerprint; a signature does not (it signs it).
func TestReportChainHashCoversEveryField(t *testing.T) {
	base, _ := vectorEvents(t)
	base.ChainPrev = vectorGenesis
	want := ReportChainHash(base, "")
	mutate := map[string]func(e *events.Event){
		"event_id":     func(e *events.Event) { e.EventID = "evt-x" },
		"execution_id": func(e *events.Event) { e.ExecutionID = "exec-x" },
		"event_type":   func(e *events.Event) { e.EventType = "llm_call" },
		"sequence":     func(e *events.Event) { e.Sequence = 2 },
		"timestamp":    func(e *events.Event) { e.Timestamp = e.Timestamp.Add(time.Microsecond) },
		"duration_ms":  func(e *events.Event) { e.DurationMs = 1 },
		"payload":      func(e *events.Event) { e.Payload = json.RawMessage(`{"tool_name":"search"}`) },
		"chain_prev":   func(e *events.Event) { e.ChainPrev = vectorHash1 },
	}
	for name, m := range mutate {
		e := *base
		m(&e)
		if ReportChainHash(&e, "") == want {
			t.Errorf("changing %s did not change the fingerprint", name)
		}
	}
	e := *base
	e.Signature = &events.ReportSignature{KeyID: "k", Sig: "x"}
	if ReportChainHash(&e, "") != want {
		t.Error("the signature changed the fingerprint it signs")
	}
}

func link(prev, hash string) ReportChainLink { return ReportChainLink{Prev: prev, Hash: hash} }

func TestVerifyReportChain(t *testing.T) {
	g := ReportChainGenesis("x")
	intact := []ReportChainLink{link(g, "a"), link("a", "b"), link("b", "c")}
	cases := []struct {
		name      string
		links     []ReportChainLink
		unchained int
		broken    string
	}{
		{"intact, in any order", []ReportChainLink{intact[2], intact[0], intact[1]}, 0, ""},
		{"a report removed from the middle", []ReportChainLink{intact[0], intact[2]}, 0, "does not hold"},
		{"the first report removed", intact[1:], 0, "first report is missing"},
		{"a report slipped in beside another", append(intact[:3:3], link("a", "z")), 0, "already claims"},
		{"a second start", append(intact[:3:3], link(g, "y")), 0, "claim to start"},
		{"an unchained report in a chained run", intact, 1, "carry no chain"},
	}
	for _, c := range cases {
		v := VerifyReportChain("x", c.links, c.unchained, "")
		got := strings.Join(v.Broken, "; ")
		if (c.broken == "") != (got == "") || !strings.Contains(got, c.broken) {
			t.Errorf("%s: broken = %q, want mention of %q", c.name, got, c.broken)
		}
		if !v.Chained {
			t.Errorf("%s: not marked chained", c.name)
		}
	}
	if v := VerifyReportChain("x", nil, 5, ""); v.Chained || len(v.Broken) != 0 {
		t.Errorf("a run with no chained reports is unchained, not broken: %+v", v)
	}
	signed := []ReportChainLink{{Prev: g, Hash: "a", Signed: true}, {Prev: "a", Hash: "b"}}
	if v := VerifyReportChain("x", signed, 0, ""); !v.Signed || v.Unsigned != 1 {
		t.Errorf("signed run with one unsigned report: %+v", v)
	}
}

// The shared vector file the SDKs read must agree with the constants
// above, so a change to one side cannot pass unnoticed on the other.
func TestReportChainSharedVectorFile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "spec", "report_chain_vectors.json"))
	if err != nil {
		t.Fatalf("read shared vectors: %v", err)
	}
	var v struct {
		ExecutionID string `json:"execution_id"`
		Genesis     string `json:"genesis"`
		Events      []struct {
			EventID     string `json:"event_id"`
			EventType   string `json:"event_type"`
			Sequence    int    `json:"sequence"`
			Timestamp   string `json:"timestamp"`
			DurationMs  *int64 `json:"duration_ms"`
			Payload     string `json:"payload"`
			Fingerprint string `json:"fingerprint"`
		} `json:"events"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v.Genesis != vectorGenesis || ReportChainGenesis(v.ExecutionID) != v.Genesis {
		t.Fatalf("genesis in the shared file disagrees")
	}
	if len(v.Events) != 2 {
		t.Fatalf("want 2 shared events, got %d", len(v.Events))
	}
	prev := v.Genesis
	for _, ve := range v.Events {
		ts, err := time.Parse(time.RFC3339Nano, ve.Timestamp)
		if err != nil {
			t.Fatal(err)
		}
		e := &events.Event{EventID: ve.EventID, ExecutionID: v.ExecutionID, EventType: events.EventType(ve.EventType),
			Sequence: ve.Sequence, Timestamp: ts, Payload: json.RawMessage(ve.Payload), ChainPrev: prev}
		if ve.DurationMs != nil {
			e.DurationMs = *ve.DurationMs
		}
		if got := ReportChainHash(e, ""); got != ve.Fingerprint {
			t.Fatalf("%s: fingerprint %s, shared file says %s", ve.EventID, got, ve.Fingerprint)
		}
		prev = ve.Fingerprint
	}
	if v.Events[0].Fingerprint != vectorHash1 || v.Events[1].Fingerprint != vectorHash2 {
		t.Fatal("shared file and constants disagree")
	}
}

// The shared signature vector: the backend verifies exactly what the
// SDKs produce from the published test key.
func TestReportChainSharedSignatureVector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "spec", "report_chain_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		Signature struct {
			Seed   string `json:"private_key_seed_hex"`
			Public string `json:"public_key_b64"`
			Sig    string `json:"signature_b64"`
		} `json:"signature"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	seed, _ := hex.DecodeString(v.Signature.Seed)
	priv := ed25519.NewKeyFromSeed(seed)
	if got := base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)); got != v.Signature.Public {
		t.Fatalf("public key %s", got)
	}
	msg, _ := hex.DecodeString(vectorHash1)
	if got := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg)); got != v.Signature.Sig {
		t.Fatalf("signature %s", got)
	}
}

// The ending: without a declared end, a run whose last reports went
// missing still forms a clean path; with one, it is broken.
func TestVerifyReportChainEnd(t *testing.T) {
	g := ReportChainGenesis("x")
	full := []ReportChainLink{link(g, "a"), link("a", "b"), link("b", "c")}
	cut := full[:2] // the last report, "c", is gone
	if v := VerifyReportChain("x", cut, 0, ""); len(v.Broken) != 0 || !v.EndUnconfirmed {
		t.Fatalf("no end declared: %+v", v)
	}
	if v := VerifyReportChain("x", cut, 0, "c"); len(v.Broken) != 1 || !strings.Contains(v.Broken[0], "last report is missing") {
		t.Fatalf("missing tail: %+v", v)
	}
	if v := VerifyReportChain("x", full, 0, "c"); len(v.Broken) != 0 || v.EndUnconfirmed {
		t.Fatalf("intact with end: %+v", v)
	}
	if v := VerifyReportChain("x", full, 0, "b"); len(v.Broken) != 1 || !strings.Contains(v.Broken[0], "continue past") {
		t.Fatalf("end declared early: %+v", v)
	}
	if v := VerifyReportChain("x", nil, 0, "c"); !v.Chained || len(v.Broken) != 1 {
		t.Fatalf("every chained report lost: %+v", v)
	}
	if a, b := hex.EncodeToString(ReportChainEndMessage("x", "c")), "c"; a == b || len(a) != 64 {
		t.Fatal("the end message must be its own 32-byte value, never the fingerprint")
	}
}

// The shared end vector: the message and its signature from the test key.
func TestReportChainSharedEndVector(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "spec", "report_chain_vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		ExecutionID string `json:"execution_id"`
		Signature   struct {
			Seed string `json:"private_key_seed_hex"`
		} `json:"signature"`
		End struct {
			Last string `json:"last_fingerprint"`
			Msg  string `json:"end_message_hex"`
			Sig  string `json:"signature_b64"`
		} `json:"end"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v.End.Last != vectorHash2 {
		t.Fatalf("the end vector names %s, not the second event", v.End.Last)
	}
	msg := ReportChainEndMessage(v.ExecutionID, v.End.Last)
	if hex.EncodeToString(msg) != v.End.Msg {
		t.Fatalf("end message %x", msg)
	}
	seed, _ := hex.DecodeString(v.Signature.Seed)
	if got := base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), msg)); got != v.End.Sig {
		t.Fatalf("end signature %s", got)
	}
}
