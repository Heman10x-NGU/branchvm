package types

import (
	"bytes"
	"encoding/binary"
	"io"
	"strings"
	"testing"
)

func TestPTYFrameRoundtrip(t *testing.T) {
	cases := []struct {
		name    string
		kind    byte
		payload []byte
	}{
		{"data", PTYFrameData, []byte("ls -la\n")},
		{"empty", PTYFrameData, nil},
		{"resize", PTYFrameResize, []byte{0, 40, 0, 120}},
		{"exit", PTYFrameExit, []byte{0, 0, 0, 2}},
		{"max", PTYFrameData, bytes.Repeat([]byte("a"), PTYMaxFrameBytes)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WritePTYFrame(&buf, tc.kind, tc.payload); err != nil {
				t.Fatalf("WritePTYFrame: %v", err)
			}
			gotKind, gotPayload, err := ReadPTYFrame(&buf)
			if err != nil {
				t.Fatalf("ReadPTYFrame: %v", err)
			}
			if gotKind != tc.kind {
				t.Fatalf("kind = %d, want %d", gotKind, tc.kind)
			}
			if !bytes.Equal(gotPayload, tc.payload) {
				t.Fatalf("payload mismatch: got %d bytes, want %d bytes", len(gotPayload), len(tc.payload))
			}
		})
	}
}

func TestPTYFrameBoundsAndTruncated(t *testing.T) {
	tooBig := make([]byte, PTYMaxFrameBytes+1)
	var buf bytes.Buffer
	if err := WritePTYFrame(&buf, PTYFrameData, tooBig); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("WritePTYFrame oversized err = %v, want too large", err)
	}

	var hdr [5]byte
	hdr[0] = PTYFrameData
	binary.BigEndian.PutUint32(hdr[1:], PTYMaxFrameBytes+1)
	if _, _, err := ReadPTYFrame(bytes.NewReader(hdr[:])); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("ReadPTYFrame oversized err = %v, want too large", err)
	}

	if _, _, err := ReadPTYFrame(bytes.NewReader([]byte{1, 0})); err != io.ErrUnexpectedEOF {
		t.Fatalf("ReadPTYFrame truncated hdr err = %v, want ErrUnexpectedEOF", err)
	}
}
