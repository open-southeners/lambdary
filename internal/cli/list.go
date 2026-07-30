package cli

import (
	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List discovered functions",
		RunE: func(cmd *cobra.Command, args []string) error {
			// TODO: implement function discovery and listing
			return nil
		},
	}
}
