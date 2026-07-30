// Package cli implements the lambdary command-line interface.
package cli

import (
	"github.com/spf13/cobra"
)

// version is the lambdary release version, set at build time via
// -X github.com/open-southeners/lambdary/internal/cli.version=...
var version = "dev"

// root is the functions root folder, shared by all subcommands.
var root string

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "lambdary",
		Short:         "Local development server for AWS Lambda functions",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.PersistentFlags().StringVar(&root, "root", ".", "functions root folder")

	cmd.AddCommand(newListCmd())
	cmd.AddCommand(newDevCmd())
	cmd.AddCommand(newInvokeCmd())

	return cmd
}

// Execute runs the lambdary root command, printing any error before
// returning it to the caller.
func Execute() error {
	cmd := newRootCmd()
	if err := cmd.Execute(); err != nil {
		cmd.PrintErrln(err)
		return err
	}
	return nil
}
