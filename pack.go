// Packfiles and pkt-lines: the git object wire format.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
)

// ------------------------------------------------------------------ pkt-lines

func pktLine(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	copy(out[4:], payload)
	hex4(out[:4], len(payload)+4)
	return out
}

func hex4(dst []byte, n int) {
	const digits = "0123456789abcdef"
	dst[0] = digits[(n>>12)&0xf]
	dst[1] = digits[(n>>8)&0xf]
	dst[2] = digits[(n>>4)&0xf]
	dst[3] = digits[n&0xf]
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type pktItem struct {
	kind    byte // 'd' data, 'f' flush, 'e' delim
	payload []byte
}

func pktLines(items []pktItem) []byte {
	var buf bytes.Buffer
	for _, it := range items {
		switch it.kind {
		case 'f':
			buf.Write(pktFlush)
		case 'e':
			buf.Write(pktDelim)
		default:
			buf.Write(pktLine(it.payload))
		}
	}
	return buf.Bytes()
}

func pktStr(s string) pktItem      { return pktItem{kind: 'd', payload: []byte(s)} }
func pktFlushItem() pktItem        { return pktItem{kind: 'f'} }
func pktDelimItem() pktItem        { return pktItem{kind: 'e'} }
func pktBytes(b []byte) pktItem    { return pktItem{kind: 'd', payload: b} }

func decodePktLines(buf []byte) ([]pktItem, error) {
	var out []pktItem
	off := 0
	for off < len(buf) {
		if off+4 > len(buf) {
			return nil, errors.New("truncated pkt-line")
		}
		n, err := strconvParseHex4(buf[off : off+4])
		if err != nil {
			return nil, errors.New("bad pkt-line length")
		}
		if n == 0 {
			out = append(out, pktItem{kind: 'f'})
			off += 4
		} else if n == 1 {
			out = append(out, pktItem{kind: 'e'})
			off += 4
		} else {
			if off+n > len(buf) {
				return nil, errors.New("truncated pkt-line payload")
			}
			out = append(out, pktItem{kind: 'd', payload: buf[off+4 : off+n]})
			off += n
		}
	}
	return out, nil
}

func strconvParseHex4(b []byte) (int, error) {
	n := 0
	for _, c := range b {
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n += int(c - '0')
		case c >= 'a' && c <= 'f':
			n += int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n += int(c-'A') + 10
		default:
			return 0, errors.New("bad hex")
		}
	}
	return n, nil
}

// ------------------------------------------------------------------ packfiles

var typeCode = map[objType]int{objCommit: 1, objTree: 2, objBlob: 3, objTag: 4}
var codeType = map[int]objType{1: objCommit, 2: objTree, 3: objBlob, 4: objTag}

func writePackObjectHeader(typ objType, size int) []byte {
	code := typeCode[typ]
	var out []byte
	rem := size
	first := (code << 4) | (rem & 0x0f)
	rem /= 16
	for rem > 0 {
		out = append(out, byte(first|0x80))
		first = rem & 0x7f
		rem /= 128
	}
	out = append(out, byte(first))
	return out
}

type packObj struct {
	typ  objType
	data []byte
	// oid is the object's id when already computed (readPack fills it in);
	// empty when unknown (writePack callers do not need it).
	oid string
}

func writePack(list []packObj) []byte {
	var body bytes.Buffer
	var hdr [12]byte
	copy(hdr[0:4], "PACK")
	binary.BigEndian.PutUint32(hdr[4:8], 2)
	binary.BigEndian.PutUint32(hdr[8:12], uint32(len(list)))
	body.Write(hdr[:])
	for _, o := range list {
		body.Write(writePackObjectHeader(o.typ, len(o.data)))
		var zb bytes.Buffer
		zw := zlib.NewWriter(&zb)
		zw.Write(o.data)
		zw.Close()
		body.Write(zb.Bytes())
	}
	sum := sha1.Sum(body.Bytes())
	out := make([]byte, 0, body.Len()+20)
	out = append(out, body.Bytes()...)
	out = append(out, sum[:]...)
	return out
}

func readVarintSize(buf []byte, start int) (size int, next int, first int) {
	b := buf[start]
	first = int(b)
	size = int(b & 0x0f)
	shift := uint(4)
	off := start + 1
	for b&0x80 != 0 {
		b = buf[off]
		off++
		size += int(b&0x7f) << shift
		shift += 7
	}
	return size, off, first
}

func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() (int, error) {
		v := 0
		shift := uint(0)
		for {
			if off >= len(delta) {
				return 0, errors.New("delta truncated")
			}
			b := delta[off]
			off++
			v += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readSize()
	if err != nil {
		return nil, err
	}
	tgtSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	out := make([]byte, tgtSize)
	written := 0
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			co, cs := 0, 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					co += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					cs += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			if cs == 0 {
				cs = 0x10000
			}
			copy(out[written:written+cs], base[co:co+cs])
			written += cs
		} else if op != 0 {
			lit := int(op & 0x7f)
			copy(out[written:written+lit], delta[off:off+lit])
			off += lit
			written += lit
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if written != tgtSize {
		return nil, errors.New("delta size mismatch")
	}
	return out, nil
}

type rawPackEntry struct {
	offset     int
	typeCode   int
	size       int
	data       []byte
	baseOffset int
	baseOid    string
}

func readPack(pack []byte, s *store) ([]packObj, error) {
	if len(pack) < 32 || string(pack[0:4]) != "PACK" {
		return nil, errors.New("bad pack header")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, errors.New("unsupported pack version")
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	trailerStart := len(pack) - 20
	var raw []*rawPackEntry
	hasDelta := false
	off := 12
	for i := 0; i < count; i++ {
		entryOffset := off
		size, next, first := readVarintSize(pack, off)
		off = next
		tc := (first >> 4) & 0x07
		entry := &rawPackEntry{offset: entryOffset, typeCode: tc, size: size}
		if tc == 6 || tc == 7 {
			hasDelta = true
		}
		if tc == 6 {
			b := pack[off]
			off++
			v := int(b & 0x7f)
			for b&0x80 != 0 {
				b = pack[off]
				off++
				v = ((v + 1) * 128) + int(b&0x7f)
			}
			entry.baseOffset = entryOffset - v
		} else if tc == 7 {
			entry.baseOid = hex.EncodeToString(pack[off : off+20])
			off += 20
		} else if codeType[tc] == "" {
			return nil, fmt.Errorf("unknown pack type %d", tc)
		}
		// Inflate exactly one zlib stream starting at off; bytes.Reader tracks
		// the consumed bytes so off lands on the next entry.
		br := bytes.NewReader(pack[off:trailerStart])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return nil, err
		}
		inflated, err := io.ReadAll(zr)
		zr.Close()
		if err != nil {
			return nil, err
		}
		if len(inflated) != size {
			return nil, errors.New("pack entry size mismatch")
		}
		entry.data = inflated
		off = trailerStart - br.Len()
		raw = append(raw, entry)
	}
	if !hasDelta {
		// Fast path (the bench packs and most pushes): no deltas, so no
		// resolve maps and no second SHA-1. Hash each object once here;
		// handleReceivePack stores it without re-hashing or copying.
		out := make([]packObj, 0, len(raw))
		for _, e := range raw {
			t := codeType[e.typeCode]
			out = append(out, packObj{typ: t, data: e.data, oid: hashObject(t, e.data)})
		}
		return out, nil
	}
	byOffset := make(map[int]*rawPackEntry, len(raw))
	for _, e := range raw {
		byOffset[e.offset] = e
	}
	resolved := make(map[int]packObj)
	byOid := make(map[string]packObj)
	resolving := make(map[int]bool)
	var resolve func(e *rawPackEntry) (packObj, error)
	resolve = func(e *rawPackEntry) (packObj, error) {
		if hit, ok := resolved[e.offset]; ok {
			return hit, nil
		}
		if resolving[e.offset] {
			return packObj{}, errors.New("delta cycle")
		}
		resolving[e.offset] = true
		var out packObj
		var err error
		switch e.typeCode {
		case 6:
			base, err2 := resolve(byOffset[e.baseOffset])
			if err2 != nil {
				err = err2
				break
			}
			var data []byte
			data, err = applyDelta(base.data, e.data)
			if err == nil {
				out = packObj{typ: base.typ, data: data}
			}
		case 7:
			var base packObj
			if b, ok := byOid[e.baseOid]; ok {
				base = b
			} else if o := s.getObject(e.baseOid); o != nil {
				base = packObj{typ: o.typ, data: o.data}
			} else {
				err = errors.New("ref-delta base missing")
				break
			}
			var data []byte
			data, err = applyDelta(base.data, e.data)
			if err == nil {
				out = packObj{typ: base.typ, data: data}
			}
		default:
			out = packObj{typ: codeType[e.typeCode], data: e.data}
		}
		delete(resolving, e.offset)
		if err != nil {
			return packObj{}, err
		}
		out.oid = hashObject(out.typ, out.data)
		resolved[e.offset] = out
		byOid[out.oid] = out
		return out, nil
	}
	out := make([]packObj, 0, len(raw))
	for _, e := range raw {
		o, err := resolve(e)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// ------------------------------------------------------------------ fetch planning

type fetchPlan struct {
	objs       []packObj
	newShallow []string
	unshallow  []string
}

func (s *store) fetchObjects(r *repo, wants, haves []string, depth *int, shallow []string) fetchPlan {
	wantCommits := []string{}
	wantCommitSet := make(map[string]bool)
	type qEnt struct {
		oid string
		d   int
	}
	queue := []qEnt{}
	for _, w := range wants {
		queue = append(queue, qEnt{oid: w, d: 0})
	}
	seenQ := make(map[string]bool)
	for qi := 0; qi < len(queue); qi++ {
		ent := queue[qi]
		if seenQ[ent.oid] {
			continue
		}
		seenQ[ent.oid] = true
		obj := s.objects[ent.oid]
		if obj == nil {
			continue
		}
		if obj.typ == objCommit {
			if depth == nil || ent.d < *depth {
				if !wantCommitSet[ent.oid] {
					wantCommitSet[ent.oid] = true
					wantCommits = append(wantCommits, ent.oid)
				}
				for _, p := range parseCommit(obj.data).parents {
					queue = append(queue, qEnt{oid: p, d: ent.d + 1})
				}
			}
		} else if obj.typ == objTag {
			queue = append(queue, qEnt{oid: parseTag(obj.data).object, d: ent.d})
		}
	}
	var newShallow []string
	if depth != nil {
		for _, c := range wantCommits {
			parents := parseCommit(s.objects[c].data).parents
			beyond := len(parents) == 0
			if !beyond {
				for _, p := range parents {
					if !wantCommitSet[p] {
						beyond = true
						break
					}
				}
			}
			if beyond {
				newShallow = append(newShallow, c)
			}
		}
	}
	wantedSet := make(map[string]bool)
	var wantedOrder []string
	addWanted := func(oid string) {
		if !wantedSet[oid] {
			wantedSet[oid] = true
			wantedOrder = append(wantedOrder, oid)
		}
	}
	for _, c := range wantCommits {
		addWanted(c)
		if obj := s.objects[c]; obj != nil && obj.typ == objCommit {
			closure := make(map[string]bool)
			s.treeObjectClosure(parseCommit(obj.data).tree, closure)
			// treeObjectClosure order is map order; collect deterministically:
			// the final objs list is sorted anyway, so insertion order is moot.
			for oid := range closure {
				addWanted(oid)
			}
		}
	}
	for _, w := range wants {
		obj := s.objects[w]
		if obj == nil {
			continue
		}
		if obj.typ == objTag {
			addWanted(w)
			target := parseTag(obj.data).object
			if tObj := s.objects[target]; tObj != nil {
				if tObj.typ == objCommit {
					addWanted(target)
					closure := make(map[string]bool)
					s.treeObjectClosure(parseCommit(tObj.data).tree, closure)
					for oid := range closure {
						addWanted(oid)
					}
				} else {
					for oid := range s.collectReachable([]string{target}) {
						addWanted(oid)
					}
				}
			}
		} else if obj.typ != objCommit {
			addWanted(w)
		}
	}
	held := make(map[string]bool)
	shallowSet := make(map[string]bool)
	for _, sh := range shallow {
		shallowSet[sh] = true
	}
	for _, h := range haves {
		obj := s.objects[h]
		if obj == nil {
			continue
		}
		if shallowSet[h] && depth != nil {
			held[h] = true
			if obj.typ == objCommit {
				closure := make(map[string]bool)
				s.treeObjectClosure(parseCommit(obj.data).tree, closure)
				for oid := range closure {
					held[oid] = true
				}
			}
		} else {
			for oid := range s.collectReachable([]string{h}) {
				held[oid] = true
			}
		}
	}
	var objs []packObj
	for _, oid := range wantedOrder {
		if held[oid] {
			continue
		}
		if obj := s.objects[oid]; obj != nil {
			objs = append(objs, packObj{typ: obj.typ, data: obj.data})
		}
	}
	rank := func(t objType) int {
		switch t {
		case objCommit:
			return 0
		case objTree:
			return 1
		case objBlob:
			return 2
		default:
			return 3
		}
	}
	sort.Slice(objs, func(i, j int) bool {
		if rank(objs[i].typ) != rank(objs[j].typ) {
			return rank(objs[i].typ) < rank(objs[j].typ)
		}
		return hashObject(objs[i].typ, objs[i].data) < hashObject(objs[j].typ, objs[j].data)
	})
	newShallowSet := make(map[string]bool)
	for _, ns := range newShallow {
		newShallowSet[ns] = true
	}
	var unshallow []string
	for _, sh := range shallow {
		if !newShallowSet[sh] && wantCommitSet[sh] {
			unshallow = append(unshallow, sh)
		}
	}
	if newShallow == nil {
		newShallow = []string{}
	}
	if unshallow == nil {
		unshallow = []string{}
	}
	return fetchPlan{objs: objs, newShallow: newShallow, unshallow: unshallow}
}
