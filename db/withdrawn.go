package db

// OutputWithdrawal identifies a deleted producer of retained exploration output.
type OutputWithdrawal struct {
	IntentID int64  `json:"intent_id"`
	Reason   string `json:"reason"`
}

// OutputWithdrawals follows persisted causal lineage and reverse covers edges,
// so both retained output and digests containing it carry the deletion reason.
// Hard deletion removes the producer and its edges; it cannot be reconstructed here.
func (s *ExplorationStore) OutputWithdrawals() (map[int64][]OutputWithdrawal, error) {
	rows, err := s.db.Query(`WITH RECURSIVE withdrawn(node_id, intent_id, reason) AS (
		SELECT id, id, COALESCE(delete_reason, '') FROM exploration_nodes
		WHERE exploration_id=$1 AND kind='intent' AND state='deleted'
		UNION
		SELECT CASE WHEN e.rel='covers' THEN e.src_id ELSE e.dst_id END, w.intent_id, w.reason
		FROM withdrawn w JOIN exploration_edges e ON e.exploration_id=$1 AND (
			(e.src_id=w.node_id AND e.rel IN ('yields', 'derived_from')) OR
			(e.dst_id=w.node_id AND e.rel='covers'))
	)
	SELECT node_id, intent_id, reason FROM withdrawn ORDER BY node_id, intent_id`, s.expID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]OutputWithdrawal{}
	for rows.Next() {
		var id int64
		var w OutputWithdrawal
		if err := rows.Scan(&id, &w.IntentID, &w.Reason); err != nil {
			return nil, err
		}
		out[id] = append(out[id], w)
	}
	return out, rows.Err()
}
