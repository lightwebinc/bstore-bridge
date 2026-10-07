// Package uhrp is the bridge's client of an incumbent UHRP host: the HTTP
// interfaces such a host already serves, spoken from their published
// descriptions and nothing of its code. It covers BRC-167's baseline
// upload-session routes (/chirp/v1/uploads, over BRC-104 with the commit
// paid by a BRC-105 402), the owner's /renew (BRC-104, paid), BRC-167's
// complete-host object route (/chirp/v1/<root>/objects/<object>, public,
// with HTTP Range), and the BRC-24 ls_uhrp lookup at an overlay host the
// incumbent publishes its advertisements to.
//
// Every URL the client reaches is built from the operator's configuration
// and a hash: nothing a request, an answer or an advertisement carries is
// ever fetched. Redirects are not followed and every answer is bounded.
package uhrp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	authhttp "github.com/bsv-blockchain/go-sdk/auth/clients/authhttp"
	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/lightwebinc/bcommon/chirp"
	"github.com/lightwebinc/bcommon/purse"
)

// Bounds on what the incumbent may answer.
const (
	// MaxObject is the largest CHIRP object: a 4 MiB blob.
	MaxObject = 4 << 20
	// maxJSON bounds a JSON answer.
	maxJSON = 64 << 10
)

// Errors.
var (
	// ErrNotFound is an object or root the incumbent does not serve (404,
	// or 410): what a reader of its URL would find.
	ErrNotFound = errors.New("uhrp: the incumbent does not serve it")
	// ErrPrice is a 402 whose price is over the most the bridge pays.
	ErrPrice = errors.New("uhrp: the incumbent's price is over the bridge's cap")
)

// Client is one incumbent.
type Client struct {
	// Base is the incumbent's public base URL: https, no trailing slash.
	// Its routes are Base/chirp/v1/... and Base/renew.
	Base string
	// HTTP reaches the incumbent and the lookup hosts; the client copies
	// it and refuses redirects and over-long answers on its own copy.
	HTTP *http.Client
	// Purse pays a 402 from the bridge's pool, up to maxPay.
	Purse func(maxPay uint64) *purse.Purse

	// Identity is the incumbent's identity key, 33 bytes: every
	// authenticated answer must be signed by it.
	Identity []byte

	once     sync.Once
	hc       *http.Client
	mu       sync.Mutex
	af       *authhttp.AuthFetch
	paying   *swapPurse
	verified bool
	broken   bool
}

// swapPurse is the AuthFetch's wallet: the purse of the call in flight.
type swapPurse struct{ *purse.Purse }

func (c *Client) client() *http.Client {
	c.once.Do(func() {
		hc := &http.Client{Timeout: 2 * time.Minute}
		if c.HTTP != nil {
			cp := *c.HTTP
			hc = &cp
		}
		base := hc.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		hc.Transport = limitTransport{base: base, max: MaxObject + 1<<20}
		hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		c.hc = hc
	})
	return c.hc
}

// Call is one authenticated answer, and what was paid for it.
type Call struct {
	Status int
	Body   []byte
	Paid   uint64
	// Payment is the BRC-105 payment made, when one was.
	Payment *transaction.Transaction
}

// ErrUnauthenticated is an answer the incumbent's key did not sign: the
// client refuses it, and every call after it, until restarted.
var ErrUnauthenticated = errors.New("uhrp: an answer not signed by the incumbent's identity key")

// callTimeout bounds one call, the handshake and any payment included.
const callTimeout = 3 * time.Minute

// do sends one request over BRC-104, paying a 402 up to maxPay (zero pays
// nothing). Calls are made one at a time, on one session per origin. The
// answer must be signed by the incumbent's identity key: go-sdk's AuthFetch
// falls back to a plain HTTP client (which follows redirects and uses
// none of this client's bounds) when a server does not complete the
// handshake, so an answer without the incumbent's signature ends the
// client's use of the incumbent, and a paid call is made only once an
// unpaid one has seen the incumbent's key. The Call returned with an error
// still carries what was paid.
func (c *Client) do(ctx context.Context, method, u string, headers map[string]string, body []byte, maxPay uint64) (*Call, error) {
	if c.Purse == nil {
		return nil, errors.New("uhrp: no wallet to authenticate with")
	}
	if len(c.Identity) != 33 {
		return nil, errors.New("uhrp: no incumbent identity to hold answers to")
	}
	if maxPay > 0 && !c.isVerified() {
		// An unpaid, authenticated call first: the incumbent's /renew with
		// no uhrpUrl is refused (400), signed. Not an empty object: the
		// TypeScript auth middleware reads a JSON body of {} as no body,
		// and the signature over "{}" then fails (401, unsigned).
		if _, err := c.do(ctx, http.MethodPost, c.url("/renew"), map[string]string{"content-type": "application/json"}, []byte(`{"uhrpUrl":""}`), 0); err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	pu := c.Purse(maxPay)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken {
		return nil, ErrUnauthenticated
	}
	out := &Call{}
	var res *http.Response
	var err error
	for attempt := 0; ; attempt++ {
		if c.af == nil {
			c.paying = &swapPurse{}
			c.af = authhttp.New(c.paying, authhttp.WithoutLogging(), authhttp.WithHttpClient(c.client()))
		}
		c.paying.Purse = pu
		res, err = c.af.Fetch(ctx, u, &authhttp.SimplifiedFetchRequestOptions{Method: method, Headers: headers, Body: body})
		// A payment sent may reach the incumbent, which may broadcast it
		// whatever it answered: it is counted as paid, never refunded.
		if tx := pu.Settle(); tx != nil {
			out.Payment = tx
			if len(tx.Outputs) > 0 {
				out.Paid = tx.Outputs[0].Satoshis
			}
		}
		if err == nil || !staleSession(err) {
			break
		}
		// The incumbent no longer knows the session (it forgot it idle,
		// or restarted): the next call handshakes afresh. Asked again at
		// once only when nothing was paid.
		c.af = nil
		if attempt > 0 || out.Payment != nil {
			break
		}
		pu = c.Purse(maxPay)
	}
	if err != nil {
		if errors.Is(err, purse.ErrOverMaxPay) || strings.Contains(err.Error(), "satoshis asked") {
			return out, fmt.Errorf("%w: %v", ErrPrice, err)
		}
		return out, err
	}
	defer res.Body.Close()
	if res.Header.Get("x-bsv-auth-identity-key") != hex.EncodeToString(c.Identity) {
		c.broken = true
		return out, ErrUnauthenticated
	}
	c.verified = true
	b, rerr := io.ReadAll(io.LimitReader(res.Body, maxJSON+1))
	out.Status, out.Body = res.StatusCode, b
	if out.Payment != nil && res.StatusCode < 400 && pu.Settler != nil {
		// The bridge's own payment is broadcast from here too; the
		// incumbent broadcasts the same bytes.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = pu.Settler.Submit(sctx, out.Payment)
		cancel()
	}
	if rerr != nil {
		return out, rerr
	}
	if len(b) > maxJSON {
		return out, fmt.Errorf("uhrp: an answer over %d bytes", maxJSON)
	}
	return out, nil
}

// staleSession reports a call the incumbent answered outside the session
// the client holds: an unsigned refusal of a session it no longer knows.
func staleSession(err error) bool {
	return strings.Contains(err.Error(), "server failed to authenticate")
}

func (c *Client) isVerified() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.verified
}

func (c *Client) url(path string) string { return strings.TrimRight(c.Base, "/") + path }

// ObjectURL is BRC-167's complete-host route for an object of root's
// closure at the incumbent.
func (c *Client) ObjectURL(root, obj [32]byte) string {
	return c.url("/chirp/v1/" + chirp.Identifier(root) + "/objects/" + chirp.Identifier(obj))
}

// URL is the UHRP URL of a hash, as /renew names it.
func URL(h [32]byte) string { return "uhrp://" + chirp.Identifier(h) }

// Session is an open upload session.
type Session struct {
	ID      string
	Expires uint64
}

// Open opens an upload session: retention seconds of lease once committed,
// for a root of logicalLength bytes.
func (c *Client) Open(ctx context.Context, retention, logicalLength uint64) (*Session, error) {
	body, _ := json.Marshal(map[string]any{"retentionSeconds": strconv.FormatUint(max(retention, 1), 10),
		"logicalLength": strconv.FormatUint(logicalLength, 10)})
	call, err := c.do(ctx, http.MethodPost, c.url("/chirp/v1/uploads"), map[string]string{"content-type": "application/json"}, body, 0)
	if err != nil {
		return nil, fmt.Errorf("open a session: %w", err)
	}
	if call.Status != http.StatusCreated {
		return nil, fmt.Errorf("open a session: %d %s", call.Status, firstLine(call.Body))
	}
	var s struct {
		UploadID         string `json:"uploadId"`
		StagingExpiresAt string `json:"stagingExpiresAt"`
	}
	if err := json.Unmarshal(call.Body, &s); err != nil || s.UploadID == "" || len(s.UploadID) > 512 {
		return nil, errors.New("open a session: no upload id")
	}
	exp, _ := strconv.ParseUint(s.StagingExpiresAt, 10, 64)
	return &Session{ID: s.UploadID, Expires: exp}, nil
}

func (c *Client) sessionURL(s *Session) string {
	return c.url("/chirp/v1/uploads/" + url.PathEscape(s.ID))
}

// Stage stages one object in the session; one the session already
// references is not sent again.
func (c *Client) Stage(ctx context.Context, s *Session, h [32]byte, data []byte) error {
	u := c.sessionURL(s) + "/objects/" + chirp.Identifier(h)
	if hc, err := c.do(ctx, http.MethodHead, u, nil, nil, 0); err == nil && hc.Status == http.StatusOK {
		return nil
	}
	var call *Call
	var err error
	for try := 0; try < 3; try++ {
		call, err = c.do(ctx, http.MethodPut, u, map[string]string{"content-type": "application/octet-stream"}, data, 0)
		if err == nil && call.Status < 500 && call.Status != http.StatusTooManyRequests {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(try+1) * time.Second):
		}
	}
	if err != nil {
		return fmt.Errorf("stage %x: %w", h[:6], err)
	}
	if call.Status != http.StatusCreated && call.Status != http.StatusNoContent {
		return fmt.Errorf("stage %x: %d %s", h[:6], call.Status, firstLine(call.Body))
	}
	return nil
}

// Committed is the incumbent's answer to a commit.
type Committed struct {
	ChirpURL           string `json:"chirpURL"`
	UHRPURL            string `json:"uhrpURL"`
	HostedFileLocation string `json:"hostedFileLocation"`
	ExpiryTime         uint64 `json:"expiryTime"`
	// Paid is what the commit cost the bridge.
	Paid uint64 `json:"-"`
}

// Commit validates and commits the closure of root staged in the session,
// paying the incumbent's 402 up to maxPay. The incumbent advertises the root
// under its own key once the closure checks.
func (c *Client) Commit(ctx context.Context, s *Session, root [32]byte, maxPay uint64) (*Committed, uint64, error) {
	body, _ := json.Marshal(map[string]string{"rootIdentifier": chirp.Identifier(root)})
	call, err := c.do(ctx, http.MethodPost, c.sessionURL(s)+"/commit", map[string]string{"content-type": "application/json"}, body, maxPay)
	paid := paidOf(call)
	if err != nil {
		return nil, paid, fmt.Errorf("commit: %w", err)
	}
	if call.Status != http.StatusCreated {
		return nil, paid, fmt.Errorf("commit: %d %s", call.Status, firstLine(call.Body))
	}
	var out Committed
	if err := json.Unmarshal(call.Body, &out); err != nil {
		return nil, paid, fmt.Errorf("commit: the answer does not read: %w", err)
	}
	out.Paid = paid
	return &out, paid, nil
}

func paidOf(c *Call) uint64 {
	if c == nil {
		return 0
	}
	return c.Paid
}

// Renewed is the incumbent's answer to a renewal.
type Renewed struct {
	NewExpiryTime  uint64 `json:"newExpiryTime"`
	PrevExpiryTime uint64 `json:"prevExpiryTime"`
	Paid           uint64 `json:"-"`
}

// Renew extends the incumbent's advertisement of root by minutes, paying
// its 402 up to maxPay: the owner's renewal (the identity that uploaded).
// The incumbent spends its advertisement into a later one.
func (c *Client) Renew(ctx context.Context, root [32]byte, minutes, maxPay uint64) (*Renewed, uint64, error) {
	body, _ := json.Marshal(map[string]any{"uhrpUrl": URL(root), "additionalMinutes": minutes})
	call, err := c.do(ctx, http.MethodPost, c.url("/renew"), map[string]string{"content-type": "application/json"}, body, maxPay)
	paid := paidOf(call)
	if err != nil {
		return nil, paid, fmt.Errorf("renew: %w", err)
	}
	if call.Status == http.StatusNotFound || call.Status == http.StatusGone {
		return nil, paid, fmt.Errorf("renew: %d %s: %w", call.Status, firstLine(call.Body), ErrNotFound)
	}
	if call.Status != http.StatusOK {
		return nil, paid, fmt.Errorf("renew: %d %s", call.Status, firstLine(call.Body))
	}
	var out struct {
		Status string `json:"status"`
		Renewed
	}
	if err := json.Unmarshal(call.Body, &out); err != nil || out.Status != "success" || out.NewExpiryTime == 0 {
		return nil, paid, fmt.Errorf("renew: the answer does not read: %s", firstLine(call.Body))
	}
	out.Paid = paid
	return &out.Renewed, paid, nil
}

// Ranged is what a ranged fetch returned.
type Ranged struct {
	Data []byte
	// Honoured is false when the incumbent answered the whole object (200)
	// or a range other than the one asked for.
	Honoured bool
	// Whole is the whole object when the incumbent answered 200 to a
	// ranged request, not yet held to its hash.
	Whole []byte
}

// Fetch reads length bytes at offset of object obj of root's closure from
// the incumbent's public object route, asking with an HTTP Range when
// ranged. size is the object's length. A 206 must name exactly the range
// asked for and carry exactly its bytes; a 200 is the whole object, held to
// size; anything else is not what a reader gets. ErrNotFound is a 404 or
// 410: the incumbent does not serve it.
func (c *Client) Fetch(ctx context.Context, root, obj [32]byte, offset, length, size uint64, ranged bool) (*Ranged, error) {
	if length == 0 || offset+length > size || size > MaxObject {
		return nil, errors.New("uhrp: a range outside the object")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.ObjectURL(root, obj), nil)
	if err != nil {
		return nil, err
	}
	whole := offset == 0 && length == size
	if ranged && !whole {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	}
	res, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusNotFound, http.StatusGone:
		return nil, ErrNotFound
	case http.StatusPartialContent:
		if !ranged || whole {
			return nil, errors.New("uhrp: a 206 to a request for the whole object")
		}
		want := fmt.Sprintf("bytes %d-%d/%d", offset, offset+length-1, size)
		b, err := io.ReadAll(io.LimitReader(res.Body, int64(length)+1))
		if err != nil {
			return nil, err
		}
		if res.Header.Get("Content-Range") != want || uint64(len(b)) != length {
			return &Ranged{Data: nil, Honoured: false}, nil
		}
		return &Ranged{Data: b, Honoured: true}, nil
	case http.StatusOK:
		b, err := io.ReadAll(io.LimitReader(res.Body, int64(size)+1))
		if err != nil {
			return nil, err
		}
		if uint64(len(b)) != size {
			// Not the object a reader of the URL would be given.
			return nil, ErrNotFound
		}
		r := &Ranged{Data: b[offset : offset+length], Honoured: !ranged || whole}
		if !r.Honoured {
			r.Whole = b
		}
		return r, nil
	}
	return nil, fmt.Errorf("uhrp: object %x: %d", obj[:6], res.StatusCode)
}

// Get reads a whole object from root's closure, held to its hash: bytes that
// do not hash to obj are ErrNotFound, since no reader takes them.
func (c *Client) Get(ctx context.Context, root, obj [32]byte, size uint64) ([]byte, error) {
	r, err := c.Fetch(ctx, root, obj, 0, size, size, false)
	if err != nil {
		return nil, err
	}
	if sha256.Sum256(r.Data) != obj {
		return nil, ErrNotFound
	}
	return r.Data, nil
}

// Head reports whether the incumbent serves an object of root's closure,
// and its length.
func (c *Client) Head(ctx context.Context, root, obj [32]byte) (uint64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.ObjectURL(root, obj), nil)
	if err != nil {
		return 0, err
	}
	res, err := c.client().Do(req)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		if res.ContentLength < 0 {
			return 0, errors.New("uhrp: no length")
		}
		return uint64(res.ContentLength), nil
	case http.StatusNotFound, http.StatusGone:
		return 0, ErrNotFound
	}
	return 0, fmt.Errorf("uhrp: head %x: %d", obj[:6], res.StatusCode)
}

func firstLine(b []byte) string {
	s := string(b)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// limitTransport cuts every answer's body at max bytes.
type limitTransport struct {
	base http.RoundTripper
	max  int64
}

func (t limitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := wireCertificateSet(req); err != nil {
		return nil, err
	}
	res, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if res.ContentLength > t.max {
		res.Body.Close()
		return nil, fmt.Errorf("uhrp: an answer of %d bytes, over %d", res.ContentLength, t.max)
	}
	res.Body = struct {
		io.Reader
		io.Closer
	}{io.LimitReader(res.Body, t.max), res.Body}
	return res, nil
}

// wireCertificateSet puts a BRC-103 handshake's requestedCertificates in
// the wire shape, {"certifiers": [...], "types": {...}}. go-sdk v1.5.2
// (the version bcommon pins) serialises its RequestedCertificateSet with
// Go's field names, {"Certifiers": [...], "CertificateTypes": {...}}, and
// the TypeScript SDK the incumbent runs (@bsv/sdk 3) refuses that
// handshake as malformed (400 ERR_AUTH_MALFORMED). go-sdk v1.7.1 emits the
// wire shape itself; this goes once bcommon moves to it. Only the
// unsigned initialRequest and certificateRequest to /.well-known/auth carry
// the set, and only its two key names are changed.
func wireCertificateSet(req *http.Request) error {
	if req.Method != http.MethodPost || req.URL.Path != "/.well-known/auth" || req.Body == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(req.Body, 1<<20+1))
	req.Body.Close()
	if err != nil {
		return err
	}
	if len(raw) > 1<<20 {
		return errors.New("uhrp: a handshake message over 1 MiB")
	}
	var msg map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) == nil {
		if set, ok := msg["requestedCertificates"]; ok {
			var fields map[string]json.RawMessage
			if json.Unmarshal(set, &fields) == nil {
				if v, ok := fields["Certifiers"]; ok {
					delete(fields, "Certifiers")
					if string(v) == "null" {
						v = json.RawMessage("[]")
					}
					fields["certifiers"] = v
				}
				if v, ok := fields["CertificateTypes"]; ok {
					delete(fields, "CertificateTypes")
					if string(v) == "null" {
						v = json.RawMessage("{}")
					}
					fields["types"] = v
				}
				if b, err := json.Marshal(fields); err == nil {
					msg["requestedCertificates"] = b
					if out, err := json.Marshal(msg); err == nil {
						raw = out
					}
				}
			}
		}
	}
	req.Body = io.NopCloser(bytes.NewReader(raw))
	req.ContentLength = int64(len(raw))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(raw)), nil }
	return nil
}
