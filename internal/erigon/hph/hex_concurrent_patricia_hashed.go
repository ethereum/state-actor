// Vendored from github.com/erigontech/erigon execution/commitment/hex_concurrent_patricia_hashed.go @ 14273f79a6 (production pin).
// Modifications: package commitment -> hph; build tag; R3 strip: Process/ParallelHashSort/CanDoConcurrentNext + Trie-interface methods (Close/SetTrace/SetTraceDomain/EnableWarmupCache/Get+SetCapture/EnableCsvMetrics/SetParticularTrace/Variant/Reset/Release/ResetContext/RootHash/RootTrie); TrieContextFactory relocated to directdrive.go
//
//go:build cgo_erigon_commitment

package hph

import (
	"context"
	"fmt"
	"sync"
)

// if nibble set is -1 then subtrie is not mounted to the nibble, but limited by depth: eg do not fold mounted trie above depth 63
func (hph *HexPatriciaHashed) mountTo(root *HexPatriciaHashed, nibble int) {
	hph.Reset()

	hph.root = root.root
	// hph.rootPresent = !hph.root.IsEmpty()
	// hph.rootPresent = false

	hph.activeRows = root.activeRows
	hph.currentKeyLen = root.currentKeyLen
	copy(hph.currentKey[:], root.currentKey[:])
	copy(hph.depths[:], root.depths[:])
	copy(hph.branchBefore[:], root.branchBefore[:])
	copy(hph.touchMap[:], root.touchMap[:])
	copy(hph.afterMap[:], root.afterMap[:])
	copy(hph.depthsToTxNum[:], root.depthsToTxNum[:])

	hph.mountedNib = nibble
	hph.mounted = true
	for row := 0; row <= hph.activeRows; row++ {
		for nib := 0; nib < len(hph.grid[row]); nib++ {
			hph.grid[row][nib] = root.grid[row][nib]
		}
	}
}

type ConcurrentPatriciaHashed struct {
	root       *HexPatriciaHashed
	rootMu     sync.Mutex
	mounts     [16]*HexPatriciaHashed
	ctxClosers [16]func()
}

// Subtrie inherits root state, address length
func NewConcurrentPatriciaHashed(root *HexPatriciaHashed, ctx PatriciaContext) *ConcurrentPatriciaHashed {
	p := &ConcurrentPatriciaHashed{root: root}

	for i := range p.mounts {
		p.mounts[i] = p.root.SpawnSubTrie(ctx, i)
	}
	return p
}

func (p *ConcurrentPatriciaHashed) foldNibble(ctx context.Context, nib int) error {
	c, err := p.mounts[nib].foldMounted(ctx, nib)
	if err != nil {
		return err
	}

	p.rootMu.Lock()
	defer p.rootMu.Unlock()

	// fmt.Printf("mounted %02x => %s\n", prevByte, c.String())
	if c.extLen > 0 { // trim first byte (2 nibbles) from extension, if any, since it's also a nibble in that row
		c.extLen--
		copy(c.extension[:], c.extension[1:])
		c.hashedExtLen -= 2
		copy(c.hashedExtension[:], c.hashedExtension[2:])
	}

	// propagate changes to top row
	p.root.touchMap[0] |= uint16(1) << nib
	if !c.IsEmpty() {
		p.root.afterMap[0] |= uint16(1) << nib
	} else {
		p.root.afterMap[0] &^= uint16(1) << nib
	}
	p.root.depths[0] = 1
	p.root.grid[0][nib] = c

	subtrie := p.mounts[nib]
	subtrie.Reset()

	// clean up subtrie
	subtrie.currentKeyLen = 0
	subtrie.activeRows = 0
	for ri := 0; ri < len(p.mounts[nib].grid); ri++ {
		subtrie.currentKey[ri] = 0
		subtrie.depths[ri] = 0
		subtrie.touchMap[ri] = 0
		subtrie.afterMap[ri] = 0
		subtrie.depthsToTxNum[ri] = 0
		subtrie.branchBefore[ri] = false

		for ci := 0; ci < len(subtrie.grid[ri]); ci++ {
			subtrie.grid[ri][ci].reset()
		}
	}

	return nil
}

func (p *ConcurrentPatriciaHashed) unfoldRoot(ctx context.Context, ctxFactory TrieContextFactory) error {
	if p.root.trace {
		fmt.Printf("=============ROOT unfold============\n")
	}
	// if p.root.rootPresent && p.root.root.hashedExtLen == 0 { // if root has no extension, we have to unfold
	zero := []byte{0}
	for unfolding := p.root.needUnfolding(zero); unfolding > 0; unfolding = p.root.needUnfolding(zero) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.root.unfold(zero, unfolding); err != nil {
			return fmt.Errorf("unfold: %w", err)
		}
	}
	// }

	if p.root.trace {
		fmt.Printf("=========END=ROOT unfold============\n")
	}

	for i := range p.mounts {
		if p.mounts[i] == nil {
			panic(fmt.Sprintf("nibble %x is nil", i))
		}
		p.mounts[i].mountTo(p.root, i)
		mountCtx, mountCtxClose := ctxFactory()
		p.mounts[i].ctx = mountCtx
		p.ctxClosers[i] = mountCtxClose
	}
	return nil
}
