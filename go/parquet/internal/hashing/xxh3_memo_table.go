// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package hashing provides utilities for and an implementation of a hash
// table which is more performant than the default go map implementation
// by leveraging xxh3 and some custom hash functions.
package hashing

import (
	"bytes"
	"math"
	"math/bits"
	"reflect"
	"unsafe"

	"github.com/apache/arrow/go/arrow"
	"github.com/apache/arrow/go/arrow/array"
	"github.com/apache/arrow/go/arrow/memory"
	"github.com/apache/arrow/go/parquet"

	"github.com/zeebo/xxh3"
)

//go:generate go run ../../../arrow/_tools/tmpl/main.go -i -data=types.tmpldata xxh3_memo_table.gen.go.tmpl

func hashInt(val uint64, alg uint64) uint64 {
	// Two of xxhash's prime multipliers (which are chosen for their
	// bit dispersion properties)
	var multipliers = [2]uint64{11400714785074694791, 14029467366897019727}
	// Multiplying by the prime number mixes the low bits into the high bits,
	// then byte-swapping (which is a single CPU instruction) allows the
	// combined high and low bits to participate in the initial hash table index.
	return bits.ReverseBytes64(multipliers[alg] * val)
}

func hashFloat32(val float32, alg uint64) uint64 {
	// grab the raw byte pattern of the
	bt := *(*[4]byte)(unsafe.Pointer(&val))
	x := uint64(*(*uint32)(unsafe.Pointer(&bt[0])))
	hx := hashInt(x, alg)
	hy := hashInt(x, alg^1)
	return 4 ^ hx ^ hy
}

func hashFloat64(val float64, alg uint64) uint64 {
	bt := *(*[8]byte)(unsafe.Pointer(&val))
	hx := hashInt(uint64(*(*uint32)(unsafe.Pointer(&bt[4]))), alg)
	hy := hashInt(uint64(*(*uint32)(unsafe.Pointer(&bt[0]))), alg^1)
	return 8 ^ hx ^ hy
}

func hashString(val string, alg uint64) uint64 {
	buf := *(*[]byte)(unsafe.Pointer(&val))
	(*reflect.SliceHeader)(unsafe.Pointer(&buf)).Cap = len(val)
	return hash(buf, alg)
}

// prime constants used for slightly increasing the hash quality further
var exprimes = [2]uint64{1609587929392839161, 9650029242287828579}

// for smaller amounts of bytes this is faster than even calling into
// xxh3 to do the hash, so we specialize in order to get the benefits
// of that performance.
func hash(b []byte, alg uint64) uint64 {
	n := uint32(len(b))
	if n <= 16 {
		switch {
		case n > 8:
			// 8 < length <= 16
			// apply same principle as above, but as two 64-bit ints
			x := *(*uint64)(unsafe.Pointer(&b[n-8]))
			y := *(*uint64)(unsafe.Pointer(&b[0]))
			hx := hashInt(x, alg)
			hy := hashInt(y, alg^1)
			return uint64(n) ^ hx ^ hy
		case n >= 4:
			// 4 < length <= 8
			// we can read the bytes as two overlapping 32-bit ints, apply different
			// hash functions to each in parallel
			// then xor the results
			x := *(*uint32)(unsafe.Pointer(&b[n-4]))
			y := *(*uint32)(unsafe.Pointer(&b[0]))
			hx := hashInt(uint64(x), alg)
			hy := hashInt(uint64(y), alg^1)
			return uint64(n) ^ hx ^ hy
		case n > 0:
			x := uint32((n << 24) ^ (uint32(b[0]) << 16) ^ (uint32(b[n/2]) << 8) ^ uint32(b[n-1]))
			return hashInt(uint64(x), alg)
		case n == 0:
			return 1
		}
	}

	// increase differentiation enough to improve hash quality
	return xxh3.Hash(b) + exprimes[alg]
}

const (
	sentinel   uint64 = 0
	loadFactor int64  = 2
)

func max(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

var isNan32Cmp = func(v float32) bool { return math.IsNaN(float64(v)) }

// KeyNotFound is the constant returned by memo table functions when a key isn't found in the table
const KeyNotFound = -1

// BinaryMemoTable is our hashtable for binary data using the BinaryBuilder
// to construct the actual data in an easy to pass around way with minimal copies
// while using a hash table to keep track of the indexes into the dictionary that
// is created as we go.
type BinaryMemoTable struct {
	tbl     *Int32HashTable
	builder *array.BinaryBuilder
	nullIdx int
}

// NewBinaryMemoTable returns a hash table for Binary data, the passed in allocator will
// be utilized for the BinaryBuilder, if nil then memory.DefaultAllocator will be used.
// initial and valuesize can be used to pre-allocate the table to reduce allocations. With
// initial being the initial number of entries to allocate for and valuesize being the starting
// amount of space allocated for writing the actual binary data.
func NewBinaryMemoTable(mem memory.Allocator, initial, valuesize int) *BinaryMemoTable {
	if mem == nil {
		mem = memory.DefaultAllocator
	}
	bldr := array.NewBinaryBuilder(mem, arrow.BinaryTypes.Binary)
	bldr.Reserve(int(initial))
	datasize := valuesize
	if datasize <= 0 {
		datasize = initial * 4
	}
	bldr.ReserveData(datasize)
	return &BinaryMemoTable{tbl: NewInt32HashTable(uint64(initial)), builder: bldr, nullIdx: KeyNotFound}
}

// Reset dumps all of the data in the table allowing it to be reutilized.
func (s *BinaryMemoTable) Reset() {
	s.tbl.Reset(32)
	s.builder.NewArray().Release()
	s.builder.Reserve(int(32))
	s.builder.ReserveData(int(32) * 4)
	s.nullIdx = KeyNotFound
}

// GetNull returns the index of a null that has been inserted into the table or
// KeyNotFound. The bool returned will be true if there was a null inserted into
// the table, and false otherwise.
func (s *BinaryMemoTable) GetNull() (int, bool) {
	return int(s.nullIdx), s.nullIdx != KeyNotFound
}

// Size returns the current size of the memo table including the null value
// if one has been inserted.
func (s *BinaryMemoTable) Size() int {
	sz := int(s.tbl.size)
	if _, ok := s.GetNull(); ok {
		sz++
	}
	return sz
}

// helper function to easily return a byte slice for any given value
// regardless of the type if it's a []byte, parquet.ByteArray,
// parquet.FixedLenByteArray or string.
func (BinaryMemoTable) valAsByteSlice(val interface{}) []byte {
	switch v := val.(type) {
	case []byte:
		return v
	case parquet.ByteArray:
		return *(*[]byte)(unsafe.Pointer(&v))
	case parquet.FixedLenByteArray:
		return *(*[]byte)(unsafe.Pointer(&v))
	case string:
		return (*(*[]byte)(unsafe.Pointer(&v)))[:len(v):len(v)]
	default:
		panic("invalid type for binarymemotable")
	}
}

// helper function to get the hash value regardless of the underlying binary type
func (BinaryMemoTable) getHash(val interface{}) uint64 {
	switch v := val.(type) {
	case string:
		return hashString(v, 0)
	case []byte:
		return hash(v, 0)
	case parquet.ByteArray:
		return hash(*(*[]byte)(unsafe.Pointer(&v)), 0)
	case parquet.FixedLenByteArray:
		return hash(*(*[]byte)(unsafe.Pointer(&v)), 0)
	default:
		panic("invalid type for binarymemotable")
	}
}

// helper function to append the given value to the builder regardless
// of the underlying binary type.
func (b *BinaryMemoTable) appendVal(val interface{}) {
	switch v := val.(type) {
	case string:
		b.builder.AppendString(v)
	case []byte:
		b.builder.Append(v)
	case parquet.ByteArray:
		b.builder.Append(*(*[]byte)(unsafe.Pointer(&v)))
	case parquet.FixedLenByteArray:
		b.builder.Append(*(*[]byte)(unsafe.Pointer(&v)))
	}
}

func (b *BinaryMemoTable) lookup(h uint64, val []byte) (*entryInt32, bool) {
	return b.tbl.Lookup(h, func(i int32) bool {
		return bytes.Equal(val, b.builder.Value(int(i)))
	})
}

// Get returns the index of the specified value in the table or KeyNotFound,
// and a boolean indicating whether it was found in the table.
func (b *BinaryMemoTable) Get(val interface{}) (int, bool) {
	if p, ok := b.lookup(b.getHash(val), b.valAsByteSlice(val)); ok {
		return int(p.payload.val), ok
	}
	return KeyNotFound, false
}

// GetOrInsert returns the index of the given value in the table, if not found
// it is inserted into the table. The return value 'found' indicates whether the value
// was found in the table (true) or inserted (false) along with any possible error.
func (b *BinaryMemoTable) GetOrInsert(val interface{}) (idx int, found bool, err error) {
	h := b.getHash(val)
	p, found := b.lookup(h, b.valAsByteSlice(val))
	if found {
		idx = int(p.payload.val)
	} else {
		idx = b.Size()
		b.appendVal(val)
		b.tbl.Insert(p, h, int32(idx), -1)
	}
	return
}

// GetOrInsertNull retrieves the index of a null in the table or inserts
// null into the table, returning the index and a boolean indicating if it was
// found in the table (true) or was inserted (false).
func (b *BinaryMemoTable) GetOrInsertNull() (idx int, found bool) {
	idx, found = b.GetNull()
	if !found {
		idx = b.Size()
		b.nullIdx = idx
		b.builder.AppendNull()
	}
	return
}

// helper function to get the offset into the builder data for a given
// index value.
func (b *BinaryMemoTable) findOffset(idx int) uintptr {
	val := b.builder.Value(idx)
	for len(val) == 0 {
		idx++
		if idx >= b.builder.Len() {
			break
		}
		val = b.builder.Value(idx)
	}
	if len(val) != 0 {
		return uintptr(unsafe.Pointer(&val[0]))
	}
	return uintptr(b.builder.DataLen()) + b.findOffset(0)
}

// CopyOffsets copies the list of offsets into the passed in slice, the offsets
// being the start and end values of the underlying allocated bytes in the builder
// for the individual values of the table. out should be at least sized to Size()+1
func (b *BinaryMemoTable) CopyOffsets(out []int8) {
	b.CopyOffsetsSubset(0, out)
}

// CopyOffsetsSubset is like CopyOffsets but instead of copying all of the offsets,
// it gets a subset of the offsets in the table starting at the index provided by "start".
func (b *BinaryMemoTable) CopyOffsetsSubset(start int, out []int8) {
	if b.builder.Len() <= start {
		return
	}

	first := b.findOffset(0)
	delta := b.findOffset(start)
	for i := start; i < b.Size(); i++ {
		offset := int8(b.findOffset(i) - delta)
		out[i-start] = offset
	}

	out[b.Size()-start] = int8(b.builder.DataLen() - int(delta) - int(first))
}

// CopyValues copies the raw binary data bytes out, out should be a []byte
// with at least ValuesSize bytes allocated to copy into.
func (b *BinaryMemoTable) CopyValues(out interface{}) {
	b.CopyValuesSubset(0, out)
}

// CopyValuesSubset copies the raw binary data bytes out starting with the value
// at the index start, out should be a []byte with at least ValuesSize bytes allocated
func (b *BinaryMemoTable) CopyValuesSubset(start int, out interface{}) {
	var (
		first  = b.findOffset(0)
		offset = b.findOffset(int(start))
		length = b.builder.DataLen() - int(offset-first)
	)

	outval := out.([]byte)
	copy(outval, b.builder.Value(start)[0:length])
}

// CopyFixedWidthValues exists to cope with the fact that the table doesn't keep
// track of the fixed width when inserting the null value the databuffer holds a
// zero length byte slice for the null value (if found)
func (b *BinaryMemoTable) CopyFixedWidthValues(start, width int, out []byte) {
	if start >= b.Size() {
		return
	}

	null, exists := b.GetNull()
	if !exists || null < start {
		// nothing to skip, proceed as usual
		b.CopyValuesSubset(start, out)
		return
	}

	var (
		leftOffset = b.findOffset(start)
		nullOffset = b.findOffset(null)
		leftSize   = nullOffset - leftOffset
	)

	if leftSize > 0 {
		copy(out, b.builder.Value(start)[0:leftSize])
	}

	rightSize := b.ValuesSize() - int(nullOffset)
	if rightSize > 0 {
		// skip the null fixed size value
		copy(out[int(leftSize)+width:], b.builder.Value(int(nullOffset))[0:rightSize])
	}
}

// VisitValues exists to run the visitFn on each value currently in the hash table.
func (b *BinaryMemoTable) VisitValues(start int, visitFn func([]byte)) {
	for i := int(start); i < b.Size(); i++ {
		visitFn(b.builder.Value(i))
	}
}

// Release is used to tell the underlying builder that it can release the memory allocated
// when the reference count reaches 0, this is safe to be called from multiple goroutines
// simultaneously
func (b *BinaryMemoTable) Release() { b.builder.Release() }

// Retain increases the ref count, it is safe to call it from multiple goroutines
// simultaneously.
func (b *BinaryMemoTable) Retain() { b.builder.Retain() }

// ValuesSize returns the current total size of all the raw bytes that have been inserted
// into the memotable so far.
func (b *BinaryMemoTable) ValuesSize() int { return b.builder.DataLen() }
