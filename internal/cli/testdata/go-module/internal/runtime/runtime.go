// Package runtime is a minimal, stdlib-only Lambda Runtime API loop, so the
// fixture builds without fetching aws-lambda-go.
package runtime

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
)

// Serve answers every invocation with an API Gateway v2 response whose body
// is text.
func Serve(text string) {
	base := "http://" + os.Getenv("AWS_LAMBDA_RUNTIME_API") + "/2018-06-01/runtime/invocation/"

	resp, _ := json.Marshal(map[string]any{
		"statusCode": 200,
		"headers":    map[string]string{"Content-Type": "text/plain"},
		"body":       text,
	})

	for {
		next, err := http.Get(base + "next")
		if err != nil {
			os.Exit(1)
		}
		io.Copy(io.Discard, next.Body)
		next.Body.Close()

		id := next.Header.Get("Lambda-Runtime-Aws-Request-Id")

		done, err := http.Post(base+id+"/response", "application/json", bytes.NewReader(resp))
		if err != nil {
			os.Exit(1)
		}
		done.Body.Close()
	}
}
