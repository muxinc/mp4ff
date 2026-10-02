package mp4

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"testing"

	"github.com/Eyevinn/mp4ff/bits"
)

type countedReadSeeker struct {
	io.ReadSeeker
	read int
}

func (r *countedReadSeeker) Read(p []byte) (int, error) {
	n, err := r.ReadSeeker.Read(p)
	r.read += n
	return n, err
}

func TestLazySkippedBoxes(t *testing.T) {
	for _, name := range []string{"mdat", "free", "skip"} {
		for _, size := range []uint64{8, 9, 24, 20 << 30} {
			t.Run(name+"/"+strconv.FormatUint(size, 10), func(t *testing.T) {
				f, err := os.CreateTemp(t.TempDir(), "sparse-*.mp4")
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				large := size > math.MaxUint32
				if err := EncodeHeaderWithSize(name, size, large, f); err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(int64(size)); err != nil {
					t.Fatal(err)
				}
				if _, err := f.Seek(int64(size), io.SeekStart); err != nil {
					t.Fatal(err)
				}
				// A following mdat verifies both physical and logical positions.
				if err := EncodeHeaderWithSize("mdat", 8, false, f); err != nil {
					t.Fatal(err)
				}
				if _, err := f.Seek(0, io.SeekStart); err != nil {
					t.Fatal(err)
				}
				r := &countedReadSeeker{ReadSeeker: f}
				parsed, err := DecodeFile(r, WithDecodeMode(DecModeLazyMdat))
				if err != nil {
					t.Fatal(err)
				}
				if len(parsed.Children) != 2 || parsed.Children[0].Size() != size {
					t.Fatalf("incorrect children or skipped size: %+v", parsed.Children)
				}
				if got := parsed.Children[1].(*MdatBox).StartPos; got != size {
					t.Fatalf("following box position = %d, want %d", got, size)
				}
				wantRead := 16
				if large {
					wantRead += 8
				}
				if r.read != wantRead {
					t.Fatalf("read %d bytes, want only %d header bytes", r.read, wantRead)
				}
				if padding, ok := parsed.Children[0].(*FreeBox); ok {
					if len(padding.notDecoded) != 0 {
						t.Fatal("padding payload retained")
					}
					if err := padding.Encode(io.Discard); err == nil {
						t.Fatal("encoding discarded padding should fail")
					}
					if err := padding.EncodeSW(bits.NewFixedSliceWriter(16)); err == nil {
						t.Fatal("slice encoding discarded padding should fail")
					}
				} else if len(parsed.Children[0].(*MdatBox).Data) != 0 {
					t.Fatal("mdat payload retained")
				}
			})
		}
	}
}

func TestLazySkippedBoxInvalidSizes(t *testing.T) {
	for _, name := range []string{"mdat", "free", "skip"} {
		for _, size := range []uint64{0, 7, 15, 17, math.MaxInt64, uint64(math.MaxInt64) + 1, math.MaxUint64 - 51342830} {
			header := make([]byte, 16)
			binary.BigEndian.PutUint32(header, 1)
			copy(header[4:8], name)
			binary.BigEndian.PutUint64(header[8:], size)
			if _, err := DecodeFile(bytes.NewReader(header), WithDecodeMode(DecModeLazyMdat)); err == nil {
				t.Errorf("%s size %d: expected error", name, size)
			}
		}
	}
}

// operationLimitedReader prevents a regression from turning this reproducer
// into an allocating infinite loop. The test requires the size error, not this
// guard's error, so hitting the guard cannot produce a false pass.
type operationLimitedReader struct {
	*bytes.Reader
	operations int
}

func (r *operationLimitedReader) Read(p []byte) (int, error) {
	r.operations++
	if r.operations > 32 {
		return 0, fmt.Errorf("reproducer operation limit exceeded")
	}
	return r.Reader.Read(p)
}

func (r *operationLimitedReader) Seek(offset int64, whence int) (int64, error) {
	r.operations++
	if r.operations > 32 {
		return 0, fmt.Errorf("reproducer operation limit exceeded")
	}
	return r.Reader.Seek(offset, whence)
}

func TestDecodeFileNegativeMdatRewindsOntoFree(t *testing.T) {
	const freeSize = 64 << 10
	var data bytes.Buffer
	if err := EncodeHeaderWithSize("ftyp", 16, false, &data); err != nil {
		t.Fatal(err)
	}
	data.WriteString("mp42isom")
	freeStart := data.Len()
	if err := EncodeHeaderWithSize("free", freeSize, false, &data); err != nil {
		t.Fatal(err)
	}
	data.Write(make([]byte, freeSize-8))
	mdatStart := data.Len()
	largeSize := uint64(int64(freeStart - mdatStart))
	if err := EncodeHeaderWithSize("mdat", largeSize, true, &data); err != nil {
		t.Fatal(err)
	}
	r := &operationLimitedReader{Reader: bytes.NewReader(data.Bytes())}
	_, err := DecodeFile(r, WithDecodeMode(DecModeLazyMdat))
	want := fmt.Sprintf("decode box %q: invalid box size %d", "mdat", largeSize)
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %s", err, want)
	}
	t.Logf("input: %d bytes; operations: %d; error: %v", data.Len(), r.operations, err)
}
