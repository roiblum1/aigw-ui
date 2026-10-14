package capacity

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// Shape is what one instance of a deployment is: one model can run in
// several, and each serves a different amount. Deployments of the same
// model and shape share a measurement, wherever they run.
type Shape struct {
	// Topology is "single", "multi-node" or "prefill-decode".
	Topology string
	// GPU is the GPU product, such as NVIDIA-H200, or "" when unknown.
	GPU             string
	GPUsPerInstance float64
	// Revision is the checkpoint the deployment declares.
	Revision string
}

// Key identifies the shape among a model's shapes.
func (s Shape) Key() string {
	return fmt.Sprintf("%s|%s|%s|%s", s.Topology, s.GPU, strconv.FormatFloat(math.Round(s.GPUsPerInstance*100)/100, 'f', -1, 64), s.Revision)
}

// Label says what the shape is: "8× NVIDIA-B300, one node".
func (s Shape) Label() string {
	gpus := "GPU not known"
	switch {
	case s.GPU != "" && s.GPUsPerInstance > 0:
		gpus = strconv.FormatFloat(math.Round(s.GPUsPerInstance*100)/100, 'f', -1, 64) + "× " + s.GPU
	case s.GPU != "":
		gpus = s.GPU
	case s.GPUsPerInstance > 0:
		gpus = strconv.FormatFloat(s.GPUsPerInstance, 'f', -1, 64) + " GPUs of an unknown kind"
	}
	topology := map[string]string{"single": "one node", "multi-node": "several nodes", "prefill-decode": "prefill and decode"}[s.Topology]
	if topology == "" {
		topology = s.Topology
	}
	label := gpus + ", " + topology
	if s.Revision != "" {
		label += ", revision " + s.Revision
	}
	return label
}

// MarshalJSON writes the shape with its key and label.
func (s Shape) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"topology": s.Topology, "gpu": s.GPU, "gpus_per_instance": s.GPUsPerInstance, "revision": s.Revision,
		"key": s.Key(), "label": s.Label(),
	})
}
