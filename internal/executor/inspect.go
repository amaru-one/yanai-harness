package executor

import (
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Inspect returns complete pre/post source for the current diff against the
// approved baseline, including untracked outputs and deletions. It does not
// execute Git diff drivers or textconv. Filtered baseline content fails closed.
func (n *Native) Inspect() (Diff, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	current, err := n.guard()
	if err != nil {
		return Diff{}, err
	}
	d := Diff{Before: n.base.Baseline(), After: current.Baseline()}
	paths := map[string]bool{}
	for p := range n.base.Content {
		paths[p] = true
	}
	for p := range current.Content {
		paths[p] = true
	}
	var ordered []string
	for p := range paths {
		if n.base.Content[p] != current.Content[p] {
			ordered = append(ordered, p)
		}
	}
	sort.Strings(ordered)
	for _, p := range ordered {
		if err = n.target.CheckPath(p); err != nil {
			return Diff{}, err
		}
		allowed := false
		for _, ticket := range n.contract.Plan.Tickets {
			for _, out := range ticket.Outputs {
				if p == out {
					allowed = true
				}
			}
		}
		if !allowed {
			return Diff{}, fmt.Errorf("diff outside approved outputs: %s", p)
		}
		e := Edit{Path: p}
		if hash, exists := n.base.Content[p]; exists {
			cmd := exec.Command("git", "-C", n.target.Root, "cat-file", "blob", n.base.Head+":"+p)
			cmd.Env = []string{"PATH=/usr/bin:/bin", "GIT_OPTIONAL_LOCKS=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
			data, err := cmd.Output()
			if err != nil {
				return Diff{}, err
			}
			mode, err := strconv.ParseUint(strings.SplitN(hash, ":", 2)[0], 8, 32)
			if err != nil {
				return Diff{}, err
			}
			e.Before = &File{Data: data, Mode: uint32(mode)}
			if len(data) > MaxSourceBytes || fingerprint(e.Before) != hash {
				return Diff{}, errors.New("baseline blob differs from approved full source")
			}
		}
		e.After, err = n.read(p)
		if err != nil {
			return Diff{}, err
		}
		d.Edits = append(d.Edits, e)
	}
	return d, nil
}
