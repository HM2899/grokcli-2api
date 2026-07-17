package postgres

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"
)

var knownTaskKinds = []string{"register", "sso_import", "json_import", "json_export", "probe", "renew"}

func (c *Connector) ListTasks(ctx context.Context, page, pageSize int, q, kind, status string) (map[string]any, error) {
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}
	where := []string{}
	args := []any{}
	q = strings.TrimSpace(q)
	if q != "" {
		args = append(args, "%"+q+"%")
		where = append(where, "(kind ILIKE $1 OR status ILIKE $1 OR summary ILIKE $1 OR COALESCE(task_id,'') ILIKE $1)")
	}
	kind = strings.TrimSpace(kind)
	if kind != "" && kind != "all" {
		args = append(args, kind)
		where = append(where, "kind = $"+itoaSQL(len(args)))
	}
	status = strings.TrimSpace(status)
	if status != "" && status != "all" {
		args = append(args, status)
		where = append(where, "status = $"+itoaSQL(len(args)))
	}
	wh := ""
	if len(where) > 0 {
		wh = " WHERE " + strings.Join(where, " AND ")
	}
	var total int64
	if err := c.Pool.QueryRow(ctx, "SELECT COUNT(*) FROM task_logs"+wh, args...).Scan(&total); err != nil {
		return nil, err
	}
	totalPages := 1
	if total > 0 {
		totalPages = int(math.Ceil(float64(total) / float64(pageSize)))
	}
	if page > totalPages {
		page = totalPages
	}
	offset := (page - 1) * pageSize
	queryArgs := append(append([]any{}, args...), pageSize, offset)
	rows, err := c.Pool.Query(ctx, `
		SELECT id, created_at, updated_at, finished_at, kind, task_id,
		       status, summary, detail, ok, progress_done, progress_total
		FROM task_logs`+wh+`
		ORDER BY created_at DESC, id DESC
		LIMIT $`+itoaSQL(len(args)+1)+` OFFSET $`+itoaSQL(len(args)+2), queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id int64
		var kindValue, statusValue string
		var taskID, summary *string
		var detailBytes []byte
		var okValue *bool
		var progressDone, progressTotal int64
		var createdAt, updatedAt, finishedAt *time.Time
		if err := rows.Scan(&id, &createdAt, &updatedAt, &finishedAt, &kindValue, &taskID, &statusValue, &summary, &detailBytes, &okValue, &progressDone, &progressTotal); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{
			"id":             id,
			"created_at":     unixOrNil(createdAt),
			"updated_at":     unixOrNil(updatedAt),
			"finished_at":    unixOrNil(finishedAt),
			"kind":           kindValue,
			"task_id":        stringPtr(taskID),
			"status":         statusValue,
			"summary":        stringPtr(summary),
			"detail":         decodeMap(detailBytes),
			"ok":             boolPtr(okValue),
			"progress_done":  progressDone,
			"progress_total": progressTotal,
			"action":         kindValue,
			"target_type":    "task",
			"target_id":      stringPtr(taskID),
			"actor":          "system",
			"ip":             nil,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{
		"ok":           true,
		"items":        items,
		"total":        total,
		"page":         page,
		"page_size":    pageSize,
		"total_pages":  totalPages,
		"q":            q,
		"kind":         nonEmpty(kind, "all"),
		"status":       nonEmpty(status, "all"),
		"action":       nonEmpty(kind, "all"),
		"store_source": "postgres",
		"log_type":     "task",
	}, nil
}

func (c *Connector) ListTaskKinds(ctx context.Context, limit int) ([]string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := c.Pool.Query(ctx, `
		SELECT kind, COUNT(*) AS c
		FROM task_logs
		GROUP BY kind
		ORDER BY c DESC, kind ASC
		LIMIT $1`, limit)
	if err != nil {
		return knownTaskKinds, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	out := []string{}
	for rows.Next() {
		var kind string
		var count int64
		if err := rows.Scan(&kind, &count); err != nil {
			return knownTaskKinds, err
		}
		if kind != "" && !seen[kind] {
			out = append(out, kind)
			seen[kind] = true
		}
	}
	for _, kind := range knownTaskKinds {
		if !seen[kind] {
			out = append(out, kind)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, rows.Err()
}

func boolPtr(ptr *bool) any {
	if ptr == nil {
		return nil
	}
	return *ptr
}

// WriteTask inserts or updates a task_logs row (Python task_log.record parity).
func (c *Connector) WriteTask(ctx context.Context, kind, status, summary, taskID string, ok *bool, detail map[string]any, progressDone, progressTotal int, finished bool) (int64, error) {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		kind = "task"
	}
	status = strings.TrimSpace(status)
	if status == "" {
		status = "done"
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detailBytes, err := json.Marshal(detail)
	if err != nil {
		return 0, err
	}
	var finishedAt *time.Time
	if finished {
		now := time.Now()
		finishedAt = &now
	}
	var id int64
	err = c.Pool.QueryRow(ctx, `
		INSERT INTO task_logs (kind, task_id, status, summary, detail, ok, progress_done, progress_total, finished_at, updated_at)
		VALUES ($1, NULLIF($2,''), $3, NULLIF($4,''), $5::jsonb, $6, $7, $8, $9::timestamptz, now())
		RETURNING id`, kind, taskID, status, summary, detailBytes, ok, progressDone, progressTotal, finishedAt).Scan(&id)
	return id, err
}
