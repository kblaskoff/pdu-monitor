package pdu

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Operation is what is done to the ports of a server (or to a single port).
type Operation struct {
	// Kind is "on", "off" or "restart".
	Kind string
	// Delay is how long the ports stay off during a restart.
	Delay time.Duration
}

// Progress is reported while the sequence runs.
type Progress struct {
	Step        string // switching-off, confirmed-off, waiting, switching-on, confirmed-on, rolled-back
	SecondsLeft int
}

// Controller switches and reads single ports (the application maps it to the drivers of the PDUs).
type Controller interface {
	Set(ctx context.Context, t Target, on bool) error
	State(ctx context.Context, t Target) (*bool, error)
}

// SequenceError says what went wrong and whether the ports that were switched off are on again.
type SequenceError struct {
	Phase    string // "off", "off-unconfirmed", "on", "on-unconfirmed"
	Detail   string
	Restored bool
}

func (e *SequenceError) Error() string {
	restored := ""
	if e.Restored {
		restored = " The ports that were already off were switched back on."
	}
	switch e.Phase {
	case "off":
		return "Could not switch off: " + e.Detail + "." + restored
	case "off-unconfirmed":
		return "Switch-off was not confirmed by the PDU: " + e.Detail + "." + restored
	case "on":
		return "Could not switch on: " + e.Detail
	}
	return "Switch-on was not confirmed by the PDU: " + e.Detail
}

// Sequencer switches the ports of one server: the same steps for one port or for the two PDUs of a dual-feed server.
// A restart is not the PDU's own "reboot" command (the two PDUs would not stay in step) but explicit OFF on all ports, confirmation, a pause,
// then ON on all ports and confirmation. If a switch-off fails, the ports already turned off are turned on again.
type Sequencer struct {
	Controller      Controller
	Label           func(Target) string
	Sleep           func(ctx context.Context, d time.Duration) error
	ConfirmTimeout  time.Duration
	ConfirmInterval time.Duration
}

func (s *Sequencer) label(t Target) string {
	if s.Label != nil {
		return s.Label(t)
	}
	return fmt.Sprintf("PDU %d outlet %d", t.PDUID, t.Outlet)
}

func (s *Sequencer) sleep(ctx context.Context, d time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, d)
	}
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run does the operation on the targets. progress may be nil.
func (s *Sequencer) Run(ctx context.Context, op Operation, targets []Target, progress func(Progress)) error {
	if len(targets) == 0 {
		return nil
	}
	report := func(p Progress) {
		if progress != nil {
			progress(p)
		}
	}
	switch op.Kind {
	case "off":
		report(Progress{Step: "switching-off"})
		return s.switchOff(ctx, targets, report)
	case "on":
		report(Progress{Step: "switching-on"})
		return s.switchOn(ctx, targets, report)
	case "restart":
		report(Progress{Step: "switching-off"})
		if err := s.switchOff(ctx, targets, report); err != nil {
			return err
		}
		for left := int((op.Delay + time.Second - 1) / time.Second); left > 0; left-- {
			report(Progress{Step: "waiting", SecondsLeft: left})
			if err := s.sleep(ctx, time.Second); err != nil {
				// asked to stop in the pause: the server must not be left without power
				s.restore(context.Background(), targets)
				return err
			}
		}
		report(Progress{Step: "switching-on"})
		return s.switchOn(ctx, targets, report)
	}
	return fmt.Errorf("unknown operation %q", op.Kind)
}

type outcome struct {
	target Target
	err    error
}

func (s *Sequencer) perform(ctx context.Context, targets []Target, on bool) []outcome {
	results := make([]outcome, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()
			results[i] = outcome{target: t, err: s.Controller.Set(ctx, t, on)}
		}(i, t)
	}
	wg.Wait()
	return results
}

func (s *Sequencer) describe(failed []outcome) string {
	parts := make([]string, len(failed))
	for i, f := range failed {
		parts[i] = fmt.Sprintf("%s: %v", s.label(f.target), f.err)
	}
	return strings.Join(parts, "; ")
}

func (s *Sequencer) switchOff(ctx context.Context, targets []Target, report func(Progress)) error {
	results := s.perform(ctx, targets, false)
	var failed []outcome
	var done []Target
	for _, r := range results {
		if r.err != nil {
			failed = append(failed, r)
		} else {
			done = append(done, r.target)
		}
	}
	if len(failed) > 0 {
		restored := s.restore(context.Background(), done)
		if restored {
			report(Progress{Step: "rolled-back"})
		}
		return &SequenceError{Phase: "off", Detail: s.describe(failed), Restored: restored}
	}
	if notOff := s.waitFor(ctx, targets, false); len(notOff) > 0 {
		restored := s.restore(context.Background(), targets)
		if restored {
			report(Progress{Step: "rolled-back"})
		}
		return &SequenceError{Phase: "off-unconfirmed", Detail: s.labels(notOff), Restored: restored}
	}
	report(Progress{Step: "confirmed-off"})
	return nil
}

func (s *Sequencer) switchOn(ctx context.Context, targets []Target, report func(Progress)) error {
	var failed []outcome
	for _, r := range s.perform(ctx, targets, true) {
		if r.err != nil {
			failed = append(failed, r)
		}
	}
	if len(failed) > 0 {
		return &SequenceError{Phase: "on", Detail: s.describe(failed)}
	}
	if notOn := s.waitFor(ctx, targets, true); len(notOn) > 0 {
		return &SequenceError{Phase: "on-unconfirmed", Detail: s.labels(notOn)}
	}
	report(Progress{Step: "confirmed-on"})
	return nil
}

func (s *Sequencer) labels(ts []Target) string {
	parts := make([]string, len(ts))
	for i, t := range ts {
		parts[i] = s.label(t)
	}
	return strings.Join(parts, ", ")
}

// waitFor returns the targets that did not reach the wanted state in time. A PDU that cannot tell its state is not waited for.
func (s *Sequencer) waitFor(ctx context.Context, targets []Target, on bool) []Target {
	timeout, interval := s.ConfirmTimeout, s.ConfirmInterval
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if interval <= 0 {
		interval = time.Second
	}
	pending := targets
	var waited time.Duration
	for {
		var still []Target
		for _, t := range pending {
			if state, err := s.Controller.State(ctx, t); err == nil && state != nil && *state != on {
				still = append(still, t)
			}
		}
		pending = still
		if len(pending) == 0 || waited >= timeout {
			return pending
		}
		if err := s.sleep(ctx, interval); err != nil {
			return pending
		}
		waited += interval
	}
}

// restore switches back on what was switched off: true when it was tried for at least one port and none failed.
func (s *Sequencer) restore(ctx context.Context, targets []Target) bool {
	if len(targets) == 0 {
		return false
	}
	for _, r := range s.perform(ctx, targets, true) {
		if r.err != nil {
			return false
		}
	}
	return true
}
