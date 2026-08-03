package utils

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
)

func warnError(err error) {
	_, _ = fmt.Fprintf(GinkgoWriter, "warning: %v\n", err)
}

// Run executes the provided command within this context.
//
// On success it returns stdout ALONE. Callers feed that straight back into the
// next command as a value — a jsonpath result, a resource name — so anything
// kubectl writes to stderr must not reach them: a CRD deprecationWarning is
// emitted on every read of a deprecated version, and folding it in spliced the
// notice into label selectors and rendered manifests (Q633).
//
// On failure it returns both streams. Diagnosis wants everything, and an
// admission rejection an e2e spec asserts on arrives on stderr.
func Run(cmd *exec.Cmd) (string, error) {
	dir, _ := GetProjectDir()
	cmd.Dir = dir

	if err := os.Chdir(cmd.Dir); err != nil {
		_, _ = fmt.Fprintf(GinkgoWriter, "chdir dir: %q\n", err)
	}

	cmd.Env = append(os.Environ(), "GO111MODULE=on")
	command := strings.Join(cmd.Args, " ")
	_, _ = fmt.Fprintf(GinkgoWriter, "running: %q\n", command)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	// Surface stderr even on success — a deprecation warning is worth seeing in
	// the spec's output, just not worth splicing into its data.
	if stderr.Len() > 0 {
		_, _ = fmt.Fprintf(GinkgoWriter, "stderr: %s\n", stderr.String())
	}
	if err != nil {
		both := stdout.String() + stderr.String()
		return both, fmt.Errorf("%q failed with error %q: %w", command, both, err)
	}

	return stdout.String(), nil
}

// UninstallCertManager uninstalls cert-manager via the Makefile target.
func UninstallCertManager() {
	if _, err := Run(exec.Command("make", "uninstall-cert-manager")); err != nil {
		warnError(err)
	}
}

// InstallCertManager installs cert-manager and waits for all components to be
// ready via the Makefile target. The version is defined in cmd/gmc/Makefile.
func InstallCertManager() error {
	_, err := Run(exec.Command("make", "install-cert-manager"))
	return err
}

// IsCertManagerCRDsInstalled checks if any Cert Manager CRDs are installed
// by verifying the existence of key CRDs related to Cert Manager.
func IsCertManagerCRDsInstalled() bool {
	// List of common Cert Manager CRDs
	certManagerCRDs := []string{
		"certificates.cert-manager.io",
		"issuers.cert-manager.io",
		"clusterissuers.cert-manager.io",
		"certificaterequests.cert-manager.io",
		"orders.acme.cert-manager.io",
		"challenges.acme.cert-manager.io",
	}

	// Execute the kubectl command to get all CRDs
	cmd := exec.Command("kubectl", "get", "crds")
	output, err := Run(cmd)
	if err != nil {
		return false
	}

	// Check if any of the Cert Manager CRDs are present
	crdList := GetNonEmptyLines(output)
	for _, crd := range certManagerCRDs {
		for _, line := range crdList {
			if strings.Contains(line, crd) {
				return true
			}
		}
	}

	return false
}

// GetNonEmptyLines converts given command output string into individual objects
// according to line breakers, and ignores the empty elements in it.
func GetNonEmptyLines(output string) []string {
	var res []string
	elements := strings.SplitSeq(output, "\n")
	for element := range elements {
		if element != "" {
			res = append(res, element)
		}
	}

	return res
}

// GetProjectDir will return the directory where the project is
func GetProjectDir() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return wd, fmt.Errorf("failed to get current working directory: %w", err)
	}
	wd = strings.ReplaceAll(wd, "/test/e2e", "")
	return wd, nil
}
