// Package proxyattribution reads the egress proxy's failure records for the
// gateway's own namespace and joins each to the worker pod that held the record's
// source address at the time (Q1184). The posture is docs/design/05-security.md §
// Proxy failure attribution.
package proxyattribution

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/apinames"
	v2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The projection the GMC writes into an attributed namespace. Its name, keys and
// the gateway's opt-in value are a convention shared with
// cmd/gmc/internal/controller/egressproxy_attribution.go, which is the authority.
const (
	projectionNamePrefix = "proxy-attribution"
	readHostKey          = "read-host"
	readPortKey          = "read-port"
	serverNameKey        = "server-name"
	caCertKey            = "ca.crt"
	optInPerJob          = "PerJob"
)

const (
	// DefaultInterval is how often every replica is read. The proxy keeps a
	// record for five minutes, so a poll or two missed loses nothing.
	DefaultInterval = 30 * time.Second
	// cursorRetention is how long a replica's cursor outlives its last good
	// read: past the proxy's five-minute retention, plus skew, so a replica read
	// again from the start after that holds no record this AGC already has.
	cursorRetention = 6 * time.Minute
	// readTimeout bounds one replica's read.
	readTimeout = 10 * time.Second
	// maxResponseBytes bounds one response body: 1024 records of capped fields.
	maxResponseBytes = 4 << 20
)

// record is one entry of the proxy's GET /v1/failures response.
type record struct {
	Seq       uint64    `json:"seq"`
	Time      time.Time `json:"time"`
	SourceIP  string    `json:"sourceIP"`
	Outcome   string    `json:"outcome"`
	DialClass string    `json:"dialClass,omitempty"`
	Host      string    `json:"host"`
	Port      string    `json:"port"`
}

type response struct {
	Epoch   string   `json:"epoch"`
	Records []record `json:"records"`
}

type pool struct{ namespace, name string }

// replica is one pool replica, by the address its headless Service resolves to.
type replica struct {
	pool pool
	addr string
}

// cursor is where the last good read of one replica ended, and when it was.
type cursor struct {
	epoch string
	after uint64
	read  time.Time
}

// Poller reads every replica of each pool this gateway's RunnerSets use, on an
// interval, while the gateway has opted in. It runs on every AGC replica.
type Poller struct {
	// Client is the cached client: the gateway and its RunnerSets.
	Client client.Reader
	// APIReader reads the projected ConfigMap and Secret, neither of which the
	// AGC caches.
	APIReader client.Reader
	// Informers supplies the pod informer the address windows are built from.
	Informers cache.Informers
	Namespace string
	Gateway   string
	Interval  time.Duration
	Metrics   *Metrics
	Log       *slog.Logger

	// Seams for tests; nil means the real resolver, dialer and clock.
	resolve func(ctx context.Context, host string) ([]string, error)
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
	now     func() time.Time

	windows  *addressWindows
	failures *failureStore
	cursors  map[replica]cursor
}

// NewPoller returns a Poller for gateway in namespace.
func NewPoller(c, apiReader client.Reader, informers cache.Informers, namespace, gateway string, m *Metrics, log *slog.Logger) *Poller {
	return &Poller{
		Client: c, APIReader: apiReader, Informers: informers,
		Namespace: namespace, Gateway: gateway, Interval: DefaultInterval,
		Metrics: m, Log: log,
		windows: newAddressWindows(), failures: newFailureStore(), cursors: map[replica]cursor{},
	}
}

// Lookup returns the failures attributed to the worker pod with UID pod. A record
// reaches it up to one poll interval after the failure.
func (p *Poller) Lookup(pod types.UID) []Failure { return p.failures.lookup(pod) }

// NeedLeaderElection reports false: every replica's sessions own their own pods.
func (p *Poller) NeedLeaderElection() bool { return false }

// Start builds the address windows off the pod informer and reads on Interval
// until ctx is cancelled.
func (p *Poller) Start(ctx context.Context) error {
	inf, err := p.Informers.GetInformer(ctx, &corev1.Pod{})
	if err != nil {
		return fmt.Errorf("proxyattribution: get pod informer: %w", err)
	}
	reg, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { p.onPod(ctx, obj) },
		UpdateFunc: func(_, obj any) { p.onPod(ctx, obj) },
		DeleteFunc: p.onPodDelete,
	})
	if err != nil {
		return fmt.Errorf("proxyattribution: add pod event handler: %w", err)
	}
	defer func() { _ = inf.RemoveEventHandler(reg) }()

	t := time.NewTicker(p.Interval)
	defer t.Stop()
	for {
		if err := p.poll(ctx); err != nil {
			p.Log.Info("proxy failure attribution read failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (p *Poller) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *Poller) onPod(ctx context.Context, obj any) {
	if pod, ok := obj.(*corev1.Pod); ok {
		p.windows.observe(pod, p.ownPod(ctx, pod), p.clock())
	}
}

func (p *Poller) onPodDelete(obj any) {
	if t, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
		obj = t.Obj
	}
	if pod, ok := obj.(*corev1.Pod); ok {
		p.windows.close(pod.UID, p.clock())
	}
}

// ownPod reports whether pod is a worker of a RunnerSet this gateway owns. A
// sibling gateway's workers share the namespace, and so its records.
func (p *Poller) ownPod(ctx context.Context, pod *corev1.Pod) bool {
	name := pod.Labels[provisioner.LabelRunnerSet]
	if name == "" {
		return false
	}
	var rs v2alpha1.RunnerSet
	if err := p.Client.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: name}, &rs); err != nil {
		return false
	}
	return rs.Spec.GatewayRef.Name == p.Gateway
}

// poll reads every replica of every pool once, and drops what it no longer reads.
func (p *Poller) poll(ctx context.Context) error {
	// Windows are built whether or not the gateway is opted in, so a pod already
	// running when it opts in can be joined; they are swept either way.
	now := p.clock()
	p.windows.sweep(now)
	var gw v2alpha1.ActionsGateway
	if err := p.Client.Get(ctx, types.NamespacedName{Namespace: p.Namespace, Name: p.Gateway}, &gw); err != nil {
		if apierrors.IsNotFound(err) {
			p.reset()
			return nil
		}
		return fmt.Errorf("read gateway: %w", err)
	}
	if gw.Spec.ProxyFailureAttribution != optInPerJob {
		p.reset()
		return nil
	}
	pools, err := p.pools(ctx, &gw)
	if err != nil {
		return err
	}
	var errs []error
	for _, pl := range pools {
		if err := p.readPool(ctx, pl); err != nil {
			errs = append(errs, fmt.Errorf("pool %s/%s: %w", pl.namespace, pl.name, err))
		}
	}
	// A cursor is kept through failed reads and a replica's absence from DNS,
	// and dropped only once the proxy can no longer hold anything it covered.
	for r, cur := range p.cursors {
		if now.Sub(cur.read) > cursorRetention {
			delete(p.cursors, r)
		}
	}
	p.failures.sweep(now)
	return errors.Join(errs...)
}

func (p *Poller) reset() {
	clear(p.cursors)
	p.failures.reset()
}

// pools resolves each of this gateway's RunnerSets to its pool, as the worker's
// port is resolved: the set's proxyRef, else the gateway's defaultProxyRef.
func (p *Poller) pools(ctx context.Context, gw *v2alpha1.ActionsGateway) ([]pool, error) {
	var sets v2alpha1.RunnerSetList
	if err := p.Client.List(ctx, &sets, client.InNamespace(p.Namespace)); err != nil {
		return nil, fmt.Errorf("list RunnerSets: %w", err)
	}
	seen := map[pool]bool{}
	var out []pool
	for i := range sets.Items {
		rs := &sets.Items[i]
		if rs.Spec.GatewayRef.Name != p.Gateway {
			continue
		}
		ref := rs.Spec.ProxyRef
		if ref == nil {
			ref = gw.Spec.DefaultProxyRef
		}
		if ref == nil {
			continue
		}
		pl := pool{namespace: ref.Namespace, name: ref.Name}
		if pl.namespace == "" {
			pl.namespace = p.Namespace
		}
		if !seen[pl] {
			seen[pl] = true
			out = append(out, pl)
		}
	}
	return out, nil
}

// readPool reads every replica of pl through its projection. A pool with no
// projection here is not attributing this namespace.
func (p *Poller) readPool(ctx context.Context, pl pool) error {
	key := types.NamespacedName{Namespace: p.Namespace, Name: apinames.Join(apinames.MaxLabelValue, projectionNamePrefix, pl.namespace, pl.name)}
	var cm corev1.ConfigMap
	if err := p.APIReader.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("read projection: %w", err)
	}
	var sec corev1.Secret
	if err := p.APIReader.Get(ctx, key, &sec); err != nil {
		return fmt.Errorf("read client certificate: %w", err)
	}
	cert, err := tls.X509KeyPair(sec.Data[corev1.TLSCertKey], sec.Data[corev1.TLSPrivateKeyKey])
	if err != nil {
		return fmt.Errorf("load client certificate: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(sec.Data[caCertKey]) {
		return fmt.Errorf("projected Secret %q holds no CA", key.Name)
	}
	port := cm.Data[readPortKey]
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("projection %q carries an invalid %s %q", key.Name, readPortKey, port)
	}

	resolve := p.resolve
	if resolve == nil {
		resolve = net.DefaultResolver.LookupHost
	}
	addrs, err := resolve(ctx, cm.Data[readHostKey])
	if err != nil {
		p.countRead(false)
		return fmt.Errorf("resolve replicas: %w", err)
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{cert},
			RootCAs:      roots,
			ServerName:   cm.Data[serverNameKey],
		},
		DialContext: p.dial,
	}
	defer tr.CloseIdleConnections()
	hc := &http.Client{Transport: tr, Timeout: readTimeout}
	for _, addr := range addrs {
		r := replica{pool: pl, addr: addr}
		p.readReplica(ctx, hc, r, net.JoinHostPort(addr, port))
	}
	return nil
}

// readReplica reads one replica from its cursor. A changed epoch is a restarted
// replica with a new buffer, so it is read again from the start. A failed read
// keeps the cursor: the next poll resumes where this one would have.
func (p *Poller) readReplica(ctx context.Context, hc *http.Client, r replica, hostPort string) {
	cur := p.cursors[r]
	resp, err := fetch(ctx, hc, hostPort, cur.after)
	if err == nil && resp.Epoch != cur.epoch && cur.after > 0 {
		resp, err = fetch(ctx, hc, hostPort, 0)
	}
	if err != nil {
		p.countRead(false)
		p.Log.Debug("proxy failure attribution replica read failed", "replica", hostPort, "error", err)
		return
	}
	p.countRead(true)
	if resp.Epoch != cur.epoch {
		cur = cursor{epoch: resp.Epoch}
	}
	cur.read = p.clock()
	for _, rec := range resp.Records {
		p.ingest(rec)
		cur.after = max(cur.after, rec.Seq)
	}
	p.cursors[r] = cur
}

func fetch(ctx context.Context, hc *http.Client, hostPort string, after uint64) (*response, error) {
	u := url.URL{Scheme: "https", Host: hostPort, Path: "/v1/failures", RawQuery: "after=" + strconv.FormatUint(after, 10)}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", res.StatusCode)
	}
	var out response
	if err := json.NewDecoder(http.MaxBytesReader(nil, res.Body, maxResponseBytes)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &out, nil
}

// ingest attributes rec to the pod that held its source address, or drops it.
func (p *Poller) ingest(rec record) {
	pod, ok := p.windows.attribute(rec.SourceIP, rec.Time)
	result := "discarded"
	if ok {
		result = "attributed"
		p.failures.add(pod, Failure{Time: rec.Time, Outcome: rec.Outcome, DialClass: rec.DialClass, Host: rec.Host, Port: rec.Port})
	}
	if p.Metrics != nil {
		p.Metrics.Records.WithLabelValues(p.Namespace, outcomeLabel(rec.Outcome), result).Inc()
	}
}

// outcomeLabel bounds the label to the outcomes the proxy writes.
func outcomeLabel(o string) string {
	switch o {
	case "denied", "dial-failed":
		return o
	}
	return "other"
}

func (p *Poller) countRead(ok bool) {
	if p.Metrics == nil {
		return
	}
	result := "failed"
	if ok {
		result = "ok"
	}
	p.Metrics.Reads.WithLabelValues(p.Namespace, result).Inc()
}
