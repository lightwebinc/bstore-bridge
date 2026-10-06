package uhrp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/lookup"

	"github.com/lightwebinc/bstore/storerec"
)

// LookupService is BRC-26's lookup service.
const LookupService = "ls_uhrp"

// Bounds on a lookup answer.
const (
	maxAnswer     = 4 << 20
	maxOutputBEEF = 1 << 20
	maxOutputs    = 64
)

// Found is an advertisement a lookup host answered, decoded and not yet
// verified: the caller holds it to its own headers before it uses it.
type Found struct {
	Tx   *transaction.Transaction
	Txid string
	Vout uint32
	Ad   *storerec.Advertisement
	Host string
}

// Find asks each lookup host for root's advertisements (ls_uhrp, by UHRP
// URL) and keeps those by key whose url is exactly url and whose hash is
// root; any other output, and any answer that does not read, is passed
// over. Duplicates across hosts are kept once.
func (c *Client) Find(ctx context.Context, hosts []string, root [32]byte, key []byte, url string) ([]Found, error) {
	var out []Found
	seen := map[string]bool{}
	var lastErr error
	for _, h := range hosts {
		a, err := c.ask(ctx, h, map[string]string{"uhrpUrl": URL(root)})
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", h, err)
			continue
		}
		for i, o := range a.Outputs {
			if i >= maxOutputs {
				break
			}
			tx, err := subject(o.Beef)
			if err != nil || uint64(o.OutputIndex) >= uint64(len(tx.Outputs)) || tx.Outputs[o.OutputIndex].LockingScript == nil {
				continue
			}
			ad, err := storerec.DecodeAdvertisement(*tx.Outputs[o.OutputIndex].LockingScript)
			if err != nil || !bytes.Equal(ad.Host, key) || ad.Hash != root || ad.URL != url {
				continue
			}
			id := tx.TxID().String()
			k := fmt.Sprintf("%s.%d", id, o.OutputIndex)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, Found{Tx: tx, Txid: id, Vout: o.OutputIndex, Ad: ad, Host: h})
		}
	}
	if len(out) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}

// subject is the transaction a lookup output's BEEF is about: its last.
func subject(beef []byte) (tx *transaction.Transaction, err error) {
	if err := guard.CheckBEEF(beef, maxOutputBEEF); err != nil {
		return nil, err
	}
	defer func() {
		if r := recover(); r != nil {
			tx, err = nil, fmt.Errorf("beef parse panicked: %v", r)
		}
	}()
	return transaction.NewTransactionFromBEEF(beef)
}

func (c *Client) ask(ctx context.Context, host string, q any) (*lookup.Answer, error) {
	body, err := json.Marshal(lookup.Question{Service: LookupService, Query: q})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(host, "/")+"/lookup", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxAnswer+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxAnswer {
		return nil, fmt.Errorf("an answer over %d bytes", maxAnswer)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lookup: %d %s", res.StatusCode, firstLine(raw))
	}
	var a lookup.Answer
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("lookup answer: %w", err)
	}
	if a.Type != lookup.TypeOutputList {
		return nil, lookup.ErrNotOutputList
	}
	return &a, nil
}
