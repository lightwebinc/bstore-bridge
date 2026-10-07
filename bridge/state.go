package bridge

import (
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// state is what the bridge keeps between runs, beside the front's own, in
// <data>/bridge/: state.json, written whole and renamed, and for each root
// it handed, roots/<root hex>: its closure's objects in order, each a
// 32-byte hash and its 4-byte big-endian length. Never the bytes.
type state struct {
	dir string

	// Roots are the closures handed to the incumbent, by root hex.
	Roots map[string]*rootRec `json:"roots"`
	// Range is what the incumbent does with an HTTP Range: "" not known
	// yet, "honoured", "ignored" (it answers whole objects), or "wrong" (a
	// ranged answer held other bytes than the whole object's range).
	Range   string `json:"range,omitempty"`
	RangeAt int64  `json:"rangeAt,omitempty"`
	// Statements are the front statements published, by bucket.
	Statements map[string]*statementRec `json:"statements"`
	// Paid is every satoshi paid to the incumbent, commits and renewals;
	// Spends are the last day's payments, for the daily budget.
	// CommitBroken is a paid commit the incumbent refused with the CHIRP
	// commit location regression: no commit is paid for again while the
	// incumbent's build is the same (incumbent.chirp_commit).
	CommitBroken *commitBroken `json:"commitBroken,omitempty"`
	Paid         uint64        `json:"paid"`
	Spends       []spend       `json:"spends,omitempty"`
}

// spend is one payment to the incumbent.
type spend struct {
	At   int64  `json:"at"`
	Sats uint64 `json:"sats"`
}

// rootRec is one closure handed to the incumbent.
type rootRec struct {
	// Length is the root's logical length; Bytes what its distinct
	// objects take.
	Length uint64 `json:"length"`
	Bytes  uint64 `json:"bytes"`
	// Owned once the bridge committed it: the incumbent's owner-only
	// renewal is the bridge's to make.
	Owned bool `json:"owned,omitempty"`
	// Pending is a commit or renewal made whose advertisement has not been
	// seen yet: its expiry, and when it was made.
	PendingUntil uint64 `json:"pendingUntil,omitempty"`
	PendingAt    int64  `json:"pendingAt,omitempty"`
	// Ad is the advertisement last returned (display txid), and its
	// expiry.
	Ad     string `json:"ad,omitempty"`
	Expiry uint64 `json:"expiry,omitempty"`
}

// commitBroken is a commit refusal that names the build it came from.
type commitBroken struct {
	Build string `json:"build"` // the API description's fingerprint; empty when none was read
	At    int64  `json:"at"`
	Why   string `json:"why"`
}

// statementRec is a front statement published for a bucket: its carrier,
// kept so that a host missing it is sent the same one again before a new
// one is minted.
type statementRec struct {
	Txid  string `json:"txid"`
	At    int64  `json:"at"`
	Beef  string `json:"beef"`
	Tries int    `json:"tries,omitempty"`
}

func loadState(dir string) (*state, error) {
	if err := os.MkdirAll(filepath.Join(dir, "roots"), 0o700); err != nil {
		return nil, err
	}
	st := &state{dir: dir}
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(raw, st); err != nil {
			return nil, fmt.Errorf("bridge state: %w", err)
		}
		st.dir = dir
	}
	if st.Roots == nil {
		st.Roots = map[string]*rootRec{}
	}
	if st.Statements == nil {
		st.Statements = map[string]*statementRec{}
	}
	return st, nil
}

func (st *state) save() error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(st.dir, "state.json"), raw)
}

// object is one object of a closure: its hash and length.
type object struct {
	hash [32]byte
	size uint32
}

func (st *state) objectsPath(root [32]byte) string {
	return filepath.Join(st.dir, "roots", hex.EncodeToString(root[:]))
}

func (st *state) writeObjects(root [32]byte, objs []object) error {
	buf := make([]byte, 0, 36*len(objs))
	for _, o := range objs {
		buf = append(buf, o.hash[:]...)
		buf = binary.BigEndian.AppendUint32(buf, o.size)
	}
	return writeFileAtomic(st.objectsPath(root), buf)
}

func (st *state) readObjects(root [32]byte) ([]object, error) {
	raw, err := os.ReadFile(st.objectsPath(root))
	if err != nil {
		return nil, err
	}
	if len(raw)%36 != 0 {
		return nil, errors.New("bridge state: a closure's object list is torn")
	}
	out := make([]object, 0, len(raw)/36)
	for i := 0; i < len(raw); i += 36 {
		out = append(out, object{hash: [32]byte(raw[i : i+32]), size: binary.BigEndian.Uint32(raw[i+32 : i+36])})
	}
	return out, nil
}

func writeFileAtomic(p string, raw []byte) error {
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
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
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(p))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
