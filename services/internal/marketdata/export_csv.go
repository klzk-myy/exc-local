// Phase-23 Task 23.3.2 — export renderers.
//
// Wire contracts (pinned by the task conventions):
//
//   - CSV trades/ticks header: trade_id,time,symbol,side,price,quantity
//     (deterministic; klines carry their OHLCV column set instead).
//   - JSON: the sync path emits the §8.8-style {"data":[...]} envelope;
//     the async artifact is newline-delimited JSON (JSONL), one document
//     per row — streaming-friendly at 1M rows.
//   - Parquet trades schema:
//     trade_id uint64, ts timestamp(ms,utc), symbol string,
//     side string, price decimal(18,8), qty decimal(18,8)
//     (klines use the analogous OHLCV column set; snappy-compressed,
//     50k-row groups so the writer never buffers a full export).
//
// Money renders as decimal strings on text formats (spec §5.3); parquet
// stores the int64-scaled form under DECIMAL(18,8) — no floats anywhere.
package marketdata

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/parquet-go/parquet-go/compress/snappy"

	"exchange/pkg/decimal"
)

// tradeCSVHeader is the pinned deterministic tape header.
var tradeCSVHeader = []string{
	"trade_id", "time", "symbol", "side", "price", "quantity",
}

// klineCSVHeader is the OHLCV counterpart.
var klineCSVHeader = []string{
	"open_time", "symbol", "interval", "open", "high", "low", "close",
	"volume", "quote_volume", "trade_count",
}

// rowEncoder renders one Row at a time and flushes a trailer.
type rowEncoder interface {
	Write(Row) error
	Flush() error
}

// newRowEncoder resolves the codec for (format, kind); async selects the
// file shapes (JSONL) over the sync envelope ({data:[...]}).
func newRowEncoder(format ExportFormat, kind ExportKind, async bool,
	w io.Writer) (rowEncoder, error) {
	switch format {
	case FormatCSV:
		return newCSVEncoder(kind, w)
	case FormatJSON:
		if async {
			return newJSONLEncoder(kind, w), nil
		}
		return newJSONEnvelopeEncoder(kind, w), nil
	case FormatParquet:
		return newParquetEncoder(kind, w)
	}
	return nil, fmt.Errorf("%w: %q", ErrExportFormatInvalid, format)
}

// ---------------------------------------------------------------------------
// Row → wire doc
// ---------------------------------------------------------------------------

// tradeDoc is the JSON/JSONL tape projection — decimal strings, RFC3339
// plus epoch-millis time (mirrors historyTickDoc).
type tradeDoc struct {
	TradeID  uint64 `json:"trade_id"`
	Time     string `json:"time"`
	TimeMs   int64  `json:"time_ms"`
	Symbol   string `json:"symbol"`
	Side     string `json:"side"`
	Price    string `json:"price"`
	Quantity string `json:"quantity"`
}

// klineDoc is the candle projection.
type klineDoc struct {
	OpenTime    string `json:"open_time"`
	OpenTimeMs  int64  `json:"open_time_ms"`
	Symbol      string `json:"symbol"`
	Interval    string `json:"interval"`
	Open        string `json:"open"`
	High        string `json:"high"`
	Low         string `json:"low"`
	Close       string `json:"close"`
	Volume      string `json:"volume"`
	QuoteVolume string `json:"quote_volume"`
	TradeCount  int64  `json:"trade_count"`
}

func rowDoc(r Row) any {
	// Kline rows always carry their interval label; tape rows never set it.
	if r.Interval != "" {
		return klineDoc{
			OpenTime:   r.Ts.Format(time.RFC3339),
			OpenTimeMs: r.Ts.UnixMilli(),
			Symbol:     r.Symbol, Interval: r.Interval,
			Open:        r.Open.StringFixed(8),
			High:        r.High.StringFixed(8),
			Low:         r.Low.StringFixed(8),
			Close:       r.Close.StringFixed(8),
			Volume:      r.Volume.StringFixed(8),
			QuoteVolume: r.QuoteVolume.StringFixed(8),
			TradeCount:  r.TradeCount,
		}
	}
	return tradeDoc{
		TradeID: r.TradeID,
		Time:    r.Ts.Format(time.RFC3339),
		TimeMs:  r.Ts.UnixMilli(),
		Symbol:  r.Symbol, Side: r.Side,
		Price:    r.Price.StringFixed(8),
		Quantity: r.Quantity.StringFixed(8),
	}
}

// ---------------------------------------------------------------------------
// CSV
// ---------------------------------------------------------------------------

type csvEncoder struct {
	w *csv.Writer
}

func newCSVEncoder(kind ExportKind, w io.Writer) (rowEncoder, error) {
	header := tradeCSVHeader
	if kind == KindKlines {
		header = klineCSVHeader
	} else if kind != KindTrades && kind != KindTicks {
		return nil, fmt.Errorf("%w: %q", ErrExportKindInvalid, kind)
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(header); err != nil {
		return nil, fmt.Errorf("marketdata: csv header: %w", err)
	}
	return &csvEncoder{w: cw}, nil
}

func (e *csvEncoder) Write(r Row) error {
	if r.Interval != "" {
		return e.w.Write([]string{
			r.Ts.Format(time.RFC3339), r.Symbol, r.Interval,
			r.Open.StringFixed(8), r.High.StringFixed(8),
			r.Low.StringFixed(8), r.Close.StringFixed(8),
			r.Volume.StringFixed(8), r.QuoteVolume.StringFixed(8),
			strconv.FormatInt(r.TradeCount, 10),
		})
	}
	return e.w.Write([]string{
		strconv.FormatUint(r.TradeID, 10),
		r.Ts.Format(time.RFC3339),
		r.Symbol, r.Side,
		r.Price.StringFixed(8), r.Quantity.StringFixed(8),
	})
}

func (e *csvEncoder) Flush() error {
	e.w.Flush()
	if err := e.w.Error(); err != nil {
		return fmt.Errorf("marketdata: csv flush: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// JSONL (async file) + sync {data:[...]} envelope
// ---------------------------------------------------------------------------

type jsonlEncoder struct {
	w   io.Writer
	buf []byte
}

func newJSONLEncoder(_ ExportKind, w io.Writer) rowEncoder {
	return &jsonlEncoder{w: w}
}

func (e *jsonlEncoder) Write(r Row) error {
	b, err := json.Marshal(rowDoc(r))
	if err != nil {
		return fmt.Errorf("marketdata: json encode: %w", err)
	}
	if _, err := e.w.Write(b); err != nil {
		return err
	}
	_, err = e.w.Write([]byte{'\n'})
	return err
}

func (e *jsonlEncoder) Flush() error { return nil }

// jsonEnvelopeEncoder streams the §8.8-style {"data":[...]} shape —
// docs are written as they arrive; the buffer never holds more than the
// framing bytes (one page's docs are written individually).
type jsonEnvelopeEncoder struct {
	w       io.Writer
	started bool
	count   int64
}

func newJSONEnvelopeEncoder(_ ExportKind, w io.Writer) rowEncoder {
	return &jsonEnvelopeEncoder{w: w}
}

func (e *jsonEnvelopeEncoder) Write(r Row) error {
	if !e.started {
		if _, err := io.WriteString(e.w, `{"data":[`); err != nil {
			return err
		}
		e.started = true
	}
	if e.count > 0 {
		if _, err := io.WriteString(e.w, ","); err != nil {
			return err
		}
	}
	b, err := json.Marshal(rowDoc(r))
	if err != nil {
		return fmt.Errorf("marketdata: json encode: %w", err)
	}
	if _, err := e.w.Write(b); err != nil {
		return err
	}
	e.count++
	return nil
}

func (e *jsonEnvelopeEncoder) Flush() error {
	if !e.started {
		if _, err := io.WriteString(e.w, `{"data":[`); err != nil {
			return err
		}
		e.started = true
	}
	_, err := io.WriteString(e.w, "]}")
	return err
}

// ---------------------------------------------------------------------------
// Parquet — schema per the pinned contract
// ---------------------------------------------------------------------------

// parquetTradeRow is the pinned tape schema:
//
//	trade_id uint64, ts timestamp(ms,utc), symbol string, side string,
//	price decimal(18,8), qty decimal(18,8)
//
// The bare `timestamp` tag yields TIMESTAMP(MILLIS, isAdjustedToUTC=true)
// — the spec's "timestamp(ms,utc)". decimal(8:18) is scale 8, precision
// 18 over an int64 physical (spec's decimal(18,8)).
type parquetTradeRow struct {
	TradeID uint64 `parquet:"trade_id,uint(64)"`
	Ts      int64  `parquet:"ts,timestamp"`
	Symbol  string `parquet:"symbol"`
	Side    string `parquet:"side"`
	Price   int64  `parquet:"price,decimal(8:18)"`
	Qty     int64  `parquet:"qty,decimal(8:18)"`
}

// parquetKlineRow is the OHLCV analog (open_time timestamp(ms,utc) …
// trade_count uint64).
type parquetKlineRow struct {
	OpenTime    int64  `parquet:"open_time,timestamp"`
	Symbol      string `parquet:"symbol"`
	Interval    string `parquet:"interval"`
	Open        int64  `parquet:"open,decimal(8:18)"`
	High        int64  `parquet:"high,decimal(8:18)"`
	Low         int64  `parquet:"low,decimal(8:18)"`
	Close       int64  `parquet:"close,decimal(8:18)"`
	Volume      int64  `parquet:"volume,decimal(8:18)"`
	QuoteVolume int64  `parquet:"quote_volume,decimal(8:18)"`
	TradeCount  uint64 `parquet:"trade_count,uint(64)"`
}

var parquetWriterOpts = []parquet.WriterOption{
	parquet.MaxRowsPerRowGroup(ExportParquetGroupRows),
	parquet.Compression(&snappy.Codec{}),
	parquet.CreatedBy("exchange", "export", "task-23.3.2"),
}

type parquetTradeEncoder struct {
	w   *parquet.GenericWriter[parquetTradeRow]
	buf []parquetTradeRow
}

type parquetKlineEncoder struct {
	w   *parquet.GenericWriter[parquetKlineRow]
	buf []parquetKlineRow
}

func newParquetEncoder(kind ExportKind, w io.Writer) (rowEncoder, error) {
	switch kind {
	case KindTrades, KindTicks:
		return &parquetTradeEncoder{
			w: parquet.NewGenericWriter[parquetTradeRow](w, parquetWriterOpts...),
		}, nil
	case KindKlines:
		return &parquetKlineEncoder{
			w: parquet.NewGenericWriter[parquetKlineRow](w, parquetWriterOpts...),
		}, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrExportKindInvalid, kind)
}

func (e *parquetTradeEncoder) Write(r Row) error {
	e.buf = append(e.buf, parquetTradeRow{
		TradeID: r.TradeID, Ts: r.Ts.UTC().UnixMilli(),
		Symbol: r.Symbol, Side: r.Side,
		Price: decimal.Scaled(r.Price), Qty: decimal.Scaled(r.Quantity),
	})
	if len(e.buf) >= ExportPageRows {
		if _, err := e.w.Write(e.buf); err != nil {
			return fmt.Errorf("marketdata: parquet write: %w", err)
		}
		e.buf = e.buf[:0]
	}
	return nil
}

func (e *parquetTradeEncoder) Flush() error {
	if len(e.buf) > 0 {
		if _, err := e.w.Write(e.buf); err != nil {
			return fmt.Errorf("marketdata: parquet write: %w", err)
		}
		e.buf = e.buf[:0]
	}
	if err := e.w.Close(); err != nil {
		return fmt.Errorf("marketdata: parquet close: %w", err)
	}
	return nil
}

func (e *parquetKlineEncoder) Write(r Row) error {
	e.buf = append(e.buf, parquetKlineRow{
		OpenTime: r.Ts.UTC().UnixMilli(), Symbol: r.Symbol, Interval: r.Interval,
		Open:        decimal.Scaled(r.Open),
		High:        decimal.Scaled(r.High),
		Low:         decimal.Scaled(r.Low),
		Close:       decimal.Scaled(r.Close),
		Volume:      decimal.Scaled(r.Volume),
		QuoteVolume: decimal.Scaled(r.QuoteVolume),
		TradeCount:  uint64(r.TradeCount),
	})
	if len(e.buf) >= ExportPageRows {
		if _, err := e.w.Write(e.buf); err != nil {
			return fmt.Errorf("marketdata: parquet write: %w", err)
		}
		e.buf = e.buf[:0]
	}
	return nil
}

func (e *parquetKlineEncoder) Flush() error {
	if len(e.buf) > 0 {
		if _, err := e.w.Write(e.buf); err != nil {
			return fmt.Errorf("marketdata: parquet write: %w", err)
		}
		e.buf = e.buf[:0]
	}
	if err := e.w.Close(); err != nil {
		return fmt.Errorf("marketdata: parquet close: %w", err)
	}
	return nil
}
