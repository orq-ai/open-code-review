// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package tool

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestOverflowCapPassesThroughWithinLimit(t *testing.T) {
	store := NewOverflowStore(t.TempDir())
	small := strings.Repeat("a", MaxToolResultBytes)
	if got := store.Cap(small); got != small {
		t.Fatalf("result within the limit must pass through unchanged")
	}
}

func TestOverflowCapTruncatesAndNamesHandle(t *testing.T) {
	store := NewOverflowStore(t.TempDir())
	big := strings.Repeat("b", MaxToolResultBytes) + strings.Repeat("c", 5000)
	capped := store.Cap(big)
	if len(capped) >= len(big) {
		t.Fatalf("oversized result was not truncated: %d bytes", len(capped))
	}
	if !strings.Contains(capped, `handle "tr_1"`) {
		t.Fatalf("truncation note does not name the handle: %q", capped)
	}
}

func TestOverflowReadReturnsTheTail(t *testing.T) {
	store := NewOverflowStore(t.TempDir())
	tail := strings.Repeat("c", 5000)
	store.Cap(strings.Repeat("b", MaxToolResultBytes) + tail)

	out, err := NewOverflowRead(store).Execute(context.Background(), map[string]any{
		"handle": "tr_1",
		"offset": float64(MaxToolResultBytes),
	})
	if err != nil {
		t.Fatalf("tool_result_read: %v", err)
	}
	if !strings.Contains(out, tail) {
		t.Fatalf("tail not returned by tool_result_read")
	}
}

func TestOverflowReadRejectsHandlesThatAreNotTokens(t *testing.T) {
	p := NewOverflowRead(NewOverflowStore(t.TempDir()))
	for _, handle := range []string{"../../etc/passwd", "tr_1/../../etc/passwd", "tr_", "tr_1.txt", ""} {
		got, err := p.Execute(context.Background(), map[string]any{"handle": handle})
		if err != nil {
			t.Fatalf("handle %q: %v", handle, err)
		}
		if !strings.HasPrefix(got, "Error:") {
			t.Fatalf("handle %q was accepted: %q", handle, got)
		}
	}
}

func TestOverflowNilStoreIsTheNoOpConfiguration(t *testing.T) {
	var store *OverflowStore
	big := strings.Repeat("b", MaxToolResultBytes+1)
	if got := store.Cap(big); got != big {
		t.Fatalf("nil store must pass results through")
	}
}

func TestOverflowCloseRemovesSpilledResults(t *testing.T) {
	dir := t.TempDir() + "/spill"
	store := NewOverflowStore(dir)
	store.Cap(strings.Repeat("b", MaxToolResultBytes+1))
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("spill dir was not created: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("spill dir survived Close: %v", err)
	}
}

func TestOverflowSaveIsSafeUnderConcurrentCalls(t *testing.T) {
	// Reviews fan out to --concurrency 8 file groups sharing one store.
	store := NewOverflowStore(t.TempDir())
	big := strings.Repeat("b", MaxToolResultBytes+1)
	handles := make(chan string, 8)
	for range 8 {
		go func() {
			h, err := store.Save(big)
			if err != nil {
				t.Errorf("Save: %v", err)
			}
			handles <- h
		}()
	}
	seen := map[string]bool{}
	for range 8 {
		h := <-handles
		if seen[h] {
			t.Fatalf("handle %q issued twice", h)
		}
		seen[h] = true
	}
}

func TestTruncateAtRuneNeverEmptiesNonUTF8Input(t *testing.T) {
	// git grep emits whatever bytes a file holds; a run of continuation bytes
	// must not walk the cut all the way back to nothing.
	s := strings.Repeat("\x80", 100)
	if got := truncateAtRune(s, 50); len(got) != 47 {
		t.Fatalf("expected the back-off to stop after 3 bytes, got %d", len(got))
	}
	if got := truncateAtRune("héllo", 2); got != "h" {
		t.Fatalf("multi-byte rune was split: %q", got)
	}
}
