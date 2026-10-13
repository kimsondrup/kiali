package models

import (
	"net/url"
	"regexp"
	"time"

	"github.com/kiali/kiali/config"
	jsonModel "github.com/kiali/kiali/models/tracing/json"
)

const (
	IstioClusterTag string = "istio.cluster_id"

	// MaxTracingLimit is the upper bound for trace query limit parameters.
	MaxTracingLimit = 10000
)

// ValidTraceIDRe validates trace IDs: 1-32 hexadecimal characters (Jaeger/Tempo compatible).
var ValidTraceIDRe = regexp.MustCompile(`^[a-fA-F0-9]{1,32}$`)

type TracingInfo struct {
	Enabled              bool               `json:"enabled"`
	Integration          bool               `json:"integration"`
	InternalURL          string             `json:"internalURL"`
	Provider             string             `json:"provider"`
	TempoConfig          config.TempoConfig `json:"tempoConfig"`
	URL                  string             `json:"url"`
	NamespaceSelector    bool               `json:"namespaceSelector"`
	UseWaypointName      bool               `json:"useWaypointName"`
	WhiteListIstioSystem []string           `json:"whiteListIstioSystem"`
}

type TracingQuery struct {
	Start       time.Time
	End         time.Time
	Tags        map[string]string
	MinDuration time.Duration
	Limit       int
	Cluster     string
}

type StructuredError struct {
	Code    int    `json:"code,omitempty"`
	Msg     string `json:"msg"`
	TraceID string `json:"traceID,omitempty"`
}

type TracingResponse struct {
	Data               []jsonModel.Trace `json:"data"`
	Errors             []StructuredError `json:"errors"`
	TracingServiceName string            `json:"tracingServiceName"`
}

type TracingSingleTrace struct {
	Data   jsonModel.Trace   `json:"data"`
	Errors []StructuredError `json:"errors"`
}

type TracingSpan struct {
	jsonModel.Span
	TraceSize int `json:"traceSize"`
}

type ParsedUrl struct {
	BaseUrl string   `json:"baseUrl"`
	Host    string   `json:"host"`
	Path    string   `json:"path"`
	Port    string   `json:"port"`
	Scheme  string   `json:"scheme"`
	Url     *url.URL `json:"url,omitempty"`
}

type ValidConfig struct {
	AuthType          string `json:"authType,omitempty"`
	NamespaceSelector *bool  `json:"namespaceSelector"`
	Provider          string `json:"provider"`
	Url               string `json:"url"`
	UseGRPC           bool   `json:"useGRPC"`
	Warning           string `json:"warning"`
}

type TracingDiagnose struct {
	Code        int           `json:"code"`
	LogLine     []LogLine     `json:"logLine"`
	Message     string        `json:"message"`
	ValidConfig []ValidConfig `json:"validConfig"`
}

type ConfigurationValidation struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

type LogLine struct {
	Result string    `json:"result"`
	Test   string    `json:"test"`
	Time   time.Time `json:"time"`
}
