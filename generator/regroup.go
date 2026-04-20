package generator

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"log"
	"sort"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethdb"
	"github.com/ethereum/go-ethereum/trie/bintrie"
)

// regroupTrieNodes reads all individual internal nodes written by the streaming
// builder and rewrites them in the grouped format.
//
// Old format (per node): [type=2][leftHash(32)][rightHash(32)] at bit-packed path
// New format (per group): [type=2][groupDepth][bitmap][present hashes...] at group boundary
//
// Algorithm:
//  1. Read all trie nodes from DB into an in-memory map: path -> blob
//  2. DFS from root to assign each node its depth (bit-packed paths are
//     ambiguous for depth, so we recover it from the tree structure)
//  3. For each group boundary, traverse groupDepth levels down via DFS,
//     collecting bottom-layer child hashes and building the bitmap
//  4. Serialize in new format, write at group boundary, delete old keys
func regroupTrieNodes(db ethdb.KeyValueStore, groupDepth int, verbose bool) error {
	if groupDepth < 1 || groupDepth > maxGroupDepth {
		return fmt.Errorf("groupDepth must be 1-%d, got %d", maxGroupDepth, groupDepth)
	}

	prefix := verkleTrieNodeKeyPrefix // "vA"

	// Step 1: Read all trie nodes into memory, keyed by bit-packed path.
	type nodeBlob struct {
		blob     []byte
		nodeType byte
	}
	rawNodes := make(map[string]nodeBlob)

	iter := db.NewIterator(prefix, nil)
	for iter.Next() {
		key := iter.Key()
		if !bytes.HasPrefix(key, prefix) {
			break
		}
		pathStr := string(key[len(prefix):])
		blob := make([]byte, len(iter.Value()))
		copy(blob, iter.Value())
		rawNodes[pathStr] = nodeBlob{
			blob:     blob,
			nodeType: blob[0],
		}
	}
	iter.Release()

	if len(rawNodes) == 0 {
		return nil
	}

	// Step 2: DFS from root to assign depths. The root (depth 0) may or
	// may not be in the DB; we probe both children at depth 1 regardless.
	type depthNode struct {
		path     bintrie.BitArray
		depth    int
		pathStr  string
		blob     []byte
		nodeType byte
	}
	var allNodes []depthNode

	type dfsItem struct {
		path  bintrie.BitArray
		depth int
	}

	// Seed the DFS. If the root exists (empty path), start there;
	// otherwise start with both depth-1 children.
	var dfsStack []dfsItem
	if _, ok := rawNodes[""]; ok {
		dfsStack = append(dfsStack, dfsItem{depth: 0})
	} else {
		var left, right bintrie.BitArray
		var root bintrie.BitArray
		left.AppendBit(&root, 0)
		right.AppendBit(&root, 1)
		dfsStack = append(dfsStack, dfsItem{path: right, depth: 1})
		dfsStack = append(dfsStack, dfsItem{path: left, depth: 1})
	}

	for len(dfsStack) > 0 {
		item := dfsStack[len(dfsStack)-1]
		dfsStack = dfsStack[:len(dfsStack)-1]

		pathStr := string(item.path.ActiveBytes())
		n, exists := rawNodes[pathStr]
		if !exists {
			continue
		}

		allNodes = append(allNodes, depthNode{
			path:     item.path,
			depth:    item.depth,
			pathStr:  pathStr,
			blob:     n.blob,
			nodeType: n.nodeType,
		})

		// Recurse into children of ungrouped internal nodes (65 bytes).
		if n.nodeType == nodeTypeInternal && len(n.blob) == 65 {
			var left, right bintrie.BitArray
			left.AppendBit(&item.path, 0)
			right.AppendBit(&item.path, 1)
			dfsStack = append(dfsStack,
				dfsItem{path: right, depth: item.depth + 1})
			dfsStack = append(dfsStack,
				dfsItem{path: left, depth: item.depth + 1})
		}
	}

	if verbose {
		var internalCount, stemCount int
		for _, dn := range allNodes {
			if dn.nodeType == nodeTypeInternal {
				internalCount++
			} else {
				stemCount++
			}
		}
		log.Printf("Regrouping: %d internal + %d stem nodes, groupDepth=%d",
			internalCount, stemCount, groupDepth)
	}

	// Build a lookup by (depth, pathStr) for the DFS within each group.
	type nodeKey struct {
		depth   int
		pathStr string
	}
	nodeIndex := make(map[nodeKey]depthNode, len(allNodes))
	for _, dn := range allNodes {
		nodeIndex[nodeKey{dn.depth, dn.pathStr}] = dn
	}

	batch := db.NewBatch()

	// Step 3: Find boundary internal nodes (depth % groupDepth == 0).
	type boundaryInfo struct {
		path  bintrie.BitArray
		depth int
	}
	var boundaries []boundaryInfo
	for _, dn := range allNodes {
		if dn.nodeType == nodeTypeInternal && dn.depth%groupDepth == 0 {
			boundaries = append(boundaries, boundaryInfo{
				path:  dn.path,
				depth: dn.depth,
			})
		}
	}
	// Process deepest first.
	sort.Slice(boundaries, func(i, j int) bool {
		return boundaries[i].depth > boundaries[j].depth
	})

	// Step 4: For each boundary, DFS groupDepth levels to collect
	// bottom-layer children.
	for _, b := range boundaries {
		bitmapSize := bitmapSizeForDepth(groupDepth)
		bitmap := make([]byte, bitmapSize)

		type groupStackEntry struct {
			path     bintrie.BitArray
			absDepth int
			relDepth int
			position int
		}
		type posHash struct {
			position int
			hash     common.Hash
		}
		var collected []posHash

		gStack := []groupStackEntry{{
			path:     b.path,
			absDepth: b.depth,
			relDepth: 0,
			position: 0,
		}}

		for len(gStack) > 0 {
			top := gStack[len(gStack)-1]
			gStack = gStack[:len(gStack)-1]

			if top.relDepth == groupDepth {
				// Bottom layer of this group.
				ps := string(top.path.ActiveBytes())
				dn, exists := nodeIndex[nodeKey{top.absDepth, ps}]
				if exists {
					h := computeNodeHashFromBlob(dn.blob)
					collected = append(collected, posHash{
						position: top.position, hash: h,
					})
				}
				continue
			}

			ps := string(top.path.ActiveBytes())
			dn, exists := nodeIndex[nodeKey{top.absDepth, ps}]
			if !exists {
				continue
			}

			if dn.nodeType == nodeTypeStem {
				// Stem at intermediate depth. Extend position using
				// the stem's actual key bits to reach the bottom layer.
				remaining := groupDepth - top.relDepth
				stemBytes := dn.blob[1 : 1+stemSize]
				leafPos := top.position
				for d := range remaining {
					bitIdx := top.absDepth + d
					bit := int(stemBytes[bitIdx/8]>>(7-(bitIdx%8))) & 1
					leafPos = leafPos*2 + bit
				}
				h := computeNodeHashFromBlob(dn.blob)
				collected = append(collected, posHash{
					position: leafPos, hash: h,
				})
				continue
			}

			// Internal node — recurse into children.
			var left, right bintrie.BitArray
			left.AppendBit(&top.path, 0)
			right.AppendBit(&top.path, 1)
			gStack = append(gStack,
				groupStackEntry{
					path:     left,
					absDepth: top.absDepth + 1,
					relDepth: top.relDepth + 1,
					position: top.position * 2,
				},
				groupStackEntry{
					path:     right,
					absDepth: top.absDepth + 1,
					relDepth: top.relDepth + 1,
					position: top.position*2 + 1,
				},
			)
		}

		if len(collected) == 0 {
			continue
		}

		sort.Slice(collected, func(i, j int) bool {
			return collected[i].position < collected[j].position
		})

		// Build bitmap and hash list.
		var hashes []common.Hash
		for _, ph := range collected {
			byteIdx := ph.position / 8
			bitIdx := 7 - (ph.position % 8)
			bitmap[byteIdx] |= 1 << uint(bitIdx)
			hashes = append(hashes, ph.hash)
		}

		// Serialize: [type=2][groupDepth][bitmap][hashes...]
		size := 1 + 1 + bitmapSize + len(hashes)*hashSize
		buf := make([]byte, size)
		buf[0] = nodeTypeInternal
		buf[1] = byte(groupDepth)
		copy(buf[2:2+bitmapSize], bitmap)
		offset := 2 + bitmapSize
		for _, h := range hashes {
			copy(buf[offset:offset+hashSize], h[:])
			offset += hashSize
		}

		// Write grouped node at the boundary path.
		pathBytes := b.path.ActiveBytes()
		key := make([]byte, len(prefix)+len(pathBytes))
		copy(key, prefix)
		copy(key[len(prefix):], pathBytes)
		if err := batch.Put(key, buf); err != nil {
			return fmt.Errorf("failed to write regrouped node: %w", err)
		}
	}

	// Step 5: Delete intermediate internal nodes (not at boundaries).
	for _, dn := range allNodes {
		if dn.nodeType != nodeTypeInternal {
			continue
		}
		if dn.depth%groupDepth != 0 {
			key := make([]byte, len(prefix)+len(dn.pathStr))
			copy(key, prefix)
			copy(key[len(prefix):], dn.pathStr)
			if err := batch.Delete(key); err != nil {
				return fmt.Errorf("failed to delete intermediate node: %w", err)
			}
		}
	}

	// Step 6: Move stems within groups to their extended paths.
	// Collect all moves first, then apply deletes before puts to avoid
	// collisions: with bit-packed paths, a stem's extended key at depth
	// D+G can equal another stem's original key at depth D.
	type stemMove struct {
		oldKey []byte
		newKey []byte
		blob   []byte
	}
	var moves []stemMove
	for _, dn := range allNodes {
		if dn.nodeType != nodeTypeStem {
			continue
		}
		groupBoundary := (dn.depth / groupDepth) * groupDepth
		nextBoundary := groupBoundary + groupDepth
		if dn.depth >= nextBoundary || dn.depth == groupBoundary {
			continue
		}
		stemBytes := dn.blob[1 : 1+stemSize]
		extendedPath := makePath(stemBytes, nextBoundary)

		oldKey := make([]byte, len(prefix)+len(dn.pathStr))
		copy(oldKey, prefix)
		copy(oldKey[len(prefix):], dn.pathStr)

		newKey := make([]byte, len(prefix)+len(extendedPath))
		copy(newKey, prefix)
		copy(newKey[len(prefix):], extendedPath)

		moves = append(moves, stemMove{oldKey, newKey, dn.blob})
	}
	for _, m := range moves {
		if err := batch.Delete(m.oldKey); err != nil {
			return fmt.Errorf("failed to delete old stem path: %w", err)
		}
	}
	for _, m := range moves {
		if err := batch.Put(m.newKey, m.blob); err != nil {
			return fmt.Errorf("failed to write extended stem path: %w", err)
		}
	}

	if batch.ValueSize() > 0 {
		if err := batch.Write(); err != nil {
			return fmt.Errorf("failed to write regroup batch: %w", err)
		}
	}

	if verbose {
		log.Printf("Regrouping complete: %d boundaries written",
			len(boundaries))
	}

	return nil
}

// computeNodeHashFromBlob computes the hash of a trie node from its serialized blob.
// For internal nodes (old format): hash = SHA256(leftHash || rightHash)
// For stem nodes: hash = SHA256(stem || 0x00 || treeRoot) using 8-level reduction
func computeNodeHashFromBlob(blob []byte) common.Hash {
	if len(blob) == 0 {
		return common.Hash{}
	}

	switch blob[0] {
	case nodeTypeInternal:
		// Old format: [2][leftHash(32)][rightHash(32)] = 65 bytes
		if len(blob) == 65 {
			return sha256.Sum256(blob[1:65])
		}
		return sha256.Sum256(blob[1:])

	case nodeTypeStem:
		// Stem node: [1][stem(31)][bitmap(32)][values...]
		// Hash = SHA256(stem || 0x00 || treeRoot)
		if len(blob) < 1+stemSize+hashSize {
			return common.Hash{}
		}
		stem := blob[1 : 1+stemSize]
		bitmapBytes := blob[1+stemSize : 1+stemSize+hashSize]

		var data [stemNodeWidth]common.Hash
		var zeroHash common.Hash
		valueOffset := 1 + stemSize + hashSize
		for i := range stemNodeWidth {
			if bitmapBytes[i/8]&(1<<(7-(i%8))) != 0 {
				if valueOffset+hashSize <= len(blob) {
					var val [hashSize]byte
					copy(val[:], blob[valueOffset:valueOffset+hashSize])
					data[i] = sha256.Sum256(val[:])
					valueOffset += hashSize
				}
			}
		}

		// 8-level tree reduction
		var buf [64]byte
		for level := 1; level <= 8; level++ {
			count := stemNodeWidth / (1 << level)
			for i := range count {
				if data[i*2] == zeroHash && data[i*2+1] == zeroHash {
					data[i] = zeroHash
					continue
				}
				copy(buf[:32], data[i*2][:])
				copy(buf[32:], data[i*2+1][:])
				data[i] = sha256.Sum256(buf[:])
			}
		}

		// Final: SHA256(stem || 0x00 || data[0])
		var final [stemSize + 1 + hashSize]byte
		copy(final[:stemSize], stem)
		final[stemSize] = 0x00
		copy(final[stemSize+1:], data[0][:])
		return sha256.Sum256(final[:])

	default:
		return sha256.Sum256(blob)
	}
}
