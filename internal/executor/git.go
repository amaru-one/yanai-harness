package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yanai/yanai-harness/internal/workflow"
)

// The controlled Git service. It works only in the approved checkout, only
// on the one ticket branch a level 0 contract names, and only through
// journaled operations: every ref or HEAD move is recorded as intent before
// it happens and confirmed against the observed repository afterwards, so
// an interruption at any point is explained by the journal rather than
// guessed at. Nothing here resets, deletes, force-moves, merges or pushes.

func (n *Native) level0() (*workflow.LevelZeroTerms, error) {
	if n.contract.Level0 == nil {
		return nil, errors.New("this contract does not authorize Git operations")
	}
	if !n.contract.Policy.Commit {
		return nil, errors.New("this contract does not permit commits")
	}
	return n.contract.Level0, nil
}

// writableBranch refuses file writes unless a level 0 checkout is on its
// ticket branch. Contracts without Git terms keep their original behavior.
func (n *Native) writableBranch() error {
	if n.contract.Level0 == nil {
		return nil
	}
	session, _, found, err := n.o.Store.GitSession(n.o.Cycle, n.o.Contract)
	if err != nil {
		return err
	}
	if !found || session.State != workflow.GitSessionOnBranch {
		return errors.New("files may change only while the checkout is on the approved ticket branch")
	}
	return nil
}

// GitSession returns the recorded Git session, if any.
func (n *Native) GitSession() (workflow.GitSession, bool, error) {
	g, _, found, err := n.o.Store.GitSession(n.o.Cycle, n.o.Contract)
	return g, found, err
}

// StartTicketBranch creates the ticket branch at the approved base and
// switches the checkout to it. It is idempotent: a resumed run finds the
// session already on the branch and does nothing.
func (n *Native) StartTicketBranch() (workflow.GitSession, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	terms, err := n.level0()
	if err != nil {
		return workflow.GitSession{}, err
	}
	current, err := n.guard()
	if err != nil {
		return workflow.GitSession{}, err
	}
	session, revision, found, err := n.o.Store.GitSession(n.o.Cycle, n.o.Contract)
	if err != nil {
		return session, err
	}
	if found && session.State != workflow.GitSessionPending {
		return session, nil
	}
	if !found {
		if err = n.target.ValidBranchName(terms.TicketBranch); err != nil {
			return session, err
		}
		name, email, err := n.target.Identity()
		if err != nil {
			return session, err
		}
		if current.Dirty || current.Head != terms.BaseSHA {
			return session, errors.New("first execution requires the clean approved base commit")
		}
		tip, err := n.target.BranchTip(terms.OriginalBranch)
		if err != nil {
			return session, err
		}
		if tip != terms.BaseSHA {
			return session, fmt.Errorf("original branch %s moved from the approved base %s to %s; review and approve again", terms.OriginalBranch, short(terms.BaseSHA), short(tip))
		}
		if existing, err := n.target.BranchTip(terms.TicketBranch); err != nil {
			return session, err
		} else if existing != "" {
			return session, fmt.Errorf("branch %s already exists and is not recorded as this cycle's; use a distinct ticket title (the existing branch was left untouched)", terms.TicketBranch)
		}
		session = workflow.GitSession{
			Contract: n.o.Contract, Root: n.target.Root, CommonDir: n.target.CommonDir,
			OriginalBranch: terms.OriginalBranch, OriginalSHA: terms.BaseSHA, TicketBranch: terms.TicketBranch,
			BaseSHA: terms.BaseSHA, ExpectedBranch: terms.OriginalBranch, ExpectedSHA: terms.BaseSHA,
			State: workflow.GitSessionPending, AuthorName: name, AuthorEmail: email, HeadState: n.base,
		}
		if revision, err = n.o.Store.SaveGitSession(n.o.Cycle, session, 0); err != nil {
			return session, err
		}
	}
	// Create the branch at the approved base, failing if anything else made it.
	create := workflow.GitOperation{ID: "create-branch", Kind: workflow.GitOpCreateBranch, Branch: terms.TicketBranch, FromBranch: terms.OriginalBranch, FromSHA: terms.BaseSHA, Pre: current}
	if done, found, err := n.o.Store.GitOperationByID(n.o.Cycle, n.o.Contract, create.ID); err != nil {
		return session, err
	} else if !found || done.State != workflow.GitOpDone {
		if _, err = n.o.Store.BeginGitOperation(n.o.Cycle, n.o.Contract, create); err != nil {
			return session, err
		}
		if err = n.hit("git:create:intent"); err != nil {
			return session, err
		}
		if session, revision, err = n.finishCreate(create, session, revision); err != nil {
			return session, err
		}
	}
	current, err = n.guard()
	if err != nil {
		return session, err
	}
	sw := workflow.GitOperation{ID: "switch-to-ticket", Kind: workflow.GitOpSwitch, Branch: terms.TicketBranch, FromBranch: terms.OriginalBranch, FromSHA: terms.BaseSHA, Pre: current}
	if _, err = n.o.Store.BeginGitOperation(n.o.Cycle, n.o.Contract, sw); err != nil {
		return session, err
	}
	if err = n.hit("git:switch:intent"); err != nil {
		return session, err
	}
	session, _, err = n.finishSwitch(sw, session, revision)
	return session, err
}

func (n *Native) finishCreate(op workflow.GitOperation, session workflow.GitSession, revision int64) (workflow.GitSession, int64, error) {
	tip, err := n.target.BranchTip(op.Branch)
	if err != nil {
		return session, revision, err
	}
	switch tip {
	case "":
		// update-ref with an empty old value creates the ref only if absent.
		if _, err = n.target.GitCommand(nil, "", "update-ref", "refs/heads/"+op.Branch, op.FromSHA, ""); err != nil {
			return session, revision, err
		}
	case op.FromSHA:
		// Created before an interruption; the journal names it as ours.
	default:
		return session, revision, fmt.Errorf("branch %s points to %s, not the recorded base; reconcile manually", op.Branch, short(tip))
	}
	if err = n.hit("git:create:moved"); err != nil {
		return session, revision, err
	}
	post, err := n.target.Snapshot()
	if err != nil {
		return session, revision, err
	}
	if post.Baseline() != op.Pre.Baseline() {
		return session, revision, errors.New("the checkout changed while creating the ticket branch")
	}
	op.ToSHA, op.Post = op.FromSHA, post
	revision, err = n.o.Store.CompleteGitOperation(n.o.Cycle, n.o.Contract, op, session, revision)
	return session, revision, err
}

func (n *Native) finishSwitch(op workflow.GitOperation, session workflow.GitSession, revision int64) (workflow.GitSession, int64, error) {
	branch, err := n.target.Branch()
	if err != nil {
		return session, revision, err
	}
	switch branch {
	case op.FromBranch:
		if _, err = n.target.GitCommand(nil, "", "switch", "--no-guess", op.Branch); err != nil {
			return session, revision, err
		}
	case op.Branch:
	default:
		return session, revision, fmt.Errorf("the checkout is on %q, neither %q nor %q; reconcile manually", branch, op.FromBranch, op.Branch)
	}
	if err = n.hit("git:switch:moved"); err != nil {
		return session, revision, err
	}
	post, err := n.target.Snapshot()
	if err != nil {
		return session, revision, err
	}
	if post.Baseline() != op.Pre.Baseline() {
		return session, revision, errors.New("switching to the ticket branch changed the working tree; reconcile manually")
	}
	op.ToSHA, op.Post = op.FromSHA, post
	session.State, session.ExpectedBranch, session.ExpectedSHA = workflow.GitSessionOnBranch, op.Branch, op.FromSHA
	if revision, err = n.o.Store.CompleteGitOperation(n.o.Cycle, n.o.Contract, op, session, revision); err != nil {
		return session, revision, err
	}
	n.branch = op.Branch
	return session, revision, nil
}

// ChangedPaths lists the paths whose content differs from the last commit.
func (n *Native) ChangedPaths() ([]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	current, err := n.guard()
	if err != nil {
		return nil, err
	}
	return changedSince(n.head, current), nil
}

func changedSince(head, current workflow.RepositoryState) []string {
	seen := map[string]bool{}
	for p, h := range current.Content {
		if head.Content[p] != h {
			seen[p] = true
		}
	}
	for p := range head.Content {
		if _, ok := current.Content[p]; !ok {
			seen[p] = true
		}
	}
	var out []string
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Commit records every uncommitted change the ticket allows as
// one commit on the ticket branch. message is already validated by the
// caller; the harness appends its own operation trailer. The commit object
// is built from a temporary index, never the checkout's own index, and is
// fully determined by the recorded parent, tree, message, identity and
// timestamp, so recovery recreates exactly the same SHA.
func (n *Native) Commit(ticket, message string) (workflow.GitCommitRecord, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.level0(); err != nil {
		return workflow.GitCommitRecord{}, err
	}
	if err := n.unfinishedChecks(); err != nil {
		return workflow.GitCommitRecord{}, err
	}
	current, err := n.guard()
	if err != nil {
		return workflow.GitCommitRecord{}, err
	}
	session, revision, found, err := n.o.Store.GitSession(n.o.Cycle, n.o.Contract)
	if err != nil {
		return workflow.GitCommitRecord{}, err
	}
	if !found || session.State != workflow.GitSessionOnBranch || current.Head != session.ExpectedSHA {
		return workflow.GitCommitRecord{}, errors.New("commits are only made on the approved ticket branch at its recorded commit")
	}
	paths := changedSince(n.head, current)
	if len(paths) == 0 {
		return workflow.GitCommitRecord{}, errors.New("there are no uncommitted changes; empty commits are not allowed")
	}
	var approved *workflow.Ticket
	for i := range n.contract.Plan.Tickets {
		if n.contract.Plan.Tickets[i].ID == ticket {
			approved = &n.contract.Plan.Tickets[i]
		}
	}
	if approved == nil {
		return workflow.GitCommitRecord{}, errors.New("unknown approved ticket")
	}
	for _, p := range paths {
		if !approved.Allows(p) {
			return workflow.GitCommitRecord{}, fmt.Errorf("uncommitted change outside the approved ticket: %s", p)
		}
	}
	tree, err := n.buildTree(session.ExpectedSHA, paths, current)
	if err != nil {
		return workflow.GitCommitRecord{}, err
	}
	id := "commit-" + short(workflow.Digest(session.ExpectedSHA+"\x00"+tree+"\x00"+message))
	full := strings.TrimRight(message, "\n") + "\nYanai-Operation: " + id + "\n"
	op := workflow.GitOperation{
		ID: id, Kind: workflow.GitOpCommit, Branch: session.TicketBranch, FromBranch: session.TicketBranch,
		FromSHA: session.ExpectedSHA, Parent: session.ExpectedSHA, Tree: tree, Message: full, Paths: paths,
		AuthorName: session.AuthorName, AuthorEmail: session.AuthorEmail,
		Timestamp: fmt.Sprintf("%d +0000", time.Now().UTC().Unix()), Pre: current,
	}
	if op, err = n.o.Store.BeginGitOperation(n.o.Cycle, n.o.Contract, op); err != nil {
		return workflow.GitCommitRecord{}, err
	}
	if err = n.hit("git:commit:intent"); err != nil {
		return workflow.GitCommitRecord{}, err
	}
	record, _, err := n.finishCommit(op, session, revision)
	return record, err
}

// buildTree writes the tree for HEAD plus the given working-tree paths using
// a private index outside the repository.
func (n *Native) buildTree(parent string, paths []string, current workflow.RepositoryState) (string, error) {
	index := filepath.Join(n.o.Artifacts.Root, ".yanai-index-"+nonce())
	defer os.Remove(index)
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := n.target.GitCommand(env, "", "read-tree", parent); err != nil {
		return "", err
	}
	var add, remove []string
	for _, p := range paths {
		if current.Content[p] == "" || current.Content[p] == "deleted" {
			remove = append(remove, p)
		} else {
			add = append(add, p)
		}
	}
	if len(add) > 0 {
		if _, err := n.target.GitCommand(env, "", append([]string{"update-index", "--add", "--"}, add...)...); err != nil {
			return "", err
		}
	}
	if len(remove) > 0 {
		if _, err := n.target.GitCommand(env, "", append([]string{"update-index", "--force-remove", "--"}, remove...)...); err != nil {
			return "", err
		}
	}
	tree, err := n.target.GitCommand(env, "", "write-tree")
	if err != nil {
		return "", err
	}
	// The files must not have changed while they were hashed into the tree.
	after, err := n.target.Snapshot()
	if err != nil {
		return "", err
	}
	if after.Baseline() != current.Baseline() {
		return "", errors.New("files changed while the commit tree was built")
	}
	return strings.TrimSpace(tree), nil
}

func (n *Native) commitObject(op workflow.GitOperation) (string, error) {
	env := []string{
		"GIT_AUTHOR_NAME=" + op.AuthorName, "GIT_AUTHOR_EMAIL=" + op.AuthorEmail, "GIT_AUTHOR_DATE=" + op.Timestamp,
		"GIT_COMMITTER_NAME=" + op.AuthorName, "GIT_COMMITTER_EMAIL=" + op.AuthorEmail, "GIT_COMMITTER_DATE=" + op.Timestamp,
	}
	out, err := n.target.GitCommand(env, op.Message, "commit-tree", op.Tree, "-p", op.Parent, "-F", "-")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (n *Native) finishCommit(op workflow.GitOperation, session workflow.GitSession, revision int64) (workflow.GitCommitRecord, int64, error) {
	sha, err := n.commitObject(op)
	if err != nil {
		return workflow.GitCommitRecord{}, revision, err
	}
	if op.ToSHA != "" && op.ToSHA != sha {
		return workflow.GitCommitRecord{}, revision, errors.New("recreated commit differs from the recorded one")
	}
	if err = n.hit("git:commit:object"); err != nil {
		return workflow.GitCommitRecord{}, revision, err
	}
	tip, err := n.target.BranchTip(op.Branch)
	if err != nil {
		return workflow.GitCommitRecord{}, revision, err
	}
	switch tip {
	case op.Parent:
		// Move only the ticket branch, and only from the recorded parent.
		if _, err = n.target.GitCommand(nil, "", "update-ref", "refs/heads/"+op.Branch, sha, op.Parent); err != nil {
			return workflow.GitCommitRecord{}, revision, err
		}
	case sha:
	default:
		return workflow.GitCommitRecord{}, revision, fmt.Errorf("branch %s moved to %s outside the harness; nothing was changed", op.Branch, short(tip))
	}
	if err = n.hit("git:commit:ref"); err != nil {
		return workflow.GitCommitRecord{}, revision, err
	}
	// The checkout's index still describes the parent; align it with the new
	// commit without touching the working tree.
	if _, err = n.target.GitCommand(nil, "", "read-tree", sha); err != nil {
		return workflow.GitCommitRecord{}, revision, err
	}
	post, err := n.target.Snapshot()
	if err != nil {
		return workflow.GitCommitRecord{}, revision, err
	}
	if post.Head != sha {
		return workflow.GitCommitRecord{}, revision, errors.New("HEAD does not point to the new commit")
	}
	for p, h := range op.Pre.Content {
		if post.Content[p] != h {
			return workflow.GitCommitRecord{}, revision, fmt.Errorf("working tree changed during the commit: %s", p)
		}
	}
	if post.Dirty {
		return workflow.GitCommitRecord{}, revision, errors.New("the checkout is not clean after committing every owned change")
	}
	if err = n.hit("git:commit:observed"); err != nil {
		return workflow.GitCommitRecord{}, revision, err
	}
	subject, _, _ := strings.Cut(op.Message, "\n")
	record := workflow.GitCommitRecord{SHA: sha, Parent: op.Parent, Operation: op.ID, Subject: subject, Paths: op.Paths}
	op.ToSHA, op.Post = sha, post
	session.ExpectedSHA, session.HeadState = sha, post
	session.Commits = append(session.Commits, record)
	revision, err = n.o.Store.CompleteGitOperation(n.o.Cycle, n.o.Contract, op, session, revision)
	if err != nil {
		return record, revision, err
	}
	n.head = post
	return record, revision, nil
}

// ReturnToOriginal switches the clean checkout back to the original branch,
// whose SHA must be unchanged. The ticket branch and its commits stay.
func (n *Native) ReturnToOriginal() (workflow.GitSession, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, err := n.level0(); err != nil {
		return workflow.GitSession{}, err
	}
	current, err := n.guard()
	if err != nil {
		return workflow.GitSession{}, err
	}
	session, revision, found, err := n.o.Store.GitSession(n.o.Cycle, n.o.Contract)
	if err != nil || !found {
		return session, errors.Join(err, errors.New("no Git session recorded"))
	}
	if session.State == workflow.GitSessionReturned {
		return session, nil
	}
	if session.State != workflow.GitSessionOnBranch {
		return session, errors.New("the checkout never moved to the ticket branch")
	}
	if current.Dirty {
		return session, errors.New("uncommitted changes remain on the ticket branch; nothing was switched")
	}
	op := workflow.GitOperation{ID: "return-to-original", Kind: workflow.GitOpReturn, Branch: session.OriginalBranch, FromBranch: session.TicketBranch, FromSHA: session.ExpectedSHA, ToSHA: session.OriginalSHA, Pre: current}
	if _, err = n.o.Store.BeginGitOperation(n.o.Cycle, n.o.Contract, op); err != nil {
		return session, err
	}
	if err = n.hit("git:return:intent"); err != nil {
		return session, err
	}
	session, _, err = n.finishReturn(op, session, revision)
	return session, err
}

func (n *Native) finishReturn(op workflow.GitOperation, session workflow.GitSession, revision int64) (workflow.GitSession, int64, error) {
	tip, err := n.target.BranchTip(op.Branch)
	if err != nil {
		return session, revision, err
	}
	if tip != session.OriginalSHA {
		return session, revision, fmt.Errorf("original branch %s moved to %s; it was not switched back and nothing was changed", op.Branch, short(tip))
	}
	branch, err := n.target.Branch()
	if err != nil {
		return session, revision, err
	}
	switch branch {
	case op.FromBranch:
		if _, err = n.target.GitCommand(nil, "", "switch", "--no-guess", op.Branch); err != nil {
			return session, revision, err
		}
	case op.Branch:
	default:
		return session, revision, fmt.Errorf("the checkout is on %q, neither the ticket branch nor the original; reconcile manually", branch)
	}
	if err = n.hit("git:return:moved"); err != nil {
		return session, revision, err
	}
	post, err := n.target.Snapshot()
	if err != nil {
		return session, revision, err
	}
	if post.Head != session.OriginalSHA || post.Dirty {
		return session, revision, errors.New("the original branch is not clean at its original commit after switching back")
	}
	op.Post = post
	session.State, session.ExpectedBranch, session.ExpectedSHA, session.HeadState = workflow.GitSessionReturned, op.Branch, session.OriginalSHA, post
	if revision, err = n.o.Store.CompleteGitOperation(n.o.Cycle, n.o.Contract, op, session, revision); err != nil {
		return session, revision, err
	}
	n.branch, n.head = op.Branch, post
	return session, revision, nil
}

// recoverGit finishes or explains an operation a dead process left as
// intent. It never resets or deletes anything: every branch requires the
// repository to be in one of the two states the journal allows.
func (n *Native) recoverGit() error {
	if n.contract.Level0 == nil {
		return nil
	}
	pending, err := n.o.Store.PendingGitOperations(n.o.Cycle, n.o.Contract)
	if err != nil || len(pending) == 0 {
		return err
	}
	op := pending[0]
	session, revision, found, err := n.o.Store.GitSession(n.o.Cycle, n.o.Contract)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("git operation recorded without a session")
	}
	// The recorded patch state still describes the pre-state; the guard is
	// deliberately not consulted until the operation is resolved.
	switch op.Kind {
	case workflow.GitOpCreateBranch:
		_, _, err = n.finishCreate(op, session, revision)
	case workflow.GitOpSwitch:
		_, _, err = n.finishSwitch(op, session, revision)
	case workflow.GitOpCommit:
		_, _, err = n.finishCommit(op, session, revision)
	case workflow.GitOpReturn:
		_, _, err = n.finishReturn(op, session, revision)
	default:
		err = fmt.Errorf("unknown git operation %q", op.Kind)
	}
	if err != nil {
		return fmt.Errorf("recover git operation %s: %w", op.ID, err)
	}
	return nil
}

// CumulativeDiff returns Git's diff from the approved base to the ticket
// branch tip: the review diff, which stays available after the checkout
// returns to the original branch.
func (n *Native) CumulativeDiff(maxBytes int) (stat, patch string, err error) {
	session, _, found, err := n.o.Store.GitSession(n.o.Cycle, n.o.Contract)
	if err != nil || !found {
		return "", "", errors.Join(err, errors.New("no Git session recorded"))
	}
	tip, err := n.target.BranchTip(session.TicketBranch)
	if err != nil {
		return "", "", err
	}
	stat, err = n.target.GitCommand(nil, "", "diff", "--no-ext-diff", "--no-textconv", "--stat", session.BaseSHA, tip)
	if err != nil {
		return "", "", err
	}
	patch, err = n.target.GitCommand(nil, "", "diff", "--no-ext-diff", "--no-textconv", session.BaseSHA, tip)
	if err != nil {
		return "", "", err
	}
	if len(patch) > maxBytes {
		patch = patch[:maxBytes] + fmt.Sprintf("\n[diff truncated at %d bytes; inspect with: git diff %s %s]\n", maxBytes, short(session.BaseSHA), session.TicketBranch)
	}
	return stat, patch, nil
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
