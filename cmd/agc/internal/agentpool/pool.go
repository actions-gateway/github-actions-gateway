// Package agentpool manages pre-registered runner agent credentials for a RunnerGroup.
package agentpool

import (
	"context"
	"crypto"
	stderrors "errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/actions-gateway/github-actions-gateway/agc/names"
	"github.com/actions-gateway/github-actions-gateway/githubapp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	labelManagedBy   = "app.kubernetes.io/managed-by"
	labelRunnerGroup = "actions-gateway/runner-group"
	// labelRunnerSet is the v2 owner-identity label on a RunnerSet's agent Secrets,
	// and the key half of the selector that enumerates them. It mirrors
	// provisioner.LabelRunnerSet, which stamps the same key on v2 worker pods and job
	// Secrets; it is re-declared here rather than imported because provisioner
	// imports listener, which imports this package. TestLabelRunnerSetMatchesProvisioner
	// fails if the two ever drift.
	labelRunnerSet  = "actions-gateway.com/runner-set"
	labelAgentIndex = "actions-gateway/agent-index"
	managedByValue  = names.ControllerName
)

// Scheme selects how a Pool derives every identity it owns from its owner's name:
// the agent Secret names, the label selector that enumerates them, and the runner
// name each agent registers with GitHub.
//
// The two schemes are disjoint by construction, because a v1alpha1 RunnerGroup and a
// v2 RunnerSet of the same name share one namespace for the whole coexistence window
// of a v1→v2 migration. Before Q466 both derived the same Secret names under the same
// labels and the same GitHub runner names, so the two controllers fought over one
// agent pool: measured live on the dogfood cluster, the v1 tenant error-looped on
// `secrets "agentpool-<name>-<index>" already exists` from the moment v2 came up —
// exactly when rollback to v1 has to stay possible.
type Scheme string

const (
	// SchemeRunnerGroup is the v1alpha1 RunnerGroup derivation: Secret
	// "agentpool-<name>-<index>", selector actions-gateway/runner-group=<name>,
	// runner name "<name>-<index>". It is byte-for-byte the shipped v1 layout and
	// must stay that way: v1 is the rollback target of a migration, so it has to keep
	// finding the Secrets and GitHub registrations it already owns.
	SchemeRunnerGroup Scheme = "RunnerGroup"
	// SchemeRunnerSet is the v2 RunnerSet derivation: Secret
	// "agentpool-rs-<name>-<index>", selector actions-gateway.com/runner-set=<name>,
	// runner name "rs-<name>-<index>". v2 is the side that moves, because the v1
	// layout must stay put. An install that already has v2 agent Secrets under the
	// old shared derivation is carried across by AdoptLegacyRunnerSetSecrets rather
	// than left behind.
	SchemeRunnerSet Scheme = "RunnerSet"
)

// Recycle retry policy for the transient GitHub 422 "runner is currently
// running a job and cannot be deleted" race after a single-use JIT job
// completes (Q259). The record (and its stable name) linger for a few to tens
// of seconds until GitHub finishes auto-removing the ephemeral runner, so
// Recycle waits it out with a bounded, jittered backoff rather than failing the
// recycle and dropping the listener slot. Bounded so a runner that never
// releases cannot spin a hot loop or wedge the goroutine indefinitely; on
// give-up the caller records actions_gateway_agent_recycle_errors_total.
const (
	defaultRecycleMaxAttempts = 6
	defaultRecycleBaseBackoff = 2 * time.Second
	defaultRecycleMaxBackoff  = 15 * time.Second
)

// RegisterParams is the input to Registrar.Register.
type RegisterParams struct {
	Name      string
	Version   string
	Labels    []string
	GroupName string
	GroupID   int
}

// AgentCredentials is returned by Registrar.Register and stored in a Secret.
type AgentCredentials struct {
	AgentID          int64
	ClientID         string
	AuthorizationURL string
	BrokerURL        string
	// PrivateKeyPEM is the PKCS#8 PEM-encoded private key for this agent.
	// Set by registrars that generate the key pair server-side (e.g. JIT config).
	// Nil when the registrar expects the caller to supply its own key pair.
	PrivateKeyPEM []byte
	// EncodedJITConfig is the raw base64-encoded JIT config blob returned by
	// GitHub's generate-jitconfig endpoint. The blob is the base64 of a JSON
	// object mapping the runner config file names (".runner", ".credentials",
	// ".credentials_rsaparams") to their individually-base64-encoded contents.
	// The worker wrapper materializes these files into /home/runner/ before
	// invoking Runner.Worker. Empty for registrars that do not produce a JIT
	// blob (e.g. the stub registrar without an explicit blob set).
	EncodedJITConfig string
}

// NameConflictError is returned by Registrar.Register when the server refuses
// the registration because a runner record with the same name already exists
// (HTTP 409). The surviving record's ID is unknown to the caller — resolve it
// with Registrar.ResolveAgentID, deregister it, and retry.
type NameConflictError struct {
	Name string
}

func (e *NameConflictError) Error() string {
	return fmt.Sprintf("agentpool: runner name %q already registered", e.Name)
}

// RunnerBusyError is returned by Registrar.Deregister when GitHub refuses to
// delete a runner record because it still considers the runner to be executing
// a job (HTTP 422 "Runner … is currently running a job and cannot be deleted").
// For a single-use JIT runner this is a transient race after the job completes:
// GitHub's auto-removal of the ephemeral record has not finished, so the record
// (and its name) linger for a few to tens of seconds. Recycle treats it as
// retriable — waiting for GitHub to release the runner — rather than fatal, so a
// concurrent job burst does not collapse the pool to a single online listener
// (Q259). AgentID is the record GitHub refused to delete.
type RunnerBusyError struct {
	AgentID int64
}

func (e *RunnerBusyError) Error() string {
	return fmt.Sprintf("agentpool: runner id %d is still running a job and cannot be deleted", e.AgentID)
}

// Registrar abstracts the runner agent registration API.
type Registrar interface {
	// Register registers a new runner agent. Returns *NameConflictError when a
	// record with the same name already exists server-side.
	Register(ctx context.Context, token string, params RegisterParams) (*AgentCredentials, error)
	Deregister(ctx context.Context, token string, agentID int64) error
	// ResolveAgentID looks up the ID of a registered runner agent by name.
	// Returns 0 with a nil error when no agent has that name.
	ResolveAgentID(ctx context.Context, token, name string) (int64, error)
}

// RecycleMetrics records agent recycle outcomes. Implemented by
// runnercore.Metrics; nil disables recording.
type RecycleMetrics interface {
	IncAgentRecycle(namespace, group, trigger string)
	IncAgentRecycleError(namespace, group string)
}

// Agent holds the credentials for one pre-registered runner agent.
type Agent struct {
	Index int
	// Name is the runner name registered with GitHub for this agent, as
	// Pool.agentName derives it. Carried on the Agent so the listener sends the
	// registered name on the wire rather than re-deriving it: the derivation is
	// Scheme-dependent and the listener knows no Scheme, so a second copy of the
	// rule drifted from this one and named no registered runner for a RunnerSet
	// (Q677).
	Name          string
	AgentID       int64
	Creds         *githubapp.RunnerCredentials
	PrivateKey    crypto.Signer
	RunnerVersion string
	BrokerURL     string
	// EncodedJITConfig is the raw base64-encoded JIT config blob from GitHub.
	// Passed through to the worker Secret so Runner.Worker can read its
	// runner configuration files at startup. Empty when the agent was
	// created via a registrar that does not return a JIT blob.
	EncodedJITConfig string
}

// Pool manages the lifecycle of pre-registered runner agents for one RunnerGroup.
// It creates, loads, and deregisters agent Secrets.
type Pool struct {
	client client.Client
	// scheme selects the derivation for this pool's Secret names, selector labels,
	// and GitHub runner names. See Scheme.
	scheme    Scheme
	namespace string
	// ownerName is the owning CR's name: the RunnerGroup (v1) or RunnerSet (v2)
	// this pool belongs to. Every derived identity is built from it.
	ownerName     string
	runnerVersion string
	runnerLabels  []string
	registrar     Registrar
	keyType       KeyType

	// Metrics records recycle outcomes for the reconcile repair pass. Optional;
	// nil disables recording. Set once before first use (not guarded by mu).
	Metrics RecycleMetrics

	mu sync.Mutex
	// ownerRef is the controller OwnerReference stamped on every agent Secret this
	// pool creates, and back-filled onto the ones it already manages. Nil until the
	// reconciler calls SetOwner. Guarded by mu because Recycle runs from listener
	// goroutines while a reconcile may be refreshing it.
	ownerRef  *metav1.OwnerReference
	agents    []*Agent       // sorted by index; populated by LoadAgents or EnsureAgents
	byIndex   map[int]*Agent // index → pooled agent, mirrors agents
	available []*Agent       // agents not currently claimed
	// claimed holds the indexes of agents a listener goroutine currently holds.
	// It is keyed by the stable agent index (not the *Agent pointer) so it
	// survives reload(), which rebuilds the agents/available slices with fresh
	// pointers on every reconcile. Without it, reload() would drop the claimed
	// set, re-add in-use agents to available (double-claim) and let scale-down
	// delete a Secret out from under a running session (Q76).
	claimed map[int]bool
	// consumed holds the indexes of agents whose single-use JIT runner record
	// has been spent by a job acquisition (Q114). Like claimed it is keyed by
	// the stable index and survives reload(). A consumed agent must never
	// re-enter available un-recycled: ReleaseAgent parks it instead, and
	// EnsureAgents re-registers parked agents on the next reconcile. Cleared by
	// a successful Recycle. In-memory only — after an AGC restart a stale agent
	// is instead detected by the listener's unauthorized-at-startup heal path.
	consumed map[int]bool

	// Recycle retry policy for the transient GitHub 422 "runner still running"
	// race (Q259). Zero values select the default* constants above. sleep is a
	// test hook for a fast, deterministic backoff; nil uses a real ctx-aware
	// timer. Set once before first use (not guarded by mu).
	recycleMaxAttempts int
	recycleBaseBackoff time.Duration
	recycleMaxBackoff  time.Duration
	sleep              func(ctx context.Context, d time.Duration) error
}

// NewPool creates a Pool for the given v1alpha1 RunnerGroup, using SchemeRunnerGroup.
// runnerLabels is the label set passed to GitHub during runner registration.
// keyType selects the algorithm for newly-generated agent keys; empty defaults to KeyTypeRSA (the secure default).
func NewPool(c client.Client, namespace, groupName, runnerVersion string, runnerLabels []string, registrar Registrar, keyType KeyType) *Pool {
	return newPool(SchemeRunnerGroup, c, namespace, groupName, runnerVersion, runnerLabels, registrar, keyType)
}

// NewRunnerSetPool creates a Pool for the given v2 RunnerSet, using SchemeRunnerSet.
// It is the v2 counterpart of NewPool and differs from it only in the derivation:
// keeping the two schemes apart is what lets a RunnerGroup and a RunnerSet of the
// same name coexist in one namespace during a migration (Q466).
func NewRunnerSetPool(c client.Client, namespace, setName, runnerVersion string, runnerLabels []string, registrar Registrar, keyType KeyType) *Pool {
	return newPool(SchemeRunnerSet, c, namespace, setName, runnerVersion, runnerLabels, registrar, keyType)
}

func newPool(scheme Scheme, c client.Client, namespace, ownerName, runnerVersion string, runnerLabels []string, registrar Registrar, keyType KeyType) *Pool {
	return &Pool{
		client:        c,
		scheme:        scheme,
		namespace:     namespace,
		ownerName:     ownerName,
		runnerVersion: runnerVersion,
		runnerLabels:  runnerLabels,
		registrar:     registrar,
		keyType:       keyType,
	}
}

// secretName is the name of the agent Secret at index, per the pool's Scheme.
func (p *Pool) secretName(index int) string {
	if p.scheme == SchemeRunnerSet {
		return runnerSetSecretName(p.ownerName, index)
	}
	return runnerGroupSecretName(p.ownerName, index)
}

func runnerGroupSecretName(name string, index int) string {
	return fmt.Sprintf("agentpool-%s-%d", name, index)
}

func runnerSetSecretName(name string, index int) string {
	return fmt.Sprintf("agentpool-rs-%s-%d", name, index)
}

// agentName is the runner name registered with GitHub for the agent at index.
//
// It is disambiguated by Scheme for the same reason the Secret name is: runner names
// are unique per registration scope (org or repo), so a RunnerGroup and a RunnerSet
// sharing one would take turns deregistering each other's live record — a 409 on
// register, resolved by deleting the incumbent — leaving both tenants' listeners
// unauthorized in a loop. Splitting the Secret name alone would have moved that fight
// from Kubernetes to GitHub rather than ending it.
func (p *Pool) agentName(index int) string {
	if p.scheme == SchemeRunnerSet {
		return fmt.Sprintf("rs-%s-%d", p.ownerName, index)
	}
	return fmt.Sprintf("%s-%d", p.ownerName, index)
}

// ownerLabels is the selector identifying this pool's agent Secrets, per its Scheme.
func (p *Pool) ownerLabels() map[string]string {
	if p.scheme == SchemeRunnerSet {
		return runnerSetOwnerLabels(p.ownerName)
	}
	return map[string]string{labelManagedBy: managedByValue, labelRunnerGroup: p.ownerName}
}

func runnerSetOwnerLabels(name string) map[string]string {
	return map[string]string{labelManagedBy: managedByValue, labelRunnerSet: name}
}

// agentLabels is the full label set written on the agent Secret at index: the
// selector plus the stable agent index.
func (p *Pool) agentLabels(index int) map[string]string {
	l := p.ownerLabels()
	l[labelAgentIndex] = strconv.Itoa(index)
	return l
}

// SetOwner records the controller OwnerReference stamped on every agent Secret this
// pool creates and back-filled onto the ones it already manages, so an agent Secret's
// lifecycle is unambiguous: it names exactly one owner, and it is garbage-collected
// with that owner even when the finalizer path never runs (an AGC that crashed, or a
// namespace torn down out from under it).
//
// Call it on every reconcile, before EnsureAgents. A Pool outlives a single reconcile
// — it is cached per owner key — so an owner deleted and recreated under the same name
// would otherwise leave the pool stamping a UID that no longer exists, and a Secret
// whose owner UID is gone is garbage-collected the moment it is written.
func (p *Pool) SetOwner(ref metav1.OwnerReference) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ownerRef = &ref
}

// ownerRefs returns the OwnerReference list for a Secret this pool writes, or nil
// when no owner has been set (unit-test pools, and the load harness).
func (p *Pool) ownerRefs() []metav1.OwnerReference {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ownerRef == nil {
		return nil
	}
	return []metav1.OwnerReference{*p.ownerRef}
}

// EnsureAgents reconciles the pool to exactly count agents.
// Idempotent: safe to call on every reconcile loop.
func (p *Pool) EnsureAgents(ctx context.Context, count int32, token string) error {
	existing, err := p.listSecretMeta(ctx)
	if err != nil {
		return err
	}

	// Build index set of existing secrets. Only labels are needed here, so the
	// metadata-only list above suffices — no Secret bodies are fetched.
	existingIdx := make(map[int]bool)
	for _, s := range existing {
		idxStr := s.Labels[labelAgentIndex]
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			continue
		}
		existingIdx[idx] = true
	}

	// Back-fill the owner reference onto agent Secrets written before Q466 added one.
	// Best-effort and metadata-only: the reference is a garbage-collection backstop
	// behind the owner's finalizer, so a failure here must not stop the pool from
	// serving jobs.
	p.backfillOwnerRefs(ctx, existing)

	// Create missing agents.
	for i := int32(0); i < count; i++ {
		if existingIdx[int(i)] {
			continue
		}
		if err := p.createAgent(ctx, int(i), token); err != nil {
			return fmt.Errorf("agentpool: create agent %d: %w", i, err)
		}
	}

	// Drop now-excess agents (index >= count) from the claimable set up front, under
	// the lock, so no listener can claim one in the window between here and the reload
	// below while we tear it down. Agents that are still claimed are left in place and
	// deleted by a later reconcile once released — see the claimed skip below.
	p.dropExcessFromAvailable(count)

	// Delete excess agents.
	for _, s := range existing {
		idxStr := s.Labels[labelAgentIndex]
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			continue
		}
		if int32(idx) < count {
			continue
		}
		// Don't tear down an agent a listener goroutine still holds — deleting its
		// Secret mid-session breaks the in-flight job (Q76). A later reconcile deletes
		// it once released (the claimed set is re-checked then).
		p.mu.Lock()
		inUse := p.claimed[idx]
		p.mu.Unlock()
		if inUse {
			slog.Info("skipping scale-down delete of in-use agent; will retry after release", "index", idx)
			continue
		}
		// Fetch the body only for the specific Secret being torn down — the
		// agentId is needed to deregister the agent from GitHub.
		full, err := p.getSecret(ctx, s.Name)
		if err != nil {
			if errors.IsNotFound(err) {
				continue // already gone
			}
			return fmt.Errorf("agentpool: get secret %s: %w", s.Name, err)
		}
		agentID, _ := strconv.ParseInt(string(full.Data["agentId"]), 10, 64)
		if err := p.registrar.Deregister(ctx, token, agentID); err != nil {
			slog.Warn("failed to deregister agent; continuing", "index", idx, "agentID", agentID, "error", err)
		}
		if delErr := p.client.Delete(ctx, full); delErr != nil && !errors.IsNotFound(delErr) {
			return fmt.Errorf("agentpool: delete secret %s: %w", s.Name, delErr)
		}
	}

	// Repair consumed (parked) agents: a listener goroutine that exited after
	// its single-use JIT runner record was spent leaves the agent parked out of
	// available (Q114). Re-register them now that we hold an installation
	// token; failures are retried on the next reconcile.
	for _, a := range p.parkedAgents(count) {
		if _, err := p.Recycle(ctx, a, token); err != nil {
			slog.Warn("agentpool: recycle of parked consumed agent failed; will retry next reconcile",
				"index", a.Index, "error", err)
			if p.Metrics != nil {
				p.Metrics.IncAgentRecycleError(p.namespace, p.ownerName)
			}
			continue
		}
		if p.Metrics != nil {
			p.Metrics.IncAgentRecycle(p.namespace, p.ownerName, "reconcile_repair")
		}
	}

	// Reload agents into memory. p.client is configured with
	// Cache.DisableFor[*corev1.Secret] in production (cmd/agc/main.go, W4 /
	// H-2) and matched in the envtest suite, so the metadata list and per-name
	// Gets in reload go straight to the API server — no informer-cache lag
	// relative to the Creates above.
	return p.reload(ctx)
}

func (p *Pool) createAgent(ctx context.Context, index int, token string) error {
	creds, privKeyPEM, err := p.registerAgent(ctx, token, index)
	if err != nil {
		return err
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            p.secretName(index),
			Namespace:       p.namespace,
			Labels:          p.agentLabels(index),
			OwnerReferences: p.ownerRefs(),
		},
		Data: p.secretDataFor(index, creds, privKeyPEM),
	}
	return p.client.Create(ctx, sec)
}

// registerAgent registers the agent for index with the registrar and returns
// its credentials plus the private key PEM to persist. A 409 name conflict —
// a record under this name survives with an ID we no longer know (e.g. a
// previous Register succeeded but the Secret write crashed, or the Secret was
// deleted out-of-band) — is resolved by looking up the surviving record's ID,
// deregistering it, and retrying once.
func (p *Pool) registerAgent(ctx context.Context, token string, index int) (*AgentCredentials, []byte, error) {
	agentName := p.agentName(index)
	params := RegisterParams{
		Name:      agentName,
		Version:   p.runnerVersion,
		Labels:    p.runnerLabels,
		GroupName: p.ownerName,
		GroupID:   1,
	}
	creds, err := p.registrar.Register(ctx, token, params)
	var conflict *NameConflictError
	if stderrors.As(err, &conflict) {
		id, rerr := p.registrar.ResolveAgentID(ctx, token, agentName)
		if rerr != nil {
			return nil, nil, fmt.Errorf("resolve conflicting runner record %q: %w", agentName, rerr)
		}
		if id > 0 {
			if derr := p.registrar.Deregister(ctx, token, id); derr != nil {
				return nil, nil, fmt.Errorf("deregister conflicting runner record %q (id %d): %w", agentName, id, derr)
			}
		}
		creds, err = p.registrar.Register(ctx, token, params)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("register agent: %w", err)
	}

	privKeyPEM := creds.PrivateKeyPEM
	if len(privKeyPEM) == 0 {
		// Fallback for stub/test registrars that don't generate a key pair server-side.
		privateKey, kerr := generateKey(p.keyType)
		if kerr != nil {
			return nil, nil, fmt.Errorf("generate agent key: %w", kerr)
		}
		privKeyPEM, kerr = marshalPrivateKey(privateKey)
		if kerr != nil {
			return nil, nil, kerr
		}
	}
	return creds, privKeyPEM, nil
}

// registerAgentWithBusyRetry re-registers the agent at index, retrying while
// registration is blocked by the just-consumed single-use JIT runner still
// lingering server-side (Q259). GitHub auto-removes the ephemeral record after a
// job completes, but for a few to tens of seconds the record survives: the
// deregister of the conflicting record returns 422 ("runner is currently running
// a job and cannot be deleted", surfaced as *RunnerBusyError) and the
// re-registration under the stable name 409s. Both clear once GitHub finishes
// releasing the runner, so a bounded, jittered backoff waits it out rather than
// failing the recycle — which would drop the listener's polling slot and, under
// a concurrent burst, collapse the pool to a single online listener. The loop is
// bounded (attempts + ctx cancellation) so a runner that never releases cannot
// spin a hot loop; on give-up the RunnerBusyError is returned to the caller,
// which records actions_gateway_agent_recycle_errors_total.
func (p *Pool) registerAgentWithBusyRetry(ctx context.Context, token string, index int) (*AgentCredentials, []byte, error) {
	maxAttempts := p.recycleMaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultRecycleMaxAttempts
	}
	for attempt := 1; ; attempt++ {
		creds, privKeyPEM, err := p.registerAgent(ctx, token, index)
		if err == nil {
			return creds, privKeyPEM, nil
		}
		var busy *RunnerBusyError
		if !stderrors.As(err, &busy) || attempt >= maxAttempts {
			return nil, nil, err
		}
		slog.Debug("agentpool: recycle blocked by still-running consumed runner; backing off and retrying",
			"index", index, "attempt", attempt, "maxAttempts", maxAttempts, "error", err)
		if serr := p.recycleSleep(ctx, p.recycleBackoff(attempt)); serr != nil {
			return nil, nil, serr
		}
	}
}

// recycleBackoff returns the jittered backoff before recycle retry attempt+1,
// growing exponentially from the base delay to the per-attempt cap.
func (p *Pool) recycleBackoff(attempt int) time.Duration {
	base := p.recycleBaseBackoff
	if base <= 0 {
		base = defaultRecycleBaseBackoff
	}
	maxD := p.recycleMaxBackoff
	if maxD <= 0 {
		maxD = defaultRecycleMaxBackoff
	}
	// Exponential: base * 2^(attempt-1), capped. attempt is >= 1.
	d := base
	for i := 1; i < attempt && d < maxD; i++ {
		d *= 2
	}
	if d > maxD {
		d = maxD
	}
	// Full jitter over [d/2, d] so concurrent recyclers do not resynchronize
	// their retries into a thundering herd against GitHub.
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1)) //nolint:gosec // jitter, not crypto
}

// recycleSleep waits for d or until ctx is cancelled, returning ctx.Err() if the
// context is cancelled first. The test hook (p.sleep) overrides it for fast,
// deterministic backoff.
func (p *Pool) recycleSleep(ctx context.Context, d time.Duration) error {
	if p.sleep != nil {
		return p.sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// secretDataFor builds the Secret data map for an agent's credentials.
func (p *Pool) secretDataFor(index int, creds *AgentCredentials, privKeyPEM []byte) map[string][]byte {
	return map[string][]byte{
		"agentId":          []byte(strconv.FormatInt(creds.AgentID, 10)),
		"clientId":         []byte(creds.ClientID),
		"authorizationUrl": []byte(creds.AuthorizationURL),
		"privateKeyPEM":    privKeyPEM,
		"agentIndex":       []byte(strconv.Itoa(index)),
		"runnerVersion":    []byte(p.runnerVersion),
		"brokerURL":        []byte(creds.BrokerURL),
		"encodedJITConfig": []byte(creds.EncodedJITConfig),
	}
}

// LoadAgents reads all existing agent Secrets and returns them in index order.
// Called on AGC startup to reconstruct state after a restart.
func (p *Pool) LoadAgents(ctx context.Context) ([]*Agent, error) {
	if err := p.reload(ctx); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Agent, len(p.agents))
	copy(out, p.agents)
	return out, nil
}

// reload refreshes the in-memory agent list from Kubernetes Secrets. It
// enumerates the pool's Secrets as metadata, then fetches each body by name —
// the bodies are needed to reconstruct agent credentials, but they never flow
// through the bulk list.
func (p *Pool) reload(ctx context.Context) error {
	metas, err := p.listSecretMeta(ctx)
	if err != nil {
		return err
	}

	agents := make([]*Agent, 0, len(metas))
	for _, m := range metas {
		full, err := p.getSecret(ctx, m.Name)
		if err != nil {
			if errors.IsNotFound(err) {
				continue // deleted between list and get
			}
			return err
		}
		a, err := p.secretToAgent(*full)
		if err != nil {
			continue
		}
		agents = append(agents, a)
	}
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].Index < agents[j].Index
	})

	p.mu.Lock()
	defer p.mu.Unlock()
	p.agents = agents
	p.byIndex = make(map[int]*Agent, len(agents))
	for _, a := range agents {
		p.byIndex[a.Index] = a
	}
	// Rebuild available as every present, unclaimed, unconsumed agent — agents
	// is the single source of truth. The claimed and consumed sets are
	// preserved across the reload (keyed by stable index, not pointer) so a
	// currently-held agent never reappears in available (Q76) and a spent
	// single-use agent stays parked until recycled (Q114); entries whose Secret
	// has since been deleted are pruned out.
	newClaimed := make(map[int]bool, len(p.claimed))
	newConsumed := make(map[int]bool, len(p.consumed))
	p.available = make([]*Agent, 0, len(agents))
	for _, a := range agents {
		if p.consumed[a.Index] {
			newConsumed[a.Index] = true
		}
		if p.claimed[a.Index] {
			newClaimed[a.Index] = true
			continue
		}
		if newConsumed[a.Index] {
			continue // parked: repaired by EnsureAgents, not claimable
		}
		p.available = append(p.available, a)
	}
	p.claimed = newClaimed
	p.consumed = newConsumed
	return nil
}

// parkedAgents returns the agents whose JIT runner record is consumed and that
// no listener goroutine currently holds, limited to indexes below count (an
// excess parked agent is deleted by scale-down instead of recycled).
func (p *Pool) parkedAgents(count int32) []*Agent {
	p.mu.Lock()
	defer p.mu.Unlock()
	var parked []*Agent
	for idx := range p.consumed {
		if p.claimed[idx] || int32(idx) >= count {
			continue
		}
		if a, ok := p.byIndex[idx]; ok {
			parked = append(parked, a)
		}
	}
	return parked
}

// dropExcessFromAvailable removes agents with index >= count from the available
// set under the lock, so they cannot be claimed while EnsureAgents tears them
// down. It does not touch the claimed set: an excess agent that is still claimed
// stays claimed and is deleted by a later reconcile once released.
func (p *Pool) dropExcessFromAvailable(count int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	kept := p.available[:0]
	for _, a := range p.available {
		if int32(a.Index) < count {
			kept = append(kept, a)
		}
	}
	p.available = kept
}

// ClaimAgent atomically marks an agent as in-use and returns it.
// Returns nil if no agent is currently available.
func (p *Pool) ClaimAgent() *Agent {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.available) == 0 {
		return nil
	}
	a := p.available[0]
	p.available = p.available[1:]
	if p.claimed == nil {
		p.claimed = make(map[int]bool)
	}
	p.claimed[a.Index] = true
	return a
}

// ReleaseAgent returns an agent to the available pool. It re-adds the current
// pooled agent for the index (not the caller's possibly-stale snapshot), and
// only if that index still exists in the pool — a concurrent scale-down or
// DeleteAll may have removed it while it was claimed, in which case the release
// is a no-op beyond clearing the claim. A consumed agent (single-use JIT
// runner record spent, not yet recycled) is parked instead of re-added: its
// credentials are dead, so handing it to another listener would burn that
// listener too (Q114). EnsureAgents recycles parked agents on the next
// reconcile.
func (p *Pool) ReleaseAgent(a *Agent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.claimed, a.Index)
	if p.consumed[a.Index] {
		return
	}
	if cur, ok := p.byIndex[a.Index]; ok {
		p.available = append(p.available, cur)
	}
}

// MarkConsumed records that the agent's single-use JIT runner record has been
// spent by a job acquisition. GitHub deletes the record server-side, so the
// stored credentials are dead until the agent is recycled. Called by the
// listener goroutine immediately after AcquireJob succeeds.
func (p *Pool) MarkConsumed(a *Agent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.consumed == nil {
		p.consumed = make(map[int]bool)
	}
	p.consumed[a.Index] = true
}

// Recycle re-registers the agent at a's index under its stable name after its
// single-use JIT runner record was consumed (Q114): it deregisters the old
// record (best-effort — GitHub has usually deleted it already), registers a
// fresh one (resolving a 409 name conflict via registerAgent), rewrites the
// agent Secret in place, and swaps the in-memory entry. The caller's claim, if
// any, is preserved; the consumed mark is cleared. The authoritative old agent
// ID is read from the pool's current entry, not from a — the caller's pointer
// may predate an earlier recycle.
//
// Registration is retried with a bounded, jittered backoff while it is blocked
// by the just-consumed runner still lingering server-side — GitHub's 422 "runner
// is currently running a job and cannot be deleted" race after job completion
// (Q259). Without the retry a concurrent job burst collapses the pool to a
// single online listener, because each replacement listener that finishes a job
// fails its recycle and drops its polling slot. On give-up (the runner never
// releases within the bound) the error is returned and the caller records
// actions_gateway_agent_recycle_errors_total.
func (p *Pool) Recycle(ctx context.Context, a *Agent, token string) (*Agent, error) {
	idx := a.Index
	p.mu.Lock()
	old := p.byIndex[idx]
	p.mu.Unlock()
	if old == nil {
		old = a
	}

	if old.AgentID > 0 {
		if err := p.registrar.Deregister(ctx, token, old.AgentID); err != nil {
			slog.Debug("agentpool: deregister of consumed agent record failed (usually already deleted server-side)",
				"index", idx, "agentID", old.AgentID, "error", err)
		}
	}

	creds, privKeyPEM, err := p.registerAgentWithBusyRetry(ctx, token, idx)
	if err != nil {
		return nil, fmt.Errorf("agentpool: recycle agent %d: %w", idx, err)
	}

	sec, err := p.getSecret(ctx, p.secretName(idx))
	if err != nil {
		if !errors.IsNotFound(err) {
			return nil, fmt.Errorf("agentpool: recycle agent %d: get secret: %w", idx, err)
		}
		// Secret deleted out-of-band (scale-down raced us): recreate it so the
		// fresh registration is not orphaned.
		sec = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            p.secretName(idx),
				Namespace:       p.namespace,
				Labels:          p.agentLabels(idx),
				OwnerReferences: p.ownerRefs(),
			},
			Data: p.secretDataFor(idx, creds, privKeyPEM),
		}
		if cerr := p.client.Create(ctx, sec); cerr != nil {
			return nil, fmt.Errorf("agentpool: recycle agent %d: recreate secret: %w", idx, cerr)
		}
	} else {
		sec.Data = p.secretDataFor(idx, creds, privKeyPEM)
		if uerr := p.client.Update(ctx, sec); uerr != nil {
			return nil, fmt.Errorf("agentpool: recycle agent %d: update secret: %w", idx, uerr)
		}
	}

	fresh, err := p.secretToAgent(*sec)
	if err != nil {
		return nil, fmt.Errorf("agentpool: recycle agent %d: %w", idx, err)
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.consumed, idx)
	if _, ok := p.byIndex[idx]; ok {
		p.byIndex[idx] = fresh
		for i, cur := range p.agents {
			if cur.Index == idx {
				p.agents[i] = fresh
				break
			}
		}
		// Deliberately not appended to available: a claimed agent stays with its
		// holder, and a parked one is surfaced by the reload() EnsureAgents runs
		// after its repair pass.
	}
	return fresh, nil
}

// DeleteAll deregisters all agents from GitHub and deletes all Secrets.
// Called when a RunnerGroup is deleted.
func (p *Pool) DeleteAll(ctx context.Context, token string) error {
	metas, err := p.listSecretMeta(ctx)
	if err != nil {
		return err
	}
	var lastErr error
	for _, m := range metas {
		full, err := p.getSecret(ctx, m.Name)
		if err != nil {
			if errors.IsNotFound(err) {
				continue // already gone
			}
			lastErr = err
			continue
		}
		agentID, _ := strconv.ParseInt(string(full.Data["agentId"]), 10, 64)
		if agentID > 0 {
			_ = p.registrar.Deregister(ctx, token, agentID) // best-effort
		}
		if delErr := p.client.Delete(ctx, full); delErr != nil && !errors.IsNotFound(delErr) {
			lastErr = delErr
		}
	}
	p.mu.Lock()
	p.agents = nil
	p.byIndex = nil
	p.available = nil
	p.claimed = nil
	p.consumed = nil
	p.mu.Unlock()
	return lastErr
}

// listSecretMeta enumerates this pool's agent Secrets as metadata only.
//
// It deliberately uses a PartialObjectMetadataList so the bulk enumeration —
// run on every reconcile (EnsureAgents, reload) — never pulls Secret bodies
// (agent private keys, JIT configs) over the wire or into process memory. Only
// names and labels come back. The few paths that need the body (reload's
// in-memory rebuild, deregistration on scale-down/delete) fetch it per-name via
// getSecret. This keeps the GitHub App / agent credential material off the
// bulk-list path entirely (k8s-best-practices.md §B B4).
//
// The list carries an explicit Secret GVK. The AGC manager client is
// configured with Cache.DisableFor[*corev1.Secret] (cmd/agc/main.go); when
// controller-runtime's shouldBypassCache strips the "List" suffix it matches
// that disabled GVK, so this read bypasses the cache and starts no Secret
// (metadata) informer — preserving the W3/H-2 "no Secret data buffered in the
// controller-runtime cache" property.
func (p *Pool) listSecretMeta(ctx context.Context) ([]metav1.PartialObjectMetadata, error) {
	var list metav1.PartialObjectMetadataList
	list.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := p.client.List(ctx, &list,
		client.InNamespace(p.namespace),
		client.MatchingLabels(p.ownerLabels()),
	); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// getSecret fetches a single agent Secret body by name. Like the metadata list
// above, this Get bypasses the controller-runtime cache (DisableFor) and hits
// the API server directly, so the body is held in process only for the
// duration of the call.
func (p *Pool) getSecret(ctx context.Context, name string) (*corev1.Secret, error) {
	var sec corev1.Secret
	if err := p.client.Get(ctx, client.ObjectKey{Namespace: p.namespace, Name: name}, &sec); err != nil {
		return nil, err
	}
	return &sec, nil
}

// backfillOwnerRefs stamps this pool's owner reference onto any agent Secret in
// metas that does not already carry it — the ones written before Q466, and any
// written while no owner was set.
//
// It patches metadata only: the merge patch is built from an empty Secret shell, so
// no agent private key or JIT config is ever fetched, preserving the bulk-path
// property listSecretMeta exists for. Errors are logged and swallowed: the reference
// is a GC backstop behind the owner's own finalizer, and failing the reconcile over
// it would take a working tenant down for a metadata cleanup.
func (p *Pool) backfillOwnerRefs(ctx context.Context, metas []metav1.PartialObjectMetadata) {
	refs := p.ownerRefs()
	if len(refs) == 0 {
		return
	}
	for _, m := range metas {
		if hasOwnerRef(m.OwnerReferences, refs[0]) {
			continue
		}
		patch := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:            m.Name,
				Namespace:       p.namespace,
				OwnerReferences: refs,
			},
		}
		if err := p.client.Patch(ctx, patch, client.Merge); err != nil && !errors.IsNotFound(err) {
			slog.Warn("agentpool: failed to back-fill owner reference on agent Secret",
				"namespace", p.namespace, "secret", m.Name, "error", err)
		}
	}
}

// hasOwnerRef reports whether refs already contains want, matched on the identity
// fields the API server persists (UID alone is enough to be the same object).
func hasOwnerRef(refs []metav1.OwnerReference, want metav1.OwnerReference) bool {
	for _, r := range refs {
		if r.UID == want.UID && r.Kind == want.Kind && r.Name == want.Name {
			return true
		}
	}
	return false
}

// secretToAgent decodes an agent Secret. A method rather than a free function so
// every decoded Agent carries the Scheme-derived registered Name (Q677); a caller
// cannot construct one without it.
func (p *Pool) secretToAgent(s corev1.Secret) (*Agent, error) {
	idxStr := string(s.Data["agentIndex"])
	if s.Labels != nil && s.Labels[labelAgentIndex] != "" {
		idxStr = s.Labels[labelAgentIndex]
	}
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		return nil, fmt.Errorf("parse agent index: %w", err)
	}
	agentID, _ := strconv.ParseInt(string(s.Data["agentId"]), 10, 64)

	privKey, err := parsePrivateKeySigner(s.Data["privateKeyPEM"])
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}

	return &Agent{
		Index:   idx,
		Name:    p.agentName(idx),
		AgentID: agentID,
		Creds: &githubapp.RunnerCredentials{
			ClientID:         string(s.Data["clientId"]),
			AuthorizationURL: string(s.Data["authorizationUrl"]),
		},
		PrivateKey:       privKey,
		RunnerVersion:    string(s.Data["runnerVersion"]),
		BrokerURL:        string(s.Data["brokerURL"]),
		EncodedJITConfig: string(s.Data["encodedJITConfig"]),
	}, nil
}
