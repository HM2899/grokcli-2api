package grok

import "strings"

type Credentials struct {
	Token  string
	Email  string
	UserID string
	TeamID string
	ID     string
}

func AccountFromCredentials(creds Credentials) Account {
	id := strings.TrimSpace(creds.ID)
	if id == "" {
		id = strings.TrimSpace(creds.UserID)
	}
	if id == "" {
		id = strings.TrimSpace(creds.Email)
	}
	return Account{ID: id, Token: strings.TrimSpace(creds.Token)}
}

func HeadersForCredentials(creds Credentials, model string, client Client) map[string]string {
	return client.Headers(strings.TrimSpace(creds.Token), model)
}
