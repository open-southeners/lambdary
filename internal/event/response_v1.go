package event

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
)

// shapedResponseV1 is the object shape a v1 (API Gateway REST API) function
// must return — see ToHTTPV1's doc comment for why "must": v1 has no
// unshaped fallback the way ToHTTP's v2 does.
type shapedResponseV1 struct {
	Headers           map[string]string   `json:"headers"`
	MultiValueHeaders map[string][]string `json:"multiValueHeaders"`
	Body              string              `json:"body"`
	IsBase64Encoded   bool                `json:"isBase64Encoded"`
}

// ToHTTPV1 writes payload — a function's raw invoke result — to w, following
// the API Gateway REST API ("1.0" payload) Lambda proxy integration output
// contract: unlike ToHTTP's Function URL/v2 rules, there is no "bare JSON ->
// 200" fallback for an unshaped return value. A v1 response MUST be a JSON
// object with an integer statusCode — anything else (invalid JSON, a
// non-object top level, a missing or non-integer statusCode) is a malformed
// Lambda proxy response, and real API Gateway itself returns 502 Bad
// Gateway for exactly that case (see the Output format section of
// https://docs.aws.amazon.com/apigateway/latest/developerguide/set-up-lambda-proxy-integrations.html).
// ToHTTPV1 mirrors that by returning an error instead of writing anything;
// callers turn it into a 502.
//
// When present, headers and multiValueHeaders are merged additively: each
// headers[k] is applied first, then every multiValueHeaders[k] value is
// appended after it (no dedup of identical key/value pairs across the two —
// AWS's own docs note a same-pair case is deduplicated, but that's an
// obscure edge the simpler additive merge here doesn't bother chasing).
// Content-Type defaults to "application/json" when the response doesn't set
// its own, same as v2. body is decoded per isBase64Encoded; a bad base64
// body is itself a malformed-response error.
func ToHTTPV1(w http.ResponseWriter, payload []byte) error {
	statusCode, ok := shapedStatusCode(payload)
	if !ok {
		return fmt.Errorf("event: malformed v1 response: not a JSON object with an integer statusCode")
	}

	var resp shapedResponseV1
	if err := json.Unmarshal(payload, &resp); err != nil {
		return fmt.Errorf("event: decoding shaped response: %w", err)
	}

	return writeShapedV1(w, statusCode, resp)
}

// writeShapedV1 applies resp's headers, multiValueHeaders, and (decoded)
// body to w under statusCode.
func writeShapedV1(w http.ResponseWriter, statusCode int, resp shapedResponseV1) error {
	body := []byte(resp.Body)
	if resp.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(resp.Body)
		if err != nil {
			return fmt.Errorf("event: decoding base64 response body: %w", err)
		}
		body = decoded
	}

	if !hasContentType(resp.Headers) {
		w.Header().Set("Content-Type", "application/json")
	}
	for k, v := range resp.Headers {
		w.Header().Add(k, v)
	}
	for k, values := range resp.MultiValueHeaders {
		for _, v := range values {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(statusCode)
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("event: writing response body: %w", err)
	}

	return nil
}
