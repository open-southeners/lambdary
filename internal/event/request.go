// Package event implements a pure, stdlib-only mapping between Go's
// net/http request/response types and the AWS Lambda Function URL / API
// Gateway v2 ("2.0") JSON event format, per DESIGN.md's "Routing & event
// mapping" section and plans/m2-http-events.md's Unit A. It does no HTTP
// serving of its own — the router owns dispatch; this package only
// translates in both directions (FromHTTP, ToHTTP).
package event

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Local placeholders used in place of the real API Gateway identifiers a
// Function URL event would carry — DESIGN.md models local routes on
// Function URLs, but there is no real API/domain to report, so a fixed,
// obviously-local value stands in.
const (
	placeholderAPIID      = "lambdary"
	placeholderDomainName = "localhost"
)

// awsTimeLayout is the layout API Gateway uses for
// requestContext.http.time — a CLF-style timestamp, e.g.
// "12/Mar/2020:19:03:58 +0000".
const awsTimeLayout = "02/Jan/2006:15:04:05 -0700"

// textualContentTypes lists the exact (parameter-stripped, lowercased)
// media types treated as human-readable, per Unit A's base64 heuristic.
// Prefix "text/" and suffixes "+json"/"+xml" are checked separately in
// isTextualContentType.
var textualContentTypes = map[string]bool{
	"application/json":                  true,
	"application/xml":                   true,
	"application/x-www-form-urlencoded": true,
	"application/javascript":            true,
}

// nowFunc and idFunc are the request event's clock and request-id sources.
// They are package-level hooks rather than FromHTTP parameters so callers
// that don't care (the router) keep a plain two-argument call, while tests
// in this package can swap in fixed values for golden-JSON assertions.
var (
	nowFunc = time.Now
	idFunc  = newRequestID
)

// RequestV2 is the Lambda Function URL / API Gateway v2 ("2.0") request
// event, marshaling to the exact JSON shape AWS sends a function: see
// https://docs.aws.amazon.com/lambda/latest/dg/urls-invocation.html for the
// reference shape this mirrors.
type RequestV2 struct {
	Version               string            `json:"version"`
	RouteKey              string            `json:"routeKey"`
	RawPath               string            `json:"rawPath"`
	RawQueryString        string            `json:"rawQueryString"`
	Cookies               []string          `json:"cookies,omitempty"`
	Headers               map[string]string `json:"headers"`
	QueryStringParameters map[string]string `json:"queryStringParameters,omitempty"`
	RequestContext        RequestContext    `json:"requestContext"`
	Body                  string            `json:"body"`
	IsBase64Encoded       bool              `json:"isBase64Encoded"`
}

// RequestContext is RequestV2's requestContext object.
type RequestContext struct {
	AccountID    string          `json:"accountId"`
	APIID        string          `json:"apiId"`
	DomainName   string          `json:"domainName"`
	DomainPrefix string          `json:"domainPrefix"`
	HTTP         HTTPDescription `json:"http"`
	RequestID    string          `json:"requestId"`
	RouteKey     string          `json:"routeKey"`
	Stage        string          `json:"stage"`
	Time         string          `json:"time"`
	TimeEpoch    int64           `json:"timeEpoch"`
}

// HTTPDescription is RequestContext's nested http object.
type HTTPDescription struct {
	Method    string `json:"method"`
	Path      string `json:"path"`
	Protocol  string `json:"protocol"`
	SourceIP  string `json:"sourceIp"`
	UserAgent string `json:"userAgent"`
}

// FromHTTP builds the v2 event for r, as if r had arrived at a Lambda
// Function URL whose route is routePrefix (DESIGN.md's "the function's
// route prefix is stripped from rawPath" decision — see stripRoutePrefix).
// It reads and fully consumes r.Body; the only error it returns comes from
// that read.
func FromHTTP(r *http.Request, routePrefix string) (*RequestV2, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("event: reading request body: %w", err)
	}

	base64Body := isBase64Body(body, r.Header.Get("Content-Type"))

	var bodyStr string
	switch {
	case len(body) == 0:
		bodyStr = ""
	case base64Body:
		bodyStr = base64.StdEncoding.EncodeToString(body)
	default:
		bodyStr = string(body)
	}

	now := nowFunc()

	return &RequestV2{
		Version:               "2.0",
		RouteKey:              "$default",
		RawPath:               stripRoutePrefix(r.URL.Path, routePrefix),
		RawQueryString:        r.URL.RawQuery,
		Cookies:               splitCookies(r.Header),
		Headers:               buildHeaders(r.Header),
		QueryStringParameters: buildQueryParams(r.URL),
		RequestContext: RequestContext{
			AccountID:    "anonymous",
			APIID:        placeholderAPIID,
			DomainName:   placeholderDomainName,
			DomainPrefix: placeholderDomainName,
			HTTP: HTTPDescription{
				Method:    r.Method,
				Path:      r.URL.Path,
				Protocol:  r.Proto,
				SourceIP:  sourceIP(r.RemoteAddr),
				UserAgent: r.Header.Get("User-Agent"),
			},
			RequestID: idFunc(),
			RouteKey:  "$default",
			Stage:     "$default",
			Time:      now.Format(awsTimeLayout),
			TimeEpoch: now.UnixMilli(),
		},
		Body:            bodyStr,
		IsBase64Encoded: base64Body,
	}, nil
}

// stripRoutePrefix strips prefix from path to produce rawPath, per
// DESIGN.md's routing decision: request "/hello/users" for route "/hello"
// becomes "/users"; bare "/hello" becomes "/". A trailing slash on path
// collapses the same way ("/hello/" -> "/"). The root route ("" or "/")
// strips nothing — there is no prefix to remove, so path passes through
// unchanged and already-full-path routing behaves like a catch-all.
func stripRoutePrefix(path, prefix string) string {
	if prefix == "" || prefix == "/" {
		return path
	}

	if path == prefix || path == prefix+"/" {
		return "/"
	}

	if rest, ok := strings.CutPrefix(path, prefix+"/"); ok {
		return "/" + rest
	}

	// path doesn't actually fall under prefix — the caller (the router)
	// is expected to have matched the route before calling FromHTTP, so
	// this is defensive: return path unchanged rather than mangling it.
	return path
}

// splitCookies builds RequestV2.Cookies from the request's Cookie
// header(s): one entry per "name=value" pair, matching the array shape a
// real Function URL event uses (rather than the single semicolon-joined
// header string net/http's own Cookie header exposes).
func splitCookies(h http.Header) []string {
	var cookies []string

	for _, line := range h.Values("Cookie") {
		for _, part := range strings.Split(line, ";") {
			part = strings.TrimSpace(part)
			if part != "" {
				cookies = append(cookies, part)
			}
		}
	}

	return cookies
}

// buildHeaders converts h into RequestV2.Headers: lowercased keys,
// multi-valued headers joined with "," (API Gateway v2's own convention),
// and the Cookie header excluded since it's represented separately in
// RequestV2.Cookies. The result is always non-nil so Headers marshals as
// "{}" rather than "null" when a request happens to have none.
func buildHeaders(h http.Header) map[string]string {
	headers := make(map[string]string, len(h))

	for k, v := range h {
		if strings.EqualFold(k, "Cookie") {
			continue
		}

		headers[strings.ToLower(k)] = strings.Join(v, ",")
	}

	return headers
}

// buildQueryParams converts u's query string into
// RequestV2.QueryStringParameters: multi-valued parameters joined with ",",
// same convention as buildHeaders. It returns nil (omitted by
// RequestV2's omitempty tag) when there is no query string, matching a
// real Function URL event.
func buildQueryParams(u *url.URL) map[string]string {
	values := u.Query()
	if len(values) == 0 {
		return nil
	}

	params := make(map[string]string, len(values))
	for k, v := range values {
		params[k] = strings.Join(v, ",")
	}

	return params
}

// sourceIP extracts the host part of r.RemoteAddr ("host:port"), falling
// back to the raw value when it isn't in that shape (net/http always sets
// RemoteAddr to "host:port" for real connections, but callers building
// *http.Request by hand — tests — may leave it as a bare host or empty).
func sourceIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}

	return host
}

// isTextualContentType reports whether contentType (as sent in a
// Content-Type header, parameters and all) is on Unit A's textual
// allowlist: text/*, the exact types in textualContentTypes, or a
// "+json"/"+xml" structured-syntax suffix. An absent content type is
// treated as textual too, so the decision defers entirely to the caller's
// UTF-8 check rather than forcing base64 just because the header was
// omitted.
func isTextualContentType(contentType string) bool {
	base := contentType
	if i := strings.IndexByte(base, ';'); i >= 0 {
		base = base[:i]
	}

	base = strings.ToLower(strings.TrimSpace(base))
	if base == "" {
		return true
	}

	if strings.HasPrefix(base, "text/") {
		return true
	}

	if textualContentTypes[base] {
		return true
	}

	return strings.HasSuffix(base, "+json") || strings.HasSuffix(base, "+xml")
}

// isBase64Body reports whether body should be base64-encoded in the event
// per Unit A's heuristic: a non-textual content type or invalid UTF-8
// forces base64; an empty body is never base64, regardless of content
// type.
func isBase64Body(body []byte, contentType string) bool {
	if len(body) == 0 {
		return false
	}

	return !isTextualContentType(contentType) || !utf8.Valid(body)
}

// newRequestID generates a crypto/rand-derived, UUID-shaped request id.
// It's the default value of idFunc; tests override idFunc directly rather
// than calling this.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand.Read only fails if the OS entropy source itself is
		// broken; a fixed fallback id keeps the request usable rather than
		// panicking mid-request over an id string nobody depends on for
		// correctness.
		return "00000000-0000-0000-0000-000000000000"
	}

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
