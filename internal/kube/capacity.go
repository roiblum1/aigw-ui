package kube

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	// MarketIDAnnotation names the model's entry in the market snapshot
	// (internal/market), such as "z-ai/glm-5.3", when its served name is not
	// a Hugging Face id the market knows.
	MarketIDAnnotation = "aigw-ui.io/market-id"
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

// The versions of LLMInferenceService the hub reads, newest first. Only
// v1alpha2 carries status.workloads (KServe 0.21): through v1alpha1, on a
// cluster that serves no newer version, the ready counts are not reported and
// the cluster's capacity stays unknown.
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
	// Deployments are the model's LLMInferenceServices with their shapes.
	Deployments []Deployment
	// Step is the capacity of the model's largest single instance.
	Step float64
	// Detail says how the number came about, for display.
	Detail string
	// Revision and MaxModelLen are what the model's deployments declare. More
	// than one value on a cluster is joined with ", ".
	Revision    string
	MaxModelLen string
	// Pools are the InferencePools the model's deployments are served
	// through, one per deployment that has a scheduler.
	Pools []Pool
	// ShapesDenied names what the hub was not allowed to list to find its
	// deployments' GPUs: "pods", "nodes" or both. Their GPUs are then not
	// known.
	ShapesDenied []string
	// ConfigsDenied is set when the hub was not allowed to list the
	// LLMInferenceServiceConfigs: the spec hash then holds their names only,
	// and a change in one does not mark a measurement stale.
	ConfigsDenied bool
}

// Pool names an InferencePool: the set of pods of one deployment that its
// scheduler picks from. A request class is defined per pool.
type Pool struct {
	Namespace string
	Name      string
	// Group is the pool's API group. Two generations of the kind exist.
	Group string
}

// defaultPoolGroup is the group of the InferencePool KServe creates today.
const defaultPoolGroup = "inference.networking.k8s.io"

// pool returns the InferencePool of an LLMInferenceService, and false for a
// service without a scheduler: its requests are not queued by class.
//
// KServe reports the pool it observed in the status. Before it has, the
// pool is the one the spec names, or the one KServe creates, which is
// called after the service.
func pool(svc *unstructured.Unstructured) (Pool, bool) {
	if _, scheduled, _ := unstructured.NestedMap(svc.Object, "spec", "router", "scheduler"); !scheduled {
		return Pool{}, false
	}
	p := Pool{Namespace: svc.GetNamespace(), Group: defaultPoolGroup}
	if observed, ok, _ := unstructured.NestedMap(svc.Object, "status", "router", "scheduler", "inferencePool"); ok {
		p.Name, _ = observed["name"].(string)
		if group, _ := observed["group"].(string); group != "" {
			p.Group = group
		}
	}
	if p.Name == "" {
		p.Name, _, _ = unstructured.NestedString(svc.Object, "spec", "router", "scheduler", "pool", "ref", "name")
	}
	if p.Name == "" {
		p.Name = svc.GetName() + "-inference-pool"
	}
	return p, true
}

// Capacity reads how many instances of each model are ready, from the
// LLMInferenceServices in every namespace that are not marked to be ignored. A cluster without KServe has none
// and that is not an error: it serves no model this tool can count.
func (c *Client) Capacity(ctx context.Context) ([]ModelCapacity, error) {
	// The services, the pods and nodes, and the configs are read side by
	// side, and then each service's workload.
	var items []unstructured.Unstructured
	var err error
	var gpus gpuShapes
	var configs baseConfigs
	var wg sync.WaitGroup
	wg.Go(func() { items, err = c.llmServices(ctx) })
	wg.Go(func() { gpus = c.gpuShapes(ctx) })
	wg.Go(func() { configs = c.baseConfigs(ctx) })
	wg.Wait()
	if err != nil {
		return nil, err
	}
	type read struct {
		w    weights.Workload
		note string
	}
	reads := make([]read, len(items))
	slots := make(chan struct{}, applyWorkers)
	for i := range items {
		svc := &items[i]
		if svc.GetAnnotations()[IgnoreAnnotation] == "true" {
			continue
		}
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			reads[i].w, reads[i].note = workload(svc)
			c.liveWorkload(ctx, svc, &reads[i].w)
		})
	}
	wg.Wait()

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
		w, note := reads[i].w, reads[i].note
		instances, known := w.Instances()

		mc := byModel[model]
		if mc == nil {
			mc = &ModelCapacity{Model: model, ShapesDenied: gpus.denied, ConfigsDenied: configs.denied}
			byModel[model] = mc
		}
		mc.Known = mc.Known || known
		mc.Capacity += instances * w.PerInstance
		d := gpus.of(svc, instances, known)
		d.Revision = svc.GetAnnotations()[RevisionAnnotation]
		// While the pods of a rollout disagree, or the configs could not be
		// read this time, the hash is left empty: the hub keeps the one it
		// has.
		images, imagesOK := gpus.images(svc)
		if bases, basesOK := configs.of(svc); imagesOK && basesOK {
			d.SpecHash = SpecHash(svc, bases, images)
		}
		d.Quantization, d.LMCache = Serving(svc)
		d.DecodeReplicas = replicas(svc, "spec", "replicas")
		d.MarketID = strings.TrimSpace(svc.GetAnnotations()[MarketIDAnnotation])
		d.Listeners = routerListeners(svc)
		mc.Deployments = append(mc.Deployments, d)
		mc.Step = max(mc.Step, w.PerInstance)
		if mc.Detail != "" {
			mc.Detail += "; "
		}
		mc.Detail += describeWorkload(svc, w, instances, known) + note
		mc.Revision = addValue(mc.Revision, svc.GetAnnotations()[RevisionAnnotation])
		mc.MaxModelLen = addValue(mc.MaxModelLen, svc.GetAnnotations()[MaxModelLenAnnotation])
		if p, ok := pool(svc); ok {
			mc.Pools = append(mc.Pools, p)
		}
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
	// KServe reads the annotation in any case (strings.EqualFold).
	stopped := strings.EqualFold(svc.GetAnnotations()[stopAnnotation], "true")
	w.Ready = readyReplicas(svc, "primary", stopped)
	if _, separate, _ := unstructured.NestedMap(svc.Object, "spec", "prefill"); separate {
		w.Prefill = &weights.Prefill{
			Desired: replicas(svc, "spec", "prefill", "replicas"),
			Ready:   readyReplicas(svc, "prefill", stopped),
		}
	}
	return w, note
}

// The kinds KServe runs a service's pods with: a Deployment, or a
// LeaderWorkerSet for a multi-node one.
var (
	deploymentsGVR = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	lwsGVR         = schema.GroupVersionResource{Group: "leaderworkerset.x-k8s.io", Version: "v1", Resource: "leaderworkersets"}
)

// liveWorkload replaces the counts KServe copied into the service's status
// with those of the Deployment or LeaderWorkerSet it names there, when the
// hub can read them. KServe copies them only when the rest of its reconcile
// succeeds, so after a failure (of the router, say) the status keeps old
// counts, and it bumps observedGeneration anyway, so that does not tell.
// The object's own spec.replicas is also what an autoscaler sets, where the
// service's spec.replicas is left out (spec.scaling).
func (c *Client) liveWorkload(ctx context.Context, svc *unstructured.Unstructured, w *weights.Workload) {
	if ready, desired, ok := c.liveCounts(ctx, svc, "primary"); ok {
		w.Ready, w.Desired = &ready, desired
	}
	if w.Prefill != nil {
		if ready, desired, ok := c.liveCounts(ctx, svc, "prefill"); ok {
			w.Prefill.Ready, w.Prefill.Desired = &ready, desired
		}
	}
}

// liveCounts reads the ready and desired replicas of one of the service's
// workloads ("primary" or "prefill"), the way KServe counts them: available
// pods of a Deployment, ready groups of a LeaderWorkerSet. ok is false when
// the status names no such object or it cannot be read.
func (c *Client) liveCounts(ctx context.Context, svc *unstructured.Unstructured, workload string) (ready, desired int64, ok bool) {
	ref, found, _ := unstructured.NestedMap(svc.Object, "status", "workloads", workload)
	if !found {
		return 0, 0, false
	}
	kind, _ := ref["kind"].(string)
	name, _ := ref["name"].(string)
	gvr, readyField := deploymentsGVR, "availableReplicas"
	switch kind {
	case "Deployment":
	case "LeaderWorkerSet":
		gvr, readyField = lwsGVR, "readyReplicas"
	default:
		return 0, 0, false
	}
	if name == "" {
		return 0, 0, false
	}
	obj, err := c.dyn.Resource(gvr).Namespace(svc.GetNamespace()).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0, 0, false
	}
	ready, _, _ = unstructured.NestedInt64(obj.Object, "status", readyField)
	return ready, replicas(obj, "spec", "replicas"), true
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

// Topologies of a deployment: what one of its instances is.
const (
	// TopologySingle is one pod on one node.
	TopologySingle = "single"
	// TopologyMultiNode is a group of pods on several nodes.
	TopologyMultiNode = "multi-node"
	// TopologyPrefillDecode is decode pods with prefill pods next to them.
	TopologyPrefillDecode = "prefill-decode"
)

// GPUProductLabel is the label the NVIDIA GPU Operator puts on a node with
// the name of its GPU, such as NVIDIA-H200.
const GPUProductLabel = "nvidia.com/gpu.product"

// gpuResource is what a pod asks for to get GPUs.
const gpuResource = "nvidia.com/gpu"

// Deployment is one LLMInferenceService of a model and its shape.
type Deployment struct {
	Namespace string
	Name      string
	Topology  string
	// GPU is the GPU of its ready pods' nodes, "" when unknown or none.
	GPU string
	// GPUsPerInstance is what its ready pods ask for, divided by its ready
	// instances. 0 when unknown.
	GPUsPerInstance float64
	Revision        string
	// MarketID is the annotation aigw-ui.io/market-id, "" when absent.
	MarketID string
	// SpecHash identifies what it runs, whatever its scale (SpecHash); empty
	// while that cannot be told, as during a rollout.
	SpecHash string
	// Quantization and LMCache say how it serves the model (Serving).
	Quantization string
	LMCache      bool
	// DecodeReplicas is its desired replicas: for a prefill/decode service,
	// the decode replicas a measurement of it is divided by.
	DecodeReplicas int64
	// Instances is its ready instances; Known is false until it reports them.
	Instances float64
	Known     bool
	// Listeners are the gateway listeners KServe put its route on, as
	// "<namespace>/<gateway>/<listener>" (status.router.gateways).
	Listeners []string
}

// routerListeners returns the gateway listeners an LLMInferenceService's
// route is on, as KServe reports them.
func routerListeners(svc *unstructured.Unstructured) []string {
	gateways, _, _ := unstructured.NestedSlice(svc.Object, "status", "router", "gateways")
	var out []string
	for _, g := range gateways {
		gw, _ := g.(map[string]any)
		ns, _ := gw["namespace"].(string)
		if ns == "" {
			ns = svc.GetNamespace()
		}
		name, _ := gw["name"].(string)
		listeners, _ := gw["listeners"].([]any)
		for _, l := range listeners {
			if l, ok := l.(string); ok {
				out = append(out, ns+"/"+name+"/"+l)
			}
		}
	}
	return out
}

// gpuShapes holds what the workload pods ask for and where they run, read
// once per look at the cluster.
type gpuShapes struct {
	pods     []unstructured.Unstructured
	listed   bool              // the pods were listed
	products map[string]string // node name -> GPU product
	denied   []string          // what the hub may not list
}

// gpuShapes reads the workload pods and the nodes' GPU labels. Not being
// allowed to read them is not an error: the shapes are then unknown and
// everything else is counted as before. The refusal is kept, so that it can
// be shown instead of a GPU that looks unlabelled.
func (c *Client) gpuShapes(ctx context.Context) gpuShapes {
	var g gpuShapes
	pods, err := c.dyn.Resource(podsGVR).List(ctx, metav1.ListOptions{LabelSelector: "kserve.io/component=workload"})
	if err == nil {
		g.pods, g.listed = pods.Items, true
	} else if apierrors.IsForbidden(err) {
		g.denied = append(g.denied, "pods")
	}
	g.products = map[string]string{}
	nodes, err := c.dyn.Resource(nodesGVR).List(ctx, metav1.ListOptions{LabelSelector: GPUProductLabel})
	if err == nil {
		for _, n := range nodes.Items {
			g.products[n.GetName()] = n.GetLabels()[GPUProductLabel]
		}
	} else if apierrors.IsForbidden(err) {
		g.denied = append(g.denied, "nodes")
	}
	return g
}

// of returns the shape of one LLMInferenceService.
func (g gpuShapes) of(svc *unstructured.Unstructured, instances float64, known bool) Deployment {
	d := Deployment{Namespace: svc.GetNamespace(), Name: svc.GetName(), Topology: TopologySingle, Instances: instances, Known: known}
	if _, ok, _ := unstructured.NestedMap(svc.Object, "spec", "prefill"); ok {
		d.Topology = TopologyPrefillDecode
	} else if _, ok, _ := unstructured.NestedMap(svc.Object, "spec", "worker"); ok {
		d.Topology = TopologyMultiNode
	}
	var gpus float64
	products := map[string]int{}
	for i := range g.pods {
		p := &g.pods[i]
		if p.GetNamespace() != svc.GetNamespace() || p.GetLabels()["app.kubernetes.io/name"] != svc.GetName() || !isReady(p) {
			continue
		}
		gpus += podGPUs(p)
		if node, _, _ := unstructured.NestedString(p.Object, "spec", "nodeName"); g.products[node] != "" {
			products[g.products[node]]++
		}
	}
	if instances > 0 {
		d.GPUsPerInstance = gpus / instances
	}
	most := 0
	for product, n := range products {
		if n > most || (n == most && product < d.GPU) {
			d.GPU, most = product, n
		}
	}
	return d
}

// podGPUs is the GPUs a pod's containers ask for.
func podGPUs(p *unstructured.Unstructured) float64 {
	containers, _, _ := unstructured.NestedSlice(p.Object, "spec", "containers")
	var n float64
	for _, c := range containers {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"limits", "requests"} {
			if v, ok, _ := unstructured.NestedString(m, "resources", field, gpuResource); ok {
				if q, err := strconv.ParseFloat(v, 64); err == nil {
					n += q
					break
				}
			}
			if v, ok, _ := unstructured.NestedInt64(m, "resources", field, gpuResource); ok {
				n += float64(v)
				break
			}
		}
	}
	return n
}
