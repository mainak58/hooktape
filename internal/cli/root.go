package cli

import (
	"os"

	"github.com/spf13/cobra"
)

var (
	dbPath  string
	noColor bool
	verbose bool
)

var rootCmd = &cobra.Command{
	Use:   "hooktape",
	Short: "Catch, store, and replay webhooks on your own machine",
}

func init() {
	defaultDB := os.Getenv("HOOKTAPE_DB")
	if defaultDB == "" {
		defaultDB = "./hooktape.db"
	}

	flags := rootCmd.PersistentFlags()
	flags.StringVar(&dbPath, "db", defaultDB, "path to the SQLite database (env HOOKTAPE_DB)")
	flags.BoolVar(&noColor, "no-color", false, "disable coloured output")
	flags.BoolVarP(&verbose, "verbose", "v", false, "print extra detail")

}

func Execute() error {
	return rootCmd.Execute()
}
