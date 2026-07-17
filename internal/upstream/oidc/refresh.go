// Package oidc implements xAI OIDC token refresh without the Grok CLI binary.
package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hm2899/grokcli-2api/internal/accounts"
)

const (
	DefaultIssuer   = "https://auth.x.ai"
	DefaultTokenURL = "https://auth.x.ai/oauth2/token"
	DefaultClientID = "grok-cli"
)

type Client struct {
	TokenURL string
	ClientID string
	HTTP     *http.Client
}

type RefreshError struct {
	Status    int
	Body      string
	Permanent bool
}

func (e *RefreshError) Error() string {
	if e == nil {
		return "refresh error"
	}
	if e.Body != "" {
		return e.Body
	}
	return fmt.Sprintf("refresh status %d", e.Status)
}

func (c *Client) tokenURL() string {
	if strings.TrimSpace(c.TokenURL) != "" {
		return strings.TrimSpace(c.TokenURL)
	}
	return DefaultTokenURL
}

func (c *Client) clientID(entry map[string]any) string {
	if id := stringField(entry, "oidc_client_id"); id != "" {
		return id
	}
	if strings.TrimSpace(c.ClientID) != "" {
		return strings.TrimSpace(c.ClientID)
	}
	return DefaultClientID
}

// RefreshAccessToken exchanges a refresh_token for a new access token payload.
func (c *Client) RefreshAccessToken(ctx context.Context, entry map[string]any) (map[string]any, error) {
	if entry == nil {
		return nil, errors.New("empty account entry")
	}
	if truthy(entry["refresh_invalid"]) {
		return nil, &RefreshError{Permanent: true, Body: firstNonEmpty(stringField(entry, "refresh_invalid_reason"), "refresh_token marked invalid")}
	}
	rt := firstNonEmpty(stringField(entry, "refresh_token"))
	if rt == "" {
		return nil, errors.New("no refresh_token on account")
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", rt)
	form.Set("client_id", c.clientID(entry))

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 400 {
		text := strings.TrimSpace(string(body))
		if len(text) > 400 {
			text = text[:400]
		}
		return nil, &RefreshError{
			Status:    resp.StatusCode,
			Body:      summarizeRefreshError(resp.StatusCode, text),
			Permanent: isPermanentRefreshFailure(resp.StatusCode, text),
		}
	}
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, err
	}
	if stringField(data, "access_token") == "" {
		return nil, errors.New("invalid refresh response")
	}
	return data, nil
}

// EntryFromTokenResponse builds a durable account entry from OIDC token JSON.
func EntryFromTokenResponse(tokenData, previous map[string]any) (string, map[string]any, error) {
	access := firstNonEmpty(stringField(tokenData, "access_token"), stringField(tokenData, "key"))
	if access == "" {
		return "", nil, errors.New("token response missing access_token")
	}
	entry := map[string]any{}
	if previous != nil {
		for k, v := range previous {
			entry[k] = v
		}
	}
	entry["key"] = access
	if rt := firstNonEmpty(stringField(tokenData, "refresh_token"), stringField(previous, "refresh_token")); rt != "" {
		entry["refresh_token"] = rt
	}
	if idt := stringField(tokenData, "id_token"); idt != "" {
		entry["id_token"] = idt
	}
	claims := accounts.DecodeJWTClaims(access)
	uid := firstNonEmpty(
		stringField(entry, "user_id"),
		stringField(entry, "principal_id"),
		stringField(claims, "principal_id"),
		stringField(claims, "sub"),
		stringField(tokenData, "sub"),
	)
	if uid != "" {
		entry["user_id"] = uid
		entry["principal_id"] = uid
	}
	if stringField(entry, "email") == "" {
		if email := firstNonEmpty(stringField(claims, "email"), stringField(tokenData, "email")); email != "" {
			entry["email"] = email
		}
	}
	if exp := accounts.ParseExpiresAt(tokenData["expires_in"], access); exp != nil {
		// expires_in is relative seconds when number small; ParseExpiresAt handles JWT exp fallback.
		if raw, ok := tokenData["expires_in"]; ok {
			switch v := raw.(type) {
			case float64:
				if v > 0 && v < 1e11 {
					e := float64(time.Now().Unix()) + v
					entry["expires_at"] = e
				} else {
					entry["expires_at"] = *exp
				}
			case json.Number:
				if f, err := v.Float64(); err == nil && f > 0 && f < 1e11 {
					entry["expires_at"] = float64(time.Now().Unix()) + f
				} else {
					entry["expires_at"] = *exp
				}
			default:
				entry["expires_at"] = *exp
			}
		} else {
			entry["expires_at"] = *exp
		}
	} else if exp2 := accounts.ParseExpiresAt(nil, access); exp2 != nil {
		entry["expires_at"] = *exp2
	}
	entry["auth_mode"] = firstNonEmpty(stringField(entry, "auth_mode"), "oidc_refresh")
	entry = accounts.MergeDurableAccountFields(entry, previous)
	// clear invalid markers on success
	delete(entry, "refresh_invalid")
	delete(entry, "refresh_invalid_reason")
	delete(entry, "refresh_invalid_at")
	aid := accounts.AccountStorageID(uid, stringField(entry, "oidc_client_id"), firstNonEmpty(stringField(previous, "id"), "https://auth.x.ai::imported"))
	return aid, entry, nil
}

func isPermanentRefreshFailure(status int, body string) bool {
	lower := strings.ToLower(body)
	if status == 400 || status == 401 {
		for _, needle := range []string{
			"refresh_token has been revoked",
			"refresh_token is invalid",
			"refresh_token revoked",
			"refresh_token expired",
			"invalid_grant",
			"token has been revoked",
			"invalid refresh",
		} {
			if strings.Contains(lower, needle) {
				return true
			}
		}
	}
	return false
}

func summarizeRefreshError(status int, body string) string {
	if body == "" {
		return fmt.Sprintf("refresh failed: HTTP %d", status)
	}
	return fmt.Sprintf("refresh failed: HTTP %d: %s", status, body)
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return strings.TrimSpace(v.String())
	case float64:
		return fmt.Sprintf("%.0f", v)
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "1" || s == "true" || s == "yes"
	case float64:
		return t != 0
	case int:
		return t != 0
	default:
		return false
	}
}
