package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeManifest(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), ".lambda.yml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestLoadEvents(t *testing.T) {
	m, err := Load(writeManifest(t, `
events:
  - sqs:
      queue: notifications
      batch_size: 5
      function_response_types: [ReportBatchItemFailures]
      visibility_timeout: 10
      dlq: notifications-dlq
      max_receive_count: 3
  - schedule: rate(1 minute)
  - schedule:
      expression: cron(0 9 ? * MON-FRI *)
      timezone: Europe/Madrid
      input: '{"kind":"digest"}'
      enabled: false
`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	no := false
	want := []Event{
		{SQS: &SQSEvent{
			Queue:                 "notifications",
			BatchSize:             5,
			FunctionResponseTypes: []string{ReportBatchItemFailures},
			QueueSettings:         QueueSettings{VisibilityTimeout: 10, DLQ: "notifications-dlq", MaxReceiveCount: 3},
		}},
		{Schedule: &ScheduleEvent{Expression: "rate(1 minute)"}},
		{Schedule: &ScheduleEvent{Expression: "cron(0 9 ? * MON-FRI *)", Timezone: "Europe/Madrid", Input: `{"kind":"digest"}`, Enabled: &no}},
	}

	if !reflect.DeepEqual(m.Events, want) {
		t.Errorf("Events = %#v, want %#v", m.Events, want)
	}

	if !m.Events[0].SQS.ReportsBatchItemFailures() || !m.Events[0].SQS.IsEnabled() {
		t.Error("the sqs event should report batch item failures and be enabled")
	}

	if m.Events[2].Schedule.IsEnabled() {
		t.Error("the disabled schedule reports enabled")
	}
}

func TestLoadEventsErrors(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{"no source", "events:\n  - {}\n", "exactly one"},
		{"two sources", "events:\n  - sqs: {queue: a}\n    schedule: rate(1 minute)\n", "exactly one"},
		{"missing queue", "events:\n  - sqs: {batch_size: 1}\n", "queue is required"},
		{"fifo queue", "events:\n  - sqs: {queue: a.fifo}\n", "FIFO"},
		{"big batch without window", "events:\n  - sqs: {queue: a, batch_size: 100}\n", "batch_window"},
		{"bad response type", "events:\n  - sqs: {queue: a, function_response_types: [Nope]}\n", "ReportBatchItemFailures"},
		{"dlq without count", "events:\n  - sqs: {queue: a, dlq: b}\n", "go together"},
		{"own dlq", "events:\n  - sqs: {queue: a, dlq: a, max_receive_count: 2}\n", "own dlq"},
		{"bad expression", "events:\n  - schedule: rate(1 minutes)\n", "singular"},
		{"bad timezone", "events:\n  - schedule: {expression: rate(1 minute), timezone: Mars/Base}\n", "timezone"},
		{"bad input", "events:\n  - schedule: {expression: rate(1 minute), input: '{'}\n", "valid JSON"},
		{"unknown schedule key", "events:\n  - schedule: {expression: rate(1 minute), every: 1}\n", "every"},
		{"unknown sqs key", "events:\n  - sqs: {queue: a, batchsize: 1}\n", "batchsize"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeManifest(t, tt.yaml))
			if err == nil {
				t.Fatalf("Load succeeded, want an error mentioning %q", tt.want)
			}

			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestLoadConfigQueues(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, `
queues:
  probe-results: {visibility_timeout: 60}
  deploy-requests: {dlq: deploy-requests-dlq, max_receive_count: 5}
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if got := c.Queues["deploy-requests"].Settings(); got.DeadLetterQueue != "deploy-requests-dlq" || got.MaxReceiveCount != 5 {
		t.Errorf("deploy-requests settings = %+v", got)
	}

	if got := c.Queues["probe-results"].Settings().VisibilityTimeout.Seconds(); got != 60 {
		t.Errorf("probe-results visibility = %vs, want 60", got)
	}

	_, err = LoadConfig(writeConfig(t, "queues:\n  bad.fifo: {}\n"))
	if !errors.Is(err, ErrInvalidQueue) {
		t.Errorf("LoadConfig with a FIFO queue = %v, want ErrInvalidQueue", err)
	}
}

func TestFunctionEntryEvents(t *testing.T) {
	c, err := LoadConfig(writeConfig(t, `
functions:
  probe-dispatch:
    go: ./cmd/probe-dispatch
    events:
      - schedule: rate(1 minute)
`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if ev := c.Functions["probe-dispatch"].Events; len(ev) != 1 || ev[0].Schedule.Expression != "rate(1 minute)" {
		t.Errorf("entry events = %#v", ev)
	}

	if _, err := LoadConfig(writeConfig(t, "functions:\n  x:\n    go: ./cmd/x\n    events:\n      - schedule: nope\n")); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("LoadConfig with a bad entry event = %v, want ErrInvalidEvent", err)
	}

	m := &Manifest{}
	m.ApplyOverrides(Manifest{Events: c.Functions["probe-dispatch"].Events})
	if len(m.Events) != 1 {
		t.Error("ApplyOverrides did not copy events")
	}
}
