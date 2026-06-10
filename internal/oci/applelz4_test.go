package oci

import (
	"bytes"
	"os"
	"testing"
)

// TestAppleLZ4RealFixture decodes genuine Apple Compression-framework output
// (macOS compression_tool -encode -a lz4 on ix, 2026-06-09). The fixture's
// matches reach across block boundaries, so it fails against any decoder that
// treats bv41 blocks as independent — the exact bug the first real ghcr layer
// pull exposed.
func TestAppleLZ4RealFixture(t *testing.T) {
	enc, err := os.ReadFile("testdata/lz4-fixture.applelz4")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/lz4-fixture.raw")
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	n, err := appleLZ4Decode(&got, bytes.NewReader(enc))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n != int64(len(want)) {
		t.Fatalf("decoded %d bytes, want %d", n, len(want))
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatal("decoded bytes differ from the original")
	}
}
