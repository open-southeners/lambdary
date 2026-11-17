// Package cli implements the lambdary command-line interface.
package cli

import (
	"os"

	"github.com/spf13/cobra"
)

// version is the lambdary release version, set at build time via
// -X github.com/open-southeners/lambdary/internal/cli.version=...
var version = "dev"

// EnvRoot names the environment variable that sets the default --root, so a
// command run with docker exec uses the same root as the dev server.
const EnvRoot = "LAMBDARY_ROOT"

// root is the functions root folder, shared by all subcommands.
var root string

// defaultRoot returns --root's default: $LAMBDARY_ROOT when set, else the
// current directory. An explicit --root overrides it.
func defaultRoot() string {
	if v := os.Getenv(EnvRoot); v != "" {
		return v
	}

	return "."
}

func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "lambdary",
		Short:         "Local development server for AWS Lambda functions",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	cmd.PersistentFlags().StringVar(&root, "root", defaultRoot(), "functions root folder (default $"+EnvRoot+", else .)")

	cmd.AddCommand(newListCmd())
	cmd.AddCommand(newDevCmd())
	cmd.AddCommand(newInvokeCmd())
	cmd.AddCommand(newInitCmd())
	cmd.AddCommand(newLogsCmd())

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
