package oci

import (
	"compress/gzip"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const (
	MediaTypeGitPackfile     = "application/vnd.git.repository.packfile.v1"
	MediaTypeGitPackfileGzip = "application/vnd.git.repository.packfile.v1+gzip"
	MediaTypeGitPackfileZstd = "application/vnd.git.repository.packfile.v1+zstd"
)

var (
	gzipWriterPool = sync.Pool{
		New: func() any {
			return gzip.NewWriter(io.Discard)
		},
	}

	zstdEncoderPool = sync.Pool{
		New: func() any {
			zw, _ := zstd.NewWriter(io.Discard,
				zstd.WithEncoderConcurrency(runtime.NumCPU()),
				zstd.WithEncoderLevel(zstd.SpeedFastest),
			)
			return zw
		},
	}
)

// getGzipWriter returns a pooled gzip.Writer already reset to w.
func getGzipWriter(w io.Writer) *gzip.Writer {
	gw, ok := gzipWriterPool.Get().(*gzip.Writer)
	if !ok {
		gw = gzip.NewWriter(w)
		return gw
	}
	gw.Reset(w)
	return gw
}

// getZstdEncoder returns a pooled zstd.Encoder already reset to w.
func getZstdEncoder(w io.Writer) *zstd.Encoder {
	zw, ok := zstdEncoderPool.Get().(*zstd.Encoder)
	if !ok {
		zw, _ = zstd.NewWriter(w,
			zstd.WithEncoderConcurrency(runtime.NumCPU()),
			zstd.WithEncoderLevel(zstd.SpeedFastest),
		)
		return zw
	}
	zw.Reset(w)
	return zw
}

type pooledGzipWriter struct {
	gw *gzip.Writer
}

func (p *pooledGzipWriter) Write(b []byte) (int, error) {
	return p.gw.Write(b)
}

func (p *pooledGzipWriter) Close() error {
	err := p.gw.Close()
	gzipWriterPool.Put(p.gw)
	return err
}

type pooledZstdWriter struct {
	zw *zstd.Encoder
}

func (p *pooledZstdWriter) Write(b []byte) (int, error) {
	return p.zw.Write(b)
}

func (p *pooledZstdWriter) Close() error {
	err := p.zw.Close()
	zstdEncoderPool.Put(p.zw)
	return err
}

type nopWriteCloser struct {
	io.Writer
}

func (n *nopWriteCloser) Close() error {
	return nil
}

// compressedMediaType reports the layer media type a compression mode produces,
// without creating a writer. Callers that must know the media type before they
// begin streaming need it separately from CompressStream.
func compressedMediaType(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "gzip":
		return MediaTypeGitPackfileGzip, nil
	case "zstd":
		return MediaTypeGitPackfileZstd, nil
	case "none", "raw", "":
		return MediaTypeGitPackfile, nil
	default:
		return "", fmt.Errorf("unsupported compression mode: %s", mode)
	}
}

// CompressStream wraps an io.Writer with a pooled compression writer for the specified mode.
func CompressStream(w io.Writer, mode string) (io.WriteCloser, string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "gzip":
		return &pooledGzipWriter{gw: getGzipWriter(w)}, MediaTypeGitPackfileGzip, nil

	case "zstd":
		return &pooledZstdWriter{zw: getZstdEncoder(w)}, MediaTypeGitPackfileZstd, nil

	case "none", "raw", "":
		return &nopWriteCloser{Writer: w}, MediaTypeGitPackfile, nil

	default:
		return nil, "", fmt.Errorf("unsupported compression mode: %s", mode)
	}
}

// decompReadCloser pairs a decompressing reader with the underlying stream.
//
// Both must be closed. zstd.NewReader starts worker goroutines that are only
// released by closing the decoder, so closing just the underlying stream leaks
// them once per fetched packfile; gzip's Close is what verifies the trailing
// CRC and length, so skipping it silently accepts a truncated or corrupt
// stream.
type decompReadCloser struct {
	r           io.Reader
	c           io.Closer
	closeDecomp func() error
}

func (d *decompReadCloser) Read(p []byte) (int, error) {
	return d.r.Read(p)
}

func (d *decompReadCloser) Close() error {
	var decompErr error
	if d.closeDecomp != nil {
		decompErr = d.closeDecomp()
	}
	closeErr := d.c.Close()
	if decompErr != nil {
		return decompErr
	}
	return closeErr
}

// DecompressStream wraps an io.ReadCloser with an automatic decompressor reader based on media type.
func DecompressStream(rc io.ReadCloser, mediaType string) (io.ReadCloser, error) {
	cleanType := strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0]))

	if cleanType == MediaTypeGitPackfileGzip {
		gr, err := gzip.NewReader(rc)
		if err != nil {
			_ = rc.Close()
			return nil, fmt.Errorf("failed to create gzip reader: %w", err)
		}
		return &decompReadCloser{r: gr, c: rc, closeDecomp: gr.Close}, nil
	}

	if cleanType == MediaTypeGitPackfileZstd {
		zr, err := zstd.NewReader(rc)
		if err != nil {
			_ = rc.Close()
			return nil, fmt.Errorf("failed to create zstd reader: %w", err)
		}
		// zstd.Decoder.Close has no error to report, but it is what releases
		// the decoder's worker goroutines.
		return &decompReadCloser{r: zr, c: rc, closeDecomp: func() error { zr.Close(); return nil }}, nil
	}

	return rc, nil
}
