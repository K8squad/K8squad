package workitemindex

import (
	"strings"
	"testing"
)

// TestIndexedContent_BoundsCommentTail (ISI-7228 GAP-2): the comment thread's contribution to the
// one-per-item embedding is tail-bounded — an oversized thread keeps the MOST RECENT comments,
// marks the elision honestly, and never feeds a 400K-char run-log blob to the embedder.
func TestIndexedContent_BoundsCommentTail(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		b.WriteString("author: padded comment line number padded padded padded ")
		b.WriteString(itoa(i))
		b.WriteString(" END")
		if i != 1999 {
			b.WriteString("\n")
		}
	}
	body := b.String()
	if len(body) <= maxCommentTailChars {
		t.Fatalf("fixture too small: %d bytes", len(body))
	}
	out := indexedContent(WorkItemIndexable{Title: "t", Body: "b", CommentBodies: body})
	if !strings.Contains(out, "[earlier comments truncated]") {
		t.Fatalf("elision marker missing")
	}
	if !strings.HasSuffix(out, "1999 END") {
		t.Fatalf("tail must keep the MOST RECENT comment")
	}
	if i := strings.Index(out, "[comments]"); i < 0 || len(out)-i > maxCommentTailChars+64 {
		t.Fatalf("comment section unbounded: %d bytes after marker", len(out)-i)
	}
}

// itoa avoids strconv in the fixture for no reason other than variety.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for i > 0 {
		d = append([]byte{byte('0' + i%10)}, d...)
		i /= 10
	}
	return string(d)
}
