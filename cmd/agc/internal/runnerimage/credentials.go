package runnerimage

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Credential is one registry login.
type Credential struct {
	Username string
	Password string
}

// Credentials maps a registry host to the login to present there, in the shape a
// kubernetes.io/dockerconfigjson Secret carries. Empty means anonymous.
type Credentials map[string]Credential

// ParseDockerConfigJSON reads the auths map of a .dockerconfigjson payload and merges
// it into c, later entries winning. A key may be a bare host, a host with a scheme
// or path (Docker Hub's legacy https://index.docker.io/v1/), and carry either an
// auth field (base64 user:password) or a username/password pair.
func (c Credentials) ParseDockerConfigJSON(data []byte) error {
	var doc struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse dockerconfigjson: %w", err)
	}
	for key, entry := range doc.Auths {
		cred := Credential{Username: entry.Username, Password: entry.Password}
		if entry.Auth != "" {
			raw, err := base64.StdEncoding.DecodeString(entry.Auth)
			if err != nil {
				return fmt.Errorf("parse dockerconfigjson: auth for %q is not base64: %w", key, err)
			}
			user, pass, ok := strings.Cut(string(raw), ":")
			if !ok {
				return fmt.Errorf("parse dockerconfigjson: auth for %q is not user:password", key)
			}
			cred = Credential{Username: user, Password: pass}
		}
		c[normalizeAuthKey(key)] = cred
	}
	return nil
}

// normalizeAuthKey reduces an auths key to its host. Docker Hub's index.docker.io
// spellings all map to docker.io, which is the registry ParseReference reports.
func normalizeAuthKey(key string) string {
	host := key
	if strings.Contains(key, "://") {
		if u, err := url.Parse(key); err == nil && u.Host != "" {
			host = u.Host
		}
	} else if i := strings.IndexByte(key, '/'); i >= 0 {
		host = key[:i]
	}
	switch host {
	case "index.docker.io", "registry-1.docker.io":
		return "docker.io"
	}
	return host
}

// lookup returns the login for a registry, false when none is configured.
func (c Credentials) lookup(registry string) (Credential, bool) {
	cred, ok := c[registry]
	return cred, ok
}
