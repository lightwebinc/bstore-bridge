package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/goldentest"
	"github.com/lightwebinc/bcommon/testchain"

	"github.com/lightwebinc/bstore/provider"
)

// A payment to the fund address is imported once, by txid or from its
// BEEF; one paying nothing is refused; an unmined one by txid is
// ErrUnmined, and from its BEEF is held back, or refused when asked.
func TestFund(t *testing.T) {
	ctx := context.Background()
	chain := testchain.New(700)
	srv := httptest.NewServer(chain)
	defer srv.Close()
	f, err := newFunder(&provider.Config{Node: srv.URL, Settle: "rpc:" + srv.URL + "/rpc", Headers: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	key := goldentest.FixedKey()
	addr, _ := script.NewAddressFromPublicKey(key.PubKey(), false)
	if _, err := chain.Generate(testchain.Maturity+4, addr.AddressString); err != nil {
		t.Fatal(err)
	}
	var coins []*transaction.Transaction
	for _, id := range chain.Txids() {
		if _, h, _ := chain.Proof(id); h <= 704 {
			coins = append(coins, chain.Tx(id))
		}
	}
	data := filepath.Join(t.TempDir(), "b")
	if _, err := provider.Init(data); err != nil {
		t.Fatal(err)
	}
	w, err := bwallet.Open(provider.WalletDir(data), provider.Profile)
	if err != nil {
		t.Fatal(err)
	}
	fund, _ := w.Signer().FundScript()
	build := func(coin *transaction.Transaction, to *script.Script) *transaction.Transaction {
		unlock, _ := p2pkh.Unlock(key, nil)
		tx := transaction.NewTransaction()
		tx.AddInputFromTx(coin, 0, unlock)
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: 20_000, LockingScript: to})
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: coin.Outputs[0].Satoshis - 20_500, LockingScript: coin.Outputs[0].LockingScript})
		if err := tx.Sign(); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	beefOf := func(tx *transaction.Transaction) []byte {
		mp, _, _ := chain.Proof(tx.Inputs[0].SourceTransaction.TxID().String())
		tx.Inputs[0].SourceTransaction.MerklePath = mp
		if mp, _, ok := chain.Proof(tx.TxID().String()); ok {
			tx.MerklePath = mp
		}
		b, err := tx.BEEF()
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	chain.SetHold(true)
	paid := build(coins[0], fund)
	if err := chain.Send(paid); err != nil {
		t.Fatal(err)
	}
	if _, err := f.txid(ctx, w, paid.TxID().String()); !errors.Is(err, bwallet.ErrUnmined) {
		t.Fatalf("unmined: %v", err)
	}
	chain.Mine()
	if res, err := f.txid(ctx, w, paid.TxID().String()); err != nil || res.added != 1 || res.sats != 20_000 || !res.mined {
		t.Fatalf("by txid: %+v %v", res, err)
	}
	if res, err := f.beef(ctx, w, beefOf(paid), false); err != nil || res.added != 0 || res.held != 1 {
		t.Fatalf("again from BEEF: %+v %v", res, err)
	}
	other := build(coins[1], coins[1].Outputs[0].LockingScript)
	if err := chain.Send(other); err != nil {
		t.Fatal(err)
	}
	chain.Mine()
	if _, err := f.txid(ctx, w, other.TxID().String()); !errors.Is(err, bwallet.ErrPaysNothing) {
		t.Fatalf("pays nothing: %v", err)
	}
	chain.SetHold(true)
	unmined := build(coins[2], fund)
	if _, err := f.beef(ctx, w, beefOf(unmined), true); !errors.Is(err, bwallet.ErrUnmined) {
		t.Fatalf("unmined refused: %v", err)
	}
	if res, err := f.beef(ctx, w, beefOf(unmined), false); err != nil || res.added != 1 || res.mined {
		t.Fatalf("unmined from BEEF: %+v %v", res, err)
	}
	if chain.Waiting() != 1 || len(w.Pool.UnprovenTxids()) != 1 {
		t.Fatalf("waiting %d, unproven %v", chain.Waiting(), w.Pool.UnprovenTxids())
	}
}
