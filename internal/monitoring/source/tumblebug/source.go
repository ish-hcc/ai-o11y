package tumblebug

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/rs/zerolog/log"

	"github.com/cloud-barista/ai-o11y/internal/monitoring"
)

// ProviderName is the name the adapter is registered under.
const ProviderName = "tumblebug"

// KeyField is the CB-Tumblebug field a target's EntityID holds: the node Uid,
// which CB-Tumblebug issues when it creates the node and never changes. A
// re-created node gets a new Uid and is a new target.
const KeyField = "NodeInfo.Uid"

// ScopeNamespace narrows a listing to one CB-Tumblebug namespace.
const ScopeNamespace = ExtraNamespace

// KindVM is the kind of every target this adapter lists.
const KindVM = "vm"

// Keys of Target.Extra this adapter fills.
const (
	ExtraNamespace     = "nsId"
	ExtraInfra         = "infraId"
	ExtraNode          = "nodeId"
	ExtraConnection    = "connectionName"
	ExtraZone          = "zone"
	ExtraCSPResourceID = "cspResourceId"
	ExtraOSType        = "osType"
)

// Source is the management platform adapter for CB-Tumblebug.
type Source struct {
	client *Client
}

// NewSource builds a Source over client.
func NewSource(client *Client) *Source {
	return &Source{client: client}
}

var _ monitoring.TargetSource = (*Source)(nil)

// KeyField implements monitoring.TargetSource.
func (*Source) KeyField() string { return KeyField }

// List returns every node, or the nodes of the namespace scope names. A node
// without a Uid cannot be told apart from a re-created one, so it is left out
// and logged.
func (s *Source) List(ctx context.Context, scope monitoring.Scope) ([]monitoring.Target, error) {
	namespaces, err := s.namespaces(ctx, scope)
	if err != nil {
		return nil, err
	}
	var targets []monitoring.Target
	specs := specCache{}
	for _, nsID := range namespaces {
		nodes, err := s.client.listNodes(ctx, nsID)
		if err != nil {
			return nil, fmt.Errorf("failed to list nodes of namespace %q: %w", nsID, err)
		}
		for _, n := range nodes {
			t := toTarget(nsID, n)
			if err := t.Validate(); err != nil {
				log.Warn().Str("nsId", nsID).Str("infraId", n.InfraID).Str("nodeId", n.ID).
					Err(err).Msg("Skipping CB-Tumblebug node")
				continue
			}
			s.fillGPU(ctx, specs, &t, n)
			targets = append(targets, t)
		}
	}
	return targets, nil
}

// msgNodeNotFound is how CB-Tumblebug (v0.13.5) says a node is not there. It
// answers a missing node with HTTP 500, the status it also uses for its own
// failures, so the message is what tells the two apart: a failure to read its
// store says "failed to get GetNodeObject" instead (core/infra/manage_info.go,
// GetNodeObjectWithNodeGroups). If CB-Tumblebug rewords it, Exists reports an
// error and nothing is dropped.
const msgNodeNotFound = "no Node found"

// Exists implements monitoring.TargetSource. It asks CB-Tumblebug for the node
// t was listed as. The node is gone when CB-Tumblebug answers 404, or 500 with
// msgNodeNotFound, or when the node under that id now has another Uid, which
// means it was re-created. Any other answer is an error.
func (s *Source) Exists(ctx context.Context, t monitoring.Target) (bool, error) {
	nsID, infraID, nodeID := t.Extra[ExtraNamespace], t.Extra[ExtraInfra], t.Extra[ExtraNode]
	if nsID == "" || infraID == "" || nodeID == "" {
		return false, fmt.Errorf("target %q has no %s, %s and %s to look it up by",
			t.EntityID, ExtraNamespace, ExtraInfra, ExtraNode)
	}
	n, err := s.client.getNode(ctx, nsID, infraID, nodeID)
	if err != nil {
		if nodeGone(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check node %s/%s/%s: %w", nsID, infraID, nodeID, err)
	}
	return n.UID == t.EntityID, nil
}

// Fetch implements monitoring.TargetSource: ref holds the node's nsId,
// infraId and nodeId.
func (s *Source) Fetch(ctx context.Context, ref map[string]string) (monitoring.Target, error) {
	nsID, infraID, nodeID := ref[ExtraNamespace], ref[ExtraInfra], ref[ExtraNode]
	if nsID == "" || infraID == "" || nodeID == "" {
		return monitoring.Target{}, fmt.Errorf("%w: %s, %s and %s are all needed", ErrBadRef, ExtraNamespace, ExtraInfra, ExtraNode)
	}
	n, err := s.client.getNode(ctx, nsID, infraID, nodeID)
	if err != nil {
		if nodeGone(err) {
			return monitoring.Target{}, fmt.Errorf("node %s/%s/%s: %w", nsID, infraID, nodeID, monitoring.ErrTargetNotFound)
		}
		return monitoring.Target{}, fmt.Errorf("failed to get node %s/%s/%s: %w", nsID, infraID, nodeID, err)
	}
	if n.InfraID == "" {
		n.InfraID = infraID
	}
	t := toTarget(nsID, n)
	if err := t.Validate(); err != nil {
		return monitoring.Target{}, fmt.Errorf("node %s/%s/%s: %w", nsID, infraID, nodeID, err)
	}
	s.fillGPU(ctx, specCache{}, &t, n)
	return t, nil
}

// ErrBadRef is returned by Fetch for a reference missing a name.
var ErrBadRef = errors.New("incomplete node reference")

func nodeGone(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.StatusCode {
	case http.StatusNotFound:
		return true
	case http.StatusInternalServerError:
		return strings.Contains(apiErr.Message, msgNodeNotFound)
	default:
		return false
	}
}

func (s *Source) namespaces(ctx context.Context, scope monitoring.Scope) ([]string, error) {
	if nsID := scope[ScopeNamespace]; nsID != "" {
		return []string{nsID}, nil
	}
	all, err := s.client.listNamespaces(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list namespaces: %w", err)
	}
	ids := make([]string, 0, len(all))
	for _, ns := range all {
		ids = append(ids, ns.ID)
	}
	return ids, nil
}

func toTarget(nsID string, n node) monitoring.Target {
	t := monitoring.Target{
		EntityID: n.UID,
		Kind:     KindVM,
		Name:     n.Name,
		Provider: n.ConnectionConfig.ProviderName,
		Region:   n.Region.Region,
		OS:       osFamily(n.Image.OSType, n.Image.OSDistribution),
		Extra: map[string]string{
			ExtraNamespace: nsID,
			ExtraInfra:     n.InfraID,
			ExtraNode:      n.ID,
		},
	}
	setGPU(&t, n.Spec)
	for key, value := range map[string]string{
		ExtraConnection:    n.ConnectionName,
		ExtraZone:          n.Region.Zone,
		ExtraCSPResourceID: n.CSPResourceID,
		ExtraOSType:        n.Image.OSType,
	} {
		if value != "" {
			t.Extra[key] = value
		}
	}
	return t
}

// setGPU copies the GPUs of spec to t when spec says its accelerator is a GPU.
// The model is kept even when the count is not known (0): it alone names the
// vendor, which is what chooses a GPU receiver.
func setGPU(t *monitoring.Target, spec accelerator) {
	if !strings.EqualFold(spec.AcceleratorType, "gpu") {
		return
	}
	t.GPUCount = max(spec.AcceleratorCount, 0)
	t.GPUModel = spec.AcceleratorModel
}

// specCache holds the specs read during one List or Fetch, by specId, so that
// nodes of the same spec cost one request. A failed read is held too, as the
// zero accelerator, and not retried within the call.
type specCache map[string]accelerator

// fillGPU reads the GPUs of node n from its spec when the node's own spec
// does not name an accelerator, which is how CB-Tumblebug (v0.13.8) lists
// nodes (see accelerator).
//
// A spec that cannot be read leaves t without GPUs and is logged; it does not
// fail the listing or the registration. The GPU fields only add a GPU
// receiver to the target's collector configuration: without them the target
// is still monitored with host metrics, and the next sync, which replaces the
// target's description with what CB-Tumblebug lists, fills them in once the
// spec answers. Failing instead would keep the whole namespace, or the node
// being registered, out of monitoring over a detail.
func (s *Source) fillGPU(ctx context.Context, specs specCache, t *monitoring.Target, n node) {
	if n.Spec.AcceleratorType != "" || n.SpecID == "" {
		return
	}
	spec, ok := specs[n.SpecID]
	if !ok {
		var err error
		spec, err = s.client.getSpec(ctx, n.SpecID)
		if err != nil {
			log.Warn().Str("specId", n.SpecID).Str("infraId", n.InfraID).Str("nodeId", n.ID).Err(err).
				Msg("Failed to read the CB-Tumblebug spec of a node; listing it without GPUs")
			spec = accelerator{}
		}
		specs[n.SpecID] = spec
	}
	setGPU(t, spec)
}

// osFamily reduces the image of a node to a family. NodeInfo.image carries no
// platform field (CB-Tumblebug keeps osPlatform on the image resource only), so
// the node is "windows" when its osType, such as "Windows 2022", or its
// osDistribution, the CSP's own text, names Windows, as mc-observability
// decides; it is "linux" when either is set otherwise, and unknown ("") when
// both are empty.
func osFamily(osType, osDistribution string) string {
	switch {
	case osType == "" && osDistribution == "":
		return ""
	case strings.Contains(strings.ToLower(osType), "windows"),
		strings.Contains(strings.ToLower(osDistribution), "windows"):
		return "windows"
	default:
		return "linux"
	}
}
