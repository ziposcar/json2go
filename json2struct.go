package json2struct

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/format"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/mohae/firkin/queue"
)

type ShortWriteError struct {
	n int
	written int
	operation string
}

func (e ShortWriteError) Error() string {
	return fmt.Sprintf("short write of %s: wrote %d bytes of %d", e.operation, e.n, e.written)
}

// stringValues is a slice of reflect.Value holding *reflect.StringValue.
// It implements the methods to sort by string.
type stringValues []reflect.Value

func (sv stringValues) Len() int           { return len(sv) }
func (sv stringValues) Swap(i, j int)      { sv[i], sv[j] = sv[j], sv[i] }
func (sv stringValues) Less(i, j int) bool { return sv.get(i) < sv.get(j) }
func (sv stringValues) get(i int) string   { return sv[i].String() }

// Transmogrifier turns JSON into Go struct definitions.
type Transmogrifier struct {
	r          io.Reader
	w          io.Writer
	jw         io.Writer
	name       string
	pkg        string
	importJSON bool
	writeJSON  bool
	isMap bool
}

// NewTransmogrifier returns a new transmogrifier that reads from r and writes
// to w.
func NewTransmogrifier(name string, r io.Reader, w io.Writer) *Transmogrifier {
	return &Transmogrifier{r: r, w: w, name: name, pkg: "main"}
}

// SetPkg set's the package name to s.  The package name will be lowercased.
func (t *Transmogrifier) SetPkg(s string) {
	t.pkg = strings.ToLower(s)
}

// SetImportJSON set's whether or not an import statement for encoding/json
// should be added to the output.
func (t *Transmogrifier) SetImportJSON(b bool) {
	t.importJSON = b
}

// SetJSONWriter set's the writer to which the original json is written to,
// This is most useful when getting the JSON from stdin.
func (t *Transmogrifier) SetJSONWriter(w io.Writer) {
	t.jw = w
}

// SetWriteJSON set's whether or not the source json used should be written
// out to a file.
func (t *Transmogrifier) SetWriteJSON(b bool) {
	t.writeJSON = b
}

// SetIsMap set's whether or not the top level of the JSON is a map.  If
// true, the type will be defined as type Name map[string][]Struct instead
// of type Name struct {}.
//
// If it is a map, the key is an actual key and not the name of the struct.
// The struct will be called Struct.
func (t *Transmogrifier) SetIsMap(b bool) {
	t.isMap = b
}

// Gen generates the struct definitions and outputs it to W.
func (t *Transmogrifier) Gen() error {
	var buff bytes.Buffer
	b := make([]byte, 1024)
	for {
		n, err := t.r.Read(b)
		if err != nil && err != io.EOF {
			return err
		}
		if n == 0 {
			break
		}
		m, err := buff.Write(b[:n])
		if err != nil {
			return err
		}
		if n != m {
			return ShortWriteError{n: n, written: m, operation: "JSON to buffer"}
		}
	}
	if t.writeJSON {
		n, err := t.jw.Write(buff.Bytes())
		if err != nil {
			return err
		}
		if n != buff.Len() {
			return ShortWriteError{n: buff.Len(), written: n, operation: "JSON to file"}
		}
	}
	var res []byte
	var err error
	if t.isMap {
		res, err = GenMapType(t.name, "", buff.Bytes())
	} else {
		res, err = Gen(t.name, buff.Bytes())
	}
	if err != nil {
		return err
	}
	buff.Reset()
	n, err := buff.WriteString(fmt.Sprintf("package %s\n\n", t.pkg))
	if err != nil {
		return err
	}
	if n != (10 + len(t.pkg)) {
		return ShortWriteError{n: len(t.pkg), written: n, operation: "package name to buffer"}
	}

	if t.importJSON {
		n, err = buff.WriteString("import (\n\t\"encoding/json\"\n)\n\n")
		if err != nil {
			return err
		}
		if n != 29 {
			return ShortWriteError{n: 29, written: n, operation: "import to buffer"}
		}
	}
	n, err = buff.Write(res)
	if err != nil {
		return err
	}
	if n != len(res) {
		return ShortWriteError{n: len(res), written: n, operation: "Go struct definition"}
	}
	fmtd, err := format.Source(buff.Bytes())
	n, err = t.w.Write(fmtd)
	if err != nil {
		return err
	}
	if n != len(fmtd) {
		return ShortWriteError{n: len(fmtd), written: n, operation: "formatted Go code"}
	}
	return nil
}

type structDef struct {
	name string
	val  reflect.Value
	buff bytes.Buffer
}

func newStructDef(name string, val reflect.Value) structDef {
	s := structDef{name: name, val: val}
	s.buff.WriteString(fmt.Sprintf("type %s struct {\n", name))
	return s
}

func (s *structDef) Bytes() []byte {
	s.buff.WriteString("}\n\n")
	return s.buff.Bytes()
}

// GenMapType unmarshals JSON-encoded data that is in the form of
// map[string][]Type and returns both the type declaration and the struct
// definition(s) for Type.
func GenMapType(typeName, name string, data []byte) ([]byte, error) {
	if len(typeName) == 0 {
		return nil, fmt.Errorf("type name required")
	}
	typeName = strings.Title(typeName)

	if len(name) == 0 {
		name = "Struct"
	} else {
		name = strings.Title(name)
	}
	var def interface{}
	err := json.Unmarshal(data, &def)
	if err != nil {
		return nil, err
	}
	switch d := def.(type) {
	case []interface{}:
		def = d[0]
	}
	// if it isn't a map, return an error as this only supports maps
	if reflect.TypeOf(def).Kind() != reflect.Map {
		return nil, fmt.Errorf("GenMapType error: expected a map, got %s", reflect.TypeOf(def).Kind())
	}
	// extract the element to use as the basis point for defining the struct
	//
	m := reflect.ValueOf(def)
	keys := m.MapKeys()
	val := m.MapIndex(keys[0])

	var buff bytes.Buffer
	// it it contains a slice, get an element from the slice
	if val.Elem().Kind() == reflect.Slice {
		buff.WriteString(fmt.Sprintf("type %s map[string][]%s\n\n", typeName, name))
		val = val.Elem().Index(0)
	} else {
		buff.WriteString(fmt.Sprintf("type %s map[string]%s\n\n", typeName, name))
	}
	var wg sync.WaitGroup
	q := queue.NewQ(2)
	result := make(chan []byte)
	// create first work item and add to the queue
	s := newStructDef(name, val.Elem())
	q.Enqueue(s)
	// start the worker &  send initial work item
	go func() {
		defineStruct(q, result, &wg)
	}()
	// collect the results until the resCh is closed
	var i int
	for {
		i++
		val, ok := <-result
		if !ok {
			break
		}
		// TODO handle error/short read
		buff.Write(val)
	}
	return buff.Bytes(), nil
}
// Gen unmarshals JSON-encoded data and returns its struct definition(s) using
// the name as the struct's name.  If the JSON includes other maps, the field
// will be an embedded struct, with that struct's definition also being
// generated.  If an error occurs during unmarshalling of the data, it will
// be returned.  If an error occurs while writing to the buffer, that error
// will be returned.
func Gen(name string, data []byte) ([]byte, error) {
	if len(name) == 0 {
		return nil, fmt.Errorf("struct name required")
	}
	name = strings.Title(name)
	// unmarshal the JSON-encoded data
	var def interface{}
	err := json.Unmarshal(data, &def)
	if err != nil {
		return nil, err
	}
	switch d := def.(type) {
	case []interface{}:
		def = d[0]
	}
	var buff bytes.Buffer
	var wg sync.WaitGroup
	q := queue.NewQ(2)
	result := make(chan []byte)
	// start the worker
	// send initial work item
	q.Enqueue(newStructDef(name, reflect.ValueOf(def)))
	go func() {
		defineStruct(q, result, &wg)
	}()
	// collect the results until the resCh is closed
	var i int
	for {
		i++
		val, ok := <-result
		if !ok {
			break
		}
		// TODO handle error/short read
		buff.Write(val)
	}
	return buff.Bytes(), nil
}

func defineStruct(q *queue.Queue, result chan []byte, wg *sync.WaitGroup) {
	for {
		if q.IsEmpty() {
			break
		}
		tmp, ok := q.Dequeue()
		if !ok {
			break
		}
		s := tmp.(structDef)
		var sv stringValues = s.val.MapKeys()
		sort.Sort(sv)
		for _, key := range sv {
			k, tag := getFieldName(key)
			val := s.val.MapIndex(key)
			typ := getValueKind(val)
			// maps are embedded structs
			if typ == reflect.Map.String() {
				tmp := newStructDef(k, val.Elem())
				q.Enqueue(tmp)
				s.buff.WriteString(fmt.Sprintf("\t%s `json:%q`\n", k, tag))
				continue
			}
			// a slicemap is a signal that it is a []T which means pluralize
			// the field name and generate the embedded sturct
			if typ == "slicemap" {
				tmp := newStructDef(k, val.Elem().Index(0).Elem())
				q.Enqueue(tmp)
				s.buff.WriteString(fmt.Sprintf("\t%ss []%s `json:%q`\n", k, k, tag))
				continue
			}
			s.buff.WriteString(fmt.Sprintf("\t%s %s `json:%q`\n", k, typ, tag))
		}
		result <- s.Bytes()
	}
	close(result)
}

func getValueKind(val reflect.Value) string {
	// if the value is nil, return interface{}; what type a nil should be
	// cannot be accurately determined.
	if val.IsNil() {
		return "interface{}"
	}
	switch val.Elem().Type().Kind() {
	case reflect.Float64:
		v := val.Elem().Float()
		if v == float64(int64(v)) {
			return reflect.Int.String()
		}
		return reflect.Float64.String()
	case reflect.Slice:
		v := val.Elem().Index(0).Elem()
		switch v.Type().Kind() {
		case reflect.Float64:
			vv := v.Float()
			if vv == float64(int64(vv)) {
				return fmt.Sprintf("[]%s", reflect.Int.String())
			}
			return fmt.Sprintf("[]%s", reflect.Float64.String())
		case reflect.Map:
			return "slicemap"
		}
		return fmt.Sprintf("[]%s", v.Type().Kind().String())
	}
	return val.Elem().Type().Kind().String()
}

// getFieldName: get the field name and tag for the key.  Underscores are
// removed and values separated by underscores have their first rune
// uppercased, when applicable.  The first part of the FieldName is cleaned to
// ensure that it starts with a valid character and is uppercased.
func getFieldName(key reflect.Value) (name, tag string) {
	tag = key.String()
	vals := strings.Split(tag, "_")
	for i, v := range vals {
		if i == 0 {
			name = cleanFieldName(v)
			name = toUpperInitialism(name)
			continue
		}
		name = fmt.Sprintf("%s%s", name, toUpperInitialism(strings.Title(v)))
	}
	return name, tag
}

func cleanFieldName(s string) string {
	var first string
	var pos int
	for i, w := 0, 0; i < len(s); i += w {
		v, width := utf8.DecodeRuneInString(s[i:])
		w = width
		if shouldDiscard(v) {
			continue
		}
		pos = i + w
		first = numToAlpha(v)
		if first != "" {
			break
		}
		first = string(unicode.ToUpper(v))
		break
	}
	return fmt.Sprintf("%s%s", first, s[pos:])

}

func shouldDiscard(r rune) bool {
	switch r {
	case '~', '!', '@', '#', '$', '%', '^', '&', '*', '-', '_', '=', '+', ':', '.', '<', '>':
		return true
	}
	return false
}

func numToAlpha(r rune) string {
	switch r {
	case '0':
		return "Zero"
	case '1':
		return "One"
	case '2':
		return "Two"
	case '3':
		return "Three"
	case '4':
		return "Four"
	case '5':
		return "Five"
	case '6':
		return "Six"
	case '7':
		return "Seven"
	case '8':
		return "Eight"
	case '9':
		return "Nine"
	}
	return ""
}

// List and comment is from https://github.com/golang/lint/blob/master/lint.go
// Original copyright:
// Copyright (c) 2013 The Go Authors. All rights reserved.
//
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file or at
// https://developers.google.com/open-source/licenses/bsd.
//
// commonInitialisms is a set of common initialisms.
// Only add entries that are highly unlikely to be non-initialisms.
// For instance, "ID" is fine (Freudian code is rare), but "AND" is not.
var commonInitialisms = map[string]struct{}{
	"API":   struct{}{},
	"ASCII": struct{}{},
	"CPU":   struct{}{},
	"CSS":   struct{}{},
	"DNS":   struct{}{},
	"EOF":   struct{}{},
	"GUID":  struct{}{},
	"HTML":  struct{}{},
	"HTTP":  struct{}{},
	"HTTPS": struct{}{},
	"ID":    struct{}{},
	"IP":    struct{}{},
	"JSON":  struct{}{},
	"LHS":   struct{}{},
	"QPS":   struct{}{},
	"RAM":   struct{}{},
	"RHS":   struct{}{},
	"RPC":   struct{}{},
	"SLA":   struct{}{},
	"SMTP":  struct{}{},
	"SSH":   struct{}{},
	"TLS":   struct{}{},
	"TTL":   struct{}{},
	"UI":    struct{}{},
	"UID":   struct{}{},
	"UUID":  struct{}{},
	"URI":   struct{}{},
	"URL":   struct{}{},
	"UTF8":  struct{}{},
	"VM":    struct{}{},
	"XML":   struct{}{},
}

func toUpperInitialism(s string) string {
	tmp := strings.ToUpper(s)
	if _, ok := commonInitialisms[tmp]; ok {
		return tmp
	}
	return s
}
