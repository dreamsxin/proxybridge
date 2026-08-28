package auth

import (
	"context"
	"errors"
	"fmt"

	"dbridge/internal/center"
)

var (
	ErrBanned      = errors.New("ip banned")
	ErrAuthFailed  = errors.New("authentication failed")
	ErrUnavailable = errors.New("auth service unavailable")
)

// Authenticator handles credential verification with caching and IP protection.
type Authenticator struct {
	client      *center.Client
	successCache *successCache
	failCache    *failCache
	guard        *Guard
}

func NewAuthenticator(client *center.Client) *Authenticator {
	return &Authenticator{
		client:       client,
		successCache: newSuccessCache(),
		failCache:    newFailCache(),
		guard:        NewGuard(),
	}
}

// Authenticate verifies credentials and returns upstream proxy info.
// clientIP is the connecting client's IP (with or without port).
func (a *Authenticator) Authenticate(ctx context.Context, username, password, clientIP string) (*center.AuthResponse, error) {
	// 1. check IP ban
	if a.guard.IsBanned(clientIP) {
		return nil, ErrBanned
	}

	credKey := fmt.Sprintf("%s:%s", username, password)

	// 2. check fail cache (same credentials recently failed)
	if a.failCache.has(credKey) {
		return nil, ErrAuthFailed
	}

	// 3. check success cache
	if cached, ok := a.successCache.get(credKey); ok {
		return cached, nil
	}

	// 4. call central server
	resp, err := a.client.Authenticate(ctx, center.AuthRequest{
		Username: username,
		Password: password,
		ClientIP: clientIP,
	})
	if err != nil {
		return nil, ErrUnavailable
	}

	// 5. handle result
	if resp == nil {
		a.failCache.set(credKey)
		if a.guard.RecordFailure(clientIP, credKey) {
			return nil, ErrBanned
		}
		return nil, ErrAuthFailed
	}

	// success: clear any prior failure records for this IP
	a.guard.ClearFailures(clientIP)
	a.successCache.set(credKey, resp)
	return resp, nil
}
