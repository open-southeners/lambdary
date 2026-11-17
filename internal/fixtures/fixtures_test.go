package fixtures

import (
	"crypto/md5" //nolint:gosec // matches SQS md5OfBody.
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func decode(t *testing.T, kind string, set map[string]string) map[string]any {
	t.Helper()

	data, err := Build(kind, set)
	if err != nil {
		t.Fatalf("Build(%s): %v", kind, err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Build(%s) is not JSON: %v", kind, err)
	}

	return m
}

func TestKinds(t *testing.T) {
	want := []string{"apigw", "s3", "schedule", "sqs"}
	if got := Kinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("Kinds() = %v, want %v", got, want)
	}
}

func TestBuildSQS(t *testing.T) {
	m := decode(t, "sqs", map[string]string{"body": "hi", "queue": "jobs", "count": "2", "attributes.ApproximateReceiveCount": "3"})
	recs := m["Records"].([]any)

	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}

	r := recs[0].(map[string]any)
	sum := md5.Sum([]byte("hi")) //nolint:gosec // test.

	checks := map[string]any{
		"body":           "hi",
		"md5OfBody":      hex.EncodeToString(sum[:]),
		"eventSource":    "aws:sqs",
		"eventSourceARN": "arn:aws:sqs:us-east-1:000000000000:jobs",
		"awsRegion":      "us-east-1",
	}
	for k, want := range checks {
		if r[k] != want {
			t.Errorf("%s = %v, want %v", k, r[k], want)
		}
	}

	if r["messageId"] == recs[1].(map[string]any)["messageId"] {
		t.Error("records share a messageId")
	}

	if r["receiptHandle"] == "" || r["messageAttributes"] == nil {
		t.Errorf("missing receiptHandle/messageAttributes: %v", r)
	}

	if got := r["attributes"].(map[string]any)["ApproximateReceiveCount"]; got != "3" {
		t.Errorf("attributes override = %v, want 3", got)
	}
}

func TestBuildS3(t *testing.T) {
	m := decode(t, "s3", map[string]string{"bucket": "b", "key": "a dir/f.txt", "size": "7", "event": "ObjectRemoved:Delete"})
	r := m["Records"].([]any)[0].(map[string]any)

	if r["eventVersion"] != "2.1" || r["eventSource"] != "aws:s3" || r["eventName"] != "ObjectRemoved:Delete" {
		t.Errorf("unexpected record header: %v", r)
	}

	s3 := r["s3"].(map[string]any)
	if got := s3["bucket"].(map[string]any)["name"]; got != "b" {
		t.Errorf("bucket = %v", got)
	}

	obj := s3["object"].(map[string]any)
	if obj["key"] != "a+dir/f.txt" || obj["size"] != float64(7) {
		t.Errorf("object = %v", obj)
	}

	d := decode(t, "s3", nil)["Records"].([]any)[0].(map[string]any)
	if d["eventName"] != "ObjectCreated:Put" {
		t.Errorf("default eventName = %v", d["eventName"])
	}
}

func TestBuildSchedule(t *testing.T) {
	m := decode(t, "schedule", map[string]string{"rule": "nightly", "time": "2025-05-06T07:08:09Z", "detail": `{"a":1}`})

	want := map[string]any{
		"version": "0", "detail-type": "Scheduled Event", "source": "aws.events",
		"account": "000000000000", "region": "us-east-1", "time": "2025-05-06T07:08:09Z",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}

	if !reflect.DeepEqual(m["resources"], []any{"nightly"}) {
		t.Errorf("resources = %v", m["resources"])
	}

	if m["detail"].(map[string]any)["a"] != float64(1) {
		t.Errorf("detail = %v", m["detail"])
	}

	if got := decode(t, "schedule", map[string]string{"schedule": "s1"})["resources"]; !reflect.DeepEqual(got, []any{"s1"}) {
		t.Errorf("schedule alias resources = %v", got)
	}
}

func TestBuildAPIGW(t *testing.T) {
	m := decode(t, "apigw", map[string]string{"method": "post", "path": "/users", "body": "{}", "query": "a=1&b=2", "header.X-Test": "yes"})

	if m["version"] != "2.0" || m["rawPath"] != "/users" || m["rawQueryString"] != "a=1&b=2" || m["body"] != "{}" {
		t.Errorf("unexpected event: %v", m)
	}

	if m["headers"].(map[string]any)["x-test"] != "yes" {
		t.Errorf("headers = %v", m["headers"])
	}

	if m["queryStringParameters"].(map[string]any)["b"] != "2" {
		t.Errorf("queryStringParameters = %v", m["queryStringParameters"])
	}

	if got := m["requestContext"].(map[string]any)["http"].(map[string]any)["method"]; got != "POST" {
		t.Errorf("method = %v", got)
	}
}

func TestBuildDeterministic(t *testing.T) {
	for _, k := range Kinds() {
		a, _ := Build(k, nil)
		b, _ := Build(k, nil)

		if string(a) != string(b) {
			t.Errorf("%s fixture is not deterministic", k)
		}
	}
}

func TestBuildErrors(t *testing.T) {
	tests := []struct {
		name string
		kind string
		set  map[string]string
		want string
	}{
		{"unknown kind", "kinesis", nil, "valid types: apigw, s3, schedule, sqs"},
		{"unknown sqs key", "sqs", map[string]string{"nope": "x"}, "valid keys: attributes.<name>, body, count, queue"},
		{"unknown s3 key", "s3", map[string]string{"nope": "x"}, "valid keys: bucket, event, key, size"},
		{"bare prefix key", "apigw", map[string]string{"header.": "x"}, "unknown key"},
		{"bad count", "sqs", map[string]string{"count": "0"}, "count"},
		{"bad size", "s3", map[string]string{"size": "x"}, "size"},
		{"bad time", "schedule", map[string]string{"time": "yesterday"}, "RFC 3339"},
		{"bad detail", "schedule", map[string]string{"detail": "[1"}, "detail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Build(tt.kind, tt.set)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}
