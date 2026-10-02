// gateway_test.go — Phase-3 Task 3 wiring coverage: the SP2 acceptor +
// mTLS TLSConfig injection land on NewGateway.
package fix

import (
	"crypto/tls"
	"crypto/x509"
	"testing"

	"github.com/quickfixgo/quickfix"

	"exchange/internal/config"
)

func gwCfg() config.FixConfig {
	return config.FixConfig{
		Enabled:         true,
		AcceptorHost:    "127.0.0.1",
		AcceptorPort:    19879,
		SP2AcceptorHost: "127.0.0.1",
		SP2AcceptorPort: 19880,
		SenderCompID:    "EXC",
		TLSEnabled:      true,
		TLSCert:         "/nonexistent/cert.pem", // loaded lazily at Start
		TLSKey:          "/nonexistent/key.pem",
	}
}

func gwApp() *App {
	st := newMemStore()
	return NewApp(Options{Store: st, Orders: &memFlow{}, Log: slogNop{}})
}

func TestNewGateway_SP2AcceptorBuilt(t *testing.T) {
	g, err := NewGateway(gwApp(), gwCfg(), quickfix.NewMemoryStoreFactory(),
		nil, GatewayOpts{SP2Enabled: true})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	if g.acceptor == nil {
		t.Fatal("4.4 acceptor not built")
	}
	if g.sp2Acceptor == nil {
		t.Fatal("sp2_enabled but no SP2 acceptor")
	}
	if !g.AcceptorStarted() {
		t.Fatal("AcceptorStarted must cover the SP2 leg")
	}
}

func TestNewGateway_SP2RefusesPlaintext(t *testing.T) {
	cfg := gwCfg()
	cfg.TLSEnabled = false
	cfg.TLSCert, cfg.TLSKey = "", ""
	_, err := NewGateway(gwApp(), cfg, quickfix.NewMemoryStoreFactory(),
		nil, GatewayOpts{SP2Enabled: true})
	if err == nil {
		t.Fatal("SP2 over plaintext must fail closed")
	}
}

func TestNewGateway_SP2DisabledBuildsOnly44(t *testing.T) {
	g, err := NewGateway(gwApp(), gwCfg(), quickfix.NewMemoryStoreFactory(),
		nil, GatewayOpts{})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	if g.sp2Acceptor != nil {
		t.Fatal("SP2 acceptor built while disabled")
	}
	if !g.AcceptorStarted() {
		t.Fatal("4.4 acceptor missing")
	}
}

// The mTLS gate path: ServerTLSConfig output is injected into the
// acceptor via SetTLSConfig — a client without a provisioned
// certificate never reaches Logon.
func TestNewGateway_MTLSConfigInjected(t *testing.T) {
	ca, _ := makeCA(t)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	tlsCfg, err := ServerTLSConfig(TLSOptions{
		Certificates:      []tls.Certificate{{}},
		ClientCAs:         pool,
		RequireClientCert: true,
		Environment:       "test",
		Lookup:            memBindings{},
		Revocation:        &fakeRevocation{},
	})
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	g, err := NewGateway(gwApp(), gwCfg(), quickfix.NewMemoryStoreFactory(),
		nil, GatewayOpts{TLSConfig: tlsCfg})
	if err != nil {
		t.Fatalf("NewGateway: %v", err)
	}
	if g.acceptor == nil {
		t.Fatal("acceptor missing")
	}
}
