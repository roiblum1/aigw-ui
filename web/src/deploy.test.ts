// Run with "npm test".
import assert from "node:assert/strict";
import { test } from "node:test";
import { argProblem, argsToText, envToText, fromPreset, getIn, gpusOf, numberOrEmpty, overridden, ranksOf, setGpus, setIn, shape, slug, textToArgs, textToEnv, weights } from "./deploy.ts";

test("a value is read and set by its dotted path, without changing the original", () => {
  const v = { model: { name: "qwen" }, serving: { replicas: 1 } };
  assert.equal(getIn(v, "serving.replicas"), 1);
  assert.equal(getIn(v, "serving.dataParallel.ranks"), undefined);
  const w = setIn(v, "serving.dataParallel.ranks", 8);
  assert.deepEqual(w, { model: { name: "qwen" }, serving: { replicas: 1, dataParallel: { ranks: 8 } } });
  assert.deepEqual(v, { model: { name: "qwen" }, serving: { replicas: 1 } });
});

test("an empty field removes its key, and a map left empty goes too, so the chart's default applies", () => {
  const v = { model: { name: "qwen", revision: "r" }, storage: { cache: { size: "5Gi" } } };
  assert.deepEqual(setIn(v, "model.revision", ""), { model: { name: "qwen" }, storage: { cache: { size: "5Gi" } } });
  assert.deepEqual(setIn(v, "storage.cache.size", undefined), { model: { name: "qwen", revision: "r" } });
  assert.deepEqual(setIn({ serving: { args: ["--a"] } }, "serving.args", []), {});
  assert.equal(numberOrEmpty(""), undefined);
  assert.equal(numberOrEmpty("4"), 4);
  assert.equal(numberOrEmpty("x"), undefined);
});

test("arguments are one per line, and the ones KServe's eval or the chart would refuse are named", () => {
  assert.deepEqual(textToArgs(" --a \n\n--speculative-config.method=mtp\n"), ["--a", "--speculative-config.method=mtp"]);
  assert.equal(argsToText(["--a", "--b=1"]), "--a\n--b=1");
  assert.equal(argProblem("--speculative-config.num_speculative_tokens=5"), "");
  assert.match(argProblem("--x=$(id)"), /plain token/);
  assert.match(argProblem("--chat-template a b"), /plain token/);
  assert.equal(argProblem(`--default-chat-template-kwargs='{"enable_thinking": false}'`), "");
  assert.match(argProblem("--kv-transfer-config"), /fields above/);
});

test("environment variables are NAME=value per line, sorted, and a bad line is named", () => {
  assert.equal(envToText({ OMP_NUM_THREADS: "16", HF_HUB_OFFLINE: 1 }), "HF_HUB_OFFLINE=1\nOMP_NUM_THREADS=16");
  assert.deepEqual(textToEnv("A=1\n B = x=y \n"), { env: { A: "1", B: "x=y" }, problem: "" });
  assert.match(textToEnv("A=1\nnot a var").problem, /not a var/);
});

test("the objects' name is the model name's slug, as the hub and the chart make it", () => {
  assert.equal(slug("glm-5.3"), "glm-5-3");
  assert.equal(slug("Qwen3.8-27B-FP8"), "qwen3-8-27b-fp8");
});

test("a deployment's shape and weights in a few words, from the chart's own keys", () => {
  assert.equal(shape({}), "1 × (CPU)");
  const qwen38 = { llmisvc: { replicas: 1, parallelism: { tensor: 4 }, resources: { limits: { "nvidia.com/gpu": "4" } } }, nodes: { gpuProduct: "NVIDIA-L4" }, lmcache: { enabled: true } };
  assert.equal(shape(qwen38), "1 × (4 × NVIDIA-L4, TP4 LMCache)");
  const glm = { llmisvc: { replicas: 2, args: ["--data-parallel-size=8", "--x"], resources: { requests: { "nvidia.com/gpu": "8" } } }, nodes: { gpuProduct: "NVIDIA-B200" } };
  assert.equal(shape(glm), "2 × (8 × NVIDIA-B200, DP8)");
  assert.equal(ranksOf(glm), 8);
  // The three shapes of one model on three clusters.
  const b300 = { nodes: { gpuProduct: "NVIDIA-B300" }, llmisvc: { replicas: 2, dataParallelExternalLB: { ranks: 8 }, extraArgs: ["--enable-expert-parallel"], resources: { limits: { "nvidia.com/gpu": "8" } } } };
  assert.equal(shape(b300), "2 × (8 × NVIDIA-B300, DP8 EP8)");
  const h200 = { nodes: { gpuProduct: "NVIDIA-H200" }, llmisvc: { parallelism: { tensor: 8 }, resources: { limits: { "nvidia.com/gpu": "8" } } } };
  assert.equal(shape(h200), "1 × (8 × NVIDIA-H200, TP8)");
  assert.equal(shape({ llmisvc: { args: ["--tensor-parallel-size=4"] } }), "1 × (CPU, TP4)");
  const roce = { nodes: { gpuProduct: "NVIDIA-H200" }, llmisvc: { multiNode: true, roce: true, parallelism: { data: 8, dataLocal: 4, tensor: 2, expert: true }, resources: { limits: { "nvidia.com/gpu": "8" } } } };
  assert.equal(shape(roce), "1 × (2 nodes × 8 × NVIDIA-H200, DP8 TP2 EP16 RoCE)");
  assert.equal(weights({ model: { subPath: "GLM-5.3", s3: { path: "llms/zai-org/GLM-5.3" } } }), "s3://llms/zai-org/GLM-5.3 → node disks");
  assert.equal(weights({ model: { uri: "hf://Qwen/Q" }, modelCache: { enabled: true } }), "hf://Qwen/Q → PVC");
});

test("GPUs per replica are set in requests and limits at once, and 0 removes them", () => {
  const v = setGpus({ llmisvc: { resources: { requests: { cpu: "4" } } } }, 4);
  assert.deepEqual(v, { llmisvc: { resources: { requests: { cpu: "4", "nvidia.com/gpu": "4" }, limits: { "nvidia.com/gpu": "4" } } } });
  assert.equal(gpusOf(v), 4);
  assert.deepEqual(setGpus(v, 0), { llmisvc: { resources: { requests: { cpu: "4" } } } });
  assert.equal(getIn(v, "llmisvc.resources.limits.nvidia\u0000com/gpu"), "4");
});

test("what a cluster sets itself is listed by its dotted keys", () => {
  assert.deepEqual(overridden({ llmisvc: { replicas: 2, env: { X: null } }, nodes: { hostnames: ["a"] } }), ["llmisvc.env", "llmisvc.replicas", "nodes.hostnames"]);
  assert.deepEqual(overridden({}), []);
});

test("a preset starts a deployment without its title or place", () => {
  assert.deepEqual(fromPreset({ preset: { title: "t" }, deployment: { cluster: "x" }, model: { name: "m" } }), { model: { name: "m" } });
  assert.deepEqual(fromPreset(undefined), {});
});
