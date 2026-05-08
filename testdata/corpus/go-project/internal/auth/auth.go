// Package auth handles minimal authentication state.
package auth

import "errors"

// Session represents an open auth session.
type Session struct {
	user string
}

// User returns the authenticated user name.
func (s *Session) User() string {
	return s.user
}

// Open validates a token and returns a Session.
func Open(token string) (*Session, error) {
	if token == "" {
		return nil, errors.New("empty token")
	}
	return &Session{user: "demo"}, nil
}
