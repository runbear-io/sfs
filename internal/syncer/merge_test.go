package syncer

import "testing"

func TestMergeText(t *testing.T) {
	for _, c := range []struct {
		name             string
		base, a, b, want string
		ok               bool
	}{{
		name: "disjoint edits in different parts of the file",
		base: "one\ntwo\nthree\nfour\nfive\n",
		a:    "ONE\ntwo\nthree\nfour\nfive\n",
		b:    "one\ntwo\nthree\nfour\nFIVE\n",
		want: "ONE\ntwo\nthree\nfour\nFIVE\n",
		ok:   true,
	}, {
		name: "one side appends, the other edits the top",
		base: "title\nbody\n",
		a:    "TITLE\nbody\n",
		b:    "title\nbody\nappended\n",
		want: "TITLE\nbody\nappended\n",
		ok:   true,
	}, {
		name: "only one side changed anything",
		base: "a\nb\n",
		a:    "a\nb\n",
		b:    "a\nB\n",
		want: "a\nB\n",
		ok:   true,
	}, {
		name: "both sides rewrote the same line",
		base: "a\nb\nc\n",
		a:    "a\nX\nc\n",
		b:    "a\nY\nc\n",
		ok:   false,
	}, {
		name: "both sides inserted at the same point",
		base: "a\nc\n",
		a:    "a\nFROM-A\nc\n",
		b:    "a\nFROM-B\nc\n",
		ok:   false,
	}, {
		name: "one side deleted a region the other left alone",
		base: "keep\ndrop\nkeep2\n",
		a:    "keep\nkeep2\n",
		b:    "keep\ndrop\nkeep2\nadded\n",
		want: "keep\nkeep2\nadded\n",
		ok:   true,
	}} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := mergeText([]byte(c.base), []byte(c.a), []byte(c.b))
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (got %q)", ok, c.ok, got)
			}
			if ok && string(got) != c.want {
				t.Fatalf("merged = %q, want %q", got, c.want)
			}
		})
	}
}

// Two devices can each observe the same concurrency and merge independently.
// If merge(base, a, b) and merge(base, b, a) disagreed they would push two
// different blobs, and the fleet would keep whichever won last-writer-wins —
// silently losing one side's work. This is the property that makes merging
// safe to do on more than one device at a time.
func TestMergeIsOrderIndependent(t *testing.T) {
	base := []byte("one\ntwo\nthree\nfour\nfive\nsix\n")
	a := []byte("ONE\ntwo\nthree\nfour\nfive\nsix\n")
	b := []byte("one\ntwo\nthree\nfour\nfive\nSIX\n")

	ab, ok1 := mergeText(base, a, b)
	ba, ok2 := mergeText(base, b, a)
	if !ok1 || !ok2 {
		t.Fatalf("both directions should merge: %v %v", ok1, ok2)
	}
	if string(ab) != string(ba) {
		t.Fatalf("order changed the result:\n a,b = %q\n b,a = %q", ab, ba)
	}
}

// A file with no trailing newline must not silently grow one, and one with a
// trailing newline must keep it: either would be a spurious diff on every peer.
func TestMergePreservesTrailingNewline(t *testing.T) {
	got, ok := mergeText([]byte("a\nb"), []byte("A\nb"), []byte("a\nb"))
	if !ok || string(got) != "A\nb" {
		t.Fatalf("no-trailing-newline: %q ok=%v", got, ok)
	}
	got, ok = mergeText([]byte("a\nb\n"), []byte("A\nb\n"), []byte("a\nb\n"))
	if !ok || string(got) != "A\nb\n" {
		t.Fatalf("trailing newline: %q ok=%v", got, ok)
	}
}

func TestMergeEmptyAndWhole(t *testing.T) {
	// One side emptied the file, the other edited it: a real conflict.
	if _, ok := mergeText([]byte("a\nb\n"), []byte(""), []byte("a\nB\n")); ok {
		t.Error("emptying a file the other side edited should not merge")
	}
	// Two different bodies from an empty base overlap by construction.
	if _, ok := mergeText([]byte(""), []byte("x\n"), []byte("y\n")); ok {
		t.Error("two different bodies from an empty base should not merge")
	}
}
