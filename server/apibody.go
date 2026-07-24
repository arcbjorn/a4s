package server

import (
	"bytes"
	"context"
	"io"
)

// bodyKey carries the already-read request body through the request context.
//
// The body must be read in full before authentication, because the signature
// covers its digest. Handlers therefore cannot read it again from the request,
// and passing it through the context keeps the handler signature unchanged
// while making it impossible for a handler to accidentally act on bytes that
// were never covered by a signature.
type bodyKey struct{}

func withBody(ctx context.Context, body []byte) context.Context {
	return context.WithValue(ctx, bodyKey{}, body)
}

func bodyFrom(ctx context.Context) []byte {
	body, _ := ctx.Value(bodyKey{}).([]byte)
	return body
}

func newReader(body []byte) io.Reader { return bytes.NewReader(body) }
