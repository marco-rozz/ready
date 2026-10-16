package oauth

import (
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestCredential_Usable(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name string
		cred *Credential
		want bool
	}{
		{"nil credential", nil, false},
		{"no client id", &Credential{AccessToken: "a", Expiry: now.Add(time.Hour)}, false},
		{"valid access token", &Credential{ClientID: "c", AccessToken: "a", Expiry: now.Add(time.Hour)}, true},
		{"no-expiry access token", &Credential{ClientID: "c", AccessToken: "a"}, true},
		{"expired access token with refresh token", &Credential{ClientID: "c", AccessToken: "a", Expiry: now.Add(-time.Hour), RefreshToken: "r"}, true},
		{"expired access token without refresh token", &Credential{ClientID: "c", AccessToken: "a", Expiry: now.Add(-time.Hour)}, false},
		{"no access token, has refresh token", &Credential{ClientID: "c", RefreshToken: "r"}, true},
		{"nothing usable", &Credential{ClientID: "c"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cred.Usable(now); got != tt.want {
				t.Errorf("Usable() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCredential_ApplyTokenAndSameToken(t *testing.T) {
	cred := NewCredential(testServerKey)
	tok := &oauth2.Token{AccessToken: "a1", RefreshToken: "r1", TokenType: tokenTypeBearer, Expiry: time.Now().Add(time.Hour)}

	if cred.SameToken(tok) {
		t.Fatal("expected SameToken to be false before ApplyToken")
	}

	cred.ApplyToken(tok)

	if !cred.SameToken(tok) {
		t.Fatal("expected SameToken to be true after ApplyToken")
	}
	if cred.UpdatedAt.IsZero() {
		t.Fatal("expected UpdatedAt to be set")
	}

	got := cred.Token()
	if got.AccessToken != tok.AccessToken || got.RefreshToken != tok.RefreshToken {
		t.Errorf("Token() = %+v, want fields matching %+v", got, tok)
	}
}

func TestCredential_TokenNeverCarriesGrantedScopes(t *testing.T) {
	cred := NewCredential(testServerKey)
	cred.GrantedScopes = []string{"openid", "offline_access"}
	cred.ApplyToken(&oauth2.Token{AccessToken: "a1"})

	if cred.Token().Extra("scope") != nil {
		t.Fatal("expected no scope on the built token")
	}
}
