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

package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"net"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	sshclient "github.com/crossplane-contrib/provider-k3s/internal/clients/ssh"
)

// sshResponse is the canned reply fakeSSHServer sends back for one exec
// command. A non-zero Delay holds the reply for that long before sending it
// -- or abandons it early, without replying, if the client tears the session
// down first (which is what sshclient.Client.Execute does on context
// cancellation) -- so tests can exercise a slow remote command without
// hand-rolling a second SSH server.
type sshResponse struct {
	Stdout   string
	Stderr   string
	ExitCode uint32
	Delay    time.Duration
}

// fakeSSHServer is a real, in-process SSH server built on
// golang.org/x/crypto/ssh -- the same library the provider's own SSH client
// uses -- that replays a canned response for each exec command it receives.
// It stands in for a live k3s host in tests, exercising the actual SSH wire
// protocol end to end rather than substituting a hand-written mock for
// sshclient.Client.
type fakeSSHServer struct {
	config    *ssh.ServerConfig
	responses map[string]sshResponse
	fallback  sshResponse
}

// startFakeSSHServer starts the server on an ephemeral localhost port and
// registers its shutdown with t.Cleanup. responses is keyed by the exact
// command string the provider sends; any command with no entry gets
// fallback, so a test only has to spell out the commands it cares about.
func startFakeSSHServer(t *testing.T, responses map[string]sshResponse, fallback sshResponse) (host string, port int) {
	t.Helper()

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("build host key signer: %v", err)
	}

	config := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			return nil, nil // accept any credential: auth is not what this harness tests
		},
	}
	config.AddHostKey(signer)

	var lc net.ListenConfig
	listener, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	srv := &fakeSSHServer{config: config, responses: responses, fallback: fallback}
	go srv.serve(listener)

	addr := listener.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

func (s *fakeSSHServer) serve(listener net.Listener) {
	for {
		nConn, err := listener.Accept()
		if err != nil {
			return // listener closed: test is done
		}
		go s.handleConn(nConn)
	}
}

func (s *fakeSSHServer) handleConn(nConn net.Conn) {
	sConn, chans, reqs, err := ssh.NewServerConn(nConn, s.config)
	if err != nil {
		return
	}
	defer sConn.Close() //nolint:errcheck // best-effort cleanup in a test harness

	go ssh.DiscardRequests(reqs)

	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			_ = newChannel.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go s.handleSession(channel, requests)
	}
}

// handleSession answers "exec" requests with the canned response for the
// command, ignoring every other request type (shell, pty-req, ...) since
// the provider only ever runs a single command per session. A response
// carrying a Delay is held for that long -- or abandoned, without replying,
// if the requests channel closes first because the client tore the session
// down (context cancellation).
func (s *fakeSSHServer) handleSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	for {
		req, ok := <-requests
		if !ok {
			return
		}
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}

		cmd := parseExecPayload(req.Payload)
		if req.WantReply {
			_ = req.Reply(true, nil)
		}

		resp, ok := s.responses[cmd]
		if !ok {
			resp = s.fallback
		}

		if resp.Delay > 0 {
			select {
			case <-time.After(resp.Delay):
			case _, ok := <-requests:
				if !ok {
					// Client tore the session down before the delay
					// elapsed. Nothing left to reply to.
					return
				}
			}
		}

		if resp.Stdout != "" {
			_, _ = channel.Write([]byte(resp.Stdout))
		}
		if resp.Stderr != "" {
			_, _ = channel.Stderr().Write([]byte(resp.Stderr))
		}
		status := make([]byte, 4)
		binary.BigEndian.PutUint32(status, resp.ExitCode)
		_, _ = channel.SendRequest("exit-status", false, status)
		_ = channel.Close()
	}
}

// parseExecPayload decodes an SSH "exec" request payload, which the
// protocol encodes as a single string prefixed with its 4-byte big-endian
// length.
func parseExecPayload(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	n := binary.BigEndian.Uint32(payload[:4])
	if uint64(len(payload)) < 4+uint64(n) {
		return ""
	}
	return string(payload[4 : 4+n])
}

// newTestSSHClient dials the real sshclient.Client against a fakeSSHServer
// address, so Observe/Delete under test run their real SSH.Execute path.
func newTestSSHClient(t *testing.T, host string, port int) *sshclient.Client {
	t.Helper()

	c, err := sshclient.NewClient(sshclient.Config{
		Host:     host,
		Port:     port,
		Username: "test",
		Password: "test",
	})
	if err != nil {
		t.Fatalf("sshclient.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
