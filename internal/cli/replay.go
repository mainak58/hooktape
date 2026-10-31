package cli

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/mainak58/hooktape/internal/config"
	"github.com/mainak58/hooktape/internal/forward"
	"github.com/mainak58/hooktape/internal/render"
	"github.com/spf13/cobra"
)

var (
	replayForward         string
	replayTimeout         time.Duration
	replayForwardPath     bool
	replayKeepCredentials bool
)

var replayCmd = &cobra.Command{
	Use:     "replay <id>...",
	Short:   "Send stored requests to your app again",
	Example: "  hooktape replay 12\n  hooktape replay 3 4 5 --forward http://localhost:3000",
	Args:    cobra.MinimumNArgs(1),
	RunE:    runReplay,
}

func init() {
	flags := replayCmd.Flags()
	flags.StringVar(&replayForward, "forward", "", "base URL to send to (env HOOKTAPE_FORWARD, else the last one used)")
	flags.DurationVar(&replayTimeout, "timeout", 10*time.Second, "timeout for each request")
	flags.BoolVar(&replayForwardPath, "forward-path", false, "append the captured path to the target, as up --forward-path does")
	flags.BoolVar(&replayKeepCredentials, "keep-credentials", false, "send Authorization and Cookie to a non-local target too")
	rootCmd.AddCommand(replayCmd)
}

func runReplay(cmd *cobra.Command, args []string) error {

	cmd.SilenceUsage = true

	ids := make([]int64, len(args))
	for i, arg := range args {
		id, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid id %q", arg)
		}
		ids[i] = id
	}

	target, err := forwardTarget()
	if err != nil {
		return err
	}

	s, err := openExisting()
	if err != nil {
		return err
	}
	defer s.Close()

	out := cmd.OutOrStdout()
	colors := paletteFor(cmd)
	client := forward.NewClient(replayTimeout)
	opts := forward.SendOptions{Replay: true, KeepPath: replayForwardPath, KeepCredentials: replayKeepCredentials}
	allOK := true
	saved := false

	if !replayKeepCredentials && !forward.IsLocalTarget(target) {
		fmt.Fprintln(out, colors.Dim("target is not local: Authorization and Cookie are dropped (--keep-credentials sends them)"))
	}

	for _, id := range ids {
		r, err := s.Get(cmd.Context(), id)
		if err != nil {
			fmt.Fprintln(out, colors.Red("✗ "+unknownID(id, err).Error()))
			allOK = false
			continue
		}

		res := forward.Send(cmd.Context(), client, r, target, opts)

		// Remembered only now, so a typo cannot break every later bare replay.
		if res.Status != 0 && !saved {
			_ = config.Save(config.Config{LastForward: target})
			saved = true
		}

		errMsg := ""
		if res.Err != nil {
			errMsg = res.Err.Error()
		}
		if err := s.SetReplayResult(cmd.Context(), id, res.Status, res.Duration.Milliseconds(), errMsg); err != nil {
			fmt.Fprintln(out, colors.Red("✗ "+unknownID(id, err).Error()))
			allOK = false
		}

		fmt.Fprintln(out, replayLine(colors, id, res))
		if verbose && res.Err == nil {
			fmt.Fprintln(out, render.PrettyBody(res.Body))
		}

		if res.Err != nil || res.Status < 200 || res.Status >= 300 {
			allOK = false
		}
	}

	if !allOK {
		return errQuiet
	}

	return nil
}

func forwardTarget() (string, error) {
	if replayForward != "" {
		return forward.CheckTarget(replayForward)
	}
	if url := os.Getenv("HOOKTAPE_FORWARD"); url != "" {
		return forward.CheckTarget(url)
	}

	c, err := config.Load()
	if err != nil {
		return "", err
	}
	if c.LastForward != "" {
		return forward.CheckTarget(c.LastForward)
	}

	return "", errors.New("no target: pass --forward http://localhost:3000")
}

func replayLine(colors render.Palette, id int64, res forward.Result) string {
	if res.Err != nil {
		return colors.Red(fmt.Sprintf("✗ #%d replay failed: %v", id, res.Err))
	}

	ms := res.Duration.Milliseconds()
	if res.Status >= 200 && res.Status < 300 {
		status := strconv.Itoa(res.Status)
		if text := http.StatusText(res.Status); text != "" {
			status += " " + text
		}
		return colors.Green(fmt.Sprintf("✓ #%d replayed → %s %dms", id, status, ms))
	}

	return colors.Red(fmt.Sprintf("✗ #%d replayed → %d %dms", id, res.Status, ms))
}
