package bridge

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/lightwebinc/bcommon/guard"

	"github.com/lightwebinc/bstore/provider"
	"github.com/lightwebinc/bstore/storerec"
)

// Config is a bridge's configuration file, JSON: the market front's own
// configuration (bstore's provider configuration, backend "bridge") and
// the incumbent it joins to the market. An unknown key is refused.
type Config struct {
	// Provider is the front: its data directory and wallet, its public URL
	// (where its front, <public_url>/bstore/v1, answers), its node, header
	// source, settlement leg, hosts, buckets, terms and limits. Its backend
	// is "bridge"; empty is taken as that.
	Provider provider.Config `json:"provider"`
	// Incumbent is the UHRP host the bridge joins to the market.
	Incumbent Incumbent `json:"incumbent"`
	// StatementCheckSeconds is how often the bridge looks for its front
	// statement at each host, and publishes it again where it is missing.
	StatementCheckSeconds uint64 `json:"statement_check_seconds,omitempty"`
}

// Incumbent names the incumbent host and bounds what the bridge pays it.
type Incumbent struct {
	// URL is the incumbent's public base URL, https, no trailing slash:
	// its upload sessions at URL/chirp/v1/uploads, its objects at
	// URL/chirp/v1/<root>/objects/<object>, its renewals at URL/renew. Its
	// advertisements must name URL/chirp/v1/<root>/objects/<root>.
	URL string `json:"url"`
	// Identity is the incumbent's identity key, 66 hex digits: the key its
	// advertisements are by, and the key bounties for them pay. Only the
	// public key: the bridge never holds the incumbent's private key.
	Identity string `json:"identity"`
	// Lookup are overlay hosts that carry tm_uhrp and answer ls_uhrp, where
	// the incumbent's advertisements are found.
	Lookup []string `json:"lookup"`
	// CA, when set, is a PEM file of certificates trusted, beside the
	// system's, for the incumbent and the lookup hosts.
	CA string `json:"ca,omitempty"`
	// MaxSatsPerGiBDay and MinPayment cap what the bridge pays the
	// incumbent for one commit or one renewal: the larger of MinPayment and
	// MaxSatsPerGiBDay for each GiB a day held, rounded up. A 402 over the
	// cap is not paid. They default to, and may not exceed, the front's
	// renew_per_gib_day and renew_minimum.
	MaxSatsPerGiBDay uint64 `json:"max_sats_per_gib_day,omitempty"`
	MinPayment       uint64 `json:"min_payment,omitempty"`
	// MaxSatsPerDay bounds everything paid to the incumbent in any 24
	// hours, commits and renewals together.
	MaxSatsPerDay uint64 `json:"max_sats_per_day,omitempty"`
	// AdWaitSeconds is how long, after a commit or a renewal, the bridge
	// looks for the incumbent's advertisement (mined) before the call
	// fails; the front tries again later.
	AdWaitSeconds uint64 `json:"ad_wait_seconds,omitempty"`
}

// LoadConfig reads and checks a configuration file.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if err := c.Check(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return &c, nil
}

// Check takes the defaults and refuses a configuration that cannot run.
func (c *Config) Check() error {
	if c.Provider.Backend == "" {
		c.Provider.Backend = "bridge"
	}
	if c.Provider.Backend != "bridge" {
		return errors.New("provider.backend: a bridge's front has backend bridge")
	}
	if err := c.Provider.Check(); err != nil {
		return fmt.Errorf("provider: %w", err)
	}
	if c.StatementCheckSeconds == 0 {
		c.StatementCheckSeconds = 6 * 3600
	}
	i := &c.Incumbent
	// By default the bridge pays the incumbent at most what it charges a
	// buyer for the same time.
	if i.MaxSatsPerGiBDay == 0 {
		i.MaxSatsPerGiBDay = c.Provider.Terms.RenewPerGiBDay
	}
	if i.MinPayment == 0 {
		i.MinPayment = c.Provider.Terms.RenewMinimum
	}
	if i.AdWaitSeconds == 0 {
		i.AdWaitSeconds = 300
	}
	if i.MaxSatsPerDay == 0 {
		i.MaxSatsPerDay = 1_000_000
	}
	u, err := url.Parse(i.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
		strings.HasSuffix(i.URL, "/") || strings.Contains(i.URL, "/chirp/v1") || strings.ContainsAny(i.URL, "?# ") {
		return fmt.Errorf("incumbent.url %q must be https://host[/path] with no user, query, fragment or trailing slash", i.URL)
	}
	k, err := guard.ParsePubKeyHex(i.Identity)
	if err != nil {
		return fmt.Errorf("incumbent.identity: %w", err)
	}
	if bytes.Equal(k.Compressed(), storerec.AnyoneKey) {
		return errors.New("incumbent.identity: the anyone key is nobody's")
	}
	if hex.EncodeToString(k.Compressed()) != i.Identity {
		return errors.New("incumbent.identity: 66 lowercase hex digits")
	}
	if len(i.Lookup) == 0 {
		return errors.New("incumbent.lookup: at least one overlay host answering ls_uhrp")
	}
	for _, h := range i.Lookup {
		if u, err := url.Parse(h); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return fmt.Errorf("incumbent.lookup: %q is not an http or https URL", h)
		}
	}
	if i.CA != "" {
		if _, err := os.ReadFile(i.CA); err != nil {
			return fmt.Errorf("incumbent.ca: %w", err)
		}
	}
	if i.MinPayment > storerec.MaxSafe || i.MaxSatsPerGiBDay > storerec.MaxSafe {
		return errors.New("incumbent: a price is above 2^53 - 1")
	}
	if c.Provider.Policy.HoldUnbountied {
		return errors.New("provider.policy.hold_unbountied: every closure a bridge takes costs it the incumbent's commit, and only a bountied entry is worth one")
	}
	if t := c.Provider.Terms; t.RenewMinimum < i.MinPayment || t.RenewPerGiBDay < i.MaxSatsPerGiBDay {
		return errors.New("provider.terms: renew_minimum and renew_per_gib_day must be at least incumbent.min_payment and incumbent.max_sats_per_gib_day, so a renewal never costs the bridge more than it is paid")
	}
	if c.Provider.PublicURL == i.URL || strings.HasPrefix(c.Provider.PublicURL+"/", i.URL+"/chirp/") {
		return errors.New("provider.public_url: the front's URL is not the incumbent's")
	}
	return nil
}

// IdentityKey is the incumbent's identity key, 33 bytes.
func (i *Incumbent) IdentityKey() []byte {
	b, _ := hex.DecodeString(i.Identity)
	return b
}

// httpClient is the client the bridge reaches the incumbent and the lookup
// hosts with: the system's roots and CA, no redirects, bounded time.
func (i *Incumbent) httpClient() (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 30 * time.Second
	if i.CA != "" {
		pem, err := os.ReadFile(i.CA)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("incumbent.ca: no certificate in the file")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &http.Client{Timeout: 2 * time.Minute, Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

// paymentCap is the most the bridge pays the incumbent to hold bytes for
// seconds: MaxSatsPerGiBDay for each GiB a day, rounded up, and never
// less than MinPayment.
func (i *Incumbent) paymentCap(bytes, seconds uint64) uint64 {
	n := new(big.Int).SetUint64(i.MaxSatsPerGiBDay)
	n.Mul(n, new(big.Int).SetUint64(bytes))
	n.Mul(n, new(big.Int).SetUint64(seconds))
	d := new(big.Int).SetUint64((1 << 30) * 86400)
	n.Add(n, new(big.Int).Sub(d, big.NewInt(1)))
	n.Div(n, d)
	if !n.IsUint64() || n.Uint64() > storerec.MaxSafe {
		return storerec.MaxSafe
	}
	return max(n.Uint64(), i.MinPayment)
}
