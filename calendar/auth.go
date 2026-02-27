package calendar

import (
	"context"
	"fmt"

	cal "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"

	"sokratos/googleauth"
	"sokratos/logger"
)

// Service is the application-wide Google Calendar API client.
var Service *cal.Service

// Init sets up the Google Calendar API client using OAuth2 credentials.
func Init(ctx context.Context, credentialsPath, tokenPath string, authIO *googleauth.AuthIO) error {
	client, err := googleauth.GetClient(ctx, "Calendar", credentialsPath, tokenPath, []string{cal.CalendarScope}, authIO)
	if err != nil {
		return err
	}
	if client == nil {
		return nil // Features disabled smoothly
	}

	svc, err := cal.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return fmt.Errorf("create calendar service: %w", err)
	}

	Service = svc
	logger.Log.Info("Google Calendar API initialized")
	return nil
}
