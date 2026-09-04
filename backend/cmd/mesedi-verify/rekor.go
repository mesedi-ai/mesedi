package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Resolving checkpoints against the public transparency log.
//
// This is the file that makes the report evidence rather than
// self-assessment. Everything else in this binary checks that Mesedi's
// export is internally consistent — which a Mesedi that lied consistently
// would also pass. Only asking a log that neither Mesedi nor Verdifax
// controls whether a given hash is actually in it breaks that circle.
//
// WHAT IS CHECKED: that the entry at the claimed log index is a
// hashedrekord whose recorded sha256 is the checkpoint's hash.
//
// WHAT IS NOT: the log's signed tree head. Be precise about what that
// leaves open, because the obvious reading is wrong. Verifying the tree
// head would guard against SIGSTORE being dishonest. It is not what
// protects anyone from MESEDI — for a hash to be found here at all, Mesedi
// must genuinely have published it to an append-only log it does not
// control, which is the property being sold. Interception is already
// covered by TLS. So the residual is the trust assumption this design
// states openly: independence comes from the log, and if the log lies the
// guarantee is gone.
//
// Doing it properly needs Rekor's public key obtained trustworthily, which
// means Sigstore's TUF root — a heavy dependency in a binary whose value
// depends on an auditor being able to read it in one sitting. Rigour in
// the verifier bought by making the verifier unverifiable is a bad trade.
//
// The more valuable adjacent work is durability rather than cryptography:
// capturing Rekor's inclusion proof and signed entry timestamp AT ANCHOR
// TIME so the evidence still verifies when Rekor is unreachable or that
// log instance has been retired. Verdifax's /attest already returns the
// inclusion proof and Mesedi currently discards it. See the tasks.

// DefaultRekorURL is Sigstore's public good instance — free, with a 99.5%
// availability SLO, and operated by neither party to this transaction,
// which is the entire reason it is worth anchoring to.
const DefaultRekorURL = "https://rekor.sigstore.dev"

// LogEntryCheck is the outcome of resolving one checkpoint.
type LogEntryCheck struct {
	Seq        uint64
	LogIndex   string
	OK         bool
	Detail     string
	Integrated time.Time
}

type rekorClient struct {
	baseURL string
	http    *http.Client
}

func newRekorClient(baseURL string) *rekorClient {
	return &rekorClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		// A generous but finite timeout. An auditor running this against a
		// month of checkpoints makes hundreds of calls; hanging on one is
		// worse than failing it, because a hang looks like a broken tool
		// rather than a reachability problem.
		http: &http.Client{Timeout: 20 * time.Second},
	}
}

// rekorEntry is the subset of Rekor's response this needs.
//
// The json tags are camelCase because they are REKOR'S wire names, not
// ours. They cannot be renamed to match this repo's snake_case convention
// without simply failing to parse the log's responses. The audit's E13
// check assumes every struct describes something we control; this one
// describes somebody else's API, and the foreign format wins.
//
// The API returns an object keyed by entry UUID, so the shape is
// map[string]rekorEntry with exactly one member. Body is base64 of the
// canonicalised entry JSON.
type rekorEntry struct {
	Body           string `json:"body"`
	LogIndex       int64  `json:"logIndex"`
	IntegratedTime int64  `json:"integratedTime"`
	LogID          string `json:"logID"`
}

// hashedRekordBody is the decoded Body. Only the recorded digest is read;
// the signature and public key in the entry attest that Verdifax submitted
// it, which is a separate question from whether the hash is in the log.
type hashedRekordBody struct {
	Kind string `json:"kind"`
	Spec struct {
		Data struct {
			Hash struct {
				Algorithm string `json:"algorithm"`
				Value     string `json:"value"`
			} `json:"hash"`
		} `json:"data"`
	} `json:"spec"`
}

// lookup fetches the entry at a log index and returns the sha256 it records.
func (c *rekorClient) lookup(ctx context.Context, logIndex string) (string, time.Time, error) {
	idx, err := strconv.ParseInt(logIndex, 10, 64)
	if err != nil || idx < 0 {
		return "", time.Time{}, fmt.Errorf(
			"log entry id %q is not a log index; this verifier expects the decimal "+
				"index Rekor returns on submission", logIndex)
	}

	endpoint := fmt.Sprintf("%s/api/v1/log/entries?logIndex=%s",
		c.baseURL, url.QueryEscape(logIndex))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("could not reach the transparency log: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// The sharpest possible finding: a checkpoint claiming an entry
		// the log does not have. Named explicitly rather than folded into
		// a generic HTTP error, because this is the one result that means
		// the record was never published.
		return "", time.Time{}, fmt.Errorf(
			"the log has no entry at index %s. The checkpoint claims to have been "+
				"published there and it was not", logIndex)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", time.Time{}, fmt.Errorf("transparency log returned %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	// 4 MiB is far more than an entry needs and bounds a hostile or broken
	// endpoint; without it a malicious response could exhaust memory on the
	// auditor's machine.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", time.Time{}, err
	}

	var entries map[string]rekorEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return "", time.Time{}, fmt.Errorf("could not parse the log's response: %w", err)
	}
	if len(entries) != 1 {
		return "", time.Time{}, fmt.Errorf(
			"expected exactly one entry at index %s, the log returned %d",
			logIndex, len(entries))
	}

	var entry rekorEntry
	for _, e := range entries {
		entry = e
	}

	// The log must agree about which index this is. A response for a
	// different index would mean a redirect, a proxy, or a substitution.
	if entry.LogIndex != idx {
		return "", time.Time{}, fmt.Errorf(
			"asked the log for index %d and it answered with index %d",
			idx, entry.LogIndex)
	}

	bodyJSON, err := base64.StdEncoding.DecodeString(entry.Body)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("entry body is not valid base64: %w", err)
	}
	var decoded hashedRekordBody
	if err := json.Unmarshal(bodyJSON, &decoded); err != nil {
		return "", time.Time{}, fmt.Errorf("entry body is not valid JSON: %w", err)
	}
	if decoded.Kind != "hashedrekord" {
		return "", time.Time{}, fmt.Errorf(
			"entry at index %s is a %q, not a hashedrekord", logIndex, decoded.Kind)
	}
	if alg := decoded.Spec.Data.Hash.Algorithm; alg != "sha256" {
		return "", time.Time{}, fmt.Errorf(
			"entry at index %s records a %s digest; this chain uses sha256", logIndex, alg)
	}

	var integrated time.Time
	if entry.IntegratedTime > 0 {
		integrated = time.Unix(entry.IntegratedTime, 0).UTC()
	}
	return strings.ToLower(decoded.Spec.Data.Hash.Value), integrated, nil
}
