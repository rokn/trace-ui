package jaeger

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// ── API response types ────────────────────────────────────────────────────────

type ServicesResponse struct {
	Data   []string `json:"data"`
	Errors []any    `json:"errors"`
}

type OperationsResponse struct {
	Data   []string `json:"data"`
	Errors []any    `json:"errors"`
}

type TracesResponse struct {
	Data   []Trace `json:"data"`
	Errors []any   `json:"errors"`
}

type TraceResponse struct {
	Data   []Trace `json:"data"`
	Errors []any   `json:"errors"`
}

type Trace struct {
	TraceID   string             `json:"traceID"`
	Spans     []Span             `json:"spans"`
	Processes map[string]Process `json:"processes"`
}

type Span struct {
	TraceID       string     `json:"traceID"`
	SpanID        string     `json:"spanID"`
	OperationName string     `json:"operationName"`
	References    []SpanRef  `json:"references"`
	StartTime     int64      `json:"startTime"` // microseconds
	Duration      int64      `json:"duration"`  // microseconds
	Tags          []KeyValue `json:"tags"`
	Logs          []SpanLog  `json:"logs"`
	ProcessID     string     `json:"processID"`
	Warnings      []string   `json:"warnings"`
}

type SpanRef struct {
	RefType string `json:"refType"`
	TraceID string `json:"traceID"`
	SpanID  string `json:"spanID"`
}

type KeyValue struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Value any    `json:"value"`
}

type SpanLog struct {
	Timestamp int64      `json:"timestamp"`
	Fields    []KeyValue `json:"fields"`
}

type Process struct {
	ServiceName string     `json:"serviceName"`
	Tags        []KeyValue `json:"tags"`
}

// ── Derived helpers ───────────────────────────────────────────────────────────

func (t *Trace) RootSpan() *Span {
	roots := t.rootSpans()
	if len(roots) > 0 {
		return roots[0]
	}
	if len(t.Spans) > 0 {
		return &t.Spans[0]
	}
	return nil
}

func (t *Trace) rootSpans() []*Span {
	childIDs := map[string]bool{}
	for _, s := range t.Spans {
		for _, r := range s.References {
			if r.RefType == "CHILD_OF" {
				childIDs[s.SpanID] = true
			}
		}
	}
	var roots []*Span
	for i := range t.Spans {
		if !childIDs[t.Spans[i].SpanID] {
			roots = append(roots, &t.Spans[i])
		}
	}
	return roots
}

func (t *Trace) Duration() time.Duration {
	root := t.RootSpan()
	if root != nil {
		return time.Duration(root.Duration) * time.Microsecond
	}
	// fallback: max end - min start
	var minStart, maxEnd int64
	for i, s := range t.Spans {
		end := s.StartTime + s.Duration
		if i == 0 || s.StartTime < minStart {
			minStart = s.StartTime
		}
		if end > maxEnd {
			maxEnd = end
		}
	}
	return time.Duration(maxEnd-minStart) * time.Microsecond
}

func (t *Trace) StartTime() time.Time {
	root := t.RootSpan()
	if root != nil {
		return time.UnixMicro(root.StartTime)
	}
	var minStart int64
	for i, s := range t.Spans {
		if i == 0 || s.StartTime < minStart {
			minStart = s.StartTime
		}
	}
	return time.UnixMicro(minStart)
}

func (t *Trace) ServiceName() string {
	root := t.RootSpan()
	if root == nil {
		return "unknown"
	}
	if p, ok := t.Processes[root.ProcessID]; ok {
		return p.ServiceName
	}
	return "unknown"
}

func (s *Span) ServiceName(processes map[string]Process) string {
	if p, ok := processes[s.ProcessID]; ok {
		return p.ServiceName
	}
	return "unknown"
}

func (s *Span) DurationString() string {
	d := time.Duration(s.Duration) * time.Microsecond
	return formatDuration(d)
}

func formatDuration(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%.0fµs", float64(d.Microseconds()))
	}
	if d < time.Second {
		return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000)
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

func (t *Trace) DurationString() string {
	return formatDuration(t.Duration())
}

func (t *Trace) SpanTree() []*SpanNode {
	byID := map[string]*SpanNode{}
	for i := range t.Spans {
		byID[t.Spans[i].SpanID] = &SpanNode{Span: &t.Spans[i]}
	}
	var roots []*SpanNode
	for i := range t.Spans {
		s := &t.Spans[i]
		node := byID[s.SpanID]
		parentID := ""
		for _, r := range s.References {
			if r.RefType == "CHILD_OF" {
				parentID = r.SpanID
				break
			}
		}
		if parentID == "" {
			roots = append(roots, node)
		} else if parent, ok := byID[parentID]; ok {
			parent.Children = append(parent.Children, node)
		} else {
			roots = append(roots, node)
		}
	}
	return roots
}

type SpanNode struct {
	Span     *Span
	Children []*SpanNode
	Depth    int
}

func FlattenTree(nodes []*SpanNode, depth int) []*SpanNode {
	var result []*SpanNode
	for _, n := range nodes {
		n.Depth = depth
		result = append(result, n)
		result = append(result, FlattenTree(n.Children, depth+1)...)
	}
	return result
}

// ── API calls ─────────────────────────────────────────────────────────────────

func (c *Client) GetServices() ([]string, error) {
	resp, err := c.HTTPClient.Get(c.BaseURL + "/api/services")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result ServicesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (c *Client) GetOperations(service string) ([]string, error) {
	u := fmt.Sprintf("%s/api/services/%s/operations", c.BaseURL, url.PathEscape(service))
	resp, err := c.HTTPClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result OperationsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

type SearchParams struct {
	Service   string
	Operation string
	Tags      string
	MinDur    string
	MaxDur    string
	Limit     int
	Lookback  string
	Start     *time.Time
	End       *time.Time
}

func (c *Client) SearchTraces(p SearchParams) ([]Trace, error) {
	q := url.Values{}
	q.Set("service", p.Service)
	if p.Operation != "" && p.Operation != "all" {
		q.Set("operation", p.Operation)
	}
	if p.Tags != "" {
		q.Set("tags", p.Tags)
	}
	if p.MinDur != "" {
		q.Set("minDuration", p.MinDur)
	}
	if p.MaxDur != "" {
		q.Set("maxDuration", p.MaxDur)
	}
	if p.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", p.Limit))
	}
	if p.Lookback != "" {
		q.Set("lookback", p.Lookback)
	}
	if p.Start != nil {
		q.Set("start", fmt.Sprintf("%d", p.Start.UnixMicro()))
	}
	if p.End != nil {
		q.Set("end", fmt.Sprintf("%d", p.End.UnixMicro()))
	}

	u := c.BaseURL + "/api/traces?" + q.Encode()
	resp, err := c.HTTPClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result TracesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (c *Client) GetTrace(traceID string) (*Trace, error) {
	u := c.BaseURL + "/api/traces/" + traceID
	resp, err := c.HTTPClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result TraceResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Data) == 0 {
		return nil, fmt.Errorf("trace not found: %s", traceID)
	}
	return &result.Data[0], nil
}
