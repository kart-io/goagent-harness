package opensandbox

import (
	"errors"
	"io"
	"net/http"
)

var errResponseLimit = errors.New("OpenSandbox RPC response exceeds configured limit")

type responseLimitTransport struct {
	base  http.RoundTripper
	limit int64
}

func (t *responseLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	response.Body = &limitedBody{ReadCloser: response.Body, left: t.limit}
	return response, nil
}

type limitedBody struct {
	io.ReadCloser
	left int64
}

func (b *limitedBody) Read(p []byte) (int, error) {
	if b.left < 0 {
		return 0, errResponseLimit
	}
	if int64(len(p)) > b.left+1 {
		p = p[:int(b.left+1)]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	if b.left < 0 {
		return n, errResponseLimit
	}
	return n, err
}
