// Package fakeuhrp is a stand-in incumbent for tests: a small UHRP host
// that speaks the HTTP shape of ts-stack's uhrp-server-basic as its
// published descriptions give it (the BRC-167 CHIRP Complete Host API
// document it serves, its README and its documentation page): BRC-104
// mutual authentication on the upload-session routes and /renew, BRC-105
// payment on the commit and the renewal, a public object route with HTTP
// Range, BRC-26 advertisements under its own key minted on a commit and
// renewed by spending them, published to an ls_uhrp lookup host. Anything
// it is not asked to model it answers as that host does an unknown route:
// 404 ERR_ROUTE_NOT_FOUND, after authentication.
//
// It is written from those descriptions and imports nothing of the
// incumbent. Nothing here is for real use.
package fakeuhrp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/auth/utils"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/lightwebinc/bcommon/bwallet"
	"github.com/lightwebinc/bcommon/chirp"
	"github.com/lightwebinc/bcommon/mint"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/producer"
	"github.com/lightwebinc/bcommon/publish"
	"github.com/lightwebinc/bcommon/purse"

	"github.com/lightwebinc/bstore/auth104"
	"github.com/lightwebinc/bstore/storerec"
)

// Profile is the stand-in's wallet profile.
var Profile = bwallet.Profile{FundProtocol: storerec.Protocol, FundKeyID: storerec.KeyFund, FundBasket: "bstore fund",
	Version: "fakeuhrp", LegacyPoolFile: "fakeuhrp-pool.json"}

// RangeMode is what the stand-in does with an HTTP Range on its object
// route.
type RangeMode int

const (
	// RangeHonour answers 206 with the range, as Express's static server
	// does.
	RangeHonour RangeMode = iota
	// RangeIgnore answers 200 with the whole object.
	RangeIgnore
	// RangeLie answers 206 with the right Content-Range and other bytes.
	RangeLie
)

// Server is the stand-in incumbent.
type Server struct {
	W       *bwallet.Embedded
	Base    string // its public URL, set once it listens
	Asset   *nodeapi.Asset
	Settler publish.Settler
	Headers chaintracker.ChainTracker
	Lookup  *Lookup
	// PricePerGiBDay and MinPrice price a commit and a renewal.
	PricePerGiBDay, MinPrice uint64

	auth *auth104.Server

	mu       sync.Mutex
	sessions map[string]*session
	store    map[[32]byte][]byte
	roots    map[[32]byte]*holding
	dropped  map[[32]byte]bool
	rng      RangeMode
	// FailAfterPay takes a commit's payment and then answers 500, as a
	// host that failed after charging does.
	FailAfterPay bool
	// Paid is every satoshi taken, Commits and Renewals what was done.
	Paid               uint64
	Commits, Renewals  int
	RangeAsked, Ranged int
}

type session struct {
	owner     string
	retention uint64
	length    *uint64
	expires   time.Time
	objects   map[[32]byte]bool
}

type holding struct {
	owner   string
	objects map[[32]byte]bool
	size    uint64
	rootLen uint64
	ad      *transaction.Transaction // live advertisement, mined
	expiry  uint64
}

// New is a stand-in over w on a chain.
func New(ctx context.Context, w *bwallet.Embedded, asset *nodeapi.Asset, settler publish.Settler, headers chaintracker.ChainTracker, lookup *Lookup) (*Server, error) {
	a, err := auth104.New(ctx, auth104.Config{Wallet: w, Handshakes: auth104.Budget{Rate: 1000, Burst: 1000},
		PerAddress: auth104.Budget{Rate: 1000, Burst: 1000}, Responses: auth104.Budget{Rate: 1000, Burst: 1000}})
	if err != nil {
		return nil, err
	}
	return &Server{W: w, Asset: asset, Settler: settler, Headers: headers, Lookup: lookup, PricePerGiBDay: 200, MinPrice: 500,
		auth: a, sessions: map[string]*session{}, store: map[[32]byte][]byte{}, roots: map[[32]byte]*holding{}, dropped: map[[32]byte]bool{}}, nil
}

// Identity is the stand-in's identity key.
func (s *Server) Identity() []byte { return s.W.Signer().Identity.Compressed() }

// SetRange sets what the object route does with a Range.
func (s *Server) SetRange(m RangeMode) {
	s.mu.Lock()
	s.rng = m
	s.mu.Unlock()
}

// Drop makes an object gone: every route answers 404 for it, as a host
// whose file was deleted does.
func (s *Server) Drop(h [32]byte) {
	s.mu.Lock()
	s.dropped[h] = true
	s.mu.Unlock()
}

// SetFailAfterPay sets FailAfterPay.
func (s *Server) SetFailAfterPay(v bool) {
	s.mu.Lock()
	s.FailAfterPay = v
	s.mu.Unlock()
}

// Holds reports whether a root is committed, and its advertisement's
// expiry.
func (s *Server) Holds(root [32]byte) (bool, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := s.roots[root]
	if h == nil {
		return false, 0
	}
	return true, h.expiry
}

// Stored reports whether the stand-in holds an object's bytes.
func (s *Server) Stored(h [32]byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.store[h]
	return ok && !s.dropped[h]
}

// ServeHTTP is the stand-in's routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/.well-known/auth":
		s.auth.Handshake(w, r)
	case strings.HasPrefix(p, "/chirp/v1/uploads"):
		s.auth.Serve(w, r, 5<<20, s.uploads)
	case strings.HasPrefix(p, "/chirp/v1/") && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		s.object(w, r)
	case p == "/renew" && r.Method == http.MethodPost:
		s.auth.Serve(w, r, 256<<10, s.renew)
	default:
		s.auth.Serve(w, r, 256<<10, func(context.Context, *http.Request, []byte, *auth104.Caller) auth104.Reply {
			return auth104.Error(http.StatusNotFound, "ERR_ROUTE_NOT_FOUND", "Route not found.")
		})
	}
}

func (s *Server) uploads(ctx context.Context, r *http.Request, body []byte, who *auth104.Caller) auth104.Reply {
	if who == nil {
		return auth104.Error(http.StatusUnauthorized, "ERR_UNAUTHORIZED", "Mutual authentication required.")
	}
	rest := strings.TrimPrefix(r.URL.Path, "/chirp/v1/uploads")
	switch {
	case rest == "" && r.Method == http.MethodPost:
		var q struct {
			RetentionSeconds string  `json:"retentionSeconds"`
			LogicalLength    *string `json:"logicalLength"`
		}
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&q); err != nil {
			return auth104.Error(http.StatusBadRequest, "ERR_INVALID_SESSION", "Invalid session request")
		}
		ret, err := strconv.ParseUint(q.RetentionSeconds, 10, 64)
		if err != nil || ret == 0 {
			return auth104.Error(http.StatusBadRequest, "ERR_INVALID_SESSION", "Invalid session request")
		}
		var length *uint64
		if q.LogicalLength != nil {
			l, err := strconv.ParseUint(*q.LogicalLength, 10, 64)
			if err != nil {
				return auth104.Error(http.StatusBadRequest, "ERR_INVALID_SESSION", "Invalid session request")
			}
			length = &l
		}
		var id [16]byte
		_, _ = rand.Read(id[:])
		exp := time.Now().Add(time.Hour)
		s.mu.Lock()
		s.sessions[hex.EncodeToString(id[:])] = &session{owner: who.Hex, retention: ret, length: length, expires: exp, objects: map[[32]byte]bool{}}
		s.mu.Unlock()
		return auth104.JSON(http.StatusCreated, map[string]string{"uploadId": hex.EncodeToString(id[:]), "stagingExpiresAt": strconv.FormatInt(exp.Unix(), 10)})
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	s.mu.Lock()
	var sess *session
	if len(parts) >= 2 {
		sess = s.sessions[parts[0]]
	}
	s.mu.Unlock()
	if sess == nil || sess.owner != who.Hex || time.Now().After(sess.expires) {
		return auth104.Error(http.StatusNotFound, "ERR_UNKNOWN_SESSION", "Unknown or expired staging session")
	}
	switch {
	case len(parts) == 3 && parts[1] == "objects":
		obj, err := chirp.ParseIdentifier(parts[2])
		if err != nil {
			return auth104.Error(http.StatusBadRequest, "ERR_IDENTIFIER", "Identifier or digest mismatch")
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Method == http.MethodHead {
			if sess.objects[obj] {
				return auth104.Reply{Status: http.StatusOK}
			}
			return auth104.Reply{Status: http.StatusNotFound}
		}
		if r.Method != http.MethodPut {
			return auth104.Error(http.StatusMethodNotAllowed, "ERR_METHOD", "method not allowed")
		}
		if len(body) > 4<<20 {
			return auth104.Error(http.StatusRequestEntityTooLarge, "ERR_TOO_LARGE", "Object exceeds the v1 limit")
		}
		if sha256.Sum256(body) != obj {
			return auth104.Error(http.StatusBadRequest, "ERR_DIGEST", "Identifier or digest mismatch")
		}
		if sess.objects[obj] {
			return auth104.Reply{Status: http.StatusNoContent}
		}
		sess.objects[obj] = true
		if _, ok := s.store[obj]; !ok {
			s.store[obj] = append([]byte(nil), body...)
		}
		return auth104.Reply{Status: http.StatusCreated}
	case len(parts) == 2 && parts[1] == "commit" && r.Method == http.MethodPost:
		return s.commit(ctx, r, body, who, sess)
	}
	return auth104.Error(http.StatusNotFound, "ERR_ROUTE_NOT_FOUND", "Route not found.")
}

func (s *Server) commit(ctx context.Context, r *http.Request, body []byte, who *auth104.Caller, sess *session) auth104.Reply {
	var q struct {
		RootIdentifier string `json:"rootIdentifier"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&q); err != nil {
		return auth104.Error(http.StatusBadRequest, "ERR_INVALID_COMMIT", "Invalid or incomplete closure")
	}
	root, err := chirp.ParseIdentifier(q.RootIdentifier)
	if err != nil {
		return auth104.Error(http.StatusBadRequest, "ERR_INVALID_COMMIT", "Invalid or incomplete closure")
	}
	s.mu.Lock()
	fetch := func(h [32]byte) ([]byte, error) {
		if b, ok := s.store[h]; ok && !s.dropped[h] && (sess.objects[h] || s.inCommitted(h)) {
			return b, nil
		}
		return nil, errors.New("not staged")
	}
	c, err := chirp.Verify(root, fetch, chirp.Limits{MaxLength: 1 << 43})
	s.mu.Unlock()
	if err != nil || (sess.length != nil && c.Root.Length != *sess.length) {
		return auth104.Error(http.StatusBadRequest, "ERR_INVALID_CLOSURE", "Invalid or incomplete closure")
	}
	var size uint64
	s.mu.Lock()
	for _, o := range c.Objects {
		size += uint64(len(s.store[o]))
	}
	rootLen := uint64(len(s.store[root]))
	s.mu.Unlock()
	price := s.price(size, sess.retention)
	if rep := s.pay(ctx, r, who, price); rep != nil {
		return *rep
	}
	s.mu.Lock()
	fail := s.FailAfterPay
	s.mu.Unlock()
	if fail {
		return auth104.Error(http.StatusInternalServerError, "ERR_INTERNAL", "Internal error.")
	}
	expiry := uint64(time.Now().Unix()) + sess.retention
	ad, err := s.advertise(ctx, root, expiry, rootLen, nil)
	if err != nil {
		return auth104.Error(http.StatusInternalServerError, "ERR_ADVERTISE", err.Error())
	}
	objs := map[[32]byte]bool{}
	for _, o := range c.Objects {
		objs[o] = true
	}
	s.mu.Lock()
	s.roots[root] = &holding{owner: who.Hex, objects: objs, size: size, rootLen: rootLen, ad: ad, expiry: expiry}
	s.Commits++
	s.mu.Unlock()
	id := chirp.Identifier(root)
	return auth104.JSON(http.StatusCreated, map[string]any{"chirpURL": "chirp://" + id, "uhrpURL": "uhrp://" + id,
		"hostedFileLocation": s.Base + "/chirp/v1/" + id + "/objects/" + id, "expiryTime": expiry})
}

// inCommitted: an object of a root already committed (the store is
// deduplicated across sessions). The caller holds s.mu.
func (s *Server) inCommitted(h [32]byte) bool {
	for _, x := range s.roots {
		if x.objects[h] {
			return true
		}
	}
	return false
}

func (s *Server) renew(ctx context.Context, r *http.Request, body []byte, who *auth104.Caller) auth104.Reply {
	if who == nil {
		return auth104.Error(http.StatusUnauthorized, "ERR_UNAUTHORIZED", "Mutual authentication required.")
	}
	var q struct {
		UHRPURL           string `json:"uhrpUrl"`
		AdditionalMinutes uint64 `json:"additionalMinutes"`
	}
	if err := json.Unmarshal(body, &q); err != nil || q.UHRPURL == "" || q.AdditionalMinutes == 0 {
		return auth104.Error(http.StatusBadRequest, "ERR_MISSING_FIELDS", "Missing uhrpUrl or additionalMinutes.")
	}
	root, err := chirp.ParseIdentifier(strings.TrimPrefix(q.UHRPURL, "uhrp://"))
	if err != nil {
		return auth104.Error(http.StatusBadRequest, "ERR_INVALID_UHRP_URL", "The UHRP URL is invalid.")
	}
	s.mu.Lock()
	h := s.roots[root]
	var prev *transaction.Transaction
	var prevExp, size, rootLen uint64
	if h != nil && h.owner == who.Hex {
		prev, prevExp, size, rootLen = h.ad, h.expiry, h.size, h.rootLen
	}
	s.mu.Unlock()
	if prev == nil {
		return auth104.Error(http.StatusNotFound, "ERR_OLD_ADVERTISEMENT_NOT_FOUND", "Couldn't find an authenticated old advertisement for "+q.UHRPURL)
	}
	if uint64(time.Now().Unix()) > prevExp {
		return auth104.Error(http.StatusGone, "ERR_ADVERTISEMENT_EXPIRED", "The advertisement for "+q.UHRPURL+" has expired")
	}
	if rep := s.pay(ctx, r, who, s.price(size, q.AdditionalMinutes*60)); rep != nil {
		return *rep
	}
	expiry := prevExp + q.AdditionalMinutes*60
	ad, err := s.advertise(ctx, root, expiry, rootLen, prev)
	if err != nil {
		return auth104.Error(http.StatusInternalServerError, "ERR_ADVERTISE", err.Error())
	}
	s.mu.Lock()
	h.ad, h.expiry = ad, expiry
	s.Renewals++
	s.mu.Unlock()
	return auth104.JSON(http.StatusOK, map[string]any{"status": "success", "prevExpiryTime": prevExp, "newExpiryTime": expiry})
}

// price is PricePerGiBDay for each GiB a day, rounded up, at least
// MinPrice.
func (s *Server) price(size, seconds uint64) uint64 {
	n := new(big.Int).SetUint64(s.PricePerGiBDay)
	n.Mul(n, new(big.Int).SetUint64(size))
	n.Mul(n, new(big.Int).SetUint64(seconds))
	d := new(big.Int).SetUint64((1 << 30) * 86400)
	n.Add(n, new(big.Int).Sub(d, big.NewInt(1)))
	n.Div(n, d)
	return max(s.MinPrice, n.Uint64())
}

// pay is BRC-105: 402 with a derivation prefix the stand-in issued, or the
// payment checked (output 0 pays the key BRC-29 derives for it, at least
// the price, verifying in its headers) and broadcast. A reply is returned
// when the route must answer with it.
func (s *Server) pay(ctx context.Context, r *http.Request, who *auth104.Caller, price uint64) *auth104.Reply {
	raw := r.Header.Get("x-bsv-payment")
	if raw == "" {
		prefix, err := utils.CreateNonce(ctx, s.W, wallet.Counterparty{Type: wallet.CounterpartyTypeSelf})
		if err != nil {
			rep := auth104.Error(http.StatusInternalServerError, "ERR_INTERNAL", "no derivation prefix")
			return &rep
		}
		rep := auth104.JSON(http.StatusPaymentRequired, map[string]any{"status": "error", "code": "ERR_PAYMENT_REQUIRED",
			"satoshisRequired": price, "description": "A BSV payment is required to complete this request."},
			"x-bsv-payment-version", "1.0", "x-bsv-payment-satoshis-required", strconv.FormatUint(price, 10),
			"x-bsv-payment-derivation-prefix", prefix)
		return &rep
	}
	bad := func(code, d string) *auth104.Reply {
		rep := auth104.Error(http.StatusBadRequest, code, d)
		return &rep
	}
	var pm struct {
		DerivationPrefix string `json:"derivationPrefix"`
		DerivationSuffix string `json:"derivationSuffix"`
		Transaction      string `json:"transaction"`
	}
	if err := json.Unmarshal([]byte(raw), &pm); err != nil {
		return bad("ERR_MALFORMED_PAYMENT", "malformed payment")
	}
	if ok, err := utils.VerifyNonce(ctx, pm.DerivationPrefix, s.W, wallet.Counterparty{Type: wallet.CounterpartyTypeSelf}); err != nil || !ok {
		return bad("ERR_INVALID_DERIVATION_PREFIX", "invalid derivation prefix")
	}
	beef, err := base64.StdEncoding.DecodeString(pm.Transaction)
	if err != nil {
		return bad("ERR_MALFORMED_PAYMENT", "malformed payment")
	}
	rem, err := purse.Remittance(pm.DerivationPrefix, pm.DerivationSuffix, who.Hex)
	if err != nil {
		return bad("ERR_MALFORMED_PAYMENT", "malformed payment")
	}
	pu := &purse.Purse{Embedded: s.W, Settler: s.Settler, Asset: s.Asset, Headers: s.Headers, Fees: mint.DefaultFees}
	in, err := pu.Check(ctx, wallet.InternalizeActionArgs{Tx: beef, Outputs: []wallet.InternalizeOutput{{OutputIndex: 0,
		Protocol: wallet.InternalizeProtocolWalletPayment, PaymentRemittance: rem}}})
	if err != nil || in.Tx.Outputs[0].Satoshis < price {
		return bad("ERR_PAYMENT_INVALID", fmt.Sprintf("the payment does not pay %d satoshis to this host", price))
	}
	for _, a := range purse.Unmined(in.Tx) {
		_ = s.Settler.Submit(ctx, a)
	}
	if err := s.Settler.Submit(ctx, in.Tx); err != nil && !strings.Contains(strings.ToLower(err.Error()), "already") {
		return bad("ERR_PAYMENT_FAILED", "the payment was not taken by the network")
	}
	s.mu.Lock()
	s.Paid += in.Tx.Outputs[0].Satoshis
	s.mu.Unlock()
	return nil
}

// advertise mints the stand-in's advertisement of root through expiry,
// spending prev when it renews one, mines it and publishes it to the
// lookup host.
func (s *Server) advertise(ctx context.Context, root [32]byte, expiry, rootLen uint64, prev *transaction.Transaction) (*transaction.Transaction, error) {
	id := chirp.Identifier(root)
	url := s.Base + "/chirp/v1/" + id + "/objects/" + id
	signer := s.W.Signer()
	host := signer.Identity.Compressed()
	fields := [][]byte{host, append([]byte(nil), root[:]...), []byte(url), storerec.CompactSize(expiry), storerec.CompactSize(rootLen)}
	lock, err := storerec.AdDerivation.Lock(ctx, s.W, "fakeuhrp", fields, true)
	if err != nil {
		return nil, err
	}
	tip, err := s.Asset.BestHeader(ctx)
	if err != nil {
		return nil, err
	}
	pay := &producer.Payer{Pool: s.W.Pool, Tip: tip.Height, Keys: map[string]*bwallet.Signer{signer.IdentityHex(): signer},
		Settler: s.Settler, Asset: s.Asset, Fees: mint.DefaultFees}
	fee, err := pay.TakeAtLeast(ctx, mint.DefaultFees.Floor*4)
	if err != nil {
		return nil, err
	}
	change, _ := signer.FundScript()
	var prevIn *mint.Input
	if prev != nil {
		prevIn = &mint.Input{Tx: prev, Vout: 0, Unlocker: storerec.AdDerivation.Unlocker(ctx, s.W, "fakeuhrp")}
	}
	tx, err := mint.Transition(lock, 1, prevIn, fee, change, mint.DefaultFees)
	if err != nil {
		pay.GiveBack()
		return nil, err
	}
	if err := s.Settler.Submit(ctx, tx); err != nil {
		pay.GiveBack()
		return nil, err
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	mp, h, err := nodeapi.WaitMined(wctx, s.Asset, tx.TxID().String(), 50*time.Millisecond)
	if err != nil {
		return nil, err
	}
	pay.Change(tx, h, mp)
	tx.MerklePath = mp
	if s.Lookup != nil {
		if prev != nil {
			s.Lookup.Remove(prev.TxID().String())
		}
		s.Lookup.Add(root, tx, 0)
	}
	return tx, nil
}

// object is BRC-167's complete-host route: an object of an unexpired
// committed closure, with Range as the stand-in is set to answer it.
func (s *Server) object(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/chirp/v1/"), "/")
	if len(parts) != 3 || parts[1] != "objects" {
		http.NotFound(w, r)
		return
	}
	root, e1 := chirp.ParseIdentifier(parts[0])
	obj, e2 := chirp.ParseIdentifier(parts[2])
	s.mu.Lock()
	h := s.roots[root]
	data, ok := s.store[obj]
	ok = ok && e1 == nil && e2 == nil && h != nil && h.objects[obj] && !s.dropped[obj] && uint64(time.Now().Unix()) <= h.expiry
	mode := s.rng
	ranged := r.Header.Get("Range") != ""
	if ranged {
		s.RangeAsked++
	}
	s.mu.Unlock()
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":"error","code":"ERR_NOT_FOUND","description":"Root is unavailable or object is outside its closure"}`))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	switch {
	case !ranged || mode == RangeIgnore:
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write(data)
		}
	case mode == RangeLie:
		var a, b int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &a, &b); err != nil || a > b || b >= len(data) {
			http.Error(w, "range", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		out := make([]byte, b-a+1)
		for i := range out {
			out[i] = data[a+i] ^ 0x5a
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
		w.WriteHeader(http.StatusPartialContent)
		if r.Method == http.MethodGet {
			_, _ = w.Write(out)
		}
	default:
		s.mu.Lock()
		s.Ranged++
		s.mu.Unlock()
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}
}

// Lookup is a stand-in overlay host's ls_uhrp: the advertisements the
// stand-in published, by hash, a spent one removed as tm_uhrp does.
type Lookup struct {
	mu  sync.Mutex
	ads map[[32]byte][]lookupAd
}

type lookupAd struct {
	txid string
	beef []byte
	vout uint32
}

// Add publishes an advertisement of root.
func (l *Lookup) Add(root [32]byte, tx *transaction.Transaction, vout uint32) {
	beef, err := tx.BEEF()
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ads == nil {
		l.ads = map[[32]byte][]lookupAd{}
	}
	l.ads[root] = append(l.ads[root], lookupAd{txid: tx.TxID().String(), beef: beef, vout: vout})
}

// Remove drops a spent advertisement.
func (l *Lookup) Remove(txid string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for r, as := range l.ads {
		var keep []lookupAd
		for _, a := range as {
			if a.txid != txid {
				keep = append(keep, a)
			}
		}
		l.ads[r] = keep
	}
}

// ServeHTTP answers POST /lookup for ls_uhrp, by uhrpUrl, with beef as an
// array of numbers as the TypeScript overlay host writes it.
func (l *Lookup) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/lookup" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	var q struct {
		Service string `json:"service"`
		Query   struct {
			UHRPURL string `json:"uhrpUrl"`
		} `json:"query"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&q); err != nil || q.Service != "ls_uhrp" {
		http.Error(w, `{"status":"error"}`, http.StatusBadRequest)
		return
	}
	root, err := chirp.ParseIdentifier(strings.TrimPrefix(q.Query.UHRPURL, "uhrp://"))
	if err != nil {
		http.Error(w, `{"status":"error"}`, http.StatusBadRequest)
		return
	}
	type out struct {
		Beef        []int  `json:"beef"`
		OutputIndex uint32 `json:"outputIndex"`
	}
	ans := struct {
		Type    string `json:"type"`
		Outputs []out  `json:"outputs"`
	}{Type: "output-list", Outputs: []out{}}
	l.mu.Lock()
	for _, a := range l.ads[root] {
		nums := make([]int, len(a.beef))
		for i, b := range a.beef {
			nums[i] = int(b)
		}
		ans.Outputs = append(ans.Outputs, out{Beef: nums, OutputIndex: a.vout})
	}
	l.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ans)
}
