package journal

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// RunScope is what governed one run: the tenant's product version and allowlisted
// endpoint ID, and the resolved settings record (digest plus applied values).
type RunScope struct {
	ProductVersion string
	EndpointID     string
	Settings       json.RawMessage // a JSON object; never credentials or URLs
}

// isJSONObject reports whether raw is a valid JSON object (not null/array/scalar).
func isJSONObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && m != nil
}

// RecordRunScope stores the run's scope under the ownership fence. It is
// write-once-or-equal: re-recording an identical scope is a no-op, a different
// one is ErrState.
func (s *Store) RecordRunScope(ctx context.Context, o Owner, scope RunScope) error {
	if scope.ProductVersion == "" || scope.EndpointID == "" || !isJSONObject(scope.Settings) {
		return ErrState
	}
	c := o.Command
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		var recorded, equal bool
		if err := tx.QueryRow(ctx, "SELECT product_version IS NOT NULL, product_version IS NOT DISTINCT FROM $3 AND endpoint_id IS NOT DISTINCT FROM $4 AND settings IS NOT DISTINCT FROM $5::jsonb FROM agent_runs WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID, scope.ProductVersion, scope.EndpointID, []byte(scope.Settings)).Scan(&recorded, &equal); err != nil {
			return err
		}
		if recorded {
			if !equal {
				return ErrState
			}
			return nil
		}
		_, err := tx.Exec(ctx, "UPDATE agent_runs SET product_version=$3,endpoint_id=$4,settings=$5 WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID, scope.ProductVersion, scope.EndpointID, []byte(scope.Settings))
		return err
	})
}

// RecordServerInfo stores the MCP initialize server info (for example
// {"name":...,"version":...}) under the ownership fence. A run may re-bind on a
// follow-up round, so later calls overwrite.
func (s *Store) RecordServerInfo(ctx context.Context, o Owner, info json.RawMessage) error {
	if !isJSONObject(info) {
		return ErrState
	}
	c := o.Command
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockOwner(ctx, tx, o); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE agent_runs SET server_info=$3 WHERE thread_id=$1 AND id=$2", c.ThreadID, c.RunID, []byte(info))
		return err
	})
}
