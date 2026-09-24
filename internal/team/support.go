package team

import (
	"errors"
	"fmt"
	"strings"

	"github.com/yanai/yanai-harness/internal/workflow"
	"github.com/yanai/yanai-harness/internal/ws"
)

func (r *Runner) checkUnresolvedAttempts(retry bool) error {
	if r.Workspace.Store == nil {
		return nil
	}
	attempts, err := r.Workspace.Store.UnresolvedAttempts()
	if err != nil {
		return err
	}
	if len(attempts) == 0 {
		return nil
	}
	if !retry {
		var b strings.Builder
		fmt.Fprintf(&b, "%d unresolved attempt(s) from an interrupted process; the model call may already have been billed:\n", len(attempts))
		for _, attempt := range attempts {
			fmt.Fprintf(&b, "  %s  cycle %03d  ticket %s  role %s  started %s\n", attempt.ID, attempt.Cycle, attempt.TicketID, attempt.Role, attempt.StartedAt.Format("2006-01-02 15:04"))
		}
		b.WriteString("Inspect: yanai status --attempts\nThen: reconcile billing with reconcile-attempt, then rerun with --retry-unresolved.")
		return errors.New(b.String())
	}
	for _, attempt := range attempts {
		if err := r.Workspace.Store.ResolveAttempt(attempt.ID); err != nil {
			return err
		}
	}
	return nil
}

func tasksForProposal(p workflow.Proposal) []ws.Task {
	var result []ws.Task
	done := map[string]bool{}
	for len(result) < len(p.Tickets) {
		progress := false
		for _, ticket := range p.Tickets {
			if done[ticket.ID] {
				continue
			}
			ready := true
			for _, dependency := range ticket.DependsOn {
				if !done[dependency] {
					ready = false
				}
			}
			if !ready {
				continue
			}
			result = append(result, ws.Task{
				ID:          ticket.ID,
				Owner:       ticket.Owner,
				Title:       ticket.Title,
				Description: ticket.Description,
				Criteria:    ticket.Criteria,
				DependsOn:   ticket.DependsOn,
				Status:      "pending",
			})
			done[ticket.ID] = true
			progress = true
		}
		if !progress {
			break
		}
	}
	return result
}
