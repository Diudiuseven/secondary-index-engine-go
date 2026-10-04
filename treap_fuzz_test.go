package engine

import (
	"fmt"
	"math/rand"
	"testing"
)

func TestTreapRandomOps(t *testing.T) {
	tr := newTreap()
	ref := map[string]string{}
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 20000; i++ {
		k := fmt.Sprintf("k-%d", rng.Intn(500))
		switch rng.Intn(3) {
		case 0, 1:
			v := fmt.Sprintf("v-%d", i)
			tr = tr.upsert([]byte(k), []byte(v))
			ref[k] = v
		case 2:
			tr = tr.delete([]byte(k))
			delete(ref, k)
		}
		// snapshot old root still valid (persistence).
	}
	if tr.len() != len(ref) {
		t.Fatalf("len %d vs ref %d", tr.len(), len(ref))
	}
	got := map[string]string{}
	tr.ascIter(nil, func(k, v []byte) bool {
		got[string(k)] = string(v)
		return false
	})
	if len(got) != len(ref) {
		t.Fatalf("iter count %d vs %d", len(got), len(ref))
	}
	for k, v := range ref {
		if got[k] != v {
			t.Fatalf("mismatch %s: %s vs %s", k, got[k], v)
		}
	}
	// range scans
	for trial := 0; trial < 100; trial++ {
		lo := fmt.Sprintf("k-%d", rng.Intn(500))
		var keys []string
		tr.ascIter([]byte(lo), func(k, v []byte) bool {
			if string(k) < lo {
				t.Fatal("range lower bound violated")
			}
			keys = append(keys, string(k))
			return len(keys) >= 10
		})
		for j := 1; j < len(keys); j++ {
			if keys[j-1] >= keys[j] {
				t.Fatalf("range order broken: %v", keys)
			}
		}
	}
}
