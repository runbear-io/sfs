package journal

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Parse decodes each line into pathRaw directly instead of going through
// Op.UnmarshalJSON, because a type carrying that method makes encoding/json
// walk every line twice — most of the cost of replaying a hub's journals.
//
// That leaves two readers of the same bytes, and path_raw is precisely the
// field where two readers disagreeing means one journal line naming two
// different files (see TestSec_Op_PathRawCannotNameADifferentPathThanPath).
// They share pathRaw.op so they cannot fork; this pins that they don't.
func TestParseAgreesWithOpUnmarshalJSON(t *testing.T) {
	nonUTF8 := secfx4Line(t, Op{Seq: 1, Device: "d", Kind: KindPut, Path: "caf\xff.md", Blob: "b"})

	for name, line := range map[string]string{
		"plain": `{"seq":1,"lamport":2,"time":"2026-01-01T00:00:00Z","device":"d","kind":"put","path":"a.md","blob":"b"}`,
		"delete": `{"seq":2,"lamport":3,"time":"2026-01-01T00:00:00Z","device":"d","kind":"delete","path":"a.md"}`,
		"path_raw round-trips": nonUTF8,
		// path_raw naming a path the `path` field is not the lossy form of:
		// both readers must refuse it, identically.
		"path_raw refused": `{"seq":1,"lamport":1,"device":"peer","kind":"put","path":"notes.md","path_raw":"Li4vLi4vLmJkcml2ZS9jb25maWcuanNvbg==","blob":"b"}`,
		"path_raw garbage": `{"seq":1,"lamport":1,"device":"peer","kind":"put","path":"notes.md","path_raw":"!!!not base64!!!","blob":"b"}`,
	} {
		ops, err := Parse([]byte(line + "\n"))
		if err != nil || len(ops) != 1 {
			t.Fatalf("%s: Parse: %v (%d ops)", name, err, len(ops))
		}
		var viaMethod Op
		if err := json.Unmarshal([]byte(line), &viaMethod); err != nil {
			t.Fatalf("%s: Op.UnmarshalJSON: %v", name, err)
		}
		if !reflect.DeepEqual(ops[0], viaMethod) {
			t.Errorf("%s: the two readers of one line disagree:\n  Parse:           %+v\n  UnmarshalJSON:   %+v",
				name, ops[0], viaMethod)
		}
	}
}
