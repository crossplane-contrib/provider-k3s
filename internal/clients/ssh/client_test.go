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
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// slowSSHServer is a real, in-process SSH server that holds every "exec"
// session open for delay before replying, so tests can exercise Execute's
// context-cancellation path against the actual SSH wire protocol rather than
// a hand-written substitute for *ssh.Session.
type slowSSHServer struct {
	config *ssh.ServerConfig
	delay  time.Duration
}

func startSlowSSHServer(t *testing.T, delay time.Duration) (host string, port int) {
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

	srv := &slowSSHServer{config: config, delay: delay}
	go srv.serve(listener)

	addr := listener.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

func (s *slowSSHServer) serve(listener net.Listener) {
	for {
		nConn, err := listener.Accept()
		if err != nil {
			return // listener closed: test is done
		}
		go s.handleConn(nConn)
	}
}

func (s *slowSSHServer) handleConn(nConn net.Conn) {
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

// handleSession answers an "exec" request only after s.delay has elapsed,
// simulating a slow remote command (a multi-minute k3s install, scaled down
// for the test). It exits early -- without ever replying -- if the requests
// channel closes first, which is what happens when Execute aborts a session
// on context cancellation (closing the session tears down its request
// stream from the server's point of view).
func (s *slowSSHServer) handleSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	for req := range requests {
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		if req.WantReply {
			_ = req.Reply(true, nil)
		}

		select {
		case <-time.After(s.delay):
			status := make([]byte, 4)
			binary.BigEndian.PutUint32(status, 0)
			_, _ = channel.Write([]byte("done"))
			_, _ = channel.SendRequest("exit-status", false, status)
			_ = channel.Close()
			return
		case _, ok := <-requests:
			if !ok {
				// Client tore down the session before the delay
				// elapsed -- exactly what Execute does on ctx
				// cancellation. Nothing left to reply to.
				return
			}
		}
	}
}

// fakeExecErrorServer is a real, in-process SSH server that answers every
// "exec" request with a non-zero exit status and a fixed stderr payload --
// standing in for a k3s install/join script that failed after echoing
// secret material back on its error path.
type fakeExecErrorServer struct {
	config *ssh.ServerConfig
	stderr string
}

func startFakeExecErrorServer(t *testing.T, kubeconfigPEM, nodeToken string) (host string, port int) {
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

	stderr := "install failed while dumping diagnostics:\nkubeconfig=" + kubeconfigPEM + "\nnode-token=" + nodeToken
	srv := &fakeExecErrorServer{config: config, stderr: stderr}
	go srv.serve(listener)

	addr := listener.Addr().(*net.TCPAddr)
	return "127.0.0.1", addr.Port
}

func (s *fakeExecErrorServer) serve(listener net.Listener) {
	for {
		nConn, err := listener.Accept()
		if err != nil {
			return // listener closed: test is done
		}
		go s.handleConn(nConn)
	}
}

func (s *fakeExecErrorServer) handleConn(nConn net.Conn) {
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

func (s *fakeExecErrorServer) handleSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	for req := range requests {
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		if req.WantReply {
			_ = req.Reply(true, nil)
		}

		_, _ = channel.Stderr().Write([]byte(s.stderr))
		status := make([]byte, 4)
		binary.BigEndian.PutUint32(status, 1)
		_, _ = channel.SendRequest("exit-status", false, status)
		_ = channel.Close()
		return
	}
}

func newSlowTestClient(t *testing.T, host string, port int) *Client {
	t.Helper()

	c, err := NewClient(Config{
		Host:     host,
		Port:     port,
		Username: "test",
		Password: "test",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestExecuteReturnsPromptlyOnContextCancellation proves Execute does not
// block past the caller's deadline: a command that would otherwise take far
// longer than the context's timeout must still cause Execute to return at
// (approximately) the deadline, not at the command's real completion time.
func TestExecuteReturnsPromptlyOnContextCancellation(t *testing.T) {
	host, port := startSlowSSHServer(t, 5*time.Second)
	c := newSlowTestClient(t, host, port)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := c.Execute(ctx, "sleep 5")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want an error when the context deadline is exceeded before the remote command finishes")
	}
	if elapsed >= 5*time.Second {
		t.Errorf("want Execute to return at the context deadline (~200ms), got %s -- it blocked for the remote command's full duration instead", elapsed)
	}
}

// TestExecuteSucceedsWithinDeadline proves the context-awareness added to
// Execute does not break the ordinary, well-within-budget case.
func TestExecuteSucceedsWithinDeadline(t *testing.T) {
	host, port := startSlowSSHServer(t, 10*time.Millisecond)
	c := newSlowTestClient(t, host, port)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stdout, _, err := c.Execute(ctx, "echo hi")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if stdout != "done" {
		t.Errorf("want stdout %q, got %q", "done", stdout)
	}
}

// testKubeconfigPEM and testNodeToken are the two secret shapes this
// provider's command output can carry: a kubeconfig's embedded client
// certificate, and a k3s node-join token. Neither string may survive
// redactSecrets/Execute's error path.
const (
	testKubeconfigPEM = "-----BEGIN CERTIFICATE-----\n" +
		"MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAtestcertificatedata\n" +
		"-----END CERTIFICATE-----"
	testNodeToken = "K10a1b2c3d4e5f60718293a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3c4d5::server:f6e5d4c3b2a1908f7e6d5c4b3a2918070605040302010"
)

// TestRedactSecretsRemovesPEMBlockAndNodeToken proves the redactor's actual
// behaviour on the two shapes that matter -- an assertion that a
// no-op redactor (one that merely returns its input) would also pass would
// not be load-bearing.
func TestRedactSecretsRemovesPEMBlockAndNodeToken(t *testing.T) {
	input := "join failed: kubeconfig was:\n" + testKubeconfigPEM + "\ntoken was: " + testNodeToken

	got := redactSecrets(input)

	if strings.Contains(got, "BEGIN CERTIFICATE") || strings.Contains(got, "testcertificatedata") {
		t.Errorf("want the PEM block fully redacted, got %q", got)
	}
	if strings.Contains(got, testNodeToken) {
		t.Errorf("want the node token fully redacted, got %q", got)
	}
	if !strings.Contains(got, redactedPlaceholder) {
		t.Errorf("want the redaction placeholder present so the message still names what was removed, got %q", got)
	}
}

// TestExecuteRedactsSecretsOnErrorPath drives a kubeconfig and a node-token
// through the REAL Execute error path (a failing remote command whose
// stderr echoes them back, the same shape a failed k3s install/join
// produces) and proves neither string survives in Execute's returned
// stdout/stderr -- this is the client choke point every controller error
// message is built from.
func TestExecuteRedactsSecretsOnErrorPath(t *testing.T) {
	host, port := startFakeExecErrorServer(t, testKubeconfigPEM, testNodeToken)
	c := newSlowTestClient(t, host, port)

	_, stderr, err := c.Execute(context.Background(), "curl -sfL https://get.k3s.io | sh -")
	if err == nil {
		t.Fatal("want an error from a command that exits non-zero")
	}
	if strings.Contains(stderr, testKubeconfigPEM) || strings.Contains(stderr, "BEGIN CERTIFICATE") {
		t.Errorf("want the kubeconfig PEM block redacted from stderr, got %q", stderr)
	}
	if strings.Contains(stderr, testNodeToken) {
		t.Errorf("want the node token redacted from stderr, got %q", stderr)
	}
}
