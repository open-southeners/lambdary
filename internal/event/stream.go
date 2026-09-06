package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// frameDelimiter is the eight-NUL sequence awslambda.streamifyResponse's
// runtime framing inserts between a streaming response's JSON prelude and
// its body — see plans/response-streaming.md's "What was measured" section
// for the byte dump this was reverse-engineered from. There's no
// length-prefix or escaping around it: a body that happens to contain this
// exact eight-byte run would be mis-split too, but that ambiguity exists in
// the real frame format itself, not something introduced here.
var frameDelimiter = []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}

// Prelude mirrors shapedResponse minus the body: the JSON object a
// streaming handler passes to HttpResponseStream.from() before writing to
// the stream it returns. It's exposed mainly as ToHTTPStream's own decode
// target; SplitFrame itself only needs shapedStatusCode's looser
// object-with-integer-statusCode check to decide whether payload is framed
// at all.
type Prelude struct {
	StatusCode int               `json:"statusCode"`
	Headers    map[string]string `json:"headers"`
	Cookies    []string          `json:"cookies"`
}

// SplitFrame splits payload — a function's raw invoke result under
// RESPONSE_STREAM invoke mode — into its prelude and body at the first
// eight-NUL frameDelimiter. It reports ok=false, with prelude and body both
// nil, when there is no delimiter at all, or when the bytes preceding it
// don't parse as shapedStatusCode expects: a JSON object with an integer
// statusCode field. Either way this isn't an error — it means payload isn't
// actually a frame, whether because the handler never called
// streamifyResponse or because a body just happens to contain a NUL run —
// and callers (ToHTTPStream, and Unit B's router) fall back to the ordinary
// buffered path (event.ToHTTP) instead of failing the request over it.
func SplitFrame(payload []byte) (prelude []byte, body []byte, ok bool) {
	i := bytes.Index(payload, frameDelimiter)
	if i < 0 {
		return nil, nil, false
	}

	candidate := payload[:i]
	if _, ok := shapedStatusCode(candidate); !ok {
		return nil, nil, false
	}

	return candidate, payload[i+len(frameDelimiter):], true
}

// ErrNotFramed is ToHTTPStream's signal that payload didn't parse as an
// http-integration-response frame (see SplitFrame). It is returned before
// ToHTTPStream touches w in any way — no header set, no status written, no
// bytes written — so a caller that gets it back can safely retry the same
// ResponseWriter with event.ToHTTP.
var ErrNotFramed = errors.New("event: payload is not a framed streaming response")

// ToHTTPStream writes payload — a function's raw invoke result framed as
// application/vnd.awslambda.http-integration-response — to w as a real HTTP
// response: SplitFrame's prelude supplies the status code, headers and
// cookies, and the bytes after the delimiter become the body, written
// unmodified. Streaming frames have no isBase64Encoded concept —
// HttpResponseStream.from() writes whatever bytes the handler gives it — so
// unlike ToHTTP this never base64-decodes.
//
// If payload isn't a valid frame, ToHTTPStream returns ErrNotFramed without
// writing anything to w at all; the caller is expected to fall back to
// event.ToHTTP in that case, per its doc comment. Any other error comes
// from decoding the prelude JSON or writing the body, matching ToHTTP's own
// error contract (the caller turns it into a 502).
func ToHTTPStream(w http.ResponseWriter, payload []byte) error {
	preludeJSON, body, ok := SplitFrame(payload)
	if !ok {
		return ErrNotFramed
	}

	var prelude Prelude
	if err := json.Unmarshal(preludeJSON, &prelude); err != nil {
		return fmt.Errorf("event: decoding stream prelude: %w", err)
	}

	// A streamed body is arbitrary bytes, not JSON — unlike writeShaped's
	// "application/json" default, an absent Content-Type here defaults to
	// "application/octet-stream" (plans/response-streaming.md Unit A).
	return writeHeadersStatusBody(w, prelude.StatusCode, prelude.Headers, prelude.Cookies, body, "application/octet-stream")
}
