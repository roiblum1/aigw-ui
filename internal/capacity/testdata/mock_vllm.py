# A stand-in for vLLM's OpenAI server: streams a short answer, reports usage
# with cached tokens, and keeps vLLM's token counters on /metrics.
import asyncio, json, time
from aiohttp import web

counters = {"vllm:prompt_tokens_total": 0.0, "vllm:prompt_tokens_cached_total": 0.0, "vllm:generation_tokens_total": 0.0,
            "vllm:num_requests_running": 0.0}
seen = set()
requests = {"n": 0}

def words(messages):
    n = 0
    for m in messages:
        c = m.get("content")
        if isinstance(c, list):
            c = " ".join(p.get("text", "") for p in c if isinstance(p, dict))
        n += len((c or "").split())
    return n

async def chat(req):
    body = await req.json()
    requests["n"] += 1
    prompt = max(words(body.get("messages", [])), 1)
    first = json.dumps(body.get("messages", [])[:1])
    cached = int(prompt * 0.8) if first in seen else 0
    seen.add(first)
    out = min(int(body.get("max_tokens") or body.get("max_completion_tokens") or 16), 24)
    counters["vllm:prompt_tokens_total"] += prompt
    counters["vllm:prompt_tokens_cached_total"] += cached
    counters["vllm:generation_tokens_total"] += out
    resp = web.StreamResponse(headers={"Content-Type": "text/event-stream"})
    await resp.prepare(req)
    counters["vllm:num_requests_running"] += 1
    rid, created = "chatcmpl-x", int(time.time())
    await asyncio.sleep(0.05)
    for i in range(out):
        chunk = {"id": rid, "object": "chat.completion.chunk", "created": created, "model": body["model"],
                 "choices": [{"index": 0, "delta": {"content": "tok "}, "finish_reason": None}]}
        await resp.write(b"data: " + json.dumps(chunk).encode() + b"\n\n")
        await asyncio.sleep(0.01)
    last = {"id": rid, "object": "chat.completion.chunk", "created": created, "model": body["model"],
            "choices": [{"index": 0, "delta": {}, "finish_reason": "length"}],
            "usage": {"prompt_tokens": prompt, "completion_tokens": out, "total_tokens": prompt + out,
                      "prompt_tokens_details": {"cached_tokens": cached}}}
    await resp.write(b"data: " + json.dumps(last).encode() + b"\n\ndata: [DONE]\n\n")
    counters["vllm:num_requests_running"] -= 1
    return resp

async def metrics(_):
    text = "".join(f'{k}{{engine="0",model_name="m"}} {v}\n' for k, v in counters.items())
    return web.Response(text=text)

async def models(_):
    return web.json_response({"object": "list", "data": [{"id": "m", "object": "model", "max_model_len": 8192}]})

app = web.Application(client_max_size=64 << 20)
app.add_routes([web.post("/v1/chat/completions", chat), web.get("/metrics", metrics), web.get("/v1/models", models)])
web.run_app(app, host="127.0.0.1", port=8000, print=None)
