// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dockwell/dockwell/internal/buildinfo"
)

func newVersionCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show which build of dockwell this is",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := buildinfo.Read()
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(info)
			}
			return writeVersion(cmd.OutOrStdout(), info)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the build as JSON")
	return cmd
}

func writeVersion(w io.Writer, info buildinfo.Info) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "dockwell\t%s\n", info.Version)
	if info.Commit != "" {
		commit := info.ShortCommit()
		if info.Modified {
			commit += " (modified)"
		}
		fmt.Fprintf(tw, "commit\t%s\n", commit)
	}
	if !info.CommitTime.IsZero() {
		fmt.Fprintf(tw, "committed\t%s\n", info.CommitTime.Format("2 Jan 2006 15:04 MST"))
	}
	fmt.Fprintf(tw, "go\t%s\n", info.GoVersion)
	fmt.Fprintf(tw, "platform\t%s\n", info.Platform)
	return tw.Flush()
}
