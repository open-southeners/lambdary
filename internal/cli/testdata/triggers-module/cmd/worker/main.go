// Command worker consumes SQS batches: it records every body and reports the
// ones reading "poison" as batch item failures.
package main

import (
	"encoding/json"

	"example.com/triggers/internal/runtime"
)

func main() {
	runtime.Serve(func(event []byte) []byte {
		var ev struct {
			Records []struct {
				MessageID   string `json:"messageId"`
				Body        string `json:"body"`
				EventSource string `json:"eventSource"`
			} `json:"Records"`
		}
		json.Unmarshal(event, &ev)

		failures := []map[string]string{}

		for _, r := range ev.Records {
			runtime.Record(r.EventSource + " " + r.Body)

			if r.Body == "poison" {
				failures = append(failures, map[string]string{"itemIdentifier": r.MessageID})
			}
		}

		out, _ := json.Marshal(map[string]any{"batchItemFailures": failures})
		return out
	})
}
