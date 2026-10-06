package main

// The bridge end to end, on a reference overlay host loading the bstore
// host module and a local test chain (bcommon testchain), with a native
// bstore-provider, the bstore command line as the bucket's owner and a
// writer, and a stand-in incumbent (internal/fakeuhrp) that speaks the
// uhrp-server-basic HTTP shape, joined to the market by bstore-bridge:
//
//	BSTORE_BRIDGE_E2E_REFHOST=/path/to/reference-host BSTORE_BRIDGE_E2E_NODE=/path/to/node24 \
//	BSTORE_BRIDGE_E2E_MODULE=/path/to/bstore-module.js go test -run TestBridgeE2E -v ./cmd/bstore-bridge/
//
// make e2e runs it. It needs Docker for the host's MySQL. The writer puts
// an object of seven blobs with a bounty of t = 2, K = 2, its pieces
// delivered to each provider's submit route as the plane delivers them;
// the bridge receives them, checks the closure, uploads it to the
// incumbent through an upload session, pays the commit, and mirrors the
// incumbent's own advertisement onto the bucket's records topic beside its
// front statement; the writer's race challenges the native provider at its
// own front and the incumbent at the bridge's (by reference, fetched from
// the incumbent's URL with Range), both pass and are settled by rank; the
// settlement is forwarded for the incumbent, whose operator's claim finds
// the output paying its key; the owner anchors and reads the object back;
// a renewal bought at the bridge passes through to the incumbent's /renew;
// and a blob the incumbent drops is caught by a challenge through the
// bridge.

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
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	authhttp "github.com/bsv-blockchain/go-sdk/auth/clients/authhttp"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/headers"
	"github.com/lightwebinc/bcommon/lookup"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/purse"
	"github.com/lightwebinc/bcommon/testchain"

	"github.com/lightwebinc/bstore-bridge/bridge"
	"github.com/lightwebinc/bstore-bridge/internal/fakeuhrp"
	"github.com/lightwebinc/bstore/storerec"
)

type rig struct {
	t      *testing.T
	ctx    context.Context
	dir    string
	cs     *httptest.Server
	client *http.Client
	cert   string
	key    string
	host   string
	bin    string // bstore
	pbin   string // bstore-provider
	bbin   string // bstore-bridge
	asset  *nodeapi.Asset
	rpc    *nodeapi.RPC
	hdrs   *headers.Client
}

func TestBridgeE2E(t *testing.T) {
	refhost, node, module := os.Getenv("BSTORE_BRIDGE_E2E_REFHOST"), os.Getenv("BSTORE_BRIDGE_E2E_NODE"), os.Getenv("BSTORE_BRIDGE_E2E_MODULE")
	if refhost == "" || node == "" || module == "" {
		t.Skip("set BSTORE_BRIDGE_E2E_REFHOST, BSTORE_BRIDGE_E2E_NODE and BSTORE_BRIDGE_E2E_MODULE (make e2e)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	e := &rig{t: t, ctx: ctx, dir: t.TempDir()}
	if d := os.Getenv("BSTORE_BRIDGE_E2E_LOGS"); d != "" {
		e.dir = d
		_ = os.MkdirAll(d, 0o700)
	}
	e.cs = httptest.NewServer(testchain.New(700))
	defer e.cs.Close()
	e.asset, e.rpc = &nodeapi.Asset{Base: e.cs.URL}, &nodeapi.RPC{URL: e.cs.URL + "/rpc", ID: "e2e"}
	var err error
	if e.hdrs, err = headers.NewSource(e.cs.URL); err != nil {
		t.Fatal(err)
	}
	var pool *x509.CertPool
	e.cert, e.key, pool = writeCert(t, e.dir)
	e.client = &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	e.bin, e.pbin, e.bbin = filepath.Join(e.dir, "bstore"), filepath.Join(e.dir, "bstore-provider"), filepath.Join(e.dir, "bstore-bridge")
	for _, b := range [][2]string{{e.bbin, "."}, {e.pbin, "github.com/lightwebinc/bstore/cmd/bstore-provider"}, {e.bin, "github.com/lightwebinc/bstore/cmd/bstore"}} {
		build := exec.Command("go", "build", "-o", b[0], b[1])
		build.Env = append(os.Environ(), "GOWORK=off")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", b[1], err, out)
		}
	}

	// The stand-in incumbent, its ls_uhrp host, and its own funds.
	incDir := filepath.Join(e.dir, "incumbent")
	incW, err := bwallet.Create(incDir, fakeuhrp.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := bwallet.FundFromCoinbase(ctx, incW.Signer(), incW.Pool, e.rpc, e.asset, 104, 0); err != nil {
		t.Fatal(err)
	}
	look := &fakeuhrp.Lookup{}
	lookSrv := httptest.NewServer(look)
	defer lookSrv.Close()
	inc, err := fakeuhrp.New(ctx, incW, e.asset, &publish.RPCSettler{RPC: e.rpc}, e.hdrs, look)
	if err != nil {
		t.Fatal(err)
	}
	incSrv := httptest.NewUnstartedServer(inc)
	c, _ := tls.LoadX509KeyPair(e.cert, e.key)
	incSrv.TLS = &tls.Config{Certificates: []tls.Certificate{c}}
	incSrv.StartTLS()
	defer incSrv.Close()
	inc.Base = incSrv.URL
	incKey := hex.EncodeToString(inc.Identity())
	e.ok("the stand-in incumbent %s at %s, its ls_uhrp host at %s", incKey[:12], inc.Base, lookSrv.URL)

	hostPort := freePort(t)
	e.host = fmt.Sprintf("http://127.0.0.1:%d", hostPort)
	portA, portB := freePort(t), freePort(t)
	directA, directB := fmt.Sprintf("https://127.0.0.1:%d", portA), fmt.Sprintf("https://127.0.0.1:%d", portB)

	// The owner and a writer; the writer delivers pieces to each
	// provider's submit route, as the plane does.
	owner, writer := filepath.Join(e.dir, "owner"), filepath.Join(e.dir, "writer")
	common := []string{"-network", "regtest", "-node", e.cs.URL, "-headers", e.cs.URL, "-hosts", e.host, "-ca", e.cert, "-fetch-private"}
	e.must(owner, 0, append([]string{"init", "-settle", "rpc:" + e.cs.URL + "/rpc"}, common...)...)
	wout := e.must(writer, 0, append([]string{"init", "-settle", "rpc:" + e.cs.URL + "/rpc", "-providers", directA + "," + directB}, common...)...)
	writerKey := field(wout, "identity")
	e.must(owner, 0, "fund", "-blocks", "104")
	e.must(writer, 0, "fund", "-blocks", "104")
	out := e.must(owner, 3, "bucket", "new", "photos", "-writers", writerKey)
	bucket, ownerKey, id := field(out, "bucket"), field(out, "owner"), field(out, "id")
	if bucket == "" || id == "" {
		e.fail("bucket new: %s", out)
	}
	e.startHostAt(refhost, node, module, []string{bucket}, []string{bucket + ":" + ownerKey + ":" + id}, hostPort)
	e.must(owner, 0, "sync")
	e.must(writer, 0, "bucket", "add", bucket, "-owner", ownerKey, "-id", id)
	e.ok("bucket %s on the reference host; owner and writer funded", bucket)

	// A native provider, and the bridge.
	pa := e.provider("a", portA, bucket)
	e.run(e.pbin, "fund", "-config", pa.config, "-blocks", "104")
	pa.start(e, pa.direct+"/healthz")
	br := e.bridge(portB, bucket, inc.Base, incKey, lookSrv.URL)
	e.run(e.bbin, "fund", "-config", br.config, "-blocks", "104")
	br.start(e, br.direct+"/healthz")
	bridgeFront := br.direct + "/front/bstore/v1"
	e.eventually("the bridge's front statement is on the host", 2*time.Minute, func() bool {
		return e.frontStatement(bucket, incKey, br.identity, bridgeFront)
	})
	e.ok("a native provider %s and the bridge %s run; the host answers the bridge's front statement for the incumbent", pa.identity[:12], br.identity[:12])

	// The put: seven blobs, t = 2, K = 2, pieces to both providers.
	content := make([]byte, 6*storerec.BlobSize+123457)
	h := sha256.Sum256([]byte("bstore bridge e2e"))
	for i := 0; i < len(content); i += 32 {
		copy(content[i:], h[:])
		h = sha256.Sum256(h[:])
	}
	built, err := storerec.Build(content)
	if err != nil {
		t.Fatal(err)
	}
	root := built.RootHash
	bigPath := filepath.Join(e.dir, "big.bin")
	if err := os.WriteFile(bigPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	out = e.must(writer, 0, "put", "photos/big.bin", bigPath, "-bounty", "20000", "-t", "2", "-k", "2", "-retention", "86400",
		"-deadline", "600", "-check-c", "30")
	entry := field(out, "entry")
	if entry == "" {
		e.fail("put: %s", out)
	}
	e.ok("put %d bytes (7 blobs): entry %s, pieces to both providers' submit routes", len(content), entry[:12])

	// The bridge receives, the incumbent stores and advertises.
	e.eventually("the incumbent holds the closure", 5*time.Minute, func() bool {
		held, _ := inc.Holds(root)
		if !held {
			return false
		}
		for _, o := range built.Objects() {
			if !inc.Stored(sha256.Sum256(o)) {
				return false
			}
		}
		return true
	})
	e.eventually("the bridge adopted the incumbent's advertisement", time.Minute, func() bool { return br.logged("adopted") })
	if inc.Commits != 1 || inc.Paid == 0 {
		e.fail("the incumbent: %d commits, %d paid", inc.Commits, inc.Paid)
	}
	if n := countFiles(t, filepath.Join(br.data, "store", "objects")); n != 0 {
		e.fail("the bridge kept %d object(s) of the handed closure", n)
	}
	e.eventually("the incumbent's advertisement is on the bucket's records topic", time.Minute, func() bool {
		return e.adOnHost(bucket, root, incKey)
	})
	e.ok("the bridge uploaded the closure by upload session (paid %d), kept no bytes, and mirrored the incumbent's advertisement to the host", inc.Paid)

	// The race: both pass, the incumbent through the bridge's front.
	out = e.must(writer, 0, "holds", "-watch", "8m")
	race := readRace(t, writer, entry)
	passed := map[string]string{}
	for _, cand := range race.Cands {
		if cand.Status == "passed" {
			passed[cand.Advertiser] = cand.Front
		}
	}
	if passed[pa.identity] == "" || passed[incKey] != bridgeFront {
		e.fail("the race: %s\n%+v", out, passed)
	}
	e.ok("the race: the native provider passed at its own front, the incumbent at the bridge's (by reference)")

	// Settlement: by rank, the incumbent's share to the incumbent's key.
	out, _ = e.cli(writer, "settle", "-entry", entry)
	race = readRace(t, writer, entry)
	if race.State != "settled" || race.Settlement == nil || len(race.Settlement.Payees) != 2 {
		e.fail("settle: %s", out)
	}
	var incShare uint64
	for _, p := range race.Settlement.Payees {
		if p.Advertiser == incKey {
			incShare = p.Satoshis
		}
	}
	if incShare == 0 {
		e.fail("no payee is the incumbent: %+v", race.Settlement.Payees)
	}
	e.eventually("the native provider takes its payout", 2*time.Minute, func() bool { return pa.logged("pays rank") })
	e.eventually("the bridge forwards the settlement", 3*time.Minute, func() bool {
		return strings.Contains(e.run(e.bbin, "payouts", "-config", br.config), race.Settlement.Txid)
	})
	keyFile := filepath.Join(e.dir, "incumbent.key")
	raw, _ := os.ReadFile(filepath.Join(incDir, "identity.json"))
	var idj struct {
		WIF string `json:"wif"`
	}
	_ = json.Unmarshal(raw, &idj)
	_ = os.WriteFile(keyFile, []byte(idj.WIF+"\n"), 0o600)
	claimOut := e.run(e.bbin, "claim", "-remittances", filepath.Join(br.data, bridge.RemittancesFile), "-key-file", keyFile)
	var cl bridge.Claimed
	if err := json.Unmarshal([]byte(strings.SplitN(claimOut, "\n", 2)[0]), &cl); err != nil || cl.Txid != race.Settlement.Txid || cl.Satoshis != incShare {
		e.fail("claim: %v\n%s", err, claimOut)
	}
	e.ok("settled by rank: the incumbent's %d satoshis pay its own key (found by its operator's claim from the forwarded settlement); the bridge took no bounty", incShare)

	// The owner anchors and reads the object back.
	e.must(owner, 0, "anchor")
	got := filepath.Join(e.dir, "got.bin")
	e.must(owner, 0, "get", "photos/big.bin", got, "-proof-root")
	if b, _ := os.ReadFile(got); !bytes.Equal(b, content) {
		e.fail("get: the bytes differ")
	}
	e.ok("the owner anchored; get read the object back, every check made")

	// A paid challenge through the bridge, by reference with Range.
	before := inc.Ranged
	out = e.must(writer, 0, "challenge", "photos/big.bin", "-provider", incKey, "-front", bridgeFront, "-answerer", br.identity,
		"-c", "30", "-budget", "1000")
	if !strings.Contains(out, "pass") || inc.Ranged < before+30 {
		e.fail("a paid challenge through the bridge: %s (ranged reads %d)", out, inc.Ranged-before)
	}
	e.ok("a paid challenge through the bridge passed; %d ranged reads at the incumbent (the bridge's and the buyer's)", inc.Ranged-before)

	// Renewal passthrough: bought at the bridge, made at the incumbent.
	st := readBridgeState(t, br.data)
	rec := st.Roots[hex.EncodeToString(root[:])]
	if rec == nil {
		e.fail("the bridge has no record of the root")
	}
	until := rec.Expiry + 86400
	buyer := e.buyer("buyer")
	body, _ := json.Marshal(map[string]any{"root": hex.EncodeToString(root[:]), "until": until})
	status, ans := buyer.post(ctx, bridgeFront+"/renew", body, 100000)
	if status != 200 {
		e.fail("renew at the bridge: %d %s", status, ans)
	}
	var rn struct {
		Advertisement string `json:"advertisement"`
		Txid          string `json:"txid"`
		ExpiryTime    uint64 `json:"expiryTime"`
	}
	_ = json.Unmarshal(ans, &rn)
	beef, _ := base64.StdEncoding.DecodeString(rn.Advertisement)
	ntx, err := transaction.NewTransactionFromBEEF(beef)
	if err != nil || rn.ExpiryTime < until || inc.Renewals != 1 || !spends(ntx, rec.Ad) {
		e.fail("the renewal: %v expiry %d renewals %d", err, rn.ExpiryTime, inc.Renewals)
	}
	if _, exp := inc.Holds(root); exp < until {
		e.fail("the incumbent's expiry %d, not %d", exp, until)
	}
	e.eventually("the renewed advertisement is on the records topic", time.Minute, func() bool {
		return e.adTxOnHost(bucket, root, rn.Txid)
	})
	e.ok("a renewal bought at the bridge passed through the incumbent's /renew: its successor %s spends the advertisement and is mirrored", rn.Txid[:12])

	// A blob the incumbent drops is caught through the bridge.
	blob := sha256.Sum256(content[3*storerec.BlobSize : 4*storerec.BlobSize])
	inc.Drop(blob)
	caught := false
	for try := 0; try < 4 && !caught; try++ {
		out, code := e.cli(writer, "challenge", "photos/big.bin", "-provider", incKey, "-front", bridgeFront, "-answerer", br.identity,
			"-c", "113", "-budget", "1000")
		switch code {
		case 0:
		case 3:
			if !strings.Contains(out, "unknown-root") {
				e.fail("the caught challenge: %s", out)
			}
			caught = true
		default:
			e.fail("challenge exit %d: %s", code, out)
		}
	}
	if !caught || !br.logged("DAMAGED") {
		e.fail("four challenges of c = 113 missed a blob the incumbent dropped")
	}
	e.ok("a blob dropped at the incumbent (1 of 7) was caught by a challenge through the bridge: it read the sample from the incumbent's URL, found it gone, and signed nothing")
	if !strings.Contains(e.must(writer, 0, "challenge", "photos/big.bin", "-provider", pa.identity, "-c", "113", "-budget", "1000"), "pass") {
		e.fail("the native provider's challenge did not pass")
	}
	e.ok("the native provider passes the same challenge")

	pa.stop()
	br.stop()
}

// --- the processes

type proc struct {
	name     string
	data     string
	config   string
	direct   string
	identity string
	cmd      *exec.Cmd
	out      *lockedBuf
	done     chan error
	args     []string
	bin      string
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
	f  *os.File
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_, _ = l.f.Write(p)
	}
	return l.b.Write(p)
}

func (p *proc) log() string {
	p.out.mu.Lock()
	defer p.out.mu.Unlock()
	return p.out.b.String()
}

func (p *proc) logged(s string) bool { return strings.Contains(p.log(), s) }

func (p *proc) start(e *rig, health string) {
	p.cmd = exec.Command(p.bin, p.args...)
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	if err := p.cmd.Start(); err != nil {
		e.t.Fatal(err)
	}
	p.done = make(chan error, 1)
	cmd := p.cmd
	go func() { p.done <- cmd.Wait() }()
	e.eventually(p.name+" listens", time.Minute, func() bool {
		res, err := e.client.Get(health)
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == 200
	})
	e.t.Cleanup(func() { _ = cmd.Process.Kill() })
}

func (p *proc) stop() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(30 * time.Second):
			_ = p.cmd.Process.Kill()
		}
	}
}

func terms() map[string]any {
	return map[string]any{"challenge_base": 10, "challenge_per_sample": 1, "free_c": 30, "min_bounty": 1000, "min_age": 2,
		"renew_per_gib_day": 100, "renew_minimum": 2000}
}

func limits() map[string]any {
	return map[string]any{"challenge_burst": 20, "handshakes_per_addr_per_sec": 100, "handshake_addr_burst": 200,
		"object_reads_per_addr_per_sec": 1000, "object_read_burst": 1000, "submits_per_addr_per_sec": 1000, "submit_burst": 2000}
}

// provider is a native provider's configuration for bucket on port.
func (e *rig) provider(name string, port int, bucket string) *proc {
	data := filepath.Join(e.dir, "provider-"+name)
	out := e.run(e.pbin, "init", "-data", data)
	id := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "identity"))
	cfg := map[string]any{
		"data": data, "listen": fmt.Sprintf("127.0.0.1:%d", port), "tls_cert": e.cert, "tls_key": e.key,
		"public_url": fmt.Sprintf("https://127.0.0.1:%d/store", port), "node": e.cs.URL, "settle": "rpc:" + e.cs.URL + "/rpc",
		"headers": e.cs.URL, "hosts": []string{e.host}, "buckets": []map[string]any{{"bucket": bucket}}, "tree_count": 8,
		"terms": terms(), "limits": limits(),
	}
	return e.writeProc("provider "+name, "provider-"+name, data, id, cfg, e.pbin, port)
}

// bridge is the bridge's configuration for bucket on port, for the
// incumbent at incURL.
func (e *rig) bridge(port int, bucket, incURL, incKey, lookupURL string) *proc {
	data := filepath.Join(e.dir, "bridge")
	out := e.run(e.bbin, "init", "-data", data)
	id := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "identity"))
	cfg := map[string]any{
		"provider": map[string]any{
			"data": data, "listen": fmt.Sprintf("127.0.0.1:%d", port), "tls_cert": e.cert, "tls_key": e.key,
			"public_url": fmt.Sprintf("https://127.0.0.1:%d/front", port), "node": e.cs.URL, "settle": "rpc:" + e.cs.URL + "/rpc",
			"headers": e.cs.URL, "hosts": []string{e.host}, "buckets": []map[string]any{{"bucket": bucket}}, "tree_count": 8,
			"backend": "bridge", "terms": terms(), "limits": limits(),
		},
		"incumbent": map[string]any{"url": incURL, "identity": incKey, "lookup": []string{lookupURL}, "ca": e.cert,
			"max_sats_per_gib_day": 100, "min_payment": 2000, "ad_wait_seconds": 120},
	}
	return e.writeProc("bridge", "bridge", data, id, cfg, e.bbin, port)
}

func (e *rig) writeProc(name, file, data, id string, cfg map[string]any, bin string, port int) *proc {
	raw, _ := json.MarshalIndent(cfg, "", "  ")
	path := filepath.Join(e.dir, file+".json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		e.t.Fatal(err)
	}
	f, _ := os.Create(filepath.Join(e.dir, file+".log"))
	return &proc{name: name, data: data, config: path, identity: id, out: &lockedBuf{f: f}, bin: bin,
		args: []string{"run", "-config", path}, direct: fmt.Sprintf("https://127.0.0.1:%d", port)}
}

// --- the command line

func (e *rig) cli(home string, args ...string) (string, int) {
	e.t.Helper()
	c := exec.CommandContext(e.ctx, e.bin, append([]string{"-home", home}, args...)...)
	out, err := c.CombinedOutput()
	code := 0
	var x *exec.ExitError
	if errors.As(err, &x) {
		code = x.ExitCode()
	} else if err != nil {
		e.fail("bstore %s: %v", strings.Join(args, " "), err)
	}
	e.t.Logf("$ bstore %s  (exit %d)\n%s", strings.Join(args, " "), code, indent(string(out)))
	return string(out), code
}

func (e *rig) must(home string, want int, args ...string) string {
	e.t.Helper()
	out, code := e.cli(home, args...)
	if code != want {
		e.fail("bstore %s: exit %d, want %d\n%s", strings.Join(args, " "), code, want, out)
	}
	return out
}

func (e *rig) run(bin string, args ...string) string {
	e.t.Helper()
	out, err := exec.CommandContext(e.ctx, bin, args...).Output()
	if err != nil {
		var x *exec.ExitError
		if errors.As(err, &x) {
			out = append(out, x.Stderr...)
		}
		e.fail("%s %s: %v\n%s", filepath.Base(bin), strings.Join(args, " "), err, out)
	}
	return string(out)
}

func indent(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	return "    " + strings.ReplaceAll(s, "\n", "\n    ")
}

func field(out, name string) string {
	m := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s+(\S+)`).FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return m[1]
}

// raceFile is what the e2e reads of the writer's race record.
type raceFile struct {
	State string `json:"state"`
	Cands []struct {
		Advertiser string `json:"advertiser"`
		Status     string `json:"status"`
		Front      string `json:"front"`
	} `json:"candidates"`
	Settlement *struct {
		Txid   string `json:"txid"`
		Payees []struct {
			Advertiser string `json:"advertiser"`
			Rank       int    `json:"rank"`
			Satoshis   uint64 `json:"satoshis"`
		} `json:"payees"`
	} `json:"settlement"`
}

func readRace(t *testing.T, home, entry string) *raceFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, "races", entry+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var r raceFile
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return &r
}

type bridgeState struct {
	Roots map[string]*struct {
		Ad     string `json:"ad"`
		Expiry uint64 `json:"expiry"`
	} `json:"roots"`
}

func readBridgeState(t *testing.T, data string) *bridgeState {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(data, "bridge", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s bridgeState
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return &s
}

func countFiles(t *testing.T, dir string) int {
	n := 0
	_ = filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info.Mode().IsRegular() {
			n++
		}
		return nil
	})
	return n
}

func spends(tx *transaction.Transaction, txid string) bool {
	for _, in := range tx.Inputs {
		if in.SourceTXID != nil && in.SourceTXID.String() == txid {
			return true
		}
	}
	return false
}

// --- the host

func (e *rig) ask(q map[string]string) []lookup.Output {
	body, _ := json.Marshal(lookup.Question{Service: storerec.LookupService, Query: q})
	res, err := http.Post(e.host+"/lookup", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var a lookup.Answer
	if json.Unmarshal(raw, &a) != nil {
		return nil
	}
	return a.Outputs
}

func (e *rig) frontStatement(bucket, adv, front, url string) bool {
	for _, o := range e.ask(map[string]string{"bucket": bucket, "advertiser": adv, "front": front}) {
		ad, err := storerec.Admit(e.ctx, o.Beef, nil, storerec.Host{Bucket: bucket, Headers: e.hdrs})
		if err == nil && ad.Carrier != nil && ad.Carrier.Front != nil && ad.Carrier.Front.URL == url {
			return true
		}
	}
	return false
}

func (e *rig) adOnHost(bucket string, root [32]byte, key string) bool {
	k, _ := hex.DecodeString(key)
	for _, o := range e.ask(map[string]string{"bucket": bucket, "root": hex.EncodeToString(root[:]), "after": ""}) {
		tx, err := transaction.NewTransactionFromBEEF(o.Beef)
		if err != nil || int(o.OutputIndex) >= len(tx.Outputs) {
			continue
		}
		if a, err := storerec.DecodeAdvertisement(*tx.Outputs[o.OutputIndex].LockingScript); err == nil && bytes.Equal(a.Host, k) && a.Hash == root {
			return true
		}
	}
	return false
}

func (e *rig) adTxOnHost(bucket string, root [32]byte, txid string) bool {
	for _, o := range e.ask(map[string]string{"bucket": bucket, "root": hex.EncodeToString(root[:]), "after": ""}) {
		if tx, err := transaction.NewTransactionFromBEEF(o.Beef); err == nil && tx.TxID().String() == txid {
			return true
		}
	}
	return false
}

// startHostAt starts the reference host on port, loading the bstore
// module bundle, over MySQL in Docker.
func (e *rig) startHostAt(refhost, node, module string, buckets, served []string, port int) {
	t := e.t
	pw := "bstore-e2e"
	id := strings.TrimSpace(runOut(t, "docker", "run", "-d", "--rm", "-e", "MYSQL_ROOT_PASSWORD="+pw, "-e", "MYSQL_DATABASE=overlay",
		"-p", "127.0.0.1::3306", "--tmpfs", "/var/lib/mysql", "mysql:9"))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	portLine := strings.Split(strings.TrimSpace(runOut(t, "docker", "port", id, "3306/tcp")), "\n")[0]
	dbPort := portLine[strings.LastIndex(portLine, ":")+1:]
	for i := 0; ; i++ {
		if exec.Command("docker", "exec", id, "mysql", "-h127.0.0.1", "-uroot", "-p"+pw, "-e", "select 1").Run() == nil {
			break
		}
		if i == 120 {
			t.Fatal("MySQL did not come up")
		}
		time.Sleep(time.Second)
	}
	stage := filepath.Join(e.dir, "host")
	_ = os.MkdirAll(filepath.Join(stage, "modules", "bstore"), 0o700)
	_ = os.MkdirAll(filepath.Join(stage, "state"), 0o700)
	_ = os.Remove(filepath.Join(stage, "node_modules"))
	if err := os.Symlink(filepath.Join(refhost, "node_modules"), filepath.Join(stage, "node_modules")); err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile(module)
	if err != nil {
		t.Fatalf("the bstore module bundle: %v", err)
	}
	mod := filepath.Join(stage, "modules", "bstore", "bstore-module.js")
	if err := os.WriteFile(mod, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	var topics []string
	for _, b := range buckets {
		topics = append(topics, storerec.RecordsTopic(b))
	}
	cmd := exec.Command(node, filepath.Join(refhost, "dist", "index.js"))
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"),
		"OVERLAY_TOPICS=" + strings.Join(topics, ","), "OVERLAY_MODULES=" + mod,
		"OVERLAY_KNEX_URL=mysql://root:" + pw + "@127.0.0.1:" + dbPort + "/overlay",
		"OVERLAY_CHAIN_TRACKER_URL=" + e.cs.URL, "OVERLAY_ADMIN_TOKEN=bstore-e2e", "OVERLAY_LISTEN=127.0.0.1",
		"OVERLAY_PORT=" + strconv.Itoa(port), "BSTORE_BUCKETS=" + strings.Join(buckets, ","), "BSTORE_STATE_DIR=" + filepath.Join(stage, "state"),
		"BSTORE_HEADERS_URL=" + e.cs.URL, "BSTORE_SERVED=" + strings.Join(served, ",")}
	f, _ := os.Create(filepath.Join(e.dir, "host.log"))
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	e.eventually("the host is ready", time.Minute, func() bool {
		res, err := http.Get(e.host + "/readyz")
		if err != nil {
			return false
		}
		res.Body.Close()
		return res.StatusCode == 200
	})
}

// --- a buyer over BRC-104

type buyer struct {
	e  *rig
	w  *bwallet.Embedded
	mu sync.Mutex
	af *authhttp.AuthFetch
	sp *swapPurse
}

type swapPurse struct{ *purse.Purse }

func (e *rig) buyer(name string) *buyer {
	w, err := bwallet.Create(filepath.Join(e.dir, name), fakeuhrp.Profile)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, _, err := bwallet.FundFromCoinbase(e.ctx, w.Signer(), w.Pool, e.rpc, e.asset, 104, 0); err != nil {
		e.t.Fatal(err)
	}
	return &buyer{e: e, w: w}
}

func (b *buyer) post(ctx context.Context, url string, body []byte, maxPay uint64) (int, []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := b.e
	s := b.w.Signer()
	settler := &publish.RPCSettler{RPC: e.rpc}
	pu := &purse.Purse{Embedded: b.w, MaxPay: maxPay, Fees: mint.DefaultFees, Settler: settler, Asset: e.asset, Headers: e.hdrs,
		Wait: time.Minute, Poll: 100 * time.Millisecond, NewPayer: func() *producer.Payer {
			tip := uint32(0)
			if h, err := e.asset.BestHeader(ctx); err == nil {
				tip = h.Height
			}
			return &producer.Payer{Pool: b.w.Pool, Tip: tip, Keys: map[string]*bwallet.Signer{s.IdentityHex(): s},
				Settler: settler, Asset: e.asset, Fees: mint.DefaultFees}
		}}
	if b.af == nil {
		b.sp = &swapPurse{}
		b.af = authhttp.New(b.sp, authhttp.WithoutLogging(), authhttp.WithHttpClient(e.client))
	}
	b.sp.Purse = pu
	res, err := b.af.Fetch(ctx, url, &authhttp.SimplifiedFetchRequestOptions{Method: http.MethodPost,
		Headers: map[string]string{"content-type": "application/json"}, Body: body})
	if err != nil {
		e.fail("post %s: %v", url, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if tx := pu.Settle(); tx != nil {
		_ = settler.Submit(ctx, tx)
	}
	return res.StatusCode, raw
}

// --- the rest

func runOut(t *testing.T, name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		t.Fatalf("%s %v: %v", name, args, err)
	}
	return string(out)
}

func (e *rig) ok(format string, args ...any) { e.t.Logf("ok  "+format, args...) }

func (e *rig) fail(format string, args ...any) {
	e.t.Helper()
	e.t.Fatalf("e2e: "+format+"\n(logs under %s)", append(args, e.dir)...)
}

func (e *rig) eventually(what string, d time.Duration, f func() bool) {
	e.t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if f() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	e.fail("%s: did not happen", what)
}

func freePort(t *testing.T) int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func writeCert(t *testing.T, dir string) (string, string, *x509.CertPool) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "bstore bridge e2e"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: true, BasicConstraintsValid: true}
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
