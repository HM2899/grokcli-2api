package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

type ModelRecord struct {
	ID                      string
	Name                    *string
	Description             *string
	OwnedBy                 string
	Hidden                  bool
	Synthetic               bool
	ContextWindow           *int64
	SupportsReasoningEffort *bool
	Extra                   map[string]any
	SortOrder               int
	FetchedAt               *time.Time
	UpdatedAt               *time.Time
}

func (c *Connector) ListModels(ctx context.Context, includeHidden bool) ([]ModelRecord, error) {
	query := `
		SELECT id, name, description, owned_by, hidden, synthetic,
		       context_window, supports_reasoning_effort, extra,
		       sort_order, fetched_at, updated_at
		FROM models
		WHERE ($1::boolean OR hidden = false)
		ORDER BY sort_order ASC, id ASC`
	rows, err := c.Pool.Query(ctx, query, includeHidden)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []ModelRecord{}
	for rows.Next() {
		var rec ModelRecord
		var extra []byte
		if err := rows.Scan(
			&rec.ID,
			&rec.Name,
			&rec.Description,
			&rec.OwnedBy,
			&rec.Hidden,
			&rec.Synthetic,
			&rec.ContextWindow,
			&rec.SupportsReasoningEffort,
			&extra,
			&rec.SortOrder,
			&rec.FetchedAt,
			&rec.UpdatedAt,
		); err != nil {
			return nil, err
		}
		rec.Extra = decodeJSONObject(extra)
		if rec.OwnedBy == "" {
			rec.OwnedBy = "xai"
		}
		if rec.SortOrder == 0 && rec.ID != "" {
			rec.SortOrder = 100
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func decodeJSONObject(data []byte) map[string]any {
	if len(data) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// ReplaceModels writes a full model catalog snapshot (non-synthetic rows).
func (c *Connector) ReplaceModels(ctx context.Context, items []map[string]any, meta map[string]any) (int, error) {
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM models WHERE COALESCE(synthetic, false) = false`); err != nil {
		return 0, err
	}
	n := 0
	for i, item := range items {
		id, _ := item["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		name, _ := item["name"].(string)
		desc, _ := item["description"].(string)
		ownedBy, _ := item["owned_by"].(string)
		if ownedBy == "" {
			ownedBy = "xai"
		}
		extraBytes, _ := json.Marshal(item)
		var ctxWin *int64
		if v, ok := item["context_window"].(float64); ok {
			iv := int64(v)
			ctxWin = &iv
		}
		var supports *bool
		if v, ok := item["supports_reasoning_effort"].(bool); ok {
			supports = &v
		}
		sortOrder := (i + 1) * 10
		synthetic := false
		if v, ok := item["synthetic"].(bool); ok {
			synthetic = v
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO models (
				id, name, description, owned_by, hidden, synthetic,
				context_window, supports_reasoning_effort, extra, sort_order, fetched_at, updated_at
			) VALUES (
				$1, NULLIF($2,''), NULLIF($3,''), $4, false, $9,
				$5, $6, $7::jsonb, $8, now(), now()
			)
			ON CONFLICT (id) DO UPDATE SET
				name = EXCLUDED.name,
				description = EXCLUDED.description,
				owned_by = EXCLUDED.owned_by,
				synthetic = EXCLUDED.synthetic,
				context_window = EXCLUDED.context_window,
				supports_reasoning_effort = EXCLUDED.supports_reasoning_effort,
				extra = EXCLUDED.extra,
				sort_order = EXCLUDED.sort_order,
				fetched_at = now(),
				updated_at = now()
`, id, name, desc, ownedBy, ctxWin, supports, extraBytes, sortOrder, synthetic); err != nil {
			return 0, err
		}
		n++
	}
	if meta != nil {
		metaBytes, _ := json.Marshal(meta)
		_, _ = tx.Exec(ctx, `
			INSERT INTO app_settings (key, value, updated_at)
			VALUES ('models_meta', $1::jsonb, now())
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()
		`, metaBytes)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
}
