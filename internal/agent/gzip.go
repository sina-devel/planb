package agent

import (
	"compress/gzip"
	"io"
)

// gzipReader returns a reader yielding the gzip-compressed content of r.
func gzipReader(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		gw := gzip.NewWriter(pw)
		_, err := io.Copy(gw, r)
		if cerr := gw.Close(); err == nil {
			err = cerr
		}
		_ = pw.CloseWithError(err)
	}()
	return pr
}

func gunzipReader(r io.Reader) (io.ReadCloser, error) {
	return gzip.NewReader(r)
}
