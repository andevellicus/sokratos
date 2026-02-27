package gmail

import (
	"context"
	"fmt"

	gm "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"sokratos/googleauth"
	"sokratos/logger"
)

// Service is the application-wide Gmail API client.
var Service *gm.Service

// Init sets up the Gmail API client using OAuth2 credentials.
func Init(ctx context.Context, credentialsPath, tokenPath string, authIO *googleauth.AuthIO) error {
	client, err := googleauth.GetClient(ctx, "Gmail", credentialsPath, tokenPath, []string{gm.GmailReadonlyScope, gm.GmailSendScope}, authIO)
	if err != nil {
		return err
	}
	if client == nil {
		return nil // Features disabled smoothly
	}

	svc, err := gm.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return fmt.Errorf("create gmail service: %w", err)
	}

	Service = svc
	logger.Log.Info("Gmail API initialized")
	return nil
}
