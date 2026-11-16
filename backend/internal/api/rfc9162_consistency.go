package api

// Consistency proof verification, RFC 9162 section 2.1.4.2: does the tree
// of size second, with root secondRoot, contain the tree of size first,
// with root firstRoot, as a prefix? Interior nodes are RFC 6962's
// SHA-256(0x01 || left || right).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

func rfc6962Node(left, right []byte) []byte {
	h := sha256.New()
	h.Write([]byte{0x01})
	h.Write(left)
	h.Write(right)
	return h.Sum(nil)
}

func verifyConsistencyRFC9162(first int64, firstRootHex string, second int64, secondRootHex string, pathHex []string) error {
	firstRoot, err1 := hex.DecodeString(strings.ToLower(firstRootHex))
	secondRoot, err2 := hex.DecodeString(strings.ToLower(secondRootHex))
	if err1 != nil || err2 != nil || len(firstRoot) != sha256.Size || len(secondRoot) != sha256.Size {
		return errors.New("a root is not 32 bytes of hex")
	}
	path := make([][]byte, len(pathHex))
	for i, h := range pathHex {
		b, err := hex.DecodeString(strings.ToLower(h))
		if err != nil || len(b) != sha256.Size {
			return fmt.Errorf("proof hash %d is not 32 bytes of hex", i)
		}
		path[i] = b
	}
	if first <= 0 || second < first {
		return fmt.Errorf("sizes %d and %d cannot be consistent", first, second)
	}
	if first == second {
		if len(path) != 0 || !bytes.Equal(firstRoot, secondRoot) {
			return errors.New("equal sizes need equal roots and no proof")
		}
		return nil
	}
	if len(path) == 0 {
		return errors.New("empty proof between different sizes")
	}
	if first&(first-1) == 0 { // a power of two: the first tree is a complete subtree
		path = append([][]byte{firstRoot}, path...)
	}
	fn, sn := first-1, second-1
	for fn&1 == 1 {
		fn >>= 1
		sn >>= 1
	}
	fr, sr := path[0], path[0]
	for _, c := range path[1:] {
		if sn == 0 {
			return errors.New("the proof is longer than the trees are deep")
		}
		if fn&1 == 1 || fn == sn {
			fr = rfc6962Node(c, fr)
			sr = rfc6962Node(c, sr)
			for fn&1 == 0 && fn != 0 {
				fn >>= 1
				sn >>= 1
			}
		} else {
			sr = rfc6962Node(sr, c)
		}
		fn >>= 1
		sn >>= 1
	}
	if sn != 0 || !bytes.Equal(fr, firstRoot) || !bytes.Equal(sr, secondRoot) {
		return errors.New("the proof does not lead to both roots")
	}
	return nil
}
