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

	"github.com/lightwebinc/bstore/provider"
)

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
