package kube

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"aigw-ui/internal/weights"
)

// CapacityAnnotation, on an LLMInferenceService, says what one ready instance
// of that deployment can serve, as a number. The unit is free as long as it
// is the same for every deployment of the model on every site, for example
// tokens per second from a benchmark. Without it an instance counts as 1.
const CapacityAnnotation = "aigw-ui.io/capacity-per-instance"

// capacityPerReplica is the older name of CapacityAnnotation. It is read when
// the newer one is not set.
const capacityPerReplica = "aigw-ui.io/capacity-per-replica"

const (
	// RevisionAnnotation identifies the checkpoint a deployment serves. Sites
	// with different revisions of one model should not share its traffic.
	RevisionAnnotation = "aigw-ui.io/model-revision"
	// MaxModelLenAnnotation is the longest request the deployment takes.
	MaxModelLenAnnotation = "aigw-ui.io/max-model-len"
)

// IgnoreAnnotation set to "true" keeps an LLMInferenceService out of the
// count: a canary, a test deployment, or one the gateway has no route to.
const IgnoreAnnotation = "aigw-ui.io/ignore"

// A declared capacity outside these bounds is a mistake, and a huge one
// would overflow the weight the gateway takes.
const (
	minPerInstance = 0.01
	maxPerInstance = 10000
)

// stopAnnotation is how KServe is told to stop a service's workloads.
const stopAnnotation = "serving.kserve.io/stop"

// The versions of LLMInferenceService that carry status.workloads, newest first.
var llmServiceGVRs = []schema.GroupVersionResource{
	{Group: "serving.kserve.io", Version: "v1alpha2", Resource: "llminferenceservices"},
	{Group: "serving.kserve.io", Version: "v1alpha1", Resource: "llminferenceservices"},
}

// ModelCapacity is what one cluster can serve of one model right now.
type ModelCapacity struct {
	// Model is the name requests carry: spec.model.name, or the name of the
	// LLMInferenceService when that is empty.
	Model string
	// Capacity is ready instances times the capacity of one, summed over the
	// model's deployments. Known is false when none of them has reported its
	// ready count; that is "unknown", never "zero". A deployment without a
	// count next to one that has reported adds nothing: it is new, and
	// waiting for it would keep the weight up while the others lose pods.
	Capacity float64
	Known    bool
	// Step is the capacity of the model's largest single instance.
	Step float64
	// Detail says how the number came about, for display.
	Detail string
	// Revision and MaxModelLen are what the model's deployments declare. More
	// than one value on a cluster is joined with ", ".
	Revision    string
	MaxModelLen string
}

// Capacity reads how many instances of each model are ready, from the
// LLMInferenceServices in every namespace that are not marked to be ignored. A cluster without KServe has none
// and that is not an error: it serves no model this tool can count.
func (c *Client) Capacity(ctx context.Context) ([]ModelCapacity, error) {
	var items []unstructured.Unstructured
	for i, gvr := range llmServiceGVRs {
		list, err := c.dyn.Resource(gvr).List(ctx, metav1.ListOptions{})
		if missingKind(err) {
			if i == len(llmServiceGVRs)-1 {
				return nil, nil
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list LLMInferenceServices: %w", err)
		}
		items = list.Items
		break
	}

	byModel := map[string]*ModelCapacity{}
	for i := range items {
		svc := &items[i]
		if svc.GetAnnotations()[IgnoreAnnotation] == "true" {
			continue
		}
		model, _, _ := unstructured.NestedString(svc.Object, "spec", "model", "name")
		if model == "" {
			model = svc.GetName()
		}
		w, note := workload(svc)
		instances, known := w.Instances()

		mc := byModel[model]
		if mc == nil {
			mc = &ModelCapacity{Model: model}
			byModel[model] = mc
		}
		mc.Known = mc.Known || known
		mc.Capacity += instances * w.PerInstance
		mc.Step = max(mc.Step, w.PerInstance)
		if mc.Detail != "" {
			mc.Detail += "; "
		}
		mc.Detail += describeWorkload(svc, w, instances, known) + note
		mc.Revision = addValue(mc.Revision, svc.GetAnnotations()[RevisionAnnotation])
		mc.MaxModelLen = addValue(mc.MaxModelLen, svc.GetAnnotations()[MaxModelLenAnnotation])
	}
	out := make([]ModelCapacity, 0, len(byModel))
	for _, mc := range byModel {
		out = append(out, *mc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out, nil
}

// workload reads what capacity needs from one LLMInferenceService. KServe
// copies the ready counts into status.workloads from the Deployment's
// available replicas, or from the LeaderWorkerSet's ready groups for a
// multi-node deployment, so one multi-node instance counts once.
//
// The note says what an operator should know about the declared capacity.
func workload(svc *unstructured.Unstructured) (weights.Workload, string) {
	w := weights.Workload{PerInstance: 1, Desired: replicas(svc, "spec", "replicas")}
	declared, note := svc.GetAnnotations()[CapacityAnnotation], ""
	if declared == "" {
		if declared = svc.GetAnnotations()[capacityPerReplica]; declared != "" {
			note = " (declared with " + capacityPerReplica + ", the older name of " + CapacityAnnotation + ")"
		}
	}
	if declared != "" {
		// ParseFloat also takes "Inf" and "NaN"; the bounds refuse both.
		if v, err := strconv.ParseFloat(declared, 64); err == nil && v >= minPerInstance && v <= maxPerInstance {
			w.PerInstance = v
		} else {
			note = fmt.Sprintf(" (declared capacity %q is not a number from %v to %v, so an instance counts as 1)", declared, minPerInstance, maxPerInstance)
		}
	}
	stopped := svc.GetAnnotations()[stopAnnotation] == "true"
	w.Ready = readyReplicas(svc, "primary", stopped)
	if _, separate, _ := unstructured.NestedMap(svc.Object, "spec", "prefill"); separate {
		w.Prefill = &weights.Prefill{
			Desired: replicas(svc, "spec", "prefill", "replicas"),
			Ready:   readyReplicas(svc, "prefill", stopped),
		}
	}
	return w, note
}

// replicas returns a replica count from the spec, which is 1 when left out.
func replicas(svc *unstructured.Unstructured, path ...string) int64 {
	if n, found, _ := unstructured.NestedInt64(svc.Object, path...); found {
		return n
	}
	return 1
}

// readyReplicas returns nil when the count is not there. A stopped service
// has no workload status at all, and is known to serve nothing.
func readyReplicas(svc *unstructured.Unstructured, workload string, stopped bool) *int64 {
	n, found, _ := unstructured.NestedInt64(svc.Object, "status", "workloads", workload, "readyReplicas")
	if !found {
		if stopped {
			return new(int64)
		}
		return nil
	}
	return &n
}

func describeWorkload(svc *unstructured.Unstructured, w weights.Workload, instances float64, known bool) string {
	name := svc.GetNamespace() + "/" + svc.GetName()
	if !known {
		return name + ": ready count not reported"
	}
	s := fmt.Sprintf("%s: %s of %d ready", name, strconv.FormatFloat(instances, 'f', -1, 64), w.Desired)
	if w.Prefill != nil {
		s += " (with prefill)"
	}
	if w.PerInstance != 1 {
		s += " × " + strconv.FormatFloat(w.PerInstance, 'f', -1, 64)
	}
	return s
}

// addValue adds one deployment's value to the list of distinct values seen
// for the model on this cluster.
func addValue(list, value string) string {
	if value == "" {
		return list
	}
	if list == "" {
		return value
	}
	values := strings.Split(list, ", ")
	if slices.Contains(values, value) {
		return list
	}
	values = append(values, value)
	sort.Strings(values)
	return strings.Join(values, ", ")
}
