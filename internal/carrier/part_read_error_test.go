package carrier

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type failedPartReader struct{ err error }

func (r failedPartReader) Read([]byte) (int, error) { return 0, r.err }
func TestPartSignaturePreservesTransportErrors(t *testing.T) {
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded, io.ErrUnexpectedEOF} {
		_, err := OpenPart(failedPartReader{cause})
		if !errors.Is(err, cause) || errors.Is(err, ErrNotPNG) {
			t.Fatal("transport failure masked as corrupt PNG", cause, err)
		}
	}
	if _, err := OpenPart(bytes.NewReader([]byte("short"))); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("short stream misclassified", err)
	}
	if _, err := OpenPart(bytes.NewReader([]byte("not PNG!"))); !errors.Is(err, ErrNotPNG) {
		t.Fatal("confirmed signature mismatch not classified", err)
	}
}
