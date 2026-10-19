package capture

import (
	"bytes"
	"testing"
)

func sc(b ...byte) []byte { return append([]byte{0, 0, 0, 1}, b...) }

func TestAUParserGroupsAccessUnits(t *testing.T) {
	sps := sc(0x67, 0x42, 0x00, 0x0a)
	pps := sc(0x68, 0xce, 0x38, 0x80)
	idr := sc(0x65, 0x11, 0x22)
	sl1 := sc(0x41, 0x33)
	sl2 := sc(0x41, 0x44)

	var stream []byte
	for _, n := range [][]byte{sps, pps, idr, sl1, sl2} {
		stream = append(stream, n...)
	}
	stream = append(stream, 0, 0, 0, 1, 0x41, 0x55) // trailing start code to flush sl2

	run := func(name string, chunk int) {
		p := &AUParser{}
		var aus [][]byte
		for i := 0; i < len(stream); i += chunk {
			end := i + chunk
			if end > len(stream) {
				end = len(stream)
			}
			aus = append(aus, p.Push(stream[i:end])...)
		}
		if len(aus) != 3 {
			t.Fatalf("%s: got %d AUs, want 3", name, len(aus))
		}
		types := func(au []byte) []byte {
			var got []byte
			for _, nal := range splitNALs(au) {
				got = append(got, nalType(nal))
			}
			return got
		}
		if got := types(aus[0]); !bytes.Equal(got, []byte{7, 8, 5}) {
			t.Fatalf("%s: AU0 types = %v, want [7 8 5]", name, got)
		}
		if got := types(aus[1]); !bytes.Equal(got, []byte{1}) {
			t.Fatalf("%s: AU1 types = %v, want [1]", name, got)
		}
		if got := types(aus[2]); !bytes.Equal(got, []byte{1}) {
			t.Fatalf("%s: AU2 types = %v, want [1]", name, got)
		}
	}
	run("whole", len(stream))
	run("byte-by-byte", 1)
	run("chunk3", 3)
}

// TestAUParserFlushPending verifies a complete but not-yet-delimited access
// unit (no trailing start code) is emitted on demand, as used by the idle-flush
// low-latency path.
func TestAUParserFlushPending(t *testing.T) {
	sps := sc(0x67, 0x42, 0x00, 0x0a)
	pps := sc(0x68, 0xce, 0x38, 0x80)
	idr := sc(0x65, 0x11, 0x22)

	p := &AUParser{}
	if aus := p.Push(append(append(append([]byte{}, sps...), pps...), idr...)); len(aus) != 0 {
		t.Fatalf("push emitted %d AUs prematurely, want 0", len(aus))
	}
	au := p.FlushPending()
	if au == nil {
		t.Fatal("flushPending returned nil, want the buffered IDR access unit")
	}
	var got []byte
	for _, nal := range splitNALs(au) {
		got = append(got, nalType(nal))
	}
	if len(got) != 3 || got[0] != 7 || got[1] != 8 || got[2] != 5 {
		t.Fatalf("flushed AU types = %v, want [7 8 5]", got)
	}
	// After flushing, a subsequent P-frame parses cleanly.
	p.Push(sc(0x41, 0x33))
	if au2 := p.FlushPending(); au2 == nil {
		t.Fatal("flushPending returned nil for buffered P-frame")
	}
}

func TestDropArgPair(t *testing.T) {
	in := []string{"-f", "avfoundation", "-framerate", "60", "-i", "1:none", "-r", "60"}
	out, changed := dropArgPair(in, "-framerate")
	if !changed {
		t.Fatal("expected changed=true")
	}
	want := []string{"-f", "avfoundation", "-i", "1:none", "-r", "60"}
	if len(out) != len(want) || !containsSeq(out, want...) {
		t.Fatalf("dropArgPair = %v, want %v", out, want)
	}
	if _, changed := dropArgPair([]string{"-i", "x"}, "-framerate"); changed {
		t.Fatal("expected changed=false when flag absent")
	}
}

// --- test helpers ---

func splitNALs(b []byte) [][]byte {
	var out [][]byte
	start := findStartCode(b, 0)
	if start < 0 {
		return out
	}
	for {
		next := findStartCode(b, start+3)
		if next < 0 {
			out = append(out, b[start:])
			return out
		}
		out = append(out, b[start:next])
		start = next
	}
}

func containsSeq(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		match := true
		for j := range seq {
			if args[i+j] != seq[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
