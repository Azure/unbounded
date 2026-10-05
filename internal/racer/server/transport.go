// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// transportListener owns sockets from acceptance until close, including sockets
// not yet visible to net/http. Excess sockets are closed in the accept loop,
// without spawning a goroutine or queuing work. The kernel backlog is separate.
type transportListener struct {
	net.Listener
	config      *tls.Config
	timeout     time.Duration
	ctx         context.Context
	cancel      context.CancelFunc
	ready       chan net.Conn
	acceptDone  chan struct{}
	acceptErr   error
	connections chan struct{}
	handshakes  chan struct{}
	mu          sync.Mutex
	closed      bool
	live        map[*transportConn]struct{}
	closeOnce   sync.Once
	closeErr    error
}

func newTransportListener(ctx context.Context, listener net.Listener, config *tls.Config, limits Limits) *transportListener {
	ctx, cancel := context.WithCancel(ctx)

	l := &transportListener{
		Listener: listener, config: config, timeout: limits.WriteTimeout,
		ctx: ctx, cancel: cancel, ready: make(chan net.Conn), acceptDone: make(chan struct{}),
		connections: make(chan struct{}, max(0, limits.MaxConnections)),
		handshakes:  make(chan struct{}, max(0, limits.MaxConcurrentHandshakes)),
		live:        make(map[*transportConn]struct{}),
	}
	go l.run()

	return l
}

func (l *transportListener) run() {
	defer close(l.acceptDone)

	var retryDelay time.Duration

	for {
		if l.ctx.Err() != nil {
			l.acceptErr = net.ErrClosed
			return
		}

		conn, err := l.Listener.Accept()
		if err != nil {
			// Retry here, not in net/http: once this pump exits, Accept can
			// only replay acceptErr and cannot resume accepting sockets.
			var temporary net.Error
			if errors.As(err, &temporary) && temporary.Temporary() { //nolint:staticcheck // Match net/http's accept-error retry contract, including EMFILE.
				if retryDelay == 0 {
					retryDelay = 5 * time.Millisecond
				} else {
					retryDelay = min(2*retryDelay, time.Second)
				}

				timer := time.NewTimer(retryDelay)
				select {
				case <-timer.C:
				case <-l.ctx.Done():
					timer.Stop()

					l.acceptErr = net.ErrClosed

					return
				}

				continue
			}

			l.acceptErr = err

			return
		}

		retryDelay = 0

		if !take(l.connections) {
			closeTransport(conn)
			continue
		}

		c := &transportConn{Conn: conn, owner: l}
		l.mu.Lock()

		closed := l.closed || l.ctx.Err() != nil
		if !closed {
			l.live[c] = struct{}{}
		}
		l.mu.Unlock()

		if closed || !take(l.handshakes) {
			closeTransport(c)
			continue
		}

		go l.handshake(c)
	}
}

func (l *transportListener) handshake(raw *transportConn) {
	// Only this goroutine releases handshake admission, after HandshakeContext
	// actually returns, never from a TLS configuration/verification callback.
	conn := tls.Server(raw, l.config)
	ctx, cancel := context.WithTimeout(l.ctx, l.timeout)

	err := raw.SetDeadline(time.Now().Add(l.timeout))
	if err == nil {
		err = conn.HandshakeContext(ctx)
	}

	cancel()
	release(l.handshakes)

	if err != nil {
		closeTransport(raw)
		return
	}

	if err := raw.SetDeadline(time.Time{}); err != nil {
		closeTransport(raw)
		return
	}
	// Return the concrete *tls.Conn, not a wrapper: net/http requires that type
	// to populate Request.TLS and enforce client-certificate authentication.
	select {
	case l.ready <- conn:
	case <-l.ctx.Done():
		closeTransport(raw)
	}
}

func (l *transportListener) Accept() (net.Conn, error) {
	select {
	case <-l.acceptDone:
		return nil, l.acceptErr
	case conn := <-l.ready:
		return conn, nil
	}
}

func (l *transportListener) Close() error {
	l.closeOnce.Do(func() {
		l.cancel()
		l.mu.Lock()
		l.closed = true

		connections := make([]*transportConn, 0, len(l.live))
		for conn := range l.live {
			connections = append(connections, conn)
		}
		l.mu.Unlock()
		// Raw close bypasses TLS close-notify, including for handshakes and
		// completed handshakes still waiting to be handed to net/http.
		for _, conn := range connections {
			closeTransport(conn)
		}

		l.closeErr = l.Listener.Close()
	})

	return l.closeErr
}

type transportConn struct {
	net.Conn
	owner *transportListener
	once  sync.Once
	err   error
}

func (c *transportConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.live, c)
		c.owner.mu.Unlock()
		release(c.owner.connections)
	})

	return c.err
}

const servingCertificatePollInterval = time.Second

type servingCertificate struct {
	prefixes []servingCertificatePrefix
}

type servingCertificatePrefix struct {
	certificate         tls.Certificate
	notBefore, notAfter time.Time
}

// Published certificates are immutable. Handshakes only load a pointer and check
// its validity window; all filesystem access and parsing happens in the poller.
type servingCertificateReloader struct {
	certificateFile, keyFile string
	current                  atomic.Pointer[servingCertificate]
}

func newServingCertificateReloader(certificateFile, keyFile string) (*servingCertificateReloader, error) {
	r := &servingCertificateReloader{certificateFile: certificateFile, keyFile: keyFile}
	if err := r.reload(); err != nil {
		return nil, err
	}

	return r, nil
}

func (r *servingCertificateReloader) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return r.getCertificateAt(time.Now())
}

func (r *servingCertificateReloader) getCertificateAt(now time.Time) (*tls.Certificate, error) {
	certificate := r.current.Load()
	if certificate == nil {
		return nil, wire.Unavailable
	}

	return certificate.at(now)
}

func (c *servingCertificate) at(now time.Time) (*tls.Certificate, error) {
	// Longest first: retain compatibility until its suffix expires, then use
	// an already validated prefix. No file reads, parsing, or verification here.
	for i := range c.prefixes {
		if prefix := &c.prefixes[i]; !now.Before(prefix.notBefore) && now.Before(prefix.notAfter) {
			return &c.prefixes[i].certificate, nil
		}
	}

	return nil, wire.Unavailable
}

func (r *servingCertificateReloader) run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var nextLog time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}

			if err := r.reload(); err != nil && !time.Now().Before(nextLog) {
				// Do not log paths, PEM contents, or parser errors. Bound repeated
				// failures even if a broken projection remains mounted indefinitely.
				ctrl.LoggerFrom(ctx).Error(wire.Unavailable, "serving TLS certificate reload rejected; retaining last valid certificate")

				nextLog = time.Now().Add(time.Minute)
			}
		}
	}
}

func (r *servingCertificateReloader) reload() error {
	return r.reloadAt(time.Now())
}

func (r *servingCertificateReloader) reloadAt(now time.Time) error {
	certificatePath, certificateGeneration, err := servingFilePath(r.certificateFile)
	if err != nil {
		return wire.Unavailable
	}

	keyPath, keyGeneration, err := servingFilePath(r.keyFile)
	if err != nil || certificateGeneration != keyGeneration {
		return wire.Unavailable
	}

	certificatePEM, err := readServingFile(certificatePath)
	if err != nil {
		return wire.Unavailable
	}

	keyPEM, err := readServingFile(keyPath)
	if err != nil {
		return wire.Unavailable
	}
	// Recheck both names and contents. Projected paths must still name the same
	// immutable generation; standalone files must be stable across both reads.
	for _, file := range []struct {
		name, path, generation string
		contents               []byte
	}{
		{r.certificateFile, certificatePath, certificateGeneration, certificatePEM},
		{r.keyFile, keyPath, keyGeneration, keyPEM},
	} {
		path, generation, err := servingFilePath(file.name)
		if err != nil || path != file.path || generation != file.generation {
			return wire.Unavailable
		}

		contents, err := readServingFile(path)
		if err != nil || !bytes.Equal(contents, file.contents) {
			return wire.Unavailable
		}
	}
	// X509KeyPair tolerates trailing malformed PEM. Do not accidentally accept
	// a truncated chain as a valid leaf-only deployment.
	for remaining := bytes.TrimSpace(certificatePEM); len(remaining) > 0; {
		if !bytes.HasPrefix(remaining, []byte("-----BEGIN CERTIFICATE-----")) {
			return wire.Unavailable
		}

		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return wire.Unavailable
		}

		remaining = bytes.TrimSpace(rest)
	}

	certificate, err := tls.X509KeyPair(certificatePEM, keyPEM)
	if err != nil {
		return wire.Unavailable
	}

	validated, err := validateServingCertificate(certificate, now)
	if err != nil {
		return err
	}

	r.current.Store(validated)

	return nil
}

// Kubernetes AtomicWriter projections have a ..data symlink at the volume root.
// Resolve each file and require both to belong to that same generation, including
// nested projected paths. Never follow a second generation while reading a pair.
func servingFilePath(name string) (string, string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", "", err
	}

	path, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", err
	}

	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		data := filepath.Join(dir, "..data")
		if _, err := os.Lstat(data); err == nil {
			generation, err := filepath.EvalSymlinks(data)
			if err != nil {
				return "", "", err
			}

			relative, err := filepath.Rel(generation, path)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return "", "", wire.Unavailable
			}

			return path, generation, nil
		} else if !os.IsNotExist(err) {
			return "", "", err
		}

		if filepath.Dir(dir) == dir {
			break
		}
	}

	return path, "", nil
}

func readServingFile(path string) (data []byte, result error) {
	// Reject special files before opening, and bound memory for malformed input.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, wire.Unavailable
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}

	defer func() { result = errors.Join(result, f.Close()) }()

	const maxBytes = 1024 * 1024

	contents, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil || len(contents) > maxBytes {
		return nil, wire.Unavailable
	}

	return contents, nil
}

func validateServingCertificate(certificate tls.Certificate, now time.Time) (*servingCertificate, error) {
	if len(certificate.Certificate) == 0 {
		return nil, wire.Unavailable
	}

	chain := make([]*x509.Certificate, len(certificate.Certificate))
	for i, der := range certificate.Certificate {
		cert, err := x509.ParseCertificate(der)
		if err != nil || cert.NotBefore.After(now) || !cert.NotBefore.Before(cert.NotAfter) {
			return nil, wire.Unavailable
		}

		chain[i] = cert
		if i > 0 && chain[i-1].CheckSignatureFrom(cert) != nil {
			return nil, wire.Unavailable
		}
	}

	if !now.Before(chain[0].NotAfter) || chain[0].IsCA {
		return nil, wire.Unavailable
	}
	// Check the ENTIRE supplied chain's constraints before allowing any trim.
	// Expired compatibility certificates need not overlap a newly issued leaf,
	// so verify structure using copies with neutral validity windows. The actual
	// windows above and in each cached prefix are enforced separately. Raw DER,
	// signatures, EKU, name constraints, and path length constraints are unchanged.
	structural := make([]*x509.Certificate, len(chain))
	for i, cert := range chain {
		copy := *cert
		copy.NotBefore, copy.NotAfter = now.Add(-time.Hour), now.Add(time.Hour)
		structural[i] = &copy
	}

	if err := verifyServingPrefix(structural, now); err != nil {
		return nil, wire.Unavailable
	}

	certificate.Leaf = chain[0]
	validated := &servingCertificate{}

	for n := len(chain); n > 0; n-- {
		// Only a cross-signed CA starts an optional compatibility suffix. Do
		// not reinterpret an expired ordinary self-signed issuer as optional.
		if n < len(chain) && (!chain[n].IsCA || chain[n].CheckSignatureFrom(chain[n]) == nil) {
			continue
		}

		if err := verifyServingPrefix(structural[:n], now); err != nil {
			return nil, wire.Unavailable
		}

		prefix := servingCertificatePrefix{certificate: certificate, notBefore: chain[0].NotBefore, notAfter: chain[0].NotAfter}

		prefix.certificate.Certificate = certificate.Certificate[:n:n]
		for _, cert := range chain[:n] {
			if cert.NotBefore.After(prefix.notBefore) {
				prefix.notBefore = cert.NotBefore
			}

			if cert.NotAfter.Before(prefix.notAfter) {
				prefix.notAfter = cert.NotAfter
			}
		}

		if n < len(chain) {
			// A shorter prefix is eligible only after its compatibility suffix
			// expires, never to work around a not-yet-valid supplied certificate.
			expires := chain[n].NotAfter
			for _, cert := range chain[n:] {
				if cert.NotAfter.Before(expires) {
					expires = cert.NotAfter
				}

				if cert.NotBefore.After(prefix.notBefore) {
					prefix.notBefore = cert.NotBefore
				}
			}

			if expires.After(prefix.notBefore) {
				prefix.notBefore = expires
			}
		}

		validated.prefixes = append(validated.prefixes, prefix)
	}

	if _, err := validated.at(now); err != nil {
		return nil, err
	}

	return validated, nil
}

// Serving trust is deployment-provided, distinct from peer trust. The last
// certificate in a prefix is the local validation anchor, not a client trust
// decision. Clients must still build a path to their own trusted current CA.
func verifyServingPrefix(chain []*x509.Certificate, now time.Time) error {
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(chain[len(chain)-1])

	for _, cert := range chain[1:] {
		intermediates.AddCert(cert)
	}

	if _, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return wire.Unavailable
	}

	return nil
}
