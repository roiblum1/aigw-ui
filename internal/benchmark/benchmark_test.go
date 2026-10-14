package benchmark

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"aigw-ui/internal/capacity"
	"aigw-ui/internal/kube"
	"aigw-ui/internal/store"
)

type fakeStore struct {
	deployments []store.Deployment
	runs        map[string]*store.CapacityRun
}

func (f *fakeStore) GetDeployment(_ context.Context, modelID, clusterID, namespace, name string) (store.Deployment, error) {
	for _, d := range f.deployments {
		if d.ModelID == modelID && d.ClusterID == clusterID && d.Namespace == namespace && d.Name == name {
			return d, nil
		}
	}
	return store.Deployment{}, store.ErrNotFound
}

func (f *fakeStore) CreateCapacityRun(_ context.Context, n store.NewRun) error {
	for _, r := range f.runs {
		if r.ModelID == n.ModelID && r.Status == store.RunRunning {
			return store.ErrConflict
		}
	}
	f.runs[n.ID] = &store.CapacityRun{ID: n.ID, ModelID: n.ModelID, ClusterID: n.ClusterID, Namespace: n.Namespace, Deployment: n.Deployment,
		JobName: n.JobName, Instance: n.Instance, Shape: n.Shape, Settings: n.Settings, Status: store.RunRunning, CreatedAt: time.Now()}
	return nil
}

func (f *fakeStore) GetCapacityRun(_ context.Context, id string) (store.CapacityRun, error) {
	if r, ok := f.runs[id]; ok {
		return *r, nil
	}
	return store.CapacityRun{}, store.ErrNotFound
}

func (f *fakeStore) RunningCapacityRuns(context.Context) ([]store.CapacityRun, error) {
	var out []store.CapacityRun
	for _, r := range f.runs {
		if r.Status == store.RunRunning {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (f *fakeStore) UpdateCapacityRun(_ context.Context, id string, d *capacity.Dataset, steps []capacity.Step) error {
	f.runs[id].Dataset, f.runs[id].Steps = d, steps
	return nil
}

func (f *fakeStore) FinishCapacityRun(_ context.Context, id, status, detail string) error {
	r := f.runs[id]
	if r.Status != store.RunRunning {
		return store.ErrNotFound
	}
	r.Status, r.Detail = status, detail
	return nil
}

type fakeCluster struct {
	instance kube.Instance
	findErr  error
	jobs     map[string]*unstructured.Unstructured
	state    kube.JobState
	log      string
	problem  string
	deleted  []string
}

func (c *fakeCluster) Instance(_ context.Context, _, _, pod string) (kube.Instance, error) {
	if c.findErr != nil {
		return kube.Instance{}, c.findErr
	}
	i := c.instance
	i.Pod = pod
	return i, nil
}

func (c *fakeCluster) ReadyPods(context.Context, string, string) ([]string, error) {
	return []string{c.instance.Pod}, nil
}

func (c *fakeCluster) CreateJob(_ context.Context, job *unstructured.Unstructured) error {
	c.jobs[job.GetName()] = job
	return nil
}

func (c *fakeCluster) GetJobState(context.Context, string, string) (kube.JobState, error) {
	return c.state, nil
}

func (c *fakeCluster) PodLog(context.Context, string, string, string) (string, bool, error) {
	return c.log, c.log != "", nil
}

func (c *fakeCluster) PodProblem(context.Context, string, string) string { return c.problem }

func (c *fakeCluster) DeleteJob(_ context.Context, _, name string) error {
	c.deleted = append(c.deleted, name)
	return nil
}

func setup(image string) (*fakeStore, *fakeCluster, *Service) {
	st := &fakeStore{runs: map[string]*store.CapacityRun{}, deployments: []store.Deployment{{ModelID: "m1", ClusterID: "c1", Namespace: "llm",
		Name: "glm", Topology: "single", GPU: "NVIDIA-B300", GPUsPerInstance: 8}}}
	cl := &fakeCluster{instance: kube.Instance{Namespace: "llm", Pod: "glm-0", URL: "http://10.0.0.5:8000", Model: "glm-5", MaxModelLen: 200000}, jobs: map[string]*unstructured.Unstructured{}}
	return st, cl, New(st, func(context.Context, string) (Cluster, error) { return cl, nil }, image, time.Minute)
}

const twoSteps = `AIGW-DATASET {"sessions":200,"requests":4000,"prompt_mean":99000,"prompt_max":166000}
AIGW-STEP {"concurrency":1,"requests":20,"duration_s":300,"input_tokens":2000000,"output_tokens":20000,"ttft_p95_ms":900,"itl_p95_ms":20}
AIGW-STEP {"concurrency":2,"requests":40,"duration_s":300,"input_tokens":4000000,"output_tokens":40000,"ttft_p95_ms":9000,"itl_p95_ms":25}
`

func TestStartAndFollow(t *testing.T) {
	st, cl, s := setup("registry/aiperf:1")
	ctx := context.Background()
	run, err := s.Start(ctx, "m1", "c1", "llm", "glm", "glm-0", capacity.Defaults(0))
	if err != nil {
		t.Fatal(err)
	}
	if run.Namespace != "llm" || run.Instance != "glm-0" || run.Deployment != "glm" || run.Shape.GPU != "NVIDIA-B300" || cl.jobs[run.JobName] == nil {
		t.Fatalf("run = %+v", run)
	}
	if _, err := s.Start(ctx, "m1", "c1", "llm", "glm", "glm-0", capacity.Defaults(0)); !errors.Is(err, ErrBusy) {
		t.Fatalf("second start: %v", err)
	}

	// Two steps printed, the Job still running.
	cl.log = twoSteps
	s.Tick(ctx, time.Now())
	r := st.runs[run.ID]
	if r.Status != store.RunRunning || len(r.Steps) != 2 || r.Dataset == nil {
		t.Fatalf("running: %+v", r)
	}

	// The Job finished.
	cl.log += "AIGW-DONE {}\n"
	cl.state = kube.JobState{Succeeded: true}
	s.Tick(ctx, time.Now())
	if r.Status != store.RunSucceeded || !strings.Contains(r.Detail, "at 2 in flight") {
		t.Fatalf("done: %+v", r)
	}
}

func TestStartRefuses(t *testing.T) {
	ctx := context.Background()
	_, _, s := setup("")
	if _, err := s.Start(ctx, "m1", "c1", "llm", "glm", "glm-0", capacity.Defaults(0)); !errors.Is(err, ErrNoImage) {
		t.Fatalf("no image: %v", err)
	}
	_, _, s = setup("img")
	if _, err := s.Start(ctx, "m1", "c1", "llm", "other", "glm-0", capacity.Defaults(0)); !errors.Is(err, ErrNotServed) {
		t.Fatalf("not served: %v", err)
	}
	_, cl, s := setup("img")
	cl.instance.MaxModelLen = 8192
	if _, err := s.Start(ctx, "m1", "c1", "llm", "glm", "glm-0", capacity.Defaults(0)); err == nil || !strings.Contains(err.Error(), "exceed the 8192") {
		t.Fatalf("too long: %v", err)
	}
	_, cl, s = setup("img")
	cl.findErr = errors.New("no pod to measure: llm/glm has no ready pod")
	if _, err := s.Start(ctx, "m1", "c1", "llm", "glm", "glm-0", capacity.Defaults(0)); err == nil || !strings.Contains(err.Error(), "no ready pod") {
		t.Fatalf("no pod: %v", err)
	}
}

func TestFollowEndings(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		state   kube.JobState
		log     string
		problem string
		age     time.Duration
		status  string
		detail  string
		deleted bool
	}{
		"deadline":       {state: kube.JobState{Failed: true, Reason: "DeadlineExceeded: Job was active longer than specified deadline"}, log: twoSteps, status: store.RunFailed, detail: "DeadlineExceeded"},
		"script error":   {state: kube.JobState{Failed: true, Reason: "BackoffLimitExceeded"}, log: `AIGW-ERROR {"error":"making up the sessions failed: boom"}`, status: store.RunFailed, detail: "boom"},
		"job deleted":    {state: kube.JobState{Gone: true}, status: store.RunFailed, detail: "deleted on the cluster"},
		"image missing":  {problem: "ImagePullBackOff: not found", age: 5 * time.Minute, status: store.RunFailed, detail: "could not start", deleted: true},
		"image waits":    {problem: "ImagePullBackOff: not found", age: time.Minute, status: store.RunRunning},
		"nothing passed": {state: kube.JobState{Succeeded: true}, log: `AIGW-STEP {"concurrency":1,"requests":20,"duration_s":300,"ttft_p95_ms":99999,"itl_p95_ms":20}` + "\nAIGW-DONE {}", status: store.RunSucceeded, detail: "no step met"},
	} {
		st, cl, s := setup("img")
		run, err := s.Start(ctx, "m1", "c1", "llm", "glm", "glm-0", capacity.Defaults(0))
		if err != nil {
			t.Fatal(err)
		}
		cl.state, cl.log, cl.problem = tc.state, tc.log, tc.problem
		s.Tick(ctx, time.Now().Add(tc.age))
		r := st.runs[run.ID]
		if r.Status != tc.status || !strings.Contains(r.Detail, tc.detail) || (len(cl.deleted) > 0) != tc.deleted {
			t.Errorf("%s: %s %q deleted %v", name, r.Status, r.Detail, cl.deleted)
		}
	}
}

func TestCancel(t *testing.T) {
	ctx := context.Background()
	st, cl, s := setup("img")
	run, _ := s.Start(ctx, "m1", "c1", "llm", "glm", "glm-0", capacity.Defaults(0))
	got, err := s.Cancel(ctx, run.ID)
	if err != nil || got.Status != store.RunCancelled || len(cl.deleted) != 1 || st.runs[run.ID].Status != store.RunCancelled {
		t.Fatalf("cancel: %+v %v %v", got, err, cl.deleted)
	}
	// A run that has ended is left as it is.
	if got, err := s.Cancel(ctx, run.ID); err != nil || got.Status != store.RunCancelled || len(cl.deleted) != 1 {
		t.Fatalf("second cancel: %+v %v", got, err)
	}
}

func TestNewID(t *testing.T) {
	id, err := newID()
	if err != nil || len(id) != 36 || id[14] != '4' {
		t.Fatalf("id %q %v", id, err)
	}
}
