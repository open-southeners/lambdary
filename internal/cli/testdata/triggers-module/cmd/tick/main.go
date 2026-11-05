// Command tick records each scheduled event it receives.
package main

import (
	"encoding/json"

	"example.com/triggers/internal/runtime"
)

func main() {
	runtime.Serve(func(event []byte) []byte {
		var ev struct {
			DetailType string `json:"detail-type"`
			Source     string `json:"source"`
		}
		json.Unmarshal(event, &ev)

		runtime.Record(ev.Source + " " + ev.DetailType)

		return []byte("null")
	})
}
