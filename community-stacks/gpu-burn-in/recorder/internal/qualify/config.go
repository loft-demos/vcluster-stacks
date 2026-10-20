package qualify

import (
	"fmt"
	"strings"
)

// Target is where a result is recorded.
type Target string

const (
	// TargetMachine labels the vCluster Platform Machine through the management API. It is
	// provider-neutral, but needs a Platform build that lets users label Machines.
	TargetMachine Target = "machine"
	// TargetBareMetalHost labels the Metal3 BareMetalHost on the cluster that runs Metal3. It works
	// on any Platform build; the Platform mirrors the labels to the Machine.
	TargetBareMetalHost Target = "bmh"
)

// Config is the controller-wide policy. Per-run choices come from the StackInstance parameters
// (see RunParams).
type Config struct {
	// TemplateNames are the StackTemplates whose instances are recorded.
	TemplateNames []string
	// LabelDomain prefixes the result label and annotations: <domain>/result and so on.
	LabelDomain string
	Target      Target
	// LocalCluster is the Platform cluster name of the cluster this controller runs in. A Metal3
	// provider whose clusterRef names it uses the controller's own client.
	LocalCluster string
	// ClusterKubeconfigDir holds one kubeconfig file per other Platform cluster, named after it.
	ClusterKubeconfigDir string
	// NetBox tags, used when a run sets netboxUpdate. The device comes from the Machine's NetBox
	// annotations, and the token from the controller's NetBox client.
	NetBoxAdmitTag     string
	NetBoxQualifiedTag string
	// DryRun logs every write instead of making it.
	DryRun bool
}

// Validate rejects settings that would record nothing useful or nothing at all.
func (c *Config) Validate() error {
	if len(c.TemplateNames) == 0 {
		return fmt.Errorf("no StackTemplate names to watch")
	}
	if c.LabelDomain == "" {
		return fmt.Errorf("label domain is empty")
	}
	// Newer Platform builds keep labels in these domains out of NodeType selection, so a result
	// recorded there could never gate allocation.
	d := c.LabelDomain
	for _, reserved := range []string{"vcluster.com", "loft.sh"} {
		if d == reserved || strings.HasSuffix(d, "."+reserved) {
			return fmt.Errorf("label domain %q: NodeType selection ignores labels in %s domains", d, reserved)
		}
	}
	switch c.Target {
	case TargetMachine, TargetBareMetalHost:
	default:
		return fmt.Errorf("target %q: want %q or %q", c.Target, TargetMachine, TargetBareMetalHost)
	}
	return nil
}

func (c *Config) key(name string) string { return c.LabelDomain + "/" + name }

func (c *Config) watches(template string) bool {
	for _, n := range c.TemplateNames {
		if n == template {
			return true
		}
	}
	return false
}
