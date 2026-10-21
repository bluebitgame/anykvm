package pixelops

import (
	"reflect"
	"testing"
)

func TestDirtyBlockCandidates(t *testing.T) {
	const w, h = 130, 65
	prev, cur := make([]byte, w*h*4), make([]byte, w*h*4)
	cur[0] = 20
	cur[(64*w+129)*4] = 20
	all := DirtyBlocks(prev, cur, w, h, 0)
	for _, ids := range [][]uint32{{999}, {5, 0}, {0, 0}, {^uint32(0)}} {
		if got := DirtyBlocksCandidates(prev, cur, w, h, 0, ids); !reflect.DeepEqual(got, all) {
			t.Fatalf("unsafe IDs %v: %v", ids, got)
		}
	}
	if got := DirtyBlocksCandidates(prev, cur, w, h, 0, []uint32{5}); !reflect.DeepEqual(got, []Block{{128, 64, 2, 1}}) {
		t.Fatalf("edge candidate: %v", got)
	}
	if got := DirtyBlocksCandidates(prev, cur, w, h, 0, nil); len(got) != 0 {
		t.Fatal("empty candidates compared")
	}
	if got := DirtyBlocksCandidates(nil, cur, w, h, 0, nil); len(got) != 6 {
		t.Fatal("missing baseline did not force refresh")
	}
}
