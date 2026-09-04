package attest

import (
	"encoding/hex"
	"errors"
	"fmt"
)

// Inclusion proofs for the INTERVAL tree — the tree whose leaves are
// tenant leaves and whose root is the checkpoint's MerkleRoot.
//
// Why this exists separately from Prove in digest.go: that one proves an
// event is inside one execution's digest. This one proves a tenant's leaf
// is inside an hour's checkpoint. An auditor needs both to get from "my
// event happened" to "and it is under a root anchored in a public log".
//
// The privacy argument is the whole point. VerifyIntervalLeaves already
// lets someone confirm a root, but only by handing them EVERY tenant's
// leaf for that interval — which discloses how many customers exist and
// their relative ordering. A proof discloses about log2(n) opaque sibling
// hashes instead, so an agency can verify its own inclusion without
// learning anything about anyone else's.

// ErrLeafNotInInterval is returned when the named project has no leaf in
// the interval. Distinct from a malformed proof on purpose: "you were not
// in this hour" is a legitimate, expected answer for an agency that ran
// nothing, and it must not be reported as a verification failure.
var ErrLeafNotInInterval = errors.New("attest: project has no leaf in this interval")

// ProveTenantLeaf returns the inclusion proof for one project's leaf
// under a checkpoint's MerkleRoot.
//
// leaves MUST be in the order they were committed — the order the
// checkpoint's root was computed over, which is the stored position
// column, sorted by project id. Any other order produces a proof against
// a root that does not exist. This is checked, not assumed: the function
// recomputes the root from the supplied leaves and refuses if it does not
// match the checkpoint, so a caller that passes leaves in map-iteration
// order gets an error rather than a plausible-looking wrong proof.
//
// Keyed by projectID rather than by index deliberately. An index argument
// invites an off-by-one that yields a VALID proof for the WRONG tenant —
// the verifier would happily confirm it, and the auditor would be looking
// at someone else's leaf believing it was theirs.
func ProveTenantLeaf(c Checkpoint, leaves []TenantLeaf, projectID string) (InclusionProof, error) {
	if projectID == "" {
		return InclusionProof{}, fmt.Errorf("attest: empty project id")
	}

	// Refuse before proving if these leaves are not the ones the
	// checkpoint committed to. Everything below would otherwise produce a
	// well-formed proof against a root nobody anchored.
	if err := VerifyIntervalLeaves(c, leaves); err != nil {
		return InclusionProof{}, fmt.Errorf("attest: cannot prove against a root these "+
			"leaves do not produce: %w", err)
	}

	index := -1
	leafHex := make([]string, 0, len(leaves))
	for i, l := range leaves {
		leafHex = append(leafHex, TenantLeafHash(l))
		if l.ProjectID == projectID {
			if index >= 0 {
				// Two leaves for one project in one interval means the
				// checkpoint builder is broken. Refusing is the only safe
				// answer: proving the first would silently hide the second.
				return InclusionProof{}, fmt.Errorf(
					"attest: project %q appears at both leaf %d and leaf %d of checkpoint %d",
					projectID, index, i, c.Seq)
			}
			index = i
		}
	}
	if index < 0 {
		return InclusionProof{}, fmt.Errorf("%w: project %q, checkpoint %d",
			ErrLeafNotInInterval, projectID, c.Seq)
	}

	path, err := provePath(leafHex, index)
	if err != nil {
		return InclusionProof{}, err
	}

	return InclusionProof{
		LeafIndex: index,
		LeafHash:  leafHex[index],
		TreeSize:  len(leafHex),
		Root:      c.MerkleRoot,
		Path:      path,
		Algorithm: AlgorithmV1,
	}, nil
}

// VerifyTenantLeafInclusion checks that a tenant leaf is under a
// checkpoint's anchored root.
//
// Deliberately takes the TenantLeaf rather than a leaf hash. Given only a
// hash, a verifier confirms that SOME leaf is in the tree; given the leaf,
// it confirms that THIS tenant's leaf, with these counts and this
// predecessor, is in the tree. The hash is recomputed here rather than
// read from the proof, so a proof carrying a leaf hash that does not match
// the leaf it claims to describe is rejected — otherwise an attacker could
// pair a genuine proof with a fabricated leaf.
func VerifyTenantLeafInclusion(c Checkpoint, leaf TenantLeaf, p InclusionProof) error {
	want := TenantLeafHash(leaf)
	if p.LeafHash != want {
		return fmt.Errorf("attest: proof is for leaf %s but the supplied leaf hashes to %s",
			short(p.LeafHash), short(want))
	}
	if p.Root != c.MerkleRoot {
		return fmt.Errorf("attest: proof is against root %s but checkpoint %d anchored %s",
			short(p.Root), c.Seq, short(c.MerkleRoot))
	}
	if p.TreeSize != c.TenantLeafCount {
		return fmt.Errorf("attest: proof claims a tree of %d but checkpoint %d committed to %d",
			p.TreeSize, c.Seq, c.TenantLeafCount)
	}
	if _, err := hex.DecodeString(p.LeafHash); err != nil {
		return fmt.Errorf("attest: malformed leaf hash in proof: %w", err)
	}

	ok, err := VerifyInclusion(p)
	if err != nil {
		return fmt.Errorf("attest: checkpoint %d: %w", c.Seq, err)
	}
	if !ok {
		return fmt.Errorf("attest: leaf for project %q is NOT under checkpoint %d's anchored root",
			leaf.ProjectID, c.Seq)
	}
	return nil
}
