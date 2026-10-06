package bridge

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/guard"
)

// Claimed is one output of a forwarded settlement that pays the
// incumbent's key: the arguments of a BRC-100 wallet's internalizeAction
// ("wallet payment", BRC-29) that takes it.
type Claimed struct {
	Entry             string `json:"entry"`
	Txid              string `json:"txid"`
	Rank              int    `json:"rank"`
	OutputIndex       uint32 `json:"outputIndex"`
	Satoshis          uint64 `json:"satoshis"`
	DerivationPrefix  string `json:"derivationPrefix"`
	DerivationSuffix  string `json:"derivationSuffix"`
	SenderIdentityKey string `json:"senderIdentityKey"`
	// Transaction is the settlement as Atomic BEEF, base64.
	Transaction string `json:"transaction"`
}

// ReadKey reads a private key from a file: 64 hex digits or WIF, alone on
// its first line. It is the incumbent's operator's tool: the key never
// enters the bridge's configuration or state. A file others may read is
// refused.
func ReadKey(path string) (*ec.PrivateKey, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s may be read by others (mode %o): make it 0600 first", path, st.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s := strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
	if k, err := ec.PrivateKeyFromHex(s); err == nil && len(s) == 64 {
		return k, nil
	}
	if k, err := ec.PrivateKeyFromWif(s); err == nil {
		return k, nil
	}
	return nil, errors.New("the key file holds neither 64 hex digits nor a WIF")
}

// Claim finds, in forwarded settlements, every output that pays key under
// BRC-29 for its rank: the key the writer derived for the advertiser with
// the entry's prefix and the rank's suffix. Only the holder of the
// incumbent's private key can tell which outputs are the incumbent's.
func Claim(rems []Remittance, key *ec.PrivateKey) ([]Claimed, error) {
	kd := wallet.NewKeyDeriver(key)
	me := key.PubKey().Compressed()
	var out []Claimed
	seen := map[string]bool{}
	for _, r := range rems {
		adv, err := guard.ParsePubKeyHex(r.Advertiser)
		if err != nil || !bytes.Equal(adv.Compressed(), me) {
			continue
		}
		tx, err := transaction.NewTransactionFromBEEF(r.BEEF)
		if err != nil || tx.TxID().String() != r.Txid {
			continue
		}
		sender, err := guard.ParsePubKeyHex(r.Writer)
		if err != nil {
			continue
		}
		for i, suffix := range r.Suffixes {
			pub, err := kd.DerivePublicKey(bwallet.PaymentProtocol, bwallet.PaymentKeyID(r.DerivationPrefix, suffix),
				wallet.Counterparty{Type: wallet.CounterpartyTypeOther, Counterparty: sender}, true)
			if err != nil {
				return nil, err
			}
			want, err := p2pkhScript(pub)
			if err != nil {
				return nil, err
			}
			for vout, o := range tx.Outputs {
				if o.LockingScript == nil || !bytes.Equal(*o.LockingScript, *want) {
					continue
				}
				k := fmt.Sprintf("%s.%d", r.Txid, vout)
				if seen[k] {
					continue
				}
				seen[k] = true
				out = append(out, Claimed{Entry: r.Entry, Txid: r.Txid, Rank: i + 1, OutputIndex: uint32(vout), Satoshis: o.Satoshis,
					DerivationPrefix: r.DerivationPrefix, DerivationSuffix: suffix, SenderIdentityKey: r.Writer,
					Transaction: base64.StdEncoding.EncodeToString(r.BEEF)})
			}
		}
	}
	return out, nil
}

func p2pkhScript(pub *ec.PublicKey) (*script.Script, error) {
	addr, err := script.NewAddressFromPublicKey(pub, true)
	if err != nil {
		return nil, fmt.Errorf("address: %w", err)
	}
	return p2pkh.Lock(addr)
}
