package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const (
	backoffInitial = 1 * time.Second
	backoffMax     = 60 * time.Second
)

// Connector manages reconnection to the hub with exponential backoff.
type Connector struct {
	hubAddr    string
	caCert     string // path
	clientCert string // path
	clientKey  string // path
	logger     *slog.Logger
}

// NewConnector creates a Connector with the given hub address and TLS cert paths.
func NewConnector(hubAddr, caCert, clientCert, clientKey string, logger *slog.Logger) *Connector {
	return &Connector{
		hubAddr:    hubAddr,
		caCert:     caCert,
		clientCert: clientCert,
		clientKey:  clientKey,
		logger:     logger,
	}
}

// Connect attempts to establish a mTLS connection to the hub. On failure it
// retries with exponential backoff starting at 1s and capped at 60s. It
// returns when a connection succeeds or the context is cancelled.
func (c *Connector) Connect(ctx context.Context) (*HubClient, error) {
	delay := backoffInitial

	for {
		client, err := DialWithMTLS(c.hubAddr, c.caCert, c.clientCert, c.clientKey, c.logger)
		if err == nil {
			return client, nil
		}

		c.logger.Warn("failed to connect to hub, retrying",
			"addr", c.hubAddr,
			"err", err,
			"backoff", delay,
		)

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect to hub: %w", ctx.Err())
		case <-time.After(delay):
		}

		delay *= 2
		if delay > backoffMax {
			delay = backoffMax
		}
	}
}
