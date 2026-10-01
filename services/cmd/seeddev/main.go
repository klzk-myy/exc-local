// seeddev — dev/e2e fixture funder for seeded accounts.
//
// Funds accounts through the REAL ledger path — one DEPOSIT journal per
// (account, currency) posted via settlement.LedgerService.Post — so the
// wallet rows satisfy the §5.3 invariant the reconciliation engine
// audits (ledger_entries net == balances.total == journal_sums.net).
// Direct balances INSERTs bypass journal_entries/ledger_lines/ledger_
// entries and trip RECONCILIATION_MISMATCH auto-halts; this tool exists
// so fixture funding never takes that shortcut.
//
// With -reset it first clears the account's wallet legs (balances,
// journal_sums, ledger_entries), clears ACTIVE ACCOUNT-scope trading
// suspensions + halt:account:* Redis flags, and sets kyc_tier — making
// browser-e2e stacks reproducible after recon halts or partial seeds.
//
// Env: EXC_POSTGRES_DSN (required) · EXC_REDIS_ADDR / EXC_REDIS_DB
// (required — §5.3 account locks) · EXC_NATS_URLS (optional; without it
// BalanceChanged dispatch reports committed-with-dispatch-error and the
// post still counts as funded).
// Exit: 0 pass · 1 failure · 2 setup error.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"

	"exchange/internal/ledger"
	excnats "exchange/internal/nats"
	excredis "exchange/internal/redis"
	"exchange/internal/settlement"
)

func main() {
	emails := flag.String("emails", "e2e.taker@example.com,e2e.maker@example.com",
		"comma-separated user emails whose primary accounts get funded")
	amount := flag.String("amount", "1000000", "per-currency deposit amount")
	currencies := flag.String("currencies", "AUD,CAD,CHF,EUR,GBP,JPY,MXN,NZD,USD",
		"comma-separated wallet currencies to fund")
	tier := flag.String("kyc-tier", "T1", "kyc tier to set (orders reject T0)")
	intent := flag.String("settlement-intent", "PHYSICAL_DELIVERY",
		"account settlement_intent (ROLLING_MARGIN requires the engine 2PC "+
			"reservation channel, which is Aeron-only — shm-mode fixtures use "+
			"PHYSICAL_DELIVERY so fills segregate available→locked)")
	reset := flag.Bool("reset", false,
		"wipe the accounts' wallet legs + clear ACCOUNT suspensions before funding")
	allowlistIPs := flag.String("allowlist-ips", "",
		"comma-separated IPs to exempt from the §8.8 ban machinery "+
			"(dev stack: the host driving browser/API traffic — clears "+
			"ip_ban:/rl429:/strikes then SADDs ip_allowlist)")
	flag.Parse()

	if err := run(*emails, *amount, *currencies, *tier, *intent, *reset,
		*allowlistIPs); err != nil {
		fmt.Fprintln(os.Stderr, "seeddev:", err)
		os.Exit(2)
	}
}

func run(emails, amount, currencies, tier, intent string, reset bool,
	allowlistIPs string) error {
	dsn := os.Getenv("EXC_POSTGRES_DSN")
	if dsn == "" {
		return fmt.Errorf("EXC_POSTGRES_DSN required")
	}
	amt, err := decimal.NewFromString(amount)
	if err != nil || !amt.IsPositive() {
		return fmt.Errorf("amount %q must parse positive", amount)
	}
	ccys := strings.Split(currencies, ",")
	for i := range ccys {
		ccys[i] = strings.ToUpper(strings.TrimSpace(ccys[i]))
		if len(ccys[i]) != 3 {
			return fmt.Errorf("currency %q must be ISO-4217 len 3", ccys[i])
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return fmt.Errorf("pg connect: %w", err)
	}
	defer pool.Close()

	rdbAddr := os.Getenv("EXC_REDIS_ADDR")
	if rdbAddr == "" {
		return fmt.Errorf("EXC_REDIS_ADDR required (account locks)")
	}
	rdbDB := 0
	if v := os.Getenv("EXC_REDIS_DB"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &rdbDB); err != nil {
			return fmt.Errorf("EXC_REDIS_DB %q: %w", v, err)
		}
	}
	rdb := excredis.New(rdbAddr, os.Getenv("EXC_REDIS_PASSWORD"), rdbDB)
	defer rdb.Close()

	// BalanceChanged dispatch is a MUST under Post — wire NATS when
	// available; a missing stream surfaces committed-with-dispatch-error
	// which we tolerate for seeding (funds are final).
	var pub settlement.Publisher
	if urls := os.Getenv("EXC_NATS_URLS"); urls != "" {
		nc, nerr := excnats.Connect(ctx, excnats.DefaultConfig(strings.Split(urls, ",")), nil)
		if nerr == nil {
			pub = settlement.NatsPublisher{JS: nc.JetStream()}
			defer nc.Close()
		}
	}
	lsvc, err := settlement.NewLedgerService(pool, rdb, pub)
	if err != nil {
		return err
	}

	for _, ip := range strings.Split(allowlistIPs, ",") {
		ip = strings.TrimSpace(ip)
		if ip == "" {
			continue
		}
		// Clear any accrued ban state, then exempt — the §8.8 allowlist
		// skips the ban machinery entirely (rate-limit Lua step 1).
		if err := rdb.Del(ctx, "ip_ban:"+ip, "rl429:"+ip,
			"ip_ban_strikes:"+ip).Err(); err != nil {
			return fmt.Errorf("unban %s: %w", ip, err)
		}
		if err := rdb.SAdd(ctx, "ip_allowlist", ip).Err(); err != nil {
			return fmt.Errorf("allowlist %s: %w", ip, err)
		}
		fmt.Printf("ip %s allowlisted (ban state cleared)\n", ip)
	}

	for _, email := range strings.Split(emails, ",") {
		email = strings.TrimSpace(email)
		if email == "" {
			continue
		}
		var accountID int64
		if err := pool.QueryRow(ctx,
			`SELECT a.id FROM accounts a JOIN users u ON u.id = a.user_id
			  WHERE u.email = $1 ORDER BY a.id LIMIT 1`, email).Scan(&accountID); err != nil {
			return fmt.Errorf("account lookup %q: %w (register via API first)", email, err)
		}
		if reset {
			if err := resetAccount(ctx, pool, rdb, accountID, tier, intent); err != nil {
				return fmt.Errorf("reset account %d: %w", accountID, err)
			}
		} else {
			if _, err := pool.Exec(ctx,
				`UPDATE accounts SET kyc_tier=$2::kyc_tier_enum,
				        settlement_intent=$3::settlement_intent_enum
				  WHERE id=$1`, accountID, tier, intent); err != nil {
				return fmt.Errorf("kyc promote account %d: %w", accountID, err)
			}
		}
		for _, ccy := range ccys {
			res, err := lsvc.Post(ctx, ledger.Journal{
				EntryType:      ledger.EntryDeposit,
				Description:    fmt.Sprintf("seeddev fixture funding %s %s", amt, ccy),
				PostedBy:       "seeddev",
				IdempotencyKey: fmt.Sprintf("seeddev:%d:%s:%s", accountID, ccy, amt),
				Lines: []ledger.Line{
					ledger.DebitLine(ledger.Nostro(ccy), ccy, amt, "fixture deposit received"),
					ledger.CreditLine(ledger.CustomerLiability(ccy), ccy, amt, "fixture client liability"),
				},
				Effects: []ledger.AccountEffect{{
					AccountID: accountID, Currency: ccy, AvailableDelta: amt,
				}},
			})
			switch {
			case err != nil && res.Committed:
				// Committed but BalanceChanged dispatch failed — money
				// final; acceptable for fixtures.
				fmt.Printf("account=%d %s +%s (committed; dispatch: %v)\n", accountID, ccy, amt, err)
			case err != nil:
				return fmt.Errorf("deposit %s → account %d: %w", ccy, accountID, err)
			default:
				fmt.Printf("account=%d %s +%s ok\n", accountID, ccy, amt)
			}
		}
	}
	return nil
}

// resetAccount removes every wallet leg for the account so the fresh
// DEPOSIT journals land on a clean slate, then clears the reconciliation
// auto-halt rows and halt flags that stale mismatches may have emitted.
func resetAccount(ctx context.Context, pool *pgxpool.Pool, rdb *excredis.Client,
	accountID int64, tier, intent string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, s := range []struct {
		q    string
		args []any
	}{
		// journal_sums.last_entry_id FK → ledger_entries: sums first.
		{`DELETE FROM journal_sums WHERE account_id = $1`, []any{accountID}},
		{`DELETE FROM ledger_entries WHERE account_id = $1`, []any{accountID}},
		{`DELETE FROM balances WHERE account_id = $1`, []any{accountID}},
		// Prior seeddev journals + their GL lines — otherwise the
		// idempotency keys replay-commit WITHOUT rewriting the wallet
		// legs we just deleted (journals would exist with no balances).
		{`DELETE FROM ledger_lines WHERE journal_entry_id IN
		    (SELECT id FROM journal_entries
		      WHERE posted_by='seeddev' AND idempotency_key LIKE $1)`,
			[]any{fmt.Sprintf("seeddev:%d:%%", accountID)}},
		{`DELETE FROM journal_entries
		  WHERE posted_by='seeddev' AND idempotency_key LIKE $1`,
			[]any{fmt.Sprintf("seeddev:%d:%%", accountID)}},
		{`DELETE FROM trading_suspensions
		  WHERE scope='ACCOUNT' AND target_id=$1 AND state='ACTIVE'`,
			[]any{fmt.Sprintf("%d", accountID)}},
		{`UPDATE accounts SET kyc_tier=$2::kyc_tier_enum,
		        settlement_intent=$3::settlement_intent_enum WHERE id=$1`,
			[]any{accountID, tier, intent}},
	} {
		if _, err := tx.Exec(ctx, s.q, s.args...); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	// halt:{scope}:{target} — the kill-switch flag plane (admin).
	return rdb.Del(ctx, fmt.Sprintf("halt:account:%d", accountID)).Err()
}
