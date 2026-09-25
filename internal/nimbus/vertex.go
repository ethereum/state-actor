package nimbus

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
	"github.com/holiman/uint256"
)

// Vertex record layout (aristo_blobify.nim blobifyTo / deblobify). Every
// record ends with a trailer byte `bits<<6 | len(hexPrefix)`:
//
//	Branch:    [key32] ‖ SBE(startVid) ‖ BE16(used) ‖ trailer            (no hex-prefix)
//	ExtBranch: [key32] ‖ SBE(startVid) ‖ BE16(used) ‖ HP(pfx,ext) ‖ trailer
//	AccLeaf:   payload ‖ HP(pfx,leaf) ‖ trailer   payload = fields ‖ BE16(lens) ‖ mask
//	StoLeaf:   payload ‖ HP(pfx,leaf) ‖ trailer   payload = SBE(value) ‖ 0x20
//
// bits: 0b01 = leaf, 0b10 = a 32-byte Merkle key precedes the record (branches
// only, and only when the node's key is a full hash rather than an inline
// short node). Leaves never carry a key.
const (
	bitsLeaf   byte = 0b01
	bitsHasKey byte = 0b10

	maskNonce    byte = 0x01
	maskBalance  byte = 0x02
	maskStoID    byte = 0x04
	maskCodeHash byte = 0x08
	maskStoHint  byte = 0x10
	maskStoLeaf  byte = 0x20
)

// VertexType is the decoded record kind.
type VertexType uint8

const (
	Branch VertexType = iota
	ExtBranch
	AccLeaf
	StoLeaf
)

func (t VertexType) String() string {
	switch t {
	case Branch:
		return "Branch"
	case ExtBranch:
		return "ExtBranch"
	case AccLeaf:
		return "AccLeaf"
	case StoLeaf:
		return "StoLeaf"
	}
	return fmt.Sprintf("VertexType(%d)", uint8(t))
}

// AppendBranch appends a Branch (empty pfx) or ExtBranch (non-empty pfx)
// record. key32 is nil (no stored key) or exactly 32 bytes.
func AppendBranch(dst []byte, key32 []byte, startVid VertexID, used uint16, pfx []byte) []byte {
	var bits byte
	switch len(key32) {
	case 0:
	case 32:
		dst = append(dst, key32...)
		bits = bitsHasKey
	default:
		panic("nimbus: branch key must be nil or 32 bytes")
	}
	dst = AppendSBE(dst, uint64(startVid))
	dst = append(dst, byte(used>>8), byte(used))
	hpLen := 0
	if len(pfx) > 0 {
		start := len(dst)
		dst = AppendHexPrefix(dst, pfx, false)
		hpLen = len(dst) - start
	}
	return append(dst, bits<<6|byte(hpLen))
}

// AppendAccLeafPayload appends an account leaf payload: the present fields in
// nonce / balance / stoID / stoHint / codeHash order, then BE16(lens) and the
// presence mask. lens packs (len-1) of the SBE nonce (3 bits), balance (5 bits
// at <<3) and stoID (3 bits at <<8). stoID 0 means "no storage trie"; the code
// hash is omitted when it is EmptyCodeHash.
func AppendAccLeafPayload(dst []byte, nonce uint64, balance *uint256.Int, stoID VertexID, stoHint byte, codeHash common.Hash) []byte {
	var lens uint16
	var mask byte
	if nonce > 0 {
		mask |= maskNonce
		lens |= uint16(SBELen(nonce) - 1)
		dst = AppendSBE(dst, nonce)
	}
	if balance != nil && !balance.IsZero() {
		mask |= maskBalance
		b := balance.Bytes()
		lens |= uint16(len(b)-1) << 3
		dst = append(dst, b...)
	}
	if stoID != 0 {
		mask |= maskStoID
		lens |= uint16(SBELen(uint64(stoID))-1) << 8
		dst = AppendSBE(dst, uint64(stoID))
	}
	if stoHint > 0 {
		mask |= maskStoHint
		dst = append(dst, stoHint)
	}
	if codeHash != EmptyCodeHash {
		mask |= maskCodeHash
		dst = append(dst, codeHash[:]...)
	}
	dst = append(dst, byte(lens>>8), byte(lens))
	return append(dst, mask)
}

// AppendStoLeafPayload appends a storage leaf payload for a non-zero 32-byte
// slot value: SBE(value) ‖ 0x20.
func AppendStoLeafPayload(dst []byte, value common.Hash) []byte {
	dst = AppendSBEBytes32(dst, value)
	return append(dst, maskStoLeaf)
}

// AppendStoLeafPayloadFromRLP is AppendStoLeafPayload for a value that is
// already RLP-encoded (what internal/streamingtrie hands a HashBuilder). A
// trimmed non-zero slot value is at most 32 bytes, so its RLP is either the
// single byte itself (< 0x80) or a one-byte length prefix followed by the
// content — which is exactly the SBE bytes.
func AppendStoLeafPayloadFromRLP(dst []byte, valueRLP []byte) []byte {
	switch {
	case len(valueRLP) == 1 && valueRLP[0] < 0x80:
		dst = append(dst, valueRLP[0])
	case len(valueRLP) >= 2 && valueRLP[0] > 0x80 && valueRLP[0] <= 0xa0 && len(valueRLP) == int(valueRLP[0]-0x80)+1:
		dst = append(dst, valueRLP[1:]...)
	default:
		// 0x80 (zero — callers must skip it) or malformed input: encode as the
		// zero byte so the record stays well-formed; the verifier rejects the
		// zero-valued leaf downstream.
		dst = append(dst, 0)
	}
	return append(dst, maskStoLeaf)
}

// AppendLeafRecord appends payload ‖ HP(pfx, leaf) ‖ trailer.
func AppendLeafRecord(dst, payload, pfx []byte) []byte {
	dst = append(dst, payload...)
	start := len(dst)
	dst = AppendHexPrefix(dst, pfx, true)
	hpLen := len(dst) - start
	return append(dst, bitsLeaf<<6|byte(hpLen))
}

// Vertex is a decoded record. Balance is never nil for an AccLeaf.
type Vertex struct {
	Type VertexType
	Pfx  []byte // extension path (ExtBranch) or remaining leaf path (leaves)

	// Branch / ExtBranch
	Key      []byte // nil or the stored 32-byte Merkle key
	StartVid VertexID
	Used     uint16

	// AccLeaf
	Nonce    uint64
	Balance  *uint256.Int
	StoID    VertexID // 0 = no storage trie
	StoHint  byte
	CodeHash common.Hash

	// StoLeaf
	StoValue common.Hash
}

var errVertex = errors.New("nimbus: malformed vertex record")

// DecodeVertex mirrors aristo_blobify.nim's deblobify and deblobifyLeaf,
// rejecting exactly what nimbus rejects (plus trailing payload bytes, which
// nimbus ignores and this package never writes).
func DecodeVertex(rec []byte) (Vertex, error) {
	if len(rec) < 3 {
		return Vertex{}, fmt.Errorf("%w: %d bytes", errVertex, len(rec))
	}
	if len(rec) > MaxVertexBlobSize {
		return Vertex{}, fmt.Errorf("%w: %d bytes exceeds the %d-byte reader buffer", errVertex, len(rec), MaxVertexBlobSize)
	}
	trailer := rec[len(rec)-1]
	bits := trailer >> 6
	isLeaf := bits&bitsLeaf != 0
	hasKey := bits&bitsHasKey != 0
	psLen := int(trailer & 0x3f)
	start := 0
	if hasKey {
		start = 32
	}
	if psLen > len(rec)-2 || start > len(rec)-2-psLen {
		return Vertex{}, fmt.Errorf("%w: path segment / key do not fit", errVertex)
	}
	psPos := len(rec) - psLen - 1

	var pfx []byte
	if psLen > 0 {
		nib, leafFlag, err := DecodeHexPrefix(rec[psPos : len(rec)-1])
		if err != nil {
			return Vertex{}, err
		}
		if leafFlag != isLeaf {
			return Vertex{}, fmt.Errorf("%w: hex-prefix leaf flag disagrees with record type", errVertex)
		}
		pfx = nib
	}

	if !isLeaf {
		svLen := psPos - start - 2
		if svLen < 1 {
			return Vertex{}, fmt.Errorf("%w: branch without startVid", errVertex)
		}
		startVid, err := DecodeSBE(rec[start : start+svLen])
		if err != nil {
			return Vertex{}, err
		}
		v := Vertex{
			Type:     Branch,
			Pfx:      pfx,
			StartVid: VertexID(startVid),
			Used:     binary.BigEndian.Uint16(rec[start+svLen : start+svLen+2]),
		}
		if psLen > 0 {
			v.Type = ExtBranch
		}
		if hasKey {
			v.Key = append([]byte(nil), rec[:32]...)
		}
		return v, nil
	}

	if isLeaf && psLen == 0 {
		return Vertex{}, fmt.Errorf("%w: leaf without hex-prefix segment", errVertex)
	}
	data := rec[start:psPos]
	if len(data) == 0 {
		return Vertex{}, fmt.Errorf("%w: empty leaf payload", errVertex)
	}
	mask := data[len(data)-1]
	switch {
	case mask&maskStoLeaf != 0:
		val, err := DecodeSBE256(data[:len(data)-1])
		if err != nil {
			return Vertex{}, err
		}
		return Vertex{Type: StoLeaf, Pfx: pfx, StoValue: val.Bytes32()}, nil
	case mask&0xe0 == 0:
		if len(data) < 3 {
			return Vertex{}, fmt.Errorf("%w: account leaf payload too short", errVertex)
		}
		lens := binary.BigEndian.Uint16(data[len(data)-3 : len(data)-1])
		fields := data[:len(data)-3]
		v := Vertex{Type: AccLeaf, Pfx: pfx, Balance: new(uint256.Int), CodeHash: EmptyCodeHash}
		p := 0
		take := func(n int) ([]byte, error) {
			if p+n > len(fields) {
				return nil, fmt.Errorf("%w: account leaf field overruns payload", errVertex)
			}
			s := fields[p : p+n]
			p += n
			return s, nil
		}
		if mask&maskNonce != 0 {
			b, err := take(int(lens&0b111) + 1)
			if err != nil {
				return Vertex{}, err
			}
			if v.Nonce, err = DecodeSBE(b); err != nil {
				return Vertex{}, err
			}
		}
		if mask&maskBalance != 0 {
			b, err := take(int((lens>>3)&0b11111) + 1)
			if err != nil {
				return Vertex{}, err
			}
			if v.Balance, err = DecodeSBE256(b); err != nil {
				return Vertex{}, err
			}
		}
		if mask&maskStoID != 0 {
			b, err := take(int((lens>>8)&0b111) + 1)
			if err != nil {
				return Vertex{}, err
			}
			sto, err := DecodeSBE(b)
			if err != nil {
				return Vertex{}, err
			}
			v.StoID = VertexID(sto)
		}
		if mask&maskStoHint != 0 {
			b, err := take(1)
			if err != nil {
				return Vertex{}, err
			}
			if b[0] > MaxStoHint {
				return Vertex{}, fmt.Errorf("%w: stoHint %d > %d", errVertex, b[0], MaxStoHint)
			}
			v.StoHint = b[0]
		}
		if mask&maskCodeHash != 0 {
			b, err := take(32)
			if err != nil {
				return Vertex{}, err
			}
			copy(v.CodeHash[:], b)
		}
		if p != len(fields) {
			return Vertex{}, fmt.Errorf("%w: %d trailing bytes in account leaf payload", errVertex, len(fields)-p)
		}
		return v, nil
	default:
		return Vertex{}, fmt.Errorf("%w: unknown leaf mask 0x%02x", errVertex, mask)
	}
}

// Encode re-serialises a decoded vertex; DecodeVertex(v.Encode()) == v.
func (v Vertex) Encode() []byte {
	switch v.Type {
	case Branch, ExtBranch:
		return AppendBranch(nil, v.Key, v.StartVid, v.Used, v.Pfx)
	case AccLeaf:
		payload := AppendAccLeafPayload(nil, v.Nonce, v.Balance, v.StoID, v.StoHint, v.CodeHash)
		return AppendLeafRecord(nil, payload, v.Pfx)
	case StoLeaf:
		return AppendLeafRecord(nil, AppendStoLeafPayload(nil, v.StoValue), v.Pfx)
	}
	panic("nimbus: Encode of unknown vertex type")
}
