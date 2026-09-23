package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type Observation struct {
	ID               string            `json:"id"`
	Role             string            `json:"role"`
	Detail           ObservationDetail `json:"detail"`
	Resolution       string            `json:"resolution,omitempty"`
	ApprovedContract string            `json:"approved_contract,omitempty"`
}

func (s *Store) Observations(cycle int) ([]Observation, error) {
	rows, err := s.db.Query(`SELECT id,role,detail,resolution,approved_contract FROM workflow_observations WHERE project=? AND cycle=? ORDER BY id`, s.project, cycle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		var o Observation
		var raw string
		if err = rows.Scan(&o.ID, &o.Role, &raw, &o.Resolution, &o.ApprovedContract); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &o.Detail); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// RecordObservations is idempotent across response replay. Events and gates commit together.
func (s *Store) RecordObservations(cycle int, role, source string, details []ObservationDetail) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	changed := false
	for i, d := range details {
		if err = d.Validate(); err != nil {
			return err
		}
		raw, _ := json.Marshal(d)
		id := "O-" + Digest(fmt.Sprintf("%s|%s|%d", role, source, i))[:16]
		result, err := tx.Exec(`INSERT INTO workflow_observations(project,cycle,id,role,detail) VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`, s.project, cycle, id, role, string(raw))
		if err != nil {
			return err
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			continue
		}
		changed = true
		if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorEngine, Type: "observation.raised", Payload: id + " " + string(raw)}); err != nil {
			return err
		}
	}
	if changed {
		if _, err = tx.Exec(`UPDATE workflow_cycles SET state_version=state_version+1 WHERE project=? AND cycle=?`, s.project, cycle); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ResolveObservation(cycle int, id, note string) error {
	if strings.TrimSpace(note) == "" {
		return errors.New("resolution requires a human --note; revise the ticket if requirements changed")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE workflow_observations SET resolution=?,approved_contract='' WHERE project=? AND cycle=? AND id=? AND resolution=''`, note, s.project, cycle, id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return errors.New("observation not found or already resolved")
	}
	if _, err = tx.Exec(`UPDATE workflow_cycles SET state_version=state_version+1 WHERE project=? AND cycle=?`, s.project, cycle); err != nil {
		return err
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorHuman, Type: "observation.resolved", Payload: id + " " + note}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RequireResolved(cycle int) error {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM workflow_observations WHERE project=? AND cycle=? AND resolution=''`, s.project, cycle).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%d observation(s) await a human response; inspect status and use resolve --observation ID --note ...", n)
	}
	return nil
}

// ObservationReviewHash binds the review token to exact human resolutions while
// leaving the original patch contract intact for execution-time pauses.
func (s *Store) ObservationReviewHash(cycle int, contract string) (string, error) {
	obs, err := s.Observations(cycle)
	if err != nil {
		return "", err
	}
	if len(obs) == 0 {
		return contract, nil
	}
	for i := range obs {
		obs[i].ApprovedContract = ""
	}
	return Hash(struct {
		Contract     string
		Observations []Observation
	}{contract, obs})
}

// ApproveObservations is a supplemental human approval of the same executable
// contract. It never resets patch state or grants approval to a different plan.
func (s *Store) ApproveObservations(cycle int, version int64, contract, reviewed string) error {
	if err := s.RequireResolved(cycle); err != nil {
		return err
	}
	hash, err := s.ObservationReviewHash(cycle, contract)
	if err != nil {
		return err
	}
	if hash != reviewed {
		return errors.New("observations changed since review")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE workflow_cycles SET state_version=state_version+1 WHERE project=? AND cycle=? AND state_version=? AND active_contract=? AND phase IN ('approved','awaiting_execution','awaiting_review') AND legacy=0`, s.project, cycle, version, contract)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return &ErrStaleVersion{Kind: "cycle", Expected: version}
	}
	if _, err = tx.Exec(`UPDATE workflow_observations SET approved_contract=? WHERE project=? AND cycle=? AND resolution<>''`, contract, s.project, cycle); err != nil {
		return err
	}
	if err = s.appendEventTx(tx, Event{Cycle: cycle, Actor: ActorHuman, Type: "observations.approved", Payload: reviewed}); err != nil {
		return err
	}
	return tx.Commit()
}
