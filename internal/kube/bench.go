package kube

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	podsGVR  = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	nodesGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
	jobsGVR  = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
)

// Instance is one ready pod of a model's deployment: what a capacity
// measurement is aimed at.
type Instance struct {
	Namespace string
	Pod       string
	// URL reaches the pod's model server directly, past the gateway and
	// the scheduler. URLs has one address per port the pod serves on: one
	// per data-parallel rank of a pod that runs an API server for each
	// (vLLM's --data-parallel-multi-port-external-lb), else just URL.
	URL  string
	URLs []string
	// Model is the name the server serves the model under.
	Model string
	// MaxModelLen is what the deployment declares, or 0.
	MaxModelLen int
}

// ReadyPods returns the names of the ready pods of a deployment that answer
// requests, by name.
func (c *Client) ReadyPods(ctx context.Context, namespace, name string) ([]string, error) {
	pods, err := c.deploymentPods(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, p := range readyPods(pods) {
		names = append(names, p.GetName())
	}
	return names, nil
}

// Instance returns a ready pod of a deployment, to aim a measurement at.
//
// A deployment with separate prefill pods is refused: one of its instances
// is a prefill and a decode pod together, and a single pod is not one.
func (c *Client) Instance(ctx context.Context, namespace, name, pod string) (Instance, error) {
	svc, err := c.llmService(ctx, namespace, name)
	if err != nil {
		return Instance{}, err
	}
	if _, separate, _ := unstructured.NestedMap(svc.Object, "spec", "prefill"); separate {
		return Instance{}, fmt.Errorf("%s/%s has separate prefill pods, and one pod of it is not an instance; such a deployment is not measured yet", namespace, name)
	}
	pods, err := c.deploymentPods(ctx, namespace, name)
	if err != nil {
		return Instance{}, err
	}
	for _, p := range readyPods(pods) {
		if p.GetName() != pod {
			continue
		}
		urls, err := serverURLs(p, c.rankPorts(ctx, svc))
		if err != nil {
			return Instance{}, fmt.Errorf("%s: %w", pod, err)
		}
		model, _, _ := unstructured.NestedString(svc.Object, "spec", "model", "name")
		if model == "" {
			model = svc.GetName()
		}
		maxLen, _ := strconv.Atoi(svc.GetAnnotations()[MaxModelLenAnnotation])
		return Instance{Namespace: namespace, Pod: pod, URL: urls[0], URLs: urls, Model: model, MaxModelLen: maxLen}, nil
	}
	return Instance{}, fmt.Errorf("%s/%s has no ready pod called %s", namespace, name, pod)
}

// llmService reads one LLMInferenceService, in the newest version the
// cluster has.
func (c *Client) llmService(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	for i, gvr := range llmServiceGVRs {
		svc, err := c.dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if meta.IsNoMatchError(err) && i < len(llmServiceGVRs)-1 {
			continue
		}
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("no LLMInferenceService %s/%s on this cluster", namespace, name)
		}
		return svc, err
	}
	return nil, fmt.Errorf("no LLMInferenceService %s/%s on this cluster", namespace, name)
}

// deploymentPods lists the workload pods of an LLMInferenceService.
func (c *Client) deploymentPods(ctx context.Context, namespace, name string) ([]unstructured.Unstructured, error) {
	pods, err := c.dyn.Resource(podsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=" + name + ",kserve.io/component=workload",
	})
	if err != nil {
		return nil, fmt.Errorf("list the pods of %s/%s: %w", namespace, name, err)
	}
	return pods.Items, nil
}

// llmServices lists the LLMInferenceServices in every namespace, in the
// newest version the cluster has. A cluster without KServe has none.
func (c *Client) llmServices(ctx context.Context) ([]unstructured.Unstructured, error) {
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
		return list.Items, nil
	}
	return nil, nil
}

// readyPods returns the pods that serve requests now, by name. A prefill
// pod of a split deployment does not answer a request on its own.
func readyPods(pods []unstructured.Unstructured) []unstructured.Unstructured {
	var out []unstructured.Unstructured
	for _, p := range pods {
		if p.GetLabels()["llm-d.ai/role"] != "prefill" && isReady(&p) {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
}

// isReady says whether a pod is running, has an address and passes its
// readiness check.
func isReady(p *unstructured.Unstructured) bool {
	if p.GetDeletionTimestamp() != nil {
		return false
	}
	if phase, _, _ := unstructured.NestedString(p.Object, "status", "phase"); phase != "Running" {
		return false
	}
	if ip, _, _ := unstructured.NestedString(p.Object, "status", "podIP"); ip == "" {
		return false
	}
	conditions, _, _ := unstructured.NestedSlice(p.Object, "status", "conditions")
	for _, c := range conditions {
		if m, ok := c.(map[string]any); ok && m["type"] == "Ready" && m["status"] == "True" {
			return true
		}
	}
	return false
}

// The InferencePool in its two generations, by API group.
var poolGVRs = map[string]schema.GroupVersionResource{
	"inference.networking.k8s.io":   {Group: "inference.networking.k8s.io", Version: "v1", Resource: "inferencepools"},
	"inference.networking.x-k8s.io": {Group: "inference.networking.x-k8s.io", Version: "v1alpha2", Resource: "inferencepools"},
}

// rankPorts returns the ports the service's InferencePool sends requests to
// on each pod, the ones the scheduler picks from. A pod that runs one API
// server per data-parallel rank lists each: glm53 serves on 8000 to 8007
// and answers health checks only on 8208, which its probes use. nil when
// the service has no scheduler or its pool cannot be read.
func (c *Client) rankPorts(ctx context.Context, svc *unstructured.Unstructured) []int64 {
	p, ok := pool(svc)
	gvr, known := poolGVRs[p.Group]
	if !ok || !known {
		return nil
	}
	obj, err := c.dyn.Resource(gvr).Namespace(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	if n, found, _ := unstructured.NestedInt64(obj.Object, "spec", "targetPortNumber"); found {
		return []int64{n}
	}
	ports, _, _ := unstructured.NestedSlice(obj.Object, "spec", "targetPorts")
	var out []int64
	for _, port := range ports {
		if m, ok := port.(map[string]any); ok {
			if n, ok := m["number"].(int64); ok && n > 0 {
				out = append(out, n)
			}
		}
	}
	return out
}

// serverURLs returns an address of the pod's model server for each of the
// given ports, or the one serverURL finds when there are none.
func serverURLs(pod unstructured.Unstructured, ports []int64) ([]string, error) {
	url, err := serverURL(pod)
	if err != nil || len(ports) == 0 {
		return []string{url}, err
	}
	base := url[:strings.LastIndex(url, ":")+1]
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, base+strconv.FormatInt(p, 10))
	}
	return out, nil
}

// serverURL reads where a pod's model server listens from the readiness
// check of its main container. KServe's vLLM serves plain HTTP on 8000.
func serverURL(pod unstructured.Unstructured) (string, error) {
	ip, _, _ := unstructured.NestedString(pod.Object, "status", "podIP")
	containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
	var main map[string]any
	for _, c := range containers {
		if m, ok := c.(map[string]any); ok && (main == nil || m["name"] == "main") {
			main = m
		}
	}
	port, scheme := int64(8000), "HTTP"
	if get, ok, _ := unstructured.NestedMap(main, "readinessProbe", "httpGet"); ok {
		if s, ok := get["scheme"].(string); ok && s != "" {
			scheme = s
		}
		switch p := get["port"].(type) {
		case int64:
			port = p
		case float64:
			port = int64(p)
		case string:
			if n, ok := namedPort(main, p); ok {
				port = n
			}
		}
	}
	if strings.EqualFold(scheme, "HTTPS") {
		return "", fmt.Errorf("the model server answers over HTTPS only, which a measurement does not reach yet")
	}
	if strings.Contains(ip, ":") {
		ip = "[" + ip + "]"
	}
	return fmt.Sprintf("http://%s:%d", ip, port), nil
}

func namedPort(container map[string]any, name string) (int64, bool) {
	ports, _, _ := unstructured.NestedSlice(container, "ports")
	for _, p := range ports {
		if m, ok := p.(map[string]any); ok && m["name"] == name {
			if n, ok := m["containerPort"].(int64); ok {
				return n, true
			}
		}
	}
	return 0, false
}

// CreateJob creates a Job.
func (c *Client) CreateJob(ctx context.Context, job *unstructured.Unstructured) error {
	_, err := c.dyn.Resource(jobsGVR).Namespace(job.GetNamespace()).Create(ctx, job, metav1.CreateOptions{FieldManager: fieldManager})
	return err
}

// JobState is how far a Job has got.
type JobState struct {
	// Gone is set when the Job no longer exists.
	Gone      bool
	Succeeded bool
	Failed    bool
	// Reason says why a Job failed, as Kubernetes puts it.
	Reason string
}

// GetJobState reads the state of a Job.
func (c *Client) GetJobState(ctx context.Context, namespace, name string) (JobState, error) {
	job, err := c.dyn.Resource(jobsGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return JobState{Gone: true}, nil
	}
	if err != nil {
		return JobState{}, err
	}
	var s JobState
	conditions, _, _ := unstructured.NestedSlice(job.Object, "status", "conditions")
	for _, c := range conditions {
		m, ok := c.(map[string]any)
		if !ok || m["status"] != "True" {
			continue
		}
		switch m["type"] {
		case "Complete":
			s.Succeeded = true
		case "Failed":
			s.Failed = true
			reason, _ := m["reason"].(string)
			message, _ := m["message"].(string)
			s.Reason = strings.TrimSpace(reason + ": " + message)
		}
	}
	return s, nil
}

// maxLog is the most of a pod's log that is read. The measurement prints a
// few lines per step; AIPerf's own output goes to files.
const maxLog = 4 << 20

// PodLog returns the log of the container of the pod that carries a label,
// and false while there is no such pod or it has not started.
func (c *Client) PodLog(ctx context.Context, namespace, selector, container string) (string, bool, error) {
	pods, err := c.dyn.Resource(podsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", false, err
	}
	if len(pods.Items) == 0 {
		return "", false, nil
	}
	pod := pods.Items[len(pods.Items)-1]
	if phase, _, _ := unstructured.NestedString(pod.Object, "status", "phase"); phase == "Pending" || phase == "" {
		return "", false, nil
	}
	raw, err := c.disc.RESTClient().Get().
		AbsPath("/api/v1/namespaces", namespace, "pods", pod.GetName(), "log").
		Param("container", container).
		Param("limitBytes", strconv.Itoa(maxLog)).
		DoRaw(ctx)
	if err != nil {
		return "", false, err
	}
	return string(raw), true, nil
}

// PodProblem returns why the pod that carries a label cannot run, such as an
// image that cannot be pulled, or "" when there is none to report.
func (c *Client) PodProblem(ctx context.Context, namespace, selector string) string {
	pods, err := c.dyn.Resource(podsGVR).Namespace(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	statuses, _, _ := unstructured.NestedSlice(pods.Items[len(pods.Items)-1].Object, "status", "containerStatuses")
	for _, s := range statuses {
		waiting, ok, _ := unstructured.NestedMap(s.(map[string]any), "state", "waiting")
		if !ok {
			continue
		}
		reason, _ := waiting["reason"].(string)
		message, _ := waiting["message"].(string)
		switch reason {
		case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError":
			return strings.TrimSpace(reason + ": " + message)
		}
	}
	return ""
}

// DeleteJob deletes a Job and its pod. A Job that is gone already is not an
// error.
func (c *Client) DeleteJob(ctx context.Context, namespace, name string) error {
	background := metav1.DeletePropagationBackground
	err := c.dyn.Resource(jobsGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &background})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
