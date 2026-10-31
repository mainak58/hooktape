package cli

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Overwritten at link time by -ldflags in the Makefile.
var (
	version = "dev"
	commit  = "none"
)

var versionCmd = &cobra.Command{
	Use:     "version",
	Short:   "Print the hooktape version",
	Example: "  hooktape version",
	Args:    cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		v, c := buildVersion()
		fmt.Fprintf(cmd.OutOrStdout(), "hooktape %s (%s)\n", v, c)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}

// buildVersion falls back to what the go tool recorded, for go install builds
// that never went through the Makefile.
func buildVersion() (string, string) {
	v, c := version, commit
	if v != "dev" {
		return v, c
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return v, c
	}

	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		v = info.Main.Version
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			c = setting.Value[:min(len(setting.Value), 7)]
		}
	}

	return v, c
}
