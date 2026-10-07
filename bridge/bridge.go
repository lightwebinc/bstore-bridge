package bridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/headers"
	"github.com/lightwebinc/bcommon/lookup"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/pushdrop"

	"github.com/lightwebinc/bstore-bridge/uhrp"
	"github.com/lightwebinc/bstore/provider"
	"github.com/lightwebinc/bstore/storerec"
)

// Options are what a caller embedding a bridge may set.
type Options struct {
	Now  func() time.Time
	Tick time.Duration
	Logf func(format string, args ...any)
	// Poll paces the bridge's own waits (an advertisement to appear, a
	// funding tree to mine).
	Poll time.Duration
}

// Bridge is a running bridge: bstore's provider as the market front, over
// the bridge backend.
type Bridge struct {
	cfg  *Config
	opts Options
	p    *provider.Provider
	be   *Backend
	inc  *uhrp.Client
	hc   *http.Client

	// chain is the chain view the bridge's own reads go to (a node's,
	// the front's node); every proof is checked against hdrs.
	chain nodeapi.Chain
	hdrs  *headers.Client

	fmu    sync.Mutex // remittances.jsonl
	stmtMu sync.Mutex
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Init creates the bridge's identity and wallet under data (the front's:
// it is the key the bridge authenticates to the incumbent with, signs
// challenge responses and its front statements under, and pays from).
func Init(data string) (string, error) { return provider.Init(data) }

// Open opens a bridge over a checked configuration; Start serves it.
func Open(ctx context.Context, cfg *Config, opts Options) (*Bridge, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	if opts.Poll == 0 {
		opts.Poll = time.Second
	}
	hc, err := cfg.Incumbent.httpClient()
	if err != nil {
		return nil, err
	}
	hdrs, err := headers.NewSource(cfg.Provider.Headers)
	if err != nil {
		return nil, err
	}
	chain, err := nodeapi.ParseChain("asset:"+cfg.Provider.Node, nodeapi.ChainOptions{Headers: hdrs})
	if err != nil {
		return nil, fmt.Errorf("provider.node: %w", err)
	}
	b := &Bridge{cfg: cfg, opts: opts, hc: hc, chain: chain, hdrs: hdrs}
	b.inc = &uhrp.Client{Base: cfg.Incumbent.URL, HTTP: hc, Identity: cfg.Incumbent.IdentityKey()}
	logf := func(format string, args ...any) { opts.Logf("bstore-bridge: "+format, args...) }
	be, err := newBackend(filepath.Join(cfg.Provider.Data, "bridge"), &cfg.Incumbent, b.inc, b.chain, hdrs, logf, opts.Now)
	if err != nil {
		return nil, err
	}
	be.poll = opts.Poll
	b.be = be
	p, err := provider.New(ctx, &cfg.Provider, provider.Options{Now: opts.Now, Tick: opts.Tick, Logf: opts.Logf,
		Front: &provider.Front{Backend: be, Advertiser: cfg.Incumbent.IdentityKey(), Forward: b.forward}})
	if err != nil {
		return nil, err
	}
	b.p = p
	be.source = p.Staged
	b.inc.Purse = p.Purse
	return b, nil
}

func (b *Bridge) logf(format string, args ...any) { b.opts.Logf("bstore-bridge: "+format, args...) }

// Identity is the bridge's key: the front's.
func (b *Bridge) Identity() string { return b.p.Identity() }

// Provider is the market front.
func (b *Bridge) Provider() *provider.Provider { return b.p }

// Addr is where the front's routes are served.
func (b *Bridge) Addr() string { return b.p.Addr() }

// Start serves the front and starts the bridge's own work: its front
// statement on every host, looked for and published again where missing.
func (b *Bridge) Start(ctx context.Context) error {
	if err := b.p.Start(ctx); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	b.cancel = cancel
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		every := time.Duration(b.cfg.StatementCheckSeconds) * time.Second
		for {
			if err := b.Statements(ctx); err != nil && ctx.Err() == nil {
				b.logf("front statements: %v; tried again", err)
				every = min(every, 10*time.Minute)
			} else {
				every = time.Duration(b.cfg.StatementCheckSeconds) * time.Second
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(every):
			}
		}
	}()
	return nil
}

// Close stops the bridge and its front.
func (b *Bridge) Close() error {
	if b.cancel != nil {
		b.cancel()
	}
	b.wg.Wait()
	return b.p.Close()
}

// carrierParams are a carrier's under d, as the host admits them.
func carrierParams(d pushdrop.Derivation) carrier.Params {
	return carrier.Params{Derivation: d, FundingTag: storerec.TagFunding,
		ValidatePayload: func([]byte) error { return nil }, ErrIdentity: storerec.ErrIdentity}
}

// Statements makes sure every host carries, for every bucket, a front
// statement (spec section 8.2) naming the bridge as the incumbent's front
// at the front's URL: one is looked for with the host's front class and,
// where it is missing, one is minted (a mined funding output under the
// front key) and submitted to that host's records topic. A host keeps
// another's statement a day only, unless it is among the first for the
// advertiser (spec section 12.3), so it is looked for again periodically.
func (b *Bridge) Statements(ctx context.Context) error {
	b.stmtMu.Lock()
	defer b.stmtMu.Unlock()
	var errs []error
	for _, bc := range b.cfg.Provider.Buckets {
		var missing []string
		for _, h := range b.cfg.Provider.Hosts {
			ok, err := b.statementAt(ctx, h, bc.Bucket)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", h, err))
				continue
			}
			if !ok {
				missing = append(missing, h)
			}
		}
		b.be.mu.Lock()
		rec := b.be.st.Statements[bc.Bucket]
		var kept statementRec
		if rec != nil {
			if len(missing) == 0 {
				rec.Tries = 0
				_ = b.be.st.save()
			}
			kept = *rec
		}
		b.be.mu.Unlock()
		if len(missing) == 0 {
			continue
		}
		// The statement already made is sent again, twice at most, before
		// a new one is minted: a host that lost it, or never took it, costs
		// no new fee.
		if beef, err := hex.DecodeString(kept.Beef); err == nil && len(beef) > 0 && kept.Tries < 2 &&
			b.opts.Now().Sub(time.Unix(kept.At, 0)) < 30*24*time.Hour {
			b.be.mu.Lock()
			b.be.st.Statements[bc.Bucket].Tries++
			_ = b.be.st.save()
			b.be.mu.Unlock()
			errs = append(errs, b.submitStatement(ctx, bc.Bucket, beef, missing))
			b.logf("bucket %s: front statement %s sent again to %d host(s)", bc.Bucket, short(kept.Txid), len(missing))
			continue
		}
		if err := b.publishStatement(ctx, bc.Bucket, missing); err != nil {
			errs = append(errs, fmt.Errorf("bucket %s: %w", bc.Bucket, err))
		}
	}
	return errors.Join(errs...)
}

// statementAt reports whether host answers a statement by this front for
// the incumbent in bucket, at the front's URL, that admits.
func (b *Bridge) statementAt(ctx context.Context, host, bucket string) (bool, error) {
	q := map[string]string{"bucket": bucket, "advertiser": b.cfg.Incumbent.Identity, "front": b.Identity()}
	body, _ := json.Marshal(lookup.Question{Service: storerec.LookupService, Query: q})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(host, "/")+"/lookup", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := b.hc.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return false, err
	}
	if res.StatusCode != http.StatusOK {
		return false, fmt.Errorf("lookup: %d", res.StatusCode)
	}
	var a lookup.Answer
	if err := json.Unmarshal(raw, &a); err != nil {
		return false, err
	}
	me, _ := hex.DecodeString(b.Identity())
	for _, o := range a.Outputs {
		ad, err := storerec.Admit(ctx, o.Beef, nil, storerec.Host{Bucket: bucket, Headers: b.hdrs})
		if err != nil || ad.Carrier == nil || ad.Carrier.Front == nil {
			continue
		}
		f := ad.Carrier.Front
		if f.Bucket == bucket && bytes.Equal(f.Front, me) && bytes.Equal(f.Advertiser, b.cfg.Incumbent.IdentityKey()) &&
			f.URL == b.cfg.Provider.FrontURL() {
			return true, nil
		}
	}
	return false, nil
}

// publishStatement mints a front statement for bucket and submits it to
// hosts.
func (b *Bridge) publishStatement(ctx context.Context, bucket string, hosts []string) error {
	w := b.p.Wallet()
	const originator = "bstore-bridge"
	params := carrierParams(storerec.FrontDerivation)
	lock, err := carrier.FundingLock(ctx, w, originator, params)
	if err != nil {
		return err
	}
	fees := b.p.Fees()
	pay := b.p.NewPayer()
	if h, err := b.hdrs.CurrentHeight(ctx); err == nil {
		pay.Tip = h
	}
	fee, err := pay.TakeAtLeast(ctx, 1+fees.Floor*4)
	if err != nil {
		return fmt.Errorf("a funding output for the statement: %w", err)
	}
	change, err := b.p.Signer().FundScript()
	if err != nil {
		pay.GiveBack()
		return err
	}
	tree, err := mint.FundingTree(lock, 1, 1, fee, change, fees)
	if err != nil {
		pay.GiveBack()
		return err
	}
	settler := b.p.Settler()
	if err := settler.Submit(ctx, tree); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already") {
		pay.GiveBack()
		return fmt.Errorf("the statement's funding output: %w", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	mp, height, err := nodeapi.WaitMinedOn(wctx, b.chain, tree.TxID().String(), b.opts.Poll)
	if err != nil {
		pay.Change(tree, 0, nil)
		return fmt.Errorf("the statement's funding output is not mined: %w", err)
	}
	pay.Change(tree, height, mp)
	tree.MerklePath = mp
	me, _ := hex.DecodeString(b.Identity())
	rec, err := (&storerec.Front{Bucket: bucket, Front: me, Advertiser: b.cfg.Incumbent.IdentityKey(), URL: b.cfg.Provider.FrontURL(),
		Created: uint64(b.opts.Now().UnixMilli())}).Encode()
	if err != nil {
		return err
	}
	tx, err := carrier.Mint(ctx, w, originator, params, rec, tree, 0)
	if err != nil {
		return err
	}
	beef, err := tx.AtomicBEEF(false)
	if err != nil {
		return err
	}
	b.be.mu.Lock()
	b.be.st.Statements[bucket] = &statementRec{Txid: tx.TxID().String(), At: b.opts.Now().Unix(), Beef: hex.EncodeToString(beef)}
	_ = b.be.st.save()
	b.be.mu.Unlock()
	err = b.submitStatement(ctx, bucket, beef, hosts)
	b.logf("bucket %s: front statement %s published, naming this bridge as %s's front at %s", bucket, short(tx.TxID().String()),
		short(b.cfg.Incumbent.Identity), b.cfg.Provider.FrontURL())
	return err
}

// submitStatement sends a statement's carrier to hosts' records topic, and
// to the facade when one is set.
func (b *Bridge) submitStatement(ctx context.Context, bucket string, beef []byte, hosts []string) error {
	var errs []error
	for _, h := range hosts {
		f := &publish.Facade{Base: strings.TrimRight(h, "/")}
		if _, err := f.Submit(ctx, storerec.RecordsTopic(bucket), beef); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", h, err))
		}
	}
	if b.cfg.Provider.Facade != "" {
		f := &publish.Facade{Base: strings.TrimRight(b.cfg.Provider.Facade, "/")}
		_, _ = f.Submit(ctx, storerec.RecordsTopic(bucket), beef)
	}
	return errors.Join(errs...)
}

// Remittance is a settlement forwarded for the incumbent: what its
// operator needs to find and take the incumbent's output (BRC-29, spec
// section 9.5). The bridge never holds the incumbent's key, so it cannot
// tell which rank, if any, is the incumbent's; Suffixes are every rank's.
type Remittance struct {
	provider.Settlement
	Advertiser string   `json:"advertiser"`
	Suffixes   []string `json:"derivationSuffixes"`
	At         int64    `json:"at"`
}

// RemittancesFile is where forwarded settlements are appended, one JSON
// object a line, under the data directory.
const RemittancesFile = "remittances.jsonl"

// forward appends a settlement to the remittances file, synced.
func (b *Bridge) forward(_ context.Context, s provider.Settlement) error {
	r := Remittance{Settlement: s, Advertiser: b.cfg.Incumbent.Identity, At: b.opts.Now().Unix()}
	for rank := 1; uint64(rank) <= s.K; rank++ {
		r.Suffixes = append(r.Suffixes, storerec.DerivationSuffix(rank))
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	b.fmu.Lock()
	defer b.fmu.Unlock()
	f, err := os.OpenFile(filepath.Join(b.cfg.Provider.Data, RemittancesFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	b.logf("entry %s: settlement %s forwarded for the incumbent (%s)", short(s.Entry), short(s.Txid), RemittancesFile)
	return nil
}

// ReadRemittances reads the forwarded settlements under data.
func ReadRemittances(data string) ([]Remittance, error) {
	return ReadRemittanceFile(filepath.Join(data, RemittancesFile))
}

// ReadRemittanceFile reads forwarded settlements from a file.
func ReadRemittanceFile(path string) ([]Remittance, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Remittance
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var r Remittance
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func hexOf(h [32]byte) string { return hex.EncodeToString(h[:]) }

func parseHash(s string) ([32]byte, bool) {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		return [32]byte{}, false
	}
	return [32]byte(b), true
}
