package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func testConnector(t *testing.T) *Connector {
	t.Helper()
	dsn := os.Getenv("GROK2API_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("GROK2API_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn
}

func TestRecordUsageIdempotentIntegration(t *testing.T) {
	conn := testConnector(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("it-%d", time.Now().UnixNano())
	accountID := "acc-" + suffix
	keyID := "key-" + suffix
	modelID := "model-" + suffix
	requestID := "req-" + suffix

	_, err := conn.Pool.Exec(ctx, `INSERT INTO accounts (id, payload) VALUES ($1, '{}'::jsonb)`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Pool.Exec(ctx, `INSERT INTO account_pool (account_id, extra) VALUES ($1, '{}'::jsonb)`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = conn.Pool.Exec(ctx, `INSERT INTO api_keys (id, name, prefix, key_hash, enabled) VALUES ($1, 'integration', 'g2a-it', $2, true)`, keyID, "hash-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Pool.Exec(context.Background(), `DELETE FROM request_usage_idempotency WHERE request_id = $1`, requestID)
		_, _ = conn.Pool.Exec(context.Background(), `DELETE FROM usage_events WHERE request_id = $1`, requestID)
		_, _ = conn.Pool.Exec(context.Background(), `UPDATE usage_daily SET requests = requests - 1, success = success - 1, prompt_tokens = prompt_tokens - 10, completion_tokens = completion_tokens - 5, total_tokens = total_tokens - 15 WHERE day = CURRENT_DATE AND dim = 'global' AND dim_id = ''`)
		_, _ = conn.Pool.Exec(context.Background(), `DELETE FROM usage_daily WHERE day = CURRENT_DATE AND ((dim = 'key' AND dim_id = $1) OR (dim = 'account' AND dim_id = $2) OR (dim = 'model' AND dim_id = $3))`, keyID, accountID, modelID)
		_, _ = conn.Pool.Exec(context.Background(), `DELETE FROM api_keys WHERE id = $1`, keyID)
		_, _ = conn.Pool.Exec(context.Background(), `DELETE FROM account_pool WHERE account_id = $1`, accountID)
		_, _ = conn.Pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, accountID)
	})

	stream := false
	rec := UsageRecord{
		RequestID:        requestID,
		APIKeyID:         keyID,
		AccountID:        accountID,
		Model:            modelID,
		Protocol:         "openai_chat",
		Path:             "/v1/chat/completions",
		Stream:           &stream,
		OK:               true,
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15,
		StatusCode:       intPtrValue(httpStatusOK),
		Detail:           map[string]any{"test": true},
	}
	eventID, inserted, err := conn.RecordUsage(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if !inserted || eventID <= 0 {
		t.Fatalf("first record inserted=%v eventID=%d", inserted, eventID)
	}
	dupID, inserted, err := conn.RecordUsage(ctx, rec)
	if err != nil {
		t.Fatal(err)
	}
	if inserted || dupID != eventID {
		t.Fatalf("duplicate inserted=%v dupID=%d want existing %d", inserted, dupID, eventID)
	}

	var events int64
	if err := conn.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM usage_events WHERE request_id = $1`, requestID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("events=%d want 1", events)
	}
	assertUsageDaily(t, conn, "key", keyID, 1, 1, 0, 10, 5, 15)
	assertUsageDaily(t, conn, "account", accountID, 1, 1, 0, 10, 5, 15)
	assertUsageDaily(t, conn, "model", modelID, 1, 1, 0, 10, 5, 15)

	var keyPrompt, keyCompletion, keyTotal int64
	if err := conn.Pool.QueryRow(ctx, `SELECT prompt_tokens_total, completion_tokens_total, total_tokens_total FROM api_keys WHERE id = $1`, keyID).Scan(&keyPrompt, &keyCompletion, &keyTotal); err != nil {
		t.Fatal(err)
	}
	if keyPrompt != 10 || keyCompletion != 5 || keyTotal != 15 {
		t.Fatalf("key totals = %d/%d/%d", keyPrompt, keyCompletion, keyTotal)
	}
	var accPrompt, accCompletion, accTotal int64
	if err := conn.Pool.QueryRow(ctx, `SELECT prompt_tokens_total, completion_tokens_total, total_tokens_total FROM account_pool WHERE account_id = $1`, accountID).Scan(&accPrompt, &accCompletion, &accTotal); err != nil {
		t.Fatal(err)
	}
	if accPrompt != 10 || accCompletion != 5 || accTotal != 15 {
		t.Fatalf("account totals = %d/%d/%d", accPrompt, accCompletion, accTotal)
	}
}

func TestRecordUsageFailureDoesNotAccumulateTokensIntegration(t *testing.T) {
	conn := testConnector(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("it-fail-%d", time.Now().UnixNano())
	requestID := "req-" + suffix
	modelID := "model-" + suffix
	t.Cleanup(func() {
		_, _ = conn.Pool.Exec(context.Background(), `DELETE FROM request_usage_idempotency WHERE request_id = $1`, requestID)
		_, _ = conn.Pool.Exec(context.Background(), `DELETE FROM usage_events WHERE request_id = $1`, requestID)
		_, _ = conn.Pool.Exec(context.Background(), `UPDATE usage_daily SET requests = requests - 1, fail = fail - 1 WHERE day = CURRENT_DATE AND dim = 'global' AND dim_id = ''`)
		_, _ = conn.Pool.Exec(context.Background(), `DELETE FROM usage_daily WHERE day = CURRENT_DATE AND dim = 'model' AND dim_id = $1`, modelID)
	})

	status := 502
	_, inserted, err := conn.RecordUsage(ctx, UsageRecord{RequestID: requestID, Model: modelID, Protocol: "openai_chat", OK: false, PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15, StatusCode: &status, Error: "upstream failed"})
	if err != nil {
		t.Fatal(err)
	}
	if !inserted {
		t.Fatal("failure record was not inserted")
	}
	assertUsageDaily(t, conn, "model", modelID, 1, 0, 1, 0, 0, 0)
}

func TestPoolWritersIntegration(t *testing.T) {
	conn := testConnector(t)
	ctx := context.Background()
	accountID := fmt.Sprintf("acc-pool-%d", time.Now().UnixNano())
	_, err := conn.Pool.Exec(ctx, `INSERT INTO accounts (id, payload) VALUES ($1, '{}'::jsonb)`, accountID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Pool.Exec(context.Background(), `DELETE FROM account_pool WHERE account_id = $1`, accountID)
		_, _ = conn.Pool.Exec(context.Background(), `DELETE FROM accounts WHERE id = $1`, accountID)
	})

	if err := conn.ReportPoolSuccess(ctx, accountID, false); err != nil {
		t.Fatal(err)
	}
	var requests, successes int64
	var status string
	if err := conn.Pool.QueryRow(ctx, `SELECT request_count, success_count, pool_status FROM account_pool WHERE account_id = $1`, accountID).Scan(&requests, &successes, &status); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || successes != 1 || status != "normal" {
		t.Fatalf("success row = requests:%d success:%d status:%s", requests, successes, status)
	}

	code := 429
	until := time.Now().Add(time.Hour)
	if err := conn.ReportPoolFailure(ctx, PoolFailure{AccountID: accountID, Error: "rate limited", StatusCode: &code, CooldownUntil: &until, CooldownReason: "rate limited", CooldownCode: "rate_limit", BlockedModel: "grok-4", BlockedUntil: &until, Detail: map[string]any{"source": "test"}}); err != nil {
		t.Fatal(err)
	}
	var fails, cooldownCount int64
	var blocked map[string]any
	var blockedBytes []byte
	if err := conn.Pool.QueryRow(ctx, `SELECT fail_count, pool_status, cooldown_count, blocked_models FROM account_pool WHERE account_id = $1`, accountID).Scan(&fails, &status, &cooldownCount, &blockedBytes); err != nil {
		t.Fatal(err)
	}
	blocked = decodeMap(blockedBytes)
	if fails != 1 || status != "cooldown" || cooldownCount != 1 || blocked["grok-4"] == nil {
		t.Fatalf("failure row = fails:%d status:%s cooldown:%d blocked:%#v", fails, status, cooldownCount, blocked)
	}
}

const httpStatusOK = 200

func intPtrValue(value int) *int { return &value }

func assertUsageDaily(t *testing.T, conn *Connector, dim, dimID string, requests, success, fail, prompt, completion, total int64) {
	t.Helper()
	var got usageTotals
	if err := conn.Pool.QueryRow(context.Background(), `SELECT requests, success, fail, prompt_tokens, completion_tokens, total_tokens FROM usage_daily WHERE day = CURRENT_DATE AND dim = $1 AND dim_id = $2`, dim, dimID).Scan(&got.Requests, &got.Success, &got.Fail, &got.PromptTokens, &got.CompletionTokens, &got.TotalTokens); err != nil {
		t.Fatal(err)
	}
	if got.Requests != requests || got.Success != success || got.Fail != fail || got.PromptTokens != prompt || got.CompletionTokens != completion || got.TotalTokens != total {
		t.Fatalf("usage_daily %s/%s = %#v", dim, dimID, got)
	}
}
