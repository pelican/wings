package plugin

import (
	"bytes"
	"strings"
	"testing"
)

func TestCopyFileLimitedRejectsOversizedInput(t *testing.T) {
	var dst bytes.Buffer
	err := copyFileLimited(&dst, strings.NewReader("abcd"), 3)
	if err == nil {
		t.Fatal("expected oversized file to be rejected")
	}
}

func TestCopyFileLimitedAllowsMaxSizedInput(t *testing.T) {
	var dst bytes.Buffer
	if err := copyFileLimited(&dst, strings.NewReader("abc"), 3); err != nil {
		t.Fatalf("copyFileLimited: %v", err)
	}
	if got := dst.String(); got != "abc" {
		t.Fatalf("copied %q, want abc", got)
	}
}
