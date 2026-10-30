package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/mainak58/hooktape/internal/render"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var errQuiet = errors.New("")

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
	rootCmd.SilenceErrors = true

	err := rootCmd.Execute()
	if err != nil && !errors.Is(err, errQuiet) {
		fmt.Fprintln(rootCmd.ErrOrStderr(), "Error:", err)
	}

	return err
}

// paletteFor turns colour off for --no-color, NO_COLOR, and output that is not
// a terminal, so colour codes never end up in a file or a pipe.
func paletteFor(cmd *cobra.Command) render.Palette {
	file, ok := cmd.OutOrStdout().(*os.File)
	enabled := ok && !noColor && os.Getenv("NO_COLOR") == "" && term.IsTerminal(int(file.Fd()))
	return render.Colors(enabled)
}
