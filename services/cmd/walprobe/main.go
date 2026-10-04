package main

import (
	"fmt"
	"os"
	"strconv"

	"exchange/internal/recovery"
)

func main() {
	lo, _ := strconv.ParseUint(os.Args[1], 10, 64)
	for _, dir := range os.Args[2:] {
		trades, err := recovery.ScanTrades(dir)
		if err != nil {
			fmt.Println(dir, "err", err)
			continue
		}
		n, mx := 0, uint64(0)
		for _, t := range trades {
			if t.TradeID > lo {
				n++
				if t.TradeID > mx {
					mx = t.TradeID
				}
			}
		}
		fmt.Println(dir, "trades>", lo, ":", n, "max:", mx)
	}
}
