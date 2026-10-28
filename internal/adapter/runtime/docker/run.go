package docker

import (
	"context"
	"io"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// runExitPoll is how long Run waits between asking whether a command whose
// output has ended has also exited. The daemon closes the stream a moment
// before it records the exit code.
const runExitPoll = 50 * time.Millisecond

// runExitWait bounds that wait.
const runExitWait = 10 * time.Second

// Run runs a command to completion in a running workload, with no terminal
// (R-423).
//
// Not Exec: Exec is a person's terminal, and without a TTY Docker sends stdout
// and stderr down one connection in frames. Read as one stream, a database
// dump would carry the dump tool's warnings in the middle of it. Here they are
// separated, stdout is the caller's and stderr is kept only to explain a
// failure — the same split the volume helper makes.
func (a *Adapter) Run(ctx context.Context, ref api.WorkloadRef, req api.RunRequest) error {
	c, err := a.findContainer(ctx, ref.BundleID, ref.Workload)
	if err != nil {
		return err
	}
	if c == nil || c.State != "running" {
		return errs.Newf(errs.NotFound, "There is nothing running called %q.", ref.Workload)
	}

	stdin := req.Stdin != nil
	created, err := a.cli.ExecCreate(ctx, c.ID, client.ExecCreateOptions{
		Cmd:          req.Command,
		AttachStdin:  stdin,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Pando could not run a command in "+ref.Workload+".", err)
	}
	attached, err := a.cli.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Pando could not run a command in "+ref.Workload+".", err)
	}
	defer attached.Close()

	stdout := req.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := &tailBuffer{max: helperStderrMax}

	if stdin {
		go func() {
			_, _ = io.Copy(attached.Conn, req.Stdin)
			// End of input is what tells the command its input is over.
			_ = attached.CloseWrite()
		}()
	}
	if _, err := stdcopy.StdCopy(stdout, stderr, attached.Reader); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Pando could not read what "+ref.Workload+" wrote.", err)
	}

	code, err := a.execExit(ctx, created.ID)
	if err != nil {
		return err
	}
	if code != 0 {
		return commandFailed(ref.Workload, int64(code), stderr.lastLine())
	}
	return nil
}

// execExit waits for an exec whose output has ended to report its exit code.
func (a *Adapter) execExit(ctx context.Context, execID string) (int, error) {
	deadline := time.Now().Add(runExitWait)
	for {
		inspect, err := a.cli.ExecInspect(ctx, execID, client.ExecInspectOptions{})
		if err != nil {
			return 0, errs.Wrap(errs.AdapterFailed, "Pando could not read how a command it ran ended.", err)
		}
		if !inspect.Running {
			return inspect.ExitCode, nil
		}
		if time.Now().After(deadline) {
			return 0, errs.New(errs.AdapterFailed, "A command Pando ran stopped writing but did not exit.")
		}
		select {
		case <-ctx.Done():
			return 0, errs.Wrap(errs.AdapterFailed, "Pando stopped waiting for a command it ran.", ctx.Err())
		case <-time.After(runExitPoll):
		}
	}
}

// commandFailed reports a non-zero exit from Run, with the command's last line
// of stderr when it printed one.
func commandFailed(workload string, code int64, said string) error {
	if said == "" {
		return errs.Newf(errs.AdapterFailed,
			"A command Pando ran in %s exited with status %d.", workload, code)
	}
	return errs.Newf(errs.AdapterFailed,
		"A command Pando ran in %s exited with status %d and said %q.", workload, code, said)
}
