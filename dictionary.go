package parquet

import (
	"io"
	"math/bits"
	"unsafe"

	"github.com/segmentio/parquet-go/deprecated"
	"github.com/segmentio/parquet-go/encoding"
	"github.com/segmentio/parquet-go/encoding/plain"
	"github.com/segmentio/parquet-go/internal/bitpack"
	"github.com/segmentio/parquet-go/internal/unsafecast"
)

const (
	// Completely arbitrary, feel free to adjust if a different value would be
	// more representative of the map implementation in Go.
	mapSizeOverheadPerItem = 8
)

// The Dictionary interface represents type-specific implementations of parquet
// dictionaries.
//
// Programs can instantiate dictionaries by call the NewDictionary method of a
// Type object.
//
// The current implementation has a limitation which prevents applications from
// providing custom versions of this interface because it contains unexported
// methods. The only way to create Dictionary values is to call the
// NewDictionary of Type instances. This limitation may be lifted in future
// releases.
type Dictionary interface {
	// Returns the type that the dictionary was created from.
	Type() Type

	// Returns the number of value indexed in the dictionary.
	Len() int

	// Returns the dictionary value at the given index.
	Index(index int32) Value

	// Inserts values from the second slice to the dictionary and writes the
	// indexes at which each value was inserted to the first slice.
	//
	// The method panics if the length of the indexes slice is smaller than the
	// length of the values slice.
	Insert(indexes []int32, values []Value)

	// Given an array of dictionary indexes, lookup the values into the array
	// of values passed as second argument.
	//
	// The method panics if len(indexes) > len(values), or one of the indexes
	// is negative or greater than the highest index in the dictionary.
	Lookup(indexes []int32, values []Value)

	// Returns the min and max values found in the given indexes.
	Bounds(indexes []int32) (min, max Value)

	// Resets the dictionary to its initial state, removing all values.
	Reset()

	// Returns a BufferedPage representing the content of the dictionary.
	//
	// The returned page shares the underlying memory of the buffer, it remains
	// valid to use until the dictionary's Reset method is called.
	Page() BufferedPage

	// See ColumnBuffer.writeValues for details on the use of unexported methods
	// on interfaces.
	insert(indexes []int32, rows array, size, offset uintptr)
	//lookup(indexes []int32, rows array, size, offset uintptr)
}

func checkLookupIndexBounds(indexes []int32, rows array) {
	if rows.len < len(indexes) {
		panic("dictionary lookup with more indexes than values")
	}
}

// The boolean dictionary always contains two values for true and false.
type booleanDictionary struct {
	booleanPage
	// There are only two possible values for booleans, false and true.
	// Rather than using a Go map, we track the indexes of each values
	// in an array of two 32 bits integers. When inserting values in the
	// dictionary, we ensure that an index exist for each boolean value,
	// then use the value 0 or 1 (false or true) to perform a lookup in
	// the dictionary's map.
	hashmap [2]int32
}

func newBooleanDictionary(typ Type, columnIndex int16, numValues int32, values []byte) *booleanDictionary {
	indexOfFalse, indexOfTrue := int32(-1), int32(-1)

	for i := int32(0); i < numValues && indexOfFalse < 0 && indexOfTrue < 0; i += 8 {
		v := values[i]
		if v != 0x00 {
			indexOfTrue = i + int32(bits.TrailingZeros8(v))
		}
		if v != 0xFF {
			indexOfFalse = i + int32(bits.TrailingZeros8(^v))
		}
	}

	return &booleanDictionary{
		booleanPage: booleanPage{
			typ:         typ,
			bits:        values[:bitpack.ByteCount(uint(numValues))],
			numValues:   numValues,
			columnIndex: ^columnIndex,
		},
		hashmap: [2]int32{
			0: indexOfFalse,
			1: indexOfTrue,
		},
	}
}

func (d *booleanDictionary) Type() Type { return newIndexedType(d.typ, d) }

func (d *booleanDictionary) Len() int { return int(d.numValues) }

func (d *booleanDictionary) Index(i int32) Value { return d.makeValue(d.index(i)) }

func (d *booleanDictionary) index(i int32) bool { return d.valueAt(int(i)) }

func (d *booleanDictionary) Insert(indexes []int32, values []Value) {
	var value Value
	d.insert(indexes, makeArrayValue(values), unsafe.Sizeof(value), unsafe.Offsetof(value.u64))
}

func (d *booleanDictionary) insert(indexes []int32, rows array, size, offset uintptr) {
	_ = indexes[:rows.len]

	if d.hashmap[0] < 0 {
		d.hashmap[0] = d.numValues
		d.numValues++
		d.bits = plain.AppendBoolean(d.bits, int(d.hashmap[0]), false)
	}

	if d.hashmap[1] < 0 {
		d.hashmap[1] = d.numValues
		d.numValues++
		d.bits = plain.AppendBoolean(d.bits, int(d.hashmap[1]), true)
	}

	dict := d.hashmap

	for i := 0; i < rows.len; i++ {
		v := *(*byte)(rows.index(i, size, offset)) & 1
		indexes[i] = dict[v]
	}
}

func (d *booleanDictionary) Lookup(indexes []int32, values []Value) {
	model := d.makeValue(false)
	memsetValues(values, model)
	d.lookup(indexes, makeArrayValue(values), unsafe.Sizeof(model), unsafe.Offsetof(model.u64))
}

func (d *booleanDictionary) lookup(indexes []int32, rows array, size, offset uintptr) {
	checkLookupIndexBounds(indexes, rows)
	for i, j := range indexes {
		*(*bool)(rows.index(i, size, offset)) = d.index(j)
	}
}

func (d *booleanDictionary) Bounds(indexes []int32) (min, max Value) {
	if len(indexes) > 0 {
		hasFalse, hasTrue := false, false

		for _, i := range indexes {
			v := d.index(i)
			if v {
				hasTrue = true
			} else {
				hasFalse = true
			}
			if hasTrue && hasFalse {
				break
			}
		}

		min = d.makeValue(!hasFalse)
		max = d.makeValue(hasTrue)
	}
	return min, max
}

func (d *booleanDictionary) Reset() {
	d.bits = d.bits[:0]
	d.offset = 0
	d.numValues = 0
	d.hashmap = [2]int32{-1, -1}
}

func (d *booleanDictionary) Page() BufferedPage {
	return &d.booleanPage
}

type int32Dictionary struct {
	int32Page
	hashmap map[int32]int32
}

func newInt32Dictionary(typ Type, columnIndex int16, numValues int32, values []byte) *int32Dictionary {
	return &int32Dictionary{
		int32Page: int32Page{
			typ:         typ,
			values:      unsafecast.BytesToInt32(values)[:numValues],
			columnIndex: ^columnIndex,
		},
	}
}

func (d *int32Dictionary) Type() Type { return newIndexedType(d.typ, d) }

func (d *int32Dictionary) Len() int { return len(d.values) }

func (d *int32Dictionary) Index(i int32) Value { return d.makeValue(d.index(i)) }

func (d *int32Dictionary) index(i int32) int32 { return d.values[i] }

func (d *int32Dictionary) Insert(indexes []int32, values []Value) {
	var value Value
	d.insert(indexes, makeArrayValue(values), unsafe.Sizeof(value), unsafe.Offsetof(value.u64))
}

func (d *int32Dictionary) insert(indexes []int32, rows array, size, offset uintptr) {
	_ = indexes[:rows.len]

	if d.hashmap == nil {
		d.hashmap = make(map[int32]int32, cap(d.values))
		for i, v := range d.values {
			d.hashmap[v] = int32(i)
		}
	}

	for i := 0; i < rows.len; i++ {
		value := *(*int32)(rows.index(i, size, offset))

		index, exists := d.hashmap[value]
		if !exists {
			index = int32(len(d.values))
			d.values = append(d.values, value)
			d.hashmap[value] = index
		}

		indexes[i] = index
	}
}

func (d *int32Dictionary) Lookup(indexes []int32, values []Value) {
	model := d.makeValue(0)
	memsetValues(values, model)
	d.lookup(indexes, makeArrayValue(values), unsafe.Sizeof(model), unsafe.Offsetof(model.u64))
}

func (d *int32Dictionary) Bounds(indexes []int32) (min, max Value) {
	if len(indexes) > 0 {
		minValue, maxValue := d.bounds(indexes)
		min = d.makeValue(minValue)
		max = d.makeValue(maxValue)
	}
	return min, max
}

func (d *int32Dictionary) Reset() {
	d.values = d.values[:0]
	d.hashmap = nil
}

func (d *int32Dictionary) Page() BufferedPage {
	return &d.int32Page
}

type int64Dictionary struct {
	int64Page
	hashmap map[int64]int32
}

func newInt64Dictionary(typ Type, columnIndex int16, numValues int32, values []byte) *int64Dictionary {
	return &int64Dictionary{
		int64Page: int64Page{
			typ:         typ,
			values:      unsafecast.BytesToInt64(values)[:numValues],
			columnIndex: ^columnIndex,
		},
	}
}

func (d *int64Dictionary) Type() Type { return newIndexedType(d.typ, d) }

func (d *int64Dictionary) Len() int { return len(d.values) }

func (d *int64Dictionary) Index(i int32) Value { return d.makeValue(d.index(i)) }

func (d *int64Dictionary) index(i int32) int64 { return d.values[i] }

func (d *int64Dictionary) Insert(indexes []int32, values []Value) {
	var value Value
	d.insert(indexes, makeArrayValue(values), unsafe.Sizeof(value), unsafe.Offsetof(value.u64))
}

func (d *int64Dictionary) insert(indexes []int32, rows array, size, offset uintptr) {
	_ = indexes[:rows.len]

	if d.hashmap == nil {
		d.hashmap = make(map[int64]int32, cap(d.values))
		for i, v := range d.values {
			d.hashmap[v] = int32(i)
		}
	}

	for i := 0; i < rows.len; i++ {
		value := *(*int64)(rows.index(i, size, offset))

		index, exists := d.hashmap[value]
		if !exists {
			index = int32(len(d.values))
			d.values = append(d.values, value)
			d.hashmap[value] = index
		}

		indexes[i] = index
	}
}

func (d *int64Dictionary) Lookup(indexes []int32, values []Value) {
	model := d.makeValue(0)
	memsetValues(values, model)
	d.lookup(indexes, makeArrayValue(values), unsafe.Sizeof(model), unsafe.Offsetof(model.u64))
}

func (d *int64Dictionary) Bounds(indexes []int32) (min, max Value) {
	if len(indexes) > 0 {
		minValue, maxValue := d.bounds(indexes)
		min = d.makeValue(minValue)
		max = d.makeValue(maxValue)
	}
	return min, max
}

func (d *int64Dictionary) Reset() {
	d.values = d.values[:0]
	d.hashmap = nil
}

func (d *int64Dictionary) Page() BufferedPage {
	return &d.int64Page
}

type int96Dictionary struct {
	int96Page
	hashmap map[deprecated.Int96]int32
}

func newInt96Dictionary(typ Type, columnIndex int16, numValues int32, values []byte) *int96Dictionary {
	return &int96Dictionary{
		int96Page: int96Page{
			typ:         typ,
			values:      deprecated.BytesToInt96(values)[:numValues],
			columnIndex: ^columnIndex,
		},
	}
}

func (d *int96Dictionary) Type() Type { return newIndexedType(d.typ, d) }

func (d *int96Dictionary) Len() int { return len(d.values) }

func (d *int96Dictionary) Index(i int32) Value { return d.makeValue(d.index(i)) }

func (d *int96Dictionary) index(i int32) deprecated.Int96 { return d.values[i] }

func (d *int96Dictionary) Insert(indexes []int32, values []Value) {
	d.insertValues(indexes, len(values), func(i int) deprecated.Int96 {
		return values[i].Int96()
	})
}

func (d *int96Dictionary) insert(indexes []int32, rows array, size, offset uintptr) {
	d.insertValues(indexes, rows.len, func(i int) deprecated.Int96 {
		return *(*deprecated.Int96)(rows.index(i, size, offset))
	})
}

func (d *int96Dictionary) insertValues(indexes []int32, count int, valueAt func(int) deprecated.Int96) {
	_ = indexes[:count]

	if d.hashmap == nil {
		d.hashmap = make(map[deprecated.Int96]int32, cap(d.values))
		for i, v := range d.values {
			d.hashmap[v] = int32(i)
		}
	}

	for i := 0; i < count; i++ {
		value := valueAt(i)

		index, exists := d.hashmap[value]
		if !exists {
			index = int32(len(d.values))
			d.values = append(d.values, value)
			d.hashmap[value] = index
		}

		indexes[i] = index
	}
}

func (d *int96Dictionary) Lookup(indexes []int32, values []Value) {
	for i, j := range indexes {
		values[i] = d.Index(j)
	}
}

func (d *int96Dictionary) Bounds(indexes []int32) (min, max Value) {
	if len(indexes) > 0 {
		minValue := d.index(indexes[0])
		maxValue := minValue

		for _, i := range indexes[1:] {
			value := d.index(i)
			switch {
			case value.Less(minValue):
				minValue = value
			case maxValue.Less(value):
				maxValue = value
			}
		}

		min = d.makeValue(minValue)
		max = d.makeValue(maxValue)
	}
	return min, max
}

func (d *int96Dictionary) Reset() {
	d.values = d.values[:0]
	d.hashmap = nil
}

func (d *int96Dictionary) Page() BufferedPage {
	return &d.int96Page
}

type floatDictionary struct {
	floatPage
	hashmap map[float32]int32
}

func newFloatDictionary(typ Type, columnIndex int16, numValues int32, values []byte) *floatDictionary {
	return &floatDictionary{
		floatPage: floatPage{
			typ:         typ,
			values:      unsafecast.BytesToFloat32(values)[:numValues],
			columnIndex: ^columnIndex,
		},
	}
}

func (d *floatDictionary) Type() Type { return newIndexedType(d.typ, d) }

func (d *floatDictionary) Len() int { return len(d.values) }

func (d *floatDictionary) Index(i int32) Value { return d.makeValue(d.index(i)) }

func (d *floatDictionary) index(i int32) float32 { return d.values[i] }

func (d *floatDictionary) Insert(indexes []int32, values []Value) {
	var value Value
	d.insert(indexes, makeArrayValue(values), unsafe.Sizeof(value), unsafe.Offsetof(value.u64))
}

func (d *floatDictionary) insert(indexes []int32, rows array, size, offset uintptr) {
	_ = indexes[:rows.len]

	if d.hashmap == nil {
		d.hashmap = make(map[float32]int32, cap(d.values))
		for i, v := range d.values {
			d.hashmap[v] = int32(i)
		}
	}

	for i := 0; i < rows.len; i++ {
		value := *(*float32)(rows.index(i, size, offset))

		index, exists := d.hashmap[value]
		if !exists {
			index = int32(len(d.values))
			d.values = append(d.values, value)
			d.hashmap[value] = index
		}

		indexes[i] = index
	}
}

func (d *floatDictionary) Lookup(indexes []int32, values []Value) {
	model := d.makeValue(0)
	memsetValues(values, model)
	d.lookup(indexes, makeArrayValue(values), unsafe.Sizeof(model), unsafe.Offsetof(model.u64))
}

func (d *floatDictionary) Bounds(indexes []int32) (min, max Value) {
	if len(indexes) > 0 {
		minValue, maxValue := d.bounds(indexes)
		min = d.makeValue(minValue)
		max = d.makeValue(maxValue)
	}
	return min, max
}

func (d *floatDictionary) Reset() {
	d.values = d.values[:0]
	d.hashmap = nil
}

func (d *floatDictionary) Page() BufferedPage {
	return &d.floatPage
}

type doubleDictionary struct {
	doublePage
	hashmap map[float64]int32
}

func newDoubleDictionary(typ Type, columnIndex int16, numValues int32, values []byte) *doubleDictionary {
	return &doubleDictionary{
		doublePage: doublePage{
			typ:         typ,
			values:      unsafecast.BytesToFloat64(values)[:numValues],
			columnIndex: ^columnIndex,
		},
	}
}

func (d *doubleDictionary) Type() Type { return newIndexedType(d.typ, d) }

func (d *doubleDictionary) Len() int { return len(d.values) }

func (d *doubleDictionary) Index(i int32) Value { return d.makeValue(d.index(i)) }

func (d *doubleDictionary) index(i int32) float64 { return d.values[i] }

func (d *doubleDictionary) Insert(indexes []int32, values []Value) {
	var value Value
	d.insert(indexes, makeArrayValue(values), unsafe.Sizeof(value), unsafe.Offsetof(value.u64))
}

func (d *doubleDictionary) insert(indexes []int32, rows array, size, offset uintptr) {
	_ = indexes[:rows.len]

	if d.hashmap == nil {
		d.hashmap = make(map[float64]int32, cap(d.values))
		for i, v := range d.values {
			d.hashmap[v] = int32(i)
		}
	}

	for i := 0; i < rows.len; i++ {
		value := *(*float64)(rows.index(i, size, offset))

		index, exists := d.hashmap[value]
		if !exists {
			index = int32(len(d.values))
			d.values = append(d.values, value)
			d.hashmap[value] = index
		}

		indexes[i] = index
	}
}

func (d *doubleDictionary) Lookup(indexes []int32, values []Value) {
	model := d.makeValue(0)
	memsetValues(values, model)
	d.lookup(indexes, makeArrayValue(values), unsafe.Sizeof(model), unsafe.Offsetof(model.u64))
}

func (d *doubleDictionary) Bounds(indexes []int32) (min, max Value) {
	if len(indexes) > 0 {
		minValue, maxValue := d.bounds(indexes)
		min = d.makeValue(minValue)
		max = d.makeValue(maxValue)
	}
	return min, max
}

func (d *doubleDictionary) Reset() {
	d.values = d.values[:0]
	d.hashmap = nil
}

func (d *doubleDictionary) Page() BufferedPage {
	return &d.doublePage
}

type byteArrayDictionary struct {
	byteArrayPage
	offsets []uint32
	hashmap map[string]int32
}

func newByteArrayDictionary(typ Type, columnIndex int16, numValues int32, values []byte) *byteArrayDictionary {
	d := &byteArrayDictionary{
		offsets: make([]uint32, 0, numValues),
		byteArrayPage: byteArrayPage{
			typ:         typ,
			values:      values,
			numValues:   numValues,
			columnIndex: ^columnIndex,
		},
	}

	for i := 0; i < len(values); {
		n := plain.ByteArrayLength(values[i:])
		d.offsets = append(d.offsets, uint32(i))
		i += plain.ByteArrayLengthSize
		i += n
	}

	return d
}

func (d *byteArrayDictionary) Type() Type { return newIndexedType(d.typ, d) }

func (d *byteArrayDictionary) Len() int { return len(d.offsets) }

func (d *byteArrayDictionary) Index(i int32) Value { return d.makeValueBytes(d.index(i)) }

func (d *byteArrayDictionary) index(i int32) []byte { return d.valueAt(d.offsets[i]) }

func (d *byteArrayDictionary) Insert(indexes []int32, values []Value) {
	var value Value
	d.insert(indexes, makeArrayValue(values), unsafe.Sizeof(value), unsafe.Offsetof(value.ptr))
}

func (d *byteArrayDictionary) insert(indexes []int32, rows array, size, offset uintptr) {
	_ = indexes[:rows.len]

	if d.hashmap == nil {
		d.hashmap = make(map[string]int32, cap(d.offsets))
		for index, offset := range d.offsets {
			d.hashmap[string(d.valueAt(offset))] = int32(index)
		}
	}

	for i := 0; i < rows.len; i++ {
		value := *(*string)(rows.index(i, size, offset))

		index, exists := d.hashmap[value]
		if !exists {
			index = int32(len(d.offsets))
			value = d.append(value)
			d.hashmap[value] = index
		}

		indexes[i] = index
	}
}

func (d *byteArrayDictionary) append(value string) string {
	offset := len(d.values)
	d.values = plain.AppendByteArrayString(d.values, value)
	d.offsets = append(d.offsets, uint32(offset))
	d.numValues++
	return string(d.values[offset+plain.ByteArrayLengthSize : len(d.values)])
}

func (d *byteArrayDictionary) Lookup(indexes []int32, values []Value) {
	model := d.makeValueString("")
	memsetValues(values, model)
	d.lookupString(indexes, makeArrayValue(values), unsafe.Sizeof(model), unsafe.Offsetof(model.ptr))
}

func (d *byteArrayDictionary) Bounds(indexes []int32) (min, max Value) {
	if len(indexes) > 0 {
		base := d.index(indexes[0])
		minValue := unsafecast.BytesToString(base)
		maxValue := minValue
		values := [64]string{}

		for i := 1; i < len(indexes); i += len(values) {
			n := len(indexes) - i
			if n > len(values) {
				n = len(values)
			}
			j := i + n
			d.lookupString(indexes[i:j:j], makeArrayString(values[:n:n]), unsafe.Sizeof(values[0]), 0)

			for _, value := range values[:n:n] {
				switch {
				case value < minValue:
					minValue = value
				case value > maxValue:
					maxValue = value
				}
			}
		}

		min = d.makeValueString(minValue)
		max = d.makeValueString(maxValue)
	}
	return min, max
}

func (d *byteArrayDictionary) Reset() {
	d.offsets = d.offsets[:0]
	d.values = d.values[:0]
	d.numValues = 0
	d.hashmap = nil
}

func (d *byteArrayDictionary) Page() BufferedPage {
	return &d.byteArrayPage
}

type fixedLenByteArrayDictionary struct {
	fixedLenByteArrayPage
	hashmap map[string]int32
}

func newFixedLenByteArrayDictionary(typ Type, columnIndex int16, numValues int32, data []byte) *fixedLenByteArrayDictionary {
	size := typ.Length()
	return &fixedLenByteArrayDictionary{
		fixedLenByteArrayPage: fixedLenByteArrayPage{
			typ:         typ,
			size:        size,
			data:        data,
			columnIndex: ^columnIndex,
		},
	}
}

func (d *fixedLenByteArrayDictionary) Type() Type { return newIndexedType(d.typ, d) }

func (d *fixedLenByteArrayDictionary) Len() int { return len(d.data) / d.size }

func (d *fixedLenByteArrayDictionary) Index(i int32) Value {
	return d.makeValueBytes(d.index(i))
}

func (d *fixedLenByteArrayDictionary) index(i int32) []byte {
	j := (int(i) + 0) * d.size
	k := (int(i) + 1) * d.size
	return d.data[j:k:k]
}

func (d *fixedLenByteArrayDictionary) Insert(indexes []int32, values []Value) {
	d.insertValues(indexes, len(values), func(i int) *byte {
		return values[i].ptr
	})
}

func (d *fixedLenByteArrayDictionary) insert(indexes []int32, rows array, size, offset uintptr) {
	d.insertValues(indexes, rows.len, func(i int) *byte {
		return (*byte)(rows.index(i, size, offset))
	})
}

func (d *fixedLenByteArrayDictionary) insertValues(indexes []int32, count int, valueAt func(int) *byte) {
	_ = indexes[:count]

	if d.hashmap == nil {
		d.hashmap = make(map[string]int32, cap(d.data)/d.size)
		for i, j := 0, int32(0); i < len(d.data); i += d.size {
			d.hashmap[string(d.data[i:i+d.size])] = j
			j++
		}
	}

	for i := 0; i < count; i++ {
		value := unsafe.Slice(valueAt(i), d.size)

		index, exists := d.hashmap[string(value)]
		if !exists {
			index = int32(d.Len())
			start := len(d.data)
			d.data = append(d.data, value...)
			d.hashmap[string(d.data[start:])] = index
		}

		indexes[i] = index
	}
}

func (d *fixedLenByteArrayDictionary) Lookup(indexes []int32, values []Value) {
	model := d.makeValueString("")
	memsetValues(values, model)
	d.lookupString(indexes, makeArrayValue(values), unsafe.Sizeof(model), unsafe.Offsetof(model.ptr))
}

func (d *fixedLenByteArrayDictionary) Bounds(indexes []int32) (min, max Value) {
	if len(indexes) > 0 {
		base := d.index(indexes[0])
		minValue := unsafecast.BytesToString(base)
		maxValue := minValue
		values := [64]string{}

		for i := 1; i < len(indexes); i += len(values) {
			n := len(indexes) - i
			if n > len(values) {
				n = len(values)
			}
			j := i + n
			d.lookupString(indexes[i:j:j], makeArrayString(values[:n:n]), unsafe.Sizeof(values[0]), 0)

			for _, value := range values[:n:n] {
				switch {
				case value < minValue:
					minValue = value
				case value > maxValue:
					maxValue = value
				}
			}
		}

		min = d.makeValueString(minValue)
		max = d.makeValueString(maxValue)
	}
	return min, max
}

func (d *fixedLenByteArrayDictionary) Reset() {
	d.data = d.data[:0]
	d.hashmap = nil
}

func (d *fixedLenByteArrayDictionary) Page() BufferedPage {
	return &d.fixedLenByteArrayPage
}

type uint32Dictionary struct {
	uint32Page
	hashmap map[uint32]int32
}

func newUint32Dictionary(typ Type, columnIndex int16, numValues int32, data []byte) *uint32Dictionary {
	return &uint32Dictionary{
		uint32Page: uint32Page{
			typ:         typ,
			values:      unsafecast.BytesToUint32(data)[:numValues],
			columnIndex: ^columnIndex,
		},
	}
}

func (d *uint32Dictionary) Type() Type { return newIndexedType(d.typ, d) }

func (d *uint32Dictionary) Len() int { return len(d.values) }

func (d *uint32Dictionary) Index(i int32) Value { return d.makeValue(d.index(i)) }

func (d *uint32Dictionary) index(i int32) uint32 { return d.values[i] }

func (d *uint32Dictionary) Insert(indexes []int32, values []Value) {
	var value Value
	d.insert(indexes, makeArrayValue(values), unsafe.Sizeof(value), unsafe.Offsetof(value.u64))
}

func (d *uint32Dictionary) insert(indexes []int32, rows array, size, offset uintptr) {
	_ = indexes[:rows.len]

	if d.hashmap == nil {
		d.hashmap = make(map[uint32]int32, cap(d.values))
		for i, v := range d.values {
			d.hashmap[v] = int32(i)
		}
	}

	for i := 0; i < rows.len; i++ {
		value := *(*uint32)(rows.index(i, size, offset))

		index, exists := d.hashmap[value]
		if !exists {
			index = int32(len(d.values))
			d.values = append(d.values, value)
			d.hashmap[value] = index
		}

		indexes[i] = index
	}
}

func (d *uint32Dictionary) Lookup(indexes []int32, values []Value) {
	model := d.makeValue(0)
	memsetValues(values, model)
	d.lookup(indexes, makeArrayValue(values), unsafe.Sizeof(model), unsafe.Offsetof(model.u64))
}

func (d *uint32Dictionary) Bounds(indexes []int32) (min, max Value) {
	if len(indexes) > 0 {
		minValue, maxValue := d.bounds(indexes)
		min = d.makeValue(minValue)
		max = d.makeValue(maxValue)
	}
	return min, max
}

func (d *uint32Dictionary) Reset() {
	d.values = d.values[:0]
	d.hashmap = nil
}

func (d *uint32Dictionary) Page() BufferedPage {
	return &d.uint32Page
}

type uint64Dictionary struct {
	uint64Page
	hashmap map[uint64]int32
}

func newUint64Dictionary(typ Type, columnIndex int16, numValues int32, data []byte) *uint64Dictionary {
	return &uint64Dictionary{
		uint64Page: uint64Page{
			typ:         typ,
			values:      unsafecast.BytesToUint64(data),
			columnIndex: ^columnIndex,
		},
	}
}

func (d *uint64Dictionary) Type() Type { return newIndexedType(d.typ, d) }

func (d *uint64Dictionary) Len() int { return len(d.values) }

func (d *uint64Dictionary) Index(i int32) Value { return d.makeValue(d.index(i)) }

func (d *uint64Dictionary) index(i int32) uint64 { return d.values[i] }

func (d *uint64Dictionary) Insert(indexes []int32, values []Value) {
	var value Value
	d.insert(indexes, makeArrayValue(values), unsafe.Sizeof(value), unsafe.Offsetof(value.u64))
}

func (d *uint64Dictionary) insert(indexes []int32, rows array, size, offset uintptr) {
	_ = indexes[:rows.len]

	if d.hashmap == nil {
		d.hashmap = make(map[uint64]int32, cap(d.values))
		for i, v := range d.values {
			d.hashmap[v] = int32(i)
		}
	}

	for i := 0; i < rows.len; i++ {
		value := *(*uint64)(rows.index(i, size, offset))

		index, exists := d.hashmap[value]
		if !exists {
			index = int32(len(d.values))
			d.values = append(d.values, value)
			d.hashmap[value] = index
		}

		indexes[i] = index
	}
}

func (d *uint64Dictionary) Lookup(indexes []int32, values []Value) {
	model := d.makeValue(0)
	memsetValues(values, model)
	d.lookup(indexes, makeArrayValue(values), unsafe.Sizeof(model), unsafe.Offsetof(model.u64))
}

func (d *uint64Dictionary) Bounds(indexes []int32) (min, max Value) {
	if len(indexes) > 0 {
		minValue, maxValue := d.bounds(indexes)
		min = d.makeValue(minValue)
		max = d.makeValue(maxValue)
	}
	return min, max
}

func (d *uint64Dictionary) Reset() {
	d.values = d.values[:0]
	d.hashmap = nil
}

func (d *uint64Dictionary) Page() BufferedPage {
	return &d.uint64Page
}

type be128Dictionary struct {
	be128Page
	hashmap map[[16]byte]int32
}

func newBE128Dictionary(typ Type, columnIndex int16, numValues int32, data []byte) *be128Dictionary {
	return &be128Dictionary{
		be128Page: be128Page{
			typ:         typ,
			values:      unsafecast.BytesToUint128(data),
			columnIndex: ^columnIndex,
		},
	}
}

func (d *be128Dictionary) Type() Type { return newIndexedType(d.typ, d) }

func (d *be128Dictionary) Len() int { return len(d.values) }

func (d *be128Dictionary) Index(i int32) Value { return d.makeValue(d.index(i)) }

func (d *be128Dictionary) index(i int32) *[16]byte { return &d.values[i] }

func (d *be128Dictionary) Insert(indexes []int32, values []Value) {
	d.insertValues(indexes, len(values), func(i int) [16]byte {
		return *(*[16]byte)(values[i].ByteArray())
	})
}

func (d *be128Dictionary) insert(indexes []int32, rows array, size, offset uintptr) {
	d.insertValues(indexes, rows.len, func(i int) [16]byte {
		return *(*[16]byte)(rows.index(i, size, offset))
	})
}

func (d *be128Dictionary) insertValues(indexes []int32, count int, valueAt func(int) [16]byte) {
	_ = indexes[:count]

	if d.hashmap == nil {
		d.hashmap = make(map[[16]byte]int32, cap(d.values))
		for i, v := range d.values {
			d.hashmap[v] = int32(i)
		}
	}

	for i := 0; i < count; i++ {
		value := valueAt(i)

		index, exists := d.hashmap[value]
		if !exists {
			index = int32(len(d.values))
			d.values = append(d.values, value)
			d.hashmap[value] = index
		}

		indexes[i] = index
	}
}

func (d *be128Dictionary) Lookup(indexes []int32, values []Value) {
	model := d.makeValueString("")
	memsetValues(values, model)
	d.lookupString(indexes, makeArrayValue(values), unsafe.Sizeof(model), unsafe.Offsetof(model.ptr))
}

func (d *be128Dictionary) Bounds(indexes []int32) (min, max Value) {
	if len(indexes) > 0 {
		minValue, maxValue := d.bounds(indexes)
		min = d.makeValue(minValue)
		max = d.makeValue(maxValue)
	}
	return min, max
}

func (d *be128Dictionary) Reset() {
	d.values = d.values[:0]
	d.hashmap = nil
}

func (d *be128Dictionary) Page() BufferedPage {
	return &d.be128Page
}

// indexedType is a wrapper around a Type value which overrides object
// constructors to use indexed versions referencing values in the dictionary
// instead of storing plain values.
type indexedType struct {
	Type
	dict Dictionary
}

func newIndexedType(typ Type, dict Dictionary) *indexedType {
	return &indexedType{Type: typ, dict: dict}
}

func (t *indexedType) NewColumnBuffer(columnIndex, numValues int) ColumnBuffer {
	return newIndexedColumnBuffer(t, makeColumnIndex(columnIndex), makeNumValues(numValues))
}

func (t *indexedType) NewPage(columnIndex, numValues int, data []byte) Page {
	return newIndexedPage(t, makeColumnIndex(columnIndex), makeNumValues(numValues), data)
}

// indexedPage is an implementation of the BufferedPage interface which stores
// indexes instead of plain value. The indexes reference the values in a
// dictionary that the page was created for.
type indexedPage struct {
	typ         *indexedType
	values      []int32
	columnIndex int16
}

func newIndexedPage(typ *indexedType, columnIndex int16, numValues int32, values []byte) *indexedPage {
	// RLE encoded values that contain dictionary indexes in data pages are
	// sometimes truncated when they contain only zeros. We account for this
	// special case here and extend the values buffer if it is shorter than
	// needed to hold `numValues`.
	size := 4 * int(numValues)

	if len(values) < size {
		if cap(values) < size {
			tmp := make([]byte, size)
			copy(tmp, values)
			values = tmp
		} else {
			clear := values[len(values) : len(values)+size]
			for i := range clear {
				clear[i] = 0
			}
		}
	}

	return &indexedPage{
		typ:         typ,
		values:      unsafecast.BytesToInt32(values[:size]),
		columnIndex: ^columnIndex,
	}
}

func (page *indexedPage) Type() Type { return indexedPageType{page.typ} }

func (page *indexedPage) Column() int { return int(^page.columnIndex) }

func (page *indexedPage) Dictionary() Dictionary { return page.typ.dict }

func (page *indexedPage) NumRows() int64 { return int64(len(page.values)) }

func (page *indexedPage) NumValues() int64 { return int64(len(page.values)) }

func (page *indexedPage) NumNulls() int64 { return 0 }

func (page *indexedPage) Size() int64 { return 4 * int64(len(page.values)) }

func (page *indexedPage) RepetitionLevels() []byte { return nil }

func (page *indexedPage) DefinitionLevels() []byte { return nil }

func (page *indexedPage) Data() []byte { return unsafecast.Int32ToBytes(page.values) }

func (page *indexedPage) Values() ValueReader { return &indexedPageValues{page: page} }

func (page *indexedPage) Buffer() BufferedPage { return page }

func (page *indexedPage) Bounds() (min, max Value, ok bool) {
	if ok = len(page.values) > 0; ok {
		min, max = page.typ.dict.Bounds(page.values)
		min.columnIndex = page.columnIndex
		max.columnIndex = page.columnIndex
	}
	return min, max, ok
}

func (page *indexedPage) Clone() BufferedPage {
	return &indexedPage{
		typ:         page.typ,
		values:      append([]int32{}, page.values...),
		columnIndex: page.columnIndex,
	}
}

func (page *indexedPage) Slice(i, j int64) BufferedPage {
	return &indexedPage{
		typ:         page.typ,
		values:      page.values[i:j],
		columnIndex: page.columnIndex,
	}
}

// indexedPageType is an adapter for the indexedType returned when accessing
// the type of an indexedPage value. It overrides the Encode/Decode methods to
// account for the fact that an indexed page is holding indexes of values into
// its dictionary instead of plain values.
type indexedPageType struct{ *indexedType }

func (t indexedPageType) Encode(dst, src []byte, enc encoding.Encoding) ([]byte, error) {
	return enc.EncodeInt32(dst, src)
}

func (t indexedPageType) Decode(dst, src []byte, enc encoding.Encoding) ([]byte, error) {
	return enc.DecodeInt32(dst, src)
}

type indexedPageValues struct {
	page   *indexedPage
	offset int
}

func (r *indexedPageValues) ReadValues(values []Value) (n int, err error) {
	if n = len(r.page.values) - r.offset; n == 0 {
		return 0, io.EOF
	}
	if n > len(values) {
		n = len(values)
	}
	r.page.typ.dict.Lookup(r.page.values[r.offset:r.offset+n], values[:n])
	r.offset += n
	if r.offset == len(r.page.values) {
		err = io.EOF
	}
	return n, err
}

// indexedColumnBuffer is an implementation of the ColumnBuffer interface which
// builds a page of indexes into a parent dictionary when values are written.
type indexedColumnBuffer struct{ indexedPage }

func newIndexedColumnBuffer(typ *indexedType, columnIndex int16, numValues int32) *indexedColumnBuffer {
	return &indexedColumnBuffer{
		indexedPage: indexedPage{
			typ:         typ,
			values:      make([]int32, 0, numValues),
			columnIndex: ^columnIndex,
		},
	}
}

func (col *indexedColumnBuffer) Clone() ColumnBuffer {
	return &indexedColumnBuffer{
		indexedPage: indexedPage{
			typ:         col.typ,
			values:      append([]int32{}, col.values...),
			columnIndex: col.columnIndex,
		},
	}
}

func (col *indexedColumnBuffer) ColumnIndex() ColumnIndex { return indexedColumnIndex{col} }

func (col *indexedColumnBuffer) OffsetIndex() OffsetIndex { return indexedOffsetIndex{col} }

func (col *indexedColumnBuffer) BloomFilter() BloomFilter { return nil }

func (col *indexedColumnBuffer) Dictionary() Dictionary { return col.typ.dict }

func (col *indexedColumnBuffer) Pages() Pages { return onePage(col.Page()) }

func (col *indexedColumnBuffer) Page() BufferedPage { return &col.indexedPage }

func (col *indexedColumnBuffer) Reset() { col.values = col.values[:0] }

func (col *indexedColumnBuffer) Cap() int { return cap(col.values) }

func (col *indexedColumnBuffer) Len() int { return len(col.values) }

func (col *indexedColumnBuffer) Less(i, j int) bool {
	u := col.typ.dict.Index(col.values[i])
	v := col.typ.dict.Index(col.values[j])
	return col.typ.Compare(u, v) < 0
}

func (col *indexedColumnBuffer) Swap(i, j int) {
	col.values[i], col.values[j] = col.values[j], col.values[i]
}

func (col *indexedColumnBuffer) WriteValues(values []Value) (int, error) {
	i := len(col.values)
	j := len(col.values) + len(values)

	if j <= cap(col.values) {
		col.values = col.values[:j]
	} else {
		tmp := make([]int32, j, 2*j)
		copy(tmp, col.values)
		col.values = tmp
	}

	col.typ.dict.Insert(col.values[i:], values)
	return len(values), nil
}

func (col *indexedColumnBuffer) writeValues(rows array, size, offset uintptr, _ columnLevels) {
	i := len(col.values)
	j := len(col.values) + rows.len

	if j <= cap(col.values) {
		col.values = col.values[:j]
	} else {
		tmp := make([]int32, j, 2*j)
		copy(tmp, col.values)
		col.values = tmp
	}

	col.typ.dict.insert(col.values[i:], rows, size, offset)
}

func (col *indexedColumnBuffer) ReadValuesAt(values []Value, offset int64) (n int, err error) {
	i := int(offset)
	switch {
	case i < 0:
		return 0, errRowIndexOutOfBounds(offset, int64(len(col.values)))
	case i >= len(col.values):
		return 0, io.EOF
	default:
		for n < len(values) && i < len(col.values) {
			values[n] = col.typ.dict.Index(col.values[i])
			values[n].columnIndex = col.columnIndex
			n++
			i++
		}
		if n < len(values) {
			err = io.EOF
		}
		return n, err
	}
}

func (col *indexedColumnBuffer) ReadRowAt(row Row, index int64) (Row, error) {
	switch {
	case index < 0:
		return row, errRowIndexOutOfBounds(index, int64(len(col.values)))
	case index >= int64(len(col.values)):
		return row, io.EOF
	default:
		v := col.typ.dict.Index(col.values[index])
		v.columnIndex = col.columnIndex
		return append(row, v), nil
	}
}

type indexedColumnIndex struct{ col *indexedColumnBuffer }

func (index indexedColumnIndex) NumPages() int       { return 1 }
func (index indexedColumnIndex) NullCount(int) int64 { return 0 }
func (index indexedColumnIndex) NullPage(int) bool   { return false }
func (index indexedColumnIndex) MinValue(int) Value {
	min, _, _ := index.col.Bounds()
	return min
}
func (index indexedColumnIndex) MaxValue(int) Value {
	_, max, _ := index.col.Bounds()
	return max
}
func (index indexedColumnIndex) IsAscending() bool {
	min, max, _ := index.col.Bounds()
	return index.col.typ.Compare(min, max) <= 0
}
func (index indexedColumnIndex) IsDescending() bool {
	min, max, _ := index.col.Bounds()
	return index.col.typ.Compare(min, max) > 0
}

type indexedOffsetIndex struct{ col *indexedColumnBuffer }

func (index indexedOffsetIndex) NumPages() int                { return 1 }
func (index indexedOffsetIndex) Offset(int) int64             { return 0 }
func (index indexedOffsetIndex) CompressedPageSize(int) int64 { return index.col.Size() }
func (index indexedOffsetIndex) FirstRowIndex(int) int64      { return 0 }
