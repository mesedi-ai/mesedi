// Package rekorverify tests, the cryptographic correctness of the
// RFC 6962 Merkle inclusion proof recomputation.
//
// These tests use deterministic synthetic trees built directly in Go so
// the expected roots are computable from first principles. The whole
// point is that a buggy `recomputeRootRFC6962` would silently let bad
// proofs verify (or reject good ones), which would invalidate every
// "anchored on Rekor" claim downstream, so we want this code path
// drilled with tests before anything else uses it.
package main

// PORTED VERBATIM from verdifax-orchestrator/internal/rekorverify on
// 2026-09-05. Only the package clause changed; the code and its
// comments above are unmodified.
//
// COPIED RATHER THAN IMPORTED, for three reasons in order of weight: it
// lives under internal/ in a different Go module and is not importable
// at all; this binary is MIT and must not acquire a dependency on a
// private repository; and its worth rests on an auditor being able to
// read the whole thing, which a cross-repo dependency defeats.
//
// THE TWO COPIES MUST STAY IN STEP AND NO COMPILER WILL ENFORCE IT. If
// the Rekor proof format or the checkpoint signature scheme changes,
// both move together. The test file came across with the code for
// exactly that reason: it is the record of what these checks are meant
// to catch.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// Reference Merkle tree builder (so test vectors are computable)
// ─────────────────────────────────────────────────────────────────────────────

// buildRFC6962Tree constructs a complete RFC 6962 Merkle tree from a
// list of leaf-data byte slices. Returns the leaf hashes (already
// 0x00-prefixed and SHA-256'd), and the root hash.
//
// For each internal node, hash = SHA256(0x01 || left || right).
// For each leaf, hash = SHA256(0x00 || data).
//
// This is the simplest possible reference implementation; it is NOT
// the one being tested. The function under test (recomputeRootRFC6962)
// works from a single leaf hash + sibling path and must produce the
// same root as this builder.
func buildRFC6962Tree(leaves [][]byte) (leafHashes [][]byte, root []byte) {
	leafHashes = make([][]byte, len(leaves))
	for i, leaf := range leaves {
		h := sha256.New()
		h.Write([]byte{0x00})
		h.Write(leaf)
		leafHashes[i] = h.Sum(nil)
	}
	root = computeMerkleRootRecursive(leafHashes)
	return
}

// computeMerkleRootRecursive is the textbook RFC 6962 root computation
// , used by the tests as the ground truth.
func computeMerkleRootRecursive(hashes [][]byte) []byte {
	if len(hashes) == 0 {
		// Empty tree, RFC 6962 §2.1 defines this as SHA-256("").
		empty := sha256.Sum256(nil)
		return empty[:]
	}
	if len(hashes) == 1 {
		return hashes[0]
	}
	// Split at the largest power of 2 less than n.
	k := 1
	for k*2 < len(hashes) {
		k *= 2
	}
	left := computeMerkleRootRecursive(hashes[:k])
	right := computeMerkleRootRecursive(hashes[k:])
	return hashChildren(left, right)
}

// inclusionProof returns the sibling path for leaf at index i in a
// tree of n leaves, the same shape Rekor returns and that
// recomputeRootRFC6962 consumes.
func inclusionProof(hashes [][]byte, i int) [][]byte {
	if len(hashes) <= 1 {
		return nil
	}
	k := 1
	for k*2 < len(hashes) {
		k *= 2
	}
	if i < k {
		// Leaf is in the left half, sibling is the right subtree root.
		sub := inclusionProof(hashes[:k], i)
		return append(sub, computeMerkleRootRecursive(hashes[k:]))
	}
	// Leaf is in the right half, sibling is the left subtree root.
	sub := inclusionProof(hashes[k:], i-k)
	return append(sub, computeMerkleRootRecursive(hashes[:k]))
}

// ─────────────────────────────────────────────────────────────────────────────
// Tests
// ─────────────────────────────────────────────────────────────────────────────

func TestRecomputeRootRFC6962_SingleLeafTree(t *testing.T) {
	// A tree with one leaf has no siblings, root == leaf hash.
	leaves := [][]byte{[]byte("alpha")}
	leafHashes, expectedRoot := buildRFC6962Tree(leaves)

	got, err := recomputeRootRFC6962(leafHashes[0], 0, 1, nil)
	if err != nil {
		t.Fatalf("recomputeRootRFC6962 returned error: %v", err)
	}
	if !bytesEqual(got, expectedRoot) {
		t.Fatalf("root mismatch:\n  got      %s\n  expected %s",
			hex.EncodeToString(got), hex.EncodeToString(expectedRoot))
	}
}

func TestRecomputeRootRFC6962_TwoLeafTree(t *testing.T) {
	leaves := [][]byte{[]byte("alpha"), []byte("beta")}
	leafHashes, expectedRoot := buildRFC6962Tree(leaves)

	for i := 0; i < len(leaves); i++ {
		path := inclusionProof(leafHashes, i)
		got, err := recomputeRootRFC6962(leafHashes[i], int64(i), int64(len(leaves)), path)
		if err != nil {
			t.Fatalf("leaf %d: recomputeRootRFC6962 returned error: %v", i, err)
		}
		if !bytesEqual(got, expectedRoot) {
			t.Fatalf("leaf %d: root mismatch:\n  got      %s\n  expected %s",
				i, hex.EncodeToString(got), hex.EncodeToString(expectedRoot))
		}
	}
}

func TestRecomputeRootRFC6962_FourLeafTree(t *testing.T) {
	leaves := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d")}
	leafHashes, expectedRoot := buildRFC6962Tree(leaves)

	for i := 0; i < len(leaves); i++ {
		path := inclusionProof(leafHashes, i)
		got, err := recomputeRootRFC6962(leafHashes[i], int64(i), int64(len(leaves)), path)
		if err != nil {
			t.Fatalf("leaf %d: recomputeRootRFC6962 returned error: %v", i, err)
		}
		if !bytesEqual(got, expectedRoot) {
			t.Fatalf("leaf %d: root mismatch:\n  got      %s\n  expected %s",
				i, hex.EncodeToString(got), hex.EncodeToString(expectedRoot))
		}
	}
}

// TestRecomputeRootRFC6962_LopsidedTree exercises the RFC 6962
// asymmetric-split case where len(hashes) is not a power of 2. The
// algorithm picks the largest power of 2 less than n for the left
// subtree and recurses on the unbalanced right subtree.
func TestRecomputeRootRFC6962_LopsidedTree(t *testing.T) {
	// 5 leaves: split is left=4, right=1.
	leaves := [][]byte{
		[]byte("L0"), []byte("L1"), []byte("L2"), []byte("L3"), []byte("L4"),
	}
	leafHashes, expectedRoot := buildRFC6962Tree(leaves)

	for i := 0; i < len(leaves); i++ {
		path := inclusionProof(leafHashes, i)
		got, err := recomputeRootRFC6962(leafHashes[i], int64(i), int64(len(leaves)), path)
		if err != nil {
			t.Fatalf("leaf %d: recomputeRootRFC6962 returned error: %v", i, err)
		}
		if !bytesEqual(got, expectedRoot) {
			t.Fatalf("leaf %d: root mismatch:\n  got      %s\n  expected %s",
				i, hex.EncodeToString(got), hex.EncodeToString(expectedRoot))
		}
	}
}

// TestRecomputeRootRFC6962_LargeTree exercises a randomly-sized tree
// (47 leaves) to stress the index/sibling-path bookkeeping over
// multiple deep paths. 47 is intentionally chosen, neither a power
// of 2 nor a power-of-2-minus-1, so the lopsidedness varies along
// every path.
func TestRecomputeRootRFC6962_LargeTree(t *testing.T) {
	const n = 47
	leaves := make([][]byte, n)
	for i := 0; i < n; i++ {
		leaves[i] = []byte{byte(i)}
	}
	leafHashes, expectedRoot := buildRFC6962Tree(leaves)

	for i := 0; i < n; i++ {
		path := inclusionProof(leafHashes, i)
		got, err := recomputeRootRFC6962(leafHashes[i], int64(i), int64(n), path)
		if err != nil {
			t.Fatalf("leaf %d: recomputeRootRFC6962 returned error: %v", i, err)
		}
		if !bytesEqual(got, expectedRoot) {
			t.Fatalf("leaf %d: root mismatch:\n  got      %s\n  expected %s",
				i, hex.EncodeToString(got), hex.EncodeToString(expectedRoot))
		}
	}
}

// TestRecomputeRootRFC6962_TamperedSiblingFails confirms that flipping
// a single bit in a sibling hash makes the recomputed root differ
// from the expected root, i.e. we don't silently accept invalid
// proofs. This is the safety property the entire Verdifax-on-Rekor
// claim depends on.
func TestRecomputeRootRFC6962_TamperedSiblingFails(t *testing.T) {
	leaves := [][]byte{
		[]byte("a"), []byte("b"), []byte("c"), []byte("d"),
		[]byte("e"), []byte("f"), []byte("g"), []byte("h"),
	}
	leafHashes, expectedRoot := buildRFC6962Tree(leaves)

	// Build a valid path for leaf 3, then tamper with the first sibling.
	path := inclusionProof(leafHashes, 3)
	if len(path) == 0 {
		t.Fatal("expected non-empty inclusion path for leaf 3 of an 8-leaf tree")
	}
	tampered := make([][]byte, len(path))
	copy(tampered, path)
	tampered[0] = make([]byte, sha256.Size)
	copy(tampered[0], path[0])
	tampered[0][0] ^= 0x01 // flip one bit

	got, err := recomputeRootRFC6962(leafHashes[3], 3, int64(len(leaves)), tampered)
	if err != nil {
		// Some tamper patterns might not produce a length-mismatch
		// error, the path length is preserved, only the bytes change.
		t.Fatalf("recomputeRootRFC6962 returned unexpected error: %v", err)
	}
	if bytesEqual(got, expectedRoot) {
		t.Fatal("tampered sibling should NOT recompute to the expected root, but did")
	}
}

// TestRecomputeRootRFC6962_OutOfRangeIndex confirms that a log_index
// >= tree_size is rejected up front, guarding against an attacker
// who crafts a proof for "leaf 1000 in a tree of 100 leaves".
func TestRecomputeRootRFC6962_OutOfRangeIndex(t *testing.T) {
	leaves := [][]byte{[]byte("a"), []byte("b"), []byte("c"), []byte("d")}
	leafHashes, _ := buildRFC6962Tree(leaves)

	_, err := recomputeRootRFC6962(leafHashes[0], 5, 4, nil)
	if err == nil {
		t.Fatal("expected error for log_index >= tree_size, got nil")
	}
}

// TestVerifyAnchor_RejectsEmptyPubkey confirms that the top-level
// VerifyAnchor surfaces a clear, descriptive error when the
// embedded Rekor public key is unpopulated. This is the safety
// guarantee that prevents accidental "verified" results from a
// build with an empty key file.
func TestVerifyAnchor_RejectsEmptyPubkey(t *testing.T) {
	// The current rekor_pubkey.go has RekorPublicKeyPEM = "".
	// VerifyAnchor's first calls don't need the pubkey (they validate
	// input + recompute Merkle root from in-memory data), but the
	// signed-checkpoint check does. We construct a structurally-valid
	// AnchorInput so the failure is specifically the empty-pubkey one.
	leaves := [][]byte{[]byte("a"), []byte("b")}
	leafHashes, root := buildRFC6962Tree(leaves)
	path := inclusionProof(leafHashes, 0)

	in := AnchorInput{
		LeafHashHex:   hex.EncodeToString(leafHashes[0]),
		LogIndex:      0,
		TreeSize:      2,
		RootHashHex:   hex.EncodeToString(root),
		InclusionPath: hexEncode(path),
		Checkpoint:    "rekor.sigstore.dev\n2\n" + base64Encode(root) + "\n\n,  rekor SBYx//==\n",
		LogID:         "deadbeef" + repeatHex("0", 56),
	}

	err := VerifyAnchor(in)
	if err == nil {
		t.Fatal("expected an error from VerifyAnchor when RekorPublicKeyPEM is empty, got nil")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Test helpers
// ─────────────────────────────────────────────────────────────────────────────

func hexEncode(slices [][]byte) []string {
	out := make([]string, len(slices))
	for i, s := range slices {
		out[i] = hex.EncodeToString(s)
	}
	return out
}

func base64Encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

func repeatHex(c string, n int) string {
	s := ""
	for i := 0; i < n; i++ {
		s += c
	}
	return s
}
