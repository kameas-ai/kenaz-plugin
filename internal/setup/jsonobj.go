package setup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// object is a JSON object that keeps its members in file order and their
// values as raw JSON, so a merge touches only the keys it means to.
type object struct {
	members []member
}

type member struct {
	Key string
	Val json.RawMessage
}

var errNotObject = errors.New("not a JSON object")

func parseObject(b []byte) (*object, error) {
	if len(bytes.TrimSpace(b)) == 0 {
		return &object{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errNotObject
	}
	o := &object{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("unexpected token %v", tok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		o.members = append(o.members, member{key, raw})
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data after JSON object")
	}
	return o, nil
}

func (o *object) get(key string) (json.RawMessage, bool) {
	for i := len(o.members) - 1; i >= 0; i-- {
		if o.members[i].Key == key {
			return o.members[i].Val, true
		}
	}
	return nil, false
}

// set replaces the last occurrence of key in place, or appends it.
func (o *object) set(key string, val json.RawMessage) {
	for i := len(o.members) - 1; i >= 0; i-- {
		if o.members[i].Key == key {
			o.members[i].Val = val
			return
		}
	}
	o.members = append(o.members, member{key, val})
}

func (o *object) del(key string) {
	out := o.members[:0]
	for _, m := range o.members {
		if m.Key != key {
			out = append(out, m)
		}
	}
	o.members = out
}

func (o *object) len() int { return len(o.members) }

// render writes the object with stable two-space indentation.
func (o *object) render(prefix string) ([]byte, error) {
	if len(o.members) == 0 {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, m := range o.members {
		buf.WriteString(prefix + "  ")
		k, err := encodeString(m.Key)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteString(": ")
		if err := json.Indent(&buf, m.Val, prefix+"  ", "  "); err != nil {
			return nil, err
		}
		if i < len(o.members)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	buf.WriteString(prefix + "}")
	return buf.Bytes(), nil
}

// encodeString JSON-encodes s without HTML escaping.
func encodeString(s string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// sameJSON reports whether two raw JSON values are equal ignoring
// insignificant whitespace.
func sameJSON(a, b json.RawMessage) bool {
	var ca, cb bytes.Buffer
	if json.Compact(&ca, a) != nil || json.Compact(&cb, b) != nil {
		return bytes.Equal(a, b)
	}
	return bytes.Equal(ca.Bytes(), cb.Bytes())
}

// rawString returns the Go string in a raw JSON string value.
func rawString(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	return s, true
}
