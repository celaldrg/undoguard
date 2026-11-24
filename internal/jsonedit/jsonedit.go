// Package jsonedit reads and writes JSON documents while keeping the
// order of object keys, so that undoguard can add its hooks to an agent's
// settings file without reordering what the user wrote.
package jsonedit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Object is a JSON object that keeps its keys in document order.
type Object struct {
	keys   []string
	values map[string]any
}

// NewObject returns an empty object.
func NewObject() *Object {
	return &Object{values: map[string]any{}}
}

// Get returns the value of key.
func (o *Object) Get(key string) (any, bool) {
	v, ok := o.values[key]
	return v, ok
}

// Set sets the value of key. A new key goes last.
func (o *Object) Set(key string, v any) {
	if _, ok := o.values[key]; !ok {
		o.keys = append(o.keys, key)
	}
	o.values[key] = v
}

// Delete removes key.
func (o *Object) Delete(key string) {
	if _, ok := o.values[key]; !ok {
		return
	}
	delete(o.values, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// Len returns the number of keys.
func (o *Object) Len() int {
	return len(o.keys)
}

// Parse reads a JSON document. Objects become *Object, arrays []any and
// numbers json.Number, so that they are written back as they were;
// strings, booleans and null are as encoding/json makes them.
func Parse(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := value(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the JSON document")
	}
	return v, nil
}

func value(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch d {
	case '{':
		o := NewObject()
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, ok := kt.(string)
			if !ok {
				return nil, fmt.Errorf("object key %v is not a string", kt)
			}
			v, err := value(dec)
			if err != nil {
				return nil, err
			}
			o.Set(k, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return o, nil
	case '[':
		arr := []any{}
		for dec.More() {
			v, err := value(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return arr, nil
	}
	return nil, fmt.Errorf("unexpected %q", rune(d))
}

// Marshal writes v as JSON indented by indent, with a final newline.
func Marshal(v any, indent string) ([]byte, error) {
	var buf bytes.Buffer
	if err := write(&buf, v, indent, 0); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

func write(buf *bytes.Buffer, v any, indent string, depth int) error {
	newline := func(d int) {
		buf.WriteByte('\n')
		for range d {
			buf.WriteString(indent)
		}
	}
	switch t := v.(type) {
	case *Object:
		if t.Len() == 0 {
			buf.WriteString("{}")
			return nil
		}
		buf.WriteByte('{')
		for i, k := range t.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			newline(depth + 1)
			if err := scalar(buf, k); err != nil {
				return err
			}
			buf.WriteString(": ")
			if err := write(buf, t.values[k], indent, depth+1); err != nil {
				return err
			}
		}
		newline(depth)
		buf.WriteByte('}')
	case []any:
		if len(t) == 0 {
			buf.WriteString("[]")
			return nil
		}
		buf.WriteByte('[')
		for i, x := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			newline(depth + 1)
			if err := write(buf, x, indent, depth+1); err != nil {
				return err
			}
		}
		newline(depth)
		buf.WriteByte(']')
	default:
		return scalar(buf, t)
	}
	return nil
}

// scalar writes a string, number, boolean or null without escaping HTML
// characters, so that commands such as "a && b" stay readable.
func scalar(buf *bytes.Buffer, v any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return err
	}
	buf.Write(bytes.TrimRight(b.Bytes(), "\n"))
	return nil
}

// Indent guesses the indentation of a JSON document: the leading white
// space of its first indented line, or two spaces.
func Indent(b []byte) string {
	lines := bytes.Split(b, []byte("\n"))
	for _, line := range lines[1:] {
		rest := bytes.TrimLeft(line, " \t")
		if len(rest) == 0 {
			continue
		}
		if n := len(line) - len(rest); n > 0 {
			return string(line[:n])
		}
		break
	}
	return "  "
}
