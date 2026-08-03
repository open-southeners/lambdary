package event

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// resourceProxyPlus is RequestV1.Resource's fixed value. A real REST API
// only reports a templated resource like "/{proxy+}" when the deployed API
// actually uses a greedy proxy resource; Lambdary always routes a function
// at a single prefix via one such resource (mirroring how FromHTTP models
// every route as a Function URL), so the value never varies per request.
const resourceProxyPlus = "/{proxy+}"

// RequestV1 is the API Gateway REST API ("1.0" payload) Lambda proxy
// integration event, marshaling to the exact JSON shape AWS sends a
// function: see
// https://docs.aws.amazon.com/apigateway/latest/developerguide/set-up-lambda-proxy-integrations.html#api-gateway-simple-proxy-for-lambda-input-format
// for the reference shape this mirrors. Unlike RequestV2, several fields
// marshal as explicit JSON null (no omitempty) when absent — that's what a
// real REST API event does (see e.g. its own "pathParameters": null on a
// non-proxy route), so this type follows suit rather than omitting them.
type RequestV1 struct {
	Resource                        string              `json:"resource"`
	Path                            string              `json:"path"`
	HTTPMethod                      string              `json:"httpMethod"`
	Headers                         map[string]string   `json:"headers"`
	MultiValueHeaders               map[string][]string `json:"multiValueHeaders"`
	QueryStringParameters           map[string]string   `json:"queryStringParameters"`
	MultiValueQueryStringParameters map[string][]string `json:"multiValueQueryStringParameters"`
	PathParameters                  map[string]string   `json:"pathParameters"`
	StageVariables                  map[string]string   `json:"stageVariables"`
	RequestContext                  RequestContextV1    `json:"requestContext"`
	Body                            *string             `json:"body"`
	IsBase64Encoded                 bool                `json:"isBase64Encoded"`
}

// RequestContextV1 is RequestV1's requestContext object. It carries only
// the subset of a real REST API's requestContext that Lambdary can
// meaningfully fill in locally (there's no real account, authorizer, or
// resource id to report) — see plans/m5-extras.md's Unit A field list.
type RequestContextV1 struct {
	AccountID        string     `json:"accountId"`
	APIID            string     `json:"apiId"`
	Stage            string     `json:"stage"`
	RequestID        string     `json:"requestId"`
	Identity         IdentityV1 `json:"identity"`
	HTTPMethod       string     `json:"httpMethod"`
	Path             string     `json:"path"`
	Protocol         string     `json:"protocol"`
	RequestTime      string     `json:"requestTime"`
	RequestTimeEpoch int64      `json:"requestTimeEpoch"`
}

// IdentityV1 is RequestContextV1's nested identity object, trimmed to the
// two fields Lambdary can actually derive from a local request (sourceIp,
// userAgent) rather than the many auth-related fields a real REST API
// populates only when an authorizer is configured.
type IdentityV1 struct {
	SourceIP  string `json:"sourceIp"`
	UserAgent string `json:"userAgent"`
}

// FromHTTPV1 builds the v1 (API Gateway REST API / "1.0" payload) event for
// r, as if r had arrived at a REST API's single "ANY /{proxy+}" method whose
// resource is mounted at routePrefix (the same route-prefix-stripping
// FromHTTP applies for v2 — see stripRoutePrefix). It reads and fully
// consumes r.Body; the only error it returns comes from that read.
func FromHTTPV1(r *http.Request, routePrefix string) (*RequestV1, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, fmt.Errorf("event: reading request body: %w", err)
	}

	base64Body := isBase64Body(body, r.Header.Get("Content-Type"))

	var bodyPtr *string
	if len(body) > 0 {
		var bodyStr string
		if base64Body {
			bodyStr = base64.StdEncoding.EncodeToString(body)
		} else {
			bodyStr = string(body)
		}
		bodyPtr = &bodyStr
	}

	now := nowFunc()
	path := stripRoutePrefix(r.URL.Path, routePrefix)
	query := r.URL.Query()

	return &RequestV1{
		Resource:                        resourceProxyPlus,
		Path:                            path,
		HTTPMethod:                      r.Method,
		Headers:                         buildHeadersV1(r.Header),
		MultiValueHeaders:               buildMultiValueHeadersV1(r.Header),
		QueryStringParameters:           buildQueryParamsV1(query),
		MultiValueQueryStringParameters: buildMultiValueQueryParamsV1(query),
		PathParameters:                  buildProxyPathParameters(path),
		StageVariables:                  nil,
		RequestContext: RequestContextV1{
			AccountID: "anonymous",
			APIID:     placeholderAPIID,
			Stage:     "$default",
			RequestID: idFunc(),
			Identity: IdentityV1{
				SourceIP:  sourceIP(r.RemoteAddr),
				UserAgent: r.Header.Get("User-Agent"),
			},
			HTTPMethod:       r.Method,
			Path:             r.URL.Path,
			Protocol:         r.Proto,
			RequestTime:      now.Format(awsTimeLayout),
			RequestTimeEpoch: now.UnixMilli(),
		},
		Body:            bodyPtr,
		IsBase64Encoded: base64Body,
	}, nil
}

// buildProxyPathParameters builds RequestV1.PathParameters from path (the
// already prefix-stripped path FromHTTPV1 computes): {"proxy": path without
// its leading slash}. It returns nil when path is "/" — a bare hit on the
// route's root — since a real "/{proxy+}" resource variable requires at
// least one path segment to match at all, and an empty proxy value would
// misrepresent that.
func buildProxyPathParameters(path string) map[string]string {
	if path == "/" {
		return nil
	}

	return map[string]string{"proxy": strings.TrimPrefix(path, "/")}
}

// buildHeadersV1 converts h into RequestV1.Headers: canonical casing
// (unlike buildHeaders' v2 lowercasing — API Gateway REST APIs preserve
// it), the Cookie header included like any other (v1 has no top-level
// cookies array to carry it separately), and the LAST value kept for a
// header sent multiple times. That last-value rule (rather than v2's
// comma-join) matches AWS's own documented example: a client sending
// "header2: value1" and "header2: value2" gets back
// headers.header2 == "value2" while multiValueHeaders.header2 == ["value1",
// "value2"] — see the Input format section of
// https://docs.aws.amazon.com/apigateway/latest/developerguide/set-up-lambda-proxy-integrations.html
func buildHeadersV1(h http.Header) map[string]string {
	headers := make(map[string]string, len(h))

	for k, v := range h {
		if len(v) == 0 {
			continue
		}

		headers[k] = v[len(v)-1]
	}

	return headers
}

// buildMultiValueHeadersV1 converts h into RequestV1.MultiValueHeaders: one
// entry per header name (canonical casing, Cookie included), holding every
// value the client sent for it in order.
func buildMultiValueHeadersV1(h http.Header) map[string][]string {
	mvh := make(map[string][]string, len(h))

	for k, v := range h {
		mvh[k] = v
	}

	return mvh
}

// buildQueryParamsV1 converts query into RequestV1.QueryStringParameters:
// the LAST value for a repeated parameter (see buildHeadersV1's doc comment
// for the AWS-documented reasoning this mirrors), nil when query is empty.
func buildQueryParamsV1(query url.Values) map[string]string {
	if len(query) == 0 {
		return nil
	}

	params := make(map[string]string, len(query))
	for k, v := range query {
		if len(v) == 0 {
			continue
		}

		params[k] = v[len(v)-1]
	}

	return params
}

// buildMultiValueQueryParamsV1 converts query into
// RequestV1.MultiValueQueryStringParameters: every value per parameter, nil
// when query is empty.
func buildMultiValueQueryParamsV1(query url.Values) map[string][]string {
	if len(query) == 0 {
		return nil
	}

	mvq := make(map[string][]string, len(query))
	for k, v := range query {
		mvq[k] = v
	}

	return mvq
}
