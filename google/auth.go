package google

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"sokratos/logger"

	"golang.org/x/oauth2"
	goauth "golang.org/x/oauth2/google"
)

// AuthErrorMessage is the user-facing message returned when a Google API call
// fails due to an expired or revoked OAuth2 token.
const AuthErrorMessage = "⚠️ Google authorization has expired. Use /google to re-authenticate."

// IsAuthError returns true when err looks like an OAuth2 token-expiry or
// revocation error (invalid_grant, token expired/revoked). The oauth2 library
// wraps these inside HTTP transport errors, so string matching is the
// pragmatic detection approach.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "Token has been expired or revoked") ||
		strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "oauth2: cannot fetch token")
}

// AuthIO provides callbacks for the OAuth2 flow so it can happen over
// Telegram (or any other channel) instead of stdin.
type AuthIO struct {
	Send    func(msg string)       // send a message to the user
	Receive func() (string, error) // wait for user input (blocks)
}

// GetClientFromToken sets up an OAuth2-authenticated HTTP client using only
// a previously saved token. No interactive flow — if the token file doesn't
// exist, returns (nil, nil) so features are silently disabled. Used at startup
// for non-blocking initialization.
func GetClientFromToken(ctx context.Context, serviceName, credentialsPath, tokenPath string, scopes []string) (*http.Client, error) {
	creds, err := os.ReadFile(credentialsPath)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Log.Warnf("%s credentials file %q not found — %s features disabled", serviceName, credentialsPath, serviceName)
			return nil, nil
		}
		return nil, fmt.Errorf("read credentials: %w", err)
	}

	config, err := goauth.ConfigFromJSON(creds, scopes...)
	if err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}

	tok, err := loadToken(tokenPath)
	if err != nil {
		logger.Log.Warnf("%s token not found — use /google to authenticate", serviceName)
		return nil, nil
	}

	return config.Client(ctx, tok), nil
}

// GetClient sets up an OAuth2-authenticated HTTP client with interactive flow.
// If credentialsPath does not exist, it logs a warning and returns nil, nil
// so features can be silently skipped. When a token doesn't exist yet,
// the OAuth2 flow sends the auth URL via authIO and waits for the user
// to paste the authorization code back.
func GetClient(ctx context.Context, serviceName, credentialsPath, tokenPath string, scopes []string, authIO *AuthIO) (*http.Client, error) {
	creds, err := os.ReadFile(credentialsPath)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Log.Warnf("%s credentials file %q not found — %s features disabled", serviceName, credentialsPath, serviceName)
			return nil, nil
		}
		return nil, fmt.Errorf("read credentials: %w", err)
	}

	config, err := goauth.ConfigFromJSON(creds, scopes...)
	if err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}

	tok, err := loadToken(tokenPath)
	if err != nil {
		tok, err = getTokenFromWeb(config, serviceName, authIO)
		if err != nil {
			return nil, fmt.Errorf("obtain token: %w", err)
		}
		if err := saveToken(tokenPath, tok); err != nil {
			logger.Log.Warnf("Failed to save %s token: %v", serviceName, err)
		}
	}

	return config.Client(ctx, tok), nil
}

func getTokenFromWeb(config *oauth2.Config, serviceName string, authIO *AuthIO) (*oauth2.Token, error) {
	// Use OOB-style redirect so Google displays the code in the browser.
	config.RedirectURL = "urn:ietf:wg:oauth:2.0:oob"

	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)

	send := func(msg string) { fmt.Println(msg) }
	if authIO != nil && authIO.Send != nil {
		send = authIO.Send
	}
	send(fmt.Sprintf("%s authorization required.\nOpen this link and paste the authorization code back here:\n\n%s", serviceName, authURL))

	var code string
	if authIO != nil && authIO.Receive != nil {
		codeCh := make(chan string, 1)
		errCh := make(chan error, 1)
		go func() {
			c, err := authIO.Receive()
			if err != nil {
				errCh <- err
			} else {
				codeCh <- c
			}
		}()

		select {
		case code = <-codeCh:
		case err := <-errCh:
			return nil, fmt.Errorf("receive auth code: %w", err)
		case <-time.After(5 * time.Minute):
			return nil, fmt.Errorf("OAuth2 authorization timed out (5 minutes)")
		}
	} else {
		fmt.Print("Code: ")
		if _, err := fmt.Scan(&code); err != nil {
			return nil, fmt.Errorf("read auth code: %w", err)
		}
	}

	code = strings.TrimSpace(code)
	tok, err := config.Exchange(context.Background(), code)
	if err != nil {
		return nil, fmt.Errorf("exchange token: %w", err)
	}

	send(fmt.Sprintf("%s authorized successfully!", serviceName))
	return tok, nil
}

func loadToken(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var tok oauth2.Token
	if err := json.NewDecoder(f).Decode(&tok); err != nil {
		return nil, err
	}
	return &tok, nil
}

func saveToken(path string, tok *oauth2.Token) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(tok)
}
