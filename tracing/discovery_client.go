package tracing

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/kiali/kiali/config"
	"github.com/kiali/kiali/log"
	"github.com/kiali/kiali/models"
	"github.com/kiali/kiali/tracing/jaeger"
	"github.com/kiali/kiali/tracing/jaeger/apiv3"
	"github.com/kiali/kiali/tracing/otel"
	"github.com/kiali/kiali/tracing/tempo"
	"github.com/kiali/kiali/tracing/tempo/querypb"
	"github.com/kiali/kiali/util/grpcutil"
	"github.com/kiali/kiali/util/httputil"
)

type DialFunc func(network, address string, timeout time.Duration) (net.Conn, error)

var MakeRequestFunc = otel.MakeRequest

func DiagnoseTracingConfig(ctx context.Context, conf *config.Config, token string) (*models.TracingDiagnose, error) {
	cfgTracing := conf.ExternalServices.Tracing
	test := models.TracingDiagnose{}
	logs := []models.LogLine{}
	zl := log.WithGroup(log.TracingLogName)

	// Tracing not enabled
	if !cfgTracing.Enabled {
		test.Message = "tracing is not enabled"
		return &test, nil
	}

	// Internal URL not set
	url := cfgTracing.InternalURL
	if url == "" {
		url = cfgTracing.ExternalURL
		logs = append(logs, models.LogLine{Time: time.Now(), Test: fmt.Sprintf("Using external url [%s] because not in cluster", url)})
		if url == "" {
			return nil, fmt.Errorf("external_url should not be empty (Not in cluster)")
		}
	}

	parsedURL, ll, err := parseUrl(url)
	if err != nil {
		return &test, fmt.Errorf("external_services.tracing.internal_url is required and must be a valid URL")
	}
	logs = append(logs, ll...)

	// Get Auth
	auth := cfgTracing.Auth
	if auth.UseKialiToken {
		auth.Token = config.Credential(token)
	}

	ports, ll := discoverPortsWithDial(zl, parsedURL.Host, net.DialTimeout)
	logs = append(logs, ll...)

	validConfig, ll := discoverUrl(ctx, zl, *parsedURL, url, ports, &auth, cfgTracing)
	test.ValidConfig = validConfig
	test.LogLine = append(logs, ll...)
	// Tempo's discovery is left as it was, which set neither: the outcome and its message are
	// about Jaeger's api_v3, and a Tempo diagnose payload should not change with it.
	if isJaeger(cfgTracing) {
		test.Code, test.Message = diagnoseOutcome(parsedURL.Host, ports, validConfig)
	}

	return &test, nil
}

// isJaeger reports whether the configuration is read as Jaeger, which an empty provider is too.
func isJaeger(cfgTracing config.TracingConfig) bool {
	return cfgTracing.Provider == "" || cfgTracing.Provider == config.JaegerProvider
}

// diagnoseOutcome sums up a Jaeger discovery for the Discovery tab, which colours the message by
// the code. Finding nothing is reported in the payload rather than as an error, so the tab still
// shows the log of every endpoint probed; the message says why a Jaeger that answers may still
// not count, since an older Jaeger answers the ports discovery probes but not api_v3.
func diagnoseOutcome(host string, ports []string, validConfigs []models.ValidConfig) (int, string) {
	if len(validConfigs) > 0 {
		return http.StatusOK, fmt.Sprintf("Found [%d] possible tracing configuration(s)", len(validConfigs))
	}
	probed := "none of the known tracing ports is open"
	if len(ports) > 0 {
		probed = fmt.Sprintf("open ports [%s] were probed", strings.Join(ports, ", "))
	}
	return http.StatusNotFound, fmt.Sprintf("No valid tracing configuration found on host [%s]: the configured URL did not answer as Jaeger api_v3, and %s. Jaeger is read through its api_v3 API (/api/v3 over HTTP, jaeger.api_v3.QueryService over gRPC), which requires Jaeger 1.54 or newer. Check the URL and use_grpc, and see the logs for each endpoint probed.", host, probed)
}

// Parse URL
func parseUrl(urlToParse string) (*models.ParsedUrl, []models.LogLine, error) {
	parsedURL, err := url.Parse(urlToParse)
	logLines := []models.LogLine{}
	if err != nil {
		return nil, logLines, fmt.Errorf("cannot parse url: %s", err.Error())
	}
	host, port, err := net.SplitHostPort(parsedURL.Host)
	if err != nil {
		host = parsedURL.Host
		port = ""
	}
	baseURL := fmt.Sprintf("%s://%s", parsedURL.Scheme, parsedURL.Host)
	rest := parsedURL.RequestURI()

	logLines = append(logLines, models.LogLine{Time: time.Now(), Test: "Parsed url", Result: fmt.Sprintf("[Ok] host %s - port %s - baseURL %s - rest %s - prot: %s", host, port, baseURL, rest, parsedURL.Scheme)})
	return &models.ParsedUrl{BaseUrl: baseURL, Host: host, Port: port, Path: rest, Scheme: parsedURL.Scheme}, logLines, nil
}

// discoverPorts try to discover open ports
// dial is just to make it testable
func discoverPortsWithDial(zl *zerolog.Logger, host string, dial DialFunc) ([]string, []models.LogLine) {
	portsToScan := []string{"16686", "16685", "80", "3200", "3100", "8080", "9095", "443"}
	openPorts := []string{}
	logLines := []models.LogLine{}

	for _, port := range portsToScan {
		address := fmt.Sprintf("%s:%s", host, port)
		conn, err := dial("tcp", address, 500*time.Millisecond)
		if err == nil {
			logLines = append(logLines, models.LogLine{Time: time.Now(), Test: "Checking open ports", Result: fmt.Sprintf("[Ok] Port %s is open", port)})
			openPorts = append(openPorts, port)
			conn.Close()
		}
	}
	zl.Trace().Msgf("[Discovery client] Open ports: %v", openPorts)
	return openPorts, logLines
}

// discoverUrl try to discover valid URLs. configured is the URL the configuration names, which
// for Jaeger is probed first, as it is configured: the port scan knows only the default ports and
// base paths, and would report a working Jaeger on any other one as not found.
func discoverUrl(ctx context.Context, zl *zerolog.Logger, parsedUrl models.ParsedUrl, configured string, ports []string, auth *config.Auth, cfgTracing config.TracingConfig) ([]models.ValidConfig, []models.LogLine) {
	validConfigs := []models.ValidConfig{}
	var client http.Client
	logs := []models.LogLine{}

	// Create client
	timeout := time.Duration(config.Get().ExternalServices.Tracing.QueryTimeout) * time.Second
	conf := config.Get()
	transport, err := httputil.CreateTransport(conf, auth, &http.Transport{}, timeout, cfgTracing.CustomHeaders)
	if err != nil {
		logs = append(logs, models.LogLine{Time: time.Now(), Test: "Create HTTP client", Result: fmt.Sprintf("[ERROR] Cannot create transport: %s", err.Error())})
		// TODO: Validate auth?
		return validConfigs, logs
	} else {
		client = http.Client{Transport: transport, Timeout: timeout}
	}

	if isJaeger(cfgTracing) {
		vc, ll := validateConfiguredJaeger(ctx, client, zl, conf, auth, parsedUrl, configured, cfgTracing.UseGRPC)
		validConfigs = append(validConfigs, vc...)
		logs = append(logs, ll...)
	}

	for _, port := range ports {
		switch port {
		case "16686":
			{
				vc, ll := validateJaegerHTTP(ctx, client, zl, parsedUrl, port)
				validConfigs = append(validConfigs, vc...)
				logs = append(logs, ll...)
			}
		case "8080":
			{
				vc, ll := validateTempoHTTP(ctx, client, zl, parsedUrl, port)
				validConfigs = append(validConfigs, vc...)
				logs = append(logs, ll...)
			}
		case "80", "443":
			{
				if port == "443" {
					parsedUrl.Scheme = "https"
				}
				vc, ll := validateJaegerHTTP(ctx, client, zl, parsedUrl, port)
				validConfigs = append(validConfigs, vc...)
				logs = append(logs, ll...)
				vc, ll = validateTempoHTTP(ctx, client, zl, parsedUrl, port)
				validConfigs = append(validConfigs, vc...)
				logs = append(logs, ll...)
				vc, ll = validateSimpleTempoHTTP(ctx, client, zl, parsedUrl, port)
				validConfigs = append(validConfigs, vc...)
				logs = append(logs, ll...)
			}
		case "3200", "3100":
			{
				vc, ll := validateSimpleTempoHTTP(ctx, client, zl, parsedUrl, port)
				validConfigs = append(validConfigs, vc...)
				logs = append(logs, ll...)
			}
		case "16685":
			{
				// Try gRPC Jaeger client
				vc, ll := validateJaegerGRPC(ctx, conf, auth, parsedUrl, port)
				validConfigs = append(validConfigs, vc...)
				logs = append(logs, ll...)
			}
		case "9095":
			{
				// Try GRPC Tempo Client
				// And this also requires HTTP Client
				targetHost := parsedURLHostname(parsedUrl)
				dialOps, err := grpcutil.GetAuthDialOptions(conf, targetHost, parsedUrl.Scheme == "https", auth)
				if err != nil {
					msg := fmt.Sprintf("Error creating gRPC dial options: %v", err)
					logs = append(logs, models.LogLine{Time: time.Now(), Test: fmt.Sprintf("Create gRPC Dial Options port [%s] error", port), Result: msg})
					break
				}
				grpcAddress := fmt.Sprintf("%s:%s", parsedUrl.Host, port)
				clientConn, err := grpc.NewClient(grpcAddress, dialOps...)
				if err != nil {
					msg := fmt.Sprintf("Error creating gRPC client connection: %v", err)
					logs = append(logs, models.LogLine{Time: time.Now(), Test: fmt.Sprintf("Create gRPC Client Connection port [%s] error", port), Result: msg})
					break
				}
				streamClient, err := tempo.NewgRPCClient(clientConn)
				if err != nil {
					msg := fmt.Sprintf("Error creating gRPC Client %s", err.Error())
					logs = append(logs, models.LogLine{Time: time.Now(), Test: fmt.Sprintf("Create gRPC Tempo Client port [%s] error", port), Result: msg})
					zl.Error().Msg(msg)
				} else {
					ok, err := streamClient.GetServices(ctx)
					if ok {
						// TODO: Different config gRPC Port!!!
						vc := models.ValidConfig{Url: grpcAddress, Provider: "tempo", UseGRPC: true}
						validConfigs = append(validConfigs, vc)
						logs = append(logs, models.LogLine{Time: time.Now(), Test: fmt.Sprintf("Create gRPC Tempo Client port [%s] Ok", port), Result: "Valid gRPC Client found. Notice this config also requires any valid HTTP configuration. "})
					} else {
						logs = append(logs, models.LogLine{Time: time.Now(), Test: fmt.Sprintf("GetServices gRPC Tempo Client port [%s]", port), Result: fmt.Sprintf("error getting gRPC Services: [%s]", err.Error())})
					}
				}
			}
		}
	}
	return withoutDuplicates(validConfigs), logs
}

// validateConfiguredJaeger probes the configured Jaeger endpoint on the configured transport, the
// way the client reads it: over gRPC at the URL's host and port, over HTTP under the URL's base
// path.
func validateConfiguredJaeger(ctx context.Context, client http.Client, zl *zerolog.Logger, conf *config.Config, auth *config.Auth, parsedUrl models.ParsedUrl, configured string, useGRPC bool) ([]models.ValidConfig, []models.LogLine) {
	u, err := url.Parse(configured)
	if err != nil {
		return nil, []models.LogLine{{Time: time.Now(), Test: "Configured Jaeger URL", Result: fmt.Sprintf("cannot parse url: %s", err)}}
	}
	if useGRPC {
		port := u.Port()
		if port == "" {
			p, _ := net.LookupPort("tcp", u.Scheme)
			port = strconv.Itoa(p)
		}
		return validateJaegerGRPC(ctx, conf, auth, parsedUrl, port)
	}

	base := url.URL{Host: u.Host, Path: strings.TrimSuffix(u.Path, "/"), Scheme: u.Scheme}
	vc, ll, err := validateEndpoint(ctx, client, zl, base.String()+jaegerServicesPath, base.String(), "jaeger")
	if err != nil {
		return nil, ll
	}
	return []models.ValidConfig{*vc}, ll
}

// withoutDuplicates drops a configuration found twice, which the configured endpoint is whenever
// it is also one the port scan probes.
func withoutDuplicates(validConfigs []models.ValidConfig) []models.ValidConfig {
	seen := map[string]bool{}
	kept := []models.ValidConfig{}
	for _, vc := range validConfigs {
		key := fmt.Sprintf("%s|%s|%t", vc.Provider, vc.Url, vc.UseGRPC)
		if seen[key] {
			continue
		}
		seen[key] = true
		kept = append(kept, vc)
	}
	return kept
}

func validateJaegerGRPC(ctx context.Context, conf *config.Config, auth *config.Auth, parsedUrl models.ParsedUrl, port string) ([]models.ValidConfig, []models.LogLine) {
	logs := []models.LogLine{}
	validConfigs := []models.ValidConfig{}

	vc, log, err := validateGRPCClient(ctx, conf, auth, parsedUrl, port)
	if vc != nil {
		validConfigs = append(validConfigs, *vc)
	}
	logs = append(logs, log...)

	if err != nil && strings.Contains(err.Error(), "no transport security set") {
		noAuth := &config.Auth{}
		vc, log, _ = validateGRPCClient(ctx, conf, noAuth, parsedUrl, port)
		if vc != nil {
			validConfigs = append(validConfigs, *vc)
		}
		logs = append(logs, log...)
	}

	return validConfigs, logs
}

// validateGRPCClient
func validateGRPCClient(ctx context.Context, conf *config.Config, auth *config.Auth, parsedUrl models.ParsedUrl, port string) (*models.ValidConfig, []models.LogLine, error) {
	logs := []models.LogLine{}
	cfgTracing := conf.ExternalServices.Tracing
	targetHost := parsedURLHostname(parsedUrl)
	opts, err := grpcutil.GetAuthDialOptions(conf, targetHost, parsedUrl.Scheme == "https", auth)
	if err == nil {
		address := parsedUrl.Host + ":" + port
		logs = append(logs, models.LogLine{Time: time.Now(), Test: fmt.Sprintf("gRPC Client port [%s]", port), Result: fmt.Sprintf("%s GRPC client info: address=%s, auth.type=%s", cfgTracing.Provider, address, auth.Type)})

		if len(cfgTracing.CustomHeaders) > 0 {
			logs = append(logs, models.LogLine{Time: time.Now(), Test: fmt.Sprintf("gRPC Client port [%s]", port), Result: fmt.Sprintf("Adding [%v] custom headers to Tracing client", len(cfgTracing.CustomHeaders))})
			ctx = metadata.NewOutgoingContext(ctx, metadata.New(cfgTracing.CustomHeaders))
		}
		conn, err := grpc.NewClient(address, append(opts, jaeger.DialOptions()...)...)
		if err == nil {
			defer conn.Close()
			timeout := time.Duration(cfgTracing.QueryTimeout) * time.Second
			clientgRPC := jaeger.NewGRPCClient(apiv3.NewQueryServiceClient(conn), timeout)
			// The probe is the one the client runs as its health check, so an endpoint is only
			// suggested if the client can read it. Its errors already say when the port does not
			// serve api_v3 or speaks HTTP.
			services, err := clientgRPC.GetServicesList(ctx)
			if err == nil {
				authType := auth.Type
				if authType == "" {
					authType = "none"
				}
				ns := hasNamespacedServiceName(services)
				validConfig := models.ValidConfig{AuthType: authType, NamespaceSelector: &ns, Url: fmt.Sprintf("%s://%s", parsedUrl.Scheme, address), Provider: "jaeger", UseGRPC: true}
				logs = append(logs, models.LogLine{Time: time.Now(), Test: fmt.Sprintf("Create gRPC Client port [%s] Ok", port), Result: "Valid Jaeger api_v3 gRPC Client found"})
				return &validConfig, logs, nil
			}
			logs = append(logs, models.LogLine{Time: time.Now(), Test: fmt.Sprintf("GetServices gRPC Client port [%s]", port), Result: fmt.Sprintf("error getting Jaeger api_v3 gRPC Services: [%s]", err.Error())})
			return nil, logs, fmt.Errorf("error getting Jaeger api_v3 gRPC Services: [%w]", err)
		} else {
			logs = append(logs, models.LogLine{Time: time.Now(), Test: fmt.Sprintf("gRPC client port [%s]", port), Result: fmt.Sprintf("error creating client: %s", err.Error())})
			return nil, logs, fmt.Errorf("error creating client %s", err.Error())
		}
	} else {
		logs = append(logs, models.LogLine{Time: time.Now(), Test: fmt.Sprintf("gRPC client port [%s]", port), Result: fmt.Sprintf("Error creating dial options: %s", err.Error())})
		return nil, logs, fmt.Errorf("error creating client %s", err.Error())
	}
}

func parsedURLHostname(parsedUrl models.ParsedUrl) string {
	if parsedUrl.Url != nil && parsedUrl.Url.Hostname() != "" {
		return parsedUrl.Url.Hostname()
	}
	if host, _, err := net.SplitHostPort(parsedUrl.Host); err == nil {
		return host
	}
	return parsedUrl.Host
}

// jaegerServicesPath is the api_v3 route every Jaeger probe asks, the same one the client's health
// check asks, so discovery never suggests an endpoint the client then cannot read.
const jaegerServicesPath = "/api/v3/services"

// validateJaegerHTTP validate specific path for Jaeger and HTTP endpoint
func validateJaegerHTTP(ctx context.Context, client http.Client, zl *zerolog.Logger, parsedUrl models.ParsedUrl, port string) ([]models.ValidConfig, []models.LogLine) {
	logs := []models.LogLine{}
	validConfigs := []models.ValidConfig{}

	// We assume it is Jaeger. Try Jaeger URL
	validEndpoint := fmt.Sprintf("%s://%s:%s/jaeger", parsedUrl.Scheme, parsedUrl.Host, port)
	endpointJ := validEndpoint + jaegerServicesPath
	vc, ll, err := validateEndpoint(ctx, client, zl, endpointJ, validEndpoint, "jaeger")
	if err == nil {
		validConfigs = append(validConfigs, *vc)
	}
	logs = append(logs, ll...)

	// Try Tempo URL
	validEndpoint = fmt.Sprintf("%s://%s:%s", parsedUrl.Scheme, parsedUrl.Host, port)
	endpointJ = validEndpoint + jaegerServicesPath
	vc, ll, err = validateEndpoint(ctx, client, zl, endpointJ, validEndpoint, "jaeger")
	if err == nil {
		validConfigs = append(validConfigs, *vc)
	}
	logs = append(logs, ll...)
	return validConfigs, logs
}

// validateTempoHTTP validate specific path for Tempo and HTTP endpoint
func validateTempoHTTP(ctx context.Context, client http.Client, zl *zerolog.Logger, parsedUrl models.ParsedUrl, port string) ([]models.ValidConfig, []models.LogLine) {
	logs := []models.LogLine{}
	validConfigs := []models.ValidConfig{}

	// Tempo from GW, Tempo multi tenant uses security and the gateway
	if strings.Contains(parsedUrl.Host, "gateway") {
		splitUrl := strings.Split(parsedUrl.Path, "/")
		tenant := ""
		conf := config.Get()
		if conf.ExternalServices.Tracing.TempoConfig.Tenant != "" {
			tenant = conf.ExternalServices.Tracing.TempoConfig.Tenant
		} else {
			if len(splitUrl) > 4 {
				tenant = splitUrl[4]
			} else {
				logs = append(logs, models.LogLine{Time: time.Now(), Test: fmt.Sprintf("Create http client in port [%s]", port), Result: fmt.Sprintf("tenant name not found: %s", parsedUrl.Path)})
			}
		}

		validEndpoint := fmt.Sprintf("%s://%s:%s/api/traces/v1/%s/tempo", parsedUrl.Scheme, parsedUrl.Host, port, tenant)
		endpointJ := fmt.Sprintf("%s/api/search?q={}", validEndpoint)
		vc, ll, err := validateEndpoint(ctx, client, zl, endpointJ, validEndpoint, "tempo")
		if err == nil {
			validConfigs = append(validConfigs, *vc)
		}
		logs = append(logs, ll...)

		// Try GW Jaeger Endpoint
		validEndpoint = fmt.Sprintf("%s://%s:%s/api/traces/v1/%s", parsedUrl.Scheme, parsedUrl.Host, port, tenant)
		endpointJ = validEndpoint + jaegerServicesPath
		vc, ll, err = validateEndpoint(ctx, client, zl, endpointJ, validEndpoint, "jaeger")
		if err == nil {
			validConfigs = append(validConfigs, *vc)
		}
		logs = append(logs, ll...)

	} else {
		// Tempo service (No GW)
		validEndpoint := fmt.Sprintf("%s://%s:%s/api/traces/v1/tempo", parsedUrl.Scheme, parsedUrl.Host, port)
		endpointJ := fmt.Sprintf("%s/api/search?q={}", validEndpoint)
		vc, ll, err := validateEndpoint(ctx, client, zl, endpointJ, validEndpoint, "tempo")
		if err == nil {
			validConfigs = append(validConfigs, *vc)
		}
		logs = append(logs, ll...)

		// Try GW Jaeger Endpoint
		validEndpoint = fmt.Sprintf("%s://%s:%s/api/traces/v1", parsedUrl.Scheme, parsedUrl.Host, port)
		endpointJ = validEndpoint + jaegerServicesPath
		vc, ll, err = validateEndpoint(ctx, client, zl, endpointJ, validEndpoint, "jaeger")
		if err == nil {
			validConfigs = append(validConfigs, *vc)
		}
		logs = append(logs, ll...)
	}

	return validConfigs, logs
}

// validateSimpleTempoHTTP validate specific path for Tempo and HTTP endpoint
func validateSimpleTempoHTTP(ctx context.Context, client http.Client, zl *zerolog.Logger, parsedUrl models.ParsedUrl, port string) ([]models.ValidConfig, []models.LogLine) {
	logs := []models.LogLine{}
	validConfigs := []models.ValidConfig{}

	// Try Tempo HTTP client
	validEndpoint := fmt.Sprintf("%s://%s:%s", parsedUrl.Scheme, parsedUrl.Host, port)
	endpointJ := fmt.Sprintf("%s/api/search?q={}", validEndpoint)
	vc, ll, err := validateEndpoint(ctx, client, zl, endpointJ, validEndpoint, "tempo")
	if err == nil {
		validConfigs = append(validConfigs, *vc)
	}
	logs = append(logs, ll...)

	// Try Jaeger?
	validEndpoint = fmt.Sprintf("%s://%s:%s", parsedUrl.Scheme, parsedUrl.Host, port)
	endpointJ = validEndpoint + jaegerServicesPath
	vc, ll, err = validateEndpoint(ctx, client, zl, endpointJ, validEndpoint, "jaeger")
	if err == nil {
		validConfigs = append(validConfigs, *vc)
	}
	logs = append(logs, ll...)

	return validConfigs, logs
}

// validateEndpoint Given an endpoint, validates it is valid, otherwise returns an error or logLines
func validateEndpoint(ctx context.Context, client http.Client, zl *zerolog.Logger, endpoint, validEndpoint string, provider string) (*models.ValidConfig, []models.LogLine, error) {
	logs := []models.LogLine{}
	resp, code, reqError := MakeRequestFunc(ctx, client, endpoint, nil)
	if code != 200 {
		// Try to handle possible known errors
		// http to https
		msg := fmt.Sprintf("[Discovery client] Cannot query endpoint: %s. Code [%d].", endpoint, code)
		if reqError != nil {
			msg = fmt.Sprintf("%s. Error: %s", msg, reqError.Error())
		}
		if provider == "jaeger" {
			if wrongTransport := jaeger.HTTPWrongTransportError(endpoint, reqError); wrongTransport != nil {
				msg = fmt.Sprintf("[Discovery client] %s", wrongTransport)
			}
		}
		if resp != nil {
			msg = fmt.Sprintf("%s. Response: %s", msg, resp)
			response := string(resp)
			// HTTPS required. Try HTTPS URL
			if strings.Contains(response, "Client sent an HTTP request to an HTTPS") && !strings.Contains(endpoint, "https") {
				endpoint = strings.Replace(endpoint, "http", "https", 1)
				validEndpoint = strings.Replace(validEndpoint, "http", "https", 1)
				return validateEndpoint(ctx, client, zl, endpoint, validEndpoint, provider)
			}
			// No tenants found
			if strings.Contains(response, "tenant not found") {
				vc := models.ValidConfig{Url: validEndpoint, Provider: provider, UseGRPC: false, Warning: "Tenant must be specified."}
				return &vc, logs, nil
			}
		}
		// Certificate error: Try valid host
		if reqError != nil && strings.Contains(reqError.Error(), "certificate is valid for") {
			validRe := regexp.MustCompile(`certificate is valid for ([^,]+)`)
			notRe := regexp.MustCompile(`not ([^\s]+)`)
			validMatch := validRe.FindStringSubmatch(reqError.Error())
			notMatch := notRe.FindStringSubmatch(reqError.Error())
			if len(validMatch) > 1 && len(notMatch) > 1 {
				replacedEndpoint := strings.Replace(endpoint, notMatch[1], validMatch[1], 1)
				replacedValidEndpoint := strings.Replace(validEndpoint, notMatch[1], validMatch[1], 1)
				if replacedEndpoint != endpoint {
					return validateEndpoint(ctx, client, zl, replacedEndpoint, replacedValidEndpoint, provider)
				}
			}
		}
		// Auth issue
		var unknownAuthErr x509.UnknownAuthorityError
		if errors.As(reqError, &unknownAuthErr) {
			vc := models.ValidConfig{Url: validEndpoint, Provider: provider, UseGRPC: false, Warning: "Auth section must be configured properly"}
			return &vc, logs, nil
		}
		logs = append(logs, models.LogLine{Time: time.Now(), Test: endpoint, Result: msg})
		zl.Trace().Msg(msg)
		return nil, logs, errors.New(msg)
	}

	if provider == "jaeger" {
		// The client's own decoder, so an answer it would refuse (the pre-api_v3 {"data":...}
		// shape, or the Jaeger UI's HTML under a wrong base path) is not suggested.
		services, errDecode := jaeger.DecodeServices(resp)
		if errDecode != nil {
			msg := fmt.Sprintf("[Discovery client] Not a Jaeger api_v3 answer (Jaeger 1.54 or newer is required): %s [URL: %v]", errDecode, endpoint)
			logs = append(logs, models.LogLine{Time: time.Now(), Test: endpoint, Result: msg})
			zl.Trace().Msg(msg)
			return nil, logs, errors.New(msg)
		}
		ns := hasNamespacedServiceName(services)
		vc := models.ValidConfig{Url: validEndpoint, Provider: provider, UseGRPC: false}
		vc.NamespaceSelector = &ns
		msg := fmt.Sprintf("[Discovery client] Found valid Config %v", vc)
		logs = append(logs, models.LogLine{Time: time.Now(), Test: endpoint, Result: msg})
		zl.Trace().Msg(msg)
		return &vc, logs, nil
	}
	// Try Tempo. The probe asks /api/search for everything, so the body is the search answer the
	// Tempo client reads, and every Tempo sends more of it than the IDL declares: a strict decode
	// rejects a healthy backend here.
	var response querypb.SearchResponse
	if errMarshal := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(resp, &response); errMarshal != nil {
		msg := fmt.Sprintf("[Discovery client] Error unmarshalling Tempo response: %s [URL: %v]", errMarshal, endpoint)
		logs = append(logs, models.LogLine{Time: time.Now(), Test: endpoint, Result: msg})
		zl.Trace().Msg(msg)
		return nil, logs, errors.New(msg)
	}
	ns := false
	vc := models.ValidConfig{Url: validEndpoint, Provider: "tempo", UseGRPC: false}
	for _, rd := range response.GetTraces() {
		parts := strings.Split(rd.GetRootServiceName(), ".")
		if len(parts) > 1 {
			ns = true
			break
		}
	}
	vc.NamespaceSelector = &ns
	msg := fmt.Sprintf("[Discovery client] Found valid Config %v", vc)
	logs = append(logs, models.LogLine{Time: time.Now(), Test: endpoint, Result: msg})
	zl.Trace().Msg(msg)
	return &vc, logs, nil
}

// hasNamespacedServiceName guesses namespace_selector from the service names a store reports: a
// name with a dot is taken to be "<service>.<namespace>", which is how Istio names its spans when
// the namespace is part of the service name.
func hasNamespacedServiceName(services []string) bool {
	for _, service := range services {
		if strings.Contains(service, ".") {
			return true
		}
	}
	return false
}
