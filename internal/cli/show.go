package cli

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/mainak58/hooktape/internal/model"
	"github.com/mainak58/hooktape/internal/render"
	"github.com/mainak58/hooktape/internal/store"
	"github.com/spf13/cobra"
)

var (
	showHeadersOnly bool
	showBodyOnly    bool
	showRaw         bool
)

var showCmd = &cobra.Command{
	Use:     "show <id>",
	Short:   "Show one captured request",
	Example: "  hooktape show 12\n  hooktape show 12 --raw > body.bin",
	Args:    cobra.ExactArgs(1),
	RunE:    runShow,
}

func init() {
	flags := showCmd.Flags()
	flags.BoolVar(&showHeadersOnly, "headers-only", false, "print only the headers")
	flags.BoolVar(&showBodyOnly, "body-only", false, "print only the formatted body")
	flags.BoolVar(&showRaw, "raw", false, "write the exact body bytes to stdout")
	showCmd.MarkFlagsMutuallyExclusive("headers-only", "body-only", "raw")
	rootCmd.AddCommand(showCmd)
}

func runShow(cmd *cobra.Command, args []string) error {
	// Past this point an error is about the request asked for, not how the
	// command was typed.
	cmd.SilenceUsage = true

	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return fmt.Errorf("invalid id %q", args[0])
	}

	s, err := openExisting()
	if err != nil {
		return err
	}
	defer s.Close()

	r, err := s.Get(cmd.Context(), id)
	if err != nil {
		return unknownID(id, err)
	}

	out := cmd.OutOrStdout()
	colors := paletteFor(cmd)
	body := render.PrettyBody(r.Body)

	switch {
	case showRaw:
		_, err := out.Write(r.Body)
		return err
	case showHeadersOnly:
		printHeaders(out, colors, r.Headers)
		return nil
	case showBodyOnly:
		fmt.Fprintln(out, body)
		return nil
	}

	target := r.Path
	if r.Query != "" {
		target += "?" + r.Query
	}

	size := humanize.Bytes(uint64(r.BodySize))
	if r.Truncated {
		size += " truncated at --max-body"
	}

	fmt.Fprintf(out, "%s  %s\n", colors.Bold(r.Method+" "+target), colors.Dim(fmt.Sprintf("(#%d, endpoint %s, %s, %s)",
		r.ID, r.Endpoint, size, formatTime(r.CreatedAt, time.Now()))))
	printHeaders(out, colors, r.Headers)
	fmt.Fprintln(out)
	fmt.Fprintln(out, body)
	fmt.Fprintln(out, resultLine(colors, "app", r.ForwardStatus, r.ForwardMs, r.ForwardError))
	if replayed(r) {
		fmt.Fprintln(out, resultLine(colors, "replay", r.ReplayStatus, r.ReplayMs, r.ReplayError))
	}

	return nil
}

func unknownID(id int64, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no request with id %d (see hooktape list)", id)
	}

	return err
}

func printHeaders(out io.Writer, colors render.Palette, headers map[string][]string) {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		for _, value := range headers[name] {
			fmt.Fprintf(out, "%s: %s\n", colors.Cyan(name), value)
		}
	}
}

func replayed(r *model.Request) bool {
	return r.ReplayStatus != 0 || r.ReplayError != ""
}

func resultLine(colors render.Palette, name string, status int, ms int64, errMsg string) string {
	label := resultLabel(colors, status, errMsg)

	switch {
	case errMsg != "":
		return fmt.Sprintf("%s: %s (%s)", name, label, errMsg)
	case status != 0:
		return fmt.Sprintf("%s: %s in %dms", name, label, ms)
	default:
		return name + ": " + label
	}
}
