package bridge

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lightwebinc/bstore-bridge/uhrp"
	"github.com/lightwebinc/bstore/provider"
)

// buildNote says what the incumbent's free API description says of its
// build, and what the bridge does about it under incumbent.chirp_commit.
func buildNote(ctx context.Context, hc *http.Client, i *Incumbent) string {
	b, err := uhrp.ProbeBuild(ctx, hc, i.URL)
	switch {
	case err != nil:
		return fmt.Sprintf("the build could not be checked (%v): commits are not paid for until it can (chirp_commit %s)", err, i.CHIRPCommit)
	case b.Affected && i.CHIRPCommit == "trust":
		return fmt.Sprintf("a build of ts-stack b3155fa2 or later (%s); chirp_commit trust: the operator states it carries the commit location fix", b.Fingerprint[:12])
	case b.Affected:
		return fmt.Sprintf("REFUSED: a build of ts-stack b3155fa2 or later (%s), whose CHIRP commit fails after payment; no commit is paid for (docs/bridge.md)", b.Fingerprint[:12])
	case !b.Served:
		return "no API description served: not a build in the affected range"
	}
	return fmt.Sprintf("a build before the affected range (%s)", b.Fingerprint[:12])
}

// Doctor is the provider's doctor for the front, then the incumbent: its
// identity, whether its URL and its lookup hosts answer, and how many
// settlements the bridge has forwarded for it. It reads only. ca, when
// set, is trusted for the front's own certificate; the incumbent's is
// trusted as the configuration's incumbent.ca says.
func Doctor(ctx context.Context, cfg *Config, ca string, w io.Writer) error {
	if err := provider.Doctor(ctx, &cfg.Provider, ca, w); err != nil {
		return err
	}
	hc, err := caClient(cfg.Incumbent.CA)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "incumbent    %s\n", cfg.Incumbent.Identity)
	fmt.Fprintf(w, "incumbent    %s: %s\n", cfg.Incumbent.URL, provider.Reach(ctx, hc, strings.TrimRight(cfg.Incumbent.URL, "/")+"/"))
	fmt.Fprintf(w, "chirp commit %s\n", buildNote(ctx, hc, &cfg.Incumbent))
	for _, l := range cfg.Incumbent.Lookup {
		fmt.Fprintf(w, "ls_uhrp      %s: %s\n", l, provider.Reach(ctx, hc, strings.TrimRight(l, "/")+"/"))
	}
	rems, err := ReadRemittances(cfg.Provider.Data)
	if err != nil {
		fmt.Fprintf(w, "forwarded    %v\n", err)
	} else {
		fmt.Fprintf(w, "forwarded    %d settlement(s) for the incumbent (bstore-bridge payouts)\n", len(rems))
	}
	return nil
}

func caClient(ca string) (*http.Client, error) {
	hc := &http.Client{Timeout: 10 * time.Second}
	if ca == "" {
		return hc, nil
	}
	pem, err := os.ReadFile(ca)
	if err != nil {
		return nil, err
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("doctor: %s holds no certificate", ca)
	}
	hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return hc, nil
}
