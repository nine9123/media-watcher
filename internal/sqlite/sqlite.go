// Package sqlite reads tables out of an SQLite 3 database file, read-only.
//
// Just enough to import the databases the Python version wrote (state.db
// with watch progress, library.db with the scan cache): rowid tables,
// overflow pages, UTF-8 text, and committed frames of a -wal file that was
// never checkpointed. No SQL, no indexes, no WITHOUT ROWID tables.
//
// File format: https://www.sqlite.org/fileformat2.html
package sqlite

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
)

// DB is a database file, with its WAL applied, in memory.
type DB struct {
	data     []byte            // the main file
	wal      map[uint32][]byte // page number -> newest committed copy
	pageSize int
	usable   int
	pages    uint32
}

// Open reads path and, if there is one, path+"-wal".
func Open(path string) (*DB, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 100 || !bytes.HasPrefix(data, []byte("SQLite format 3\x00")) {
		return nil, errors.New("not an SQLite 3 database")
	}
	db := &DB{data: data, wal: map[uint32][]byte{}}
	db.pageSize = int(binary.BigEndian.Uint16(data[16:18]))
	if db.pageSize == 1 {
		db.pageSize = 65536
	}
	if db.pageSize < 512 {
		return nil, errors.New("bad page size")
	}
	db.usable = db.pageSize - int(data[20])
	if enc := binary.BigEndian.Uint32(data[56:60]); enc != 0 && enc != 1 {
		return nil, errors.New("only UTF-8 databases are supported")
	}
	db.pages = uint32(len(data) / db.pageSize)
	if wal, err := os.ReadFile(path + "-wal"); err == nil {
		db.applyWAL(wal)
	}
	return db, nil
}

// applyWAL keeps the newest copy of every page from frames up to the last
// valid commit. A torn or stale tail (bad salt or checksum) is ignored, as
// SQLite itself would.
func (db *DB) applyWAL(wal []byte) {
	if len(wal) < 32 {
		return
	}
	magic := binary.BigEndian.Uint32(wal[0:4])
	if magic != 0x377f0682 && magic != 0x377f0683 {
		return
	}
	var order binary.ByteOrder = binary.LittleEndian
	if magic&1 == 1 {
		order = binary.BigEndian
	}
	pageSize := int(binary.BigEndian.Uint32(wal[8:12]))
	if pageSize != db.pageSize {
		return
	}
	salt1, salt2 := binary.BigEndian.Uint32(wal[16:20]), binary.BigEndian.Uint32(wal[20:24])
	s1, s2 := checksum(order, wal[0:24], 0, 0)
	if s1 != binary.BigEndian.Uint32(wal[24:28]) || s2 != binary.BigEndian.Uint32(wal[28:32]) {
		return
	}
	pending := map[uint32][]byte{}
	for off := 32; off+24+pageSize <= len(wal); off += 24 + pageSize {
		hdr := wal[off : off+24]
		if binary.BigEndian.Uint32(hdr[8:12]) != salt1 || binary.BigEndian.Uint32(hdr[12:16]) != salt2 {
			break
		}
		page := wal[off+24 : off+24+pageSize]
		s1, s2 = checksum(order, hdr[0:8], s1, s2)
		s1, s2 = checksum(order, page, s1, s2)
		if s1 != binary.BigEndian.Uint32(hdr[16:20]) || s2 != binary.BigEndian.Uint32(hdr[20:24]) {
			break
		}
		pending[binary.BigEndian.Uint32(hdr[0:4])] = page
		if commit := binary.BigEndian.Uint32(hdr[4:8]); commit != 0 {
			for n, p := range pending {
				db.wal[n] = p
			}
			pending = map[uint32][]byte{}
			db.pages = commit
		}
	}
}

func checksum(order binary.ByteOrder, b []byte, s1, s2 uint32) (uint32, uint32) {
	for i := 0; i+8 <= len(b); i += 8 {
		s1 += order.Uint32(b[i:]) + s2
		s2 += order.Uint32(b[i+4:]) + s1
	}
	return s1, s2
}

func (db *DB) page(n uint32) ([]byte, error) {
	if p, ok := db.wal[n]; ok {
		return p, nil
	}
	if n == 0 || n > db.pages {
		return nil, fmt.Errorf("page %d out of range", n)
	}
	off := int(n-1) * db.pageSize
	if off+db.pageSize > len(db.data) {
		return nil, fmt.Errorf("page %d beyond end of file", n)
	}
	return db.data[off : off+db.pageSize], nil
}

func varint(b []byte) (int64, int) {
	var v uint64
	for i := 0; i < 9 && i < len(b); i++ {
		if i == 8 {
			return int64(v<<8 | uint64(b[i])), 9
		}
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return int64(v), i + 1
		}
	}
	return int64(v), len(b)
}

// walk calls fn with the payload of every row of the table b-tree at root.
func (db *DB) walk(root uint32, fn func(payload []byte) error, depth int) error {
	if depth > 64 {
		return errors.New("b-tree too deep")
	}
	p, err := db.page(root)
	if err != nil {
		return err
	}
	h := 0
	if root == 1 {
		h = 100
	}
	if len(p) < h+12 {
		return errors.New("short page")
	}
	kind := p[h]
	cells := int(binary.BigEndian.Uint16(p[h+3 : h+5]))
	switch kind {
	case 0x05: // interior table page
		for i := 0; i < cells; i++ {
			ptr := int(binary.BigEndian.Uint16(p[h+12+2*i:]))
			if err := db.walk(binary.BigEndian.Uint32(p[ptr:]), fn, depth+1); err != nil {
				return err
			}
		}
		return db.walk(binary.BigEndian.Uint32(p[h+8:h+12]), fn, depth+1)
	case 0x0d: // leaf table page
		for i := 0; i < cells; i++ {
			ptr := int(binary.BigEndian.Uint16(p[h+8+2*i:]))
			size, n := varint(p[ptr:])
			ptr += n
			_, n = varint(p[ptr:]) // rowid
			ptr += n
			payload, err := db.payload(p, ptr, int(size))
			if err != nil {
				return err
			}
			if err := fn(payload); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("page %d is not a table b-tree page (%#x)", root, kind)
}

// payload assembles a cell's payload, following overflow pages.
func (db *DB) payload(p []byte, ptr, size int) ([]byte, error) {
	u := db.usable
	x := u - 35
	if size <= x {
		if ptr+size > len(p) {
			return nil, errors.New("cell beyond page")
		}
		return p[ptr : ptr+size], nil
	}
	m := (u-12)*32/255 - 23
	k := m + (size-m)%(u-4)
	local := m
	if k <= x {
		local = k
	}
	if ptr+local+4 > len(p) {
		return nil, errors.New("cell beyond page")
	}
	out := make([]byte, 0, size)
	out = append(out, p[ptr:ptr+local]...)
	next := binary.BigEndian.Uint32(p[ptr+local:])
	for len(out) < size && next != 0 {
		op, err := db.page(next)
		if err != nil {
			return nil, err
		}
		n := min(size-len(out), u-4)
		out = append(out, op[4:4+n]...)
		next = binary.BigEndian.Uint32(op[0:4])
	}
	if len(out) != size {
		return nil, errors.New("truncated overflow chain")
	}
	return out, nil
}

// record decodes a row: nil, int64, float64, string or []byte per column.
func record(b []byte) ([]any, error) {
	hlen, n := varint(b)
	if int(hlen) > len(b) || hlen < int64(n) {
		return nil, errors.New("bad record header")
	}
	var types []int64
	for pos := n; pos < int(hlen); {
		t, k := varint(b[pos:])
		types = append(types, t)
		pos += k
	}
	body := b[hlen:]
	out := make([]any, len(types))
	for i, t := range types {
		var size int
		switch {
		case t == 0, t == 8, t == 9:
			size = 0
		case t >= 1 && t <= 4:
			size = int(t)
		case t == 5:
			size = 6
		case t == 6, t == 7:
			size = 8
		case t >= 12:
			size = int((t - 12) / 2)
		default:
			return nil, fmt.Errorf("bad serial type %d", t)
		}
		if size > len(body) {
			return nil, errors.New("record body too short")
		}
		v := body[:size]
		body = body[size:]
		switch {
		case t == 0:
			out[i] = nil
		case t == 8:
			out[i] = int64(0)
		case t == 9:
			out[i] = int64(1)
		case t <= 6:
			var x int64
			for _, c := range v {
				x = x<<8 | int64(c)
			}
			shift := 64 - 8*uint(size)
			out[i] = x << shift >> shift // sign-extend
		case t == 7:
			out[i] = math.Float64frombits(binary.BigEndian.Uint64(v))
		case t%2 == 0:
			out[i] = append([]byte(nil), v...)
		default:
			out[i] = string(v)
		}
	}
	return out, nil
}

// Row maps column names to values.
type Row map[string]any

// Table reads every row of a table. Columns added by ALTER TABLE after a
// row was written are missing from that row (nil).
func (db *DB) Table(name string) ([]Row, error) {
	var root uint32
	var sql string
	err := db.walk(1, func(payload []byte) error {
		r, err := record(payload)
		if err != nil {
			return err
		}
		if len(r) >= 5 && r[0] == "table" && strings.EqualFold(fmt.Sprint(r[1]), name) {
			if n, ok := r[3].(int64); ok {
				root = uint32(n)
			}
			sql, _ = r[4].(string)
		}
		return nil
	}, 0)
	if err != nil {
		return nil, err
	}
	if root == 0 {
		return nil, fmt.Errorf("no table %q", name)
	}
	cols := columns(sql)
	var rows []Row
	err = db.walk(root, func(payload []byte) error {
		r, err := record(payload)
		if err != nil {
			return err
		}
		row := Row{}
		for i, c := range cols {
			if i < len(r) {
				row[c] = r[i]
			} else {
				row[c] = nil
			}
		}
		rows = append(rows, row)
		return nil
	}, 0)
	return rows, err
}

// columns pulls the column names out of a CREATE TABLE statement.
func columns(sql string) []string {
	start, end := strings.Index(sql, "("), strings.LastIndex(sql, ")")
	if start < 0 || end <= start {
		return nil
	}
	var out []string
	depth, from := 0, start+1
	defs := []string{}
	for i := start + 1; i < end; i++ {
		switch sql[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				defs = append(defs, sql[from:i])
				from = i + 1
			}
		}
	}
	defs = append(defs, sql[from:end])
	for _, d := range defs {
		f := strings.Fields(d)
		if len(f) == 0 {
			continue
		}
		switch strings.ToUpper(f[0]) {
		case "PRIMARY", "UNIQUE", "CHECK", "FOREIGN", "CONSTRAINT":
			continue
		}
		out = append(out, strings.Trim(f[0], "\"`[]"))
	}
	return out
}
