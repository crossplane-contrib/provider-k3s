/*
Copyright 2025 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ssh

import (
	"bytes"
	"fmt"
	"net"
	"strings"

	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
)

const (
	errParsePrivateKey = "cannot parse SSH private key"
	errSSHDial         = "cannot connect to SSH server"
	errSSHSession      = "cannot create SSH session"
	errSSHExecute      = "cannot execute SSH command"
)

// Config holds SSH connection parameters.
type Config struct {
	Host       string
	Port       int
	Username   string
	Password   string // for password-based auth
	PrivateKey []byte // for key-based auth (PEM-encoded)
}

// Client wraps an SSH connection for remote command execution.
type Client struct {
	conn *ssh.Client
}

// NewClient establishes an SSH connection using the provided config.
// It tries key-based auth first (if PrivateKey is set), then falls back to password.
func NewClient(cfg Config) (*Client, error) {
	var authMethods []ssh.AuthMethod

	if len(cfg.PrivateKey) > 0 {
		signer, err := ssh.ParsePrivateKey(cfg.PrivateKey)
		if err != nil {
			return nil, errors.Wrap(err, errParsePrivateKey)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	if cfg.Password != "" {
		authMethods = append(authMethods, ssh.Password(cfg.Password))
	}

	sshConfig := &ssh.ClientConfig{
		User:            cfg.Username,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // same as k3sup
	}

	address := net.JoinHostPort(cfg.Host, fmt.Sprintf("%d", cfg.Port))
	conn, err := ssh.Dial("tcp", address, sshConfig)
	if err != nil {
		return nil, errors.Wrap(err, errSSHDial)
	}

	return &Client{conn: conn}, nil
}

// Execute runs a command on the remote host and returns stdout and stderr.
func (c *Client) Execute(cmd string) (stdout, stderr string, err error) {
	session, err := c.conn.NewSession()
	if err != nil {
		return "", "", errors.Wrap(err, errSSHSession)
	}
	defer session.Close() //nolint:errcheck

	var stdoutBuf, stderrBuf bytes.Buffer
	session.Stdout = &stdoutBuf
	session.Stderr = &stderrBuf

	if err := session.Run(cmd); err != nil {
		return strings.TrimSpace(stdoutBuf.String()), strings.TrimSpace(stderrBuf.String()), errors.Wrap(err, errSSHExecute)
	}

	return strings.TrimSpace(stdoutBuf.String()), strings.TrimSpace(stderrBuf.String()), nil
}

// Close terminates the SSH connection.
func (c *Client) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}
