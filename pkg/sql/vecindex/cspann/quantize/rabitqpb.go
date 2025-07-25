// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package quantize

import (
	math "math"
	"slices"

	"github.com/cockroachdb/cockroach/pkg/sql/vecindex/cspann/utils"
	"github.com/cockroachdb/cockroach/pkg/sql/vecindex/vecpb"
	"github.com/cockroachdb/cockroach/pkg/util/buildutil"
	"github.com/cockroachdb/cockroach/pkg/util/num32"
	"github.com/cockroachdb/cockroach/pkg/util/vector"
	"github.com/cockroachdb/errors"
)

// RaBitQCode is a quantization code that partially encodes a quantized vector.
// It has 1 bit per dimension of the quantized vector it represents. For
// example, if the quantized vector has 512 dimensions, then its code will have
// 512 bits that are packed into uint64 values using big-endian ordering (i.e.
// a width of 64 bytes). If the dimensions are not evenly divisible by 64, the
// trailing bits of the code are set to zero.
type RaBitQCode []uint64

// RaBitQCodeSetWidth returns the number of uint64values needed to store 1 bit
// per dimension for a RaBitQ code.
func RaBitQCodeSetWidth(dims int) int {
	return (dims + 63) / 64
}

// MakeRaBitQCodeSet returns an empty set of quantization codes, where each code
// in the set represents a quantized vector with the given number of dimensions.
func MakeRaBitQCodeSet(dims int) RaBitQCodeSet {
	return RaBitQCodeSet{
		Count: 0,
		Width: RaBitQCodeSetWidth(dims),
	}
}

// MakeRaBitQCodeSetFromRawData constructs a set of quantization codes from a
// raw slice of codes. The raw codes are packed contiguously in memory and
// represent quantized vectors having the given number of dimensions.
// NB: The data slice is directly used rather than copied; do not use it outside
// the context of this code set after this point.
func MakeRaBitQCodeSetFromRawData(data []uint64, width int) RaBitQCodeSet {
	if len(data)%width != 0 {
		panic(errors.AssertionFailedf(
			"data length %d is not a multiple of the width %d", len(data), width))
	}
	return RaBitQCodeSet{Count: len(data) / width, Width: width, Data: data}
}

// Clone makes a deep copy of the code set. Changes to either the original or
// clone will not affect the other.
func (cs *RaBitQCodeSet) Clone() RaBitQCodeSet {
	return RaBitQCodeSet{
		Count: cs.Count,
		Width: cs.Width,
		Data:  slices.Clone(cs.Data),
	}
}

// Clear resets the code set so that it can be reused.
func (cs *RaBitQCodeSet) Clear() {
	if buildutil.CrdbTestBuild {
		// Write non-zero values to cleared memory.
		for i := range len(cs.Data) {
			cs.Data[i] = 0xBADF00D
		}
	}
	cs.Count = 0
	cs.Data = cs.Data[:0]
}

// At returns the code at the given position in the set as a slice of uint64
// values that can be read or written by the caller.
func (cs *RaBitQCodeSet) At(offset int) RaBitQCode {
	start := offset * cs.Width
	return cs.Data[start : start+cs.Width]
}

// Add appends the given code to this set.
func (cs *RaBitQCodeSet) Add(code RaBitQCode) {
	if len(code) != cs.Width {
		panic(errors.AssertionFailedf(
			"cannot add code with %d width to set with width %d", len(code), cs.Width))
	}
	cs.Data = append(cs.Data, code...)
	cs.Count++
}

// AddUndefined adds the given number of codes to this set. The codes should be
// set to defined values before use.
func (cs *RaBitQCodeSet) AddUndefined(count int) {
	cs.Data = slices.Grow(cs.Data, count*cs.Width)
	cs.Count += count
	cs.Data = cs.Data[:cs.Count*cs.Width]
	if buildutil.CrdbTestBuild {
		for i := len(cs.Data) - count*cs.Width; i < len(cs.Data); i++ {
			cs.Data[i] = 0xBADF00D
		}
	}
}

// ReplaceWithLast removes the code at the given offset from the set, replacing
// it with the last code in the set. The modified set has one less element and
// the last code's position changes.
func (cs *RaBitQCodeSet) ReplaceWithLast(offset int) {
	targetStart := offset * cs.Width
	sourceEnd := len(cs.Data)
	copy(cs.Data[targetStart:targetStart+cs.Width], cs.Data[sourceEnd-cs.Width:sourceEnd])
	cs.Data = cs.Data[:sourceEnd-cs.Width]
	cs.Count--
}

// GetCount implements the QuantizedVectorSet interface.
func (vs *RaBitQuantizedVectorSet) GetCount() int {
	return len(vs.CodeCounts)
}

// ReplaceWithLast implements the QuantizedVectorSet interface.
func (vs *RaBitQuantizedVectorSet) ReplaceWithLast(offset int) {
	vs.Codes.ReplaceWithLast(offset)
	vs.CodeCounts = utils.ReplaceWithLast(vs.CodeCounts, offset)
	vs.CentroidDistances = utils.ReplaceWithLast(vs.CentroidDistances, offset)
	vs.QuantizedDotProducts = utils.ReplaceWithLast(vs.QuantizedDotProducts, offset)
	if vs.CentroidDotProducts != nil {
		// This is nil for the L2Squared distance metric.
		vs.CentroidDotProducts = utils.ReplaceWithLast(vs.CentroidDotProducts, offset)
	}
}

// Clone implements the QuantizedVectorSet interface.
func (vs *RaBitQuantizedVectorSet) Clone() QuantizedVectorSet {
	return &RaBitQuantizedVectorSet{
		Metric:               vs.Metric,
		Centroid:             vs.Centroid, // Centroid is immutable
		Codes:                vs.Codes.Clone(),
		CodeCounts:           slices.Clone(vs.CodeCounts),
		CentroidDistances:    slices.Clone(vs.CentroidDistances),
		QuantizedDotProducts: slices.Clone(vs.QuantizedDotProducts),
		CentroidDotProducts:  slices.Clone(vs.CentroidDotProducts),
		CentroidNorm:         vs.CentroidNorm,
	}
}

// Clear implements the QuantizedVectorSet interface
func (vs *RaBitQuantizedVectorSet) Clear(centroid vector.T) {
	if buildutil.CrdbTestBuild {
		if vs.Centroid == nil {
			panic(errors.New("Clear cannot be called on an uninitialized vector set"))
		}
		vs.scribble(0, len(vs.CodeCounts))
	}

	// Recompute the centroid norm for Cosine and InnerProduct metrics, but only
	// if a new centroid is provided.
	if vs.Metric != vecpb.L2SquaredDistance {
		if &vs.Centroid[0] != &centroid[0] {
			vs.CentroidNorm = num32.Norm(centroid)
		}
	}

	// vs.Centroid is immutable, so do not try to reuse its memory.
	vs.Centroid = centroid
	vs.Codes.Clear()
	vs.CodeCounts = vs.CodeCounts[:0]
	vs.CentroidDistances = vs.CentroidDistances[:0]
	vs.QuantizedDotProducts = vs.QuantizedDotProducts[:0]
	vs.CentroidDotProducts = vs.CentroidDotProducts[:0]
}

// AddUndefined adds the given number of quantized vectors to this set. The new
// quantized vector information should be set to defined values before use.
func (vs *RaBitQuantizedVectorSet) AddUndefined(count int) {
	newCount := len(vs.CodeCounts) + count
	vs.Codes.AddUndefined(count)
	vs.CodeCounts = slices.Grow(vs.CodeCounts, count)
	vs.CodeCounts = vs.CodeCounts[:newCount]
	vs.CentroidDistances = slices.Grow(vs.CentroidDistances, count)
	vs.CentroidDistances = vs.CentroidDistances[:newCount]
	vs.QuantizedDotProducts = slices.Grow(vs.QuantizedDotProducts, count)
	vs.QuantizedDotProducts = vs.QuantizedDotProducts[:newCount]
	if vs.Metric != vecpb.L2SquaredDistance {
		// L2Squared doesn't need this.
		vs.CentroidDotProducts = slices.Grow(vs.CentroidDotProducts, count)
		vs.CentroidDotProducts = vs.CentroidDotProducts[:newCount]
	}
	if buildutil.CrdbTestBuild {
		vs.scribble(newCount-count, newCount)
	}
}

// scribble writes garbage values to undefined vector set values. This is only
// called in test builds to make detecting bugs easier.
func (vs *RaBitQuantizedVectorSet) scribble(start, end int) {
	for i := start; i < end; i++ {
		vs.CodeCounts[i] = 0xBADF00D
	}
	for i := start; i < end; i++ {
		vs.CentroidDistances[i] = math.Pi
	}
	for i := start; i < end; i++ {
		vs.QuantizedDotProducts[i] = math.Pi
	}
	if vs.Metric != vecpb.L2SquaredDistance {
		for i := start; i < end; i++ {
			vs.CentroidDotProducts[i] = math.Pi
		}
	}
	// RaBitQCodeSet Clear and AddUndefined methods take care of scribbling
	// memory for vs.Codes.
}
