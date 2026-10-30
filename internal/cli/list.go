package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/mainak58/hooktape/internal/model"
	"github.com/mainak58/hooktape/internal/render"
	"github.com/mainak58/hooktape/internal/store"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

var (
	listLimit    int
	listEndpoint string
	listJSON     bool
	listWatch    bool
)

var listCmd = &cobra.Command{
	Use:     "list",
	Short:   "List captured requests, newest first",
	Example: "  hooktape list --limit 10 --endpoint stripe\n  hooktape list --watch",
	Args:    cobra.NoArgs,
	RunE:    runList,
}

func init() {
	flags := listCmd.Flags()
	flags.IntVar(&listLimit, "limit", 50, "maximum number of requests to show")
	flags.StringVar(&listEndpoint, "endpoint", "", "only show requests for this endpoint")
	flags.BoolVar(&listJSON, "json", false, "print a JSON array without bodies")
	flags.BoolVar(&listWatch, "watch", false, "reprint the table every 2s until Ctrl+C")
	listCmd.MarkFlagsMutuallyExclusive("json", "watch")
	rootCmd.AddCommand(listCmd)
}

func runList(cmd *cobra.Command, args []string) error {
	s, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer s.Close()

	if !listWatch {
		return printList(cmd.Context(), cmd, s)
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cmd.SilenceUsage = true
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for ctx.Err() == nil {
		clearScreen(cmd)
		if err := printList(ctx, cmd, s); err != nil && ctx.Err() == nil {
			return err
		}

		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}

	return nil
}

func clearScreen(cmd *cobra.Command) {
	if file, ok := cmd.OutOrStdout().(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		fmt.Fprint(file, "\033[H\033[2J")
	}
}

func printList(ctx context.Context, cmd *cobra.Command, s *store.Store) error {
	requests, err := s.List(ctx, store.ListOptions{Limit: listLimit, Endpoint: listEndpoint})
	if err != nil {
		return err
	}

	out := cmd.OutOrStdout()

	if listJSON {
		return json.NewEncoder(out).Encode(requests)
	}

	if len(requests) == 0 {
		fmt.Fprintln(out, `No requests yet. Run "hooktape up" and send a webhook to /h/<endpoint>/...`)
		return nil
	}

	colors := paletteFor(cmd)
	now := time.Now()

	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(table, "ID\tTIME\tENDPOINT\tMETHOD\tPATH\tSIZE\tAPP")
	for _, r := range requests {
		fmt.Fprintf(table, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, formatTime(r.CreatedAt, now), r.Endpoint, r.Method, r.Path,
			render.HumanSize(r.BodySize), appLabel(colors, &r))
	}

	return table.Flush()
}

func formatTime(createdAt int64, now time.Time) string {
	t := time.UnixMilli(createdAt).In(now.Location())

	y, m, d := t.Date()
	nowY, nowM, nowD := now.Date()
	if y == nowY && m == nowM && d == nowD {
		return t.Format("15:04:05")
	}

	return t.Format("Jan 02 15:04")
}

func appLabel(colors render.Palette, r *model.Request) string {
	label := render.ForwardLabel(r.ForwardStatus, r.ForwardError)

	switch {
	case label == "-":
		return colors.Dim(label)
	case r.ForwardStatus >= 200 && r.ForwardStatus < 300:
		return colors.Green(label)
	case r.ForwardStatus >= 300 && r.ForwardStatus < 400:
		return colors.Yellow(label)
	default:
		return colors.Red(label)
	}
}
