package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// TestE2ESQSAndSchedules proves the queue and schedule triggers' done-when
// against a real dev server running real Go functions: a message sent with the
// AWS SDK reaches the function as an SQS event, a failed item is retried, it
// lands in the DLQ after the limit, and a schedule fires its function.
func TestE2ESQSAndSchedules(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e test skipped: -short")
	}

	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("e2e test skipped: go not on PATH: %v", err)
	}

	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("testdata/triggers-module")); err != nil {
		t.Fatalf("copying fixture: %v", err)
	}

	out := t.TempDir()
	workerLog := filepath.Join(out, "worker.log")
	tickLog := filepath.Join(out, "tick.log")

	// at() takes wall-clock seconds, so the schedule fires once, soon, without
	// the test waiting out a minute-long rate.
	fireAt := time.Now().UTC().Add(10 * time.Second).Format("2006-01-02T15:04:05")

	config := fmt.Sprintf(`functions:
  worker:
    go: ./cmd/worker
    environment:
      OUT_FILE: %q
    events:
      - sqs:
          queue: jobs
          visibility_timeout: 1
          dlq: jobs-dlq
          max_receive_count: 2
          function_response_types: [ReportBatchItemFailures]
  tick:
    go: ./cmd/tick
    environment:
      OUT_FILE: %q
    events:
      - schedule: at(%s)
`, workerLog, tickLog, fireAt)

	if err := os.WriteFile(filepath.Join(root, "lambdary.yml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := startVersionsDev(t, root, "process")

	ctx := context.Background()
	client := sqs.New(sqs.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.url),
		Credentials:  credentials.NewStaticCredentialsProvider("local", "local", ""),
	})

	jobs, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("jobs")})
	if err != nil {
		t.Fatalf("GetQueueUrl(jobs): %v", err)
	}

	if _, err := client.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
		QueueUrl: jobs.QueueUrl,
		Entries: []sqstypes.SendMessageBatchRequestEntry{
			{Id: aws.String("a"), MessageBody: aws.String("ok-1")},
			{Id: aws.String("b"), MessageBody: aws.String("poison")},
			{Id: aws.String("c"), MessageBody: aws.String("ok-2")},
		},
	}); err != nil {
		t.Fatalf("SendMessageBatch: %v", err)
	}

	dlq, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("jobs-dlq")})
	if err != nil {
		t.Fatalf("GetQueueUrl(jobs-dlq): %v", err)
	}

	// The first invoke builds and cold-starts the worker, so allow minutes.
	var dead []sqstypes.Message

	deadline := time.Now().Add(3 * time.Minute)
	for len(dead) == 0 && time.Now().Before(deadline) {
		recv, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: dlq.QueueUrl, MaxNumberOfMessages: 10, WaitTimeSeconds: 5})
		if err != nil {
			t.Fatalf("ReceiveMessage(jobs-dlq): %v", err)
		}
		dead = recv.Messages
	}

	if len(dead) != 1 || aws.ToString(dead[0].Body) != "poison" {
		t.Fatalf("jobs-dlq holds %d messages, want poison only: %+v", len(dead), dead)
	}

	lines := readLines(t, workerLog)
	if count(lines, "aws:sqs ok-1") != 1 || count(lines, "aws:sqs ok-2") != 1 {
		t.Errorf("worker saw %q, want ok-1 and ok-2 once each", lines)
	}
	if n := count(lines, "aws:sqs poison"); n != 2 {
		t.Errorf("worker saw poison %d times, want max_receive_count (2)", n)
	}

	attrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: jobs.QueueUrl, AttributeNames: []sqstypes.QueueAttributeName{"All"}})
	if err != nil {
		t.Fatalf("GetQueueAttributes(jobs): %v", err)
	}
	if attrs.Attributes["ApproximateNumberOfMessages"] != "0" || attrs.Attributes["ApproximateNumberOfMessagesNotVisible"] != "0" {
		t.Errorf("jobs still holds messages: %v", attrs.Attributes)
	}

	waitUntil(t, 2*time.Minute, "the schedule to fire tick", func() bool {
		return count(readLines(t, tickLog), "aws.events Scheduled Event") == 1
	})
}

func readLines(t *testing.T, path string) []string {
	t.Helper()

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}

	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func count(lines []string, want string) int {
	n := 0
	for _, l := range lines {
		if l == want {
			n++
		}
	}

	return n
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
