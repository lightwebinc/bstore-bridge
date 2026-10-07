package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bsv-blockchain/go-sdk/chainhash"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/headers"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"

	"github.com/lightwebinc/bstore/provider"
)

// funded is what an import took into the pool.
type funded struct {
	txid                       string
	mined                      bool
	height                     uint32
	paying, held, spent, added int
	sats                       uint64
}

// funder imports a payment to the fund address: by txid from the chain
// view (bwallet.ImportTxid), or from its BEEF with no lookup
// (bwallet.ImportBEEF).
type funder struct {
	chain   nodeapi.Chain
	headers *headers.Client
	settler publish.Settler
}

func newFunder(pc *provider.Config) (*funder, error) {
	hc, err := headers.NewSource(pc.Headers)
	if err != nil {
		return nil, fmt.Errorf("headers: %w", err)
	}
	chain, err := nodeapi.ParseChain("asset:"+pc.Node, nodeapi.ChainOptions{Headers: hc})
	if err != nil {
		return nil, fmt.Errorf("node: %w", err)
	}
	settler, _, err := publish.ParseSettler(pc.Settle, publish.SettleOptions{Key: pc.ArcadeKey, RPCUser: pc.RPCUser,
		RPCPass: pc.RPCPass, RPCID: "bstore-bridge", Spends: chain})
	if err != nil {
		return nil, err
	}
	return &funder{chain: chain, headers: hc, settler: settler}, nil
}

func (f *funder) txid(ctx context.Context, w *bwallet.Embedded, txid string) (funded, error) {
	txid = strings.ToLower(strings.TrimSpace(txid))
	if _, err := chainhash.NewHashFromHex(txid); err != nil || len(txid) != 64 {
		return funded{}, fmt.Errorf("not a transaction id: %q", txid)
	}
	fund, err := w.Signer().FundScript()
	if err != nil {
		return funded{}, err
	}
	im, err := bwallet.ImportTxid(ctx, txid, fund, f.chain, f.headers)
	if err != nil {
		return funded{}, err
	}
	return f.take(ctx, w.Pool, im)
}

// beef imports a payment from its BEEF. One not mined yet is taken when
// every parent is proven, unless refuseUnmined; it is handed to the
// settlement leg in case the wallet did not broadcast it, and its outputs
// wait in the pool until it mines.
func (f *funder) beef(ctx context.Context, w *bwallet.Embedded, raw []byte, refuseUnmined bool) (funded, error) {
	fund, err := w.Signer().FundScript()
	if err != nil {
		return funded{}, err
	}
	im, err := bwallet.ImportBEEF(ctx, raw, fund, f.headers, bwallet.ImportOptions{RefuseUnmined: refuseUnmined})
	if err != nil {
		return funded{}, err
	}
	if !im.Mined {
		if err := f.settler.Submit(ctx, im.Tx); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already") {
			return funded{}, fmt.Errorf("payment %s: the settlement leg did not take it: %w", im.Txid, err)
		}
	}
	return f.take(ctx, w.Pool, im)
}

// take adds the outputs the pool does not hold. A mined output the chain
// view shows spent is left out; one it cannot vouch for is an error and
// nothing is added.
func (f *funder) take(ctx context.Context, pool *bwallet.Pool, im *bwallet.Import) (funded, error) {
	res := funded{txid: im.Txid, mined: im.Mined, height: im.Height, paying: len(im.Outputs)}
	held := map[string]bool{}
	for _, o := range pool.Outputs() {
		held[o.Outpoint()] = true
	}
	var outs []bwallet.Output
	for _, o := range im.Outputs {
		if held[o.Outpoint()] {
			res.held++
			continue
		}
		if im.Mined {
			by, err := f.chain.Spender(ctx, o.TxID, o.Vout)
			if err != nil {
				return funded{}, fmt.Errorf("output %s: %w", o.Outpoint(), err)
			}
			if by != "" {
				res.spent++
				continue
			}
		}
		outs = append(outs, o)
		res.sats += o.Satoshis
	}
	n, err := pool.Add(outs...)
	res.added = n
	return res, err
}

func (r funded) String() string {
	state := fmt.Sprintf("mined at height %d", r.height)
	if !r.mined {
		state = "not mined yet (held until it mines)"
	}
	return fmt.Sprintf("%s %s: %d output(s) to the fund address, %d imported (%d satoshis), %d already held, %d spent",
		r.txid, state, r.paying, r.added, r.sats, r.held, r.spent)
}

// readBEEF reads a payment's BEEF from a file, or standard input for "-",
// as bytes or as hex.
func readBEEF(path string) ([]byte, error) {
	const max = 64 << 20
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	raw, err := io.ReadAll(io.LimitReader(r, 2*max+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 2*max {
		return nil, errors.New("the BEEF is larger than 64 MiB")
	}
	if t := bytes.TrimSpace(raw); len(t) > 0 && len(t)%2 == 0 {
		if b, err := hex.DecodeString(string(t)); err == nil {
			return b, nil
		}
	}
	if len(raw) == 0 {
		return nil, errors.New("no BEEF read")
	}
	return raw, nil
}
