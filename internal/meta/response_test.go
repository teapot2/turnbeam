package meta

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const success = `{"sessionId":"local-session","transcript":"Hello.","audioDurationMs":1040,"turns":[{"turnId":0,"startMs":0,"endMs":1000,"transcript":"Hello.","speaker":"A"}]}`

func TestResponsePreservesKnownRawFields(t *testing.T) {
	data := `{"sessionId":"session","transcript":"First. Second.","audioDurationMs":1040,"extension":{"audio":"not retained"},"turns":[{"turnId":9,"startMs":500,"endMs":500,"transcript":"Second.","speaker":null,"confidence":0.9},{"turnId":2,"startMs":0,"endMs":600,"transcript":"First.","speaker":"A"},{"turnId":5,"startMs":700,"endMs":800,"transcript":""}]}`
	r, err := decodeResponse([]byte(data), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if r.AudioDurationMS != 1040 || len(r.Turns) != 3 || r.Turns[0].TurnID != 9 || r.Turns[0].StartMS != r.Turns[0].EndMS || r.Turns[0].Speaker != nil || r.Turns[1].Speaker == nil || *r.Turns[1].Speaker != "A" || r.Turns[2].Transcript != "" {
		t.Fatalf("raw data/order changed: %+v", r)
	}
	encoded, err := json.Marshal(r)
	if err != nil || strings.Contains(string(encoded), "confidence") || strings.Contains(string(encoded), "extension") || strings.Contains(string(encoded), "audio") && strings.Contains(string(encoded), "not retained") {
		t.Fatal("unknown extensions persisted")
	}
	if _, err := decodeResponse([]byte(`{"sessionId":"silent","transcript":"","audioDurationMs":0,"turns":[]}`), 1000); err != nil {
		t.Fatal("silent response rejected")
	}
}

func TestMalformedResponses(t *testing.T) {
	for _, replacement := range [][2]string{
		{`"sessionId":`, `"error":{"message":"failed"},"sessionId":`},
		{`"sessionId":"local-session",`, ""}, {`"sessionId":"local-session"`, `"sessionId":null`}, {`"sessionId":"local-session"`, `"sessionId":" "`},
		{`"audioDurationMs":1040`, `"audioDurationMs":-1`}, {`"audioDurationMs":1040`, `"audioDurationMs":true`}, {`"audioDurationMs":1040`, `"audioDurationMs":1.5`},
		{`"turnId":0`, `"turnId":-1`}, {`"turnId":0,`, ""}, {`"turnId":0`, `"turnId":null`}, {`"turnId":0`, `"turnId":0,"turnId":1`},
		{`"startMs":0,`, ""}, {`"startMs":0`, `"startMs":null`}, {`"startMs":0`, `"startMs":-1`}, {`"startMs":0`, `"startMs":1001`},
		{`"endMs":1000`, `"endMs":1101`}, {`"endMs":1000`, `"endMs":9223372036854775808`}, {`"endMs":1000`, `"endMs":"1000"`},
		{`"transcript":"Hello."`, `"transcript":null`}, {`"speaker":"A"`, `"speaker":4`}, {`"speaker":"A"`, `"speaker":{}`},
		{`"sessionId":`, `"SessionId":`}, {`"sessionId":"local-session",`, `"sessionId":"x","sessionId":"local-session",`},
	} {
		data := strings.Replace(success, replacement[0], replacement[1], 1)
		if r, err := decodeResponse([]byte(data), 1000); r != nil || !errors.Is(err, ErrResponse) {
			t.Fatalf("malformed response accepted (%s): %+v, %v", replacement[1], r, err)
		}
	}
	for _, data := range []string{"null", "[]", success + success, success[:len(success)-1], strings.Replace(success, `"turns":[`, `"turns":[null,`, 1), strings.Replace(success, `}]}`, `},{"turnId":0,"startMs":0,"endMs":1,"transcript":""}]}`, 1), `{"sessionId":"s","transcript":"text without provenance","audioDurationMs":1000,"turns":[]}`, `{"sessionId":"s","transcript":"","audioDurationMs":1000,"turns":null}`} {
		if r, err := decodeResponse([]byte(data), 1000); r != nil || err == nil {
			t.Fatalf("invalid JSON/turns accepted: %+v, %v", r, err)
		}
	}
	if r, err := decodeResponse(append([]byte(success), 0xff), 1000); r != nil || err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func FuzzResponse(f *testing.F) {
	f.Add([]byte(success))
	f.Add([]byte(`{"sessionId":"s","transcript":"","audioDurationMs":0,"turns":[]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			return
		}
		r, err := decodeResponse(data, 1000)
		if err != nil {
			if r != nil {
				t.Fatal("partial response")
			}
			return
		}
		ids := map[int64]bool{}
		for _, turn := range r.Turns {
			if turn.TurnID < 0 || ids[turn.TurnID] || turn.StartMS < 0 || turn.EndMS < turn.StartMS || turn.EndMS > 1000 {
				t.Fatal("invalid raw turn")
			}
			ids[turn.TurnID] = true
		}
	})
}

func TestSmallTurnOvershootIsClipped(t *testing.T) {
	for _, tc := range []struct{ start, end, wantStart, wantEnd int64 }{
		{0, 1040, 0, 1000},       // Meta's clip clock runs about 40 ms long
		{0, 1100, 0, 1000},       // exactly at the tolerance
		{1050, 1060, 1000, 1000}, // a turn wholly in the overshoot keeps zero duration
	} {
		data := strings.Replace(success, `"startMs":0,"endMs":1000`, fmt.Sprintf(`"startMs":%d,"endMs":%d`, tc.start, tc.end), 1)
		r, err := decodeResponse([]byte(data), 1000)
		if err != nil || r.Turns[0].StartMS != tc.wantStart || r.Turns[0].EndMS != tc.wantEnd {
			t.Fatalf("%d-%d: got %+v, %v", tc.start, tc.end, r, err)
		}
		encoded, _ := json.Marshal(r)
		if again, err := decodeResponse(encoded, 1000); err != nil || again.Turns[0].StartMS != tc.wantStart || again.Turns[0].EndMS != tc.wantEnd {
			t.Fatal("clipped response did not survive a checkpoint round trip", err)
		}
	}
}
