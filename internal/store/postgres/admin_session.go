package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const adminSessionTTL = 7 * 24 * time.Hour

func (c *Connector) VerifyAdminSession(token string) bool {
	token = strings.TrimSpace(token)
	if token == "" || c == nil || c.Pool == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var data []byte
	if err := c.Pool.QueryRow(ctx, "SELECT value FROM app_settings WHERE key = 'sessions'").Scan(&data); err != nil {
		return false
	}
	var sessions map[string]any
	if err := json.Unmarshal(data, &sessions); err != nil || sessions == nil {
		return false
	}
	raw, ok := sessions[token]
	if !ok {
		return false
	}
	ts, ok := toFloat(raw)
	if !ok || time.Since(time.Unix(int64(ts), 0)) > adminSessionTTL {
		return false
	}
	return true
}

func toFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int64:
		return float64(v), true
	case int:
		return float64(v), true
	default:
		return 0, false
	}
}

func (c *Connector) CreateAdminSession(token string) error {
	token = strings.TrimSpace(token)
	if token == "" || c == nil || c.Pool == nil {
		return errors.New("postgres admin session store unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var data []byte
	err := c.Pool.QueryRow(ctx, "SELECT value FROM app_settings WHERE key = 'sessions'").Scan(&data)
	sessions := map[string]any{}
	if err == nil {
		_ = json.Unmarshal(data, &sessions)
		if sessions == nil {
			sessions = map[string]any{}
		}
	}
	now := float64(time.Now().Unix())
	// prune expired
	for k, v := range sessions {
		ts, ok := toFloat(v)
		if !ok || time.Since(time.Unix(int64(ts), 0)) > adminSessionTTL {
			delete(sessions, k)
		}
	}
	sessions[token] = now
	encoded, err := json.Marshal(sessions)
	if err != nil {
		return err
	}
	_, err = c.Pool.Exec(ctx, `
		INSERT INTO app_settings (key, value, updated_at)
		VALUES ('sessions', $1::jsonb, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()
	`, encoded)
	return err
}

func (c *Connector) DeleteAdminSession(token string) error {
	token = strings.TrimSpace(token)
	if token == "" || c == nil || c.Pool == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var data []byte
	err := c.Pool.QueryRow(ctx, "SELECT value FROM app_settings WHERE key = 'sessions'").Scan(&data)
	if err != nil {
		return nil
	}
	var sessions map[string]any
	if err := json.Unmarshal(data, &sessions); err != nil || sessions == nil {
		return nil
	}
	delete(sessions, token)
	encoded, err := json.Marshal(sessions)
	if err != nil {
		return err
	}
	_, err = c.Pool.Exec(ctx, `UPDATE app_settings SET value = $1::jsonb, updated_at = now() WHERE key = 'sessions'`, encoded)
	return err
}
