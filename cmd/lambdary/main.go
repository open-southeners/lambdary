// Command lambdary is a local development server for AWS Lambda functions.
package main

import (
	"os"

	"github.com/open-southeners/lambdary/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		os.Exit(1)
	}
}
