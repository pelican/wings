package websocket

import (
	jsonv1 "encoding/json"
	"reflect"
	"testing"
)

// DecodeMessage must accept and reject exactly what encoding/json v1 did, and
// produce the same Message, so no client needs to change.
func TestDecodeMessageMatchesV1(t *testing.T) {
	inputs := []string{
		`{"event":"send command","args":["say hi"]}`,
		`{"event":"set state","args":["restart"]}`,
		`{"event":"auth","args":["eyJhbGciOi.payload.sig"]}`,
		`{"event":"send logs"}`,
		`{"event":"send stats","args":[]}`,
		`{"event":"send stats","args":null}`,
		`{"event":"send command","args":["say \u00a7aHello \"world\" <b>&"]}`,
		`{"event":"send command","args":["a","b","c"],"extra":{"ignored":true}}`,
		// Duplicate keys: the last one wins.
		`{"event":"set state","event":"send command","args":["x"]}`,
		// Field names match regardless of case.
		`{"Event":"send command","ARGS":["x"]}`,
		// Invalid UTF-8 is replaced, not rejected.
		"{\"event\":\"send command\",\"args\":[\"caf\xe9 \xff\xfe\"]}",
		`null`,
		`{}`,
		// Rejected by both.
		`{"event":"send command","args":"not a list"}`,
		`{"event":42}`,
		`{"event":"send command","args":[1]}`,
		`[]`,
		`not json`,
		`{"event":"send command"`,
		``,
	}
	for _, in := range inputs {
		var want Message
		wantErr := jsonv1.Unmarshal([]byte(in), &want)

		got, gotErr := DecodeMessage([]byte(in))
		if (gotErr != nil) != (wantErr != nil) {
			t.Errorf("%q: error = %v, v1 error = %v", in, gotErr, wantErr)
			continue
		}
		if wantErr == nil && !reflect.DeepEqual(got, want) {
			t.Errorf("%q:\n got %#v\nwant %#v", in, got, want)
		}
	}
}

func BenchmarkDecodeMessage(b *testing.B) {
	p := []byte(`{"event":"send command","args":["say hello from the console"]}`)
	b.Run("json/v1", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var m Message
			if err := jsonv1.Unmarshal(p, &m); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("json/v2", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := DecodeMessage(p); err != nil {
				b.Fatal(err)
			}
		}
	})
}
