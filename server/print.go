// Copyright 2014 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package server

import (
	"bytes"
	"fmt"

	"golang.org/x/debug/arch"
	"golang.org/x/debug/dwarf"
)

// typeAndAddress associates an address in the target with a DWARF type.
type typeAndAddress struct {
	Type    dwarf.Type
	Address uint64
}

// Routines to print a value using DWARF type descriptions.
// TODO: Does this deserve its own package? It has no dependencies on Server.

// A Printer pretty-prints values in the target address space.
// It can be reused after each printing operation to avoid unnecessary
// allocations. However, it is not safe for concurrent access.
type Printer struct {
	err      error // Sticky error value.
	server   *Server
	dwarf    *dwarf.Data
	arch     *arch.Architecture
	printBuf bytes.Buffer            // Accumulates the output.
	visited  map[typeAndAddress]bool // Prevents looping on cyclic data.
}

// printf prints to printBuf.
func (p *Printer) printf(format string, args ...interface{}) {
	fmt.Fprintf(&p.printBuf, format, args...)
}

// errorf prints the error to printBuf, then sets the sticky error for the
// printer, if not already set.
func (p *Printer) errorf(format string, args ...interface{}) {
	fmt.Fprintf(&p.printBuf, "<"+format+">", args...)
	if p.err != nil {
		return
	}
	p.err = fmt.Errorf(format, args...)
}

// NewPrinter returns a printer that can use the Server to access and print
// values of the specified architecture described by the provided DWARF data.
func NewPrinter(arch *arch.Architecture, dwarf *dwarf.Data, server *Server) *Printer {
	return &Printer{
		server:  server,
		arch:    arch,
		dwarf:   dwarf,
		visited: make(map[typeAndAddress]bool),
	}
}

// reset resets the Printer. It must be called before starting a new
// printing operation.
func (p *Printer) reset() {
	p.err = nil
	p.printBuf.Reset()
	// Just wipe the map rather than reallocating. It's almost always tiny.
	for k := range p.visited {
		delete(p.visited, k)
	}
}

// Sprint returns the pretty-printed value of the item with the given name, such as "main.global".
func (p *Printer) Sprint(name string) (string, error) {
	entry, err := p.dwarf.LookupEntry(name)
	if err != nil {
		return "", err
	}
	p.reset()
	switch entry.Tag {
	case dwarf.TagVariable: // TODO: What other entries have global location attributes?
		var a uint64
		iface := entry.Val(dwarf.AttrLocation)
		if iface != nil {
			a = p.decodeLocation(iface.([]byte))
		}
		p.printEntryValueAt(entry, a)
	default:
		p.errorf("unrecognized entry type %s", entry.Tag)
	}
	return p.printBuf.String(), p.err
}

// Figure 24 of DWARF v4.
const (
	locationAddr = 0x03
)

// decodeLocation decodes the dwarf data describing an address.
func (p *Printer) decodeLocation(data []byte) uint64 {
	switch data[0] {
	case locationAddr:
		return p.arch.Uintptr(data[1:])
	default:
		p.errorf("unimplemented location type %#x", data[0])
	}
	return 0
}

// SprintEntry returns the pretty-printed value of the item with the specified DWARF Entry and address.
func (p *Printer) SprintEntry(entry *dwarf.Entry, a uint64) (string, error) {
	p.reset()
	p.printEntryValueAt(entry, a)
	return p.printBuf.String(), p.err
}

// printEntryValueAt pretty-prints the data at the specified address.
// using the type information in the Entry.
func (p *Printer) printEntryValueAt(entry *dwarf.Entry, a uint64) {
	if a == 0 {
		p.printf("<nil>")
		return
	}
	switch entry.Tag {
	case dwarf.TagVariable, dwarf.TagFormalParameter:
		// OK
	default:
		p.errorf("unrecognized entry type %s", entry.Tag)
		return
	}
	iface := entry.Val(dwarf.AttrType)
	if iface == nil {
		p.errorf("no type")
		return
	}
	typ, err := p.dwarf.Type(iface.(dwarf.Offset))
	if err != nil {
		p.errorf("type lookup: %v", err)
		return
	}
	p.printValueAt(typ, a)
}

// printValueAt pretty-prints the data at the specified address.
// using the provided type information.
func (p *Printer) printValueAt(typ dwarf.Type, a uint64) {
	if a != 0 {
		// Check if we are repeating the same type and address.
		ta := typeAndAddress{typ, a}
		if p.visited[ta] {
			p.printf("(%v %#x)", typ, a)
			return
		}
		p.visited[ta] = true
	}
	switch typ := typ.(type) {
	case *dwarf.BoolType:
		if typ.ByteSize != 1 {
			p.errorf("unrecognized bool size %d", typ.ByteSize)
			return
		}
		if b, err := p.server.peekUint8(a); err != nil {
			p.errorf("reading bool: %s", err)
		} else {
			p.printf("%t", b != 0)
		}
	case *dwarf.PtrType:
		if ptr, err := p.server.peekPtr(a); err != nil {
			p.errorf("reading pointer: %s", err)
		} else {
			p.printf("%#x", ptr)
		}
	case *dwarf.IntType:
		// Sad we can't tell a rune from an int32.
		if i, err := p.server.peekInt(a, typ.ByteSize); err != nil {
			p.errorf("reading integer: %s", err)
		} else {
			p.printf("%d", i)
		}
	case *dwarf.UintType:
		if u, err := p.server.peekUint(a, typ.ByteSize); err != nil {
			p.errorf("reading unsigned integer: %s", err)
		} else {
			p.printf("%d", u)
		}
	case *dwarf.FloatType:
		buf := make([]byte, typ.ByteSize)
		if err := p.server.peekBytes(a, buf); err != nil {
			p.errorf("reading float: %s", err)
			return
		}
		switch typ.ByteSize {
		case 4:
			p.printf("%g", p.arch.Float32(buf))
		case 8:
			p.printf("%g", p.arch.Float64(buf))
		default:
			p.errorf("unrecognized float size %d", typ.ByteSize)
		}
	case *dwarf.ComplexType:
		buf := make([]byte, typ.ByteSize)
		if err := p.server.peekBytes(a, buf); err != nil {
			p.errorf("reading complex: %s", err)
			return
		}
		switch typ.ByteSize {
		case 8:
			p.printf("%g", p.arch.Complex64(buf))
		case 16:
			p.printf("%g", p.arch.Complex128(buf))
		default:
			p.errorf("unrecognized complex size %d", typ.ByteSize)
		}
	case *dwarf.StructType:
		if typ.Kind != "struct" {
			// Could be "class" or "union".
			p.errorf("can't handle struct type %s", typ.Kind)
			return
		}
		p.printf("%s {", typ.String())
		for i, field := range typ.Field {
			if i != 0 {
				p.printf(", ")
			}
			p.printValueAt(field.Type, a+uint64(field.ByteOffset))
		}
		p.printf("}")
	case *dwarf.ArrayType:
		p.printArrayAt(typ, a)
	case *dwarf.InterfaceType:
		p.printInterfaceAt(typ, a)
	case *dwarf.MapType:
		p.printMapAt(typ, a)
	case *dwarf.ChanType:
		p.printChannelAt(typ, a)
	case *dwarf.SliceType:
		p.printSliceAt(typ, a)
	case *dwarf.StringType:
		p.printStringAt(typ, a)
	case *dwarf.TypedefType:
		p.printValueAt(typ.Type, a)
	case *dwarf.FuncType:
		p.printf("%v @%#x ", typ, a)
	case *dwarf.VoidType:
		p.printf("void")
	default:
		p.errorf("unimplemented type %v", typ)
	}
}

func (p *Printer) printArrayAt(typ *dwarf.ArrayType, a uint64) {
	elemType := typ.Type
	length := typ.Count
	stride, ok := p.arrayStride(typ)
	if !ok {
		p.errorf("can't determine element size")
	}
	p.printf("%s{", typ)
	n := length
	if n > 100 {
		n = 100 // TODO: Have a way to control this?
	}
	for i := int64(0); i < n; i++ {
		if i != 0 {
			p.printf(", ")
		}
		p.printValueAt(elemType, a)
		a += stride // TODO: Alignment and padding - not given by Type
	}
	if n < length {
		p.printf(", ...")
	}
	p.printf("}")
}

func (p *Printer) printInterfaceAt(t *dwarf.InterfaceType, a uint64) {
	// t should be a typedef binding a typedef binding a struct.
	tt, ok := t.TypedefType.Type.(*dwarf.TypedefType)
	if !ok {
		p.errorf("bad interface type: not a typedef")
		return
	}
	st, ok := tt.Type.(*dwarf.StructType)
	if !ok {
		p.errorf("bad interface type: not a typedef of a struct")
		return
	}
	p.printf("(")
	tab, err := p.server.peekPtrStructField(st, a, "tab")
	if err != nil {
		p.errorf("reading interface type: %s", err)
	} else {
		f, err := getField(st, "tab")
		if err != nil {
			p.errorf("%s", err)
		} else {
			p.printTypeOfInterface(f.Type, tab)
		}
	}
	p.printf(", ")
	data, err := p.server.peekPtrStructField(st, a, "data")
	if err != nil {
		p.errorf("reading interface value: %s", err)
	} else if data == 0 {
		p.printf("<nil>")
	} else {
		p.printf("%#x", data)
	}
	p.printf(")")
}

// printTypeOfInterface prints the type of the given tab pointer.
func (p *Printer) printTypeOfInterface(t dwarf.Type, a uint64) {
	if a == 0 {
		p.printf("<nil>")
		return
	}
	// t should be a pointer to a typedef binding a struct which contains a field _type.
	// _type should be a pointer to a typedef binding a struct which contains a field _string.
	// _string is the name of the type.
	t1, ok := t.(*dwarf.PtrType)
	if !ok {
		p.errorf("bad type")
		return
	}
	t2, ok := t1.Type.(*dwarf.TypedefType)
	if !ok {
		p.errorf("bad type")
		return
	}
	t3, ok := t2.Type.(*dwarf.StructType)
	if !ok {
		p.errorf("bad type")
		return
	}
	typeField, err := getField(t3, "_type")
	if err != nil {
		p.errorf("%s", err)
		return
	}
	t4, ok := typeField.Type.(*dwarf.PtrType)
	if !ok {
		p.errorf("bad type")
		return
	}
	t5, ok := t4.Type.(*dwarf.TypedefType)
	if !ok {
		p.errorf("bad type")
		return
	}
	t6, ok := t5.Type.(*dwarf.StructType)
	if !ok {
		p.errorf("bad type")
		return
	}
	stringField, err := getField(t6, "_string")
	if err != nil {
		p.errorf("%s", err)
		return
	}
	t7, ok := stringField.Type.(*dwarf.PtrType)
	if !ok {
		p.errorf("bad type")
		return
	}
	stringType, ok := t7.Type.(*dwarf.StringType)
	if !ok {
		p.errorf("bad type")
		return
	}
	typeAddr, err := p.server.peekPtrStructField(t3, a, "_type")
	if err != nil {
		p.errorf("reading interface type: %s", err)
		return
	}
	stringAddr, err := p.server.peekPtrStructField(t6, typeAddr, "_string")
	if err != nil {
		p.errorf("reading interface type: %s", err)
		return
	}
	p.printStringAt(stringType, stringAddr)
}

// maxMapValuesToPrint values are printed for each map; any remaining values are
// truncated to "...".
const maxMapValuesToPrint = 8

func (p *Printer) printMapAt(typ *dwarf.MapType, a uint64) {
	count := 0
	fn := func(keyAddr, valAddr uint64, keyType, valType dwarf.Type) (stop bool) {
		count++
		if count > maxMapValuesToPrint {
			return false
		}
		if count > 1 {
			p.printf(" ")
		}
		p.printValueAt(keyType, keyAddr)
		p.printf(":")
		p.printValueAt(valType, valAddr)
		return true
	}
	p.printf("map[")
	if err := p.server.peekMapValues(typ, a, fn); err != nil {
		p.errorf("reading map values: %s", err)
	}
	if count > maxMapValuesToPrint {
		p.printf(" ...")
	}
	p.printf("]")
}

func (p *Printer) printChannelAt(ct *dwarf.ChanType, a uint64) {
	p.printf("(chan %s ", ct.ElemType)
	defer p.printf(")")

	a, err := p.server.peekPtr(a)
	if err != nil {
		p.errorf("reading channel: %s", err)
		return
	}
	if a == 0 {
		p.printf("<nil>")
		return
	}
	p.printf("%#x", a)

	// ct is a typedef for a pointer to a struct.
	pt, ok := ct.TypedefType.Type.(*dwarf.PtrType)
	if !ok {
		p.errorf("bad channel type: not a pointer")
		return
	}
	st, ok := pt.Type.(*dwarf.StructType)
	if !ok {
		p.errorf("bad channel type: not a pointer to a struct")
		return
	}

	// Print the channel buffer's length (qcount) and capacity (dataqsiz),
	// if not 0/0.
	qcount, err := p.server.peekUintOrIntStructField(st, a, "qcount")
	if err != nil {
		p.errorf("reading channel: %s", err)
		return
	}
	dataqsiz, err := p.server.peekUintOrIntStructField(st, a, "dataqsiz")
	if err != nil {
		p.errorf("reading channel: %s", err)
		return
	}
	if qcount != 0 || dataqsiz != 0 {
		p.printf(" [%d/%d]", qcount, dataqsiz)
	}
}

func (p *Printer) printSliceAt(typ *dwarf.SliceType, a uint64) {
	// Slices look like a struct with fields array *elemtype, len uint32/64, cap uint32/64.
	// BUG: Slice header appears to have fields with ByteSize == 0
	ptr, err := p.server.peekPtrStructField(&typ.StructType, a, "array")
	if err != nil {
		p.errorf("reading slice: %s", err)
		return
	}
	length, err := p.server.peekUintOrIntStructField(&typ.StructType, a, "len")
	if err != nil {
		p.errorf("reading slice: %s", err)
		return
	}
	// Capacity is not used yet.
	_, err = p.server.peekUintOrIntStructField(&typ.StructType, a, "cap")
	if err != nil {
		p.errorf("reading slice: %s", err)
		return
	}
	elemType := typ.ElemType
	size, ok := p.sizeof(typ.ElemType)
	if !ok {
		p.errorf("can't determine element size")
	}
	p.printf("%s{", typ)
	for i := uint64(0); i < length; i++ {
		if i != 0 {
			p.printf(", ")
		}
		p.printValueAt(elemType, ptr)
		ptr += size // TODO: Alignment and padding - not given by Type
	}
	p.printf("}")
}

func (p *Printer) printStringAt(typ *dwarf.StringType, a uint64) {
	const maxStringSize = 100
	if s, err := p.server.peekString(typ, a, maxStringSize); err != nil {
		p.errorf("reading string: %s", err)
	} else {
		p.printf("%q", s)
	}
}

// sizeof returns the byte size of the type.
func (p *Printer) sizeof(typ dwarf.Type) (uint64, bool) {
	size := typ.Size() // Will be -1 if ByteSize is not set.
	if size >= 0 {
		return uint64(size), true
	}
	switch typ.(type) {
	case *dwarf.PtrType:
		// This is the only one we know of, but more may arise.
		return uint64(p.arch.PointerSize), true
	}
	return 0, false
}

// arrayStride returns the stride of a dwarf.ArrayType in bytes.
func (p *Printer) arrayStride(t *dwarf.ArrayType) (uint64, bool) {
	stride := t.StrideBitSize
	if stride > 0 {
		return uint64(stride / 8), true
	}
	return p.sizeof(t.Type)
}

// getField finds the *dwarf.StructField in a dwarf.StructType with name fieldName.
func getField(t *dwarf.StructType, fieldName string) (*dwarf.StructField, error) {
	var r *dwarf.StructField
	for _, f := range t.Field {
		if f.Name == fieldName {
			if r != nil {
				return nil, fmt.Errorf("struct definition repeats field %s", fieldName)
			}
			r = f
		}
	}
	if r == nil {
		return nil, fmt.Errorf("struct field %s missing", fieldName)
	}
	return r, nil
}
