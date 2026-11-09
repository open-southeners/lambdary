package queue

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// newClient serves store's API on a test server and returns a real SDK
// client pointed at it. The SDK validates MD5 checksums, so every call it
// accepts proves those right too.
func newClient(t *testing.T, store *Store) (*sqs.Client, string) {
	t.Helper()

	api := NewAPI(store)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !Handles(r) {
			http.NotFound(w, r)
			return
		}
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	client := sqs.New(sqs.Options{
		Region:       "us-east-1",
		BaseEndpoint: aws.String(srv.URL),
		Credentials:  credentials.NewStaticCredentialsProvider("local", "local", ""),
	})

	return client, srv.URL
}

func TestAPIWithSDK(t *testing.T) {
	ctx := context.Background()
	store := NewStore()
	client, base := newClient(t, store)

	if err := store.Ensure("notifications", Settings{}); err != nil {
		t.Fatal(err)
	}

	// GetQueueUrl, as Kiarga's awsctl resolves a queue.
	got, err := client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("notifications")})
	if err != nil {
		t.Fatalf("GetQueueUrl: %v", err)
	}

	url := aws.ToString(got.QueueUrl)
	if want := strings.TrimPrefix(base, "http://") + "/000000000000/notifications"; !strings.HasSuffix(url, want) {
		t.Errorf("QueueUrl = %q, want it to end in %q", url, want)
	}

	// A missing queue is the SDK's typed QueueDoesNotExist.
	_, err = client.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String("missing")})

	var notExist *sqstypes.QueueDoesNotExist
	if !errors.As(err, &notExist) {
		t.Errorf("GetQueueUrl on a missing queue = %v, want QueueDoesNotExist", err)
	}

	// SendMessage with attributes, then ReceiveMessage asking for them.
	if _, err := client.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(url),
		MessageBody: aws.String(`{"kind":"email"}`),
		MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			"trace": {DataType: aws.String("String"), StringValue: aws.String("abc")},
		},
	}); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	batch, err := client.SendMessageBatch(ctx, &sqs.SendMessageBatchInput{
		QueueUrl: aws.String(url),
		Entries: []sqstypes.SendMessageBatchRequestEntry{
			{Id: aws.String("one"), MessageBody: aws.String("1")},
			{Id: aws.String("two"), MessageBody: aws.String("2")},
		},
	})
	if err != nil {
		t.Fatalf("SendMessageBatch: %v", err)
	}
	if len(batch.Successful) != 2 || len(batch.Failed) != 0 {
		t.Fatalf("SendMessageBatch = %+v", batch)
	}

	recv, err := client.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:                    aws.String(url),
		MaxNumberOfMessages:         10,
		WaitTimeSeconds:             1,
		MessageAttributeNames:       []string{"All"},
		MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{"All"},
	})
	if err != nil {
		t.Fatalf("ReceiveMessage: %v", err)
	}
	if len(recv.Messages) != 3 {
		t.Fatalf("ReceiveMessage got %d messages, want 3", len(recv.Messages))
	}

	first := recv.Messages[0]
	if aws.ToString(first.Body) != `{"kind":"email"}` {
		t.Errorf("first body = %q", aws.ToString(first.Body))
	}
	if v := first.MessageAttributes["trace"]; aws.ToString(v.StringValue) != "abc" {
		t.Errorf("first attributes = %+v", first.MessageAttributes)
	}
	if first.Attributes["ApproximateReceiveCount"] != "1" {
		t.Errorf("first system attributes = %+v", first.Attributes)
	}

	// Delete one alone and the others in a batch.
	if _, err := client.DeleteMessage(ctx, &sqs.DeleteMessageInput{QueueUrl: aws.String(url), ReceiptHandle: first.ReceiptHandle}); err != nil {
		t.Fatalf("DeleteMessage: %v", err)
	}

	del, err := client.DeleteMessageBatch(ctx, &sqs.DeleteMessageBatchInput{
		QueueUrl: aws.String(url),
		Entries: []sqstypes.DeleteMessageBatchRequestEntry{
			{Id: aws.String("a"), ReceiptHandle: recv.Messages[1].ReceiptHandle},
			{Id: aws.String("b"), ReceiptHandle: recv.Messages[2].ReceiptHandle},
		},
	})
	if err != nil || len(del.Successful) != 2 {
		t.Fatalf("DeleteMessageBatch = %+v, %v", del, err)
	}

	attrs, err := client.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(url),
		AttributeNames: []sqstypes.QueueAttributeName{"All"},
	})
	if err != nil {
		t.Fatalf("GetQueueAttributes: %v", err)
	}
	if attrs.Attributes["ApproximateNumberOfMessages"] != "0" || attrs.Attributes["QueueArn"] != ARN("notifications") {
		t.Errorf("GetQueueAttributes = %+v", attrs.Attributes)
	}
}

func TestAPICreateQueueWithRedrive(t *testing.T) {
	ctx := context.Background()
	store := NewStore()
	client, _ := newClient(t, store)

	if _, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String("jobs-dlq")}); err != nil {
		t.Fatalf("CreateQueue dlq: %v", err)
	}

	created, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName: aws.String("jobs"),
		Attributes: map[string]string{
			"VisibilityTimeout": "0",
			"RedrivePolicy":     `{"deadLetterTargetArn":"` + ARN("jobs-dlq") + `","maxReceiveCount":"1"}`,
		},
	})
	if err != nil {
		t.Fatalf("CreateQueue: %v", err)
	}

	info, err := store.Info("jobs")
	if err != nil {
		t.Fatal(err)
	}
	if info.Settings.DeadLetterQueue != "jobs-dlq" || info.Settings.MaxReceiveCount != 1 {
		t.Errorf("settings = %+v, want the redrive policy applied", info.Settings)
	}

	list, err := client.ListQueues(ctx, &sqs.ListQueuesInput{QueueNamePrefix: aws.String("jobs")})
	if err != nil || len(list.QueueUrls) != 2 {
		t.Fatalf("ListQueues = %+v, %v", list, err)
	}

	if _, err := client.CreateQueue(ctx, &sqs.CreateQueueInput{
		QueueName:  aws.String("orders"),
		Attributes: map[string]string{"FifoQueue": "true"},
	}); err == nil {
		t.Error("CreateQueue of a FIFO queue succeeded, want an error")
	}

	if _, err := client.PurgeQueue(ctx, &sqs.PurgeQueueInput{QueueUrl: created.QueueUrl}); err != nil {
		t.Errorf("PurgeQueue: %v", err)
	}

	if _, err := client.DeleteQueue(ctx, &sqs.DeleteQueueInput{QueueUrl: created.QueueUrl}); err != nil {
		t.Errorf("DeleteQueue: %v", err)
	}

	if store.Exists("jobs") {
		t.Error("DeleteQueue left the queue")
	}
}
