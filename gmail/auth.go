package gmail

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	gm "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"sokratos/logger"
)

// Service is the application-wide Gmail API client.
var Service *gm.Service

// AuthIO provides callbacks for the OAuth2 flow so it can happen over
// Telegram (or any other channel) instead of stdin.
type AuthIO struct {
	Send    func(msg string)       // send a message to the user
	Receive func() (string, error) // wait for user input (blocks)
}

// Init sets up the Gmail API client using OAuth2 credentials.
// If credentialsPath does not exist, it logs a warning and returns nil
// so Gmail features are silently skipped. When a token doesn't exist yet,
// the OAuth2 flow sends the auth URL via authIO and waits for the user
// to paste the authorization code back.
func Init(ctx context.Context, credentialsPath, tokenPath string, authIO *AuthIO) error {
	creds, err := os.ReadFile(credentialsPath)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Log.Warnf("Gmail credentials file %q not found — Gmail features disabled", credentialsPath)
			return nil
		}
		return fmt.Errorf("read credentials: %w", err)
	}

	config, err := google.ConfigFromJSON(creds, gm.GmailReadonlyScope, gm.GmailSendScope)
	if err != nil {
		return fmt.Errorf("parse credentials: %w", err)
	}

	tok, err := loadToken(tokenPath)
	if err != nil {
		tok, err = getTokenFromWeb(config, authIO)
		if err != nil {
			return fmt.Errorf("obtain token: %w", err)
		}
		if err := saveToken(tokenPath, tok); err != nil {
			logger.Log.Warnf("Failed to save token: %v", err)
		}
	}

	client := config.Client(ctx, tok)
	svc, err := gm.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return fmt.Errorf("create gmail service: %w", err)
	}

	Service = svc
	logger.Log.Info("Gmail API initialized")
	return nil
}

func getTokenFromWeb(config *oauth2.Config, authIO *AuthIO) (*oauth2.Token, error) {
	// Use OOB-style redirect so Google displays the code in the browser.
	config.RedirectURL = "urn:ietf:wg:oauth:2.0:oob"

	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)

	send := func(msg string) { fmt.Println(msg) }
	if authIO != nil && authIO.Send != nil {
		send = authIO.Send
	}
	send(fmt.Sprintf("Gmail authorization required.\nOpen this link and paste the authorization code back here:\n\n%s", authURL))

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

	send("Gmail authorized successfully!")
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
