package attest

// OpenTimestamps receipts: reading, writing, upgrading and checking them.
//
// The second public anchor (owner decision D, 21 October 2026). Each
// checkpoint hash is also sent to OpenTimestamps calendar servers, which
// commit it, among many others, to a Bitcoin transaction. The receipt is
// a list of operations (append, prepend, hash) that turns the checkpoint
// hash into a Bitcoin block's Merkle root, so anyone holding the block
// header can confirm the checkpoint existed by the time that block was
// mined. It proves WHEN; Mesedi's signature on the Rekor entry proves WHO.
//
// The format is the one the reference client writes
// (github.com/opentimestamps/python-opentimestamps, core/timestamp.py,
// core/op.py, core/notary.py), implemented here from that description:
//
//	timestamp   := (0xff entry)* entry
//	entry       := 0x00 attestation | op timestamp
//	attestation := tag[8] varbytes(payload)
//	op          := 0x08 sha256 | 0xf0 append varbytes | 0xf1 prepend varbytes
//	             | 0xf2 reverse | 0xf3 hexlify
//
// varuint is base-128, least significant group first; varbytes is a
// varuint length then that many bytes. SHA-1, RIPEMD-160 and Keccak
// operations are refused: calendars do not use them, and a proof path
// through SHA-1 is weaker than the anchor it would vouch for.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
)

// Limits, from the reference client, so a hostile receipt cannot exhaust
// memory or the stack.
const (
	otsMaxMsgLength     = 4096
	otsMaxPayloadLength = 8192
	otsMaxDepth         = 256
	otsMaxEntries       = 100000
)

// Operation tags.
const (
	OTSOpSHA256  byte = 0x08
	OTSOpAppend  byte = 0xf0
	OTSOpPrepend byte = 0xf1
	OTSOpReverse byte = 0xf2
	OTSOpHexlify byte = 0xf3
)

// Attestation tags.
var (
	OTSTagBitcoin = [8]byte{0x05, 0x88, 0x96, 0x0d, 0x73, 0xd7, 0x19, 0x01}
	OTSTagPending = [8]byte{0x83, 0xdf, 0xe3, 0x0d, 0x2e, 0xf9, 0x0c, 0x8e}
)

// otsFileMagic opens a detached .ots file; calendar answers omit it.
var otsFileMagic = []byte("\x00OpenTimestamps\x00\x00Proof\x00\xbf\x89\xe2\xe8\x84\xe8\x92\x94")

// OTSTimestamp is one node: the message at this point, the attestations
// made about it, and the operations leading on from it.
type OTSTimestamp struct {
	Msg          []byte
	Attestations []OTSAttestation
	Ops          []OTSOp
}

// OTSOp is one operation and the timestamp of its result.
type OTSOp struct {
	Tag  byte
	Arg  []byte
	Next *OTSTimestamp
}

// OTSAttestation is a claim about a node's message.
type OTSAttestation struct {
	Tag     [8]byte
	Payload []byte
}

// OTSBitcoinClaim says Msg is the Merkle root of the Bitcoin block at
// Height, in the byte order the block header stores it.
type OTSBitcoinClaim struct {
	Height uint64
	Msg    []byte
}

// OTSPendingClaim says the calendar at URI will commit Commitment to a
// Bitcoin block, and can be asked for the result.
type OTSPendingClaim struct {
	URI        string
	Commitment []byte
}

type otsReader struct {
	b       []byte
	entries int
}

func (r *otsReader) byte1() (byte, error) {
	if len(r.b) == 0 {
		return 0, errors.New("ots: receipt ends early")
	}
	c := r.b[0]
	r.b = r.b[1:]
	return c, nil
}

func (r *otsReader) bytesN(n int) ([]byte, error) {
	if n < 0 || n > len(r.b) {
		return nil, errors.New("ots: receipt ends early")
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out, nil
}

func (r *otsReader) varuint() (uint64, error) {
	var v uint64
	for shift := uint(0); shift < 64; shift += 7 {
		c, err := r.byte1()
		if err != nil {
			return 0, err
		}
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, nil
		}
	}
	return 0, errors.New("ots: varuint too long")
}

func (r *otsReader) varbytes(max int) ([]byte, error) {
	n, err := r.varuint()
	if err != nil {
		return nil, err
	}
	if n > uint64(max) {
		return nil, fmt.Errorf("ots: field of %d bytes exceeds the %d-byte limit", n, max)
	}
	return r.bytesN(int(n))
}

// ParseOTSTimestamp reads a bare timestamp (what a calendar returns) for
// msg, and refuses trailing bytes.
func ParseOTSTimestamp(msg, data []byte) (*OTSTimestamp, error) {
	r := &otsReader{b: data}
	t, err := r.timestamp(msg, otsMaxDepth)
	if err != nil {
		return nil, err
	}
	if len(r.b) != 0 {
		return nil, fmt.Errorf("ots: %d unread bytes after the timestamp", len(r.b))
	}
	return t, nil
}

// ParseOTSFile reads a detached .ots file: its magic, version, the
// SHA-256 file digest it is about, and the timestamp on that digest.
func ParseOTSFile(data []byte) ([]byte, *OTSTimestamp, error) {
	if !bytes.HasPrefix(data, otsFileMagic) {
		return nil, nil, errors.New("ots: not an OpenTimestamps file")
	}
	r := &otsReader{b: data[len(otsFileMagic):]}
	if v, err := r.varuint(); err != nil || v != 1 {
		return nil, nil, fmt.Errorf("ots: unsupported file version (%v)", err)
	}
	if op, err := r.byte1(); err != nil || op != OTSOpSHA256 {
		return nil, nil, errors.New("ots: file digest is not SHA-256")
	}
	digest, err := r.bytesN(sha256.Size)
	if err != nil {
		return nil, nil, err
	}
	t, err := r.timestamp(digest, otsMaxDepth)
	if err != nil {
		return nil, nil, err
	}
	if len(r.b) != 0 {
		return nil, nil, fmt.Errorf("ots: %d unread bytes after the timestamp", len(r.b))
	}
	return digest, t, nil
}

func (r *otsReader) timestamp(msg []byte, depth int) (*OTSTimestamp, error) {
	if depth <= 0 {
		return nil, errors.New("ots: receipt nests too deeply")
	}
	t := &OTSTimestamp{Msg: msg}
	tag, err := r.byte1()
	if err != nil {
		return nil, err
	}
	for tag == 0xff {
		next, err := r.byte1()
		if err != nil {
			return nil, err
		}
		if err := r.entry(t, next, depth); err != nil {
			return nil, err
		}
		if tag, err = r.byte1(); err != nil {
			return nil, err
		}
	}
	if err := r.entry(t, tag, depth); err != nil {
		return nil, err
	}
	return t, nil
}

func (r *otsReader) entry(t *OTSTimestamp, tag byte, depth int) error {
	r.entries++
	if r.entries > otsMaxEntries {
		return errors.New("ots: receipt has too many entries")
	}
	if tag == 0x00 {
		raw, err := r.bytesN(8)
		if err != nil {
			return err
		}
		payload, err := r.varbytes(otsMaxPayloadLength)
		if err != nil {
			return err
		}
		var a OTSAttestation
		copy(a.Tag[:], raw)
		a.Payload = payload
		t.Attestations = append(t.Attestations, a)
		return nil
	}
	var arg []byte
	if tag == OTSOpAppend || tag == OTSOpPrepend {
		var err error
		if arg, err = r.varbytes(otsMaxMsgLength); err != nil {
			return err
		}
	}
	result, err := applyOTSOp(tag, arg, t.Msg)
	if err != nil {
		return err
	}
	next, err := r.timestamp(result, depth-1)
	if err != nil {
		return err
	}
	t.Ops = append(t.Ops, OTSOp{Tag: tag, Arg: arg, Next: next})
	return nil
}

// applyOTSOp computes one operation's result.
func applyOTSOp(tag byte, arg, msg []byte) ([]byte, error) {
	var out []byte
	switch tag {
	case OTSOpSHA256:
		sum := sha256.Sum256(msg)
		out = sum[:]
	case OTSOpAppend:
		out = append(append([]byte{}, msg...), arg...)
	case OTSOpPrepend:
		out = append(append([]byte{}, arg...), msg...)
	case OTSOpReverse:
		out = make([]byte, len(msg))
		for i := range msg {
			out[i] = msg[len(msg)-1-i]
		}
	case OTSOpHexlify:
		out = []byte(hex.EncodeToString(msg))
	default:
		return nil, fmt.Errorf("ots: unsupported operation 0x%02x", tag)
	}
	if len(out) > otsMaxMsgLength {
		return nil, fmt.Errorf("ots: operation result of %d bytes exceeds the limit", len(out))
	}
	return out, nil
}

// Serialize writes the timestamp in the reference client's order:
// attestations, then operations, each sorted, with 0xff before every
// entry but the last.
func (t *OTSTimestamp) Serialize() []byte {
	var buf bytes.Buffer
	t.serializeTo(&buf)
	return buf.Bytes()
}

func (t *OTSTimestamp) serializeTo(buf *bytes.Buffer) {
	atts := append([]OTSAttestation(nil), t.Attestations...)
	sort.Slice(atts, func(i, j int) bool {
		if c := bytes.Compare(atts[i].Tag[:], atts[j].Tag[:]); c != 0 {
			return c < 0
		}
		return bytes.Compare(atts[i].Payload, atts[j].Payload) < 0
	})
	ops := append([]OTSOp(nil), t.Ops...)
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Tag != ops[j].Tag {
			return ops[i].Tag < ops[j].Tag
		}
		return bytes.Compare(ops[i].Arg, ops[j].Arg) < 0
	})
	total := len(atts) + len(ops)
	n := 0
	for _, a := range atts {
		n++
		if n < total {
			buf.WriteByte(0xff)
		}
		buf.WriteByte(0x00)
		buf.Write(a.Tag[:])
		writeVarbytes(buf, a.Payload)
	}
	for _, op := range ops {
		n++
		if n < total {
			buf.WriteByte(0xff)
		}
		buf.WriteByte(op.Tag)
		if op.Tag == OTSOpAppend || op.Tag == OTSOpPrepend {
			writeVarbytes(buf, op.Arg)
		}
		op.Next.serializeTo(buf)
	}
}

func writeVaruint(buf *bytes.Buffer, v uint64) {
	for v >= 0x80 {
		buf.WriteByte(byte(v) | 0x80)
		v >>= 7
	}
	buf.WriteByte(byte(v))
}

func writeVarbytes(buf *bytes.Buffer, b []byte) {
	writeVaruint(buf, uint64(len(b)))
	buf.Write(b)
}

// walk visits every node, depth first.
func (t *OTSTimestamp) walk(fn func(*OTSTimestamp)) {
	fn(t)
	for _, op := range t.Ops {
		op.Next.walk(fn)
	}
}

// BitcoinClaims lists every Bitcoin block attestation in the receipt.
func (t *OTSTimestamp) BitcoinClaims() []OTSBitcoinClaim {
	var out []OTSBitcoinClaim
	t.walk(func(n *OTSTimestamp) {
		for _, a := range n.Attestations {
			if a.Tag != OTSTagBitcoin {
				continue
			}
			r := &otsReader{b: a.Payload}
			h, err := r.varuint()
			if err != nil || len(r.b) != 0 {
				continue
			}
			out = append(out, OTSBitcoinClaim{Height: h, Msg: n.Msg})
		}
	})
	return out
}

// InvalidBitcoinAttestations counts Bitcoin block attestations whose
// payload does not read as a block height. BitcoinClaims leaves them out,
// so without this count a corrupted attestation would read as a receipt
// still pending rather than as a bad one (the outside review's round 2,
// 17 November 2026). A receipt with any is false evidence.
func (t *OTSTimestamp) InvalidBitcoinAttestations() int {
	n := 0
	t.walk(func(node *OTSTimestamp) {
		for _, a := range node.Attestations {
			if a.Tag != OTSTagBitcoin {
				continue
			}
			r := &otsReader{b: a.Payload}
			if _, err := r.varuint(); err != nil || len(r.b) != 0 {
				n++
			}
		}
	})
	return n
}

// PendingClaims lists every calendar still to be asked for its result.
func (t *OTSTimestamp) PendingClaims() []OTSPendingClaim {
	var out []OTSPendingClaim
	t.walk(func(n *OTSTimestamp) {
		for _, a := range n.Attestations {
			if a.Tag != OTSTagPending {
				continue
			}
			r := &otsReader{b: a.Payload}
			uri, err := r.varbytes(1000)
			if err != nil || len(r.b) != 0 {
				continue
			}
			out = append(out, OTSPendingClaim{URI: string(uri), Commitment: n.Msg})
		}
	})
	return out
}

// Graft attaches a calendar's upgrade, a timestamp on commitment, to the
// node whose message is commitment, and drops that node's pending
// attestations. Reports whether a node matched. The upgrade's own
// message must equal the commitment, so a graft can never change what
// the receipt proves about the checkpoint, only extend it.
func (t *OTSTimestamp) Graft(commitment []byte, upgrade *OTSTimestamp) bool {
	if upgrade == nil || !bytes.Equal(upgrade.Msg, commitment) {
		return false
	}
	grafted := false
	t.walk(func(n *OTSTimestamp) {
		if grafted || !bytes.Equal(n.Msg, commitment) {
			return
		}
		kept := n.Attestations[:0]
		for _, a := range n.Attestations {
			if a.Tag != OTSTagPending {
				kept = append(kept, a)
			}
		}
		n.Attestations = append(kept, upgrade.Attestations...)
		n.Ops = append(n.Ops, upgrade.Ops...)
		grafted = true
	})
	return grafted
}
