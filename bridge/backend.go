// Package bridge is the bridge backend of bstore's spec section 17: it
// joins an incumbent UHRP host (a complete host serving BRC-167's upload
// sessions and object routes, BRC-26 advertisements and the owner's
// /renew) to the storage market without changing how the incumbent runs.
//
// bstore's provider is the market front: it receives pieces and entries,
// checks each closure, recomputes the proof root, decides, races, answers
// challenges and renewals, and keeps every segment hash. This package is
// the hold backend under it (Backend), and the rest of the bridge around
// it (Bridge): the front statement that names the bridge as the
// incumbent's front, and the forwarding of settlements that pay the
// incumbent's key.
package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"

	"github.com/lightwebinc/bcommon/chirp"
	"github.com/lightwebinc/bcommon/nodeapi"

	"github.com/lightwebinc/bstore-bridge/uhrp"
	"github.com/lightwebinc/bstore/backend"
	"github.com/lightwebinc/bstore/storerec"
)

// Range modes: what the incumbent does with an HTTP Range.
const (
	rangeUnknown  = ""
	rangeHonoured = "honoured"
	rangeIgnored  = "ignored"
	rangeWrong    = "wrong"
)

// Timing.
const (
	// cacheTTL is how long a whole blob read from the incumbent is reused
	// for the samples of one challenge that fall in it.
	cacheTTL   = 5 * time.Second
	cacheBlobs = 4
	// retentionSlack is added to a session's retention, so the incumbent's
	// lease, which starts at its commit, still reaches the expiry asked.
	retentionSlack = 600
	// rangeProbeEvery is how often what the incumbent does with Range is
	// checked again.
	rangeProbeEvery = 24 * time.Hour
)

// pendingHold is how long a commit or renewal that may have been paid for
// keeps the bridge from paying again while its advertisement is not seen.
const pendingHold = 24 * time.Hour

// Backend is the bridge backend: backend.Backend over an incumbent.
//
//   - Put takes one object of a closure the front checked: it is held to
//     its hash and length and recorded as owed to the incumbent (a root's
//     and a branch's children are read so the closure can be walked); no
//     bytes are kept.
//   - Advertise makes the closure held through until at the incumbent and
//     returns the incumbent's own advertisement, mined: one already live
//     and lasting is taken as it is; one the bridge committed is renewed
//     through the incumbent's /renew; otherwise the closure is uploaded
//     through an upload session (its bytes read from the front's copy, or
//     streamed from the incumbent's own URL when the front no longer has
//     them) and committed, the incumbent's price paid within the cap and the
//     daily budget. The advertisement is found on ls_uhrp, by the
//     incumbent's key, the root, the incumbent's own URL and the root
//     node's length, and held to the bridge's headers and its node's spend
//     evidence. Before the first advertisement of a root is returned,
//     every object of the closure is read from the incumbent's URL and held
//     to its hash. A payment that may have been made is never made again
//     while its advertisement is awaited.
//   - Get reads from the incumbent's public object route: with a Range
//     when the incumbent honours one, else the whole blob held to its hash.
//     A 404 under every root naming the object is the object missing
//     (ErrMissing); any other failure is a passing one. Nothing is answered
//     from anything but the incumbent's URL.
//   - ServeURL is the incumbent's CHIRP URL of the root.
type Backend struct {
	inc  *uhrp.Client
	icfg *Incumbent
	key  []byte

	asset *nodeapi.Asset
	hdrs  chaintracker.ChainTracker
	logf  func(string, ...any)
	now   func() time.Time
	poll  time.Duration

	// source reads an object from the front's own copy.
	source func([32]byte) ([]byte, error)

	advMu sync.Mutex // one Advertise at a time
	mu    sync.Mutex
	st    *state
	// What Put recorded since the start: lengths, and each node's
	// children and, for a root, its logical length.
	sizes    map[[32]byte]uint32
	children map[[32]byte][]chirp.Child
	lengths  map[[32]byte]uint64
	// objects maps each object of a closure the bridge adopted an
	// advertisement for to the roots naming it and its length: where Get
	// reads it.
	objects map[[32]byte][]objAt
	cache   []cached
	// started is when the backend opened its state: a commit refusal
	// recorded before it is forgotten under chirp_commit "trust".
	started time.Time
}

type objAt struct {
	root [32]byte
	size uint32
}

type cached struct {
	hash [32]byte
	data []byte
	at   time.Time
}

var (
	_ backend.Backend       = (*Backend)(nil)
	_ backend.RangeReporter = (*Backend)(nil)
)

// newBackend opens the bridge's state under dir and indexes the objects of
// every closure an advertisement was adopted for.
func newBackend(dir string, icfg *Incumbent, inc *uhrp.Client, asset *nodeapi.Asset, hdrs chaintracker.ChainTracker,
	logf func(string, ...any), now func() time.Time) (*Backend, error) {
	st, err := loadState(dir)
	if err != nil {
		return nil, err
	}
	b := &Backend{inc: inc, icfg: icfg, key: icfg.IdentityKey(), asset: asset, hdrs: hdrs, logf: logf, now: now, poll: time.Second,
		st: st, sizes: map[[32]byte]uint32{}, children: map[[32]byte][]chirp.Child{}, lengths: map[[32]byte]uint64{},
		objects: map[[32]byte][]objAt{}, started: now()}
	for rh, rec := range st.Roots {
		root, ok := parseHash(rh)
		if !ok || rec.Ad == "" {
			continue
		}
		objs, err := st.readObjects(root)
		if err != nil {
			b.logf("root %s: %v; it cannot be answered for until it is handed again", short(rh), err)
			continue
		}
		b.index(root, objs)
	}
	return b, nil
}

// index records root as naming each of objs; the caller holds b.mu or is
// the only user.
func (b *Backend) index(root [32]byte, objs []object) {
	for _, o := range objs {
		if !slices.ContainsFunc(b.objects[o.hash], func(a objAt) bool { return a.root == root }) {
			b.objects[o.hash] = append(b.objects[o.hash], objAt{root: root, size: o.size})
		}
	}
}

// roots are where an object is read: the roots naming it, those with an
// unexpired advertisement first.
func (b *Backend) roots(h [32]byte) []objAt {
	b.mu.Lock()
	defer b.mu.Unlock()
	ats := slices.Clone(b.objects[h])
	now := uint64(b.now().Unix())
	live := func(a objAt) bool {
		rec := b.st.Roots[hexOf(a.root)]
		return rec != nil && rec.Expiry > now
	}
	slices.SortStableFunc(ats, func(x, y objAt) int {
		switch lx, ly := live(x), live(y); {
		case lx && !ly:
			return -1
		case ly && !lx:
			return 1
		}
		return 0
	})
	return ats
}

// Put records one object of a checked closure as owed to the incumbent.
func (b *Backend) Put(h [32]byte, data []byte) error {
	if len(data) > uhrp.MaxObject {
		return backend.ErrTooLarge
	}
	if sha256.Sum256(data) != h {
		return backend.ErrHash
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sizes[h] = uint32(len(data))
	if len(data) >= len(chirp.Magic) && bytes.Equal(data[:len(chirp.Magic)], chirp.Magic[:]) {
		// Read as a node; whether it is one is the closure's to say, and
		// the walk descends only where a parent names a branch.
		if r, err := chirp.DecodeRoot(data); err == nil {
			b.children[h], b.lengths[h] = r.Children, r.Length
		} else if br, err := chirp.DecodeBranch(data); err == nil {
			b.children[h] = br.Children
		}
	}
	return nil
}

// Has reports whether the incumbent serves the object now, at its length,
// under some root naming it.
func (b *Backend) Has(h [32]byte) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, at := range b.roots(h) {
		if n, err := b.inc.Head(ctx, at.root, h); err == nil && n == uint64(at.size) {
			return true
		}
	}
	return false
}

// ServeURL is the incumbent's CHIRP URL of root.
func (b *Backend) ServeURL(root [32]byte) string { return b.inc.Base + storerec.CHIRPURLSuffix(root) }

// passing is a failure to read from the incumbent that says nothing of
// whether it holds the bytes: a transport error, a 5xx. Its text is kept
// and nothing of it unwraps, so no caller takes it for an end of file.
func passing(err error) error { return fmt.Errorf("the incumbent did not answer: %v", err) }

// Get reads a range of an object of a handed closure from the incumbent's
// public object route.
func (b *Backend) Get(h [32]byte, offset, length uint64) ([]byte, error) {
	ats := b.roots(h)
	if len(ats) == 0 {
		return nil, backend.ErrMissing
	}
	size := uint64(ats[0].size)
	if length == 0 || offset+length > size {
		return nil, backend.ErrRange
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var lastErr error
	for _, at := range ats {
		data, err := b.read(ctx, at.root, h, offset, length, size)
		switch {
		case err == nil:
			return data, nil
		case errors.Is(err, uhrp.ErrNotFound):
			continue
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, passing(lastErr)
	}
	return nil, backend.ErrMissing
}

// read is Get under one root.
func (b *Backend) read(ctx context.Context, root, h [32]byte, offset, length, size uint64) ([]byte, error) {
	b.mu.Lock()
	mode := b.st.Range
	b.mu.Unlock()
	whole := offset == 0 && length == size
	if !whole && mode != rangeIgnored && mode != rangeWrong {
		r, err := b.inc.Fetch(ctx, root, h, offset, length, size, true)
		if err != nil {
			return nil, err
		}
		if r.Honoured {
			return r.Data, nil
		}
		b.noteRange(rangeIgnored)
		if r.Whole != nil && sha256.Sum256(r.Whole) == h {
			b.keep(h, r.Whole)
			return append([]byte(nil), r.Whole[offset:offset+length]...), nil
		}
	}
	data, err := b.wholeObject(ctx, root, h, size)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), data[offset:offset+length]...), nil
}

// wholeObject reads an object whole from the incumbent, held to its hash,
// reusing one read a few seconds old at most.
func (b *Backend) wholeObject(ctx context.Context, root, h [32]byte, size uint64) ([]byte, error) {
	now := b.now()
	b.mu.Lock()
	b.cache = slices.DeleteFunc(b.cache, func(c cached) bool { return now.Sub(c.at) > cacheTTL })
	for _, c := range b.cache {
		if c.hash == h {
			b.mu.Unlock()
			return c.data, nil
		}
	}
	b.mu.Unlock()
	data, err := b.inc.Get(ctx, root, h, size)
	if err != nil {
		return nil, err
	}
	b.keep(h, data)
	return data, nil
}

func (b *Backend) keep(h [32]byte, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.cache) >= cacheBlobs {
		b.cache = b.cache[1:]
	}
	b.cache = append(b.cache, cached{hash: h, data: data, at: b.now()})
}

// RangeWrong is the front's word that a ranged read came back as other
// bytes than the whole object, held to its hash, has there: the incumbent's
// ranges are not used again until a probe finds them right.
func (b *Backend) RangeWrong([32]byte) { b.noteRange(rangeWrong) }

// noteRange records what the incumbent does with a Range, and says what
// it costs when it is not honoured.
func (b *Backend) noteRange(mode string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st.RangeAt = b.now().Unix()
	if b.st.Range == mode {
		_ = b.st.save()
		return
	}
	b.st.Range = mode
	_ = b.st.save()
	switch mode {
	case rangeHonoured:
		b.logf("the incumbent honours HTTP Range: a sampled segment costs one 4 KiB fetch, for the bridge and for a buyer")
	case rangeIgnored:
		b.logf("the incumbent ignores HTTP Range: a sampled segment costs its whole blob, up to 4 MiB, for the bridge and for every buyer; a challenge of c samples costs up to c x 4 MiB of reads (spec section 7.5)")
	case rangeWrong:
		b.logf("the incumbent answers HTTP Range with wrong bytes while its whole blobs are right: samples are read from whole blobs, up to 4 MiB each; a buyer rechecks each such sample from the whole blob and does not blame the answer (spec section 7.4)")
	}
}

// Advertise makes root's closure held at the incumbent through until and
// returns the incumbent's advertisement, mined.
func (b *Backend) Advertise(ctx context.Context, root [32]byte, until uint64) (*backend.Advert, error) {
	b.advMu.Lock()
	defer b.advMu.Unlock()
	rec, objs, err := b.closure(root)
	if err != nil {
		return nil, err
	}
	rootLen := uint64(objs[0].size)
	best, err := b.liveAd(ctx, root, rootLen)
	if err != nil {
		// Not knowing is not absence: nothing is paid on it.
		return nil, fmt.Errorf("the incumbent's advertisements could not be looked up: %w; tried again later", err)
	}
	if best != nil && best.Ad.Expiry >= until {
		return b.adopt(ctx, root, rec, objs, best)
	}
	b.mu.Lock()
	pendingUntil, pendingAt := rec.PendingUntil, rec.PendingAt
	b.mu.Unlock()
	if pendingAt != 0 {
		switch {
		case best != nil && best.Ad.Expiry >= pendingUntil:
			// What was paid for has appeared.
			b.clearPending(rec)
		case b.now().Sub(time.Unix(pendingAt, 0)) < pendingHold:
			// Paid, or perhaps paid: never paid again while its
			// advertisement is awaited. One look, not a wait: Advertise
			// holds advMu, and waiting here for an advertisement that may
			// never come (the incumbent refused the commit after taking
			// the payment) would stall every other root's commit and
			// renewal behind it, AdWaitSeconds at a time.
			f := best
			if f == nil || f.Ad.Expiry < min(until, pendingUntil) {
				return nil, fmt.Errorf("a commit or renewal through %d made at %d is awaited: the incumbent's advertisement through %d is not seen mined on ls_uhrp yet; looked for again later", pendingUntil, pendingAt, min(until, pendingUntil))
			}
			if f.Ad.Expiry >= until {
				return b.adopt(ctx, root, rec, objs, f)
			}
			b.clearPending(rec)
			best = f
		default:
			b.logf("root %s: no advertisement through %d appeared within %s of a commit or renewal that may have been paid; paying again", short(hexOf(root)), pendingUntil, pendingHold)
			b.clearPending(rec)
		}
	}
	if best != nil && rec.Owned {
		minutes := (until - best.Ad.Expiry + 59) / 60
		cap := b.icfg.paymentCap(rec.Length, minutes*60)
		if err := b.budget(cap); err != nil {
			return nil, err
		}
		// A renewal re-signs the advertisement's metadata, CHIRP location
		// and all: a build with the regression takes the payment and
		// refuses it as it refuses a commit.
		build, err := b.commitGate(ctx, true)
		if err != nil {
			return nil, err
		}
		b.pending(rec, until, 0)
		r, paid, err := b.inc.Renew(ctx, root, minutes, cap)
		b.paid(rec, paid)
		if err != nil && paid > 0 && (uhrp.RenewFailedAfterPayment(err) || strings.Contains(err.Error(), "ERR_OLD_ADVERTISEMENT_NOT_FOUND")) {
			b.noteBroken(root, build, paid, err)
		}
		switch {
		case err == nil:
			b.pending(rec, r.NewExpiryTime, 0)
			b.logf("root %s: renewed at the incumbent through %d (paid %d)", short(hexOf(root)), r.NewExpiryTime, paid)
			f, err := b.waitAd(ctx, root, rootLen, until)
			if err != nil {
				return nil, err
			}
			return b.adopt(ctx, root, rec, objs, f)
		case paid > 0:
			return nil, fmt.Errorf("%w (a payment of %d was sent; it is not made again while the renewal is awaited)", err, paid)
		case errors.Is(err, uhrp.ErrNotFound):
			b.clearPending(rec)
			b.mu.Lock()
			rec.Owned = false
			_ = b.st.save()
			b.mu.Unlock()
		default:
			b.clearPending(rec)
			return nil, err
		}
	}
	return b.commit(ctx, root, rec, objs, until)
}

// commit uploads the closure through an upload session and commits it.
func (b *Backend) commit(ctx context.Context, root [32]byte, rec *rootRec, objs []object, until uint64) (*backend.Advert, error) {
	now := uint64(b.now().Unix())
	retention := until - min(now, until) + retentionSlack
	cap := b.icfg.paymentCap(rec.Bytes, retention)
	if err := b.budget(cap); err != nil {
		return nil, err
	}
	build, err := b.commitGate(ctx, false)
	if err != nil {
		return nil, err
	}
	s, err := b.inc.Open(ctx, retention, rec.Length)
	if err != nil {
		return nil, err
	}
	for _, o := range objs {
		data, err := b.bytesOf(ctx, root, o)
		if err != nil {
			return nil, fmt.Errorf("object %x of the closure: %w", o.hash[:6], err)
		}
		if err := b.inc.Stage(ctx, s, o.hash, data); err != nil {
			return nil, err
		}
	}
	b.pending(rec, until, 0)
	c, paid, err := b.inc.Commit(ctx, s, root, cap)
	b.paid(rec, paid)
	if err != nil {
		if paid > 0 && uhrp.FailedAfterPayment(err) {
			b.noteBroken(root, build, paid, err)
		}
		if paid == 0 {
			b.clearPending(rec)
			return nil, err
		}
		return nil, fmt.Errorf("%w (a payment of %d was sent; it is not made again while the commit is awaited)", err, paid)
	}
	b.mu.Lock()
	rec.Owned = true
	b.mu.Unlock()
	b.pending(rec, c.ExpiryTime, 0)
	b.logf("root %s: committed at the incumbent through %d (paid %d)", short(hexOf(root)), c.ExpiryTime, paid)
	if c.HostedFileLocation != b.ServeURL(root) {
		return nil, fmt.Errorf("the incumbent serves the root at %q, not at its own CHIRP URL %q: not bridged", c.HostedFileLocation, b.ServeURL(root))
	}
	if c.ExpiryTime < until {
		return nil, fmt.Errorf("the incumbent leased the root only to %d, not %d", c.ExpiryTime, until)
	}
	f, err := b.waitAd(ctx, root, uint64(objs[0].size), until)
	if err != nil {
		return nil, err
	}
	return b.adopt(ctx, root, rec, objs, f)
}

// commitGate decides, before anything is uploaded or paid, whether the
// incumbent's build can commit a CHIRP root, and returns its fingerprint.
// The incumbent's free API description names its build: one in the range
// with the upstream commit location regression is refused under "probe";
// so, under either setting, is a build that already refused a paid commit
// that way (under "trust", only a refusal since the bridge started).
func (b *Backend) commitGate(ctx context.Context, renew bool) (string, error) {
	trust := b.icfg.CHIRPCommit == "trust"
	bld, err := b.inc.Build(ctx)
	if err != nil {
		// A renewal is not held up by a description that cannot be read:
		// a lease left to lapse costs more than one refused renewal.
		if !trust && !renew {
			return "", fmt.Errorf("the incumbent's build could not be checked (%v): nothing is paid until it is; tried again later", err)
		}
		bld = &uhrp.Build{}
	}
	b.mu.Lock()
	br := b.st.CommitBroken
	if br != nil && (br.Build != bld.Fingerprint || time.Unix(br.At, 0).Before(b.started)) {
		// Another build, or a refusal from before the operator restarted
		// the bridge: the refusal code is the incumbent's catch-all, so a
		// transient failure on a clean build latches too, and a restart is
		// the operator's word that it may be tried again. A build the
		// description names as affected is still refused below.
		b.st.CommitBroken = nil
		_ = b.st.save()
		br = nil
	}
	b.mu.Unlock()
	if br != nil {
		return "", fmt.Errorf("%w: the incumbent took a payment at %d and refused it (%s) and its build is unchanged; nothing is paid for a commit or renewal until it is upgraded or the bridge restarted (a build in the affected range: patched, with incumbent.chirp_commit set to trust)",
			ErrIncumbentBuild, br.At, br.Why)
	}
	if bld.Affected && !trust {
		return "", fmt.Errorf("%w: its API description (%s) is of ts-stack b3155fa2 or later, whose CHIRP commit fails after payment (server log %q); nothing is paid. Run a build with the fix and set incumbent.chirp_commit to trust",
			ErrIncumbentBuild, uhrp.OpenAPIPath, uhrp.LocationBugText)
	}
	return bld.Fingerprint, nil
}

// noteBroken records a payment the incumbent took and then refused the
// commit or renewal with its catch-all refusal: nothing is paid for a
// commit or renewal again while its build is the same and the bridge has
// not restarted.
func (b *Backend) noteBroken(root [32]byte, build string, paid uint64, err error) {
	b.mu.Lock()
	b.st.CommitBroken = &commitBroken{Build: build, At: b.now().Unix(), Why: err.Error()}
	serr := b.st.save()
	b.mu.Unlock()
	if serr != nil {
		b.logf("root %s: the refusal below could not be saved, and is held only until the bridge restarts: %v", short(hexOf(root)), serr)
	}
	b.logf("root %s: the incumbent took a payment (%d) and refused it (%v), as the CHIRP commit location regression does; nothing is paid for a commit or renewal again while its build is the same and the bridge is not restarted (docs/bridge.md, The commit build check)", short(hexOf(root)), paid, err)
}

// ErrIncumbentBuild is a commit refused before payment because the
// incumbent's build cannot commit a CHIRP root.
var ErrIncumbentBuild = errors.New("the incumbent's build has the CHIRP commit location regression")

// pending records a commit or renewal about to be, or just, made: its
// expiry, and when; a later call pays nothing more while it is awaited.
func (b *Backend) pending(rec *rootRec, until, paid uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if rec.PendingAt == 0 || until > rec.PendingUntil {
		rec.PendingUntil = until
	}
	if rec.PendingAt == 0 {
		rec.PendingAt = b.now().Unix()
	}
	_ = b.st.save()
}

func (b *Backend) clearPending(rec *rootRec) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rec.PendingUntil, rec.PendingAt = 0, 0
	_ = b.st.save()
}

// paid counts a payment to the incumbent.
func (b *Backend) paid(rec *rootRec, sats uint64) {
	if sats == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.st.Paid += sats
	b.st.Spends = append(b.st.Spends, spend{At: b.now().Unix(), Sats: sats})
	_ = b.st.save()
}

// budget refuses a payment of up to sats that would take the last day's
// payments to the incumbent past MaxSatsPerDay.
func (b *Backend) budget(sats uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	cut := b.now().Add(-24 * time.Hour).Unix()
	b.st.Spends = slices.DeleteFunc(b.st.Spends, func(s spend) bool { return s.At < cut })
	var day uint64
	for _, s := range b.st.Spends {
		day += s.Sats
	}
	if day+sats > b.icfg.MaxSatsPerDay {
		return fmt.Errorf("a payment of up to %d would take the last day's payments to the incumbent (%d) past max_sats_per_day (%d); tried again later",
			sats, day, b.icfg.MaxSatsPerDay)
	}
	return nil
}

// bytesOf is one object's bytes for an upload: from the front's copy, or,
// when the front has dropped it, from the incumbent's own URL; held to the
// object's hash either way.
func (b *Backend) bytesOf(ctx context.Context, root [32]byte, o object) ([]byte, error) {
	if b.source != nil {
		if data, err := b.source(o.hash); err == nil && sha256.Sum256(data) == o.hash {
			return data, nil
		}
	}
	data, err := b.inc.Get(ctx, root, o.hash, uint64(o.size))
	if err != nil {
		return nil, fmt.Errorf("neither the front nor the incumbent has it: %w", err)
	}
	return data, nil
}

// verifyServed holds the incumbent to serving every object of the closure
// under root: each read whole and held to its hash, one at a time. It then
// checks what the incumbent does with a Range, when that is not known or
// was checked a day ago.
func (b *Backend) verifyServed(ctx context.Context, root [32]byte, objs []object) error {
	for _, o := range objs {
		if _, err := b.inc.Get(ctx, root, o.hash, uint64(o.size)); err != nil {
			return fmt.Errorf("the incumbent does not serve object %x of the closure: %w", o.hash[:6], err)
		}
	}
	b.mu.Lock()
	due := b.st.Range == rangeUnknown || b.now().Sub(time.Unix(b.st.RangeAt, 0)) > rangeProbeEvery
	b.mu.Unlock()
	if due {
		b.probeRange(ctx, root, objs)
	}
	return nil
}

// probeRange asks the incumbent for a range of the closure's largest
// object and compares it with the same range of the whole object, held to
// its hash.
func (b *Backend) probeRange(ctx context.Context, root [32]byte, objs []object) {
	var o object
	for _, x := range objs {
		if x.size > o.size {
			o = x
		}
	}
	if o.size < 2 {
		return
	}
	whole, err := b.inc.Get(ctx, root, o.hash, uint64(o.size))
	if err != nil {
		return
	}
	n := min(uint64(storerec.SegmentSize), uint64(o.size)-1)
	off := uint64(o.size) - n
	r, err := b.inc.Fetch(ctx, root, o.hash, off, n, uint64(o.size), true)
	switch {
	case err != nil:
		return
	case !r.Honoured:
		b.noteRange(rangeIgnored)
	case !bytes.Equal(r.Data, whole[off:off+n]):
		b.noteRange(rangeWrong)
	default:
		b.noteRange(rangeHonoured)
	}
}

// found is an incumbent advertisement held to the bridge's headers.
type found = uhrp.Found

// liveAd is the incumbent's live advertisement of root with the latest
// expiry (ties to the lower txid): found on ls_uhrp by the incumbent's key,
// the root, the incumbent's own URL and the root node's length, unexpired,
// mined with a proof that verifies in the bridge's headers, and unspent by
// the node's positive evidence. None is nil; an error is not knowing.
func (b *Backend) liveAd(ctx context.Context, root [32]byte, rootLen uint64) (*found, error) {
	fs, err := b.inc.Find(ctx, b.icfg.Lookup, root, b.key, b.ServeURL(root))
	if err != nil {
		return nil, err
	}
	now := uint64(b.now().Unix())
	var best *found
	for i := range fs {
		f := &fs[i]
		if f.Ad.Expiry <= now || f.Ad.FileSize != rootLen {
			continue
		}
		mp, _, err := b.asset.Proof(ctx, f.Txid)
		if err != nil {
			continue
		}
		vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		ok, err := mp.Verify(vctx, f.Tx.TxID(), b.hdrs)
		cancel()
		if err != nil || !ok {
			continue
		}
		by, err := b.asset.Spender(ctx, f.Txid, f.Vout)
		if err != nil || by != "" {
			continue
		}
		f.Tx.MerklePath = mp
		if best == nil || f.Ad.Expiry > best.Ad.Expiry || (f.Ad.Expiry == best.Ad.Expiry && f.Txid < best.Txid) {
			best = f
		}
	}
	return best, nil
}

// waitAd looks for the incumbent's advertisement of root lasting through
// until, for AdWaitSeconds at most.
func (b *Backend) waitAd(ctx context.Context, root [32]byte, rootLen, until uint64) (*found, error) {
	end := b.now().Add(time.Duration(b.icfg.AdWaitSeconds) * time.Second)
	for {
		f, _ := b.liveAd(ctx, root, rootLen)
		if f != nil && f.Ad.Expiry >= until {
			return f, nil
		}
		if b.now().After(end) {
			return nil, fmt.Errorf("the incumbent's advertisement through %d is not seen mined on ls_uhrp yet; looked for again later", until)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(b.poll):
		}
	}
}

// adopt records an advertisement as the root's and indexes the closure's
// objects where Get reads them. The first time, every object is read from
// the incumbent and held to its hash before anything is returned: the
// front drops its own copy once this returns.
func (b *Backend) adopt(ctx context.Context, root [32]byte, rec *rootRec, objs []object, f *found) (*backend.Advert, error) {
	b.mu.Lock()
	first := rec.Ad == ""
	b.mu.Unlock()
	if first {
		if err := b.verifyServed(ctx, root, objs); err != nil {
			return nil, err
		}
	}
	beef, err := aloneBEEF(f.Tx)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	rec.Ad, rec.Expiry = f.Txid, f.Ad.Expiry
	if f.Ad.Expiry >= rec.PendingUntil {
		rec.PendingUntil, rec.PendingAt = 0, 0
	}
	if err := b.st.save(); err != nil {
		return nil, err
	}
	b.index(root, objs)
	for _, o := range objs {
		delete(b.sizes, o.hash)
		delete(b.children, o.hash)
		delete(b.lengths, o.hash)
	}
	return &backend.Advert{Txid: f.Txid, Vout: f.Vout, Expiry: f.Ad.Expiry, BEEF: beef}, nil
}

// closure is root's record and its objects in closure order (the root
// first): the saved list, or, the first time, the walk of what Put
// recorded, descending only into children a node names as branches,
// saved.
func (b *Backend) closure(root [32]byte) (*rootRec, []object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rh := hexOf(root)
	if rec := b.st.Roots[rh]; rec != nil {
		if objs, err := b.st.readObjects(root); err == nil && len(objs) > 0 && objs[0].hash == root {
			return rec, objs, nil
		}
	}
	length, ok := b.lengths[root]
	if !ok {
		return nil, nil, errors.New("the root was not put: the closure is unknown to the bridge")
	}
	var objs []object
	seen := map[[32]byte]bool{}
	var walk func(h [32]byte, node bool, depth int) error
	walk = func(h [32]byte, node bool, depth int) error {
		if seen[h] {
			return nil
		}
		if depth > chirp.MaxDepth || len(objs) > chirp.DefaultMaxReferences+1 {
			return errors.New("the closure is over the bounds")
		}
		size, ok := b.sizes[h]
		if !ok {
			return fmt.Errorf("object %x of the closure was not put", h[:6])
		}
		seen[h] = true
		objs = append(objs, object{hash: h, size: size})
		if !node {
			return nil
		}
		for _, c := range b.children[h] {
			if err := walk(c.Hash, c.Kind == chirp.ChildBranch, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(root, true, 1); err != nil {
		return nil, nil, err
	}
	var total uint64
	for _, o := range objs {
		total += uint64(o.size)
	}
	if err := b.st.writeObjects(root, objs); err != nil {
		return nil, nil, err
	}
	rec := b.st.Roots[rh]
	if rec == nil {
		rec = &rootRec{}
		b.st.Roots[rh] = rec
	}
	rec.Length, rec.Bytes = length, total
	if err := b.st.save(); err != nil {
		return nil, nil, err
	}
	return rec, objs, nil
}

// aloneBEEF is a mined transaction with its proof, as a host admits a
// mined object.
func aloneBEEF(tx *transaction.Transaction) ([]byte, error) {
	if tx.MerklePath == nil {
		return nil, errors.New("not mined")
	}
	alone, err := transaction.NewTransactionFromBytes(tx.Bytes())
	if err != nil {
		return nil, err
	}
	alone.MerklePath = tx.MerklePath
	return alone.BEEF()
}
