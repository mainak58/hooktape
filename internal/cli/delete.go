package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mainak58/hooktape/internal/store"
	"github.com/spf13/cobra"
)

var (
	deleteFrom       string
	deleteTo         string
	deleteOlderThan  string
	deleteEndpoint   string
	deleteEverything bool
	deleteDryRun     bool
	deleteYes        bool
)

var deleteCmd = &cobra.Command{
	Use:   "delete [<id>...]",
	Short: "Delete stored requests by id, date range, age, or endpoint",
	Long: `Requests are kept until you delete them. Pick what goes by id, by date, by
age, by endpoint, or all of it. Dates are in local time, and a --to with no time
covers that whole day.`,
	Example: `  hooktape delete 12 13 14
  hooktape delete --from 2026-01-01 --to 2026-03-31
  hooktape delete --to 2026-01-01
  hooktape delete --from "2026-03-01 09:00" --to "2026-03-01 17:30"
  hooktape delete --older-than 30d
  hooktape delete --endpoint stripe --from 2026-01-01
  hooktape delete --all`,
	RunE: runDelete,
}

func init() {
	flags := deleteCmd.Flags()
	flags.StringVar(&deleteFrom, "from", "", "delete from this date or time on")
	flags.StringVar(&deleteTo, "to", "", "delete up to and including this date or time")
	flags.StringVar(&deleteOlderThan, "older-than", "", "delete requests older than this (e.g. 30d or 48h)")
	flags.StringVar(&deleteEndpoint, "endpoint", "", "only delete requests for this endpoint")
	flags.BoolVar(&deleteEverything, "all", false, "delete every request")
	flags.BoolVar(&deleteDryRun, "dry-run", false, "print how many would go and delete nothing")
	flags.BoolVarP(&deleteYes, "yes", "y", false, "do not ask for confirmation")
	rootCmd.AddCommand(deleteCmd)
}

func runDelete(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true

	// An unset shell variable must not quietly widen what gets deleted.
	for _, name := range []string{"from", "to", "older-than", "endpoint"} {
		if flag := cmd.Flags().Lookup(name); flag.Changed && flag.Value.String() == "" {
			return fmt.Errorf("--%s is empty", name)
		}
	}

	now := time.Now()

	ids, filter, label, err := deleteSelection(args, now)
	if err != nil {
		return err
	}

	// Rows caught while the prompt waits were not part of the count it showed.
	if filter.ToMs == 0 {
		filter.ToMs = now.UnixMilli() + 1
	}

	s, err := openExisting()
	if err != nil {
		return err
	}
	defer s.Close()

	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	var count int64
	if len(ids) > 0 {
		count, err = countIDs(ctx, s, ids)
	} else {
		count, err = s.CountMatching(ctx, filter)
	}
	if err != nil {
		return err
	}

	if deleteDryRun {
		fmt.Fprintf(out, "Would delete %d request(s) (%s).\n", count, label)
		return nil
	}

	// This also runs when nothing matches, because that is how rows deleted from
	// the dashboard, which never vacuums, get scrubbed from the file.
	shrink := func() {
		if err := s.Vacuum(ctx); err != nil {
			fmt.Fprintf(out, "warning: could not shrink %s: %v\n", dbPath, err)
		}
	}

	if count == 0 {
		fmt.Fprintf(out, "Nothing to delete (%s).\n", label)
		shrink()
		return nil
	}
	if !deleteYes && !confirm(cmd, count, label) {
		return errors.New("nothing deleted")
	}

	var deleted int64
	if len(ids) > 0 {
		deleted, err = s.DeleteByIDs(ctx, ids)
	} else {
		deleted, err = s.DeleteMatching(ctx, filter)
	}
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Deleted %d request(s).\n", deleted)

	shrink()

	return nil
}

// deleteSelection turns the arguments into ids or a filter, plus a description
// for the prompt. It refuses anything ambiguous, and an empty filter matches
// every row, so asking for nothing has to be an error here.
func deleteSelection(args []string, now time.Time) ([]int64, store.DeleteFilter, string, error) {
	var filter store.DeleteFilter

	filtered := deleteFrom != "" || deleteTo != "" || deleteOlderThan != "" || deleteEndpoint != ""

	switch {
	case len(args) > 0 && (filtered || deleteEverything):
		return nil, filter, "", errors.New("ids cannot be combined with --all or a filter")
	case deleteEverything && filtered:
		return nil, filter, "", errors.New("--all cannot be combined with a filter")
	case deleteOlderThan != "" && (deleteFrom != "" || deleteTo != ""):
		return nil, filter, "", errors.New("--older-than cannot be combined with --from or --to")
	case len(args) == 0 && !filtered && !deleteEverything:
		return nil, filter, "", errors.New("nothing selected: pass ids, --from, --to, --older-than, --endpoint, or --all")
	}

	if deleteEverything {
		return nil, filter, "everything", nil
	}

	if len(args) > 0 {
		ids := make([]int64, len(args))
		for i, arg := range args {
			id, err := strconv.ParseInt(arg, 10, 64)
			if err != nil {
				return nil, filter, "", fmt.Errorf("invalid id %q", arg)
			}
			ids[i] = id
		}
		slices.Sort(ids)
		ids = slices.Compact(ids)

		return ids, filter, "ids " + strings.Join(args, " "), nil
	}

	var label []string

	if deleteFrom != "" {
		from, _, err := parseDate("--from", deleteFrom)
		if err != nil {
			return nil, filter, "", err
		}
		filter.FromMs = from.UnixMilli()
		label = append(label, "from "+deleteFrom)
	}

	if deleteTo != "" {
		to, dateOnly, err := parseDate("--to", deleteTo)
		if err != nil {
			return nil, filter, "", err
		}

		// ToMs is exclusive. AddDate rather than 24h, since a day can be 23 or
		// 25 hours long across a clock change.
		if dateOnly {
			filter.ToMs = to.AddDate(0, 0, 1).UnixMilli()
		} else {
			filter.ToMs = to.UnixMilli() + 1
		}
		label = append(label, "to "+deleteTo)
	}

	if deleteOlderThan != "" {
		age, err := parseAge(deleteOlderThan)
		if err != nil {
			return nil, filter, "", err
		}
		filter.ToMs = now.Add(-age).UnixMilli()
		label = append(label, "older than "+deleteOlderThan)
	}

	// Zero means unbounded to the store, so a bound at the epoch would quietly
	// widen the delete to every row.
	if (deleteFrom != "" && filter.FromMs <= 0) || ((deleteTo != "" || deleteOlderThan != "") && filter.ToMs <= 0) {
		return nil, filter, "", errors.New("dates before 1970 are not supported")
	}
	if deleteFrom != "" && deleteTo != "" && filter.FromMs >= filter.ToMs {
		return nil, filter, "", fmt.Errorf("--from %s is after --to %s", deleteFrom, deleteTo)
	}

	if deleteEndpoint != "" {
		filter.Endpoint = deleteEndpoint
		label = append(label, "endpoint "+deleteEndpoint)
	}

	return nil, filter, strings.Join(label, ", "), nil
}

func countIDs(ctx context.Context, s *store.Store, ids []int64) (int64, error) {
	var count int64
	for _, id := range ids {
		_, err := s.Get(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return 0, err
		}
		count++
	}

	return count, nil
}

func confirm(cmd *cobra.Command, count int64, label string) bool {
	fmt.Fprintf(cmd.OutOrStdout(), "Delete %d request(s) (%s)? [y/N] ", count, label)

	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))

	return answer == "y" || answer == "yes"
}

var timeLayouts = []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02T15:04"}

// parseDate reports whether value had no time part, which --to needs to tell a
// whole day from an instant.
// ponytail: a date is read as local midnight, so in a zone whose clock change
// skips midnight (Havana, Santiago) that day's bound is an hour off.
func parseDate(flag, value string) (time.Time, bool, error) {
	if t, err := time.ParseInLocation("2006-01-02", value, time.Local); err == nil {
		return t, true, nil
	}

	for _, layout := range timeLayouts {
		if t, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return t, false, nil
		}
	}

	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, false, nil
	}

	return time.Time{}, false, fmt.Errorf(`invalid %s %q: use 2026-01-31 or "2026-01-31 15:04"`, flag, value)
}

func parseAge(value string) (time.Duration, error) {
	invalid := fmt.Errorf("invalid --older-than %q: use something like 30d or 48h", value)

	if days, ok := strings.CutSuffix(value, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		// Negated so that NaN, which fails every comparison, is rejected too.
		if err != nil || !(n > 0 && n*24*float64(time.Hour) < math.MaxInt64) {
			return 0, invalid
		}

		// Compared again because a tiny value truncates to zero, which is every row.
		if d := time.Duration(n * 24 * float64(time.Hour)); d > 0 {
			return d, nil
		}

		return 0, invalid
	}

	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, invalid
	}

	return d, nil
}
