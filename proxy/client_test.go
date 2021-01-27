package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"path/filepath"
	"testing"

	qt "github.com/frankban/quicktest"
	"go.uber.org/zap/zaptest"
	"golang.org/x/net/nettest"
)

const (
	textFixtures = "./testcerts"
)

func TestClient_Run_Cancellation(t *testing.T) {
	c := qt.New(t)
	client, err := NewClient(testOptions(t))
	c.Assert(err, qt.IsNil)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan bool)
	go func() {
		err := client.Run(ctx)
		c.Assert(err, qt.IsNil)
		close(done)

	}()

	cancel()
	<-done
}

func TestClient_clientCerts(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	clientCert := tls.Certificate{}
	caCert := &x509.Certificate{
		RawSubject: []byte("a-subject"),
	}

	org, db, branch := "myorg", "mydb", "mybranch"
	instance := fmt.Sprintf("%s/%s/%s", org, db, branch)

	certSource := &fakeCertSource{
		CertFn: func(ctx context.Context, o, d, b string) (*Cert, error) {
			c.Check(o, qt.Equals, org)
			c.Check(d, qt.Equals, db)
			c.Check(b, qt.Equals, branch)
			return &Cert{
				ClientCert: clientCert,
				CACert:     caCert,
			}, nil
		},
	}

	testOpts := testOptions(t)
	testOpts.CertSource = certSource
	client, err := NewClient(testOpts)
	c.Assert(err, qt.IsNil)

	cert, err := client.clientCerts(ctx, instance)
	c.Assert(err, qt.IsNil)
	c.Assert(certSource.CertFnInvoked, qt.IsTrue)
	c.Assert(cert.Certificates, qt.HasLen, 1)
	c.Assert(cert.Certificates[0], qt.DeepEquals, clientCert)

	c.Assert(cert.RootCAs.Subjects(), qt.HasLen, 1)
	c.Assert(cert.RootCAs.Subjects()[0], qt.DeepEquals, caCert.RawSubject)
}

func TestClient_clientCerts_has_cache(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	org, db, branch := "myorg", "mydb", "mybranch"
	instance := fmt.Sprintf("%s/%s/%s", org, db, branch)

	certSource := &fakeCertSource{
		CertFn: func(ctx context.Context, o, d, b string) (*Cert, error) {
			return &Cert{}, nil
		},
	}

	testOpts := testOptions(t)
	client, err := NewClient(testOpts)
	c.Assert(err, qt.IsNil)

	cfg := &tls.Config{ServerName: "server"}
	client.configCache.Add(instance, cfg)

	cert, err := client.clientCerts(ctx, instance)
	c.Assert(err, qt.IsNil)
	c.Assert(certSource.CertFnInvoked, qt.IsFalse)
	c.Assert(cert.ServerName, qt.Equals, cfg.ServerName)
}

func TestClient_run(t *testing.T) {
	c := qt.New(t)
	ctx := context.Background()

	org, db, branch := "myorg", "mydb", "mybranch"
	instance := fmt.Sprintf("%s/%s/%s", org, db, branch)

	certs := testCerts(c)

	certSource := &fakeCertSource{
		CertFn: func(ctx context.Context, o, d, b string) (*Cert, error) {
			return &Cert{
				ClientCert: certs.clientCert,
				CACert:     certs.caCert,
			}, nil
		},
	}

	localListener, err := nettest.NewLocalListener("tcp")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { localListener.Close() })

	remoteListener, err := nettest.NewLocalListener("tcp")
	c.Assert(err, qt.IsNil)
	c.Cleanup(func() { remoteListener.Close() })

	testOpts := testOptions(t)
	testOpts.Instance = instance
	testOpts.RemoteAddr = remoteListener.Addr().String()
	testOpts.CertSource = certSource
	client, err := NewClient(testOpts)
	c.Assert(err, qt.IsNil)

	// run the client proxy
	done := make(chan bool)
	go func() {
		err := client.run(ctx, localListener)
		c.Assert(err, qt.IsNil)
	}()

	msg := "Don't Try To Understand It. Feel It."

	// run the remote, server proxy
	go func() {
		conn, err := remoteListener.Accept()
		c.Assert(err, qt.IsNil)
		tlsConn := tls.Server(conn, certs.serverCfg)

		// read from the proxy
		buf := make([]byte, len(msg))
		_, err = tlsConn.Read(buf[:])
		c.Assert(err, qt.IsNil)

		// we should read the same message the client sent us
		c.Assert(string(buf), qt.Equals, msg)

		// bail out
		close(done)
	}()

	// open a TCP connection to our proxy
	conn, err := net.Dial(localListener.Addr().Network(), localListener.Addr().String())
	c.Assert(err, qt.IsNil)

	// and send the message
	_, err = conn.Write([]byte(msg))
	c.Assert(err, qt.IsNil)

	// wait until we have read the message on the remote listener
	<-done
}

type testCert struct {
	serverCfg  *tls.Config
	clientCert tls.Certificate
	caCert     *x509.Certificate
}

func testCerts(t testing.TB) testCert {
	c := qt.New(t)
	c.Helper()

	caBuf, err := ioutil.ReadFile(filepath.Join(textFixtures, "ca.crt"))
	c.Assert(err, qt.IsNil)

	caCert, err := parseCert(caBuf)
	c.Assert(err, qt.IsNil)

	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caBuf)

	serverCerts, err := tls.LoadX509KeyPair(
		filepath.Join(textFixtures, "server.crt"),
		filepath.Join(textFixtures, "server.key"),
	)
	c.Assert(err, qt.IsNil)

	clientCerts, err := tls.LoadX509KeyPair(
		filepath.Join(textFixtures, "client.crt"),
		filepath.Join(textFixtures, "client.key"),
	)
	c.Assert(err, qt.IsNil)

	serverCfg := &tls.Config{
		PreferServerCipherSuites: true,
		MinVersion:               tls.VersionTLS12,
		ClientCAs:                caPool,
		Certificates:             []tls.Certificate{serverCerts},
	}

	return testCert{
		serverCfg:  serverCfg,
		clientCert: clientCerts,
		caCert:     caCert,
	}
}

func testOptions(t *testing.T) Options {
	return Options{
		Logger: zaptest.NewLogger(t),
	}
}

type fakeCertSource struct {
	CertFn        func(ctx context.Context, org, db, branch string) (*Cert, error)
	CertFnInvoked bool
}

func (f *fakeCertSource) Cert(ctx context.Context, org, db, branch string) (*Cert, error) {
	f.CertFnInvoked = true

	return f.CertFn(ctx, org, db, branch)

}

func parseCert(pemCert []byte) (*x509.Certificate, error) {
	bl, _ := pem.Decode(pemCert)
	if bl == nil {
		return nil, errors.New("invalid PEM: " + string(pemCert))
	}
	return x509.ParseCertificate(bl.Bytes)
}
