package config

import (
	"context"
	"os"
)

type EmailConfig struct {
	EmailName    string
	EmailAddress string
}

// StrictEmailVerification requires a fresh provider verdict before campaign sends.
func StrictEmailVerification() bool { return isTrue(os.Getenv("EMAIL_VERIFY_STRICT")) }

func (c *Config) LoadEmailConfig(ctx context.Context) (*EmailConfig, error) {
	emailName, err := c.GetStringRaw(ctx, "EMAIL_NAME", "email/name")
	if err != nil {
		return nil, err
	}

	emailAddress, err := c.GetStringRaw(ctx, "EMAIL_ADDRESS", "email/address")
	if err != nil {
		return nil, err
	}

	return &EmailConfig{
		EmailName:    emailName,
		EmailAddress: emailAddress,
	}, nil
}
