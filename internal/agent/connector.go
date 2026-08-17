package agent

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ykhdr/hubfuse/internal/common"
	"google.golang.org/grpc/credentials"
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

	// creds caches the loaded mTLS credentials so repeat dials neither re-read
	// three files from disk nor risk picking up a half-written or re-minted
	// certificate set (see dialWithCreds). Guarded by credsMu.
	//
	// The cache is lazy and stores only SUCCESS, both deliberately. Loading
	// eagerly in NewConnector would turn a missing or corrupt client.crt from
	// "Connect keeps retrying until the files appear" into a hard NewDaemon
	// failure — an undeclared change to how the daemon starts. Caching a
	// failure would be the mirror-image bug: the retry loop would be
	// permanently hopeless even after the files showed up. (#77)
	credsMu sync.Mutex
	creds   credentials.TransportCredentials
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

// Dial builds one hub connection and returns. It does NOT retry, and that is
// the whole point of it existing next to Connect: the daemon replaces a
// connection from inside reconnectSession, which already runs its own backoff
// loop. Handing that path a function with a second, unbounded retry loop inside
// it would nest two backoffs and make the outer one's timing a fiction. (#77)
//
// Because grpc.NewClient is lazy, "success" here means only that the target
// parsed and the credentials loaded — no TCP, no TLS handshake, nothing that can
// hang. Whether the hub is actually reachable is answered by the first RPC.
func (c *Connector) Dial() (*HubClient, error) {
	creds, err := c.transportCreds()
	if err != nil {
		return nil, err
	}
	return dialWithCreds(c.hubAddr, creds, c.logger)
}

// transportCreds returns the cached mTLS credentials, loading them on first use.
func (c *Connector) transportCreds() (credentials.TransportCredentials, error) {
	c.credsMu.Lock()
	defer c.credsMu.Unlock()

	if c.creds != nil {
		return c.creds, nil
	}

	tlsCfg, err := common.LoadTLSClientConfig(c.caCert, c.clientCert, c.clientKey)
	if err != nil {
		return nil, fmt.Errorf("load mTLS config: %w", err)
	}
	c.creds = credentials.NewTLS(tlsCfg)
	return c.creds, nil
}

// Connect attempts to establish a mTLS connection to the hub. On failure it
// retries with exponential backoff starting at 1s and capped at 60s. It
// returns when a connection succeeds or the context is cancelled.
//
// It goes through Dial so the bootstrap connection and every later replacement
// share one credential cache — which is also what guarantees they all carry the
// same identity for the daemon's lifetime.
func (c *Connector) Connect(ctx context.Context) (*HubClient, error) {
	delay := backoffInitial

	for {
		client, err := c.Dial()
		if err == nil {
			return client, nil
		}

		if isTLSCertError(err) {
			return nil, fmt.Errorf("hub certificate not trusted — the hub CA may have changed, please re-join with: hubfuse join %s", c.hubAddr)
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

// isTLSCertError reports whether err is a TLS certificate validation error
// that will not resolve on retry (e.g. the hub CA has changed).
func isTLSCertError(err error) bool {
	if err == nil {
		return false
	}
	var unknownAuth x509.UnknownAuthorityError
	if errors.As(err, &unknownAuth) {
		return true
	}
	var certInvalid x509.CertificateInvalidError
	return errors.As(err, &certInvalid)
}
