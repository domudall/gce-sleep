package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version number of gce-sleep",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(version)

		if verbose {
			fmt.Println(commit)
			fmt.Println(date)
		}
	},
}
