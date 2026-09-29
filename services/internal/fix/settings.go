// QuickFIX session settings construction from the venue config (spec
// §9.3): FIX.4.4, HeartBtInt=30, LogonTimeout=10s, LogoutTimeout=5s,
// daily ResetSeqTime at the FX open boundary (22:00 UTC — NY close /
// trading-day rollover), TLS 1.3, dynamic acceptor sessions.
package fix

import (
	"fmt"
	"strconv"

	"github.com/quickfixgo/quickfix"
	qfconfig "github.com/quickfixgo/quickfix/config"

	vencfg "exchange/internal/config"
)

// AcceptorSettings builds the [DEFAULT]-only settings doc for the
// inbound order-entry/drop-copy port. Sessions are DynamicSessions —
// provisioned by fix_sessions rows and authenticated at Logon.
func AcceptorSettings(cfg vencfg.FixConfig) (*quickfix.Settings, error) {
	s := quickfix.NewSettings()
	d := s.GlobalSettings()
	d.Set(qfconfig.BeginString, "FIX.4.4")
	d.Set(qfconfig.SenderCompID, cfg.SenderCompID)
	d.Set(qfconfig.DynamicSessions, "Y")
	d.Set(qfconfig.HeartBtInt, "30")
	d.Set(qfconfig.LogonTimeout, "10")
	d.Set(qfconfig.LogoutTimeout, "5")
	d.Set(qfconfig.StartTime, "00:00:00")
	d.Set(qfconfig.EndTime, "00:00:00") // 24/7 accept loop; FX market state is venue-side
	d.Set(qfconfig.ResetSeqTime, resetSeqTimeOf(cfg.ResetTimeUTC))
	d.Set(qfconfig.PersistMessages, "N") // PG store owns replay — never file double-persist
	d.Set(qfconfig.SocketAcceptHost, cfg.AcceptorHost)
	d.Set(qfconfig.SocketAcceptPort, strconv.Itoa(cfg.AcceptorPort))
	if cfg.TLSEnabled {
		if err := applyTLS13(d, cfg); err != nil {
			return nil, err
		}
	}
	if cfg.MaxLatencyMS > 0 {
		d.Set(qfconfig.MaxLatency, strconv.Itoa(cfg.MaxLatencyMS))
	}
	return s, nil
}

// AcceptorSettingsSP2 builds the FIXT.1.1/FIX.5.0SP2 acceptor settings
// (Task 18.3.5): BeginString=FIXT.1.1 with DefaultApplVerID=9, dynamic
// sessions on their own port, same heartbeat/reset/TLS profile as the
// 4.4 acceptor. TLS 1.3 is mandatory for SP2 — TLSEnabled must be set
// (the task AC requires TLS 1.3 on the 5.0 SP2 surface; this helper
// refuses to configure a plaintext SP2 listener).
func AcceptorSettingsSP2(cfg vencfg.FixConfig) (*quickfix.Settings, error) {
	if !cfg.TLSEnabled {
		return nil, fmt.Errorf("fix: FIX 5.0 SP2 acceptor requires TLS 1.3")
	}
	s, err := AcceptorSettings(cfg)
	if err != nil {
		return nil, err
	}
	d := s.GlobalSettings()
	d.Set(qfconfig.BeginString, BeginStringFIXT11)
	d.Set(qfconfig.DefaultApplVerID, ApplVerIDFIX50SP2)
	return s, nil
}

// InitiatorSettingsSP2 builds outward FIXT.1.1/FIX.5.0SP2 initiator
// settings — the initiator leg of Task 18.3.5's "sessions established
// (initiator + acceptor)" AC. Per-session DefaultApplVerID=9; outward
// SP2 venues are provisioned via cfg.OutwardSessions.
func InitiatorSettingsSP2(cfg vencfg.FixConfig) (*quickfix.Settings, error) {
	s, err := InitiatorSettings(cfg)
	if err != nil {
		return nil, err
	}
	d := s.GlobalSettings()
	d.Set(qfconfig.BeginString, BeginStringFIXT11)
	d.Set(qfconfig.DefaultApplVerID, ApplVerIDFIX50SP2)
	return s, nil
}

// InitiatorSettings builds settings for outward FIX sessions —
// venue→venue bridging and the drop-copy/affirmation relays (Tasks
// 18.3.4/18.3.6). One [SESSION] stanza per cfg.OutwardSessions entry;
// heartbeat/timeouts carry the same spec defaults.
func InitiatorSettings(cfg vencfg.FixConfig) (*quickfix.Settings, error) {
	s := quickfix.NewSettings()
	d := s.GlobalSettings()
	d.Set(qfconfig.BeginString, "FIX.4.4")
	d.Set(qfconfig.SenderCompID, cfg.SenderCompID)
	d.Set(qfconfig.HeartBtInt, "30")
	d.Set(qfconfig.LogonTimeout, "10")
	d.Set(qfconfig.LogoutTimeout, "5")
	d.Set(qfconfig.StartTime, "00:00:00")
	d.Set(qfconfig.EndTime, "00:00:00")
	d.Set(qfconfig.ResetSeqTime, resetSeqTimeOf(cfg.ResetTimeUTC))
	d.Set(qfconfig.PersistMessages, "N")
	d.Set(qfconfig.ResetOnLogon, boolYN(cfg.ResetOnLogon))
	if cfg.TLSEnabled {
		if err := applyTLS13(d, cfg); err != nil {
			return nil, err
		}
	}
	for _, out := range cfg.OutwardSessions {
		if out.TargetCompID == "" || out.ConnectHost == "" || out.ConnectPort == 0 {
			return nil, fmt.Errorf("fix: outward session %q requires "+
				"target_comp_id/connect_host/connect_port", out.TargetCompID)
		}
		ss := quickfix.NewSessionSettings()
		ss.Set(qfconfig.TargetCompID, out.TargetCompID)
		ss.Set(qfconfig.SocketConnectHost, out.ConnectHost)
		ss.Set(qfconfig.SocketConnectPort, strconv.Itoa(out.ConnectPort))
		ss.Set(qfconfig.ReconnectInterval, "30")
		if _, err := s.AddSession(ss); err != nil {
			return nil, fmt.Errorf("fix: outward session %s: %w",
				out.TargetCompID, err)
		}
	}
	return s, nil
}

// resetSeqTimeOf defaults the daily reset to 22:00 UTC — the FX
// trading day boundary (NY close), matching spec §9.3's daily reset.
func resetSeqTimeOf(v string) string {
	if v == "" {
		return "22:00:00"
	}
	return v
}

func boolYN(b bool) string {
	if b {
		return "Y"
	}
	return "N"
}

// applyTLS13 pins TLS1.3 + the venue cert/key/CA (spec §9.3 TLS 1.3
// only; client-cert verification is the Task 18.3.11 mTLS layer).
func applyTLS13(d *quickfix.SessionSettings, cfg vencfg.FixConfig) error {
	if cfg.TLSCert == "" || cfg.TLSKey == "" {
		return fmt.Errorf("fix: tls_enabled requires fix.tls_cert + fix.tls_key")
	}
	d.Set(qfconfig.SocketUseSSL, "Y")
	d.Set(qfconfig.SocketMinimumTLSVersion, "TLS13")
	d.Set(qfconfig.SocketInsecureSkipVerify, "N")
	d.Set(qfconfig.SocketCertificateFile, cfg.TLSCert)
	d.Set(qfconfig.SocketPrivateKeyFile, cfg.TLSKey)
	if cfg.TLSCA != "" {
		d.Set(qfconfig.SocketCAFile, cfg.TLSCA)
	}
	return nil
}
