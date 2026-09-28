//go:build productmetrics_testhook

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/gchome"
	"github.com/gastownhall/gascity/internal/productmetrics"
)

const (
	taggedProductMetricsEndpointEnvironment = "GC_PRODUCT_METRICS_TESTHOOK_ENDPOINT"
	taggedProductMetricsCAFileEnvironment   = "GC_PRODUCT_METRICS_TESTHOOK_CA_FILE"
	taggedProductMetricsMaximumCABytes      = 64 * 1024
	taggedProductMetricsReleaseVersion      = "0.31.0"
	// taggedProductMetricsStateLockWait is the real ceiling on the tagged
	// binary's state-lock wait, matching the house per-process test deadline.
	taggedProductMetricsStateLockWait = 10 * time.Second
)

func configuredPrivateProductMetricsRunner() privateProductMetricsRunFunc {
	return runProductMetricsTaggedChild
}

func runProductMetricsTaggedChild(ctx context.Context, invocation productmetrics.PrivateUploaderInvocation) error {
	if os.Getenv(taggedProductMetricsEndpointEnvironment) == "" {
		return runProductionProductMetricsChild(ctx, invocation)
	}
	return runProductMetricsTesthookChild(ctx, invocation)
}

func runProductMetricsTesthookChild(ctx context.Context, invocation productmetrics.PrivateUploaderInvocation) error {
	service, err := openProductMetricsTesthookService()
	if err != nil {
		return err
	}
	return service.RunPrivateUploader(ctx, invocation)
}

func configuredProductMetricsControlService() (*productmetrics.Service, error) {
	if os.Getenv(taggedProductMetricsEndpointEnvironment) == "" {
		return openProductionProductMetricsService()
	}
	return openProductMetricsTesthookService()
}

func openProductMetricsTesthookService() (*productmetrics.Service, error) {
	endpoint := os.Getenv(taggedProductMetricsEndpointEnvironment)
	if err := validateProductMetricsTesthookEndpoint(endpoint); err != nil {
		return nil, err
	}
	certificatePEM, err := readProductMetricsTesthookCA(os.Getenv(taggedProductMetricsCAFileEnvironment))
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificatePEM) {
		return nil, errors.New("product metrics testhook CA file has no certificate")
	}
	options := productMetricsTesthookOptions(endpoint, roots)
	options.Home = gchome.ResolveReadOnly()
	return productmetrics.OpenTesthook(options)
}

// productMetricsTesthookOptions builds the tagged service's options with one
// clock model. The clock is frozen so scheduler time spent inside RecordOnce's
// production 50 ms best-effort decision window cannot break the process
// contract, and the state-lock wait follows that frozen clock: RecordOnce
// hands its lock a deadline of the window's remaining budget, which a frozen
// clock always reports as the full 50 ms, and the production builder turns
// that into a live 50 ms wall-clock timer. On a loaded runner, opening,
// validating and flocking state.lock can take longer than that, the record
// drops, and "gc help" leaves the queue empty (pl-7sm). The lock wait here is
// instead bounded by a fixed real ceiling, so a lock that is genuinely stuck
// still drops the record rather than hanging the child.
func productMetricsTesthookOptions(endpoint string, roots *x509.CertPool) productmetrics.TesthookOptions {
	now := time.Now()
	return productmetrics.TesthookOptions{
		ReleaseVersion: taggedProductMetricsReleaseVersion,
		MetricsEpoch:   1,
		NoticeVersion:  1,
		NoticeText:     []byte("Gas City product metrics test-only notice."),
		Endpoint:       endpoint,
		Now:            func() time.Time { return now },
		WithDeadline: func(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, taggedProductMetricsStateLockWait)
		},
		Client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    roots,
		}}},
	}
}

func validateProductMetricsTesthookEndpoint(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" {
		return errors.New("product metrics testhook endpoint is invalid")
	}
	host := parsed.Hostname()
	address := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (address == nil || !address.IsLoopback()) {
		return errors.New("product metrics testhook endpoint is not loopback")
	}
	return nil
}

func readProductMetricsTesthookCA(path string) (contents []byte, returnErr error) {
	if path == "" {
		return nil, errors.New("product metrics testhook CA file is absent")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open product metrics testhook CA file: %w", err)
	}
	defer func() { returnErr = errors.Join(returnErr, file.Close()) }()
	contents, err = io.ReadAll(io.LimitReader(file, taggedProductMetricsMaximumCABytes+1))
	if err != nil {
		return nil, fmt.Errorf("read product metrics testhook CA file: %w", err)
	}
	if len(contents) == 0 || len(contents) > taggedProductMetricsMaximumCABytes {
		return nil, errors.New("product metrics testhook CA file has invalid size")
	}
	return contents, nil
}
