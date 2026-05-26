// Package agentpool manages pre-registered runner agent credentials for a RunnerGroup.
package agentpool

import (
	"context"
	"crypto"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"

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
	labelAgentIndex  = "actions-gateway/agent-index"
	managedByValue   = names.ControllerName
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
}

// Registrar abstracts the runner agent registration API.
type Registrar interface {
	Register(ctx context.Context, token string, params RegisterParams) (*AgentCredentials, error)
	Deregister(ctx context.Context, token string, agentID int64) error
}

// Agent holds the credentials for one pre-registered runner agent.
type Agent struct {
	Index         int
	AgentID       int64
	Creds         *githubapp.RunnerCredentials
	PrivateKey    crypto.Signer
	RunnerVersion string
	BrokerURL     string
}

// Pool manages the lifecycle of pre-registered runner agents for one RunnerGroup.
// It creates, loads, and deregisters agent Secrets.
type Pool struct {
	client        client.Client
	namespace     string
	groupName     string
	runnerVersion string
	registrar     Registrar
	keyType       KeyType

	mu        sync.Mutex
	agents    []*Agent // sorted by index; populated by LoadAgents or EnsureAgents
	available []*Agent // agents not currently claimed
}

// NewPool creates a Pool for the given RunnerGroup.
// keyType selects the algorithm for newly-generated agent keys; empty defaults to KeyTypeEd25519.
func NewPool(c client.Client, namespace, groupName, runnerVersion string, registrar Registrar, keyType KeyType) *Pool {
	return &Pool{
		client:        c,
		namespace:     namespace,
		groupName:     groupName,
		runnerVersion: runnerVersion,
		registrar:     registrar,
		keyType:       keyType,
	}
}

func (p *Pool) secretName(index int) string {
	return fmt.Sprintf("agentpool-%s-%d", p.groupName, index)
}

// EnsureAgents reconciles the pool to exactly count agents.
// Idempotent: safe to call on every reconcile loop.
func (p *Pool) EnsureAgents(ctx context.Context, count int32, token string) error {
	existing, err := p.listSecrets(ctx)
	if err != nil {
		return err
	}

	// Build index set of existing secrets.
	existingIdx := make(map[int]bool)
	for _, s := range existing {
		idxStr := s.Labels[labelAgentIndex]
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			continue
		}
		existingIdx[idx] = true
	}

	// Create missing agents.
	for i := int32(0); i < count; i++ {
		if existingIdx[int(i)] {
			continue
		}
		if err := p.createAgent(ctx, int(i), token); err != nil {
			return fmt.Errorf("agentpool: create agent %d: %w", i, err)
		}
	}

	// Delete excess agents.
	for _, s := range existing {
		idxStr := s.Labels[labelAgentIndex]
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			continue
		}
		if int32(idx) >= count {
			// TODO(milestone-3): pool claim/reload race — a listener goroutine may have
			// claimed this agent; deleting its Secret while it is in use will cause the
			// goroutine's next CreateSession to fail. Add a claimed-set guard before M3.
			agentID, _ := strconv.ParseInt(string(s.Data["agentId"]), 10, 64)
			a := agentFromSecret(s, agentID, idx)
			if err := p.registrar.Deregister(ctx, token, a.AgentID); err != nil {
				slog.Warn("failed to deregister agent; continuing", "index", a.Index, "agentID", a.AgentID, "error", err)
			}
			sec := s // capture
			if delErr := p.client.Delete(ctx, &sec); delErr != nil && !errors.IsNotFound(delErr) {
				return fmt.Errorf("agentpool: delete secret %s: %w", s.Name, delErr)
			}
		}
	}

	// Reload agents into memory.
	return p.reload(ctx)
}

func (p *Pool) createAgent(ctx context.Context, index int, token string) error {
	agentName := fmt.Sprintf("%s-%d", p.groupName, index)
	params := RegisterParams{
		Name:      agentName,
		Version:   p.runnerVersion,
		Labels:    []string{},
		GroupName: p.groupName,
		GroupID:   1,
	}
	creds, err := p.registrar.Register(ctx, token, params)
	if err != nil {
		return fmt.Errorf("register agent: %w", err)
	}

	privKeyPEM := creds.PrivateKeyPEM
	if len(privKeyPEM) == 0 {
		// Fallback for stub/test registrars that don't generate a key pair server-side.
		privateKey, err := generateKey(p.keyType)
		if err != nil {
			return fmt.Errorf("generate agent key: %w", err)
		}
		privKeyPEM, err = marshalPrivateKey(privateKey)
		if err != nil {
			return err
		}
	}

	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.secretName(index),
			Namespace: p.namespace,
			Labels: map[string]string{
				labelManagedBy:   managedByValue,
				labelRunnerGroup: p.groupName,
				labelAgentIndex:  strconv.Itoa(index),
			},
		},
		Data: map[string][]byte{
			"agentId":          []byte(strconv.FormatInt(creds.AgentID, 10)),
			"clientId":         []byte(creds.ClientID),
			"authorizationUrl": []byte(creds.AuthorizationURL),
			"privateKeyPEM":    privKeyPEM,
			"agentIndex":       []byte(strconv.Itoa(index)),
			"runnerVersion":    []byte(p.runnerVersion),
			"brokerURL":        []byte(creds.BrokerURL),
		},
	}
	return p.client.Create(ctx, sec)
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

// reload refreshes the in-memory agent list from Kubernetes Secrets.
func (p *Pool) reload(ctx context.Context) error {
	secrets, err := p.listSecrets(ctx)
	if err != nil {
		return err
	}

	agents := make([]*Agent, 0, len(secrets))
	for _, s := range secrets {
		a, err := secretToAgent(s)
		if err != nil {
			continue
		}
		agents = append(agents, a)
	}
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].Index < agents[j].Index
	})

	p.mu.Lock()
	p.agents = agents
	p.available = make([]*Agent, len(agents))
	copy(p.available, agents)
	p.mu.Unlock()
	return nil
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
	return a
}

// ReleaseAgent returns an agent to the available pool.
func (p *Pool) ReleaseAgent(a *Agent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.available = append(p.available, a)
}

// DeleteAll deregisters all agents from GitHub and deletes all Secrets.
// Called when a RunnerGroup is deleted.
func (p *Pool) DeleteAll(ctx context.Context, token string) error {
	secrets, err := p.listSecrets(ctx)
	if err != nil {
		return err
	}
	var lastErr error
	for _, s := range secrets {
		agentID, _ := strconv.ParseInt(string(s.Data["agentId"]), 10, 64)
		if agentID > 0 {
			_ = p.registrar.Deregister(ctx, token, agentID) // best-effort
		}
		sec := s
		if delErr := p.client.Delete(ctx, &sec); delErr != nil && !errors.IsNotFound(delErr) {
			lastErr = delErr
		}
	}
	p.mu.Lock()
	p.agents = nil
	p.available = nil
	p.mu.Unlock()
	return lastErr
}

// listSecrets returns all agent Secrets for this pool.
func (p *Pool) listSecrets(ctx context.Context) ([]corev1.Secret, error) {
	var list corev1.SecretList
	if err := p.client.List(ctx, &list,
		client.InNamespace(p.namespace),
		client.MatchingLabels{
			labelManagedBy:   managedByValue,
			labelRunnerGroup: p.groupName,
		},
	); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// agentFromSecret creates a minimal Agent with only the fields needed for deregistration.
func agentFromSecret(s corev1.Secret, agentID int64, index int) *Agent {
	return &Agent{
		Index:   index,
		AgentID: agentID,
	}
}

func secretToAgent(s corev1.Secret) (*Agent, error) {
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
		AgentID: agentID,
		Creds: &githubapp.RunnerCredentials{
			ClientID:         string(s.Data["clientId"]),
			AuthorizationURL: string(s.Data["authorizationUrl"]),
		},
		PrivateKey:    privKey,
		RunnerVersion: string(s.Data["runnerVersion"]),
		BrokerURL:     string(s.Data["brokerURL"]),
	}, nil
}
