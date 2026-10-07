package uhrp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The CHIRP commit location regression: since ts-stack b3155fa2, the
// uhrp-server-basic advertisement metadata check accepts only a
// /cdn/<object> location, so every CHIRP commit fails after its payment
// was taken, before anything is advertised. The server logs "UHRP hosted
// file location is invalid" and answers the client only with its generic
// commit refusal: 400, code ERR_CHIRP_COMMIT ("CHIRP closure validation or
// advertisement failed."), the code of any commit failure that is not one
// of its own named CHIRP errors.
const (
	LocationBugText   = "UHRP hosted file location is invalid"
	CommitRefusedCode = "ERR_CHIRP_COMMIT"
	// RenewRefusedCode is /renew's catch-all refusal (500), which the
	// same check answers when a renewal re-signs a CHIRP location.
	RenewRefusedCode = "ERR_INTERNAL_RENEW"
)

// RenewFailedAfterPayment reports whether err is a renewal the incumbent
// refused with its catch-all refusal.
func RenewFailedAfterPayment(err error) bool {
	var re *RenewError
	return errors.As(err, &re) && re.Status == http.StatusInternalServerError && re.Code == RenewRefusedCode
}

// FailedAfterPayment reports whether err is a commit the incumbent refused
// with its generic commit refusal: after a payment, the regression's
// answer.
func FailedAfterPayment(err error) bool {
	var ce *CommitError
	return errors.As(err, &ce) && ce.Status == http.StatusBadRequest && ce.Code == CommitRefusedCode
}

// OpenAPIPath is the incumbent's CHIRP API description, served without
// authentication or payment.
const OpenAPIPath = "/chirp/v1/openapi.json"

// maxOpenAPI bounds the API description read.
const maxOpenAPI = 1 << 20

// Build is what the incumbent's free API description says of its build.
type Build struct {
	// Served is whether it serves an API description at all.
	Served bool
	// Fingerprint is the SHA-256 of the description, hex; empty when none
	// is served. A build with another description has another fingerprint.
	Fingerprint string
	// Affected is a description of a build in the range with the CHIRP
	// commit location regression: from ts-stack b3155fa2, which added the
	// bsvAuth security scheme to the description in the same change that
	// added the location check, to the latest release.
	Affected bool
}

// ProbeBuild reads the incumbent's API description at base (no
// authentication, nothing paid) and says what build it is. A 404 is a
// build with no description, which no release in the affected range is.
// Any other failure is an error: not knowing is not a clean build.
func ProbeBuild(ctx context.Context, hc *http.Client, base string) (*Build, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+OpenAPIPath, nil)
	if err != nil {
		return nil, err
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return &Build{}, nil
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("uhrp: %s answered %d", OpenAPIPath, res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, maxOpenAPI+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxOpenAPI {
		return nil, fmt.Errorf("uhrp: %s is over %d bytes", OpenAPIPath, maxOpenAPI)
	}
	return ReadBuild(b)
}

// ReadBuild says what build an API description is of.
func ReadBuild(doc []byte) (*Build, error) {
	var d struct {
		Components struct {
			SecuritySchemes map[string]json.RawMessage `json:"securitySchemes"`
		} `json:"components"`
		Paths map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(doc, &d); err != nil {
		return nil, fmt.Errorf("uhrp: %s does not read: %w", OpenAPIPath, err)
	}
	sum := sha256.Sum256(doc)
	_, commit := d.Paths["/chirp/v1/uploads/{uploadId}/commit"]
	_, auth := d.Components.SecuritySchemes["bsvAuth"]
	return &Build{Served: true, Fingerprint: hex.EncodeToString(sum[:]), Affected: commit && auth}, nil
}

// Build probes the incumbent's build over the client's bounded HTTP client.
func (c *Client) Build(ctx context.Context) (*Build, error) {
	return ProbeBuild(ctx, c.client(), c.Base)
}
