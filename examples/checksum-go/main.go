// POST /checksum — returns the SHA-256 of whatever's in the request body.
//
// This is a `provided.al2023` custom runtime: a compiled binary named
// "bootstrap" that speaks AWS's Lambda Runtime API loop directly, with no
// language shim in between (contrast the Node/Python examples, which lean
// on Lambdary's built-in shims). It's also pinned to the container
// backend, so it always runs inside the real
// public.ecr.aws/lambda/provided:al2023 image via Docker — proof Lambdary
// drives the same RIE any language can, as long as it follows this
// contract.
//
// Runtime API (frozen, versioned — see DESIGN.md's "Background" section):
//
//	GET  $AWS_LAMBDA_RUNTIME_API/2018-06-01/runtime/invocation/next
//	POST .../invocation/<id>/response   on success
//	POST .../invocation/<id>/error      on failure
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

// event is the subset of the Function URL v2.0 event Lambdary delivers
// that this handler cares about — see README.md's "How it works" section
// for the full shape.
type event struct {
	Body            string `json:"body"`
	IsBase64Encoded bool   `json:"isBase64Encoded"`
}

type response struct {
	StatusCode int               `json:"statusCode"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

func main() {
	runtimeAPI := os.Getenv("AWS_LAMBDA_RUNTIME_API")
	if runtimeAPI == "" {
		log.Fatal("checksum-go: AWS_LAMBDA_RUNTIME_API is required")
	}

	base := "http://" + runtimeAPI + "/2018-06-01/runtime"

	for {
		requestID, ev, err := next(base)
		if err != nil {
			log.Fatalf("checksum-go: fetching next invocation: %v", err)
		}

		resp, err := handle(ev)
		if err != nil {
			if postErr := postError(base, requestID, err); postErr != nil {
				log.Printf("checksum-go: reporting error for %s: %v", requestID, postErr)
			}

			continue
		}

		if err := postJSON(base+"/invocation/"+requestID+"/response", resp); err != nil {
			log.Printf("checksum-go: posting response for %s: %v", requestID, err)
		}
	}
}

// next long-polls for the next invocation, returning its request ID
// (carried in the Lambda-Runtime-Aws-Request-Id header) and decoded event.
func next(base string) (string, event, error) {
	resp, err := http.Get(base + "/invocation/next")
	if err != nil {
		return "", event{}, err
	}
	defer resp.Body.Close()

	requestID := resp.Header.Get("Lambda-Runtime-Aws-Request-Id")

	var ev event
	if err := json.NewDecoder(resp.Body).Decode(&ev); err != nil {
		return requestID, event{}, fmt.Errorf("decoding event: %w", err)
	}

	return requestID, ev, nil
}

// handle decodes the request body (base64 when Lambdary marked it binary)
// and returns its SHA-256 checksum as a Function URL response.
func handle(ev event) (response, error) {
	raw := []byte(ev.Body)
	if ev.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(ev.Body)
		if err != nil {
			return response{}, fmt.Errorf("decoding base64 body: %w", err)
		}

		raw = decoded
	}

	sum := sha256.Sum256(raw)

	body, err := json.Marshal(map[string]any{
		"sha256": hex.EncodeToString(sum[:]),
		"bytes":  len(raw),
	})
	if err != nil {
		return response{}, fmt.Errorf("encoding response: %w", err)
	}

	return response{
		StatusCode: 200,
		Headers:    map[string]string{"content-type": "application/json"},
		Body:       string(body),
	}, nil
}

func postJSON(url string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}

	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	_, err = io.Copy(io.Discard, resp.Body)

	return err
}

func postError(base, requestID string, handlerErr error) error {
	return postJSON(base+"/invocation/"+requestID+"/error", map[string]string{
		"errorMessage": handlerErr.Error(),
		"errorType":    "HandlerError",
	})
}
