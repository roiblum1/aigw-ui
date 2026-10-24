#!/usr/bin/env python3
"""The flags of `vllm serve`, for the hub's flag check (internal/vllmflags).

Run it inside the vLLM image that serves the model, so the list is that
image's own parser and nothing is guessed (hack/vllm-flags.sh does):

    vllm-flags.py catalog [IMAGE]       the flag list as JSON, on stdout
    vllm-flags.py verdicts CASES.json   what vLLM says of each argument list

`catalog` reads the parser `vllm serve` builds: every option, its value type,
choices, default and deprecation, and the keys of the JSON configs
(--speculative-config and the like). `verdicts` runs argument lists through
vLLM's own parser, its quick serve checks and its parallel and scheduler
config checks; internal/vllmflags tests that the hub reaches the same verdict
on every case.
"""

import argparse
import contextlib
import dataclasses
import enum
import io
import json
import re
import sys
import typing

MODE = sys.argv[1] if len(sys.argv) > 1 else "catalog"
ARG = sys.argv[2] if len(sys.argv) > 2 else ""
# vLLM fills its help texts only when --help is on the command line
# (vllm/engine/arg_utils.py NEEDS_HELP), read at import.
sys.argv = [sys.argv[0], "--help"]

import vllm  # noqa: E402
from vllm.engine.arg_utils import EngineArgs  # noqa: E402
from vllm.entrypoints.cli.serve import ServeSubcommand  # noqa: E402
from vllm.utils.argparse_utils import FlexibleArgumentParser  # noqa: E402

try:  # 0.30 and later
    from vllm.entrypoints.launchers.cli_args import validate_parsed_serve_args
except ImportError:  # older layouts
    from vllm.entrypoints.openai.cli_args import validate_parsed_serve_args


def version():
    # A CPU or ROCm build says 0.32.0+cpu: the flags are the release's.
    return vllm.__version__.split("+", 1)[0]


def serve_parser():
    """The parsers `vllm serve` uses: the top one (it rewrites the
    arguments) and the serve subcommand's (it holds the options)."""
    top = FlexibleArgumentParser(prog="vllm")
    subparsers = top.add_subparsers(dest="subparser")
    serve = ServeSubcommand().subparser_init(subparsers)
    return top, serve


def closure(fn, name):
    code = getattr(fn, "__code__", None)
    if code is None or fn.__closure__ is None or name not in code.co_freevars:
        return None
    return fn.__closure__[code.co_freevars.index(name)].cell_contents


def value_type(t):
    """What a value must look like, from the type callable argparse runs."""
    if t is None or t is str:
        return {"type": "str"}
    if t is int:
        return {"type": "int"}
    if t is float:
        return {"type": "float"}
    if t is json.loads:
        return {"type": "json"}
    name = getattr(t, "__name__", type(t).__name__)
    if name == "human_readable_int":
        return {"type": "hint"}
    if name == "human_readable_int_or_auto":
        return {"type": "hint_auto"}
    if name == "union_dict_and_str":
        return {"type": "dict_or_str"}
    if name == "_optional_type":
        return {**value_type(closure(t, "return_type")), "optional": True}
    if name == "_parse_type":
        return value_type(closure(t, "return_type"))
    if name == "parse_dataclass":
        cls = (t.__defaults__ or (None,))[0]
        if dataclasses.is_dataclass(cls):
            CONFIGS.add(cls)
            return {"type": "config", "config": cls.__name__}
    if name == "check_port":
        return {"type": "port"}
    if name == "<lambda>":
        # --moe-backend and its kind compare the value lowercased, with
        # dashes as underscores.
        try:
            if t("A-B") == "a_b":
                return {"type": "str", "normalize": "lower_underscore"}
        except Exception:
            pass
    # A converter this script does not know: any value passes the hub's
    # check, and the catalog says so.
    return {"type": "any", "converter": name}


CONFIGS = set()


def startup_config(dest):
    """The config class a JSON flag becomes at startup: speculative_config
    is vllm.config.SpeculativeConfig."""
    import vllm.config

    cls = getattr(vllm.config, "".join(w.capitalize() for w in dest.split("_")), None)
    if dataclasses.is_dataclass(cls):
        CONFIGS.add(cls)
        return cls
    return None


def kind(action):
    if isinstance(action, argparse.BooleanOptionalAction):
        return "bool"
    if isinstance(
        action,
        (argparse._StoreTrueAction, argparse._StoreFalseAction, argparse._StoreConstAction),
    ):
        return "switch"
    if isinstance(action, argparse._AppendAction):
        return "append"
    if isinstance(action, argparse._CountAction):
        return "count"
    return "value"


def jsonable(v):
    try:
        json.dumps(v)
        return v
    except TypeError:
        if dataclasses.is_dataclass(v):
            return None  # a config object: its keys are under "configs"
        return repr(v)


def first_sentence(text):
    text = " ".join((text or "").split()).replace("%%", "%")
    # A sentence ends at . ! or ? before a capital letter or the end, so
    # "e.g." and "0.9" do not end it.
    m = re.match(r"(.+?(?<!e\.g)(?<!i\.e)[.!?])(?=\s+[A-Z`\[(]|$)", text)
    text = m.group(1) if m else text
    return text if len(text) <= 200 else text[:197] + "..."


def field_type(hint):
    """A config key's type, as far as the hub checks it: choices, a nested
    config, or a plain kind."""
    out = {}
    args = typing.get_args(hint)
    origin = typing.get_origin(hint)
    if origin is typing.Annotated:
        return field_type(args[0])
    if origin in (typing.Union, getattr(__import__("types"), "UnionType", None)):
        inner = [a for a in args if a is not type(None)]
        if len(inner) < len(args):
            out["optional"] = True
        if len(inner) == 1:
            return {**field_type(inner[0]), **out}
        literals = [a for a in inner if typing.get_origin(a) is typing.Literal]
        if literals and len(literals) == len(inner):
            choices = [c for a in literals for c in typing.get_args(a)]
            return {"type": "choice", "choices": sorted(map(str, choices)), **out}
        return {"type": "any", **out}
    if origin is typing.Literal:
        return {"type": "choice", "choices": sorted(map(str, args))}
    if dataclasses.is_dataclass(hint):
        CONFIGS.add(hint)
        return {"type": "config", "config": hint.__name__}
    if isinstance(hint, type) and issubclass(hint, enum.Enum):
        # vLLM's validators look the member up by name, in any case
        # (CUDAGraphMode takes "full_and_piecewise").
        return {"type": "choice", "choices": sorted(hint.__members__), "fold": True}
    for t, name in ((bool, "bool"), (int, "int"), (float, "float"), (str, "str")):
        if hint is t:
            return {"type": name}
    if origin in (dict,) or hint is dict:
        return {"type": "object"}
    if origin in (list, tuple, set) or hint in (list, tuple, set):
        return {"type": "list"}
    return {"type": "any"}


def config_entry(cls):
    try:
        hints = typing.get_type_hints(cls, include_extras=True)
    except Exception:
        hints = {}
    extra = (getattr(cls, "__pydantic_config__", None) or {}).get("extra")
    fields = {}
    for f in dataclasses.fields(cls):
        if not f.init:
            continue
        fields[f.name] = field_type(hints.get(f.name, f.type))
    return {"strict": extra == "forbid", "fields": fields}


def catalog(image):
    _, serve = serve_parser()
    deprecated = getattr(serve, "_deprecated", set())
    group_of = {}
    for group in serve._action_groups:
        for action in group._group_actions:
            group_of[id(action)] = group.title or ""
    flags = []
    for action in serve._actions:
        if isinstance(action, (argparse._HelpAction, argparse._VersionAction)):
            continue
        if not action.option_strings:
            continue  # model_tag, the positional
        entry = {"options": list(action.option_strings), "dest": action.dest, "kind": kind(action)}
        if entry["kind"] in ("value", "append"):
            if action.nargs is not None:
                entry["nargs"] = str(action.nargs)
            entry.update(value_type(action.type))
            later = startup_config(action.dest) if entry.get("type") == "json" else None
            if later is not None:
                # Parsed as plain JSON; vLLM builds the config from it when
                # the engine starts (SpeculativeConfig(**value)), and an
                # unknown key stops it there.
                entry["config"] = later.__name__
                entry["config_at"] = "startup"
            if action.choices is not None:
                entry["choices"] = [str(c) for c in action.choices]
            if action.nargs == "?":
                entry["const"] = jsonable(action.const)
        if entry["kind"] == "switch":
            entry["const"] = jsonable(action.const)  # store_false: false
        default = jsonable(action.default)
        if default is not None and default is not argparse.SUPPRESS:
            entry["default"] = default
        if action in deprecated or getattr(action, "deprecated", False):
            entry["deprecated"] = True
        if group_of.get(id(action)):
            entry["group"] = group_of[id(action)]
        if action.help and action.help is not argparse.SUPPRESS:
            entry["help"] = first_sentence(action.help)
        flags.append(entry)
    configs = {}
    done = set()
    while CONFIGS - done:
        cls = min(CONFIGS - done, key=lambda c: c.__name__)
        done.add(cls)
        configs[cls.__name__] = config_entry(cls)
    flags.sort(key=lambda f: f["options"][0])
    return {
        "vllm": version(),
        "image": image,
        "python": sys.version.split()[0],
        "allow_abbrev": bool(serve.allow_abbrev),
        "flags": flags,
        "configs": dict(sorted(configs.items())),
    }


def cuda_like():
    """Parallel checks that differ by platform (EPLB is CUDA/ROCm only) are
    run as on the GPUs the models use."""
    try:
        from vllm.platforms import current_platform

        current_platform.is_cuda_alike = lambda: True
    except Exception:
        pass


def engine_checks(ns):
    """vLLM's parallel and scheduler config checks, as create_engine_config
    runs them, without a model (they need none)."""
    from vllm.config import ParallelConfig, SchedulerConfig

    ea = EngineArgs.from_cli_args(ns)
    if ea.data_parallel_hybrid_lb and ea.data_parallel_external_lb:
        raise ValueError("--data-parallel-hybrid-lb and --data-parallel-external-lb cannot be enabled together")
    external = ea.data_parallel_external_lb or ea.data_parallel_rank is not None
    if external and ea.nnodes <= 1:
        if ea.data_parallel_rank is None:
            raise ValueError("--data-parallel-external-lb requires a data-parallel rank")
        if ea.data_parallel_size_local not in (1, None):
            raise ValueError("an external data-parallel rank requires --data-parallel-size-local 1")
    names = {f.name for f in dataclasses.fields(ParallelConfig)}
    # Only what is set: create_engine_config resolves the rest first.
    kwargs = {k: getattr(ea, k) for k in names if not k.startswith("_") and getattr(ea, k, None) is not None}
    kwargs["data_parallel_external_lb"] = external
    ParallelConfig(**kwargs)
    # SchedulerConfig.verify_max_model_len, on the values given on the
    # command line: a value the engine picks per GPU when it is left out is
    # not compared (internal/vllmflags does the same).
    length = ea.max_model_len if isinstance(ea.max_model_len, int) and ea.max_model_len > 0 else None
    batched, seqs = ea.max_num_batched_tokens, ea.max_num_seqs
    active = getattr(ea, "max_num_active_seqs", None)
    sched = {
        "max_num_batched_tokens": batched if batched is not None else max(seqs or 1, length or 1),
        "max_num_seqs": seqs if seqs is not None and (batched is not None or active is not None) else 1,
    }
    if batched is not None and ea.enable_chunked_prefill is not None:
        sched["enable_chunked_prefill"] = ea.enable_chunked_prefill
    if ea.long_prefill_token_threshold and length:
        sched["long_prefill_token_threshold"] = ea.long_prefill_token_threshold
    if active is not None and seqs is not None:
        sched["max_num_active_seqs"] = active
    SchedulerConfig(max_model_len=length or sched["max_num_batched_tokens"], is_encoder_decoder=False, **sched)


def startup_configs(ns):
    """What building a JSON flag's config at startup refuses before it needs
    the model: a key the config does not have, a value its field's type
    does not take."""
    import pydantic

    _, serve = serve_parser()
    for action in serve._actions:
        value = getattr(ns, action.dest, None)
        if not isinstance(value, dict) or value_type(action.type).get("type") != "json":
            continue
        cls = startup_config(action.dest)
        if cls is None:
            continue
        fields = {f.name: f for f in dataclasses.fields(cls) if f.init}
        unknown = sorted(set(value) - set(fields))
        if unknown:
            raise TypeError(f"{cls.__name__}.__init__() got an unexpected keyword argument '{unknown[0]}'")
        hints = typing.get_type_hints(cls, include_extras=True)
        for key, v in value.items():
            pydantic.TypeAdapter(hints[key]).validate_python(v)


def verdict(argv):
    top, _ = serve_parser()
    err = io.StringIO()
    try:
        with contextlib.redirect_stderr(err):
            ns = top.parse_args(["serve", *argv])
    except SystemExit:
        return {"ok": False, "stage": "parse", "error": err.getvalue().strip().splitlines()[-1]}
    except Exception as e:  # a converter's own error (KeyError from an enum) stops vLLM too
        return {"ok": False, "stage": "parse", "error": f"{type(e).__name__}: {e}"}
    try:
        validate_parsed_serve_args(ns)
    except (ValueError, TypeError) as e:
        return {"ok": False, "stage": "serve", "error": str(e)}
    try:
        startup_configs(ns)
    except Exception as e:
        return {"ok": False, "stage": "startup", "error": " ".join(str(e).split())[:300]}
    try:
        engine_checks(ns)
    except Exception as e:  # pydantic's ValidationError too
        return {"ok": False, "stage": "engine", "error": " ".join(str(e).split())[:300]}
    return {"ok": True}


def main():
    if MODE == "catalog":
        json.dump(catalog(ARG), sys.stdout, indent=1, sort_keys=False)
        sys.stdout.write("\n")
    elif MODE == "verdicts":
        cuda_like()
        with open(ARG) as f:
            cases = json.load(f)
        out = []
        for case in cases:
            out.append({"name": case["name"], "args": case["args"], **verdict(case["args"])})
        json.dump({"vllm": version(), "cases": out}, sys.stdout, indent=1)
        sys.stdout.write("\n")
    else:
        sys.exit(__doc__)


if __name__ == "__main__":
    main()
