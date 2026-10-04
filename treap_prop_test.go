package engine

import (
	"fmt"
	"sort"
	"testing"
)

func TestTreapIterateAll(t *testing.T) {
	tr := newTreap()
	for i, s := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		tr = tr.upsert([]byte(fmt.Sprintf("%02d-%s", i, s)), []byte{byte(i)})
	}
	var got []string
	tr.ascIter(nil, func(k, v []byte) bool {
		got = append(got, string(k))
		return false
	})
	if len(got) != 10 {
		t.Fatalf("iterated %d keys: %v", len(got), got)
	}
	if !sort.StringsAreSorted(got) {
		t.Fatalf("not sorted: %v", got)
	}
	// uniqueness check
	seen := map[string]bool{}
	for _, g := range got {
		if seen[g] {
			t.Fatalf("duplicate key iterated: %q; all=%v", g, got)
		}
		seen[g] = true
	}
}
