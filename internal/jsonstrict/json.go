// Package jsonstrict checks closed JSON contracts without accepting Go's field aliases.
package jsonstrict

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

var ErrInvalid = errors.New("invalid JSON structure")

// Validate rejects invalid UTF-8, duplicate keys, deep nesting and multiple values.
func Validate(data []byte) error {
	if !utf8.Valid(data) {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := scan(d, 0); err != nil {
		var raw json.RawMessage
		if syntax := json.Unmarshal(data, &raw); syntax != nil {
			return errors.Join(ErrInvalid, syntax)
		}
		return errors.Join(ErrInvalid, err)
	}
	if _, err := d.Token(); err != io.EOF {
		return ErrInvalid
	}
	return nil
}

// Unmarshal checks exact JSON tags, required fields and nullability before decoding.
// Call Validate first when version-gating a document; otherwise use Decode.
func Unmarshal(data []byte, target any) error {
	t := reflect.TypeOf(target)
	if t == nil || t.Kind() != reflect.Pointer || reflect.ValueOf(target).IsNil() {
		return ErrInvalid
	}
	if err := shape(data, t.Elem()); err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return errors.Join(ErrInvalid, err)
	}
	return nil
}

// Decode validates and decodes one complete, closed-schema document.
func Decode(data []byte, target any) error {
	if err := Validate(data); err != nil {
		return err
	}
	return Unmarshal(data, target)
}

func shape(data json.RawMessage, typ reflect.Type) error {
	null := bytes.Equal(bytes.TrimSpace(data), []byte("null"))
	if typ.Kind() == reflect.Pointer {
		if null {
			return nil
		}
		return shape(data, typ.Elem())
	}
	if null {
		return ErrInvalid
	}
	switch typ.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil {
			return ErrInvalid
		}
		for i := 0; i < typ.NumField(); i++ {
			name, option, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			value, exists := fields[name]
			if !exists {
				if option != "omitempty" {
					return ErrInvalid
				}
				continue
			}
			if err := shape(value, typ.Field(i).Type); err != nil {
				return err
			}
			delete(fields, name)
		}
		if len(fields) != 0 {
			return ErrInvalid
		}
	case reflect.Slice:
		var items []json.RawMessage
		if json.Unmarshal(data, &items) != nil {
			return ErrInvalid
		}
		for _, item := range items {
			if err := shape(item, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		var items map[string]json.RawMessage
		if json.Unmarshal(data, &items) != nil {
			return ErrInvalid
		}
		for _, item := range items {
			if err := shape(item, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

func scan(d *json.Decoder, depth int) error {
	if depth > 32 {
		return ErrInvalid
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	switch token {
	case json.Delim('{'):
		keys := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil {
				return err
			}
			if !ok || keys[name] {
				return ErrInvalid
			}
			keys[name] = true
			if err := scan(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
	case json.Delim('['):
		for d.More() {
			if err := scan(d, depth+1); err != nil {
				return err
			}
		}
		_, err = d.Token()
	}
	return err
}
