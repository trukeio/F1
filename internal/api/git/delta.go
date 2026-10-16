package git

// Delta encoding for fetch packs (doc/plan.md, "Pack generation"; ADR-0012).
// The format is Git's: the base and result sizes as varints, then copy
// instructions (a range of the base) and insert instructions (up to 127
// literal bytes).

const (
	deltaBlock   = 16       // matched block size; also the minimum copy
	maxInsert    = 127      // bytes per insert instruction
	maxCopy      = 0xffffff // bytes per copy instruction (24-bit size)
	maxBucket    = 64       // base positions kept per block hash
	deltaMaxSize = 1 << 20  // objects larger than this are never deltified
)

// makeDelta returns a delta that turns base into target, or nil if it would
// not be smaller than half the target (the object is then sent whole).
func makeDelta(base, target []byte) []byte {
	if len(base) < deltaBlock || len(target) < deltaBlock {
		return nil
	}
	// Index the base at block boundaries.
	index := make(map[uint64][]int32, len(base)/deltaBlock)
	for i := 0; i+deltaBlock <= len(base); i += deltaBlock {
		h := blockHash(base[i : i+deltaBlock])
		if b := index[h]; len(b) < maxBucket {
			index[h] = append(b, int32(i))
		}
	}
	limit := len(target) / 2
	out := make([]byte, 0, limit+32)
	out = appendVarint(out, uint64(len(base)))
	out = appendVarint(out, uint64(len(target)))
	lit := 0 // start of pending literal bytes in target
	for i := 0; i+deltaBlock <= len(target); {
		bestOff, bestLen := 0, 0
		for _, off := range index[blockHash(target[i:i+deltaBlock])] {
			o := int(off)
			n := 0
			for o+n < len(base) && i+n < len(target) && base[o+n] == target[i+n] {
				n++
			}
			if n > bestLen {
				bestOff, bestLen = o, n
			}
		}
		if bestLen < deltaBlock {
			i++
			continue
		}
		// Extend the match backwards into the pending literals.
		for bestOff > 0 && i > lit && base[bestOff-1] == target[i-1] {
			bestOff--
			i--
			bestLen++
		}
		out = appendInserts(out, target[lit:i])
		for n := bestLen; n > 0; {
			k := min(n, maxCopy)
			out = appendCopy(out, bestOff, k)
			bestOff += k
			n -= k
		}
		i += bestLen
		lit = i
		if len(out) > limit {
			return nil
		}
	}
	out = appendInserts(out, target[lit:])
	if len(out) > limit {
		return nil
	}
	return out
}

func blockHash(b []byte) uint64 {
	var h uint64 = 14695981039346656037
	for _, c := range b {
		h ^= uint64(c)
		h *= 1099511628211
	}
	return h
}

func appendVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func appendInserts(b, lit []byte) []byte {
	for len(lit) > 0 {
		n := min(len(lit), maxInsert)
		b = append(b, byte(n))
		b = append(b, lit[:n]...)
		lit = lit[n:]
	}
	return b
}

func appendCopy(b []byte, off, n int) []byte {
	op := len(b)
	b = append(b, 0x80)
	for i := 0; i < 4; i++ {
		if c := byte(off >> (8 * i)); c != 0 {
			b[op] |= 1 << i
			b = append(b, c)
		}
	}
	if n == 0x10000 {
		n = 0 // the encoding of 0x10000
	}
	for i := 0; i < 3; i++ {
		if c := byte(n >> (8 * i)); c != 0 {
			b[op] |= 0x10 << i
			b = append(b, c)
		}
	}
	return b
}
