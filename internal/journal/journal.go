// Package journal implements beardrive's append-only operation log.
//
// Every change to a volume is recorded as an Op in a per-device JSONL
// journal. Journals are append-only and each device only ever writes its
// own journal, so syncing is conflict-free at the transport level: a sync
// uploads your journal and downloads everyone else's. The merged view of
// a volume is a deterministic replay of the union of all ops ordered by
// (lamport, time, device, seq) — every device converges to the same state.
package journal

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	KindPut    = "put"
	KindDelete = "delete"
)

// Op is a single journaled file operation.
type Op struct {
	Seq        int64     `json:"seq"`     // per-device sequence number, 1-based
	Lamport    int64     `json:"lamport"` // logical clock for cross-device ordering
	Time       time.Time `json:"time"`
	Device     string    `json:"device"`
	DeviceName string    `json:"device_name,omitempty"`
	Author     string    `json:"author,omitempty"`    // OS/git identity (offline fallback)
	User       string    `json:"user,omitempty"`      // signed-in account email
	UserName   string    `json:"user_name,omitempty"` // signed-in account display name
	Kind       string    `json:"kind"`                // "put" or "delete"
	Path       string    `json:"path"`                // slash-separated, relative to volume root
	Blob       string    `json:"blob,omitempty"`      // sha256 hex of content (put only)
	Size       int64     `json:"size,omitempty"`
	Mode       uint32    `json:"mode,omitempty"` // permission bits
	Note       string    `json:"note,omitempty"` // e.g. "conflict copy of <path>"
	// Session is the agent session this op was committed during, set ONLY by
	// the agent sync hook (`bdrive sync --hook`). Display/join only — never an
	// input to Less or Replay, exactly like Mtime below, so replay stays
	// deterministic and ops written before this field existed simply carry "".
	//
	// It exists because Note is user-settable (`bdrive sync --note`): joining
	// a run's reads to its writes on the note string would let any member with
	// write access forge a note that collides with a teammate's session and
	// hang their reads off it. This field is the un-forgeable half of that
	// pair, so the join reads it and never the note.
	Session string `json:"session,omitempty"`

	// Mtime is when the file was last written, as opposed to Time, which is
	// when the op was committed. Display only — never an input to Less or
	// Replay, since it comes from the filesystem and can be anything.
	Mtime time.Time `json:"mtime,omitzero"` // put only
}

// opWire is Op without its JSON methods, so the marshallers below can reuse
// the struct tags without recursing.
type opWire Op

// pathRaw carries Op.Path byte-exactly when it is not valid UTF-8.
// encoding/json rewrites an invalid byte to U+FFFD, and any byte but NUL and
// '/' is a legal unix filename — so without this, "caf\xe9.md" and
// "caf\xff.md" both arrive at every peer as the same path and one file
// silently overwrites the other. The extra field is emitted only for the paths
// that need it, and an older reader still sees the (lossy) path field.
type pathRaw struct {
	opWire
	PathRaw string `json:"path_raw,omitempty"`
}

func (o Op) MarshalJSON() ([]byte, error) {
	w := pathRaw{opWire: opWire(o)}
	if !utf8.ValidString(o.Path) {
		w.PathRaw = base64.StdEncoding.EncodeToString([]byte(o.Path))
	}
	return json.Marshal(w)
}

func (o *Op) UnmarshalJSON(data []byte) error {
	var w pathRaw
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*o = w.op()
	return nil
}

// op resolves a decoded wire form into an Op. It is the ONE place path_raw is
// honored, shared by UnmarshalJSON above and by Parse, which decodes into
// pathRaw directly instead of going through Op.UnmarshalJSON: a type carrying
// an UnmarshalJSON method makes encoding/json walk every line TWICE, once to
// find the value's extent and again inside the method. Replaying a hub's
// journals is ~60k lines, where that doubling was ~3.5s of the 4.8s parse, on
// a path every viewer request goes through.
//
// The rule itself must not fork between the two readers — two spellings of
// "when does path_raw win" is exactly how the divergence below comes back —
// so it lives here and neither caller reimplements it.
func (w pathRaw) op() Op {
	o := Op(w.opWire)
	if w.PathRaw != "" {
		// path_raw is only ever the byte-exact SOURCE of the path field, so it
		// is applied only when it re-encodes to the path the line already
		// carries. Applied unconditionally, one line named two different files
		// — this reader materialized path_raw, a reader without this field
		// materialized `path`, and the writer picked which devices in a mixed
		// fleet saw which.
		if raw, err := base64.StdEncoding.DecodeString(w.PathRaw); err == nil &&
			lossy(string(raw)) == o.Path {
			o.Path = string(raw)
		}
	}
	return o
}

// lossy is what encoding/json does to a string that is not valid UTF-8: each
// invalid BYTE becomes U+FFFD (not each run — strings.ToValidUTF8 collapses a
// run, which would reject a legitimate path with two bad bytes in a row). It
// is the round trip path_raw exists to undo.
func lossy(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteRune(utf8.RuneError)
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String()
}

// SafePath reports whether p is a path an Op may name. It is THE rule, in one
// place: an op's path is arbitrary JSON off a peer's journal, and it is joined
// onto a working folder on every device, stored as a metadata row on the hub
// and rendered as a tree entry in the browser.
//
// It used to be spelled three times — syncer.unsafeRel (device), the core of
// webapp.cleanUploadPath (browser door) and templates.SafePath (seeding) — and
// they disagreed: unsafeRel, the rule the /store/* journal door relies on, had
// no control-character clause, so a NUL-bearing path the browser door answered
// 400 to was journaled and handed to every device. Three spellings of one rule
// is how these holes happen; callers add their OWN extra rules (reserved dirs,
// on-disk boundary) on top of this one, never a second copy of it.
//
// Refused, never normalized: normalizing would land two different journal
// paths on one file.
func SafePath(p string) bool {
	if p == "" || p == "." || p == ".." || strings.HasPrefix(p, "../") ||
		path.IsAbs(p) || filepath.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	// C0 and DEL. Byte-wise on purpose: a path is bytes (see lossy — two
	// distinct legal unix filenames must not collapse), and in UTF-8 no
	// continuation byte is < 0x80, so a byte in this range is always a real
	// control character and never part of a multi-byte rune.
	//
	// They are not filenames anybody types, and NUL is a value the metadata
	// backends disagree about: Postgres refuses it in a text column (a share
	// on such a path 500s) while sqlite and the file backend keep it. DEL and
	// the C0s render as nothing, so "notes\x7f.md" and "notes.md" are two
	// indistinguishable entries in one tree. Refusing at every ingest is what
	// keeps that divergence unreachable.
	//
	// ZWNJ and ZWJ are the exception, and they are the reason a path needs its
	// own rule at all rather than just calling SafeText. Both are category Cf,
	// so the class test below refuses them — and both are ORTHOGRAPHICALLY
	// REQUIRED: U+200C is what makes "می‌روم" ("I go") the right word in
	// Persian rather than the wrong one, it is mandatory in Devanagari and
	// several other Indic scripts, and U+200D is in every multi-person and
	// profession emoji the macOS file dialog offers. Refusing them does not
	// harden a hub, it tells a Persian speaker their filenames are illegal.
	//
	// The confusability they buy an attacker is real but small and local: a
	// zero-width joiner cannot REORDER anything (that is the bidi family, still
	// refused), so the worst case is two similar-looking names inside a project
	// the reader already has access to — a confusion, not a boundary crossing.
	// A note has no such requirement, so SafeText keeps refusing them.
	return safeRunes(p, true)
}

// SafeText reports whether s is free of the characters that make one rendered
// row lie about what it says. It is SafePath's character rule, split out
// because the path is not the only peer-written string the hub serves to a
// browser and a terminal — an op's Note is rendered next to it in every
// history row and in `bdrive log`.
//
// Three families, all of which render as nothing or as something else:
//
//   - C0 and DEL. Byte-wise on purpose (see lossy): in UTF-8 no continuation
//     byte is < 0x80, so a byte in this range is always a real control
//     character. NUL is also a value the metadata backends disagree about.
//   - C1 (U+0080..U+009F), which every C0 filter misses and which is CSI and
//     friends to any terminal.
//   - the bidi format controls (Trojan Source, CVE-2021-42574).
//     "invoice‮gnp.exe" renders as "invoiceexe.png" in every file
//     listing, tree node, breadcrumb and history row — and downloads as an
//     .exe.
//
// This repo already refuses all three in a project NAME (webapp trimName, "the
// bidi overrides that reorder a rendered row") and strips them on the way to a
// terminal (cmd/bdrive safeField). The path and the note, which reach further
// than either, checked none of them.
func SafeText(s string) bool { return safeRunes(s, false) }

// safeRunes is the character rule itself, in one place. allowJoiners carves out
// ZWNJ and ZWJ for paths only — see SafePath for why a filename needs them and
// a note does not.
func safeRunes(s string, allowJoiners bool) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return false
		}
	}
	for _, r := range s {
		switch {
		case r >= 0x80 && r <= 0x9f: // C1
			return false
		// Every format character (category Cf), plus the tag block, as a CLASS.
		//
		// The bidi controls (LRE/RLE/PDF/LRO/RLO, the isolates, LRM/RLM/ALM),
		// the zero-widths (ZWSP/ZWNJ/ZWJ) and the BOM this used to enumerate are
		// all Cf — and the enumeration was the bug. Round 12 stated the rule
		// ("they render as nothing, so two paths a reader cannot tell apart")
		// and then added the NEIGHBOURS of what was already listed: U+2060 WORD
		// JOINER, the character Unicode introduced to REPLACE U+FEFF, stayed
		// legal while U+FEFF was refused. So did U+00AD, U+2061, U+180E,
		// U+FFF9..U+FFFB — and U+E0020..U+E007F, which encodes all of printable
		// ASCII with no glyph, so a path can carry an arbitrary instruction to
		// whatever agent is told to read it.
		//
		// One rule instead of a list to keep extending: text that renders as
		// nothing cannot be part of a name a reader is expected to check.
		case r == 0x200c || r == 0x200d:
			// ZWNJ / ZWJ: refused in free text, required in real filenames.
			if !allowJoiners {
				return false
			}
		case unicode.Is(unicode.Cf, r), r >= 0xe0000 && r <= 0xe01ef:
			return false
		// The two Unicode line breaks the class test above cannot reach: U+2028
		// is category Zl and U+2029 is Zp, not Cf. The webapp's trimText — the
		// project-NAME rule in the same repo — already deletes both by number
		// ("CSS Text treats U+2028 as a forced break"), and a path travels
		// strictly further than a name. Measured in a browser, the folder row for
		// `line<U+2028>sep.md` paints to exactly the same glyph run as
		// `line sep.md`: one line box, identical width — so the org's public-link
		// audit shows one name for two different files, tooltip included.
		case r == 0x2028, r == 0x2029:
			return false
		}
	}
	return true
}

// Less defines the total order used to replay ops from many devices.
//
// The trailing comparisons are what make it a TOTAL order rather than a
// pre-order: (lamport, time, device, seq) is forgeable — a peer may push two
// ops sharing all four — and Sort is only stable, so without them the winner
// of a tie would be whatever order the caller happened to collect the ops in.
// Everything Replay reads is compared here, so any two ops that still tie fold
// to the same state and the invariant holds on the ops themselves.
func Less(a, b Op) bool {
	if a.Lamport != b.Lamport {
		return a.Lamport < b.Lamport
	}
	if !a.Time.Equal(b.Time) {
		return a.Time.Before(b.Time)
	}
	if a.Device != b.Device {
		return a.Device < b.Device
	}
	if a.Seq != b.Seq {
		return a.Seq < b.Seq
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.Blob != b.Blob {
		return a.Blob < b.Blob
	}
	if a.Size != b.Size {
		return a.Size < b.Size
	}
	return a.Mode < b.Mode
}

func Sort(ops []Op) {
	sort.SliceStable(ops, func(i, j int) bool { return Less(ops[i], ops[j]) })
}

// FileState is the resolved state of one path after replay.
type FileState struct {
	Blob string
	Size int64
	Mode uint32
}

// Replay folds a set of ops (from any number of devices) into the
// resulting volume state. Last writer wins per path under the total order.
func Replay(ops []Op) map[string]FileState {
	sorted := append([]Op(nil), ops...)
	Sort(sorted)
	state := make(map[string]FileState)
	for _, op := range sorted {
		switch op.Kind {
		case KindPut:
			state[op.Path] = FileState{Blob: op.Blob, Size: op.Size, Mode: op.Mode}
		case KindDelete:
			delete(state, op.Path)
		}
	}
	return state
}

// Parse decodes a JSONL journal.
//
// A line that does not decode is skipped, never fatal. Append is a plain
// O_APPEND write (the one state file that cannot be written atomically — it
// only ever grows), so a crash or a full disk leaves a torn final line; and a
// peer's journal is bytes someone else chose. All-or-nothing parsing turned
// either of those into "every op this device ever committed is unreadable",
// with no recovery path in the CLI. The ops that did decode are still the
// device's history, and every reader drops the same lines from the same bytes,
// so replay stays in agreement.
func Parse(data []byte) ([]Op, error) {
	var ops []Op
	for _, raw := range bytes.Split(data, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if len(line) == 0 {
			continue
		}
		// Decoded as pathRaw, not Op: same bytes, same result (see pathRaw.op),
		// without the second full scan an UnmarshalJSON method forces.
		var w pathRaw
		if err := json.Unmarshal(line, &w); err != nil {
			continue
		}
		op := w.op()
		// `null`, `{}` and any object with no kind decode without error and
		// are not operations. They must produce no op: op COUNTS are the sync
		// engine's cursors (pull's fresh[len(prev):], commit's seqBase), so a
		// line that yields a phantom op shifts every op after it for one
		// reader and not another — a divergence the writer chooses.
		if op.Kind != KindPut && op.Kind != KindDelete {
			continue
		}
		ops = append(ops, op)
	}
	return ops, nil
}

// ReadFile reads a journal file; a missing file is an empty journal.
func ReadFile(path string) ([]Op, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return Parse(data)
}

// Marshal encodes ops as JSONL, the journal wire format.
func Marshal(ops []Op) ([]byte, error) {
	var buf bytes.Buffer
	for _, op := range ops {
		b, err := json.Marshal(op)
		if err != nil {
			return nil, err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	return buf.Bytes(), nil
}

// Append appends ops to a journal file as JSONL.
func Append(path string, ops []Op) error {
	if len(ops) == 0 {
		return nil
	}
	data, err := Marshal(ops)
	if err != nil {
		return err
	}
	// 0600: a journal is the full path list, authorship and signed-in email
	// addresses of a private project, and it lives in $BDRIVE_HOME, whose
	// directories are 0755.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
