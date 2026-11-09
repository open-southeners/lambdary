// Package runtime is a minimal, stdlib-only Lambda Runtime API loop, so the
// fixture builds without fetching aws-lambda-go.
package runtime

import (
	"bytes"
	"io"
	"net/http"
	"os"
)

// Serve answers every invocation with handle's result.
func Serve(handle func(event []byte) []byte) {
	base := "http://" + os.Getenv("AWS_LAMBDA_RUNTIME_API") + "/2018-06-01/runtime/invocation/"

	for {
		next, err := http.Get(base + "next")
		if err != nil {
			os.Exit(1)
		}

		event, _ := io.ReadAll(next.Body)
		next.Body.Close()

		id := next.Header.Get("Lambda-Runtime-Aws-Request-Id")

		done, err := http.Post(base+id+"/response", "application/json", bytes.NewReader(handle(event)))
		if err != nil {
			os.Exit(1)
		}
		done.Body.Close()
	}
}

// Record appends line to the file named by $OUT_FILE.
func Record(line string) {
	f, err := os.OpenFile(os.Getenv("OUT_FILE"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	f.WriteString(line + "\n")
}
