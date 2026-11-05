package triggers

import (
	"crypto/rand"
	"fmt"
	"time"

	"github.com/open-southeners/lambdary/internal/queue"
)

// sqsRecord is one record of the event an SQS event source mapping sends.
type sqsRecord struct {
	MessageID              string                         `json:"messageId"`
	ReceiptHandle          string                         `json:"receiptHandle"`
	Body                   string                         `json:"body"`
	Attributes             map[string]string              `json:"attributes"`
	MessageAttributes      map[string]sqsMessageAttribute `json:"messageAttributes"`
	MD5OfBody              string                         `json:"md5OfBody"`
	MD5OfMessageAttributes string                         `json:"md5OfMessageAttributes,omitempty"`
	EventSource            string                         `json:"eventSource"`
	EventSourceARN         string                         `json:"eventSourceARN"`
	AWSRegion              string                         `json:"awsRegion"`
}

// sqsMessageAttribute is a message attribute as Lambda's SQS event spells
// it: camelCase, with the list fields AWS always includes.
type sqsMessageAttribute struct {
	StringValue      *string  `json:"stringValue,omitempty"`
	BinaryValue      []byte   `json:"binaryValue,omitempty"`
	StringListValues []string `json:"stringListValues"`
	BinaryListValues [][]byte `json:"binaryListValues"`
	DataType         string   `json:"dataType"`
}

type sqsEventPayload struct {
	Records []sqsRecord `json:"Records"`
}

// sqsEvent builds the event Lambda delivers for msgs read from queueName.
func sqsEvent(queueName string, msgs []queue.Message) sqsEventPayload {
	records := make([]sqsRecord, len(msgs))

	for i, m := range msgs {
		attrs := map[string]sqsMessageAttribute{}
		for name, a := range m.Attributes {
			attrs[name] = sqsMessageAttribute{
				StringValue:      a.StringValue,
				BinaryValue:      a.BinaryValue,
				StringListValues: []string{},
				BinaryListValues: [][]byte{},
				DataType:         a.DataType,
			}
		}

		records[i] = sqsRecord{
			MessageID:              m.ID,
			ReceiptHandle:          m.ReceiptHandle,
			Body:                   m.Body,
			Attributes:             queue.SystemAttributes(m),
			MessageAttributes:      attrs,
			MD5OfBody:              m.MD5OfBody,
			MD5OfMessageAttributes: m.MD5OfMessageAttributes,
			EventSource:            "aws:sqs",
			EventSourceARN:         queue.ARN(queueName),
			AWSRegion:              queue.Region,
		}
	}

	return sqsEventPayload{Records: records}
}

// scheduledEventPayload is EventBridge's "Scheduled Event".
type scheduledEventPayload struct {
	Version    string         `json:"version"`
	ID         string         `json:"id"`
	DetailType string         `json:"detail-type"`
	Source     string         `json:"source"`
	Account    string         `json:"account"`
	Time       string         `json:"time"`
	Region     string         `json:"region"`
	Resources  []string       `json:"resources"`
	Detail     map[string]any `json:"detail"`
}

// scheduledEvent builds the event a schedule sends when it fires at t.
func scheduledEvent(rule string, t time.Time) scheduledEventPayload {
	return scheduledEventPayload{
		Version:    "0",
		ID:         newID(),
		DetailType: "Scheduled Event",
		Source:     "aws.events",
		Account:    queue.AccountID,
		Time:       t.UTC().Format(time.RFC3339),
		Region:     queue.Region,
		Resources:  []string{rule},
		Detail:     map[string]any{},
	}
}

// newID returns a random UUID.
func newID() string {
	var b [16]byte
	rand.Read(b[:]) //nolint:errcheck // crypto/rand.Read never fails.

	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80

	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
