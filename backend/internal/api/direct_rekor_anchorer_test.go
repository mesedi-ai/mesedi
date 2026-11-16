package api

// DirectRekorAnchorer against a fake Rekor v1 log. Nothing here ever
// writes to the real public log.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest"
	"mesedi/backend/internal/store"
)

// ── RFC 6962 for the fake log ────────────────────────────────────────

func fakeLeaf(body []byte) []byte {
	h := sha256.Sum256(append([]byte{0x00}, body...))
	return h[:]
}

func fakeNode(l, r []byte) []byte {
	h := sha256.Sum256(append(append([]byte{0x01}, l...), r...))
	return h[:]
}

func fakeSplit(n int) int {
	k := 1
	for k*2 < n {
		k *= 2
	}
	return k
}

func fakeRoot(leaves [][]byte) []byte {
	if len(leaves) == 1 {
		return leaves[0]
	}
	k := fakeSplit(len(leaves))
	return fakeNode(fakeRoot(leaves[:k]), fakeRoot(leaves[k:]))
}

func fakePath(m int, leaves [][]byte) [][]byte {
	if len(leaves) <= 1 {
		return nil
	}
	k := fakeSplit(len(leaves))
	if m < k {
		return append(fakePath(m, leaves[:k]), fakeRoot(leaves[k:]))
	}
	return append(fakePath(m-k, leaves[k:]), fakeRoot(leaves[:k]))
}

// fakeSubproof is RFC 6962 section 2.1.2's SUBPROOF(m, D[n], b).
func fakeSubproof(m int, leaves [][]byte, b bool) [][]byte {
	n := len(leaves)
	if m == n {
		if b {
			return nil
		}
		return [][]byte{fakeRoot(leaves)}
	}
	k := fakeSplit(n)
	if m <= k {
		return append(fakeSubproof(m, leaves[:k], b), fakeRoot(leaves[k:]))
	}
	return append(fakeSubproof(m-k, leaves[k:], false), fakeRoot(leaves[:k]))
}

// ── the fake log ─────────────────────────────────────────────────────

type fakeRekor struct {
	t      *testing.T
	mu     sync.Mutex
	bodies [][]byte
	byUUID map[string]int
	posts  int
	logID  string
	failN  int // answer the next failN submissions with HTTP 503 after recording them
	tamper func(entry map[string]any)
	// replace swaps the submitted body for another before the log
	// records it, so the inclusion proof is genuine for what is returned.
	replace func(body []byte) []byte
	proofOK bool
	// badGrowth answers consistency requests with a corrupted proof.
	badGrowth bool
	srv       *httptest.Server
}

func newFakeRekor(t *testing.T) *fakeRekor {
	f := &fakeRekor{t: t, byUUID: map[string]int{}, logID: strings.Repeat("ab", 32), proofOK: true}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRekor) entryJSON(i int) []byte {
	leaves := make([][]byte, len(f.bodies))
	for j, b := range f.bodies {
		leaves[j] = fakeLeaf(b)
	}
	path := fakePath(i, leaves)
	hashes := make([]string, len(path))
	for j, p := range path {
		hashes[j] = hex.EncodeToString(p)
	}
	entry := map[string]any{
		"logID":          f.logID,
		"logIndex":       i,
		"body":           base64.StdEncoding.EncodeToString(f.bodies[i]),
		"integratedTime": time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC).Unix(),
		"verification": map[string]any{"inclusionProof": map[string]any{
			"logIndex": i, "treeSize": len(leaves), "rootHash": hex.EncodeToString(fakeRoot(leaves)),
			"hashes": hashes, "checkpoint": "rekor.sigstore.dev - 1\n" + strconv.Itoa(len(leaves)) + "\nroot\n\n— fake\n",
		}},
	}
	if f.tamper != nil {
		f.tamper(entry)
	}
	uuid := hex.EncodeToString(fakeLeaf(f.bodies[i]))
	out, _ := json.Marshal(map[string]any{uuid: entry})
	return out
}

func (f *fakeRekor) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/log/entries":
		f.posts++
		raw, _ := io.ReadAll(r.Body)
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			http.Error(w, `{"message":"not json"}`, http.StatusBadRequest)
			return
		}
		body := compact.Bytes()
		if !fakeSignatureVerifies(body) {
			http.Error(w, `{"message":"signature does not verify"}`, http.StatusBadRequest)
			return
		}
		uuid := hex.EncodeToString(fakeLeaf(body))
		if i, ok := f.byUUID[uuid]; ok {
			_ = i
			w.Header().Set("Location", "/api/v1/log/entries/"+uuid)
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":409,"message":"an equivalent entry already exists in the transparency log"}`))
			return
		}
		if f.replace != nil {
			body = f.replace(body)
		}
		f.bodies = append(f.bodies, body)
		f.byUUID[uuid] = len(f.bodies) - 1
		if f.failN > 0 {
			f.failN--
			http.Error(w, `{"message":"timeout"}`, http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(f.entryJSON(len(f.bodies) - 1))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/log/entries/"):
		i, ok := f.byUUID[strings.TrimPrefix(r.URL.Path, "/api/v1/log/entries/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(f.entryJSON(i))
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/log/proof":
		last, _ := strconv.Atoi(r.URL.Query().Get("lastSize"))
		if last < 1 || last > len(f.bodies) || !f.proofOK {
			http.Error(w, `{"message":"bad size"}`, http.StatusBadRequest)
			return
		}
		first, _ := strconv.Atoi(r.URL.Query().Get("firstSize"))
		all := make([][]byte, len(f.bodies))
		for j := range f.bodies {
			all[j] = fakeLeaf(f.bodies[j])
		}
		proof := fakeSubproof(first, all[:last], true)
		hashes := make([]string, len(proof))
		for j, p := range proof {
			hashes[j] = hex.EncodeToString(p)
		}
		if f.badGrowth {
			hashes[0] = strings.Repeat("cd", 32)
		}
		// Like the real log: the root of the tree as it is NOW, not at
		// lastSize (real-log run, 2026-11-16).
		out, _ := json.Marshal(map[string]any{"rootHash": hex.EncodeToString(fakeRoot(all)), "hashes": hashes})
		_, _ = w.Write(out)
	default:
		http.NotFound(w, r)
	}
}

// fakeSignatureVerifies checks a hashedrekord the way Rekor does: the
// signature over the recorded hash must verify with the carried key.
func fakeSignatureVerifies(body []byte) bool {
	var e struct {
		Kind string
		Spec struct {
			Signature struct {
				Content   string
				PublicKey struct{ Content string }
			}
			Data struct {
				Hash struct{ Algorithm, Value string }
			}
		}
	}
	if json.Unmarshal(body, &e) != nil || e.Kind != "hashedrekord" {
		return false
	}
	pemBytes, err := base64.StdEncoding.DecodeString(e.Spec.Signature.PublicKey.Content)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return false
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return false
	}
	sig, _ := base64.StdEncoding.DecodeString(e.Spec.Signature.Content)
	digest, _ := hex.DecodeString(e.Spec.Data.Hash.Value)
	k, ok := pub.(*ecdsa.PublicKey)
	return ok && ecdsa.VerifyASN1(k, digest, sig)
}

// ── helpers ──────────────────────────────────────────────────────────

func testAnchorKeyPEM(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

func testDirectAnchorer(t *testing.T, f *fakeRekor) *DirectRekorAnchorer {
	t.Helper()
	a, err := NewDirectRekorAnchorer(f.srv.URL, testAnchorKeyPEM(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func testCheckpoint(seq uint64) attest.Checkpoint {
	return attest.Checkpoint{Seq: seq, Hash: fmt.Sprintf("%064x", seq*7919)}
}

// ── tests ────────────────────────────────────────────────────────────

func TestDirectAnchorStoresAVerifiableSignedEntry(t *testing.T) {
	f := newFakeRekor(t)
	a := testDirectAnchorer(t, f)
	cp := testCheckpoint(42)
	got, err := a.AnchorCheckpoint(context.Background(), cp, TreeHead{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Anchored || got.LogEntryID != "0" || got.LedgerBackend != "rekor" || got.AnchorKind != store.AnchorKindMesediDirect {
		t.Fatalf("anchor = %+v", got)
	}
	if got.LeafPreimage != "mesedi.anchor.v1.42."+cp.Hash {
		t.Fatalf("preimage %q", got.LeafPreimage)
	}
	var env struct {
		LogID          string          `json:"log_id"`
		EntryBody      string          `json:"entry_body"`
		InclusionProof json.RawMessage `json:"inclusion_proof"`
	}
	if err := json.Unmarshal([]byte(got.AnchorProofJSON), &env); err != nil || env.LogID != f.logID || env.EntryBody == "" || len(env.InclusionProof) == 0 {
		t.Fatalf("stored proof %s (err %v)", got.AnchorProofJSON, err)
	}
	// The stored body records sha256 of the preimage, signed by this key.
	body, _ := base64.StdEncoding.DecodeString(env.EntryBody)
	sum := sha256.Sum256([]byte(got.LeafPreimage))
	if !bytes.Contains(body, []byte(hex.EncodeToString(sum[:]))) || !fakeSignatureVerifies(body) {
		t.Fatalf("stored entry body does not record a signed hash of the preimage: %s", body)
	}
}

func TestOneCheckpointAlwaysProducesTheSameEntry(t *testing.T) {
	f := newFakeRekor(t)
	a := testDirectAnchorer(t, f)
	digest := sha256.Sum256([]byte(AnchorPreimage(testCheckpoint(5))))
	s1, _ := a.Key.Sign(nil, digest[:], cryptoSHA256())
	s2, _ := a.Key.Sign(nil, digest[:], cryptoSHA256())
	if !bytes.Equal(hashedRekordJSON(digest[:], s1, a.publicKeyPEM), hashedRekordJSON(digest[:], s2, a.publicKeyPEM)) {
		t.Fatal("two signatures of one checkpoint differ; retries would create duplicate log entries")
	}
}

func TestALostAnswerIsRecoveredWithoutADuplicateEntry(t *testing.T) {
	f := newFakeRekor(t)
	a := testDirectAnchorer(t, f)
	cp := testCheckpoint(7)
	f.failN = 1 // the log records the entry, then the answer is lost
	if _, err := a.AnchorCheckpoint(context.Background(), cp, TreeHead{}); err == nil {
		t.Fatal("a lost answer was reported as anchored")
	}
	got, err := a.AnchorCheckpoint(context.Background(), cp, TreeHead{})
	if err != nil {
		t.Fatalf("the retry did not adopt the existing entry: %v", err)
	}
	if len(f.bodies) != 1 || f.posts != 2 || got.LogEntryID != "0" {
		t.Fatalf("log holds %d entries after %d submissions (anchor %s); want one entry", len(f.bodies), f.posts, got.LogEntryID)
	}
}

func TestEntriesNotSignedByThisKeyOrForAnotherCheckpointAreRefused(t *testing.T) {
	signedBody := func(key *ecdsa.PrivateKey, digest []byte) []byte {
		sig, err := key.Sign(nil, digest, crypto.SHA256)
		if err != nil {
			t.Fatal(err)
		}
		pub, _ := AnchorPublicKeyPEM(&key.PublicKey)
		var c bytes.Buffer
		_ = json.Compact(&c, hashedRekordJSON(digest, sig, pub))
		return c.Bytes()
	}
	otherKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ourDigest := func(body []byte) []byte {
		var e struct {
			Spec struct {
				Data struct{ Hash struct{ Value string } }
			}
		}
		_ = json.Unmarshal(body, &e)
		d, _ := hex.DecodeString(e.Spec.Data.Hash.Value)
		return d
	}
	cases := map[string]func(f *fakeRekor, a *DirectRekorAnchorer){
		// The log holds a genuine, properly proven entry for this
		// checkpoint's hash, but signed by someone else's key.
		"another key": func(f *fakeRekor, _ *DirectRekorAnchorer) {
			f.replace = func(body []byte) []byte { return signedBody(otherKey, ourDigest(body)) }
		},
		// The log holds an entry signed by our key, for another hash.
		"another hash": func(f *fakeRekor, a *DirectRekorAnchorer) {
			other := sha256.Sum256([]byte("some other checkpoint"))
			f.replace = func([]byte) []byte { return signedBody(a.Key, other[:]) }
		},
		// Our genuine signature, but the entry names another public key.
		// The verifier reads the signer from the entry, so this would be
		// published as someone else's. Only the key comparison catches it.
		"our signature, another named key": func(f *fakeRekor, _ *DirectRekorAnchorer) {
			f.replace = func(body []byte) []byte {
				var e map[string]any
				_ = json.Unmarshal(body, &e)
				pub, _ := AnchorPublicKeyPEM(&otherKey.PublicKey)
				e["spec"].(map[string]any)["signature"].(map[string]any)["publicKey"] = map[string]any{"content": base64.StdEncoding.EncodeToString(pub)}
				out, _ := json.Marshal(e)
				return out
			}
		},
		// A dishonest log returns our genuine signature, but recording
		// another hash. Only the hash comparison catches this one.
		"our signature, another recorded hash": func(f *fakeRekor, _ *DirectRekorAnchorer) {
			f.replace = func(body []byte) []byte {
				var e map[string]any
				_ = json.Unmarshal(body, &e)
				e["spec"].(map[string]any)["data"] = map[string]any{"hash": map[string]any{"algorithm": "sha256", "value": strings.Repeat("11", 32)}}
				out, _ := json.Marshal(e)
				return out
			}
		},
		"a proof to another root": func(f *fakeRekor, _ *DirectRekorAnchorer) {
			f.tamper = func(e map[string]any) {
				e["verification"].(map[string]any)["inclusionProof"].(map[string]any)["rootHash"] = strings.Repeat("ee", 32)
			}
		},
		"no signed checkpoint": func(f *fakeRekor, _ *DirectRekorAnchorer) {
			f.tamper = func(e map[string]any) {
				e["verification"].(map[string]any)["inclusionProof"].(map[string]any)["checkpoint"] = ""
			}
		},
		"no log id": func(f *fakeRekor, _ *DirectRekorAnchorer) {
			f.tamper = func(e map[string]any) { e["logID"] = "" }
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFakeRekor(t)
			a := testDirectAnchorer(t, f)
			setup(f, a)
			if got, err := a.AnchorCheckpoint(context.Background(), testCheckpoint(3), TreeHead{}); err == nil {
				t.Fatalf("a wrong answer was recorded as an anchor: %+v", got)
			}
		})
	}
}

func TestConsistencyProofIsStoredWithBothTreeSizes(t *testing.T) {
	f := newFakeRekor(t)
	a := testDirectAnchorer(t, f)
	var heads []TreeHead
	for seq := uint64(1); seq <= 3; seq++ {
		got, err := a.AnchorCheckpoint(context.Background(), testCheckpoint(seq), TreeHead{})
		if err != nil {
			t.Fatal(err)
		}
		heads = append(heads, storedHead(t, got))
	}
	got, err := a.AnchorCheckpoint(context.Background(), testCheckpoint(4), heads[1])
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		ConsistencyProof consistencyProofStored `json:"consistency_proof"`
	}
	if err := json.Unmarshal([]byte(got.AnchorProofJSON), &env); err != nil {
		t.Fatal(err)
	}
	if env.ConsistencyProof.FirstSize != 2 || env.ConsistencyProof.LastSize != 4 || len(env.ConsistencyProof.Hashes) == 0 {
		t.Fatalf("consistency proof %+v", env.ConsistencyProof)
	}
	// A proof that does not hold, or a log that will not prove growth,
	// costs the proof, never the anchor.
	f.badGrowth = true
	got, err = a.AnchorCheckpoint(context.Background(), testCheckpoint(5), storedHead(t, got))
	if err != nil || strings.Contains(got.AnchorProofJSON, "consistency_proof") {
		t.Fatalf("a corrupted growth proof: anchor %+v, err %v", got, err)
	}
	f.badGrowth, f.proofOK = false, false
	got, err = a.AnchorCheckpoint(context.Background(), testCheckpoint(6), storedHead(t, got))
	if err != nil || strings.Contains(got.AnchorProofJSON, "consistency_proof") {
		t.Fatalf("missing consistency proof: anchor %+v, err %v", got, err)
	}
}

func storedHead(t *testing.T, a store.CheckpointAnchor) TreeHead {
	t.Helper()
	var env struct {
		InclusionProof rekorInclusionProof `json:"inclusion_proof"`
	}
	if err := json.Unmarshal([]byte(a.AnchorProofJSON), &env); err != nil {
		t.Fatal(err)
	}
	return TreeHead{Size: env.InclusionProof.TreeSize, Root: env.InclusionProof.RootHash}
}

// The three growth proofs the real log gave between the hours of the
// real-log run, 16 November 2026 (testdata/rekor-consistency-2026-11-16.json),
// each with the two roots Sigstore signed for those sizes.
func TestTheRealLogsGrowthProofsVerify(t *testing.T) {
	raw, err := os.ReadFile("testdata/rekor-consistency-2026-11-16.json")
	if err != nil {
		t.Fatal(err)
	}
	var proofs []struct {
		FirstSize       int64    `json:"first_size"`
		FirstRoot       string   `json:"first_root"`
		SecondSize      int64    `json:"second_size"`
		SecondRoot      string   `json:"second_root"`
		Hashes          []string `json:"hashes"`
		LogReportedRoot string   `json:"log_reported_root"`
	}
	if err := json.Unmarshal(raw, &proofs); err != nil || len(proofs) != 3 {
		t.Fatalf("fixture: %v", err)
	}
	for i, p := range proofs {
		if err := verifyConsistencyRFC9162(p.FirstSize, p.FirstRoot, p.SecondSize, p.SecondRoot, p.Hashes); err != nil {
			t.Errorf("real proof %d: %v", i, err)
		}
		if p.LogReportedRoot == p.SecondRoot {
			t.Errorf("real proof %d: the log reported the root at lastSize; the anchorer's comment says it does not", i)
		}
		bad := append([]string(nil), p.Hashes...)
		bad[len(bad)/2] = strings.Repeat("00", 32)
		if verifyConsistencyRFC9162(p.FirstSize, p.FirstRoot, p.SecondSize, p.SecondRoot, bad) == nil {
			t.Errorf("real proof %d with one hash changed still verified", i)
		}
		// Roots are what bind a proof to its trees (the sizes are bound
		// with them by Sigstore's signature on each checkpoint).
		if verifyConsistencyRFC9162(p.FirstSize, p.SecondRoot, p.SecondSize, p.SecondRoot, p.Hashes) == nil {
			t.Errorf("real proof %d verified against the wrong earlier root", i)
		}
		if verifyConsistencyRFC9162(p.FirstSize, p.FirstRoot, p.SecondSize, p.FirstRoot, p.Hashes) == nil {
			t.Errorf("real proof %d verified against the wrong later root", i)
		}
	}
}

func TestConsistencyAgreesWithTheRFCForSmallTrees(t *testing.T) {
	for n := 2; n <= 40; n++ {
		leaves := make([][]byte, n)
		for i := range leaves {
			leaves[i] = fakeLeaf([]byte{byte(i), 'c'})
		}
		for m := 1; m < n; m++ {
			proof := fakeSubproof(m, leaves, true)
			hexes := make([]string, len(proof))
			for i, p := range proof {
				hexes[i] = hex.EncodeToString(p)
			}
			if err := verifyConsistencyRFC9162(int64(m), hex.EncodeToString(fakeRoot(leaves[:m])), int64(n), hex.EncodeToString(fakeRoot(leaves)), hexes); err != nil {
				t.Fatalf("trees %d to %d: %v", m, n, err)
			}
		}
	}
}

func TestLogErrorsAndRedirectsAreNotAnchors(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"maintenance"}`, http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	a, err := NewDirectRekorAnchorer(down.URL, testAnchorKeyPEM(t), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := a.AnchorCheckpoint(context.Background(), testCheckpoint(1), TreeHead{}); err == nil {
		t.Errorf("a log answering 503 was recorded as anchored: %+v", got)
	}

	// A conflict whose location is on ANOTHER host, which does hold a
	// valid entry for this checkpoint signed by this key. Adopting it would
	// record evidence read from somewhere other than the configured log.
	real := newFakeRekor(t)
	key := testAnchorKeyPEM(t)
	onReal, _ := NewDirectRekorAnchorer(real.srv.URL, key, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := onReal.AnchorCheckpoint(context.Background(), testCheckpoint(1), TreeHead{}); err != nil {
		t.Fatal(err)
	}
	var uuid string
	for u := range real.byUUID {
		uuid = u
	}
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", real.srv.URL+"/api/v1/log/entries/"+uuid)
		w.WriteHeader(http.StatusConflict)
	}))
	t.Cleanup(elsewhere.Close)
	redirected, _ := NewDirectRekorAnchorer(elsewhere.URL, key, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got, err := redirected.AnchorCheckpoint(context.Background(), testCheckpoint(1), TreeHead{}); err == nil {
		t.Errorf("an entry read from another host was recorded as anchored: %+v", got)
	}
}

func TestSigningKeyIsCheckedAndNeverEchoed(t *testing.T) {
	good := testAnchorKeyPEM(t)
	if _, err := NewDirectRekorAnchorer("", good, slog.Default()); err != nil {
		t.Fatalf("a valid P-256 key was refused: %v", err)
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(p384)
	wrongCurve := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	garbled := bytes.Replace(good, []byte("A"), []byte("B"), 3)
	for name, k := range map[string][]byte{"empty": nil, "not pem": []byte("hello"), "wrong curve": wrongCurve, "garbled": garbled} {
		_, err := NewDirectRekorAnchorer("", k, slog.Default())
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if len(k) > 20 && strings.Contains(err.Error(), string(k[30:50])) {
			t.Errorf("%s: the error echoes key material: %v", name, err)
		}
	}
	if k, err := ParseAnchorSigningKey(good); err != nil || k.Curve != elliptic.P256() {
		t.Errorf("ParseAnchorSigningKey: %v", err)
	}
	if a, _ := NewDirectRekorAnchorer("", good, slog.Default()); a.RekorURL != DefaultRekorURL {
		t.Errorf("default log address %q, want %q", a.RekorURL, DefaultRekorURL)
	}
}

func cryptoSHA256() crypto.SignerOpts { return crypto.SHA256 }
