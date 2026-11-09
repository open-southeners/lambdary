package manifest

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/open-southeners/lambdary/internal/queue"
	"github.com/open-southeners/lambdary/internal/schedule"
)

// ReportBatchItemFailures is the only function response type SQS event
// source mappings support.
const ReportBatchItemFailures = "ReportBatchItemFailures"

// Errors returned by event and queue validation.
var (
	// ErrInvalidEvent indicates an events entry that sets none or more than
	// one source, or a source with invalid settings.
	ErrInvalidEvent = errors.New("invalid event source")
	// ErrInvalidQueue indicates a queues entry with invalid settings.
	ErrInvalidQueue = errors.New("invalid queue")
)

// Event is one event source that invokes the function locally, mirroring an
// event source mapping or a schedule on AWS. Exactly one source is set.
type Event struct {
	SQS      *SQSEvent      `yaml:"sqs,omitempty"`
	Schedule *ScheduleEvent `yaml:"schedule,omitempty"`
}

// QueueSettings are an SQS queue's attributes, settable where a queue is
// declared or consumed.
type QueueSettings struct {
	// VisibilityTimeout is how long, in seconds, a received message stays
	// hidden before it can be received again. 0 means SQS's default, 30.
	VisibilityTimeout int `yaml:"visibility_timeout,omitempty"`
	// DelaySeconds delays every new message by this many seconds.
	DelaySeconds int `yaml:"delay_seconds,omitempty"`
	// DLQ and MaxReceiveCount form the queue's redrive policy: a message
	// received more than MaxReceiveCount times moves to DLQ.
	DLQ             string `yaml:"dlq,omitempty"`
	MaxReceiveCount int    `yaml:"max_receive_count,omitempty"`
}

// Settings converts s to the queue package's settings.
func (s QueueSettings) Settings() queue.Settings {
	return queue.Settings{
		VisibilityTimeout: time.Duration(s.VisibilityTimeout) * time.Second,
		Delay:             time.Duration(s.DelaySeconds) * time.Second,
		DeadLetterQueue:   s.DLQ,
		MaxReceiveCount:   s.MaxReceiveCount,
	}
}

// SQSEvent delivers messages from a local SQS queue to the function in
// batches, as an SQS event source mapping does. Its queue settings apply to
// the queue it names, which is created when the dev server starts.
type SQSEvent struct {
	// Queue is the queue name.
	Queue string `yaml:"queue"`
	// BatchSize is the most messages per invocation, 1-10000; default 10.
	BatchSize int `yaml:"batch_size,omitempty"`
	// BatchWindow is how long, in seconds, to keep gathering messages
	// before invoking with a partial batch, as MaximumBatchingWindowInSeconds.
	BatchWindow int `yaml:"batch_window,omitempty"`
	// FunctionResponseTypes may hold ReportBatchItemFailures, so the
	// function can fail single messages through batchItemFailures.
	FunctionResponseTypes []string `yaml:"function_response_types,omitempty"`
	// Qualifier is the version or alias to invoke; default $LATEST.
	Qualifier string `yaml:"qualifier,omitempty"`
	// Enabled set to false keeps the mapping from polling.
	Enabled *bool `yaml:"enabled,omitempty"`

	QueueSettings `yaml:",inline"`
}

// DefaultBatchSize is an SQS event source mapping's default batch size.
const DefaultBatchSize = 10

// ReportsBatchItemFailures reports whether the function answers with
// batchItemFailures.
func (e *SQSEvent) ReportsBatchItemFailures() bool {
	for _, t := range e.FunctionResponseTypes {
		if t == ReportBatchItemFailures {
			return true
		}
	}

	return false
}

// IsEnabled reports whether the mapping polls.
func (e *SQSEvent) IsEnabled() bool { return e.Enabled == nil || *e.Enabled }

// ScheduleEvent invokes the function on an EventBridge schedule expression.
// It can be written as the expression alone.
type ScheduleEvent struct {
	// Expression is rate(...), cron(...) or at(...), in AWS's syntax.
	Expression string `yaml:"expression"`
	// Timezone is an IANA time zone for cron and at; default UTC.
	Timezone string `yaml:"timezone,omitempty"`
	// Input, when set, is the JSON sent instead of the Scheduled Event.
	Input string `yaml:"input,omitempty"`
	// Qualifier is the version or alias to invoke; default $LATEST.
	Qualifier string `yaml:"qualifier,omitempty"`
	// Enabled set to false keeps the schedule from firing.
	Enabled *bool `yaml:"enabled,omitempty"`
}

// IsEnabled reports whether the schedule fires.
func (e *ScheduleEvent) IsEnabled() bool { return e.Enabled == nil || *e.Enabled }

// Location returns the schedule's time zone, UTC when unset.
func (e *ScheduleEvent) Location() (*time.Location, error) {
	if e.Timezone == "" {
		return time.UTC, nil
	}

	return time.LoadLocation(e.Timezone)
}

// scheduleKeys are ScheduleEvent's YAML keys, checked by hand because a
// custom unmarshaler loses the decoder's known-fields check.
var scheduleKeys = map[string]bool{"expression": true, "timezone": true, "input": true, "qualifier": true, "enabled": true}

// UnmarshalYAML accepts the expression alone or the full mapping.
func (e *ScheduleEvent) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		e.Expression = node.Value
		return nil
	}

	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			if key := node.Content[i].Value; !scheduleKeys[key] {
				return fmt.Errorf("line %d: field %s not found in type manifest.ScheduleEvent", node.Content[i].Line, key)
			}
		}
	}

	type plain ScheduleEvent

	return node.Decode((*plain)(e))
}

// validateEvents checks a function's events.
func validateEvents(events []Event) error {
	for i, ev := range events {
		if err := validateEvent(ev); err != nil {
			return fmt.Errorf("%w: events[%d]: %s", ErrInvalidEvent, i, err)
		}
	}

	return nil
}

func validateEvent(ev Event) error {
	switch {
	case (ev.SQS == nil) == (ev.Schedule == nil):
		return errors.New("set exactly one of sqs or schedule")
	case ev.SQS != nil:
		return validateSQS(ev.SQS)
	default:
		return validateSchedule(ev.Schedule)
	}
}

func validateSQS(e *SQSEvent) error {
	if e.Queue == "" {
		return errors.New("sqs.queue is required")
	}

	if e.BatchSize < 0 || e.BatchSize > 10000 {
		return fmt.Errorf("sqs.batch_size must be 1-10000, got %d", e.BatchSize)
	}

	if e.BatchWindow < 0 || e.BatchWindow > 300 {
		return fmt.Errorf("sqs.batch_window must be 0-300 seconds, got %d", e.BatchWindow)
	}

	if e.BatchSize > DefaultBatchSize && e.BatchWindow == 0 {
		return errors.New("sqs.batch_size above 10 needs a batch_window, as on AWS")
	}

	for _, t := range e.FunctionResponseTypes {
		if t != ReportBatchItemFailures {
			return fmt.Errorf("sqs.function_response_types accepts only %s, got %q", ReportBatchItemFailures, t)
		}
	}

	if err := validateQueue(e.Queue, e.QueueSettings); err != nil {
		return fmt.Errorf("sqs: %s", err)
	}

	return nil
}

func validateSchedule(e *ScheduleEvent) error {
	if e.Expression == "" {
		return errors.New("schedule expression is required")
	}

	if _, err := schedule.Parse(e.Expression); err != nil {
		return err
	}

	if _, err := e.Location(); err != nil {
		return fmt.Errorf("schedule timezone: %s", err)
	}

	if e.Input != "" && !json.Valid([]byte(e.Input)) {
		return errors.New("schedule input must be valid JSON")
	}

	return nil
}

// validateQueue checks a queue's name and settings against SQS's limits.
func validateQueue(name string, s QueueSettings) error {
	if !queue.ValidName(name) {
		return fmt.Errorf("queue name %q must be 1-80 letters, digits, - or _ (FIFO queues are not supported)", name)
	}

	if s.VisibilityTimeout < 0 || s.VisibilityTimeout > 43200 {
		return fmt.Errorf("visibility_timeout must be 0-43200 seconds, got %d", s.VisibilityTimeout)
	}

	if s.DelaySeconds < 0 || s.DelaySeconds > 900 {
		return fmt.Errorf("delay_seconds must be 0-900, got %d", s.DelaySeconds)
	}

	if (s.DLQ == "") != (s.MaxReceiveCount == 0) {
		return errors.New("dlq and max_receive_count go together")
	}

	if s.MaxReceiveCount < 0 || s.MaxReceiveCount > 1000 {
		return fmt.Errorf("max_receive_count must be 1-1000, got %d", s.MaxReceiveCount)
	}

	if s.DLQ != "" && !queue.ValidName(s.DLQ) {
		return fmt.Errorf("dlq name %q is not a valid queue name", s.DLQ)
	}

	if s.DLQ == name {
		return errors.New("a queue cannot be its own dlq")
	}

	return nil
}

// validateQueues checks lambdary.yml's queues block.
func validateQueues(queues map[string]QueueSettings) error {
	names := make([]string, 0, len(queues))
	for name := range queues {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if err := validateQueue(name, queues[name]); err != nil {
			return fmt.Errorf("%w: queues.%s: %s", ErrInvalidQueue, name, err)
		}
	}

	return nil
}
