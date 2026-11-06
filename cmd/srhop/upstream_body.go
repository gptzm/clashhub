package main

import (
	"context"
	"errors"
	"io"
	"net"
)

// Keep read errors separate from io.Copy's destination-write errors. The
// wrapper intentionally exposes no WriterTo, so every upstream read is seen.
type upstreamBodyReader struct {
	reader io.Reader
	err    error
}

func (r *upstreamBodyReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil {
		r.err = err
	}
	return n, err
}

func isUpstreamBodyFailure(err error) bool {
	return err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}
