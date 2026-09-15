// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 alibaba/open-code-review Contributors

package tool

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// handlePattern is the exact shape Save emits. Matching the whole token rather
// than rejecting path characters keeps the accepted set closed.
var handlePattern = regexp.MustCompile(`^tr_[0-9]+$`)

// MaxToolResultBytes is the largest tool result handed to the model in one
// message. A single matched line has no length bound — `git grep` prints
// whatever the file holds, and generated reports or one-line JSON datasets
// reach megabytes — so without a byte cap one tool call can fill the whole
// context window and stop the review.
//
// The budget this protects (template.MaxTokens) is denominated in tokens, this
// cap in bytes. 60,000 bytes is 20,000 tokens at the ~3 bytes/token that code
// and JSON tokenize to; prose runs nearer 4, so the byte figure errs on the
// side of fewer tokens than the target.
const MaxToolResultBytes = 60_000

// overflowReadMaxBytes bounds one tool_result_read window. Kept below
// MaxToolResultBytes so a read never itself overflows.
const overflowReadMaxBytes = 30_000

// OverflowStore persists tool results that exceeded MaxToolResultBytes so the
// model can page through them instead of losing them. Files land outside the
// repository: the review reads files at a git ref, so a scratch file in the
// worktree would be invisible to file_read anyway, and writing into the repo
// under review would pollute the diff.
type OverflowStore struct {
	dir     string
	n       atomic.Int64
	dirOnce sync.Once
	dirErr  error
}

// NewOverflowStore returns a store writing under dir. dir is created lazily on
// the first Save.
func NewOverflowStore(dir string) *OverflowStore { return &OverflowStore{dir: dir} }

// Close removes the spilled results. Handles do not outlive the run that
// created them, and a reused runner would otherwise accumulate one directory
// of megabyte-scale files per review.
func (s *OverflowStore) Close() error { return os.RemoveAll(s.dir) }

// Save writes text and returns the handle used to read it back.
func (s *OverflowStore) Save(text string) (string, error) {
	s.dirOnce.Do(func() { s.dirErr = os.MkdirAll(s.dir, 0o700) })
	if s.dirErr != nil {
		return "", fmt.Errorf("create overflow dir: %w", s.dirErr)
	}
	handle := fmt.Sprintf("tr_%d", s.n.Add(1))
	if err := os.WriteFile(filepath.Join(s.dir, handle+".txt"), []byte(text), 0o600); err != nil {
		return "", fmt.Errorf("write overflow file: %w", err)
	}
	return handle, nil
}

// Cap returns result unchanged when it fits, and otherwise the leading
// MaxToolResultBytes bytes plus a note naming the handle that holds the rest.
// A store that fails to save still yields a truncated result: losing the tail
// beats stopping the review.
func (s *OverflowStore) Cap(result string) string {
	if s == nil || len(result) <= MaxToolResultBytes {
		return result
	}
	head := truncateAtRune(result, MaxToolResultBytes)
	handle, err := s.Save(result)
	if err != nil {
		return head + fmt.Sprintf("\n\n[TRUNCATED: %d bytes total, %d shown. The rest could not be saved (%v). Narrow the call — for code_search pass file_patterns, for file_read a smaller line range.]\n", len(result), len(head), err)
	}
	return head + fmt.Sprintf("\n\n[TRUNCATED: %d bytes total, %d shown. The full output is saved as handle %q — read further with tool_result_read(handle=%q, offset=%d). Prefer narrowing the call: for code_search pass file_patterns, for file_read a smaller line range.]\n", len(result), len(head), handle, handle, len(head))
}

// truncateAtRune cuts s to at most n bytes without splitting a UTF-8 rune.
//
// git grep matches whatever bytes a file holds, so s is not necessarily valid
// UTF-8. A well-formed rune is at most 4 bytes; backing off further would walk
// a long run of continuation bytes down to an empty string, which breaks the
// byte count this function promises far worse than a split rune does.
func truncateAtRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for limit := 0; limit < 3 && n > 0 && s[n]&0xC0 == 0x80; limit++ {
		n--
	}
	return s[:n]
}

// OverflowReadProvider implements tool_result_read: a byte-range read over a
// result the model already saw the head of.
type OverflowReadProvider struct {
	Store *OverflowStore
}

func NewOverflowRead(s *OverflowStore) *OverflowReadProvider { return &OverflowReadProvider{Store: s} }

func (p *OverflowReadProvider) Tool() Tool { return ToolResultRead }

func (p *OverflowReadProvider) Execute(_ context.Context, args map[string]any) (string, error) {
	handle, _ := args["handle"].(string)
	if handle == "" {
		return "Error: handle is required", nil
	}
	// The handle names a file in the store's own directory; anything that is
	// not a bare tr_<n> token is a path traversal attempt, not a typo.
	if !handlePattern.MatchString(handle) {
		return fmt.Sprintf("Error: unknown handle %q", handle), nil
	}
	data, err := os.ReadFile(filepath.Join(p.Store.dir, handle+".txt"))
	if err != nil {
		return fmt.Sprintf("Error: handle %q is not available", handle), nil
	}

	offset := 0
	if v, ok := args["offset"].(float64); ok && v > 0 {
		offset = int(v)
	}
	if offset >= len(data) {
		return fmt.Sprintf("Handle %s: offset %d is past the end (%d bytes total).\n", handle, offset, len(data)), nil
	}
	limit := overflowReadMaxBytes
	if v, ok := args["limit"].(float64); ok && v > 0 && int(v) < limit {
		limit = int(v)
	}
	window := truncateAtRune(string(data[offset:]), limit)
	next := offset + len(window)

	var sb strings.Builder
	fmt.Fprintf(&sb, "Handle: %s (Total bytes: %d)\nBYTE_RANGE: %d-%d\n", handle, len(data), offset, next)
	sb.WriteString(window)
	if next < len(data) {
		fmt.Fprintf(&sb, "\n\n[%d bytes remain; continue with offset=%d.]\n", len(data)-next, next)
	}
	return sb.String(), nil
}
