//go:build linux

package storage

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResidentExtents(t *testing.T) {
	cases := []struct {
		name   string
		bitmap []byte
		pages  int64
		size   int64
		want   []extent
	}{
		{"empty", []byte{0}, 8, 8 * 4096, nil},
		{"all", []byte{0xff}, 8, 8 * 4096, []extent{{0, 8 * 4096}}},
		{"runs", []byte{0b1011_0011}, 8, 8 * 4096, []extent{{0, 2 * 4096}, {4 * 4096, 2 * 4096}, {7 * 4096, 4096}}},
		{"tail clamp", []byte{0b1000_0000}, 8, 7*4096 + 100, []extent{{7 * 4096, 100}}},
		{"cross byte", []byte{0x80, 0x01}, 9, 9 * 4096, []extent{{7 * 4096, 2 * 4096}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := residentExtents(c.bitmap, c.pages, 4096, c.size); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestWorkingSetRoundTrip writes a file through the page cache, so every
// page is resident, records it, and prefaults it back.
func TestWorkingSetRoundTrip(t *testing.T) {
	dir := t.TempDir()
	mem, ws := filepath.Join(dir, "mem"), filepath.Join(dir, "ws.bin")
	ps := int64(os.Getpagesize())
	size := 5*ps + 123
	if err := os.WriteFile(mem, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := RecordWorkingSet(mem, ws)
	if err != nil {
		t.Fatal(err)
	}
	if got.PageSize != ps || got.Pages != 6 || got.Resident < 1 || got.Resident > 6 {
		t.Fatalf("unexpected working set %+v", got)
	}
	read, _, err := ReadWorkingSet(ws)
	if err != nil || read != got {
		t.Fatalf("ReadWorkingSet = %+v, %v; want %+v", read, err, got)
	}
	advised, err := Prefault(mem, ws)
	if err != nil {
		t.Fatal(err)
	}
	if advised <= 0 || advised > size {
		t.Fatalf("advised %d bytes of a %d byte file", advised, size)
	}
	if err := os.WriteFile(mem, make([]byte, size+8*ps), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Prefault(mem, ws); err == nil {
		t.Fatal("Prefault accepted a working set recorded for a different file size")
	}
}

func TestReadWorkingSetRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ws.bin")
	if err := os.WriteFile(p, []byte("not a working set file at all...."), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadWorkingSet(p); err == nil {
		t.Fatal("garbage accepted")
	}
}
