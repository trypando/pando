package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// detectionTimeout is how long the CLI waits for detection by default. The
// server gives detection ten minutes (detection.Timeout), so waiting any
// less would give up on a detection that was going to finish.
const detectionTimeout = 10 * time.Minute

// longPoll is how long each request holds out for a change (issue #80). Under
// the server's own limit, so it is never cut short there.
const longPoll = 30

// ExitCode is an error that asks the process to exit with a particular code.
//
// For the outcomes a script needs to tell apart without reading the output:
// `pando app detection --wait` exits 2 when a person has to answer questions
// before the app can be accepted, and 1 when detection failed or was blocked.
type ExitCode struct {
	Code int
	Err  error
}

func (e *ExitCode) Error() string { return e.Err.Error() }
func (e *ExitCode) Unwrap() error { return e.Err }

// ExitCodeOf returns the code err asks the process to exit with: its own, if
// it carries one, and otherwise 1.
func ExitCodeOf(err error) int {
	var coded *ExitCode
	if errors.As(err, &coded) {
		return coded.Code
	}
	return 1
}

// detectionStatus is GET /apps/{id}/detection as the CLI reads it.
type detectionStatus struct {
	Status    string `json:"status"`
	Stage     string `json:"stage"`
	Elapsed   *int   `json:"elapsed_seconds"`
	Detection struct {
		Questions []struct {
			Key      string `json:"key"`
			Prompt   string `json:"prompt"`
			Deferred bool   `json:"deferred"`
		} `json:"questions"`
		Winner struct {
			Detector string `json:"detector"`
			Strategy string `json:"strategy"`
		} `json:"winning_bid"`
		Blocked *APIError `json:"blocked"`
		Error   *APIError `json:"error"`
	} `json:"detection"`
	Answers map[string]string `json:"answers"`

	// Unanswered is the server's own list of the questions still open,
	// present while status is needs_answers. The server decides what counts
	// as answered — an AI adapter's suggestion does (R-338) — so the CLI does
	// not decide again.
	Unanswered *[]string `json:"unanswered"`
}

// running reports whether detection has yet to finish. An empty status is an
// answer from before detection recorded anything.
func (d detectionStatus) running() bool {
	return d.Status == "running" || d.Status == "pending" || d.Status == ""
}

// openQuestions returns the prompts of the questions a person still has to
// answer, verbatim (R-105).
func (d detectionStatus) openQuestions() []string {
	open := map[string]bool{}
	if d.Unanswered != nil {
		for _, key := range *d.Unanswered {
			open[key] = true
		}
	}
	var out []string
	for _, q := range d.Detection.Questions {
		if d.Unanswered != nil {
			if open[q.Key] {
				out = append(out, q.Prompt)
			}
			continue
		}
		if q.Deferred {
			continue
		}
		if _, answered := d.Answers[q.Key]; !answered {
			out = append(out, q.Prompt)
		}
	}
	return out
}

// stagePhrase says what a detection stage is doing, in words.
func stagePhrase(stage string) string {
	switch stage {
	case "fetching":
		return "Fetching the source"
	case "detecting":
		return "Working out what the app is"
	case "trying":
		return "Trying a run of the app"
	case "scanning":
		return "Scanning the source for security problems"
	case "screening":
		return "Having the AI adapter check the plan"
	case "":
		return "Starting"
	default:
		return strings.ToUpper(stage[:1]) + stage[1:]
	}
}

// getDetection reads an app's detection, holding out up to wait seconds for
// it to move on (issue #80).
func (c *Client) getDetection(appID string, wait int) (detectionStatus, error) {
	path := "/apps/" + url.PathEscape(appID) + "/detection"
	if wait > 0 {
		path += fmt.Sprintf("?wait=%d", wait)
	}
	var d detectionStatus
	err := c.Do("GET", path, nil, &d)
	return d, err
}

// awaitDetection waits for an app's detection to finish, calling onStage each
// time it reaches a new stage, and gives up after timeout.
//
// Through the API's long poll, the way the console and MCP wait (R-261):
// each request is answered when detection moves on, so a stage is reported
// as it happens rather than up to a polling interval late.
func (c *Client) awaitDetection(appID string, timeout time.Duration, onStage func(stage string)) (detectionStatus, error) {
	deadline := time.Now().Add(timeout)
	stage := "\x00" // matches no stage, so the first is always reported
	for {
		asked := time.Now()
		d, err := c.getDetection(appID, longPoll)
		if err != nil {
			return d, err
		}
		if !d.running() {
			return d, nil
		}
		moved := d.Stage != stage
		if moved {
			stage = d.Stage
			if onStage != nil {
				onStage(stage)
			}
		}
		if time.Now().After(deadline) {
			return d, fmt.Errorf("detection is still running after %s — `pando app detection %s --wait` to keep waiting",
				timeout, appID)
		}
		// A server from before the long poll answers at once; so, for a
		// moment, does one whose detection has not recorded a stage yet.
		// Pausing then keeps this from turning into a tight loop.
		if !moved && time.Since(asked) < time.Second {
			time.Sleep(2 * time.Second)
		}
	}
}

// printDetection writes where an app's detection stands and what to do next.
func printDetection(w io.Writer, appID string, d detectionStatus) {
	phrase := stagePhrase(d.Stage)
	switch {
	case d.running():
		line := "Running: " + strings.ToLower(phrase[:1]) + phrase[1:]
		if d.Elapsed != nil {
			line += fmt.Sprintf(" (%s so far)", (time.Duration(*d.Elapsed) * time.Second).String())
		}
		fmt.Fprintln(w, line+".")
		fmt.Fprintf(w, "Next: `pando app detection %s --wait` waits for it to finish.\n", appID)

	case d.Status == "ready":
		fmt.Fprintf(w, "Ready: recognized it as %s, built with %s.\n",
			d.Detection.Winner.Detector, d.Detection.Winner.Strategy)
		fmt.Fprintln(w, "Next: review and accept it in the web console, then deploy it.")

	case d.Status == "needs_answers" || d.Status == "unknown":
		open := d.openQuestions()
		if len(open) == 0 {
			fmt.Fprintln(w, "Every question is answered.")
			fmt.Fprintln(w, "Next: review and accept it in the web console, then deploy it.")
			return
		}
		fmt.Fprintln(w, "Pando needs to know a few things before it can deploy this:")
		for _, q := range open {
			// Verbatim (R-105): written to be pasted into whatever wrote the
			// app, and a paraphrase would undo that.
			fmt.Fprintf(w, "\n  %s\n", q)
		}
		fmt.Fprintln(w, "\nNext: answer them in the web console, or paste a question into whatever wrote this app.")

	case d.Status == "blocked":
		fmt.Fprintln(w, "Blocked: Pando cannot run this app as it stands.")
		if d.Detection.Blocked != nil {
			fmt.Fprintf(w, "\n  %s\n", d.Detection.Blocked.Error())
		}
		fmt.Fprintln(w, "\nNext: fix what is described above, then re-run detection from the app's page in the web console.")

	case d.Status == "failed":
		fmt.Fprintln(w, "Failed: Pando could not work out how to run this app.")
		if d.Detection.Error != nil {
			fmt.Fprintf(w, "\n  %s\n", d.Detection.Error.Error())
		}
		fmt.Fprintln(w, "\nNext: re-run detection in the web console once the cause is fixed.")

	default:
		fmt.Fprintf(w, "Detection finished: %s.\n", d.Status)
	}
}

// detectionOutcome is the error, if any, a finished detection ends a waiting
// command with: exit 2 when a person has to answer questions, 1 when it
// failed or was blocked, and none when it is ready to accept.
func detectionOutcome(d detectionStatus) error {
	switch d.Status {
	case "failed", "blocked":
		return &ExitCode{Code: 1, Err: fmt.Errorf("detection %s", d.Status)}
	case "needs_answers", "unknown":
		if open := d.openQuestions(); len(open) > 0 {
			return &ExitCode{Code: 2, Err: fmt.Errorf("%d question(s) still to answer", len(open))}
		}
	}
	return nil
}

// waitAndReport waits for detection, reporting each stage on stderr, then
// prints the outcome on stdout and ends with its exit code.
func (c *Client) waitAndReport(cmd *cobra.Command, appID string, timeout time.Duration) error {
	d, err := c.awaitDetection(appID, timeout, func(stage string) {
		fmt.Fprintf(cmd.ErrOrStderr(), "%s...\n", stagePhrase(stage))
	})
	if err != nil {
		return err
	}
	printDetection(cmd.OutOrStdout(), appID, d)
	return detectionOutcome(d)
}

func appDetectionCmd(client func() (*Client, error)) *cobra.Command {
	var wait bool
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "detection <app>",
		Short: "Show how far detection has got, and what to do next",
		Long: "Shows where an app's detection stands: running and at which stage, or how it\n" +
			"finished — ready, waiting on answers, blocked or failed — and what to do next.\n" +
			"Questions are printed exactly as Pando wrote them, to paste into whatever wrote\n" +
			"the app.\n\n" +
			"With --wait, waits for detection to finish, printing each stage as it is\n" +
			"reached. It then exits 0 when the app is ready to accept, 2 when there are\n" +
			"questions to answer, and 1 when detection failed or was blocked.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if wait {
				return c.waitAndReport(cmd, args[0], timeout)
			}
			d, err := c.getDetection(args[0], 0)
			if err != nil {
				return err
			}
			printDetection(cmd.OutOrStdout(), args[0], d)
			return nil
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for detection to finish, and exit non-zero unless it is ready")
	cmd.Flags().DurationVar(&timeout, "timeout", detectionTimeout, "with --wait, how long to wait before giving up")
	return cmd
}
