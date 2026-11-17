// Package fixtures builds realistic sample AWS Lambda events — SQS, S3,
// EventBridge Scheduler and API Gateway HTTP API (payload v2.0) — for
// "lambdary invoke --event <kind>", so a handler can be exercised without
// hand-writing a payload.
//
// Output is deterministic: region us-east-1, account 000000000000 and fixed
// ids and timestamps, unless a key below overrides them. Build rejects any
// key a kind does not know, listing the valid ones.
//
// Settable keys per kind:
//
//	sqs:      body, queue, count (records), attributes.<k>
//	s3:       bucket, key, size, event (default ObjectCreated:Put)
//	schedule: rule (or schedule), time (RFC 3339), detail (JSON object)
//	apigw:    method, path, body, query, header.<k>
package fixtures

import (
	"crypto/md5" //nolint:gosec // SQS defines md5OfBody as MD5; not used for security.
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/open-southeners/lambdary/internal/event"
	"github.com/open-southeners/lambdary/internal/queue"
)

// fixedTime is the timestamp every fixture carries unless "time" overrides it.
var fixedTime = time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

// builders maps each fixture kind to its builder.
var builders = map[string]func(set map[string]string) ([]byte, error){
	"sqs":      buildSQS,
	"s3":       buildS3,
	"schedule": buildSchedule,
	"apigw":    buildAPIGW,
}

// Kinds returns the supported fixture kinds, sorted.
func Kinds() []string {
	kinds := make([]string, 0, len(builders))
	for k := range builders {
		kinds = append(kinds, k)
	}

	sort.Strings(kinds)

	return kinds
}

// IsKind reports whether kind names a fixture.
func IsKind(kind string) bool {
	_, ok := builders[kind]

	return ok
}

// Build returns the JSON event for kind with set's overrides applied. An
// unknown kind or an unknown/invalid key is an error.
func Build(kind string, set map[string]string) ([]byte, error) {
	b, ok := builders[kind]
	if !ok {
		return nil, fmt.Errorf("fixtures: unknown event type %q; valid types: %s", kind, strings.Join(Kinds(), ", "))
	}

	return b(set)
}

// checkKeys errors when set holds a key that is neither in exact nor under
// one of the prefixes (each spelled with its trailing dot).
func checkKeys(kind string, set map[string]string, exact, prefixes []string) error {
	for k := range set {
		if containsString(exact, k) {
			continue
		}

		ok := false

		for _, p := range prefixes {
			if strings.HasPrefix(k, p) && len(k) > len(p) {
				ok = true
			}
		}

		if !ok {
			valid := append([]string{}, exact...)
			for _, p := range prefixes {
				valid = append(valid, p+"<name>")
			}

			sort.Strings(valid)

			return fmt.Errorf("fixtures: unknown key %q for %s event; valid keys: %s", k, kind, strings.Join(valid, ", "))
		}
	}

	return nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}

	return false
}

// get returns set[key], or def when unset.
func get(set map[string]string, key, def string) string {
	if v, ok := set[key]; ok {
		return v
	}

	return def
}

// prefixed returns the entries of set under prefix, with the prefix removed.
func prefixed(set map[string]string, prefix string) map[string]string {
	out := map[string]string{}

	for k, v := range set {
		if strings.HasPrefix(k, prefix) {
			out[strings.TrimPrefix(k, prefix)] = v
		}
	}

	return out
}

// uuid returns a deterministic UUID-shaped id for the n-th item of a kind.
func uuid(prefix byte, n int) string {
	return fmt.Sprintf("%02x000000-0000-4000-8000-%012x", prefix, n)
}

func buildSQS(set map[string]string) ([]byte, error) {
	if err := checkKeys("sqs", set, []string{"body", "queue", "count"}, []string{"attributes."}); err != nil {
		return nil, err
	}

	count := 1

	if v, ok := set["count"]; ok {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("fixtures: sqs count must be a positive integer, got %q", v)
		}

		count = n
	}

	body := get(set, "body", "Hello from Lambdary")
	sum := md5.Sum([]byte(body)) //nolint:gosec // see import.
	sent := strconv.FormatInt(fixedTime.UnixMilli(), 10)

	type record struct {
		MessageID         string            `json:"messageId"`
		ReceiptHandle     string            `json:"receiptHandle"`
		Body              string            `json:"body"`
		Attributes        map[string]string `json:"attributes"`
		MessageAttributes map[string]any    `json:"messageAttributes"`
		MD5OfBody         string            `json:"md5OfBody"`
		EventSource       string            `json:"eventSource"`
		EventSourceARN    string            `json:"eventSourceARN"`
		AWSRegion         string            `json:"awsRegion"`
	}

	records := make([]record, count)

	for i := range records {
		attrs := map[string]string{
			"ApproximateReceiveCount":          "1",
			"SentTimestamp":                    sent,
			"SenderId":                         queue.AccountID,
			"ApproximateFirstReceiveTimestamp": sent,
		}
		for k, v := range prefixed(set, "attributes.") {
			attrs[k] = v
		}

		records[i] = record{
			MessageID:         uuid(0x51, i+1),
			ReceiptHandle:     fmt.Sprintf("AQEB-lambdary-receipt-%d", i+1),
			Body:              body,
			Attributes:        attrs,
			MessageAttributes: map[string]any{},
			MD5OfBody:         hex.EncodeToString(sum[:]),
			EventSource:       "aws:sqs",
			EventSourceARN:    queue.ARN(get(set, "queue", "my-queue")),
			AWSRegion:         queue.Region,
		}
	}

	return json.MarshalIndent(map[string]any{"Records": records}, "", "  ")
}

func buildS3(set map[string]string) ([]byte, error) {
	if err := checkKeys("s3", set, []string{"bucket", "key", "size", "event"}, nil); err != nil {
		return nil, err
	}

	var size int64 = 1024

	if v, ok := set["size"]; ok {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("fixtures: s3 size must be a non-negative integer, got %q", v)
		}

		size = n
	}

	bucket := get(set, "bucket", "my-bucket")
	key := get(set, "key", "path/to/object.txt")
	name := strings.TrimPrefix(get(set, "event", "ObjectCreated:Put"), "s3:")

	// S3 notifications URL-encode the key, spaces as "+".
	encodedKey := strings.ReplaceAll(url.QueryEscape(key), "%2F", "/")

	payload := map[string]any{
		"Records": []any{map[string]any{
			"eventVersion": "2.1",
			"eventSource":  "aws:s3",
			"awsRegion":    queue.Region,
			"eventTime":    fixedTime.Format("2006-01-02T15:04:05.000Z"),
			"eventName":    name,
			"userIdentity": map[string]any{"principalId": "AIDAEXAMPLE"},
			"requestParameters": map[string]any{
				"sourceIPAddress": "127.0.0.1",
			},
			"responseElements": map[string]any{
				"x-amz-request-id": "LAMBDARYREQ00001",
				"x-amz-id-2":       "lambdary-host-id",
			},
			"s3": map[string]any{
				"s3SchemaVersion": "1.0",
				"configurationId": "lambdary",
				"bucket": map[string]any{
					"name":          bucket,
					"ownerIdentity": map[string]any{"principalId": "AIDAEXAMPLE"},
					"arn":           "arn:aws:s3:::" + bucket,
				},
				"object": map[string]any{
					"key":       encodedKey,
					"size":      size,
					"eTag":      "d41d8cd98f00b204e9800998ecf8427e",
					"sequencer": "0055AED6DCD90281E5",
				},
			},
		}},
	}

	return json.MarshalIndent(payload, "", "  ")
}

// buildSchedule mirrors the "Scheduled Event" internal/triggers sends when a
// schedule fires.
func buildSchedule(set map[string]string) ([]byte, error) {
	if err := checkKeys("schedule", set, []string{"rule", "schedule", "time", "detail"}, nil); err != nil {
		return nil, err
	}

	rule := get(set, "rule", get(set, "schedule", "my-schedule"))
	when := fixedTime

	if v, ok := set["time"]; ok {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return nil, fmt.Errorf("fixtures: schedule time must be RFC 3339, got %q", v)
		}

		when = t.UTC()
	}

	detail := map[string]any{}

	if v, ok := set["detail"]; ok {
		if err := json.Unmarshal([]byte(v), &detail); err != nil {
			return nil, fmt.Errorf("fixtures: schedule detail must be a JSON object: %w", err)
		}
	}

	payload := map[string]any{
		"version":     "0",
		"id":          uuid(0x5c, 1),
		"detail-type": "Scheduled Event",
		"source":      "aws.events",
		"account":     queue.AccountID,
		"time":        when.Format(time.RFC3339),
		"region":      queue.Region,
		"resources":   []string{rule},
		"detail":      detail,
	}

	return json.MarshalIndent(payload, "", "  ")
}

func buildAPIGW(set map[string]string) ([]byte, error) {
	if err := checkKeys("apigw", set, []string{"method", "path", "body", "query"}, []string{"header."}); err != nil {
		return nil, err
	}

	method := strings.ToUpper(get(set, "method", "GET"))
	path := get(set, "path", "/")
	query := strings.TrimPrefix(get(set, "query", ""), "?")

	headers := map[string]string{
		"host":       "lambdary.lambda-url.us-east-1.on.aws",
		"user-agent": "lambdary-invoke",
	}
	for k, v := range prefixed(set, "header.") {
		headers[strings.ToLower(k)] = v
	}

	var params map[string]string

	if query != "" {
		values, err := url.ParseQuery(query)
		if err != nil {
			return nil, fmt.Errorf("fixtures: apigw query %q: %w", query, err)
		}

		params = map[string]string{}
		for k, v := range values {
			params[k] = strings.Join(v, ",")
		}
	}

	req := event.RequestV2{
		Version:               "2.0",
		RouteKey:              "$default",
		RawPath:               path,
		RawQueryString:        query,
		Headers:               headers,
		QueryStringParameters: params,
		RequestContext: event.RequestContext{
			AccountID:    "anonymous",
			APIID:        "lambdary",
			DomainName:   headers["host"],
			DomainPrefix: "lambdary",
			HTTP: event.HTTPDescription{
				Method:    method,
				Path:      path,
				Protocol:  "HTTP/1.1",
				SourceIP:  "127.0.0.1",
				UserAgent: headers["user-agent"],
			},
			RequestID: uuid(0xa9, 1),
			RouteKey:  "$default",
			Stage:     "$default",
			Time:      fixedTime.Format("02/Jan/2006:15:04:05 -0700"),
			TimeEpoch: fixedTime.UnixMilli(),
		},
		Body: get(set, "body", ""),
	}

	return json.MarshalIndent(req, "", "  ")
}
