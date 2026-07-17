package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type APIKeyRecord struct {
	ID           string
	Name         string
	Prefix       string
	KeyHash      string
	Secret       *string
	Enabled      bool
	Note         string
	CreatedAt    *time.Time
	LastUsedAt   *time.Time
	RequestCount int64
}

func (c *Connector) ListAPIKeys(ctx context.Context) ([]APIKeyRecord, error) {
	rows, err := c.Pool.Query(ctx, `
		SELECT id, name, prefix, key_hash, secret, enabled, note,
		       created_at, last_used_at, request_count
		FROM api_keys ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []APIKeyRecord{}
	for rows.Next() {
		var rec APIKeyRecord
		if err := rows.Scan(
			&rec.ID,
			&rec.Name,
			&rec.Prefix,
			&rec.KeyHash,
			&rec.Secret,
			&rec.Enabled,
			&rec.Note,
			&rec.CreatedAt,
			&rec.LastUsedAt,
			&rec.RequestCount,
		); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (c *Connector) FindAPIKeyByHash(ctx context.Context, keyHash string) (*APIKeyRecord, error) {
	if keyHash == "" {
		return nil, nil
	}
	row := c.Pool.QueryRow(ctx, `
		SELECT id, name, prefix, key_hash, secret, enabled, note,
		       created_at, last_used_at, request_count
		FROM api_keys WHERE key_hash = $1 LIMIT 1`, keyHash)
	var rec APIKeyRecord
	if err := row.Scan(
		&rec.ID,
		&rec.Name,
		&rec.Prefix,
		&rec.KeyHash,
		&rec.Secret,
		&rec.Enabled,
		&rec.Note,
		&rec.CreatedAt,
		&rec.LastUsedAt,
		&rec.RequestCount,
	); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &rec, nil
}

func (c *Connector) HasEnabledAPIKeys(ctx context.Context) (bool, error) {
	row := c.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM api_keys WHERE enabled = true)")
	var exists bool
	if err := row.Scan(&exists); err != nil {
		return false, err
	}
	return exists, nil
}

func (c *Connector) TouchAPIKeyUsage(ctx context.Context, keyID string) error {
	if keyID == "" || keyID == "env" {
		return nil
	}
	_, err := c.Pool.Exec(ctx, `
		UPDATE api_keys
		SET request_count = request_count + 1,
		    last_used_at = now()
		WHERE id = $1`, keyID)
	return err
}

func (r APIKeyRecord) PublicMap() map[string]any {
	out := map[string]any{
		"id":            r.ID,
		"name":          nonEmpty(r.Name, "unnamed"),
		"prefix":        r.Prefix,
		"created_at":    unixOrZero(r.CreatedAt),
		"enabled":       r.Enabled,
		"note":          r.Note,
		"last_used_at":  unixOrNil(r.LastUsedAt),
		"request_count": r.RequestCount,
		"key_hint":      r.Prefix + "…****",
		"has_secret":    r.Secret != nil && strings.TrimSpace(*r.Secret) != "",
	}
	if r.Secret != nil {
		secret := strings.TrimSpace(*r.Secret)
		if secret != "" && !strings.HasPrefix(secret, "enc:v1:") {
			out["secret"] = secret
		}
	}
	return out
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func unixOrZero(value *time.Time) float64 {
	if value == nil {
		return 0
	}
	return float64(value.UnixNano()) / 1e9
}

func unixOrNil(value *time.Time) any {
	if value == nil {
		return nil
	}
	return float64(value.UnixNano()) / 1e9
}

type CreateAPIKeyResult struct {
	Record APIKeyRecord
	Secret string
}

func (c *Connector) CreateAPIKey(ctx context.Context, name, note string) (CreateAPIKeyResult, error) {
	if c == nil || c.Pool == nil {
		return CreateAPIKeyResult{}, errors.New("postgres unavailable")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "default"
	}
	note = strings.TrimSpace(note)
	secret, prefix, err := newAPIKeySecret()
	if err != nil {
		return CreateAPIKeyResult{}, err
	}
	id := newUUID()
	hash := hashAPIKey(secret)
	now := time.Now()
	_, err = c.Pool.Exec(ctx, `
		INSERT INTO api_keys (id, name, prefix, key_hash, secret, enabled, note, created_at)
		VALUES ($1, $2, $3, $4, $5, true, $6, $7)
	`, id, name, prefix, hash, secret, note, now)
	if err != nil {
		return CreateAPIKeyResult{}, err
	}
	secretCopy := secret
	rec := APIKeyRecord{ID: id, Name: name, Prefix: prefix, KeyHash: hash, Secret: &secretCopy, Enabled: true, Note: note, CreatedAt: &now}
	return CreateAPIKeyResult{Record: rec, Secret: secret}, nil
}

func (c *Connector) UpdateAPIKey(ctx context.Context, id string, name, note *string, enabled *bool) (*APIKeyRecord, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, errors.New("key id required")
	}
	current, err := c.getAPIKey(ctx, id)
	if err != nil {
		return nil, err
	}
	if name != nil {
		current.Name = strings.TrimSpace(*name)
	}
	if note != nil {
		current.Note = strings.TrimSpace(*note)
	}
	if enabled != nil {
		current.Enabled = *enabled
	}
	_, err = c.Pool.Exec(ctx, `
		UPDATE api_keys SET name = $2, note = $3, enabled = $4 WHERE id = $1
	`, id, current.Name, current.Note, current.Enabled)
	if err != nil {
		return nil, err
	}
	return current, nil
}

func (c *Connector) DeleteAPIKey(ctx context.Context, id string) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, errors.New("key id required")
	}
	tag, err := c.Pool.Exec(ctx, `DELETE FROM api_keys WHERE id = $1`, id)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (c *Connector) RegenerateAPIKey(ctx context.Context, id string) (CreateAPIKeyResult, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return CreateAPIKeyResult{}, errors.New("key id required")
	}
	current, err := c.getAPIKey(ctx, id)
	if err != nil {
		return CreateAPIKeyResult{}, err
	}
	secret, prefix, err := newAPIKeySecret()
	if err != nil {
		return CreateAPIKeyResult{}, err
	}
	hash := hashAPIKey(secret)
	_, err = c.Pool.Exec(ctx, `
		UPDATE api_keys SET prefix = $2, key_hash = $3, secret = $4 WHERE id = $1
	`, id, prefix, hash, secret)
	if err != nil {
		return CreateAPIKeyResult{}, err
	}
	secretCopy := secret
	current.Prefix = prefix
	current.KeyHash = hash
	current.Secret = &secretCopy
	return CreateAPIKeyResult{Record: *current, Secret: secret}, nil
}

func (c *Connector) getAPIKey(ctx context.Context, id string) (*APIKeyRecord, error) {
	row := c.Pool.QueryRow(ctx, `
		SELECT id, name, prefix, key_hash, secret, enabled, note, created_at, last_used_at, request_count
		FROM api_keys WHERE id = $1
	`, id)
	var rec APIKeyRecord
	if err := row.Scan(&rec.ID, &rec.Name, &rec.Prefix, &rec.KeyHash, &rec.Secret, &rec.Enabled, &rec.Note, &rec.CreatedAt, &rec.LastUsedAt, &rec.RequestCount); err != nil {
		if err == pgx.ErrNoRows {
			return nil, errors.New("api key not found")
		}
		return nil, err
	}
	return &rec, nil
}

func newAPIKeySecret() (secret, prefix string, err error) {
	buf := make([]byte, 24)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	raw := hex.EncodeToString(buf)
	secret = "sk-g2a-" + raw
	if len(secret) > 12 {
		prefix = secret[:12]
	} else {
		prefix = secret
	}
	return secret, prefix, nil
}

func hashAPIKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("key-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
