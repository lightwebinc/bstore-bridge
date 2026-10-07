// Command bstore-bridge joins an incumbent UHRP host to the bstore storage
// market: bstore's provider as the market front over a bridge backend that
// speaks the incumbent's own HTTP interfaces. See docs/bridge.md.
//
//	bstore-bridge init -data DIR          create the bridge's identity and wallet
//	bstore-bridge address -config FILE    the identity and the fund address
//	bstore-bridge fund -config FILE -beef FILE
//	                                      import a payment you sent to the fund address, from its BEEF
//	bstore-bridge fund -config FILE -txid TXID
//	                                      import a mined payment you sent to the fund address
//	bstore-bridge rescan -config FILE -from H [-to H]
//	                                      coinbase: only on a regtest chain you run (development
//	                                      and tests); take coinbase paying the fund address into the pool
//	bstore-bridge fund -config FILE -blocks N
//	                                      coinbase: only on a regtest chain you run (development
//	                                      and tests); mine N blocks to the fund address
//	bstore-bridge run -config FILE        serve until interrupted
//	bstore-bridge payouts -config FILE    the settlements forwarded for the incumbent
//	bstore-bridge claim -remittances FILE -key-file F
//	                                      the incumbent's operator: which forwarded outputs pay its key
//	bstore-bridge doctor -config FILE [-ca FILE]
//	                                      the front, the incumbent and whether everything answers; reads only
//	bstore-bridge version                 print the version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/nodeapi"

	"github.com/lightwebinc/bstore-bridge/bridge"
	"github.com/lightwebinc/bstore/provider"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "bstore-bridge:", err)
		var u usageError
		if errors.As(err, &u) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

type usageError struct{ error }

// version is stamped by the build; "dev" otherwise.
var version = "dev"

const usage = `usage: bstore-bridge <command> [flags]

commands:
  init -data DIR                 create the bridge's identity and wallet under DIR
  address -config FILE           print the identity key and the fund address
  fund -config FILE -beef FILE   import a payment you sent to the fund address from the BEEF
                                 your wallet gave you (FILE, or - for standard input): no lookup;
                                 one not mined yet waits in the pool until it mines, unless
                                 -refuse-unmined (mainnet, testnet)
  fund -config FILE -txid TXID   import a mined payment you sent to the fund address: every
                                 output paying it that the node shows unspent, its proof checked
                                 against the header source (mainnet, testnet)
  rescan -config FILE -from H [-to H]
                                 coinbase: only on a regtest chain you run (development and tests);
                                 take coinbase paying the fund address in blocks H.. into the pool
  fund -config FILE -blocks N    coinbase: only on a regtest chain you run (development and tests);
                                 mine N blocks to the fund address through the node's RPC
                                 (settle rpc:<url>) and pool the coinbase
  run -config FILE               serve until interrupted
  payouts -config FILE           print the settlements forwarded for the incumbent, one JSON object a line
  claim -remittances FILE -key-file F
                                 the incumbent's operator, where its key is, with a copy of the bridge's
                                 remittances.jsonl: print, for each forwarded output that pays the key
                                 in F (64 hex digits or WIF, a file of mode 0600), what a BRC-100 wallet's
                                 internalizeAction takes; the key is read, used and forgotten
  doctor -config FILE [-ca FILE] the front as a provider's doctor reports it, then the incumbent: its
                                 URL and lookup hosts answering, settlements forwarded; reads only
  version                        print the version

docs/bridge.md is the operator's guide: what the incumbent must serve, the
configuration, operating and the limits.`

func run(args []string) error {
	if len(args) == 0 {
		return usageError{errors.New(usage)}
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		fmt.Println(usage)
		return nil
	case "version", "-version", "--version":
		fmt.Println("bstore-bridge", version)
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	data := fs.String("data", "", "data directory (init)")
	cfgPath := fs.String("config", "", "configuration file")
	txid := fs.String("txid", "", "import the outputs of a mined payment to the fund address (fund; mainnet, testnet)")
	beef := fs.String("beef", "", "import a payment to the fund address from its BEEF, a file or - (fund; mainnet, testnet)")
	refuse := fs.Bool("refuse-unmined", false, "with -beef, take a mined payment only")
	from := fs.Uint("from", 0, "coinbase: only on a regtest chain you run (development and tests): first block height to rescan for coinbase")
	to := fs.Uint("to", 0, "last block height to rescan for coinbase (default the tip)")
	blocks := fs.Int("blocks", 0, "coinbase: only on a regtest chain you run (development and tests): blocks to mine (fund)")
	keyFile := fs.String("key-file", "", "the incumbent's private key (claim)")
	remPath := fs.String("remittances", "", "a copy of the bridge's remittances.jsonl (claim)")
	caFile := fs.String("ca", "", "a PEM file trusted for the front's certificate (doctor)")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageError{err}
	}
	switch args[0] {
	case "init":
		if *data == "" {
			return usageError{errors.New("init needs -data")}
		}
		id, err := bridge.Init(*data)
		if err != nil {
			return err
		}
		fmt.Println("identity", id)
		return nil
	case "claim":
		if *keyFile == "" || *remPath == "" {
			return usageError{errors.New("claim needs -remittances and -key-file")}
		}
		rems, err := bridge.ReadRemittanceFile(*remPath)
		if err != nil {
			return err
		}
		key, err := bridge.ReadKey(*keyFile)
		if err != nil {
			return err
		}
		cl, err := bridge.Claim(rems, key)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		for _, c := range cl {
			_ = enc.Encode(c)
		}
		fmt.Fprintf(os.Stderr, "%d output(s) pay the key in %d forwarded settlement(s)\n", len(cl), len(rems))
		return nil
	case "address", "rescan", "fund", "run", "payouts", "doctor":
	default:
		return usageError{fmt.Errorf("unknown command %q\n%s", args[0], usage)}
	}
	if *cfgPath == "" {
		return usageError{errors.New(args[0] + " needs -config")}
	}
	cfg, err := bridge.LoadConfig(*cfgPath)
	if err != nil {
		return usageError{err}
	}
	pc := &cfg.Provider
	switch args[0] {
	case "doctor":
		return bridge.Doctor(ctx, cfg, *caFile, os.Stdout)
	case "payouts":
		rems, err := bridge.ReadRemittances(pc.Data)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(os.Stdout)
		for _, r := range rems {
			_ = enc.Encode(r)
		}
		return nil
	case "address":
		w, err := bwallet.Open(provider.WalletDir(pc.Data), provider.Profile)
		if err != nil {
			return err
		}
		addr, err := w.Signer().FundAddress(pc.Mainnet)
		if err != nil {
			return err
		}
		fmt.Println("identity", w.Signer().IdentityHex())
		fmt.Println("fund address", addr)
		fmt.Printf("pool %d output(s), %d satoshis\n", w.Pool.Count(), w.Pool.Balance())
		return nil
	case "rescan", "fund":
		w, err := bwallet.Open(provider.WalletDir(pc.Data), provider.Profile)
		if err != nil {
			return err
		}
		f, err := newFunder(pc)
		if err != nil {
			return usageError{err}
		}
		if args[0] == "fund" && (*txid != "" || *beef != "") {
			var res funded
			if *txid != "" {
				res, err = f.txid(ctx, w, *txid)
			} else {
				raw, rerr := readBEEF(*beef)
				if rerr != nil {
					return usageError{rerr}
				}
				res, err = f.beef(ctx, w, raw, *refuse)
			}
			if err != nil {
				return err
			}
			fmt.Println(res)
			fmt.Printf("pool %d output(s), %d satoshis\n", w.Pool.Count(), w.Pool.Balance())
			return nil
		}
		asset := &nodeapi.Asset{Base: pc.Node}
		if args[0] == "fund" {
			kind, addr, _ := strings.Cut(pc.Settle, ":")
			if kind != "rpc" || *blocks < 1 {
				return usageError{errors.New("fund needs -beef FILE or -txid TXID, or (coinbase: only on a regtest chain you run) settle rpc:<url> and -blocks N")}
			}
			rpc := &nodeapi.RPC{URL: addr, User: pc.RPCUser, Pass: pc.RPCPass, ID: "bstore-bridge"}
			n, _, err := bwallet.FundFromCoinbase(ctx, w.Signer(), w.Pool, rpc, asset, *blocks, 50)
			if err != nil {
				return err
			}
			fmt.Printf("%d coinbase output(s) pooled; pool %d satoshis\n", n, w.Pool.Balance())
			return nil
		}
		top := uint32(*to)
		if top == 0 {
			h, err := f.headers.CurrentHeight(ctx)
			if err != nil {
				return err
			}
			top = h
		}
		n, err := bwallet.Rescan(ctx, w.Signer(), w.Pool, asset, uint32(*from), top)
		if err != nil {
			return err
		}
		fmt.Printf("%d output(s) found; pool %d satoshis\n", n, w.Pool.Balance())
		return nil
	}
	b, err := bridge.Open(ctx, cfg, bridge.Options{})
	if err != nil {
		return err
	}
	if err := b.Start(ctx); err != nil {
		return err
	}
	fmt.Printf("bstore-bridge %s listening on %s, front %s, for incumbent %s at %s\n", b.Identity(), b.Addr(), pc.FrontURL(),
		cfg.Incumbent.Identity, cfg.Incumbent.URL)
	<-ctx.Done()
	return b.Close()
}
