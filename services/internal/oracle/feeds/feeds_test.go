package feeds

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"exchange/pkg/decimal"
)

func TestSimFeedPollRoundTrip(t *testing.T) {
	f := NewSimFeed("sim")
	f.Set("EUR/USD", decimal.NewFromFloat(1.085))
	got, err := f.Poll(context.Background(), []string{"EUR/USD", "GBP/USD"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if len(got) != 1 || !got[0].Mid.Equal(decimal.NewFromFloat(1.085)) {
		t.Fatalf("got %+v, want one EUR/USD quote at 1.085", got)
	}
	if got[0].Feed != "sim" {
		t.Fatalf("feed attribution = %q, want sim", got[0].Feed)
	}
}

func TestSimFeedClearAndStaleAge(t *testing.T) {
	f := NewSimFeed("sim")
	f.Set("EUR/USD", decimal.One)
	f.Clear("EUR/USD")
	if got, _ := f.Poll(context.Background(), []string{"EUR/USD"}); len(got) != 0 {
		t.Fatalf("cleared symbol still quoted: %+v", got)
	}

	f.Set("EUR/USD", decimal.One)
	f.Age = 10 * time.Second
	got, _ := f.Poll(context.Background(), []string{"EUR/USD"})
	if age := time.Since(got[0].Ts); age < 9*time.Second {
		t.Fatalf("Age override not applied — quote age %v", age)
	}
}

func TestSimFeedFailureBudget(t *testing.T) {
	f := NewSimFeed("sim")
	f.Set("EUR/USD", decimal.One)
	f.Fails = 2
	for i := 0; i < 2; i++ {
		if _, err := f.Poll(context.Background(), []string{"EUR/USD"}); err == nil {
			t.Fatalf("poll %d: want errFeedDown", i)
		}
	}
	if _, err := f.Poll(context.Background(), []string{"EUR/USD"}); err != nil {
		t.Fatalf("poll after budget exhausted: %v", err)
	}
}

// --- HTTP adapters over httptest ---------------------------------------------

func TestRefinitivPoll(t *testing.T) {
	var apiKeySeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKeySeen = r.Header.Get("X-API-Key")
		if r.URL.Query().Get("rics") != "EUR=,GBP=" {
			t.Errorf("rics param = %q", r.URL.Query().Get("rics"))
		}
		w.Write([]byte(`{"quotes":[
			{"ric":"EUR=","mid":"1.0847","volume":"3","ts_ms":1700000000000},
			{"ric":"BAD=","mid":"not-a-number","ts_ms":1700000000000}
		]}`))
	}))
	defer srv.Close()

	f := &Refinitiv{URL: srv.URL, APIKey: "k"}
	got, err := f.Poll(context.Background(), []string{"EUR=", "GBP="})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if apiKeySeen != "k" {
		t.Fatalf("X-API-Key = %q", apiKeySeen)
	}
	if len(got) != 1 || got[0].Symbol != "EUR=" {
		t.Fatalf("quotes %+v — want the parseable EUR= row only", got)
	}
	if !got[0].Mid.Equal(decimal.RequireFromString("1.0847")) {
		t.Fatalf("mid %s", got[0].Mid)
	}
}

func TestRefinitivErrorPaths(t *testing.T) {
	// Unconfigured endpoint.
	if _, err := (&Refinitiv{}).Poll(context.Background(), []string{"EUR="}); err == nil {
		t.Fatal("empty URL should error")
	}
	// Non-200.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	if _, err := (&Refinitiv{URL: srv.URL}).Poll(context.Background(), []string{"EUR="}); err == nil {
		t.Fatal("502 should error")
	}
}

func TestBFIXPollSymbolMapping(t *testing.T) {
	var authSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authSeen = r.Header.Get("Authorization")
		w.Write([]byte(`{"rates":[
			{"pair":"EURUSD","rate":"1.0900","ts_ms":1700000000000},
			{"pair":"USDJPY","rate":"149.50","ts_ms":1700000000000},
			{"pair":"BOGUS","rate":"x","ts_ms":1700000000000}
		]}`))
	}))
	defer srv.Close()

	f := &BFIX{URL: srv.URL, Token: "tok"}
	got, err := f.Poll(context.Background(), []string{"EUR/USD", "USD/JPY", "GBP/USD"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if authSeen != "Bearer tok" {
		t.Fatalf("Authorization = %q", authSeen)
	}
	if len(got) != 2 {
		t.Fatalf("got %d quotes, want 2 (EURUSD + USDJPY mapped)", len(got))
	}
	if got[0].Symbol != "EUR/USD" {
		t.Fatalf("BFIX pair mapping: %q, want EUR/USD", got[0].Symbol)
	}
}

func TestECBPollEURBaseMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`<gesmes:Envelope xmlns:gesmes="x"><Cube>
			<Cube time="2024-01-15">
				<Cube currency="USD" rate="1.0850"/>
				<Cube currency="JPY" rate="161.2"/>
				<Cube currency="ZZZ" rate="bad"/>
			</Cube>
		</Cube></gesmes:Envelope>`))
	}))
	defer srv.Close()

	f := &ECB{URL: srv.URL}
	got, err := f.Poll(context.Background(), []string{"EUR/USD", "EUR/JPY", "USD/JPY"})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	// USD/JPY is a non-EUR base — ECB publishes EUR-based rows only.
	if len(got) != 2 {
		t.Fatalf("got %d quotes %+v, want EUR/USD + EUR/JPY", len(got), got)
	}
	if !got[0].Mid.Equal(decimal.RequireFromString("1.0850")) {
		t.Fatalf("EUR/USD mid %s", got[0].Mid)
	}
	// Ts is the publication date at 14:00 CET.
	if got[0].Ts.Hour() != 14 {
		t.Fatalf("quote ts %v — want 14:00 publication stamp", got[0].Ts)
	}
}
