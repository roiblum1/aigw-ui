package kube

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	podsGVR = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	jobsGVR = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
)

// Instance is one ready pod of a model's deployment: what a capacity
// measurement is aimed at.
type Instance struct {
	Namespace string
	Pod       string
	// URL reaches the pod's model server directly, past the gateway and
	// the scheduler.
	URL string
	// Model is the name the server serves the model under.
	Model string
	// MaxModelLen is what the deployment declares, or 0.
	MaxModelLen int
}

// FindInstance returns a ready pod of a deployment of the model, by any of
// its names. pod picks one by name; empty takes the first.
//
// A deployment with separate prefill pods is left out: one of its instances
// is a prefill and a decode pod together, and a single pod is not one.
func (c *Client) FindInstance(ctx context.Context, names []string, pod string) (Instance, error) {
	services, err := c.llmServices(ctx)
	if err != nil {
		return Instance{}, err
	}
	var notes []string
	for i := range services {
		svc := &services[i]
		model, _, _ := unstructured.NestedString(svc.Object, "spec", "model", "name")
		if model == "" {
			model = svc.GetName()
		}
		if !slices.Contains(names, model) || svc.GetAnnotations()[IgnoreAnnotation] == "true" {
			continue
		}
		name := svc.GetNamespace() + "/" + svc.GetName()
		if _, separate, _ := unstructured.NestedMap(svc.Object, "spec", "prefill"); separate {
			notes = append(notes, name+" has separate prefill pods, which a single pod does not measure")
			continue
		}
		pods, err := c.dyn.Resource(podsGVR).Namespace(svc.GetNamespace()).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=" + svc.GetName() + ",kserve.io/component=workload",
		})
		if err != nil {
			return Instance{}, fmt.Errorf("list the pods of %s: %w", name, err)
		}
		ready := readyPods(pods.Items)
		if len(ready) == 0 {
			notes = append(notes, name+" has no ready pod")
			continue
		}
		chosen := ready[0]
		if pod != "" {
			found := false
			for _, p := range ready {
				if p.GetName() == pod {
					chosen, found = p, true
				}
			}
			if !found {
				notes = append(notes, name+" has no ready pod called "+pod)
				continue
			}
		}
		url, err := serverURL(chosen)
		if err != nil {
			return Instance{}, fmt.Errorf("%s: %w", name, err)
		}
		maxLen, _ := strconv.Atoi(svc.GetAnnotations()[MaxModelLenAnnotation])
		return Instance{Namespace: svc.GetNamespace(), Pod: chosen.GetName(), URL: url, Model: model, MaxModelLen: maxLen}, nil
	}
	if len(notes) == 0 {
		return Instance{}, fmt.Errorf("no LLMInferenceService on this cluster serves %s", strings.Join(names, " or "))
	}
	return Instance{}, fmt.Errorf("no pod to measure: %s", strings.Join(notes, "; "))
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
		if p.GetDeletionTimestamp() != nil || p.GetLabels()["llm-d.ai/role"] == "prefill" {
			continue
		}
		if phase, _, _ := unstructured.NestedString(p.Object, "status", "phase"); phase != "Running" {
			continue
		}
		if ip, _, _ := unstructured.NestedString(p.Object, "status", "podIP"); ip == "" {
			continue
		}
		conditions, _, _ := unstructured.NestedSlice(p.Object, "status", "conditions")
		for _, c := range conditions {
			if m, ok := c.(map[string]any); ok && m["type"] == "Ready" && m["status"] == "True" {
				out = append(out, p)
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetName() < out[j].GetName() })
	return out
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
