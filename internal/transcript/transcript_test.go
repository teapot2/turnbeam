package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func ptr(value string) *string { return &value }

func example() *Transcript {
	return &Transcript{
		SchemaVersion: SchemaVersion,
		Recording:     Recording{Name: "meeting.wav", DurationMS: 30000, SHA256: strings.Repeat("ab", 32)},
		Pipeline: Pipeline{
			Engine: EngineMeta, MetaModel: "muse-voice-transcribe-1.0", MetaMode: "DIARIZATION",
			DiarizationModel: "nemotron-3-diarization", DiarizationRevision: "pinned-revision", RuntimeVersion: "0.3.0",
		},
		Speakers: []Speaker{
			{ID: "speaker_0001", Index: 1, Name: ptr("Alice")},
			{ID: "speaker_0002", Index: 2},
		},
		Turns: []Turn{
			{ID: "turn_000001", StartMS: 0, EndMS: 2500, Text: "Hello.", SpeakerID: ptr("speaker_0001"),
				Attribution: Attribution{Status: Assigned, Method: TimelineOverlap}, Source: Source{ChunkID: "chunk_0001", MetaTurnID: 0}},
			{ID: "turn_000002", StartMS: 3000, EndMS: 4000, Text: "Yes.",
				Attribution: Attribution{Status: Unknown, Reason: AmbiguousOverlapOrBoundary, CandidateSpeakerIDs: []string{"speaker_0001", "speaker_0002"}},
				Source:      Source{ChunkID: "chunk_0001", MetaTurnID: 1}},
			{ID: "turn_000003", StartMS: 14000, EndMS: 14500, Text: "Next.", SpeakerID: ptr("speaker_0002"),
				Attribution: Attribution{Status: Assigned, Method: TimelineOverlap}, Source: Source{ChunkID: "chunk_0002", MetaTurnID: 0}},
		},
		SourceChunks: []SourceChunk{
			{ID: "chunk_0001", StartMS: 0, EndMS: 15000, MetaSessionID: "session-one", RawTurns: []RawTurn{
				{TurnID: 1, StartMS: 3000, EndMS: 4000, Transcript: "Yes."},
				{TurnID: 0, StartMS: 0, EndMS: 2500, Transcript: "Hello.", Speaker: ptr("A")},
			}},
			{ID: "chunk_0002", StartMS: 14000, EndMS: 30000, MetaSessionID: "session-two", RawTurns: []RawTurn{
				{TurnID: 0, StartMS: 0, EndMS: 500, Transcript: "Next.", Speaker: ptr("A")},
			}},
		},
	}
}

func TestRoundTrip(t *testing.T) {
	want := example()
	data, err := Encode(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed canonical data: %+v", got)
	}
	if !bytes.Contains(data, []byte(`"speakerId": null`)) || !bytes.Contains(data, []byte(`"name": null`)) {
		t.Fatal("unavailable identities/names must be explicit nulls")
	}
	again, err := Encode(got)
	if err != nil || !bytes.Equal(data, again) {
		t.Fatalf("encoding is not deterministic: %v", err)
	}
	// The same Meta label A in two requests intentionally maps to different IDs.
	if *got.Turns[0].SpeakerID == *got.Turns[2].SpeakerID {
		t.Fatal("request-local labels became global identities")
	}
	if got.SourceChunks[0].RawTurns[0].TurnID != 1 {
		t.Fatal("API ordering was not retained")
	}
}

func TestDisplayNamesDoNotChangeIdentity(t *testing.T) {
	doc := example()
	before, err := Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	doc.Speakers[0].Name = ptr("Alex")
	after, err := Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, bytes.Replace(before, []byte(`"Alice"`), []byte(`"Alex"`), 1)) {
		t.Fatal("changing a display name altered other canonical data")
	}
}

func TestObservedMetaEdgeCases(t *testing.T) {
	doc := example()
	// Real Meta responses can include zero-duration text and empty text records.
	doc.SourceChunks[0].RawTurns[1].EndMS = 0
	doc.SourceChunks[0].RawTurns[0].Transcript = ""
	doc.Turns[0].EndMS = 0
	doc.Turns[0].SpeakerID = nil
	doc.Turns[0].Attribution = Attribution{Status: Unknown, Reason: InsufficientTiming}
	doc.Turns = append(doc.Turns[:1], doc.Turns[2:]...)
	data, err := Encode(doc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(data)
	if err != nil || !reflect.DeepEqual(got, doc) {
		t.Fatalf("source edge cases were lost or timings invented: %v", err)
	}
	if got.Turns[0].StartMS != got.Turns[0].EndMS || got.Turns[0].SpeakerID != nil {
		t.Fatal("zero-duration text acquired invented timing or identity")
	}
	doc.Turns[0].SpeakerID = ptr("speaker_0001")
	doc.Turns[0].Attribution = Attribution{Status: Assigned, Method: TimelineOverlap}
	if err := Validate(doc); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero-duration text was allowed a speaker assignment: %v", err)
	}
}

func TestValidVariants(t *testing.T) {
	for name, change := range map[string]func(*Transcript){
		"endpointing without labels": func(doc *Transcript) {
			doc.Pipeline.MetaMode = "ENDPOINTING"
			for i := range doc.SourceChunks {
				for j := range doc.SourceChunks[i].RawTurns {
					doc.SourceChunks[i].RawTurns[j].Speaker = nil
				}
			}
		},
		"empty silent transcript": func(doc *Transcript) {
			doc.Speakers, doc.Turns, doc.SourceChunks = []Speaker{}, []Turn{}, []SourceChunk{}
		},
		"empty API turn list": func(doc *Transcript) {
			doc.Turns = []Turn{}
			doc.SourceChunks[0].RawTurns, doc.SourceChunks[1].RawTurns = []RawTurn{}, []RawTurn{}
		},
		"unknown without candidates": func(doc *Transcript) {
			doc.Turns[1].Attribution.CandidateSpeakerIDs = nil
		},
		"empty raw speaker label": func(doc *Transcript) {
			doc.SourceChunks[0].RawTurns[1].Speaker = ptr("")
		},
		"overlapping canonical turns": func(doc *Transcript) {
			doc.SourceChunks[0].RawTurns[0].StartMS = 2000
			doc.Turns[1].StartMS = 2000
		},
		"equal turn starts": func(doc *Transcript) {
			doc.SourceChunks[0].RawTurns[0].StartMS = 0
			doc.Turns[1].StartMS = 0
		},
		"exact recording end": func(doc *Transcript) {
			doc.SourceChunks[1].RawTurns[0].EndMS = 16000
			doc.Turns[2].EndMS = 30000
		},
		"Unicode and multiline text": func(doc *Transcript) {
			doc.Turns[0].Text = "Hola\n\u4f60\u597d"
			doc.Speakers[0].Name = ptr("Jos\u00e9")
		},
	} {
		t.Run(name, func(t *testing.T) {
			doc := example()
			change(doc)
			data, err := Encode(doc)
			if err != nil {
				t.Fatal(err)
			}
			got, err := Decode(data)
			if err != nil || !reflect.DeepEqual(got, doc) {
				t.Fatalf("round trip failed: %v", err)
			}
		})
	}
}

func TestExactLargeTimestamps(t *testing.T) {
	for _, duration := range []int64{1<<53 + 1, math.MaxInt64} {
		doc := example()
		doc.Recording.DurationMS = duration
		doc.SourceChunks = doc.SourceChunks[1:]
		chunk := &doc.SourceChunks[0]
		chunk.StartMS, chunk.EndMS = duration-1000, duration
		doc.Turns = doc.Turns[2:]
		doc.Turns[0].StartMS, doc.Turns[0].EndMS = duration-1000, duration-500
		data, err := Encode(doc)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(data)
		if err != nil || !reflect.DeepEqual(got, doc) {
			t.Fatalf("lost integer precision for %d: %v", duration, err)
		}
	}
}

func TestInvalidData(t *testing.T) {
	for name, change := range map[string]func(*Transcript){
		"recording path":          func(d *Transcript) { d.Recording.Name = "/private/meeting.wav" },
		"relative recording path": func(d *Transcript) { d.Recording.Name = "../meeting.wav" },
		"Windows path":            func(d *Transcript) { d.Recording.Name = `C:\private\meeting.wav` },
		"empty filename":          func(d *Transcript) { d.Recording.Name = " " },
		"dot filename":            func(d *Transcript) { d.Recording.Name = "." },
		"zero duration":           func(d *Transcript) { d.Recording.DurationMS = 0 },
		"negative duration":       func(d *Transcript) { d.Recording.DurationMS = -1 },
		"bad checksum":            func(d *Transcript) { d.Recording.SHA256 = strings.Repeat("z", 64) },
		"short checksum":          func(d *Transcript) { d.Recording.SHA256 = "ab" },
		"missing provenance":      func(d *Transcript) { d.Pipeline.DiarizationRevision = "" },
		"bad Meta mode":           func(d *Transcript) { d.Pipeline.MetaMode = "diarization" },
		"missing engine":          func(d *Transcript) { d.Pipeline.Engine = "" },
		"unknown engine":          func(d *Transcript) { d.Pipeline.Engine = "whisper" },
		"local model on Meta":     func(d *Transcript) { d.Pipeline.ASRModel = "nemotron" },
		"engine in version 1":     func(d *Transcript) { d.SchemaVersion = 1 },
		"word method on Meta":     func(d *Transcript) { d.Turns[0].Attribution.Method = WordTimelineOverlap },
		"nil speakers":            func(d *Transcript) { d.Speakers = nil },
		"nil turns":               func(d *Transcript) { d.Turns = nil },
		"nil chunks":              func(d *Transcript) { d.SourceChunks = nil },
		"duplicate speaker ID":    func(d *Transcript) { d.Speakers[1].ID = d.Speakers[0].ID },
		"noncontiguous index":     func(d *Transcript) { d.Speakers[1].Index = 3 },
		"empty display name":      func(d *Transcript) { d.Speakers[1].Name = ptr(" ") },
		"duplicate chunk ID":      func(d *Transcript) { d.SourceChunks[1].ID = d.SourceChunks[0].ID },
		"missing session":         func(d *Transcript) { d.SourceChunks[0].MetaSessionID = "" },
		"negative chunk start":    func(d *Transcript) { d.SourceChunks[0].StartMS = -1 },
		"zero chunk length":       func(d *Transcript) { d.SourceChunks[0].EndMS = 0 },
		"chunk past recording":    func(d *Transcript) { d.SourceChunks[1].EndMS = 30001 },
		"unordered chunks": func(d *Transcript) {
			d.SourceChunks[0], d.SourceChunks[1] = d.SourceChunks[1], d.SourceChunks[0]
		},
		"nil raw turns":            func(d *Transcript) { d.SourceChunks[0].RawTurns = nil },
		"negative Meta ID":         func(d *Transcript) { d.SourceChunks[0].RawTurns[0].TurnID = -1 },
		"duplicate Meta ID":        func(d *Transcript) { d.SourceChunks[0].RawTurns[0].TurnID = 0 },
		"negative raw start":       func(d *Transcript) { d.SourceChunks[0].RawTurns[0].StartMS = -1 },
		"inverted raw interval":    func(d *Transcript) { d.SourceChunks[0].RawTurns[0].EndMS = 2999 },
		"raw past chunk":           func(d *Transcript) { d.SourceChunks[0].RawTurns[0].EndMS = 15001 },
		"canonical from empty raw": func(d *Transcript) { d.SourceChunks[0].RawTurns[0].Transcript = "" },
		"invalid raw text":         func(d *Transcript) { d.SourceChunks[0].RawTurns[0].Transcript = string([]byte{0xff}) },
		"invalid Meta label":       func(d *Transcript) { d.SourceChunks[0].RawTurns[0].Speaker = ptr(string([]byte{0xff})) },
		"duplicate turn ID":        func(d *Transcript) { d.Turns[1].ID = d.Turns[0].ID },
		"empty turn ID":            func(d *Transcript) { d.Turns[0].ID = "" },
		"negative turn start":      func(d *Transcript) { d.Turns[0].StartMS = -1 },
		"zero turn length":         func(d *Transcript) { d.Turns[0].EndMS = 0 },
		"inverted turn":            func(d *Transcript) { d.Turns[0].StartMS = 2501 },
		"turn past recording":      func(d *Transcript) { d.Turns[2].EndMS = 30001 },
		"empty text":               func(d *Transcript) { d.Turns[0].Text = " " },
		"invalid UTF-8 text":       func(d *Transcript) { d.Turns[0].Text = string([]byte{0xff}) },
		"unordered turns":          func(d *Transcript) { d.Turns[0], d.Turns[1] = d.Turns[1], d.Turns[0] },
		"missing chunk":            func(d *Transcript) { d.Turns[0].Source.ChunkID = "missing" },
		"missing Meta turn":        func(d *Transcript) { d.Turns[0].Source.MetaTurnID = 99 },
		"wrong chunk offset":       func(d *Transcript) { d.Turns[2].StartMS++ },
		"wrong end offset":         func(d *Transcript) { d.Turns[2].EndMS++ },
		"missing global ID":        func(d *Transcript) { d.Turns[0].SpeakerID = nil },
		"request-local ID":         func(d *Transcript) { d.Turns[0].SpeakerID = ptr("A") },
		"wrong method":             func(d *Transcript) { d.Turns[0].Attribution.Method = "meta_label" },
		"assigned with reason":     func(d *Transcript) { d.Turns[0].Attribution.Reason = "ambiguous" },
		"assigned with candidates": func(d *Transcript) {
			d.Turns[0].Attribution.CandidateSpeakerIDs = []string{"speaker_0001"}
		},
		"unknown with global ID": func(d *Transcript) { d.Turns[1].SpeakerID = ptr("speaker_0001") },
		"unknown without reason": func(d *Transcript) { d.Turns[1].Attribution.Reason = "" },
		"unknown made-up reason": func(d *Transcript) { d.Turns[1].Attribution.Reason = "made_up" },
		"duplicate source turn": func(d *Transcript) {
			copy := d.Turns[2]
			copy.ID = "turn_copy"
			d.Turns = append(d.Turns, copy)
		},
		"uppercase recording hash": func(d *Transcript) { d.Recording.SHA256 = strings.ToUpper(d.Recording.SHA256) },
		"unknown with method":      func(d *Transcript) { d.Turns[1].Attribution.Method = TimelineOverlap },
		"unknown status":           func(d *Transcript) { d.Turns[1].Attribution.Status = "maybe" },
		"missing candidate": func(d *Transcript) {
			d.Turns[1].Attribution.CandidateSpeakerIDs = []string{"missing"}
		},
		"duplicate candidate": func(d *Transcript) {
			d.Turns[1].Attribution.CandidateSpeakerIDs = []string{"speaker_0001", "speaker_0001"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			doc := example()
			change(doc)
			if err := Validate(doc); !errors.Is(err, ErrInvalid) {
				t.Fatalf("expected invalid data, got %v", err)
			}
			if data, err := Encode(doc); data != nil || !errors.Is(err, ErrInvalid) {
				t.Fatalf("invalid data was encoded: %v", err)
			}
		})
	}
	if err := Validate(nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestMalformedJSON(t *testing.T) {
	valid, err := Encode(example())
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"empty":                   nil,
		"truncated":               valid[:len(valid)-1],
		"null":                    []byte("null"),
		"array":                   []byte("[]"),
		"trailing object":         append(bytes.Clone(valid), []byte("{}")...),
		"trailing junk":           append(bytes.Clone(valid), 'x'),
		"missing version":         []byte("{}"),
		"null version":            []byte(`{"schemaVersion":null}`),
		"string version":          []byte(`{"schemaVersion":"1"}`),
		"fractional version":      []byte(`{"schemaVersion":1.1}`),
		"fractional milliseconds": bytes.Replace(valid, []byte(`"startMs": 0`), []byte(`"startMs": 0.5`), 1),
		"integer overflow":        bytes.Replace(valid, []byte(`"durationMs": 30000`), []byte(`"durationMs": 9223372036854775808`), 1),
		"missing zero start":      bytes.Replace(valid, []byte("\"startMs\": 0,"), nil, 1),
		"missing zero ID":         bytes.Replace(valid, []byte("\"metaTurnId\": 0"), []byte(`"ignored": 0`), 1),
		"null milliseconds":       bytes.Replace(valid, []byte(`"startMs": 0`), []byte(`"startMs": null`), 1),
		"null raw array":          bytes.Replace(valid, []byte(`"rawTurns": [`), []byte(`"rawTurns": null, "ignored": [`), 1),
		"null optional slice":     bytes.Replace(valid, []byte(`"method": "timeline_overlap"`), []byte(`"method": "timeline_overlap", "candidateSpeakerIds": null`), 1),
		"missing nullable ID":     bytes.Replace(valid, []byte("\"speakerId\": null,"), nil, 1),
		"wrong field casing":      bytes.Replace(valid, []byte(`"startMs"`), []byte(`"StartMs"`), 1),
		"duplicate root key":      bytes.Replace(valid, []byte(`"schemaVersion": 2`), []byte(`"schemaVersion": 1, "schemaVersion": 2`), 1),
		"duplicate nested key":    bytes.Replace(valid, []byte(`"startMs": 0`), []byte(`"startMs": 5, "startMs": 0`), 1),
		"duplicate escaped key":   bytes.Replace(valid, []byte(`"startMs": 0`), []byte(`"start\u004ds": 5, "startMs": 0`), 1),
		"invalid UTF-8":           bytes.Replace(valid, []byte("Hello."), []byte{0xff}, 1),
		"too deep":                []byte(strings.Repeat("[", 40) + "0" + strings.Repeat("]", 40)),
	} {
		t.Run(name, func(t *testing.T) {
			if doc, err := Decode(data); doc != nil || !errors.Is(err, ErrInvalid) {
				t.Fatalf("malformed data accepted: doc=%v, err=%v", doc, err)
			}
		})
	}
	_, err = Decode(valid[:len(valid)-1])
	var syntax *json.SyntaxError
	if !errors.Is(err, ErrInvalid) || !errors.As(err, &syntax) {
		t.Fatalf("underlying decode error was not preserved: %v", err)
	}
}

func TestVersionGate(t *testing.T) {
	for _, version := range []int{0, 3, -1} {
		doc := example()
		doc.SchemaVersion = version
		if _, err := Encode(doc); !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("unsupported version was encoded: %v", err)
		}
		// Future fields must not cause a future schema to be partially interpreted.
		data, _ := json.Marshal(map[string]any{"schemaVersion": version, "future": true})
		if doc, err := Decode(data); doc != nil || !errors.Is(err, ErrUnsupportedVersion) {
			t.Fatalf("unsupported version was decoded: %v", err)
		}
	}
}

func TestPrivacyBoundary(t *testing.T) {
	valid, err := Encode(example())
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"apiKey", "authorization", "headers", "audio", "cachePath", "confidence"} {
		for _, anchor := range []string{`"schemaVersion": 2`, `"name": "meeting.wav"`, `"turnId": 1`} {
			data := bytes.Replace(valid, []byte(anchor), []byte(anchor+`, "`+field+`": "sensitive-value"`), 1)
			if doc, err := Decode(data); doc != nil || !errors.Is(err, ErrInvalid) {
				t.Fatalf("unexpected field %s accepted: %v", field, err)
			} else if strings.Contains(err.Error(), "sensitive-value") {
				t.Fatal("validation exposed a field value")
			}
		}
	}
	data := bytes.Replace(valid, []byte(`"speaker": "A"`), []byte(`"speaker": null`), 1)
	doc, err := Decode(data)
	if err != nil || doc.SourceChunks[0].RawTurns[1].Speaker != nil {
		t.Fatalf("null API speaker label was not treated as unavailable: %v", err)
	}
}

func FuzzDecode(f *testing.F) {
	valid, _ := Encode(example())
	f.Add(valid)
	f.Add([]byte(`{"schemaVersion":2}`))
	f.Add([]byte("null"))
	f.Fuzz(func(t *testing.T, data []byte) {
		doc, err := Decode(data)
		if err != nil {
			if doc != nil {
				t.Fatal("decode returned partial data on error")
			}
			return
		}
		encoded, err := Encode(doc)
		if err != nil {
			t.Fatal(err)
		}
		again, err := Decode(encoded)
		if err != nil {
			t.Fatal(err)
		}
		stable, err := Encode(again)
		if err != nil || !bytes.Equal(encoded, stable) {
			t.Fatalf("accepted data did not round trip: %v", err)
		}
	})
}

// v1 is a sidecar written before the engine field existed.
func v1() *Transcript {
	doc := example()
	doc.SchemaVersion, doc.Pipeline.Engine = 1, ""
	return doc
}

func TestVersion1RemainsReadable(t *testing.T) {
	data, err := Encode(v1())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"engine"`)) || !bytes.Contains(data, []byte(`"schemaVersion": 1`)) {
		t.Fatalf("version 1 sidecar changed shape:\n%s", data)
	}
	doc, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Encode(doc)
	if err != nil || !bytes.Equal(again, data) {
		t.Fatalf("version 1 did not round-trip unchanged: %v", err)
	}
}

// local is a valid local-engine transcript: no Meta chunks or sources.
func local() *Transcript {
	doc := example()
	doc.Pipeline = Pipeline{Engine: EngineLocal, ASRModel: "nemotron-3.5-asr-streaming-0.6b.q8_0.gguf", ASRRevision: "pinned-revision",
		DiarizationModel: "nemotron-3-diarization", DiarizationRevision: "pinned-revision", RuntimeVersion: "0.3.0"}
	doc.SourceChunks = []SourceChunk{}
	for i := range doc.Turns {
		doc.Turns[i].Source = Source{}
		if doc.Turns[i].Attribution.Status == Assigned {
			doc.Turns[i].Attribution.Method = WordTimelineOverlap
		}
	}
	return doc
}

func TestLocalEngine(t *testing.T) {
	data, err := Encode(local())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"source"`)) || bytes.Contains(data, []byte(`"metaModel"`)) || !bytes.Contains(data, []byte(`"engine": "local"`)) {
		t.Fatalf("local transcript carries Meta fields:\n%s", data)
	}
	doc, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc, local()) {
		t.Fatal("local transcript did not round-trip")
	}
	for name, change := range map[string]func(*Transcript){
		"Meta model":         func(d *Transcript) { d.Pipeline.MetaModel = "muse" },
		"Meta mode":          func(d *Transcript) { d.Pipeline.MetaMode = "DIARIZATION" },
		"missing ASR model":  func(d *Transcript) { d.Pipeline.ASRModel = "" },
		"missing revision":   func(d *Transcript) { d.Pipeline.ASRRevision = " " },
		"Meta chunks":        func(d *Transcript) { d.SourceChunks = example().SourceChunks },
		"Meta source":        func(d *Transcript) { d.Turns[0].Source = Source{ChunkID: "chunk_0001"} },
		"turn-level method":  func(d *Transcript) { d.Turns[0].Attribution.Method = TimelineOverlap },
		"zero-length assign": func(d *Transcript) { d.Turns[0].EndMS = d.Turns[0].StartMS },
	} {
		t.Run(name, func(t *testing.T) {
			doc := local()
			change(doc)
			if err := Validate(doc); !errors.Is(err, ErrInvalid) {
				t.Fatalf("expected invalid data, got %v", err)
			}
		})
	}
}

func TestEmptyOptionalFieldsAreRejected(t *testing.T) {
	v1Data, err := Encode(v1())
	if err != nil {
		t.Fatal(err)
	}
	localData, err := Encode(local())
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"empty engine in version 1":   bytes.Replace(v1Data, []byte(`"pipeline": {`), []byte(`"pipeline": {"engine": "",`), 1),
		"empty local model in v1":     bytes.Replace(v1Data, []byte(`"pipeline": {`), []byte(`"pipeline": {"asrModel": "",`), 1),
		"empty Meta model in local":   bytes.Replace(localData, []byte(`"pipeline": {`), []byte(`"pipeline": {"metaModel": "",`), 1),
		"empty Meta mode in local":    bytes.Replace(localData, []byte(`"pipeline": {`), []byte(`"pipeline": {"metaMode": "",`), 1),
		"empty source in local turns": bytes.Replace(localData, []byte(`"text": "Hello.",`), []byte(`"text": "Hello.", "source": {"chunkId": "", "metaTurnId": 0},`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if bytes.Equal(data, v1Data) || bytes.Equal(data, localData) {
				t.Fatal("fixture was not modified")
			}
			if doc, err := Decode(data); doc != nil || !errors.Is(err, ErrInvalid) {
				t.Fatalf("empty optional field accepted: %v", err)
			}
		})
	}
	for _, data := range [][]byte{v1Data, localData} {
		if _, err := Decode(data); err != nil {
			t.Fatalf("valid sidecar rejected: %v", err)
		}
	}
}
