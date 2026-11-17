package api

// CheckpointTimestamper gives every anchored checkpoint a second public
// anchor through OpenTimestamps (owner decision D, 21 October 2026).
//
// Two jobs, both run from the checkpoint scheduler's tick:
//
//  1. Submit: each anchored checkpoint with no receipt yet has its hash
//     sent to every configured calendar. Each calendar answers at once
//     with a pending receipt, which is stored.
//  2. Upgrade: a few hours later the calendar has committed the hash to a
//     Bitcoin block. Each pending receipt's commitment is sent back to the
//     calendar that issued it; a complete answer is grafted onto the
//     receipt and stored, and the receipt is never touched again.
//
// Neither job can hold up the chain. A calendar that is down costs that
// calendar's receipt for now, and the next tick tries again. Upgrades ask
// only the calendar recorded with the receipt, never an address read
// from inside the receipt, so a hostile receipt cannot make the server
// fetch from anywhere it chooses.

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest"
	"mesedi/backend/internal/store"
)

// DefaultOTSCalendars are the public OpenTimestamps calendars the
// reference client uses by default.
var DefaultOTSCalendars = []string{
	"https://alice.btc.calendar.opentimestamps.org",
	"https://bob.btc.calendar.opentimestamps.org",
	"https://finney.calendar.eternitywall.com",
}

const (
	otsTimeout          = 20 * time.Second
	otsMaxResponseBytes = 64 << 10
	otsAccept           = "application/vnd.opentimestamps.v1"
	otsBatch            = 24
)

// CheckpointTimestamper submits and upgrades OpenTimestamps receipts.
type CheckpointTimestamper struct {
	Store      store.Store
	Calendars  []string
	HTTPClient *http.Client
	Logger     *slog.Logger
	Now        func() time.Time
}

// ParseOTSCalendars reads a comma-separated calendar list. Empty means
// DefaultOTSCalendars; "off" means none. Every entry must be an https URL.
func ParseOTSCalendars(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return append([]string(nil), DefaultOTSCalendars...), nil
	}
	if strings.EqualFold(raw, "off") {
		return nil, nil
	}
	var out []string
	for _, c := range strings.Split(raw, ",") {
		c = strings.TrimRight(strings.TrimSpace(c), "/")
		u, err := url.Parse(c)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return nil, fmt.Errorf("timestamp calendar %q is not an https URL", c)
		}
		out = append(out, c)
	}
	return out, nil
}

func (ts *CheckpointTimestamper) now() time.Time {
	if ts.Now != nil {
		return ts.Now()
	}
	return time.Now()
}

func (ts *CheckpointTimestamper) client() *http.Client {
	if ts.HTTPClient != nil {
		return ts.HTTPClient
	}
	return &http.Client{Timeout: otsTimeout}
}

// Run does one pass of both jobs. Errors are logged, never returned: the
// chain must not wait on a second anchor.
func (ts *CheckpointTimestamper) Run(ctx context.Context) {
	if len(ts.Calendars) == 0 {
		return
	}
	untimed, err := ts.Store.ListUntimestampedCheckpoints(ctx, otsBatch)
	if err != nil {
		ts.Logger.Warn("checkpoint_timestamper: list untimestamped checkpoints", "error", err.Error())
	}
	for _, u := range untimed {
		ts.submitOne(ctx, u)
	}
	pending, err := ts.Store.ListPendingTimestamps(ctx, otsBatch*len(ts.Calendars))
	if err != nil {
		ts.Logger.Warn("checkpoint_timestamper: list pending timestamps", "error", err.Error())
	}
	for _, p := range pending {
		ts.upgradeOne(ctx, p)
	}
}

// submitOne sends one checkpoint hash to every calendar and stores each
// receipt that comes back.
func (ts *CheckpointTimestamper) submitOne(ctx context.Context, u store.UntimestampedCheckpoint) {
	digest, err := hex.DecodeString(u.Hash)
	if err != nil || len(digest) != 32 {
		ts.Logger.Warn("checkpoint_timestamper: checkpoint hash is not 32 bytes of hex", "seq", u.Seq)
		return
	}
	for _, cal := range ts.Calendars {
		receipt, err := ts.submitToCalendar(ctx, cal, digest)
		if err != nil {
			ts.Logger.Warn("checkpoint_timestamper: calendar did not timestamp",
				"seq", u.Seq, "calendar", cal, "error", err.Error())
			continue
		}
		ts.record(ctx, store.CheckpointTimestamp{CheckpointSeq: u.Seq, CalendarURL: cal,
			Receipt: receipt, SubmittedAt: ts.now()})
	}
}

func (ts *CheckpointTimestamper) record(ctx context.Context, t store.CheckpointTimestamp) {
	if err := ts.Store.RecordCheckpointTimestamp(ctx, t); err != nil {
		ts.Logger.Warn("checkpoint_timestamper: store receipt", "seq", t.CheckpointSeq,
			"calendar", t.CalendarURL, "error", err.Error())
	}
}

// submitToCalendar posts the digest and returns the calendar's receipt,
// refusing one that does not parse as a timestamp on this digest or that
// promises nothing.
func (ts *CheckpointTimestamper) submitToCalendar(ctx context.Context, cal string, digest []byte) ([]byte, error) {
	status, body, err := ts.do(ctx, http.MethodPost, cal+"/digest", digest)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", status)
	}
	t, err := attest.ParseOTSTimestamp(digest, body)
	if err != nil {
		return nil, fmt.Errorf("the receipt does not read: %w", err)
	}
	if len(t.PendingClaims()) == 0 && len(t.BitcoinClaims()) == 0 {
		return nil, errors.New("the receipt makes no attestation")
	}
	if t.InvalidBitcoinAttestations() > 0 {
		return nil, errors.New("the receipt carries a Bitcoin attestation that does not read")
	}
	return t.Serialize(), nil
}

// upgradeOne asks the receipt's own calendar for the result of each of
// its pending commitments, and stores the receipt once a Bitcoin block
// attestation is in it.
func (ts *CheckpointTimestamper) upgradeOne(ctx context.Context, p store.CheckpointTimestamp) {
	digest, receipt, err := ts.readReceipt(ctx, p)
	if err != nil {
		ts.Logger.Warn("checkpoint_timestamper: stored receipt does not read",
			"seq", p.CheckpointSeq, "calendar", p.CalendarURL, "error", err.Error())
		return
	}
	changed := false
	for _, claim := range receipt.PendingClaims() {
		up, err := ts.fetchUpgrade(ctx, p.CalendarURL, claim.Commitment)
		if err != nil || up == nil {
			continue
		}
		if receipt.Graft(claim.Commitment, up) {
			changed = true
		}
	}
	if !changed || len(receipt.BitcoinClaims()) == 0 {
		return
	}
	if receipt.InvalidBitcoinAttestations() > 0 {
		ts.Logger.Warn("checkpoint_timestamper: upgraded receipt carries a Bitcoin attestation that does not read",
			"seq", p.CheckpointSeq, "calendar", p.CalendarURL)
		return
	}
	full := receipt.Serialize()
	if _, err := attest.ParseOTSTimestamp(digest, full); err != nil {
		ts.Logger.Warn("checkpoint_timestamper: upgraded receipt does not read back",
			"seq", p.CheckpointSeq, "error", err.Error())
		return
	}
	if err := ts.Store.CompleteCheckpointTimestamp(ctx, p.CheckpointSeq, p.CalendarURL, full, ts.now()); err != nil {
		ts.Logger.Warn("checkpoint_timestamper: store upgraded receipt",
			"seq", p.CheckpointSeq, "calendar", p.CalendarURL, "error", err.Error())
		return
	}
	ts.Logger.Info("checkpoint_timestamper: receipt complete",
		"seq", p.CheckpointSeq, "calendar", p.CalendarURL, "bitcoin_height", receipt.BitcoinClaims()[0].Height)
}

// readReceipt parses a stored receipt on its checkpoint's hash.
func (ts *CheckpointTimestamper) readReceipt(ctx context.Context, p store.CheckpointTimestamp) ([]byte, *attest.OTSTimestamp, error) {
	cp, err := ts.Store.GetCheckpoint(ctx, p.CheckpointSeq)
	if err != nil {
		return nil, nil, err
	}
	digest, err := hex.DecodeString(cp.Hash)
	if err != nil {
		return nil, nil, err
	}
	t, err := attest.ParseOTSTimestamp(digest, p.Receipt)
	return digest, t, err
}

// fetchUpgrade returns the calendar's timestamp on commitment, or nil
// while it is still pending.
func (ts *CheckpointTimestamper) fetchUpgrade(ctx context.Context, cal string, commitment []byte) (*attest.OTSTimestamp, error) {
	status, body, err := ts.do(ctx, http.MethodGet, cal+"/timestamp/"+hex.EncodeToString(commitment), nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusNotFound {
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", status)
	}
	return attest.ParseOTSTimestamp(commitment, body)
}

func (ts *CheckpointTimestamper) do(ctx context.Context, method, target string, body []byte) (int, []byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, otsTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(reqCtx, method, target, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", otsAccept)
	req.Header.Set("User-Agent", directAnchorUserAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := ts.client().Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("could not reach the calendar: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, otsMaxResponseBytes))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}
