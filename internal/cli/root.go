package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/mainak58/hooktape/internal/render"
	"github.com/mainak58/hooktape/internal/store"
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
	Long: `hooktape catches webhooks on a local port, stores every request in SQLite,
forwards them to your app, and replays any of them whenever you like.

Start with "hooktape up"; everything else reads or resends what it caught.
Requests are kept until you remove them with "hooktape delete".`,
	Example: `  hooktape up --forward http://localhost:3000
  hooktape list
  hooktape show 12
  hooktape replay 12
  hooktape delete --from 2026-01-01 --to 2026-03-31`,
}

func init() {
	defaultDB := os.Getenv("HOOKTAPE_DB")
	if defaultDB == "" {
		defaultDB = "./hooktape.db"
	}

	flags := rootCmd.PersistentFlags()
	flags.StringVar(&dbPath, "db", defaultDB, "path to the SQLite database (env HOOKTAPE_DB)")
	flags.BoolVar(&noColor, "no-color", false, "disable coloured output")
	flags.BoolVarP(&verbose, "verbose", "v", false, "print bodies (up: each request forwarded, plus cloudflared output; replay: your app's response)")

}

func Execute() error {
	rootCmd.SilenceErrors = true

	err := rootCmd.Execute()
	if err != nil && !errors.Is(err, errQuiet) {
		fmt.Fprintln(rootCmd.ErrOrStderr(), "Error:", err)
	}

	return err
}

// openExisting is for every command that reads what up caught.
func openExisting() (*store.Store, error) {
	s, err := store.OpenExisting(dbPath)
	if errors.Is(err, store.ErrNoDatabase) {
		return nil, fmt.Errorf("no database at %s: check --db, or run hooktape up first", dbPath)
	}

	return s, err
}

// paletteFor turns colour off for --no-color, NO_COLOR, and output that is not
// a terminal, so colour codes never end up in a file or a pipe.
func paletteFor(cmd *cobra.Command) render.Palette {
	file, ok := cmd.OutOrStdout().(*os.File)
	enabled := ok && !noColor && os.Getenv("NO_COLOR") == "" && term.IsTerminal(int(file.Fd()))
	return render.Colors(enabled)
}
