package trigger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/zakeripouya/EngramContextProtocol/internal/store"
)

// tail keeps the last n bytes written to it.
type tail struct {
	buf bytes.Buffer
	n   int
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if extra := t.buf.Len() - t.n; extra > 0 {
		t.buf.Next(extra)
	}
	return len(p), nil
}

// JobResult is what RunJob observed.
type JobResult struct {
	Result
	ExitCode int
	Duration time.Duration
	Withheld bool // the output looked like it contained a secret and was not stored
}

// RunJob runs an existing automation unchanged, passing its output through, and
// records the outcome as an episode on the job's trigger (created on first use).
// A failure is dispatched as urgent, so subscribed agents see the related tickets
// and decisions.
func RunJob(ctx context.Context, st *store.Store, name string, argv []string, stdout, stderr io.Writer) (JobResult, error) {
	if len(argv) == 0 {
		return JobResult{}, fmt.Errorf("no command to run")
	}
	if _, err := st.GetTrigger(name); err != nil {
		if _, err := st.AddTrigger(name, store.TriggerJob, ""); err != nil {
			return JobResult{}, err
		}
	}
	out := &tail{n: 4000}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = io.MultiWriter(stdout, out)
	cmd.Stderr = io.MultiWriter(stderr, out)
	start := time.Now()
	runErr := cmd.Run()
	jr := JobResult{Duration: time.Since(start)}
	status := "ok"
	summary := ""
	if runErr != nil {
		status = "failed"
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			jr.ExitCode = ee.ExitCode()
			summary = fmt.Sprintf("exit %d", jr.ExitCode)
		} else {
			jr.ExitCode = 127
			summary = runErr.Error()
		}
	}
	body := strings.TrimSpace(out.buf.String())
	if hit := store.FindSensitive(body); hit != "" {
		body, jr.Withheld = "(output withheld: it looked like it contained a "+hit+")", true
	}
	title := JobTitle(name, status, summary, jr.Duration.Milliseconds())
	if cmdline := strings.Join(argv, " "); len(cmdline) <= 200 && store.FindSensitive(cmdline) == "" {
		body = "Command: " + cmdline + "\n" + body
	}
	res, err := Record(st, Event{Trigger: name, Title: title, Body: body, Status: status, Tags: []string{"job"}})
	jr.Result = res
	return jr, err
}
