package event

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// shapedResponse is the object shape a Function URL handler can return to
// take full control of the HTTP response — everything but statusCode,
// which ToHTTP extracts separately (see its doc comment for why).
type shapedResponse struct {
	Headers         map[string]string `json:"headers"`
	Cookies         []string          `json:"cookies"`
	Body            string            `json:"body"`
	IsBase64Encoded bool              `json:"isBase64Encoded"`
}

// ToHTTP writes payload — a function's raw invoke result — to w, following
// the Function URL response rules DESIGN.md and plans/m2-http-events.md
// Unit A describe:
//
//   - a JSON object containing an integer statusCode is a "shaped"
//     response: its headers/cookies/body/isBase64Encoded are applied to w
//     (default Content-Type "application/json" when headers don't set
//     one).
//   - anything else — non-object JSON, an object without statusCode,
//     invalid JSON entirely — is written as-is with 200 and
//     Content-Type: application/json, matching how AWS serializes a
//     Function URL handler's unshaped return value.
//
// The only error ToHTTP returns is a malformed shaped response (currently:
// a non-base64 Body when isBase64Encoded is true); callers turn that into a
// 502, per the plan.
func ToHTTP(w http.ResponseWriter, payload []byte) error {
	statusCode, ok := shapedStatusCode(payload)
	if !ok {
		writeUnshaped(w, payload)
		return nil
	}

	var resp shapedResponse
	if err := json.Unmarshal(payload, &resp); err != nil {
		return fmt.Errorf("event: decoding shaped response: %w", err)
	}

	return writeShaped(w, statusCode, resp)
}

// shapedStatusCode reports payload's top-level statusCode and whether
// payload qualifies as a shaped response at all: valid JSON, a top-level
// object, with a statusCode field whose literal JSON value is an integer.
// Anything else (invalid JSON, a non-object top level, a missing or
// non-integer statusCode) reports ok=false.
func shapedStatusCode(payload []byte) (statusCode int, ok bool) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		return 0, false
	}

	raw, present := top["statusCode"]
	if !present {
		return 0, false
	}

	if err := json.Unmarshal(raw, &statusCode); err != nil {
		return 0, false
	}

	return statusCode, true
}

// writeUnshaped writes payload verbatim as the response body: 200,
// Content-Type: application/json, no other headers or cookies. AWS already
// serializes a Function URL handler's unshaped return value to JSON before
// it reaches the caller, so payload is written as-is rather than
// re-encoded.
func writeUnshaped(w http.ResponseWriter, payload []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(payload) //nolint:errcheck // best-effort; client disconnect leaves nothing to do about a write error here.
}

// writeShaped applies resp's headers, cookies, and (decoded) body to w
// under statusCode.
func writeShaped(w http.ResponseWriter, statusCode int, resp shapedResponse) error {
	body := []byte(resp.Body)
	if resp.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(resp.Body)
		if err != nil {
			return fmt.Errorf("event: decoding base64 response body: %w", err)
		}
		body = decoded
	}

	return writeHeadersStatusBody(w, statusCode, resp.Headers, resp.Cookies, body, "application/json")
}

// writeHeadersStatusBody is the tail shared by writeShaped and stream.go's
// ToHTTPStream: it sets headers (defaulting Content-Type to
// defaultContentType when headers doesn't supply its own), adds one
// Set-Cookie per entry in cookies, writes statusCode, and — if body is
// non-empty — writes body. The two callers differ only in what an absent
// Content-Type should mean (a JSON-shaped response defaults to
// "application/json"; a streamed body is arbitrary bytes and defaults to
// "application/octet-stream"), so that choice is a parameter rather than
// logic duplicated in both places.
//
// The len(body) > 0 guard matters on its own: calling w.Write with a
// zero-length slice is harmless, but skipping the call entirely is what
// keeps a body-less status (e.g. 204, or a shaped response with no "body")
// legal under Go 1.26's stricter net/http body-on-empty-status handling —
// see CHANGELOG.md's 1.0.1 entry.
func writeHeadersStatusBody(w http.ResponseWriter, statusCode int, headers map[string]string, cookies []string, body []byte, defaultContentType string) error {
	if !hasContentType(headers) {
		w.Header().Set("Content-Type", defaultContentType)
	}
	for k, v := range headers {
		w.Header().Set(k, v)
	}
	for _, cookie := range cookies {
		w.Header().Add("Set-Cookie", cookie)
	}

	w.WriteHeader(statusCode)
	if len(body) > 0 {
		if _, err := w.Write(body); err != nil {
			return fmt.Errorf("event: writing response body: %w", err)
		}
	}

	return nil
}

// hasContentType reports whether headers already sets Content-Type,
// case-insensitively — AWS Function URL responses default the header only
// when the handler didn't supply its own.
func hasContentType(headers map[string]string) bool {
	for k := range headers {
		if strings.EqualFold(k, "Content-Type") {
			return true
		}
	}

	return false
}
