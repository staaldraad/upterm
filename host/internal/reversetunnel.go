package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os/user"
	"strings"
	"time"

	"github.com/jingweno/upterm/host/api"
	"github.com/jingweno/upterm/server"
	"github.com/jingweno/upterm/upterm"
	"github.com/jingweno/upterm/ws"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
)

const (
	publickeyAuthError = "ssh: unable to authenticate, attempted methods [none]"
)

type ReverseTunnel struct {
	*ssh.Client

	Host              *url.URL
	SessionID         string
	Signers           []ssh.Signer
	KeepAliveDuration time.Duration
	Logger            log.FieldLogger

	ln net.Listener
}

func (c *ReverseTunnel) Close() {
	c.ln.Close()
	c.Client.Close()
}

func (c *ReverseTunnel) Listener() net.Listener {
	return c.ln
}

func (c *ReverseTunnel) Establish(ctx context.Context) (*server.ServerInfo, error) {
	user, err := user.Current()
	if err != nil {
		return nil, err
	}

	var auths []ssh.AuthMethod
	for _, signer := range c.Signers {
		auths = append(auths, ssh.PublicKeys(signer))
	}

	id := &api.Identifier{
		Id:   user.Username,
		Type: api.Identifier_HOST,
	}
	encodedID, err := api.EncodeIdentifier(id)
	if err != nil {
		return nil, err
	}

	config := &ssh.ClientConfig{
		User:            encodedID,
		Auth:            auths,
		ClientVersion:   upterm.HostSSHClientVersion,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	if isWSScheme(c.Host.Scheme) {
		u, _ := url.Parse(c.Host.String()) // clone
		u.User = url.UserPassword(encodedID, "")
		c.Client, err = ws.NewSSHClient(u, config, false)
	} else {
		c.Client, err = ssh.Dial("tcp", c.Host.Host, config)
	}

	if err != nil {
		return nil, sshDialError(c.Host.String(), err)
	}

	c.ln, err = c.Client.Listen("unix", c.SessionID)
	if err != nil {
		return nil, fmt.Errorf("unable to create reverse tunnel: %w", err)
	}

	// make sure connection is alive
	go keepAlive(ctx, c.KeepAliveDuration, func() {
		_, _, err := c.Client.SendRequest(upterm.OpenSSHKeepAliveRequestType, true, nil)
		if err != nil {
			c.Logger.WithError(err).Error("error pinging server")
		}
	})

	return c.serverInfo()
}

func (c *ReverseTunnel) serverInfo() (*server.ServerInfo, error) {
	ok, body, err := c.Client.SendRequest(upterm.ServerServerInfoRequestType, true, nil)
	if err != nil {
		return nil, fmt.Errorf("error fetching server info: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("error fetching server info")
	}
	var info *server.ServerInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("error unmarshaling server info: %w", err)
	}

	return info, nil
}

func keepAlive(ctx context.Context, d time.Duration, fn func()) {
	ticker := time.NewTicker(d)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fn()
		}
	}
}

func isWSScheme(scheme string) bool {
	return scheme == "ws" || scheme == "wss"
}

type PermissionDeniedError struct {
	host string
	err  error
}

func (e *PermissionDeniedError) Error() string {
	return fmt.Sprintf("%s: Permission denied (publickey).", e.host)
}

func (e *PermissionDeniedError) Unwrap() error { return e.err }

func sshDialError(host string, err error) error {
	if strings.Contains(err.Error(), publickeyAuthError) {
		return &PermissionDeniedError{
			host: host,
			err:  err,
		}
	}

	return fmt.Errorf("ssh dial error: %w", err)
}
