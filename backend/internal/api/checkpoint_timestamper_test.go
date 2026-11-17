package api

// CheckpointTimestamper against fake OpenTimestamps calendars and a real
// SQLite store. Nothing here contacts a real calendar.

import (
	"bytes"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mesedi-ai/mesedi/backend/attest"
)

// fakeCalendar issues a pending receipt (append a nonce, hash, pending
// attestation) and, once mined, the upgrade to a Bitcoin block.
type fakeCalendar struct {
	mu        sync.Mutex
	srv       *httptest.Server
	uri       string // the address written into its own receipts
	mined     bool
	down      bool
	garbage   bool
	badBlock  bool // its Bitcoin attestations carry a payload that does not read
	requests  int
	upgradeOf map[string][]byte
}

func newFakeCalendar(t *testing.T) *fakeCalendar {
	c := &fakeCalendar{upgradeOf: map[string][]byte{}}
	c.srv = httptest.NewServer(http.HandlerFunc(c.handle))
	c.uri = c.srv.URL
	t.Cleanup(c.srv.Close)
	return c
}

func otsAttestationBytes(tag [8]byte, payload []byte) []byte {
	var p bytes.Buffer
	writeOTSVarbytes(&p, payload)
	return append(append([]byte{0x00}, tag[:]...), p.Bytes()...)
}

func writeOTSVarbytes(buf *bytes.Buffer, b []byte) {
	n := len(b)
	for n >= 0x80 {
		buf.WriteByte(byte(n) | 0x80)
		n >>= 7
	}
	buf.WriteByte(byte(n))
	buf.Write(b)
}

func (c *fakeCalendar) handle(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	if c.down {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/digest":
		if c.garbage {
			_, _ = w.Write([]byte{0x42, 0x42})
			return
		}
		digest, _ := io.ReadAll(r.Body)
		nonce := []byte("nonce-12")
		_ = digest
		var uri bytes.Buffer
		writeOTSVarbytes(&uri, []byte(c.uri))
		var body bytes.Buffer
		body.WriteByte(attest.OTSOpAppend)
		writeOTSVarbytes(&body, nonce)
		body.WriteByte(attest.OTSOpSHA256)
		if c.badBlock {
			// A fork: a valid pending attestation, and a Bitcoin one that
			// does not read.
			body.WriteByte(0xff)
			body.Write(otsAttestationBytes(attest.OTSTagPending, uri.Bytes()))
			body.Write(otsAttestationBytes(attest.OTSTagBitcoin, []byte{0xa0, 0xf7, 0x36, 0xff}))
		} else {
			body.Write(otsAttestationBytes(attest.OTSTagPending, uri.Bytes()))
		}
		_, _ = w.Write(body.Bytes())
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/timestamp/"):
		if !c.mined {
			http.NotFound(w, r)
			return
		}
		var body bytes.Buffer
		body.WriteByte(attest.OTSOpPrepend)
		writeOTSVarbytes(&body, []byte("block"))
		body.WriteByte(attest.OTSOpSHA256)
		if c.badBlock {
			// A fork: a valid block attestation beside one whose payload
			// does not read, so only the invalid-attestation check can
			// refuse the upgrade.
			body.WriteByte(0xff)
			body.Write(otsAttestationBytes(attest.OTSTagBitcoin, []byte{0xa0, 0xf7, 0x36}))
			body.Write(otsAttestationBytes(attest.OTSTagBitcoin, []byte{0xa0, 0xf7, 0x36, 0xff}))
		} else {
			body.Write(otsAttestationBytes(attest.OTSTagBitcoin, []byte{0xa0, 0xf7, 0x36})) // varuint 900000
		}
		_, _ = w.Write(body.Bytes())
	default:
		http.NotFound(w, r)
	}
}

func timestamperFixture(t *testing.T, cals ...*fakeCalendar) (*chainFixture, *CheckpointTimestamper) {
	t.Helper()
	f := newChainFixture(t, false)
	urls := make([]string, len(cals))
	for i, c := range cals {
		urls[i] = c.srv.URL
	}
	ts := &CheckpointTimestamper{Store: f.st, Calendars: urls, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	f.sch.Timestamper = ts
	return f, ts
}

func receiptsFor(t *testing.T, f *chainFixture, seq uint64) map[string]*attest.OTSTimestamp {
	t.Helper()
	cp, err := f.st.GetCheckpoint(f.ctx, seq)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := hex.DecodeString(cp.Hash)
	all, err := f.st.ListCheckpointTimestamps(f.ctx, seq, seq)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*attest.OTSTimestamp{}
	for _, r := range all[seq] {
		ts, err := attest.ParseOTSTimestamp(digest, r.Receipt)
		if err != nil {
			t.Fatalf("stored receipt from %s does not read on the checkpoint hash: %v", r.CalendarURL, err)
		}
		out[r.CalendarURL] = ts
	}
	return out
}

func TestEveryAnchoredCheckpointIsTimestampedThenUpgraded(t *testing.T) {
	a, b := newFakeCalendar(t), newFakeCalendar(t)
	f, _ := timestamperFixture(t, a, b)
	f.runOne("exec-ts")
	head, _ := f.st.LatestCheckpoint(f.ctx)
	got := receiptsFor(t, f, head.Seq)
	if len(got) != 2 {
		t.Fatalf("receipts from %d calendars, want 2", len(got))
	}
	for cal, ts := range got {
		if p := ts.PendingClaims(); len(p) != 1 || p[0].URI != cal {
			t.Fatalf("receipt from %s is not pending at that calendar: %+v", cal, p)
		}
	}

	a.mined, b.mined = true, true
	if err := f.sch.tick(f.ctx); err != nil {
		t.Fatal(err)
	}
	all, _ := f.st.ListCheckpointTimestamps(f.ctx, head.Seq, head.Seq)
	for _, r := range all[head.Seq] {
		if !r.Complete {
			t.Errorf("receipt from %s not marked complete after its upgrade", r.CalendarURL)
		}
	}
	for cal, ts := range receiptsFor(t, f, head.Seq) {
		if c := ts.BitcoinClaims(); len(c) != 1 || c[0].Height != 900000 || len(ts.PendingClaims()) != 0 {
			t.Errorf("receipt from %s after upgrade: bitcoin %+v, pending %d", cal, c, len(ts.PendingClaims()))
		}
	}
}

func TestACalendarThatIsDownOrLiesNeitherStallsTheChainNorIsStored(t *testing.T) {
	good, down, liar := newFakeCalendar(t), newFakeCalendar(t), newFakeCalendar(t)
	down.down, liar.garbage = true, true
	f, _ := timestamperFixture(t, good, down, liar)
	f.runOne("exec-ts")
	head, _ := f.st.LatestCheckpoint(f.ctx)
	if a, _ := f.st.GetCheckpointAnchor(f.ctx, head.Seq); !a.Anchored {
		t.Fatal("a failing calendar held up the chain")
	}
	got := receiptsFor(t, f, head.Seq)
	if len(got) != 1 || got[good.srv.URL] == nil {
		t.Fatalf("stored receipts from %v; want only the working calendar's", keys(got))
	}
}

func TestUpgradesAskOnlyTheRecordedCalendar(t *testing.T) {
	recorded, elsewhere := newFakeCalendar(t), newFakeCalendar(t)
	recorded.uri = elsewhere.srv.URL // its receipt names another address
	f, _ := timestamperFixture(t, recorded)
	f.runOne("exec-ts")
	before := elsewhere.requests
	elsewhere.mined, recorded.mined = true, true
	if err := f.sch.tick(f.ctx); err != nil {
		t.Fatal(err)
	}
	if elsewhere.requests != before {
		t.Fatal("the upgrade fetched from an address read out of the receipt")
	}
	head, _ := f.st.LatestCheckpoint(f.ctx)
	if c := receiptsFor(t, f, head.Seq)[recorded.srv.URL].BitcoinClaims(); len(c) != 1 {
		t.Fatal("the recorded calendar's upgrade was not applied")
	}
}

func TestCalendarListIsCheckedAndCanBeTurnedOff(t *testing.T) {
	if got, err := ParseOTSCalendars(""); err != nil || len(got) != len(DefaultOTSCalendars) {
		t.Fatalf("empty: %v, %v", got, err)
	}
	if got, err := ParseOTSCalendars(" OFF "); err != nil || len(got) != 0 {
		t.Fatalf("off: %v, %v", got, err)
	}
	if got, err := ParseOTSCalendars("https://a.example/, https://b.example"); err != nil || strings.Join(got, ",") != "https://a.example,https://b.example" {
		t.Fatalf("two: %v, %v", got, err)
	}
	for _, bad := range []string{"http://plain.example", "not a url", "https://"} {
		if _, err := ParseOTSCalendars(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func keys(m map[string]*attest.OTSTimestamp) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestExportsCarryTheAnchorKindAndEveryReceipt(t *testing.T) {
	a, b := newFakeCalendar(t), newFakeCalendar(t)
	f, _ := timestamperFixture(t, a, b)
	f.runOne("exec-ts")
	a.mined = true
	if err := f.sch.tick(f.ctx); err != nil {
		t.Fatal(err)
	}
	exp, v := f.export()
	if !v.OK {
		t.Fatalf("export failed verification: %+v", v.Checks)
	}
	head, _ := f.st.LatestCheckpoint(f.ctx)
	var iv *attest.ExportedInterval
	for i := range exp.Intervals {
		if exp.Intervals[i].Checkpoint.Seq == head.Seq {
			iv = &exp.Intervals[i]
		}
	}
	if iv == nil || iv.AnchorKind != "mesedi-direct" {
		t.Fatalf("export interval anchor kind: %+v", iv)
	}
	if len(iv.Timestamps) != 2 {
		t.Fatalf("export carries %d receipts, want 2", len(iv.Timestamps))
	}
	digest, _ := hex.DecodeString(iv.Checkpoint.Hash)
	complete := 0
	for _, r := range iv.Timestamps {
		ts, err := attest.ParseOTSTimestamp(digest, r.Receipt)
		if err != nil {
			t.Fatalf("exported receipt from %s does not read: %v", r.Calendar, err)
		}
		if r.Complete {
			complete++
			if len(ts.BitcoinClaims()) != 1 {
				t.Errorf("a receipt marked complete has no Bitcoin attestation")
			}
		}
	}
	if complete != 1 {
		t.Errorf("%d receipts complete, want 1 (only one calendar was mined)", complete)
	}
}

// A receipt carrying a Bitcoin attestation that does not read is false
// evidence: it is refused when first issued, and an upgrade carrying one
// is never stored as complete (the outside review's round 2, 17 November
// 2026).
func TestAReceiptWithAnUnreadableBitcoinAttestationIsNeverStored(t *testing.T) {
	bad := newFakeCalendar(t)
	bad.badBlock = true
	f, _ := timestamperFixture(t, bad)
	f.runOne("exec-ts")
	head, _ := f.st.LatestCheckpoint(f.ctx)
	if got := receiptsFor(t, f, head.Seq); len(got) != 0 {
		t.Fatalf("a receipt with an unreadable Bitcoin attestation was stored at submission: %v", keys(got))
	}
}

func TestAnUpgradeWithAnUnreadableBitcoinAttestationIsNotStored(t *testing.T) {
	cal := newFakeCalendar(t)
	f, _ := timestamperFixture(t, cal)
	f.runOne("exec-ts")
	head, _ := f.st.LatestCheckpoint(f.ctx)
	cal.mined, cal.badBlock = true, true
	if err := f.sch.tick(f.ctx); err != nil {
		t.Fatal(err)
	}
	all, _ := f.st.ListCheckpointTimestamps(f.ctx, head.Seq, head.Seq)
	if len(all[head.Seq]) != 1 {
		t.Fatalf("%d receipts stored, want the one pending receipt", len(all[head.Seq]))
	}
	if all[head.Seq][0].Complete {
		t.Fatal("an upgrade carrying an unreadable Bitcoin attestation was stored as complete")
	}
}
