package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// configFileName is the project-wide config file lambdary looks for at the
// --root directory, per DESIGN.md.
const configFileName = "lambdary.yml"

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List discovered functions",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(cmd.OutOrStdout(), cmd.ErrOrStderr(), root)
		},
	}
}

// runList resolves the scan root and project config from root, runs
// discovery, and renders the result to out/errOut.
func runList(out, errOut io.Writer, root string) error {
	scanRoot := root

	cfgPath := filepath.Join(root, configFileName)

	var cfg *manifest.Config

	if _, err := os.Stat(cfgPath); err == nil {
		loaded, err := manifest.LoadConfig(cfgPath)
		if err != nil {
			return err
		}

		cfg = loaded
		scanRoot = filepath.Join(root, cfg.Root)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("list: %s: %w", cfgPath, err)
	}

	fns, err := discovery.Discover(scanRoot, cfg)
	if err != nil {
		return err
	}

	renderList(out, errOut, fns, scanRoot)

	return nil
}

// renderList prints fns as an aligned table (NAME/RUNTIME/BACKEND/ROUTE/
// DIR) to out, then prints every function's warnings to errOut afterward,
// one per line, prefixed "warning: ". Dirs are printed relative to
// scanRoot when possible; an empty Runtime prints as "-". When fns is
// empty, it prints a friendly message to errOut instead of a table.
func renderList(out, errOut io.Writer, fns []discovery.Function, scanRoot string) {
	if len(fns) == 0 {
		fmt.Fprintf(errOut, "no functions found under %s\n", scanRoot)
		return
	}

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tRUNTIME\tBACKEND\tROUTE\tDIR")

	for _, fn := range fns {
		fnRuntime := fn.Runtime
		if fnRuntime == "" {
			fnRuntime = "-"
		}

		dir := fn.Dir
		if rel, err := filepath.Rel(scanRoot, fn.Dir); err == nil {
			dir = rel
		}

		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", fn.Name, fnRuntime, fn.Backend, fn.Route, dir)
	}

	tw.Flush()

	for _, fn := range fns {
		for _, w := range fn.Warnings {
			fmt.Fprintf(errOut, "warning: %s\n", w)
		}
	}
}
