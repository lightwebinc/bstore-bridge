package bridge

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/headers"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/purse"
	"github.com/lightwebinc/bcommon/testchain"

	"github.com/lightwebinc/bstore-bridge/internal/fakeuhrp"
	"github.com/lightwebinc/bstore-bridge/uhrp"
	"github.com/lightwebinc/bstore/backend"
	"github.com/lightwebinc/bstore/storerec"
)

// rig is a local chain, a stand-in incumbent over TLS with its ls_uhrp
// host, and a funded bridge wallet.
type rig struct {
	t      *testing.T
	ctx    context.Context
	chain  *httptest.Server
	asset  *nodeapi.Asset
	rpc    *nodeapi.RPC
	hdrs   *headers.Client
	fake   *fakeuhrp.Server
	lookup *httptest.Server
	cert   string
	key    string
	client *http.Client
	bw     *bwallet.Embedded

	mu   sync.Mutex
	logs []string
}

func newRig(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()
	chain := httptest.NewServer(testchain.New(700))
	t.Cleanup(chain.Close)
	dir := t.TempDir()
	cert, key, pool := testCert(t, dir)
	r := &rig{t: t, ctx: ctx, chain: chain, asset: &nodeapi.Asset{Base: chain.URL}, rpc: &nodeapi.RPC{URL: chain.URL + "/rpc", ID: "t"},
		cert: cert, key: key, client: &http.Client{Timeout: time.Minute, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}}
	hc, err := headers.NewSource(chain.URL)
	if err != nil {
		t.Fatal(err)
	}
	r.hdrs = hc
	fw := r.wallet("incumbent")
	look := &fakeuhrp.Lookup{}
	r.lookup = httptest.NewServer(look)
	t.Cleanup(r.lookup.Close)
	f, err := fakeuhrp.New(ctx, fw, r.asset, &publish.RPCSettler{RPC: r.rpc}, hc, look)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(f)
	c, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{c}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	f.Base = srv.URL
	r.fake = f
	r.bw = r.wallet("bridge")
	return r
}

func (r *rig) wallet(name string) *bwallet.Embedded {
	r.t.Helper()
	w, err := bwallet.Create(filepath.Join(r.t.TempDir(), name), fakeuhrp.Profile)
	if err != nil {
		r.t.Fatal(err)
	}
	if _, _, err := bwallet.FundFromCoinbase(r.ctx, w.Signer(), w.Pool, r.rpc, r.asset, 104, 0); err != nil {
		r.t.Fatal(err)
	}
	return w
}

func (r *rig) logf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, format)
	_ = args
}

func (r *rig) purse(maxPay uint64) *purse.Purse {
	w := r.bw
	s := w.Signer()
	settler := &publish.RPCSettler{RPC: r.rpc}
	return &purse.Purse{Embedded: w, MaxPay: maxPay, Fees: mint.DefaultFees, Settler: settler, Asset: r.asset, Headers: r.hdrs,
		Wait: 30 * time.Second, Poll: 50 * time.Millisecond, NewPayer: func() *producer.Payer {
			tip := uint32(0)
			if h, err := r.asset.BestHeader(r.ctx); err == nil {
				tip = h.Height
			}
			return &producer.Payer{Pool: w.Pool, Tip: tip, Keys: map[string]*bwallet.Signer{s.IdentityHex(): s},
				Settler: settler, Asset: r.asset, Fees: mint.DefaultFees}
		}}
}

func (r *rig) incumbent() *Incumbent {
	return &Incumbent{URL: r.fake.Base, Identity: hex.EncodeToString(r.fake.Identity()), Lookup: []string{r.lookup.URL},
		CA: r.cert, MaxSatsPerGiBDay: 100000, MinPayment: 5000, MaxSatsPerDay: 1_000_000, AdWaitSeconds: 30}
}

// backend is a bridge backend over the rig, its state under dir, reading
// the front's copy from objs.
func (r *rig) backend(dir string, icfg *Incumbent, objs map[[32]byte][]byte) *Backend {
	r.t.Helper()
	hc, err := icfg.httpClient()
	if err != nil {
		r.t.Fatal(err)
	}
	inc := &uhrp.Client{Base: icfg.URL, HTTP: hc, Purse: r.purse, Identity: icfg.IdentityKey()}
	b, err := newBackend(dir, icfg, inc, r.asset, r.hdrs, r.logf, time.Now)
	if err != nil {
		r.t.Fatal(err)
	}
	b.poll = 50 * time.Millisecond
	b.source = func(h [32]byte) ([]byte, error) {
		if d, ok := objs[h]; ok {
			return d, nil
		}
		return nil, errors.New("not here")
	}
	return b
}

func content(n int, seed byte) []byte {
	out := make([]byte, 0, n+32)
	h := sha256.Sum256([]byte{seed})
	for len(out) < n {
		out = append(out, h[:]...)
		h = sha256.Sum256(h[:])
	}
	return out[:n]
}

// put builds content's closure and puts every object.
func put(t *testing.T, b *Backend, data []byte) (*storerec.Built, map[[32]byte][]byte) {
	t.Helper()
	built, err := storerec.Build(data)
	if err != nil {
		t.Fatal(err)
	}
	objs := map[[32]byte][]byte{}
	for _, o := range built.Objects() {
		h := sha256.Sum256(o)
		objs[h] = o
		if err := b.Put(h, o); err != nil {
			t.Fatal(err)
		}
	}
	return built, objs
}

func TestBackendCommitServeRenewAndLose(t *testing.T) {
	r := newRig(t)
	ctx := r.ctx
	dir := filepath.Join(t.TempDir(), "bridge")
	objs := map[[32]byte][]byte{}
	b := r.backend(dir, r.incumbent(), objs)
	data := content(2*storerec.BlobSize+7000, 1)
	built, o := put(t, b, data)
	for k, v := range o {
		objs[k] = v
	}
	root := built.RootHash

	// Put keeps nothing; refuses bytes that are not the hash's.
	if err := b.Put(sha256.Sum256([]byte("x")), []byte("y")); !errors.Is(err, backend.ErrHash) {
		t.Fatalf("put of other bytes: %v", err)
	}
	if _, err := b.Get(root, 0, 10); !errors.Is(err, backend.ErrMissing) {
		t.Fatalf("get before the closure is handed: %v", err)
	}

	until := uint64(time.Now().Unix()) + 86400
	ad, err := b.Advertise(ctx, root, until)
	if err != nil {
		t.Fatal(err)
	}
	if held, exp := r.fake.Holds(root); !held || exp < until || r.fake.Commits != 1 || r.fake.Paid == 0 {
		t.Fatalf("the incumbent: held %v expiry %d commits %d paid %d", held, exp, r.fake.Commits, r.fake.Paid)
	}
	a := decodeAd(t, ad)
	if !bytes.Equal(a.Host, r.fake.Identity()) || a.Hash != root || a.URL != b.ServeURL(root) || a.Expiry != ad.Expiry || ad.Expiry < until {
		t.Fatalf("advertisement %+v", a)
	}
	if b.st.Range != rangeHonoured {
		t.Fatalf("range mode after the commit: %q", b.st.Range)
	}

	// Ranged reads come from the incumbent's URL.
	blob0 := sha256.Sum256(data[:storerec.BlobSize])
	before := r.fake.Ranged
	seg, err := b.Get(blob0, 4096*5, 4096)
	if err != nil || !bytes.Equal(seg, data[4096*5:4096*6]) || r.fake.Ranged == before {
		t.Fatalf("ranged read: %v, ranged %d", err, r.fake.Ranged-before)
	}
	if !b.Has(blob0) {
		t.Fatal("has")
	}

	// Asked again for no later expiry: the live advertisement, no commit.
	if ad2, err := b.Advertise(ctx, root, until); err != nil || ad2.Txid != ad.Txid || r.fake.Commits != 1 {
		t.Fatalf("again: %v %v %d", err, ad2, r.fake.Commits)
	}

	// A later expiry: renewed through /renew, the successor spends it.
	ad3, err := b.Advertise(ctx, root, ad.Expiry+3600)
	if err != nil {
		t.Fatal(err)
	}
	if r.fake.Renewals != 1 || r.fake.Commits != 1 || ad3.Expiry < ad.Expiry+3600 {
		t.Fatalf("renewal: renewals %d commits %d expiry %d", r.fake.Renewals, r.fake.Commits, ad3.Expiry)
	}
	tx3, err := transaction.NewTransactionFromBEEF(ad3.BEEF)
	if err != nil || !spends(tx3, ad.Txid) {
		t.Fatalf("the renewal does not spend the advertisement it renews: %v", err)
	}

	// An incumbent that ignores Range: whole blobs, held to their hash.
	r.fake.SetRange(fakeuhrp.RangeIgnore)
	if seg, err := b.Get(blob0, 4096*9, 4096); err != nil || !bytes.Equal(seg, data[4096*9:4096*10]) {
		t.Fatalf("read with Range ignored: %v", err)
	}
	if b.st.Range != rangeIgnored {
		t.Fatalf("range mode: %q", b.st.Range)
	}
	// One that lies about ranges is found out by the probe.
	r.fake.SetRange(fakeuhrp.RangeLie)
	objsOf, _ := b.st.readObjects(root)
	b.probeRange(ctx, root, objsOf)
	if b.st.Range != rangeWrong {
		t.Fatalf("range mode after a lying probe: %q", b.st.Range)
	}
	if seg, err := b.Get(blob0, 4096*2, 4096); err != nil || !bytes.Equal(seg, data[4096*2:4096*3]) {
		t.Fatalf("read with lying ranges: %v", err)
	}

	// A restart reads the closure from its own record.
	b2 := r.backend(dir, r.incumbent(), nil)
	if seg, err := b2.Get(blob0, 0, 4096); err != nil || !bytes.Equal(seg, data[:4096]) {
		t.Fatalf("after a restart: %v", err)
	}

	// A blob the incumbent dropped is missing: the front marks the root.
	r.fake.Drop(blob0)
	time.Sleep(cacheTTL + 100*time.Millisecond)
	if _, err := b2.Get(blob0, 4096, 4096); !errors.Is(err, backend.ErrMissing) {
		t.Fatalf("a dropped blob: %v", err)
	}
	if b2.Has(blob0) {
		t.Fatal("has a dropped blob")
	}
}

func TestBackendPriceCapAndForeignAdvertisements(t *testing.T) {
	r := newRig(t)
	ctx := r.ctx
	objs := map[[32]byte][]byte{}
	icfg := r.incumbent()
	icfg.MaxSatsPerGiBDay, icfg.MinPayment = 1, 1
	b := r.backend(filepath.Join(t.TempDir(), "bridge"), icfg, objs)
	built, o := put(t, b, content(9000, 2))
	for k, v := range o {
		objs[k] = v
	}
	if _, err := b.Advertise(ctx, built.RootHash, uint64(time.Now().Unix())+86400); !errors.Is(err, uhrp.ErrPrice) || r.fake.Commits != 0 {
		t.Fatalf("over the cap: %v, commits %d", err, r.fake.Commits)
	}

	// Another host's advertisement of the same root, on the same lookup
	// host, is never taken for the incumbent's.
	other := r.wallet("other")
	f2, err := fakeuhrp.New(ctx, other, r.asset, &publish.RPCSettler{RPC: r.rpc}, r.hdrs, r.fake.Lookup)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(f2)
	defer srv.Close()
	f2.Base = srv.URL
	icfg2 := r.incumbent()
	b2 := r.backend(filepath.Join(t.TempDir(), "b2"), icfg2, objs)
	for h, d := range objs {
		_ = b2.Put(h, d)
	}
	// The other host's advertisement exists; the incumbent's does not.
	if _, err := f2Commit(ctx, r, f2, built, objs); err != nil {
		t.Fatal(err)
	}
	found, err := b2.liveAd(ctx, built.RootHash, uint64(len(objs[built.RootHash])))
	if err != nil || found != nil {
		t.Fatalf("a foreign advertisement was taken: %v %+v", err, found)
	}
}

func TestPaymentCap(t *testing.T) {
	i := &Incumbent{MaxSatsPerGiBDay: 1000, MinPayment: 100}
	if got := i.paymentCap(1<<30, 86400); got != 1000 {
		t.Fatalf("one GiB-day: %d", got)
	}
	if got := i.paymentCap(1, 1); got != 100 {
		t.Fatalf("the floor: %d", got)
	}
	if got := i.paymentCap(1<<30, 86400*3/2); got != 1500 {
		t.Fatalf("a day and a half: %d", got)
	}
	if got := i.paymentCap(1<<30, 366*86400); got != 366000 {
		t.Fatalf("a GiB for a year: %d", got)
	}
	if got := i.paymentCap(600<<20, 366*86400); got != 214454 {
		t.Fatalf("600 MiB for a year: %d", got)
	}
	if got := i.paymentCap(1<<62, 1<<40); got != storerec.MaxSafe {
		t.Fatalf("saturates: %d", got)
	}
}

func spends(tx *transaction.Transaction, txid string) bool {
	for _, in := range tx.Inputs {
		if in.SourceTXID != nil && in.SourceTXID.String() == txid {
			return true
		}
	}
	return false
}

func decodeAd(t *testing.T, ad *backend.Advert) *storerec.Advertisement {
	t.Helper()
	tx, err := transaction.NewTransactionFromBEEF(ad.BEEF)
	if err != nil {
		t.Fatal(err)
	}
	if tx.MerklePath == nil {
		t.Fatal("the advertisement is not mined")
	}
	a, err := storerec.DecodeAdvertisement(*tx.Outputs[ad.Vout].LockingScript)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func testCert(t *testing.T, dir string) (string, string, *x509.CertPool) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "bridge test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	cp, kp := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	_ = os.WriteFile(cp, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(kp, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	c, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(c)
	return cp, kp, pool
}

// f2Commit commits built's closure at another stand-in, through the
// bridge's wallet.
func f2Commit(ctx context.Context, r *rig, f2 *fakeuhrp.Server, built *storerec.Built, objs map[[32]byte][]byte) (*uhrp.Committed, error) {
	c := &uhrp.Client{Base: f2.Base, HTTP: &http.Client{Timeout: time.Minute}, Purse: r.purse, Identity: f2.Identity()}
	s, err := c.Open(ctx, 86400, built.Size)
	if err != nil {
		return nil, err
	}
	for h, d := range objs {
		if err := c.Stage(ctx, s, h, d); err != nil {
			return nil, err
		}
	}
	cm, _, err := c.Commit(ctx, s, built.RootHash, 1_000_000)
	return cm, err
}

func TestBackendNeverPaysTwice(t *testing.T) {
	r := newRig(t)
	ctx := r.ctx
	objs := map[[32]byte][]byte{}
	icfg := r.incumbent()
	icfg.AdWaitSeconds = 1
	b := r.backend(filepath.Join(t.TempDir(), "bridge"), icfg, objs)
	built, o := put(t, b, content(9000, 3))
	for k, v := range o {
		objs[k] = v
	}
	root := built.RootHash
	until := uint64(time.Now().Unix()) + 86400

	// The lookup host down: not knowing is not absence, nothing is paid.
	good := icfg.Lookup
	icfg.Lookup = []string{"http://127.0.0.1:1"}
	if _, err := b.Advertise(ctx, root, until); err == nil || r.fake.Paid != 0 {
		t.Fatalf("with the lookup host down: %v, paid %d", err, r.fake.Paid)
	}
	icfg.Lookup = good

	// The incumbent takes the payment and fails: it is not paid again.
	r.fake.SetFailAfterPay(true)
	if _, err := b.Advertise(ctx, root, until); err == nil || r.fake.Paid == 0 {
		t.Fatalf("a commit that failed after payment: %v, paid %d", err, r.fake.Paid)
	}
	paid := r.fake.Paid
	r.fake.SetFailAfterPay(false)
	if _, err := b.Advertise(ctx, root, until); err == nil || r.fake.Paid != paid || r.fake.Commits != 0 {
		t.Fatalf("paid again while the first payment's advertisement is awaited: %v, paid %d then %d", err, paid, r.fake.Paid)
	}

	// A daily budget the next payment would pass refuses it before it is
	// made.
	b2 := r.backend(filepath.Join(t.TempDir(), "b2"), &Incumbent{URL: icfg.URL, Identity: icfg.Identity, Lookup: icfg.Lookup, CA: icfg.CA,
		MaxSatsPerGiBDay: 100000, MinPayment: 5000, MaxSatsPerDay: 4000, AdWaitSeconds: 1}, objs)
	for h, d := range objs {
		_ = b2.Put(h, d)
	}
	before := r.fake.Paid
	if _, err := b2.Advertise(ctx, root, until); err == nil || !strings.Contains(err.Error(), "max_sats_per_day") || r.fake.Paid != before {
		t.Fatalf("over the daily budget: %v", err)
	}
}

func TestClientRefusesUnauthenticatedAnswers(t *testing.T) {
	r := newRig(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/.well-known/auth" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"uploadId":"x","stagingExpiresAt":"1"}`))
	}))
	defer srv.Close()
	c := &uhrp.Client{Base: srv.URL, HTTP: &http.Client{Timeout: 10 * time.Second}, Purse: r.purse, Identity: r.fake.Identity()}
	if _, err := c.Open(r.ctx, 60, 1); !errors.Is(err, uhrp.ErrUnauthenticated) {
		t.Fatalf("an unauthenticated answer: %v", err)
	}
	if _, err := c.Open(r.ctx, 60, 1); !errors.Is(err, uhrp.ErrUnauthenticated) {
		t.Fatalf("the client went on after an unauthenticated answer: %v", err)
	}
}
