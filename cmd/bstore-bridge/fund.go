package main

import (
	"fmt"

	"github.com/lightwebinc/bstore/chaincfg"
)

// fundedLine is one line for what an import took into the pool.
func fundedLine(r chaincfg.Funded) string {
	state := fmt.Sprintf("mined at height %d", r.Height)
	if !r.Mined {
		state = "not mined yet (held until it mines)"
	}
	return fmt.Sprintf("%s %s: %d output(s) to the fund address, %d imported (%d satoshis), %d already held, %d spent",
		r.Txid, state, r.Paying, r.Added, r.Satoshis, r.Held, r.Spent)
}
