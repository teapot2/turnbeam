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
