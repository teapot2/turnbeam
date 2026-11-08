package jsonstrict

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestClosedMapValues(t *testing.T) {
	type row struct {
		Count int     `json:"count"`
		Name  *string `json:"name,omitempty"`
	}
	type document struct {
		Rows map[string]row `json:"rows"`
	}
	for _, data := range []string{`{"rows":{}}`, `{"rows":{"a":{"count":0}}}`, `{"rows":{"a":{"count":0,"name":null}}}`} {
		var out document
		if err := Decode([]byte(data), &out); err != nil {
			t.Fatalf("valid closed map: %v", err)
		}
	}
	for _, data := range []string{
		`{"rows":{"a":{}}}`, `{"rows":{"a":{"Count":0}}}`, `{"rows":{"a":{"count":0,"extra":1}}}`,
		`{"rows":{"a":{"count":null}}}`, `{"rows":{"a":null}}`, `{"rows":null}`,
		`{"rows":{"a":{"count":0,"count":1}}}`, `{"rows":{},"rows":{}}`, `{"rows":{}} {}`,
		`{"rows":{"a":{"count":0.5}}}`, `{"rows":{"a":{"count":0}}`,
		strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34), string([]byte{0xff}),
	} {
		var out document
		if err := Decode([]byte(data), &out); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted malformed contract %s: %v", data, err)
		}
	}
	var out document
	var syntax *json.SyntaxError
	if err := Decode([]byte(`{"rows":`), &out); !errors.As(err, &syntax) {
		t.Fatalf("syntax error lost: %v", err)
	}
}

func TestTagOptionsMatchEncodingJSON(t *testing.T) {
	type document struct {
		Required int    `json:"required"`
		Later    int    `json:"later,string,omitempty"`
		Zero     int    `json:"zero,omitzero"`
		Plain    int    // untagged: encoding/json uses the field name
		private  int    // unexported: ignored by encoding/json
		Skipped  string `json:"-"`
	}
	for _, data := range []string{`{"required":1,"Plain":2}`, `{"required":1,"Plain":2,"later":"3","zero":4}`} {
		var out document
		if err := Decode([]byte(data), &out); err != nil {
			t.Fatalf("valid document %s rejected: %v", data, err)
		}
	}
	for _, data := range []string{`{"Plain":2}`, `{"required":1}`, `{"required":1,"Plain":2,"private":3}`, `{"required":1,"Plain":2,"-":""}`} {
		var out document
		if err := Decode([]byte(data), &out); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid document %s accepted: %v", data, err)
		}
	}
	type Embedded struct {
		Inner int `json:"inner"`
	}
	type withEmbedded struct {
		Embedded
	}
	var out withEmbedded
	if err := Decode([]byte(`{"inner":1}`), &out); !errors.Is(err, errEmbedded) {
		t.Fatalf("embedded struct not reported: %v", err)
	}
}
