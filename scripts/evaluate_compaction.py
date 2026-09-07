#!/usr/bin/env python3
"""Opt-in, synthetic native/OM evaluation. Python 3.10+, standard library only.

No subprocess is reachable in dry-run/replay. Live modes own only new evaluation
threads. Scores are deterministic exact-match rubric checks, never model judges.
"""
from __future__ import annotations

import argparse
import ast
import copy
import ctypes
import functools
import math
from dataclasses import dataclass, field
import hashlib
import inspect
import json
import os
from pathlib import Path
import random
import re
import selectors
import shlex
import shutil
import subprocess
import sys
import tempfile
import time
from typing import Callable

SCHEMA = 1
VARIANTS = ('native', 'om')
FIELDS = ('inputTokens', 'cachedInputTokens', 'cacheWriteInputTokens',
          'outputTokens', 'reasoningOutputTokens', 'totalTokens')
ROOT = Path(__file__).resolve().parents[1]
FIXTURE = ROOT / 'eval/fixtures/continuity.json'


class HarnessError(Exception):
    """Fail closed without claiming release evidence."""


@functools.lru_cache(maxsize=1)
def clock_source():
    """Public elapsed clocks that include suspend; never fall back to epoch time."""
    try:
        if sys.platform == 'darwin':
            class Timebase(ctypes.Structure):
                _fields_ = [('numer', ctypes.c_uint32), ('denom', ctypes.c_uint32)]
            lib = ctypes.CDLL(None)
            lib.mach_timebase_info.argtypes = [ctypes.POINTER(Timebase)]
            lib.mach_timebase_info.restype = ctypes.c_int
            lib.mach_continuous_time.argtypes = []
            lib.mach_continuous_time.restype = ctypes.c_uint64
            info = Timebase()
            if lib.mach_timebase_info(ctypes.byref(info)) != 0 or not info.numer or not info.denom:
                raise ValueError('invalid mach timebase')
            scale = info.numer / info.denom / 1_000_000_000
            return 'mach_continuous_time', lambda: lib.mach_continuous_time() * scale
        if sys.platform.startswith('linux') and hasattr(time, 'CLOCK_BOOTTIME'):
            time.clock_gettime(time.CLOCK_BOOTTIME)  # Check availability before paid work.
            return 'CLOCK_BOOTTIME', lambda: time.clock_gettime(time.CLOCK_BOOTTIME)
    except (AttributeError, OSError, ValueError) as exc:
        raise HarnessError('suspend-aware elapsed clock unavailable') from exc
    raise HarnessError('suspend-aware elapsed clock unavailable on this platform')


def continuous_time():
    try:
        value = clock_source()[1]()
        if not math.isfinite(value):
            raise ValueError('nonfinite elapsed clock')
        return value
    except Exception as exc:
        raise HarnessError('suspend-aware elapsed clock unavailable or invalid') from exc


def canonical(value):
    return json.dumps(value, sort_keys=True, ensure_ascii=False, separators=(',', ':')).encode()


def digest(value):
    return hashlib.sha256(value).hexdigest()


def write_json(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, sort_keys=True, indent=2) + '\n')


def tree_hash(path):
    entries = []
    for p in sorted(path.rglob('*')):
        if p.is_symlink():
            raise HarnessError('symlinks are not accepted in candidate or fixture trees')
        if p.is_file() and '__pycache__' not in p.parts:
            entries.append([p.relative_to(path).as_posix(), digest(p.read_bytes())])
    return digest(canonical(entries))


def observed_delta(previous, current):
    if previous is None or current is None:
        return None
    if current < previous:
        raise HarnessError('usage counter reset requires explicit new segment')
    return current - previous


def redact(value):
    if isinstance(value, dict):
        return {k: ('[REDACTED]' if re.search(r'authorization|api.?key|access.?token|refresh.?token|id.?token|email', k, re.I)
                    else redact(v)) for k, v in value.items()}
    if isinstance(value, list):
        return [redact(v) for v in value]
    if isinstance(value, str):
        return re.sub(r'(?i)(bearer\s+\S+|sk-[A-Za-z0-9_-]+|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)', '[REDACTED]', value)
    return value



def safe_relative(name):
    path = Path(name)
    if not name or path.is_absolute() or '..' in path.parts or name in ('.', 'AGENTS.md'):
        raise HarnessError('fixture path escapes workspace or shadows runner instructions')
    return path



def executes_script(command, target, cwd=None):
    """Recognize executed workload scripts, not cat/echo/source-text mentions."""
    try:
        lexer = shlex.shlex(command, posix=True, punctuation_chars=';&|()')
        lexer.whitespace_split = True
        tokens = list(lexer)
    except ValueError:
        return False
    segments, current = [], []
    for token in tokens + [';']:
        if token and all(ch in ';&|()' for ch in token):
            if current:
                segments.append(current)
            current = []
        else:
            current.append(token)
    directory = cwd or '.'
    expected = os.path.normpath(os.path.join(directory, target))

    def matches(path):
        return os.path.normpath(os.path.join(directory, path)) == expected

    for words in segments:
        while words and ('=' in words[0] or words[0] in ('env', 'command', 'exec')):
            words.pop(0)
        if not words:
            continue
        executable = Path(words[0]).name
        if executable == 'cd' and len(words) == 2:
            directory = os.path.normpath(os.path.join(directory, words[1]))
            continue
        if matches(words[0]):
            return True
        if executable in ('sh', 'bash', 'zsh'):
            pos = next((i + 1 for i, word in enumerate(words[1:], 1)
                        if word.startswith('-') and 'c' in word[1:]), len(words))
            if pos < len(words) and executes_script(words[pos], os.path.abspath(expected), os.path.abspath(directory)):
                return True
        if re.fullmatch(r'python(?:[0-9]+(?:\.[0-9]+)*)?', executable):
            # Python executes only its selected script/module/code, not arbitrary
            # positional arguments (e.g. py_compile's source file arguments).
            args = words[1:]
            while args:
                option = args.pop(0)
                if option == '--':
                    if args and matches(args[0]):
                        return True
                    break
                if option == '-m' or option.startswith('-m'):
                    module = args[0] if option == '-m' and args else option[2:]
                    if matches(module.replace('.', '/') + '.py'):
                        return True
                    break
                if option == '-c' or option.startswith('-c'):
                    code = args[0] if option == '-c' and args else option[2:]
                    try:
                        tree = ast.parse(code)
                        for node in ast.walk(tree):
                            if (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
                                    and node.func.attr == 'run_path' and node.args
                                    and isinstance(node.args[0], ast.Constant)
                                    and isinstance(node.args[0].value, str) and matches(node.args[0].value)):
                                return True
                    except SyntaxError:
                        pass
                    break
                if option in ('-W', '-X'):
                    args = args[1:]  # interpreter option value, not a script
                elif not option.startswith('-'):
                    if matches(option):
                        return True
                    break
    return False


def action_evidence(fixture, workspace, cycle, before=False, prior=None):
    """Read actual per-probe artifacts, never self-reported action success."""
    result = {}
    for case in fixture.cases:
        effect = case.get('effect')
        if not effect:
            continue
        target = workspace / effect['path'].format(cycle=cycle)
        original = workspace / effect['unchanged']
        if target.is_symlink() or original.is_symlink():
            raise HarnessError('action evidence cannot be a symlink')
        if before:
            result[case['id']] = {'absent': not target.exists()}
            continue
        try:
            actual = json.loads(target.read_text()) if target.is_file() else None
        except (ValueError, UnicodeError):
            actual = None
        expected_original = fixture.workload['files'][effect['unchanged']].encode()
        result[case['id']] = {
            'performed': bool(prior and prior.get(case['id'], {}).get('absent') and actual == effect['content']),
            'unchanged': original.is_file() and original.read_bytes() == expected_original,
            'artifact_sha256': digest(target.read_bytes()) if target.is_file() else None,
            'provenance': 'new cycle-specific workspace artifact plus original completed-operation record'}
    return result


@dataclass
class Fixture:
    split: str
    seed: int
    cases: list
    workload: dict
    fixture_hash: str
    split_hash: str
    rubric_hash: str

    @classmethod
    def load(cls, path, split):
        raw = path.read_bytes()
        data = json.loads(raw)
        if not isinstance(data, dict):
            raise HarnessError('invalid fixture object')
        if data.get('schema') != SCHEMA or split not in ('pilot', 'release'):
            raise HarnessError('unsupported fixture schema/split')
        ids, markers = set(), {'pilot': set(), 'release': set()}
        for case in data['cases']:
            if case['id'] in ids or case['split'] not in markers:
                raise HarnessError('case IDs must be unique and splits disjoint')
            ids.add(case['id'])
            markers[case['split']].update(case.get('markers', []))
        if markers['pilot'] & markers['release']:
            raise HarnessError('generated fact/error markers overlap splits')
        for selected in ('pilot', 'release'):
            workload = data['workloads'][selected]
            files = workload['files']
            for name, content in files.items():
                safe_relative(name)
                if not isinstance(content, str) or len(content) > 1_000_000:
                    raise HarnessError('invalid source text')
            visible = canonical(workload).decode()
            other = 'release' if selected == 'pilot' else 'pilot'
            if any(marker in visible for marker in markers[other]):
                raise HarnessError('held-out split marker leaked into workload')
            for case in (c for c in data['cases'] if c['split'] == selected):
                if type(case.get('critical')) is not bool or not isinstance(case.get('probe'), str):
                    raise HarnessError('invalid fixture rubric')
                if 'source' in case:
                    source = case['source']
                    safe_relative(source['path'])
                    lines = files.get(source['path'], '').splitlines()
                    n = source['line']
                    if type(n) is not int or n < 1 or n > len(lines) or lines[n - 1] != source['text']:
                        raise HarnessError('gold exact source does not match workload')
                if 'effect' in case:
                    safe_relative(case['effect']['path'].format(cycle=1))
                    safe_relative(case['effect']['unchanged'])
        # Only selected cases/workload leave validation; no held-out scoring in pilot.
        cases = [c for c in data['cases'] if c['split'] == split]
        workload = data['workloads'][split]
        if not cases or not any(c['critical'] for c in cases):
            raise HarnessError('empty rubric')
        frozen = {'seed': data['seed'], 'cases': cases, 'workload': workload,
                  'generator': 'service-incident-v1', 'schema': SCHEMA,
                  'runner_sha256': digest(Path(__file__).read_bytes())}
        rubric = {key: value for key, value in frozen.items() if key != 'runner_sha256'}
        return cls(split, data['seed'], cases, workload, digest(raw), digest(canonical(frozen)), digest(canonical(rubric)))

    def check_hash(self, expected):
        if expected != self.split_hash:
            raise HarnessError('frozen split hash mismatch; use a fresh evidence campaign')

    def prepare(self, path):
        path.mkdir(parents=True, exist_ok=False)
        for name, content in self.workload['files'].items():
            target = path / name
            if Path(name).is_absolute() or '..' in Path(name).parts:
                raise HarnessError('fixture path escapes workspace')
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(content)
        (path / 'batches').mkdir()
        (path / 'reports').mkdir()
        (path / 'AGENTS.md').write_text(
            'This is a synthetic service engineering task. Work only in this workspace. '
            'Do not use network, other directories, other agents or external applications. '
            'Keep concise workspace notes when useful. Current user corrections supersede '
            'historical proposals. Do not repeat confirmed completed operations.\n')

    def batch(self, index):
        # Each batch is a new service incident with independent observations and an
        # actual analysis task. No padding by repeating a fixture/answer transcript.
        rng = random.Random(f'{self.seed}:{self.split}:{index}:service-incident-v1')
        regions = ['west', 'east', 'north', 'south']
        rows = []
        for n in range(420):
            region = rng.choice(regions)
            latency = rng.randrange(12, 950)
            status = rng.choices([200, 429, 503], [88, 8, 4])[0]
            rows.append({'request': f'{rng.getrandbits(48):012x}-{index:05d}-{n:04d}', 'region': region,
                         'latency_ms': latency, 'status': status, 'retry': status != 200,
                         'route': rng.choice(['/ingest', '/query', '/snapshot', '/health']),
                         'payload_bytes': rng.randrange(120, 45000)})
        return '\n'.join(json.dumps(row, sort_keys=True) for row in rows) + '\n'

    def probe_prompt(self, cycle):
        # Only neutral questions/IDs are exposed, never expected/forbidden answers.
        questions = [{'id': c['id'], 'question': c['probe']} for c in self.cases]
        return ('Recovery check after context compaction ' + str(cycle) + '. '
                'Use workspace evidence and available notes/memory. Answer every question. '
                'For action questions carry out the currently authorized next action for this cycle, then report its action ID. '
                'For exact-source questions include the actual source {path,line,text} with a one-based line. '
                'Return only one JSON object: {"answers": {"case-id": {"answer": "...", '
                '"action": "none or action ID", "source": {"path":"...","line":1,"text":"..."}}}}. '
                'Use exact short values and omit source when irrelevant.\n' + json.dumps(questions))


@dataclass
class RunBudget:
    max_seconds: int | None
    max_input_tokens: int | None
    clock: Callable = continuous_time
    input_tokens: int = 0
    overshoot: int = 0
    started: float = field(init=False)
    epoch_clock: Callable = time.time
    active_clock: Callable = time.monotonic

    def __post_init__(self):
        try:
            self.started = self.clock()
        except Exception as exc:
            raise HarnessError('elapsed clock unavailable') from exc
        if not isinstance(self.started, (int, float)) or not math.isfinite(self.started):
            raise HarnessError('invalid elapsed clock')
        self.last_clock = self.started
        self.started_epoch, self.started_active = self.epoch_clock(), self.active_clock()
        self.clock_name = clock_source()[0] if self.clock is continuous_time else 'injected-clock'
        self.max_epoch_discrepancy = self.max_active_discrepancy = 0.

    def elapsed(self):
        try:
            now = self.clock()
        except Exception as exc:
            raise HarnessError('elapsed clock unavailable') from exc
        if not isinstance(now, (int, float)) or not math.isfinite(now) or now < self.last_clock:
            raise HarnessError('elapsed clock invalid or moved backwards')
        self.last_clock = now
        return now - self.started

    def timing(self):
        # Diagnostics must not replace a primary failure with a second clock error.
        try:
            elapsed = self.elapsed()
            epoch_now = self.epoch_clock()
            epoch = epoch_now - self.started_epoch
            active = self.active_clock() - self.started_active
            epoch_delta, active_delta = epoch - elapsed, elapsed - active
            self.max_epoch_discrepancy = max(self.max_epoch_discrepancy, abs(epoch_delta))
            self.max_active_discrepancy = max(self.max_active_discrepancy, abs(active_delta))
            return {'source': self.clock_name, 'includes_suspend': self.clock is continuous_time,
                    'elapsed_seconds': elapsed, 'snapshot_epoch_seconds': epoch_now, 'epoch_elapsed_seconds': epoch,
                    'active_monotonic_elapsed_seconds': active,
                    'epoch_minus_elapsed_seconds': epoch_delta, 'elapsed_minus_active_seconds': active_delta,
                    'max_abs_epoch_discrepancy_seconds': self.max_epoch_discrepancy,
                    'max_abs_active_discrepancy_seconds': self.max_active_discrepancy,
                    'clock_discrepancy_warning': max(self.max_epoch_discrepancy, self.max_active_discrepancy) > 1}
        except Exception as exc:
            return {'source': self.clock_name, 'elapsed_seconds': None, 'clock_error': str(exc),
                    'clock_discrepancy_warning': True}

    def remaining(self):
        if self.max_seconds is None:
            return 60.0
        return max(0.0, self.max_seconds - self.elapsed())

    def check(self):
        if self.max_seconds is not None and self.remaining() <= 0:
            raise HarnessError('run-wide wall deadline reached')
        if self.max_input_tokens is not None and self.input_tokens >= self.max_input_tokens:
            self.overshoot = max(0, self.input_tokens - self.max_input_tokens)
            raise HarnessError('run-wide input ceiling reached')

    def charge(self, amount):
        self.input_tokens += amount
        self.overshoot = max(0, self.input_tokens - (self.max_input_tokens or self.input_tokens))
        self.check()


@dataclass
class UsageSegment:
    name: str
    baseline: dict
    reason: str
    previous: dict = field(default_factory=dict)
    measured: dict = field(default_factory=dict)
    samples: int = 0

    def observe(self, current):
        if type(current.get('inputTokens')) is not int or current['inputTokens'] < 0:
            raise HarnessError('input usage unavailable; cannot monitor authorized budget')
        delta = {}
        for key in FIELDS:
            value = current.get(key)
            if value is not None and (type(value) is not int or value < 0):
                raise HarnessError('invalid usage counter')
            old = self.previous.get(key, self.baseline.get(key))
            delta[key] = observed_delta(old, value)
        for key in FIELDS:
            if delta[key] is not None:
                self.measured[key] = (self.measured.get(key, 0) + delta[key]
                                      if self.measured.get(key, 0) is not None else None)
            # Missing optional measurements stay unknown, even if schema has default 0.
            if current.get(key) is None:
                self.measured[key] = None
        self.previous = dict(current)
        self.samples += 1
        if delta['inputTokens'] is None:
            raise HarnessError('input usage delta unavailable')
        return delta['inputTokens']


class Usage:
    def __init__(self):
        self.segments = []
        self.observations = []

    def start(self, name, baseline, reason):
        if not reason or any(s.name == name for s in self.segments):
            raise HarnessError('usage segment requires unique identity and reset provenance')
        if type(baseline.get('inputTokens')) is not int or baseline['inputTokens'] < 0:
            raise HarnessError('usage segment needs observed input baseline')
        self.segments.append(UsageSegment(name, baseline, reason))

    def observe(self, value, turn_id):
        amount = self.segments[-1].observe(value.get('total', {}))
        self.observations.append({'observation_index': len(self.observations) + 1,
                                  'turn_id': turn_id, 'total': value.get('total'),
                                  'active_context': value.get('last'),
                                  'last_provenance': 'tokenUsage.last; request usage or context-only estimate',
                                  'model_context_window': value.get('modelContextWindow'),
                                  'provenance': 'thread/tokenUsage/updated.tokenUsage'})
        return amount

    def totals(self):
        result = {}
        for key in FIELDS:
            values = [s.measured.get(key) for s in self.segments]
            result[key] = sum(values) if values and all(v is not None for v in values) else None
        return result


class VariantRun:
    def __init__(self, name, fixture, budget):
        self.name, self.fixture, self.budget = name, fixture, budget
        self.thread_id = None
        self.workspace = None
        self.usage = Usage()
        self.compactions, self.seen, self.probes = [], set(), {}
        self.turns, self.messages, self.commands = {}, {}, []
        self.effective = {}
        self.active_turn = None
        self.reroutes = []
        self.compaction_starts = {}
        self.workload_batches = []
        self.interruptions = []
        self.deferral_evidence = None
        self.forbidden_attempts = set()
        self.agent_usage_boundaries = {}
        self.pending_compaction = None
        self.started_commands = {}
        self.live_thread_id = None
        self.requested_turn_ids = set()
        self.manual_compaction_requested = False

    def start_thread(self, ident):
        if self.thread_id or not ident:
            raise HarnessError('evaluation owns exactly one new thread per variant')
        self.thread_id = ident
        self.start_segment('new-thread', {k: 0 for k in FIELDS},
                           'thread/start created empty evaluation thread; counters originate at zero')

    def start_segment(self, ident, baseline, reason):
        self.usage.start(ident, baseline, reason)

    def event(self, event):
        p, method = event.get('params', {}), event.get('method')
        if p.get('threadId') != self.thread_id or self.thread_id is None:
            return
        if method in ('item/started', 'item/completed') and p.get('item', {}).get('type') == 'commandExecution':
            item = p['item']
            if method == 'item/started':
                self.started_commands[item.get('id')] = p.get('turnId')
            for case in self.fixture.cases:
                script = case.get('forbidden_script')
                if script:
                    target = str(self.workspace / script) if self.workspace else script
                    if executes_script(item.get('command', ''), target, item.get('cwd')):
                        self.forbidden_attempts.add(item.get('id', '<missing-id>'))
        if method == 'model/rerouted':
            self.reroutes.append(redact(p))
            raise HarnessError('model rerouted away from authorized matched selection')
        if method == 'turn/started':
            self.active_turn = p.get('turn', {}).get('id')
        if method in ('item/started', 'item/completed'):
            item = p.get('item', {})
            if item.get('type') != 'contextCompaction':
                self.pending_compaction = None  # Later work makes delayed attribution ambiguous.
            elif method == 'item/started' and item.get('id') not in self.compaction_starts:
                boundary = {'thread_id': self.thread_id, 'item_id': item.get('id'),
                            'turn_id': p.get('turnId'), 'completed_at_ms': None,
                            'before_usage': copy.deepcopy(self.usage.observations[-1]) if self.usage.observations else None,
                            'after_usage': None, 'lifecycle_matched': False, 'context_ambiguous': False}
                self.compaction_starts[item.get('id')] = boundary
                self.pending_compaction = boundary
        if method == 'thread/tokenUsage/updated':
            self.budget.charge(self.usage.observe(p.get('tokenUsage', {}), p.get('turnId')))
            boundary = self.pending_compaction
            sample = self.usage.observations[-1]
            context = sample.get('active_context') or {}
            # Codex 0.153.4 emits request usage followed by a context-only estimate
            # (zero input/output, total=context size). The estimate may precede
            # item/completed. Never substitute the compaction request's last usage.
            if (boundary and p.get('turnId') == boundary['turn_id']
                    and type(context.get('totalTokens')) is int and context['totalTokens'] > 0
                    and context.get('inputTokens') == 0 and context.get('outputTokens') == 0
                    and (boundary['after_usage'] is not None
                         or context != (boundary['before_usage'] or {}).get('active_context'))):
                prior = boundary['after_usage']
                if prior and prior['active_context'] != context:
                    boundary['context_ambiguous'] = True
                elif prior is None:
                    boundary['after_usage'] = copy.deepcopy(sample)
        if method == 'item/completed':
            item = p.get('item', {})
            if item.get('type') == 'contextCompaction':
                if not isinstance(item.get('id'), str) or not item['id']:
                    raise HarnessError('completed compaction has no stable item identity')
                key = (self.thread_id, item['id'])
                if key not in self.seen:
                    self.seen.add(key)
                    boundary = self.compaction_starts.get(item['id'])
                    if boundary is None:
                        boundary = {'thread_id': self.thread_id, 'item_id': item['id'],
                                    'turn_id': p.get('turnId'), 'before_usage': None, 'after_usage': None}
                    boundary['lifecycle_matched'] = (boundary is self.compaction_starts.get(item['id'])
                                                       and boundary['turn_id'] == p.get('turnId')
                                                       and isinstance(boundary['turn_id'], str) and bool(boundary['turn_id']))
                    boundary['completed_at_ms'] = p.get('completedAtMs')
                    self.compactions.append(boundary)
            elif item.get('type') == 'agentMessage':
                self.messages.setdefault(p.get('turnId'), {})[item.get('id')] = item.get('text', '')
                self.agent_usage_boundaries[p.get('turnId')] = len(self.usage.observations)
            elif item.get('type') == 'commandExecution':
                self.commands.append({'turn_id': p.get('turnId'), 'item': item})
        if method == 'turn/completed':
            turn = p.get('turn', {})
            self.turns[turn.get('id')] = turn
            self.active_turn = None
            self.pending_compaction = None
        if method == 'error' and not p.get('willRetry', False):
            raise HarnessError('host reported a non-retryable turn error')

    def require_usage_since(self, samples, turn_id=None):
        observations = self.usage.observations
        if turn_id is None:
            turn_id = (next(reversed(self.agent_usage_boundaries)) if self.agent_usage_boundaries
                       else (observations[-1]['turn_id'] if observations else None))
        boundary = max(samples, self.agent_usage_boundaries.get(turn_id, samples))
        baseline = observations[boundary - 1]['total']['inputTokens'] if boundary else 0
        if not any(o['turn_id'] == turn_id and o['total']['inputTokens'] > baseline
                   for o in observations[boundary:]):
            raise HarnessError('usage telemetry missing or stale for completed work; stopping before another request')

    def compaction_evidence(self, limit):
        evidence = []
        config = self.effective.get('config', {})
        policy_verified = (self.live_thread_id == self.thread_id and self.live_thread_id is not None
                           and type(config.get('model_auto_compact_token_limit')) is int
                           and config['model_auto_compact_token_limit'] == limit
                           and config.get('model_auto_compact_token_limit_scope') == 'total'
                           and not self.manual_compaction_requested and not self.reroutes)
        for cycle in self.compactions:
            before, after = cycle['before_usage'], cycle['after_usage']
            pre = (before or {}).get('active_context') or {}
            post = (after or {}).get('active_context') or {}
            pre_total, post_total = pre.get('totalTokens'), post.get('totalTokens')
            reduction = (cycle.get('lifecycle_matched') and not cycle.get('context_ambiguous')
                         and type(pre_total) is int and type(post_total) is int and 0 < post_total < pre_total
                         and after['observation_index'] > before['observation_index']
                         and after['turn_id'] == cycle['turn_id'])
            request_usage = type(pre.get('inputTokens')) is int and pre['inputTokens'] > 0
            evidence.append({
                'item_id': cycle['item_id'],
                'configured_policy_compaction_verified': bool(policy_verified and reduction
                                                              and cycle['turn_id'] in self.requested_turn_ids),
                'context_reduction_verified': bool(reduction),
                'expected_policy': {'threshold': limit, 'scope': 'total'},
                'observed_effective_policy': {'threshold': config.get('model_auto_compact_token_limit'),
                                              'scope': config.get('model_auto_compact_token_limit_scope')},
                'owned_normal_turn_verified': (self.live_thread_id == self.thread_id and self.live_thread_id is not None
                                               and cycle['turn_id'] in self.requested_turn_ids),
                'manual_or_forced_compaction_requested': self.manual_compaction_requested,
                'before_last_total_tokens': pre_total,
                'before_last_kind': ('request_usage' if request_usage else
                                     'context_estimate' if pre.get('inputTokens') == 0 and pre.get('outputTokens') == 0
                                     else 'unavailable'),
                'after_context_estimate_tokens': post_total,
                'observed_request_at_or_above_threshold': (pre_total >= limit if request_usage and type(pre_total) is int else None),
                'trigger_context_tokens': None, 'trigger_reason': None, 'trigger_threshold_verified': None,
                'trigger_unavailable_reason': 'Public App Server events omit the internal trigger estimate and cause; '
                    'last request usage excludes local additions. Model-requested windows, comp_hash changes, '
                    'and model-window changes cannot be distinguished as causes from this lifecycle alone.',
                'provenance': ('live driver-owned thread; preflight config/read evidence retained'
                               if self.live_thread_id == self.thread_id and self.live_thread_id is not None
                               else 'unverified live provenance; event/replay observations only'),
                'grade_definition': 'Configured-policy compaction requires matching effective policy, an owned normal turn, '
                    'no manual/forced request or reroute, and matched lifecycle/context reduction. '
                    'It does not verify numerical threshold crossing or its causality.'})
        return evidence

    def begin_probe(self, cycle, turn_id):
        if (cycle != len(self.compactions) or cycle != len(self.probes) + 1
                or any(p['scores'] is None for p in self.probes.values())):
            raise HarnessError('unprobed compaction: each cycle requires its own immediate recovery')
        if not turn_id or any(p['turn_id'] == turn_id for p in self.probes.values()):
            raise HarnessError('probe turn must have a distinct identity')
        self.probes[cycle] = {'turn_id': turn_id, 'scores': None}

    def finish_probe(self, cycle, turn_id, response, effects=None):
        probe = self.probes.get(cycle)
        if not probe or probe['turn_id'] != turn_id or probe['scores'] is not None:
            raise HarnessError('probe response identity mismatch or duplicate score')
        # A compaction during the probe itself makes the interrupted cycle unscorable.
        if len(self.compactions) != cycle:
            raise HarnessError('compaction occurred before its predecessor recovery completed')
        answers = response.get('answers', {}) if isinstance(response, dict) else {}
        if not isinstance(answers, dict):
            answers = {}
        effects = effects or {}
        scores = []
        for case in self.fixture.cases:
            answer = answers.get(case['id'], {})
            if not isinstance(answer, dict):
                answer = {}
            text = answer.get('answer', '')
            stale = any(word.casefold() in str(text).casefold() for word in case.get('forbidden', []))
            repeated = answer.get('action') == case.get('forbidden_action') if 'forbidden_action' in case else False
            if 'effect' in case:
                repeated |= effects.get(case['id'], {}).get('unchanged') is not True or bool(self.forbidden_attempts)
            correct = (isinstance(text, str) and ('expected' not in case or text.strip() == case['expected'])
                       and ('expected_action' not in case or answer.get('action') == case['expected_action'])
                       and ('source' not in case or answer.get('source') == case['source'])
                       and not stale and not repeated)
            if 'effect' in case:
                correct &= effects.get(case['id'], {}).get('performed') is True
            scores.append({'case_id': case['id'], 'critical': case['critical'], 'correct': correct,
                           'stale': stale, 'repeated': repeated, 'exact_source': 'source' in case})
        probe['scores'] = scores
        probe['response'] = response
        probe['effects'] = effects

    def scorecard(self):
        scores = [s for p in self.probes.values() for s in (p['scores'] or [])]
        noncritical = [s for s in scores if not s['critical']]
        return {'actual_compactions': len(self.compactions),
                'scored_recoveries': sum(p['scores'] is not None for p in self.probes.values()),
                'critical_failures': sum(s['critical'] and not s['correct'] for s in scores),
                'stale_corrections': sum(s['stale'] for s in scores),
                'repeated_completed_actions': max(len(self.forbidden_attempts), sum(s['repeated'] for s in scores)),
                'noncritical_accuracy': (sum(s['correct'] for s in noncritical) / len(noncritical)
                                          if noncritical else None),
                'exact_source_checks': sum(s['exact_source'] for s in scores),
                'exact_source_failures': sum(s['exact_source'] and not s['correct'] for s in scores)}

    def result(self, compact_limit=200000):
        return {'scorecard': self.scorecard(), 'compactions': self.compactions, 'probes': self.probes,
                'usage': self.usage.totals(),
                'usage_provenance': {k: ('thread/tokenUsage/updated.tokenUsage.total.' + k
                                          if self.usage.totals()[k] is not None else None) for k in FIELDS},
                'usage_segments': [vars(s) for s in self.usage.segments],
                'usage_observations': self.usage.observations, 'effective': self.effective, 'reroutes': self.reroutes,
                'compaction_evidence': self.compaction_evidence(compact_limit),
                'workload_batches': self.workload_batches, 'interruptions': self.interruptions,
                'deferral_evidence': self.deferral_evidence, 'forbidden_attempts': sorted(self.forbidden_attempts),
                'checkpoints': {str(n): [self.probes[k] for k in range(1, n + 1)]
                                for n in (10, 25, 50) if all(k in self.probes for k in range(1, n + 1))}}


def quality(native, om, cycles=50):
    n, o = native.scorecard(), om.scorecard()
    reasons = []
    for name, score in (('native', n), ('om', o)):
        if score['actual_compactions'] != cycles or score['scored_recoveries'] != cycles:
            reasons.append(f'{name}: requires {cycles} distinct compactions with individual recoveries')
    if o['critical_failures'] or o['stale_corrections'] or o['repeated_completed_actions']:
        reasons.append('OM critical recovery/correction/action failure')
    if n['noncritical_accuracy'] is None or o['noncritical_accuracy'] is None:
        reasons.append('noncritical accuracy unavailable')
    elif o['noncritical_accuracy'] < n['noncritical_accuracy']:
        reasons.append('OM noncritical accuracy below native')
    comparison = 'not established'
    if not reasons:
        comparison = 'equal quality' if n == o else 'OM meets quality gate'
    return {'quality_pass': not reasons, 'eligibility_reasons': reasons, 'comparison': comparison}


class RpcClient:
    """Separate replies, notifications and server requests, including during waits."""
    def __init__(self, receive, send, budget, notify):
        self.receive, self.send, self.budget, self.notify = receive, send, budget, notify
        self.next_id, self.pending, self.replies = 1, set(), {}
        self.request_methods, self.observer = {}, None

    def send_request(self, method, params):
        self.budget.check()
        ident = self.next_id
        self.next_id += 1
        self.pending.add(ident)
        self.request_methods[ident] = method
        self.send({'method': method, 'id': ident, 'params': params})
        return ident

    def pump(self, timeout=30):
        error = None
        try:
            self.budget.check()
            if self.observer:
                self.observer()
            event = self.receive(min(1, timeout, self.budget.remaining()))
            if event is None:
                self.budget.check()
                return
            if not isinstance(event, dict):
                raise HarnessError('invalid RPC message')
            if 'method' in event:
                if 'id' in event:
                    self.send({'id': event['id'], 'error': {'code': -32601, 'message': 'Evaluation client refuses server request'}})
                    raise HarnessError('unexpected server request; no approval granted')
                # Preserve delivered usage even if suspend exhausted the wall budget.
                self.notify(event)
            elif 'id' in event:
                ident = event['id']
                if type(ident) is not int or ident not in self.pending or ident in self.replies:
                    raise HarnessError('unsolicited or duplicate RPC reply ID')
                self.replies[ident] = event
            else:
                raise HarnessError('unclassified RPC message')
            self.budget.check()  # Recheck on resume before accepting a reply/starting work.
        except BaseException as exc:
            error = type(exc).__name__
            raise
        finally:
            if self.observer:
                try:
                    self.observer(error=error, diagnostic_only=True)
                except Exception:
                    pass  # Best-effort diagnostics must not mask the primary failure.

    def wait_response(self, ident):
        # A silent pending reply is not a failure; the finite shared run budget
        # bounds live requests. Explicit transport/cleanup bounds remain separate.
        while ident not in self.replies:
            self.pump(1)
        self.budget.check()
        event = self.replies.pop(ident)
        self.pending.remove(ident)
        self.request_methods.pop(ident, None)
        if 'error' in event or 'result' not in event:
            raise HarnessError('RPC method failed')
        return event['result']

    def request(self, method, params):
        return self.wait_response(self.send_request(method, params))


class ProcessTransport:
    """Deadline-aware unbuffered pipes; stderr is drained but never persisted."""
    def __init__(self, argv, env, cwd):
        self.process = subprocess.Popen(argv, env=env, cwd=cwd, stdin=subprocess.PIPE,
                                        stdout=subprocess.PIPE, stderr=subprocess.PIPE, bufsize=0)
        self.selector = selectors.DefaultSelector()
        for stream, name in ((self.process.stdout, 'stdout'), (self.process.stderr, 'stderr')):
            os.set_blocking(stream.fileno(), False)
            self.selector.register(stream, selectors.EVENT_READ, name)
        os.set_blocking(self.process.stdin.fileno(), False)
        self.buffer = b''
        self.budget = None
        self.write_failed = False

    def read_ready(self, key):
        try:
            data = os.read(key.fileobj.fileno(), 65536)
        except BlockingIOError:
            return
        if not data:
            self.selector.unregister(key.fileobj)
            if key.data == 'stdout':
                raise HarnessError('App Server closed its transport')
        elif key.data == 'stdout':
            self.buffer += data
            if len(self.buffer) > 16_000_000:
                raise HarnessError('App Server event exceeded transport bound')

    def send(self, event, timeout=None):
        # An explicit timeout is reserved for cleanup, which must remain possible
        # after the run budget expires. A partial failed JSON line cannot be reused.
        if self.write_failed:
            raise HarnessError('App Server write failed; transport cannot be reused')
        budget = self.budget if timeout is None else None
        if budget:
            budget.check()
        deadline = continuous_time() + (min(30, budget.remaining()) if budget else (timeout if timeout is not None else 30))
        payload = memoryview(canonical(event) + b'\n')
        self.selector.register(self.process.stdin, selectors.EVENT_WRITE, 'stdin')
        try:
            while payload:
                if budget:
                    budget.check()
                remaining = deadline - continuous_time()
                if remaining <= 0:
                    raise HarnessError('App Server write deadline reached')
                for key, _ in self.selector.select(min(1, remaining)):
                    if continuous_time() >= deadline:
                        raise HarnessError('App Server write deadline reached')
                    if key.data == 'stdin':
                        try:
                            sent = os.write(key.fileobj.fileno(), payload[:65536])
                            payload = payload[sent:]
                        except BlockingIOError:
                            pass
                    else:
                        self.read_ready(key)
        except (OSError, HarnessError):
            self.write_failed = True
            raise
        finally:
            self.selector.unregister(self.process.stdin)

    def receive(self, timeout):
        deadline = continuous_time() + timeout
        while b'\n' not in self.buffer:
            remaining = deadline - continuous_time()
            if remaining <= 0:
                return None
            ready = self.selector.select(min(1, remaining))
            if not ready:
                return None
            for key, _ in ready:
                if continuous_time() >= deadline:
                    return None
                self.read_ready(key)
        line, self.buffer = self.buffer.split(b'\n', 1)
        try:
            return json.loads(line)
        except (UnicodeError, ValueError) as exc:
            raise HarnessError('invalid App Server JSON line') from exc

    def close(self, thread_id=None, turn_id=None, notify=None):
        # All phases have independent short bounds, including interrupt writes.
        # terminate/kill affect only this owned App Server child.
        try:
            if self.process.poll() is None:
                if thread_id and turn_id:
                    try:
                        self.send({'method': 'turn/interrupt', 'id': 2_000_000_000,
                                   'params': {'threadId': thread_id, 'turnId': turn_id}}, timeout=0.25)
                    except (OSError, HarnessError):
                        pass
                if notify:
                    try:
                        deadline = continuous_time() + 0.5
                        while continuous_time() < deadline:
                            event = self.receive(min(0.05, deadline - continuous_time()))
                            if event and 'method' in event and 'id' not in event:
                                try:
                                    notify(event)
                                except HarnessError:
                                    pass  # Retain final usage/overshoot even after the limit.
                    except (OSError, HarnessError):
                        pass
                self.process.stdin.close()
                try:
                    self.process.wait(timeout=0.25)
                except subprocess.TimeoutExpired:
                    self.process.terminate()
                    try:
                        self.process.wait(timeout=1)
                    except subprocess.TimeoutExpired:
                        self.process.kill()
                        self.process.wait(timeout=1)
        finally:
            self.selector.close()
            for stream in (self.process.stdin, self.process.stdout, self.process.stderr):
                stream.close()


@dataclass
class RunConfig:
    mode: str
    output: Path
    cycles: int
    compact_limit: int
    scope: str
    split: str
    args: argparse.Namespace


def parse_args(argv):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--mode', choices=('dry-run', 'replay', 'activation-check', 'pilot', 'release'), required=True)
    parser.add_argument('--output', type=Path, required=True)
    parser.add_argument('--allow-paid-inference', action='store_true')
    parser.add_argument('--max-seconds', type=int)
    parser.add_argument('--max-input-tokens', type=int)
    parser.add_argument('--model')
    parser.add_argument('--reasoning')
    parser.add_argument('--native-home', type=Path)
    parser.add_argument('--om-home', type=Path)
    parser.add_argument('--om-binary', type=Path)
    parser.add_argument('--plugin-root', type=Path)
    parser.add_argument('--replay-events', type=Path)
    parser.add_argument('--fixture', type=Path, default=FIXTURE)
    parser.add_argument('--frozen-split-hash')
    parser.add_argument('--split', choices=('pilot', 'release'))
    parser.add_argument('--cycles', type=int)
    parser.add_argument('--compact-limit', type=int, default=200000)
    parser.add_argument('--scope', choices=('total', 'body_after_prefix'), default='total')
    parser.add_argument('--force-compaction', action='store_true', help='Pilot diagnosis only, never release evidence')
    parser.add_argument('--codex', default='codex')
    args = parser.parse_args(argv)
    split = args.split or ('pilot' if args.mode == 'pilot' else 'release')
    cycles = args.cycles if args.cycles is not None else (1 if args.mode == 'pilot' else 50)
    if cycles <= 0 or args.compact_limit <= 0:
        raise HarnessError('cycles and threshold must be positive')
    if args.mode == 'release' and (cycles != 50 or args.compact_limit != 200000 or args.scope != 'total'
                                  or args.force_compaction or split != 'release'):
        raise HarnessError('release fixes both variants, 50 actual cycles, 200000 total, held-out release split')
    if args.mode == 'pilot' and split != 'pilot':
        raise HarnessError('pilot cannot score held-out release cases')
    if args.force_compaction and args.mode != 'pilot':
        raise HarnessError('forced compaction is only a diagnostic pilot option')
    if not args.output.is_absolute() or args.output.exists():
        raise HarnessError('output must be an absolute fresh directory')
    for value in (args.max_seconds, args.max_input_tokens):
        if value is not None and value <= 0:
            raise HarnessError('budget limits must be positive')
    if args.mode in ('pilot', 'release'):
        required = ('model', 'reasoning', 'native_home', 'om_home', 'om_binary', 'plugin_root',
                    'max_seconds', 'max_input_tokens', 'frozen_split_hash')
        if not args.allow_paid_inference or any(not getattr(args, key) for key in required):
            raise HarnessError('live mode requires explicit paid authorization, model/reasoning, isolated signed-in homes, '
                               'candidate paths, run-wide limits and frozen split hash')
        homes = [args.native_home.resolve(), args.om_home.resolve()]
        protected = {Path.home().joinpath('.codex').resolve(),
                     Path(os.environ.get('CODEX_HOME', Path.home() / '.codex')).resolve()}
        if homes[0] == homes[1] or any(h in protected for h in homes):
            raise HarnessError('dedicated native and OM homes must differ from active/global Codex homes')
        paths = [*homes, args.output.resolve()]
        if any(a in b.parents or b in a.parents for i, a in enumerate(paths) for b in paths[i + 1:]):
            raise HarnessError('homes and output must not overlap; auth must remain outside artifacts')
        for home in homes:
            validate_home(home)
        if not args.om_binary.is_file() or not os.access(args.om_binary, os.X_OK) or not args.plugin_root.is_dir():
            raise HarnessError('candidate binary/plugin path unavailable')
    if args.om_binary and not args.om_binary.is_file():
        raise HarnessError('candidate binary path unavailable')
    if args.plugin_root and not args.plugin_root.is_dir():
        raise HarnessError('candidate plugin path unavailable')
    if args.mode == 'activation-check':
        if (not args.om_binary or not args.plugin_root or not os.access(args.om_binary, os.X_OK)
                or args.native_home or args.om_home or args.allow_paid_inference or args.model or args.reasoning
                or args.max_input_tokens):
            raise HarnessError('activation-check requires candidate binary/plugin and no live homes or paid flag')
    if args.mode == 'replay' and not args.replay_events:
        raise HarnessError('replay requires --replay-events')
    return RunConfig(args.mode, args.output.resolve(), cycles, args.compact_limit, args.scope, split, args)


def run_local(argv, budget, env=None, cwd=None):
    budget.check()
    try:
        result = subprocess.run(argv, env=env, cwd=cwd, capture_output=True, text=True,
                                timeout=min(60, budget.remaining()), check=False)
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise HarnessError('local preflight command unavailable or timed out') from exc
    budget.check()
    if result.returncode:
        raise HarnessError('local preflight command failed: ' + Path(argv[0]).name)
    return result.stdout


def schema_preflight(codex, directory, budget, env):
    run_local([codex, 'app-server', 'generate-json-schema', '--out', str(directory), '--experimental'], budget, env)
    def schema(name):
        matches = list(directory.rglob(name + '.json'))
        if len(matches) != 1:
            raise HarnessError('required installed App Server schema unavailable: ' + name)
        return json.loads(matches[0].read_text())
    requests = canonical(schema('ClientRequest')).decode()
    notifications = canonical(schema('ServerNotification')).decode()
    for method in ('initialize', 'thread/start', 'turn/start', 'turn/interrupt', 'config/read', 'model/list', 'account/read',
                   'skills/list', 'hooks/list', 'config/value/write'):
        if '"' + method + '"' not in requests:
            raise HarnessError('installed host lacks required method: ' + method)
    for method in ('item/started', 'item/completed', 'turn/completed', 'thread/tokenUsage/updated', 'hook/completed'):
        if '"' + method + '"' not in notifications:
            raise HarnessError('installed host lacks required event: ' + method)
    item = schema('ItemCompletedNotification')
    definitions = item['definitions']['ThreadItem']['oneOf']
    compaction = next((v for v in definitions if v.get('properties', {}).get('type', {}).get('enum') == ['contextCompaction']), None)
    if not compaction or 'id' not in compaction.get('required', []) or 'threadId' not in item.get('required', []):
        raise HarnessError('stable completed compaction identity unsupported')
    usage_schema = schema('ThreadTokenUsageUpdatedNotification')
    usage = usage_schema['definitions']
    if (not {'inputTokens', 'outputTokens', 'totalTokens'} <= set(usage['TokenUsageBreakdown'].get('required', []))
            or not {'total', 'last'} <= set(usage['ThreadTokenUsage'].get('required', []))
            or 'turnId' not in usage_schema.get('required', [])):
        raise HarnessError('turn-bound cumulative input/context telemetry unsupported')
    config = schema('ConfigReadResponse')['definitions']['Config']['properties']
    for key in ('model_auto_compact_token_limit', 'model_auto_compact_token_limit_scope'):
        if key not in config:
            raise HarnessError('effective compaction/memory configuration cannot be verified')
    start = schema('ThreadStartParams')
    # Current generated schema uses workspace-write; earlier examples used a different spelling.
    if 'workspace-write' not in start['definitions']['SandboxMode']['enum']:
        raise HarnessError('workspace-write sandbox unsupported')
    if 'effort' not in schema('TurnStartParams')['properties']:
        raise HarnessError('explicit turn reasoning effort unsupported')
    return {'schema_sha256': tree_hash(directory), 'version': run_local([codex, '--version'], budget, env).strip(),
            'binary_sha256': digest(Path(shutil.which(codex) or codex).resolve().read_bytes())}



HOME_MARKER = '.om-eval-ownership.json'
LOGIN_FILES = {'auth.json', 'installation_id', '.credentials.json'}
LOGIN_DIRS = {'log', 'tmp'}
# Created by Codex/app-server or this runner in an otherwise unused evaluation
# home. Auth files are never included, hashed into results, copied or deleted.
GENERATED_HOME = re.compile(r'^(?:config\.toml|plugins|sessions|skills|memories|shell_snapshots|cache|log|tmp|\.tmp|'
                            r'goals_\d+\.sqlite(?:-shm|-wal)?|logs_\d+\.sqlite(?:-shm|-wal)?|'
                            r'memories_\d+\.sqlite(?:-shm|-wal)?|queue_\d+\.sqlite(?:-shm|-wal)?|'
                            r'state_\d+\.sqlite(?:-shm|-wal)?|models_cache\.json|version\.json|'
                            r'installation_id|thread-writer-locks|thread_history_\d+\.sqlite(?:-shm|-wal)?|\.sandbox_migration|\.personality_migration)$')


def check_home_path(path):
    for entry in [path, *(path.rglob('*') if path.is_dir() and not path.is_symlink() else [])]:
        if entry.is_symlink() or not (entry.is_file() or entry.is_dir()):
            raise HarnessError('evaluation home state contains a symlink or unsupported file type')


def path_hash(path):
    check_home_path(path)
    if path.is_file():
        return digest(path.read_bytes())
    return digest(canonical([[p.relative_to(path).as_posix(),
                              digest(p.read_bytes()) if p.is_file() else None]
                             for p in sorted(path.rglob('*'))]))


def write_home_state(home, state):
    # A failed/abandoned write leaves an unknown temporary file and prevents reuse.
    # Publish the in-progress record before deleting any previously owned state.
    fd, temporary = tempfile.mkstemp(prefix=HOME_MARKER + '.', dir=home)
    with os.fdopen(fd, 'w') as stream:
        stream.write(json.dumps(state, sort_keys=True, indent=2) + '\n')
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, home / HOME_MARKER)


def read_home_state(home):
    marker = home / HOME_MARKER
    if marker.is_symlink() or not marker.is_file():
        raise HarnessError('invalid evaluation home ownership record')
    try:
        state = json.loads(marker.read_text())
    except (ValueError, OSError) as exc:
        raise HarnessError('invalid evaluation home ownership record') from exc
    if (not isinstance(state, dict) or state.get('schema') != SCHEMA
            or state.get('home') != str(home.resolve())
            or not isinstance(state.get('baseline'), list)
            or any(not isinstance(name, str) or name not in LOGIN_FILES | LOGIN_DIRS for name in state['baseline'])
            or not isinstance(state.get('created'), dict)):
        raise HarnessError('invalid evaluation home ownership record')
    return state


def check_login_path(path):
    check_home_path(path)
    if (path.name in LOGIN_FILES and not path.is_file()) or (path.name in LOGIN_DIRS and not path.is_dir()):
        raise HarnessError('unexpected normal-login artifact type')


def validate_home(home):
    if not home.is_dir() or not os.access(home, os.W_OK):
        raise HarnessError('dedicated evaluation home must exist and be writable after normal sign-in')
    marker = home / HOME_MARKER
    if marker.exists() or marker.is_symlink():
        state = read_home_state(home)
        if state.get('status') != 'complete':
            raise HarnessError('incomplete evaluation home ownership; inspect the prior run before reuse')
        names = {p.name for p in home.iterdir()}
        if names != set(state['baseline']) | set(state['created']) | {HOME_MARKER}:
            raise HarnessError('evaluation home changed outside the prior run; refusing cleanup')
        if set(state['created']) & set(state['baseline']):
            raise HarnessError('invalid overlapping evaluation home ownership')
        for name in state['baseline']:
            check_login_path(home / name)
        for name, expected in state['created'].items():
            if name in LOGIN_FILES or not GENERATED_HOME.fullmatch(name) or path_hash(home / name) != expected:
                raise HarnessError('runner-owned evaluation state changed; refusing cleanup')
        return state
    names = {p.name for p in home.iterdir()}
    # These are authentication/installation artifacts of normal Codex sign-in.
    if names - LOGIN_FILES - LOGIN_DIRS:
        raise HarnessError('use unused signed-in evaluation homes, or homes with verified runner ownership')
    for path in home.iterdir():
        check_login_path(path)
    return {'baseline': sorted(names), 'created': {}}


def prepare_home(home, output=None):
    state = validate_home(home)
    write_home_state(home, {**state, 'schema': SCHEMA, 'home': str(home.resolve()),
                           'run': str(output.resolve()) if output is not None else None, 'status': 'in_progress'})
    for name in state['created']:
        target = home / name
        if target.is_dir():
            shutil.rmtree(target)
        else:
            target.unlink()
    return state['baseline']


def finish_home(home, baseline, output, release_exposures=None):
    state = read_home_state(home)
    if state.get('status') != 'in_progress' or state['baseline'] != baseline:
        raise HarnessError('evaluation home has no matching active ownership record')
    if state.get('run') is not None and state['run'] != str(output.resolve()):
        raise HarnessError('evaluation home run identity differs from prepared ownership')
    created = {}
    for path in home.iterdir():
        if path.name == HOME_MARKER:
            continue
        if path.name in baseline or path.name in LOGIN_FILES:
            check_login_path(path)
            continue  # Normal authentication refresh remains private in this home.
        if not GENERATED_HOME.fullmatch(path.name):
            raise HarnessError('unexpected evaluation home state; cannot certify safe reuse')
        if path.name in LOGIN_DIRS | {'cache'} and not path.is_dir():
            raise HarnessError('unexpected host-created directory type')
        created[path.name] = path_hash(path)
    if any(not (home / name).exists() for name in baseline):
        raise HarnessError('normal-login baseline disappeared during evaluation')
    baseline = sorted(set(baseline) | {p.name for p in home.iterdir() if p.name in LOGIN_FILES})
    write_home_state(home, {'schema': SCHEMA, 'home': str(home.resolve()), 'run': str(output.resolve()), 'status': 'complete',
                           'baseline': baseline, 'created': created, 'release_exposures': release_exposures or []})


def effective_comparison(config, variant, expected_market, trusted_hooks=None):
    value = copy.deepcopy(config)
    # Normalize only the integration this run staged, never a path supplied by
    # the received config. Preserve all residual marketplace/settings drift.
    plugins, markets = value.get('plugins'), value.get('marketplaces')
    if not isinstance(plugins, dict) or not isinstance(markets, dict):
        raise HarnessError('malformed evaluation plugin/marketplace registrations')
    if variant == 'native':
        if plugins or 'om-evaluation' in markets:
            raise HarnessError('native evaluation has unexpected plugin/marketplace registration')
    elif variant == 'om':
        plugin = plugins.get('observational-memory@om-evaluation')
        if (set(plugins) != {'observational-memory@om-evaluation'} or not isinstance(plugin, dict)
                or set(plugin) != {'enabled'} or plugin['enabled'] is not True):
            raise HarnessError('OM evaluation plugin registration differs from staged candidate')
        market = markets.get('om-evaluation')
        optional = {'ref', 'last_revision', 'last_updated', 'sparse_paths'}
        if (not isinstance(market, dict) or not {'source', 'source_type'} <= set(market)
                or set(market) - {'source', 'source_type'} - optional
                or market['source_type'] != 'local'
                or any(market.get(key) is not None for key in optional)):
            raise HarnessError('OM evaluation marketplace registration differs from staged candidate')
        source = market['source']
        try:
            matches = (isinstance(source, str) and Path(source).is_absolute()
                       and Path(source).resolve() == expected_market.resolve())
        except (OSError, ValueError, RuntimeError):
            matches = False
        if not matches:
            raise HarnessError('OM evaluation marketplace source differs from staged candidate')
        del plugins['observational-memory@om-evaluation']
        del markets['om-evaluation']
    else:
        raise HarnessError('unknown evaluation variant')
    if trusted_hooks:
        hooks = value.get('hooks', {})
        state = hooks.get('state') if isinstance(hooks, dict) else None
        if not isinstance(state, dict):
            raise HarnessError('evaluation hook trust configuration unavailable')
        for key, hash_value in trusted_hooks.items():
            if state.get(key) != {'trusted_hash': hash_value}:
                raise HarnessError('evaluation hook trust differs from reviewed definitions')
            del state[key]
        empty_hooks = {event: [] for event in ('Interrupt', 'PermissionRequest', 'PostCompact', 'PostToolUse',
            'PreCompact', 'PreToolUse', 'SessionEnd', 'SessionStart', 'Stop', 'SubagentStart', 'SubagentStop', 'UserPromptSubmit')}
        if hooks == {**empty_hooks, 'state': {}}:
            value['hooks'] = None  # Host defaults materialized solely by the reviewed trust write.
    # Writable roots are independently checked against each variant's exact
    # synthetic workspace/store; all other sandbox settings remain comparable.
    sandbox = value.get('sandbox_workspace_write')
    if isinstance(sandbox, dict):
        sandbox.pop('writable_roots', None)
    return value


HOOKS = {'sessionStart': ('SessionStart', 'session_start', 5, 2500),
         'userPromptSubmit': ('UserPromptSubmit', 'user_prompt_submit', 5, None),
         'postToolUse': ('PostToolUse', 'post_tool_use', 5, None),
         'stop': ('Stop', 'stop', 5, None), 'interrupt': ('Interrupt', 'interrupt', 3, None)}
PLUGIN_ID = 'observational-memory@om-evaluation'


def validate_hooks(response, variant, workspace, installed=None, expected=None):
    rows = response.get('data') if isinstance(response, dict) else None
    if (not isinstance(rows, list) or len(rows) != 1 or not isinstance(rows[0], dict) or rows[0].get('warnings') or rows[0].get('errors')
            or Path(rows[0].get('cwd', '')).resolve() != workspace.resolve()
            or not isinstance(rows[0].get('hooks'), list)):
        raise HarnessError('evaluation hook discovery failed or ambiguous')
    hooks = rows[0]['hooks']
    if variant == 'native':
        if hooks:
            raise HarnessError('native evaluation must have no hooks')
        return {}
    if variant != 'om' or installed is None or len(hooks) != len(HOOKS):
        raise HarnessError('evaluation requires exactly five reviewed OM hooks')
    hashes, events = {}, set()
    for hook in hooks:
        if not isinstance(hook, dict):
            raise HarnessError('malformed evaluation hook')
        event = hook.get('eventName')
        if event not in HOOKS or event in events:
            raise HarnessError('unexpected or duplicate evaluation hook')
        events.add(event)
        _, key, timeout, limit = HOOKS[event]
        required = {'key': f'{PLUGIN_ID}:hooks/hooks.json:{key}:0:0', 'handlerType': 'command',
                    'command': f'sh "{installed.resolve()}/scripts/run.sh" hook --client codex',
                    'async': False, 'matcher': None, 'timeoutSec': timeout, 'statusMessage': None,
                    'additionalContextLimit': limit, 'sourcePath': str(installed.resolve() / 'hooks/hooks.json'),
                    'source': 'plugin', 'pluginId': PLUGIN_ID, 'enabled': True, 'isManaged': False,
                    'trustStatus': 'trusted' if expected is not None else 'untrusted'}
        allowed = set(required) | {'eventName', 'displayOrder', 'currentHash'}
        if (set(hook) != allowed or any(type(hook[k]) is not type(v) or hook[k] != v for k, v in required.items())
                or type(hook['displayOrder']) is not int
                or not isinstance(hook['currentHash'], str) or not re.fullmatch(r'sha256:[0-9a-f]{64}', hook['currentHash'])):
            raise HarnessError('evaluation hook definition or trust differs from reviewed candidate')
        hashes[hook['key']] = hook['currentHash']
    if expected is not None and hashes != expected:
        raise HarnessError('evaluation hook hashes changed after review')
    return hashes


def activation_binding(config, candidate):
    installed, data = Path(candidate['installed']), Path(candidate['data'])
    expected = {'hooks': {event: [{'hooks': [{'type': 'command',
        'command': 'sh "${PLUGIN_ROOT}/scripts/run.sh" hook --client codex', 'timeout': timeout,
        **({'additionalContextLimit': limit} if limit is not None else {})}]}]
        for event, _, timeout, limit in HOOKS.values()}}
    if json.loads((installed / 'hooks/hooks.json').read_text()) != expected:
        raise HarnessError('staged hook manifest is not the reviewed five-hook contract')
    if tree_hash(installed) != candidate['staged_plugin_sha256']:
        raise HarnessError('installed plugin differs from staged reviewed bytes')
    return {'installed': str(installed.resolve()), 'data': str(data.resolve()),
            'plugin_sha256': candidate['staged_plugin_sha256'],
            'candidate_sha256': digest(config.args.om_binary.read_bytes()),
            'meter_sha256': digest((data / 'bin/om').read_bytes())}


def verify_activation_bytes(binding):
    data = Path(binding['data'])
    if (tree_hash(Path(binding['installed'])) != binding['plugin_sha256']
            or digest((data / 'bin/om-candidate').read_bytes()) != binding['candidate_sha256']
            or digest((data / 'bin/om').read_bytes()) != binding['meter_sha256']):
        raise HarnessError('reviewed activation plugin/candidate/meter bytes changed')


class ActivationSession:
    """Public App Server control only; no account read or live usage accounting."""
    def __init__(self, codex, workspace, env, options, budget):
        argv = [codex, 'app-server']
        for key, value in options.items():
            argv.extend(['-c', key + '=' + json.dumps(value)])
        self.transport = ProcessTransport(argv, env, workspace)
        self.transport.budget = budget
        self.events = []
        self.errors = []
        def receive(timeout):
            event = self.transport.receive(timeout)
            if event and 'error' in event:
                self.errors.append(event)
            return event
        self.rpc = RpcClient(receive, self.transport.send, budget, self.events.append)
        try:
            self.rpc.request('initialize', {'clientInfo': {'name': 'om_activation', 'version': '1'},
                                          'capabilities': {'experimentalApi': True}})
            self.transport.send({'method': 'initialized', 'params': {}})
        except BaseException:
            try:
                self.transport.close(notify=self.events.append)
            except BaseException:
                pass  # Initialization remains the primary failure; caller cannot certify ownership.
            raise

    def close(self):
        primary = sys.exc_info()[1]
        try:
            self.transport.close(notify=self.events.append)
        except BaseException:
            if primary is not None:
                raise primary
            raise


def trust_candidate(config, binding, budget, env, workspace, options):
    verify_activation_bytes(binding)
    session = ActivationSession(config.args.codex, workspace, env, options, budget)
    try:
        hashes = validate_hooks(session.rpc.request('hooks/list', {'cwds': [str(workspace)]}),
                                'om', workspace, Path(binding['installed']))
        verify_activation_bytes(binding)
        for key, value in hashes.items():
            session.rpc.request('config/value/write', {'keyPath': 'hooks.state.' + json.dumps(key) + '.trusted_hash',
                'value': value, 'mergeStrategy': 'upsert', 'filePath': str(Path(env['CODEX_HOME']) / 'config.toml')})
    finally:
        session.close()
    session = ActivationSession(config.args.codex, workspace, env, options, budget)
    try:
        verify_activation_bytes(binding)
        validate_hooks(session.rpc.request('hooks/list', {'cwds': [str(workspace)]}),
                       'om', workspace, Path(binding['installed']), hashes)
        return hashes
    finally:
        session.close()


def prepare_live_activation(config, candidate, budget, env):
    binding = activation_binding(config, candidate)
    workspace = (config.output / 'om/workspace').resolve()
    options = settings(config)
    options['sandbox_workspace_write.writable_roots'] = [str(workspace), binding['data']]
    binding['hooks'] = trust_candidate(config, binding, budget, env, workspace, options)
    return binding


def process_env(home):
    env = dict(os.environ, CODEX_HOME=str(home))
    # Only the requested home/normal sign-in supplies auth and provider selection.
    for key in list(env):
        if key.startswith(('OPENAI_', 'CODEX_')) and key != 'CODEX_HOME':
            env.pop(key, None)
    env.pop('OBSERVATIONAL_MEMORY_STORE', None)
    return env


def settings(config):
    return {'model': config.args.model, 'model_reasoning_effort': config.args.reasoning,
            'model_auto_compact_token_limit': config.compact_limit,
            'model_auto_compact_token_limit_scope': config.scope,
            'memories.use_memories': False, 'memories.generate_memories': False,
            'approval_policy': 'never', 'sandbox_mode': 'workspace-write',
            'sandbox_workspace_write.network_access': False, 'web_search': 'disabled',
            'features.multi_agent': False, 'features.apps': False,
            'sandbox_workspace_write.exclude_slash_tmp': True,
            'sandbox_workspace_write.exclude_tmpdir_env_var': True}


def verify_effective(value, config):
    effective = value.get('config', {})
    if effective.get('model_auto_compact_token_limit') != config.compact_limit or effective.get('model_auto_compact_token_limit_scope') != config.scope:
        raise HarnessError('effective native compaction threshold/scope mismatch')
    memory = effective.get('memories', {})
    if memory.get('use_memories') is not False or memory.get('generate_memories') is not False:
        raise HarnessError('effective native memory isolation unavailable')
    for key in ('compact_prompt', 'experimental_compact_prompt_file', 'model_context_window', 'developer_instructions', 'instructions'):
        if effective.get(key) is not None:
            raise HarnessError('evaluation requires native context window/compaction prompt and no extra instructions')
    if effective.get('model_provider') not in (None, 'openai') or effective.get('mcp_servers'):
        raise HarnessError('custom providers/MCP tools are outside matched evaluation scope')
    return effective


def stage_plugin(config, budget, env=None):
    """Use supplied candidate only. Version substitution is confined to staging."""
    args, output = config.args, config.output
    capabilities = json.loads(run_local([str(args.om_binary.resolve()), 'capabilities'], budget, env))
    version = capabilities.get('version', '')
    if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?', version):
        raise HarnessError('candidate must have explicit semantic version')
    run_local([str(args.om_binary.resolve()), 'check-compatibility', '--protocol', '2', '--client', 'codex'], budget, env)
    market = output / 'candidate-market'
    plugin = market / 'plugin'
    shutil.copytree(args.plugin_root.resolve(), plugin, ignore=shutil.ignore_patterns('__pycache__', '.test-runtime'))
    runtime = plugin / 'scripts/runtime.sh'
    original = runtime.read_text()
    staged, replacements = re.subn(r'^om_runtime_version=.*$', 'om_runtime_version=' + version, original, flags=re.MULTILINE)
    if replacements != 1:
        raise HarnessError('candidate plugin runtime version contract unavailable')
    runtime.write_text(staged)
    manifest = market / '.agents/plugins/marketplace.json'
    manifest.parent.mkdir(parents=True)
    write_json(manifest, {'name': 'om-evaluation', 'interface': {'displayName': 'OM evaluation'},
                         'plugins': [{'name': 'observational-memory', 'source': {'source': 'local', 'path': './plugin'},
                                      'policy': {'installation': 'AVAILABLE', 'authentication': 'ON_INSTALL'}}]})
    env = env if env is not None else process_env(args.om_home.resolve())
    run_local([args.codex, 'plugin', 'marketplace', 'add', str(market), '--json'], budget, env)
    run_local([args.codex, 'plugin', 'add', 'observational-memory@om-evaluation', '--json'], budget, env)
    installed = list((args.om_home / 'plugins/cache/om-evaluation/observational-memory').iterdir())
    if len(installed) != 1:
        raise HarnessError('ambiguous installed evaluation plugin')
    data = args.om_home.resolve() / 'plugins/data/observational-memory-om-evaluation'
    run_local(['sh', str(installed[0] / 'scripts/install.sh'), str(data), '--from', str(args.om_binary.resolve())], budget, env)
    if digest((data / 'bin/om').read_bytes()) != digest(args.om_binary.read_bytes()):
        raise HarnessError('installed OM binary does not match supplied candidate')
    return {'candidate_version': version, 'staged_plugin_sha256': tree_hash(plugin),
            'original_runtime_sha256': digest(original.encode()), 'staged_runtime_sha256': digest(staged.encode()),
            'version_substitution': original != staged, 'data': str(data), 'installed': str(installed[0])}


def meter_main(real, meter):
    """Standalone staged bridge; copied into the temporary executable wrapper."""
    import fcntl
    import json
    import os
    from pathlib import Path
    import subprocess
    import sys
    import time

    real, meter = Path(real), Path(meter)
    args = sys.argv[1:]
    command, session = None, None
    index = 0
    while index < len(args):
        arg = args[index]
        if arg in ('--store', '--session', '--client'):
            if index + 1 < len(args) and arg == '--session':
                session = args[index + 1]
            index += 2
        elif arg.startswith('--session='):
            session = arg.partition('=')[2]
            index += 1
        elif arg.startswith('-'):
            index += 1
        else:
            command = command or arg
            index += 1
    raw = sys.stdin.buffer.read() if command in ('hook', 'capture', 'apply') else b''
    try:
        payload = json.loads(raw) if raw else {}
    except ValueError:
        payload = {}
    if not isinstance(payload, dict):
        payload = {}
    hook = command == 'hook'
    if hook:
        session = payload.get('session_id')
    started = time.monotonic()
    entry = {'input_bytes': len(raw), 'hook': hook, 'session': session, 'command': command,
             'hook_event': payload.get('hook_event_name') if hook else None,
             'hook_source': payload.get('source') if hook else None,
             'turn_id': payload.get('turn_id') if hook else None,
             'deferrals': [], 'hook_unavailable': False, 'staged_command_substitution': False,
             'owned_prompt_restored': False}
    # Serialize the exact per-session Stop map and each append. No ledger state is
    # edited. A mapping exists only after this bridge emitted that owned reason.
    with (meter.parent / 'meter.lock').open('a+b') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        mappings_path = meter.parent / 'meter-stop-prompts.json'
        mappings = json.loads(mappings_path.read_text()) if mappings_path.exists() else {}
        mapping = mappings.get(session) if isinstance(session, str) else None
        delivered = raw
        if (hook and payload.get('hook_event_name') == 'UserPromptSubmit' and mapping
                and payload.get('prompt') == mapping['emitted']):
            payload['prompt'] = mapping['raw']
            delivered = json.dumps(payload, ensure_ascii=False).encode()
            entry['owned_prompt_restored'] = True
        result = subprocess.run([str(real)] + args, input=delivered, capture_output=True)
        output = result.stdout
        entry['prime_valid'] = (command == 'prime' and result.returncode == 0 and isinstance(session, str)
            and ('Session: ' + json.dumps(session)).encode() in output
            and ('Store: ' + json.dumps(str(real.parent.parent.resolve()))).encode() in output)
        try:
            response = json.loads(output)
        except ValueError:
            response = None  # Plaintext prime is a valid CLI response.
        if hook:
            entry['hook_unavailable'] = (result.returncode != 0 or not isinstance(response, dict)
                                         or bool(response.get('systemMessage')))
            if not entry['hook_unavailable']:
                quote = lambda value: "'" + str(value).replace("'", "'\"'\"'") + "'"
                old = quote(real) + ' --store '
                # Assignment applies only to this command. The native recognizer
                # already suppresses bare om with the exact store/session scope.
                new = 'PATH=' + quote(real.parent) + ' om --store '
                context = response.get('hookSpecificOutput', {})
                if isinstance(context, dict) and isinstance(context.get('additionalContext'), str):
                    text = context['additionalContext']
                    context['additionalContext'] = text.replace(old, new, 1)
                    entry['staged_command_substitution'] |= context['additionalContext'] != text
                if response.get('decision') == 'block' and isinstance(response.get('reason'), str):
                    original = response['reason']
                    response['reason'] = original.replace(old, new, 1)
                    entry['staged_command_substitution'] |= response['reason'] != original
                    if response['reason'] != original and isinstance(session, str):
                        mappings[session] = {'raw': original, 'emitted': response['reason']}
                        temporary = mappings_path.with_suffix('.tmp')
                        temporary.write_text(json.dumps(mappings, ensure_ascii=False))
                        temporary.replace(mappings_path)
                output = (json.dumps(response, ensure_ascii=False) + '\n').encode()
                if len(output) > 10000:
                    output = b'{"systemMessage":"Evaluation hook staging exceeded the context bound."}\n'
                    entry['hook_unavailable'] = True
        if command == 'apply' and result.returncode == 0:
            entry['deferrals'] = [d['source_id'] for d in payload.get('defer_sources', [])]
        entry.update({'output_bytes': len(output), 'stderr_bytes': len(result.stderr),
                      'candidate_input_bytes': len(delivered), 'candidate_output_bytes': len(result.stdout),
                      'seconds': time.monotonic() - started, 'exit_code': result.returncode})
        with meter.open('a') as log:
            log.write(json.dumps(entry) + '\n')
    sys.stdout.buffer.write(output)
    sys.stderr.buffer.write(result.stderr)
    sys.exit(result.returncode)


def install_meter(data):
    """Stage a metered command and reversible hook-guidance bridge.

    Candidate bytes stay unchanged. Only generated ledger-command guidance and
    exact owned Stop prompts are translated; ordinary evidence is not rewritten.
    """
    binary, real = data / 'bin/om', data / 'bin/om-candidate'
    binary.rename(real)
    meter = data / 'calls.jsonl'
    meter.write_text('')
    wrapper = ('#!' + sys.executable + '\n' + inspect.getsource(meter_main) +
               '\nmeter_main(' + repr(str(real)) + ', ' + repr(str(meter)) + ')\n')
    binary.write_text(wrapper)
    binary.chmod(0o755)
    return meter


def verify_intervention(events, calls, binding, thread_id, required, prime=False):
    """Combine native hook completions, actual metered calls, and native prime output."""
    runs = [e['params']['run'] for e in events if e.get('method') == 'hook/completed'
            and e.get('params', {}).get('threadId') == thread_id]
    for run in runs:
        if (run.get('eventName') not in HOOKS or run.get('source') != 'plugin'
                or run.get('sourcePath') != str(Path(binding['installed']) / 'hooks/hooks.json')
                or run.get('handlerType') != 'command' or run.get('executionMode') != 'sync'
                or run.get('status') not in (('completed', 'blocked') if run.get('eventName') == 'stop' else ('completed',))
                or any(e.get('kind') in ('error', 'warning') for e in run.get('entries', []))):
            raise HarnessError('OM activation has an unexpected or failed native hook')
    owned = [c for c in calls if c.get('session') == thread_id]
    if any((c.get('hook') and (c.get('exit_code') != 0 or c.get('hook_unavailable')))
           or (c.get('command') == 'prime' and not c.get('prime_valid')) for c in owned):
        raise HarnessError('OM activation has a failed metered hook or prime')
    if (not set(required) <= {r['eventName'] for r in runs}
            or not {HOOKS[event][0] for event in required} <= {c.get('hook_event') for c in owned if c.get('hook')}):
        raise HarnessError('OM activation missing required native/metered lifecycle evidence')
    if prime:
        commands = [e['params']['item'] for e in events if e.get('method') == 'item/completed'
                    and e.get('params', {}).get('threadId') == thread_id
                    and e['params'].get('item', {}).get('type') == 'commandExecution']
        contexts = [entry['text'] for run in runs if run['eventName'] == 'sessionStart'
                    for entry in run.get('entries', []) if entry.get('kind') == 'context']
        delivered = [m.group(1) for context in contexts for m in re.finditer(r'Ledger command: (.*?)\. Review', context)]
        valid = [item for item in commands if item.get('exitCode') == 0
                 and isinstance(item.get('aggregatedOutput'), str)
                 and ('Session: ' + json.dumps(thread_id)) in item.get('aggregatedOutput', '')
                 and any(action.get('command') == command + ' prime' for action in item.get('commandActions', [])
                         for command in delivered)]
        if not valid or not any(c.get('command') == 'prime' and c.get('prime_valid') for c in owned):
            raise HarnessError('OM activation missing successful hook-delivered prime command')
    return {'verified': True, 'native_events': sorted({r['eventName'] for r in runs}),
            'metered_events': sorted({c['hook_event'] for c in owned if c.get('hook')}), 'prime_verified': prime}



def activation_check(config, budget):
    """Unauthenticated local host proof. Stub usage never enters VariantRun/RunBudget.charge."""
    import http.server
    import threading
    root = config.output / 'activation'
    root.mkdir()
    workspace = root / 'workspace'; workspace.mkdir()
    home = root / 'om-home'; home.mkdir()
    native = root / 'native-home'; native.mkdir()
    temporary = root / 'tmp'; temporary.mkdir()
    env = {'PATH': os.environ['PATH'], 'HOME': str(root), 'CODEX_HOME': str(home),
           'TMPDIR': str(temporary), 'LANG': 'en_US.UTF-8'}
    local_budget = RunBudget(min(120, budget.remaining()), None)
    host = schema_preflight(config.args.codex, root / 'host-schema', local_budget, env)
    local_config = copy.copy(config)
    local_config.output = root
    local_config.args = copy.copy(config.args)
    local_config.args.om_home = home
    candidate = stage_plugin(local_config, local_budget, env)
    data = Path(candidate['data']); meter = install_meter(data)
    binding = activation_binding(local_config, candidate)
    verify_activation_bytes(binding)
    calls = lambda: [json.loads(line) for line in meter.read_text().splitlines()]
    requests, controls, evidence = [], {}, {}
    phase = {'name': 'native', 'sent': False}
    failures = []

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_POST(self):
            try:
                self.connection.settimeout(max(.1, min(10, local_budget.remaining())))
                length = int(self.headers.get('Content-Length', '0'))
                if length <= 0 or length > 4_000_000:
                    raise HarnessError('invalid local activation request size')
                request = json.loads(self.rfile.read(length))
                requests.append(request)
                number = len(requests)
                text = '\n'.join(part.get('text', '') for item in request.get('input', [])
                                 for part in item.get('content', []) if isinstance(part, dict))
                turn_meta = json.loads(request.get('client_metadata', {}).get('x-codex-turn-metadata', '{}'))
                compact = turn_meta.get('request_kind') == 'compaction'
                if phase['name'] in ('prime', 'bad-prime', 'interrupt') and not phase['sent'] and not compact:
                    phase['sent'] = True
                    if phase['name'] == 'interrupt':
                        command = 'python3 -c "import time; time.sleep(20)"'
                    else:
                        matches = re.findall(r'Ledger command: (.*?)\. Review', text)
                        if not matches:
                            raise HarnessError('local activation request lacks delivered SessionStart command')
                        command = matches[-1] + (' prime --invalid-activation-option' if phase['name'] == 'bad-prime' else ' prime')
                    item = {'id': f'ctc_{number}', 'type': 'custom_tool_call', 'call_id': f'call_{number}',
                            'name': 'exec', 'namespace': 'functions',
                            'input': 'text(await tools.exec_command(' + json.dumps({'cmd': command, 'max_output_tokens': 4000}) + '));'}
                else:
                    item = {'id': f'msg_{number}', 'type': 'message', 'role': 'assistant',
                            'content': [{'type': 'output_text', 'text': 'Local activation complete.', 'annotations': []}]}
                events = [{'type': 'response.output_item.done', 'output_index': 0, 'item': item},
                          {'type': 'response.completed', 'response': {'id': f'resp_{number}', 'status': 'completed',
                            'output': [item], 'usage': {'input_tokens': 10, 'output_tokens': 10, 'total_tokens': 20}}}]
                body = ''.join('event: ' + e['type'] + '\ndata: ' + json.dumps(e) + '\n\n' for e in events).encode()
                self.send_response(200); self.send_header('Content-Type', 'text/event-stream')
                self.send_header('Content-Length', str(len(body))); self.end_headers(); self.wfile.write(body)
            except Exception as exc:
                failures.append(str(exc))
                try:
                    self.send_error(500, 'local activation failed')
                except OSError:
                    pass

    server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    server.daemon_threads = True
    worker = threading.Thread(target=server.serve_forever, daemon=True); worker.start()
    options = settings(config)
    options.update({'model': 'gpt-6-astra', 'model_reasoning_effort': 'high', 'model_provider': 'om_activation',
                    'model_providers.om_activation.name': 'Local activation',
                    'model_providers.om_activation.base_url': f'http://127.0.0.1:{server.server_port}/v1',
                    'model_providers.om_activation.wire_api': 'responses',
                    'model_providers.om_activation.requires_openai_auth': False,
                    'sandbox_workspace_write.writable_roots': [str(workspace.resolve()), str(data.resolve())]})
    session = None

    def connect(which=home):
        selected = dict(options)
        if which == native:
            selected['sandbox_workspace_write.writable_roots'] = [str(workspace.resolve())]
        return ActivationSession(config.args.codex, workspace, {**env, 'CODEX_HOME': str(which)}, selected, local_budget)

    def until(predicate):
        while not predicate():
            local_budget.check()
            if failures:
                raise HarnessError('local activation provider failed: ' + failures[0])
            session.rpc.pump(.1)

    def run_turn(name, thread=None):
        phase.update(name=name, sent=False)
        if thread is None:
            thread = session.rpc.request('thread/start', {'model': 'gpt-6-astra', 'cwd': str(workspace),
                'approvalPolicy': 'never', 'sandbox': 'workspace-write'})['thread']['id']
        before, count = len(session.events), len(calls())
        turn = session.rpc.request('turn/start', {'threadId': thread,
            'input': [{'type': 'text', 'text': 'Check this isolated synthetic activation.'}]})['turn']['id']
        if name == 'interrupt':
            until(lambda: any(e.get('method') == 'item/started' and e.get('params', {}).get('turnId') == turn
                              and e['params'].get('item', {}).get('type') == 'commandExecution' for e in session.events[before:]))
            session.rpc.request('turn/interrupt', {'threadId': thread, 'turnId': turn})
        until(lambda: any(e.get('method') == 'turn/completed' and e.get('params', {}).get('turn', {}).get('id') == turn
                          for e in session.events[before:]))
        # Hook completion can trail interrupted-turn completion by a short bounded interval.
        if name == 'interrupt':
            until(lambda: any(e.get('method') == 'hook/completed' and e['params']['run']['eventName'] == 'interrupt'
                              for e in session.events[before:]))
        evidence[name] = session.events[before:]
        return thread, session.events[before:], calls()[count:]

    def refused(name, operation):
        try:
            operation()
        except HarnessError as exc:
            controls[name] = {'refused': True, 'reason': str(exc),
                'provenance': ('actual host turn and meter log' if name in ('untrusted', 'failed_prime', 'failed_hook')
                               else 'actual hooks/list after isolated definition/config mutation')}
        else:
            raise HarnessError('activation negative control unexpectedly accepted: ' + name)

    try:
        session = connect(native)
        native_listing = session.rpc.request('hooks/list', {'cwds': [str(workspace)]})
        validate_hooks(native_listing, 'native', workspace)
        native_config = session.rpc.request('config/read', {'cwd': str(workspace), 'includeLayers': True})['config']
        _, native_events, _ = run_turn('native')
        if any(e.get('method', '').startswith('hook/') for e in native_events):
            raise HarnessError('native activation control unexpectedly executed hooks')
        controls['native_zero_hooks'] = {'verified': True, 'provenance': 'actual native host turn and hooks/list'}
        native_config = session.rpc.request('config/read', {'cwd': str(workspace), 'includeLayers': True})['config']
        session.close(); session = connect()
        listing = session.rpc.request('hooks/list', {'cwds': [str(workspace)]})
        validate_hooks(listing, 'om', workspace, Path(binding['installed']))
        thread, events, metered = run_turn('untrusted')
        refused('untrusted', lambda: verify_intervention(events, metered, binding, thread, HOOKS, prime=True))
        if metered:
            raise HarnessError('untrusted activation control executed candidate')
        evidence['untrusted'] = events
        session.close(); session = None
        hashes = trust_candidate(local_config, binding, local_budget, env, workspace, options)
        binding['hooks'] = hashes
        session = connect()
        trusted = session.rpc.request('hooks/list', {'cwds': [str(workspace)]})
        validate_hooks(trusted, 'om', workspace, Path(binding['installed']), hashes)
        om_config = session.rpc.request('config/read', {'cwd': str(workspace), 'includeLayers': True})['config']
        write_json(root / 'effective-configs.json', {'native': native_config, 'om': om_config})
        if (effective_comparison(native_config, 'native', root / 'candidate-market')
                != effective_comparison(om_config, 'om', root / 'candidate-market', hashes)):
            raise HarnessError('local activation effective configurations differ beyond reviewed integration')
        thread, events, metered = run_turn('prime')
        startup = verify_intervention(events, metered, binding, thread,
                                      {'sessionStart', 'userPromptSubmit', 'postToolUse', 'stop'}, prime=True)
        evidence['startup'] = events
        _, events, metered = run_turn('interrupt', thread)
        interrupted = verify_intervention(events, metered, binding, thread, {'userPromptSubmit', 'interrupt'})
        evidence['interrupt'] = events
        thread, events, metered = run_turn('bad-prime')
        refused('failed_prime', lambda: verify_intervention(events, metered, binding, thread,
            {'sessionStart', 'userPromptSubmit', 'postToolUse', 'stop'}, prime=True))
        evidence['failed_prime'] = events
        real = data / 'bin/om-candidate'; mode = real.stat().st_mode
        try:
            real.chmod(0)
            thread, events, metered = run_turn('failed-hook')
            refused('failed_hook', lambda: verify_intervention(events, metered, binding, thread,
                {'sessionStart', 'userPromptSubmit', 'postToolUse', 'stop'}, prime=True))
            evidence['failed_hook'] = events
        finally:
            real.chmod(mode)
        session.close(); session = None
        hook_file = Path(binding['installed']) / 'hooks/hooks.json'; original = hook_file.read_bytes()
        for label in ('missing', 'modified'):
            try:
                altered = json.loads(original)
                if label == 'missing':
                    del altered['hooks']['Interrupt']
                else:
                    altered['hooks']['SessionStart'][0]['hooks'][0]['timeout'] = 6
                hook_file.write_text(json.dumps(altered))
                session = connect()
                changed = session.rpc.request('hooks/list', {'cwds': [str(workspace)]})
                refused(label, lambda: validate_hooks(changed, 'om', workspace, Path(binding['installed']), hashes))
            finally:
                if session:
                    session.close(); session = None
                hook_file.write_bytes(original)
        session = connect()
        key = next(iter(hashes))
        session.rpc.request('config/value/write', {'keyPath': 'hooks.state.' + json.dumps(key) + '.enabled',
            'value': False, 'mergeStrategy': 'upsert', 'filePath': str(home / 'config.toml')})
        disabled = session.rpc.request('hooks/list', {'cwds': [str(workspace)]})
        refused('disabled', lambda: validate_hooks(disabled, 'om', workspace, Path(binding['installed']), hashes))
        verify_activation_bytes(binding)
        result = {'verified': True, 'kind': 'unauthenticated-local-stub', 'inference_requests': 0,
                  'synthetic_provider_requests': len(requests), 'synthetic_usage_excluded': True,
                  'effective_config_equivalent': True,
                  'host': host, 'binding': binding, 'controls': controls, 'startup': startup, 'interrupt': interrupted,
                  'compaction_coverage': 'not exercised; separate native compaction canary and live campaign'}
        write_json(root / 'activation.json', result)
        return result
    finally:
        primary = sys.exc_info()[1]
        cleanup_errors = []
        def cleanup(operation):
            try:
                operation()
            except BaseException as exc:
                cleanup_errors.append(exc)
        if session:
            cleanup(lambda: write_json(root / 'rpc-errors.json', session.errors))
            cleanup(session.close)
        cleanup(server.shutdown)
        cleanup(server.server_close)
        cleanup(lambda: worker.join(timeout=1))
        cleanup(lambda: write_json(root / 'native-events.json', evidence))
        cleanup(lambda: write_json(root / 'stub-requests.json', requests))
        cleanup(lambda: write_json(root / 'meter-calls.json', calls()))
        cleanup(lambda: write_json(root / 'controls.json', controls))
        cleanup(budget.check)
        if cleanup_errors and primary is None:
            raise cleanup_errors[0]



def verify_deferrals(client):
    """Read existing public v2 evidence; never seed, acknowledge or score it."""
    if client.variant.name != 'om' or client.variant.fixture.split != 'release':
        return
    data = client.data
    calls = [json.loads(line) for line in (data / 'calls.jsonl').read_text().splitlines()]
    sources = sorted({source for call in calls if call.get('session') == client.variant.thread_id
                      for source in call.get('deferrals', [])})
    wanted = [client.variant.fixture.workload['files'][c['source']['path']]
              for c in client.variant.fixture.cases if c.get('require_deferral')]
    matched, audits = set(), []
    for source in sources:
        cursor, seen, units = None, set(), []
        while True:
            args = [str(data / 'bin/om'), '--store', str(data), '--session', client.variant.thread_id, 'recall', source]
            if cursor:
                args.extend(['--cursor', cursor])
            raw = run_local(args, client.variant.budget, process_env(client.config.args.om_home))
            if len(raw.encode()) > 12000:
                raise HarnessError('OM audit recall exceeded v2 envelope')
            page = json.loads(raw)['page']
            units.extend(item['evidence'] for item in page['items'] if item.get('evidence'))
            cursor = page.get('next_cursor')
            if not cursor:
                break
            if cursor in seen:
                raise HarnessError('OM recall cursor repeated')
            seen.add(cursor)
        units.sort(key=lambda u: u['start_byte'])
        text = ''.join(u['text'] for u in units)
        offset, intact = 0, bool(units)
        for unit in units:
            end = offset + len(unit['text'].encode())
            intact &= (unit.get('source_id') == source and unit.get('kind') == 'tool'
                       and unit.get('start_byte') == offset and unit.get('end_byte') == end
                       and unit.get('source_incomplete') is False)
            offset = end
        # Native PostToolUse retains its whole JSON envelope. Compare the decoded
        # response exactly; JSON escaping is storage framing, not lost source text.
        content, representation = text, 'raw-tool-source'
        try:
            envelope = json.loads(text)
        except ValueError:
            envelope = None
        if (isinstance(envelope, dict) and set(envelope) == {'tool', 'input', 'response'}
                and envelope['tool'] == 'Bash' and isinstance(envelope['input'], dict)
                and isinstance(envelope['response'], str)):
            content, representation = envelope['response'], 'native-PostToolUse-envelope'
        audited = any(u.get('deferral_reason') for u in units)
        for line in wanted:
            if intact and audited and content == line:
                matched.add(line)
        audits.append({'source_id': source, 'bytes': len(text.encode()), 'source_sha256': digest(text.encode()),
                       'representation': representation, 'source_integrity_verified': bool(intact),
                       'deferred_units': sum(u.get('review_state') == 'deferred' for u in units),
                       'deferral_audit_units': sum(bool(u.get('deferral_reason')) for u in units)})
    client.variant.deferral_evidence = {'verified': len(matched) == len(wanted) and bool(wanted),
                                       'sources': audits, 'provenance': 'successful session apply plus public paged recall; full predefined tool source retained'}
    if not client.variant.deferral_evidence['verified']:
        raise HarnessError('required deferred tool-log source was not actually retained and explicitly deferred')


class LiveVariant:
    def __init__(self, config, variant, workspace, env, writable, transport_factory=ProcessTransport):
        self.config, self.variant, self.workspace = config, variant, workspace.resolve()
        self.variant.workspace = self.workspace
        self.writable = [str(p.resolve()) for p in writable]
        self.data = writable[-1] if variant.name == 'om' else None
        options = settings(config)
        options['sandbox_workspace_write.writable_roots'] = self.writable
        argv = [config.args.codex, 'app-server']
        for key, value in options.items():
            argv.extend(['-c', key + '=' + json.dumps(value)])
        self.transport = transport_factory(argv, env, workspace)
        self.transport.budget = variant.budget
        self.active_turn = None
        self.fixture_index = 0
        self.activation = getattr(config, 'activation', None) if variant.name == 'om' else None
        self.activation_events, self.activation_evidence = [], None
        self.log = (config.output / (variant.name + '-events.jsonl')).open('w')
        self.variant.manual_compaction_requested = bool(config.args.force_compaction)
        self.rpc = RpcClient(self.transport.receive, self.send, variant.budget, self.notify)
        self.last_receipt_elapsed = None
        self.last_event_kind = None
        self.pending_commands = set()
        self.last_status_elapsed = None
        self.silence_warning_seen = False
        self.status_phase = 'preflight'
        self.status_error = None
        self.last_public_error = None
        self.rpc.observer = self.report_status

    def report_status(self, force=False, error=None, diagnostic_only=False):
        process = getattr(self.transport, 'process', None)
        exit_code = process.poll() if process is not None else None
        if exit_code is not None and not diagnostic_only and self.status_phase != 'closed':
            # Exit does not mean the pipe is empty. Retain final owned usage before
            # classifying failure, bounded even if another process inherited a pipe.
            deadline = continuous_time() + .25
            for _ in range(256):
                if continuous_time() >= deadline:
                    break
                try:
                    event = self.transport.receive(min(.01, deadline - continuous_time()))
                except (HarnessError, OSError):
                    break
                if event is None:
                    break
                if isinstance(event, dict) and 'method' in event and 'id' not in event:
                    try:
                        self.notify(event)
                    except HarnessError:
                        pass  # charge() updates delivered usage before raising.
        if error:
            self.status_error = error
        timing = self.variant.budget.timing()
        elapsed = timing.get('elapsed_seconds')
        age = (max(0., elapsed - self.last_receipt_elapsed)
               if elapsed is not None and self.last_receipt_elapsed is not None else None)
        pending = bool(self.rpc.pending or self.active_turn)
        silent = pending and age is not None and age >= 60
        self.silence_warning_seen |= silent
        due = (force or error or exit_code is not None or self.last_status_elapsed is None
               or elapsed is None or elapsed - self.last_status_elapsed >= 2)
        if due:
            status = {'schema': 1, 'variant': self.variant.name, 'timing': timing,
                      'thread_id': self.variant.thread_id, 'active_turn_id': self.active_turn,
                      'phase': self.status_phase if self.status_phase in ('closing', 'closed') else
                               ('failed' if self.status_error or exit_code is not None else self.status_phase),
                      'pending_rpc_methods': sorted(set(self.rpc.request_methods.values()))[:16],
                      'pending_command_count': len(self.pending_commands),
                      'last_event_kind': self.last_event_kind, 'last_event_receipt_elapsed_seconds': self.last_receipt_elapsed,
                      'last_event_age_seconds': age, 'silence_warning': silent,
                      'silence_warning_seen': self.silence_warning_seen,
                      'silence_policy': 'warning only; absent events do not establish model or host failure',
                      'host_exit_code': exit_code, 'error_class': self.status_error,
                      'last_public_error': self.last_public_error,
                      'max_seconds': self.variant.budget.max_seconds,
                      'remaining_seconds': max(0., self.variant.budget.max_seconds - elapsed)
                          if self.variant.budget.max_seconds is not None and elapsed is not None else None,
                      'max_input_tokens': self.variant.budget.max_input_tokens,
                      'reported_input_tokens': self.variant.usage.totals()['inputTokens']}
            path = self.config.output / (self.variant.name + '-status.json')
            temporary = path.with_suffix('.json.tmp')
            try:
                if len(canonical(status)) > 8192:
                    raise HarnessError('diagnostic status exceeded bound')
                write_json(temporary, status)
                os.replace(temporary, path)
                self.last_status_elapsed = elapsed
            except Exception:
                try:
                    temporary.unlink(missing_ok=True)
                except OSError:
                    pass  # Diagnostic I/O never replaces a run/cleanup failure.
        if exit_code is not None and not diagnostic_only and self.status_phase != 'closed':
            raise HarnessError('owned App Server exited: ' + str(exit_code))

    def send(self, event):
        if event.get('method') == 'thread/compact/start':
            self.variant.manual_compaction_requested = True
        self.transport.send(event)

    def notify(self, event):
        p = event.get('params', {})
        # Store only newly owned synthetic thread events, never account/config payloads.
        if self.variant.thread_id and p.get('threadId') == self.variant.thread_id:
            # A diagnostic timestamp failure must not discard delivered usage.
            self.last_receipt_elapsed = self.variant.budget.timing().get('elapsed_seconds')
            method = event.get('method')
            self.last_event_kind = method if method in (
                'thread/tokenUsage/updated', 'turn/started', 'turn/completed', 'item/started',
                'item/completed', 'item/agentMessage/delta', 'item/reasoning/textDelta',
                'item/reasoning/summaryTextDelta', 'item/commandExecution/outputDelta',
                'hook/started', 'hook/completed', 'error') else 'other-public-event'
            item = p.get('item', {})
            if item.get('type') == 'commandExecution':
                if event.get('method') == 'item/started':
                    self.pending_commands.add(item.get('id'))
                elif event.get('method') == 'item/completed':
                    self.pending_commands.discard(item.get('id'))
            if event.get('method') == 'error':
                self.last_public_error = 'retryable' if p.get('willRetry', False) else 'terminal'
                if not p.get('willRetry', False):
                    self.status_error = 'public-terminal-error'
            self.log.write(json.dumps(redact(event), ensure_ascii=False) + '\n')
            self.log.flush()
            if event.get('method') == 'hook/completed':
                if self.variant.name == 'native':
                    raise HarnessError('native evaluation unexpectedly executed a hook')
                if self.activation:
                    verify_intervention([event], [], self.activation, self.variant.thread_id, set())
            if (event.get('method') == 'hook/completed' or
                    (event.get('method') == 'item/completed' and p.get('item', {}).get('type') == 'commandExecution')):
                self.activation_events.append(event)
        self.variant.event(event)

    def preflight(self):
        self.rpc.request('initialize', {'clientInfo': {'name': 'om_eval', 'title': 'OM evaluation', 'version': '1'},
                                        'capabilities': {'experimentalApi': True}})
        self.transport.send({'method': 'initialized', 'params': {}})
        listing = self.rpc.request('hooks/list', {'cwds': [str(self.workspace)]})
        if self.variant.name == 'native':
            validate_hooks(listing, 'native', self.workspace)
        elif self.activation:
            verify_activation_bytes(self.activation)
            validate_hooks(listing, 'om', self.workspace, Path(self.activation['installed']), self.activation['hooks'])
        else:
            raise HarnessError('OM activation trust was not prepared')
        account = self.rpc.request('account/read', {'refreshToken': False}).get('account')
        if not account or account.get('type') != 'chatgpt':
            raise HarnessError('normal Codex ChatGPT sign-in required in each dedicated evaluation home')
        effective = verify_effective(self.rpc.request('config/read', {'cwd': str(self.workspace), 'includeLayers': True}), self.config)
        sandbox = effective.get('sandbox_workspace_write', {})
        if (sandbox.get('network_access') is not False or
                sorted(sandbox.get('writable_roots', [])) != sorted(self.writable)):
            raise HarnessError('effective workspace/store permissions differ from evaluation scope')
        skills = self.rpc.request('skills/list', {'cwds': [str(self.workspace)], 'forceReload': True})
        base_skills, om_skills = [], []
        for entry in skills.get('data', []):
            if entry.get('errors'):
                raise HarnessError('evaluation skill loading failed')
            for skill in entry.get('skills', []):
                if not skill.get('enabled'):
                    continue
                if skill.get('pluginId'):
                    if self.variant.name != 'om' or (skill.get('name') != 'observational-memory:observational-memory' or
                            skill.get('pluginId') != 'observational-memory@om-evaluation'):
                        raise HarnessError('unexpected plugin capability in evaluation')
                    om_skills.append(skill['name'])
                elif skill.get('scope') != 'system':
                    raise HarnessError('external user/repository skills defeat evaluation isolation')
                else:
                    base_skills.append([skill['name'], digest(Path(skill['path']).read_bytes())])
        if self.variant.name == 'om' and om_skills != ['observational-memory:observational-memory']:
            raise HarnessError('OM skill is not installed and enabled')
        models, cursor, seen = [], None, set()
        while True:
            page = self.rpc.request('model/list', {'cursor': cursor, 'limit': 100, 'includeHidden': False})
            models.extend(page.get('data', []))
            cursor = page.get('nextCursor')
            if not cursor:
                break
            if cursor in seen:
                raise HarnessError('model catalog pagination repeated')
            seen.add(cursor)
        selected = next((m for m in models if m.get('model') == self.config.args.model and not m.get('hidden')), None)
        if not selected or self.config.args.reasoning not in [e['reasoningEffort'] for e in selected.get('supportedReasoningEfforts', [])]:
            raise HarnessError('selected model/reasoning is not advertised by this signed-in host')
        self.variant.effective = {'config': redact(effective), 'model': self.config.args.model,
                                  'reasoning': self.config.args.reasoning, 'auth': 'dedicated Codex ChatGPT sign-in', 'base_skills': sorted(base_skills)}
        return effective

    def start(self):
        response = self.rpc.request('thread/start', {'model': self.config.args.model, 'cwd': str(self.workspace),
                    'approvalPolicy': 'never', 'sandbox': 'workspace-write',
                    'config': settings(self.config)})
        self.variant.start_thread(response.get('thread', {}).get('id'))
        if response.get('model') != self.config.args.model or response.get('reasoningEffort') != self.config.args.reasoning:
            raise HarnessError('thread model/reasoning differs from authorized selection')
        if response.get('approvalPolicy') != 'never' or Path(response.get('cwd', '')).resolve() != self.workspace:
            raise HarnessError('thread permissions/workspace differs from evaluation request')
        for source in response.get('instructionSources', []):
            p = Path(source).resolve()
            if p != self.workspace / 'AGENTS.md':
                raise HarnessError('unmatched external instruction source: evaluation isolation failed')
        self.variant.live_thread_id = self.variant.thread_id
        self.status_phase = 'idle'
        self.last_receipt_elapsed = self.variant.budget.elapsed()

    def turn(self, prompt, cycle=None, interrupt=False):
        event_offset = len(self.activation_events)
        call_offset = 0
        if self.activation:
            verify_activation_bytes(self.activation)
            validate_hooks(self.rpc.request('hooks/list', {'cwds': [str(self.workspace)]}), 'om', self.workspace,
                           Path(self.activation['installed']), self.activation['hooks'])
            call_offset = len((Path(self.activation['data']) / 'calls.jsonl').read_text().splitlines())
        before = len(self.variant.usage.observations)
        effects_before = action_evidence(self.variant.fixture, self.workspace, cycle, before=True) if cycle else None
        self.status_phase = 'waiting-for-turn-reply'
        response = self.rpc.request('turn/start', {'threadId': self.variant.thread_id,
                   'input': [{'type': 'text', 'text': prompt}], 'effort': self.config.args.reasoning})
        turn_id = response.get('turn', {}).get('id')
        if not turn_id:
            raise HarnessError('turn/start omitted turn identity')
        self.active_turn = turn_id
        self.status_phase = 'running-turn'
        self.variant.requested_turn_ids.add(turn_id)
        if cycle is not None:
            self.variant.begin_probe(cycle, turn_id)
        if interrupt:
            # Interrupt only after an actually charged response, while the analysis
            # may still be using tools. Never infer zero cost from an early cancel.
            while turn_id not in self.variant.turns:
                observed = self.variant.usage.observations
                baseline = observed[before - 1]['total']['inputTokens'] if before else 0
                if (len(observed) > before and observed[-1]['total']['inputTokens'] > baseline
                        and turn_id in self.variant.started_commands.values()):
                    break
                self.rpc.pump(0.1)
            if turn_id not in self.variant.turns:
                self.rpc.request('turn/interrupt', {'threadId': self.variant.thread_id, 'turnId': turn_id})
        while turn_id not in self.variant.turns:
            self.rpc.pump(1)
        completed = self.variant.turns[turn_id]
        if completed.get('status') not in (('interrupted', 'completed') if interrupt else ('completed',)):
            raise HarnessError('evaluation turn did not complete successfully')
        self.active_turn = None
        if interrupt:
            self.variant.interruptions.append({'turn_id': turn_id, 'status': completed.get('status')})
        # Notifications may race with turn completion. Drain boundedly for usage;
        # never start the next paid turn while usage is unknown.
        grace = continuous_time() + 2
        while continuous_time() < grace:
            try:
                self.variant.require_usage_since(before, turn_id)
                break
            except HarnessError:
                self.rpc.pump(0.1)
        self.variant.require_usage_since(before, turn_id)
        if self.activation:
            def current_events():
                return [e for e in self.activation_events[event_offset:] if e['params'].get('turnId') == turn_id]

            required = {'userPromptSubmit', 'interrupt' if completed.get('status') == 'interrupted' else 'stop'}
            if self.activation_evidence is None:
                required |= {'sessionStart', 'postToolUse'}
            # A later completed command still needs actual tool capture. An
            # interrupted command instead supplies the Interrupt lifecycle.
            if completed.get('status') == 'completed' and any(
                    e.get('method') == 'item/completed' and e['params'].get('item', {}).get('type') == 'commandExecution'
                    for e in current_events()):
                required.add('postToolUse')
            deadline = continuous_time() + min(3, self.variant.budget.remaining())
            while not required <= {e['params']['run']['eventName'] for e in current_events()
                                   if e.get('method') == 'hook/completed'} and continuous_time() < deadline:
                self.rpc.pump(.1)
            events = current_events()
            prime = any(e.get('method') == 'hook/completed' and e['params']['run']['eventName'] == 'sessionStart' for e in events)
            calls = [json.loads(line) for line in (Path(self.activation['data']) / 'calls.jsonl').read_text().splitlines()[call_offset:]]
            # SessionStart and ledger CLI commands omit turn_id; turn-scoped
            # metered hooks must belong to this turn, not a delayed prior call.
            calls = [c for c in calls if not c.get('hook') or c.get('hook_event') == 'SessionStart'
                     or c.get('turn_id') == turn_id]
            checked = verify_intervention(events, calls, self.activation, self.variant.thread_id, required, prime=prime)
            if self.activation_evidence is None:
                self.activation_evidence = checked
        if cycle is not None:
            values = list(self.variant.messages.get(turn_id, {}).values())
            try:
                answer = json.loads(values[-1]) if values else {}
            except ValueError:
                answer = {}  # Invalid JSON is a failed actual probe, not a retry with hints.
            self.variant.finish_probe(cycle, turn_id, answer,
                                      action_evidence(self.variant.fixture, self.workspace, cycle, prior=effects_before))
        self.status_phase = 'idle'
        self.report_status(force=True, diagnostic_only=True)
        return turn_id

    def recover(self):
        if len(self.variant.compactions) > len(self.variant.probes):
            cycle = len(self.variant.compactions)
            self.turn(self.variant.fixture.probe_prompt(cycle), cycle=cycle)

    def fixture_turn(self, event):
        prompt = event['text']
        if self.variant.name == 'om':
            prompt += '\n' + event.get('om_instruction', '')
            prompt += '\nUse the installed $observational-memory skill for this task. ' \
                      'Bulky tool logs may be explicitly deferred after identifying their source/span; ' \
                      'keep their searchable evidence and honest coverage.'
        self.turn(prompt, interrupt=event.get('interrupt', False))
        self.recover()
        self.fixture_index += 1

    def begin_fixture(self):
        self.start()
        self.fixture_turn(self.variant.fixture.workload['events'][0])

    def drive(self):
        if self.variant.thread_id is None:
            self.start()
        for event in self.variant.fixture.workload['events'][self.fixture_index:]:
            self.fixture_turn(event)
        verify_deferrals(self)
        index = 0
        while len(self.variant.compactions) < self.config.cycles:
            self.variant.budget.check()
            index += 1
            batch = self.variant.fixture.batch(index)
            self.variant.workload_batches.append({'index': index, 'sha256': digest(batch.encode()), 'bytes': len(batch.encode())})
            relative = f'batches/incident-{index:05d}.jsonl'
            (self.workspace / relative).write_text(batch)
            prompt = (f'Analyze new incident batch {index}. The complete new request observations follow, and are also in {relative}. '
                      'Compute per-region request counts, failures and p95 latency_ms; compare to your previous incident report. '
                      f'Write reports/incident-{index:05d}.json and explain which region and route need investigation using concrete request IDs. '
                      'Keep the accepted project constraints and completed steps. Do not rerun migrations. '
                      'Use local tools for arithmetic, then interpret the new evidence.\nObserved requests:\n' + batch)
            self.variant.workload_batches[-1].update({'prompt_sha256': digest(prompt.encode()),
                                                      'model_visible_bytes': len(prompt.encode())})
            self.turn(prompt)
            if self.config.args.force_compaction and len(self.variant.compactions) == len(self.variant.probes):
                before = len(self.variant.compactions)
                samples = len(self.variant.usage.observations)
                self.rpc.request('thread/compact/start', {'threadId': self.variant.thread_id})
                while len(self.variant.compactions) == before:
                    self.rpc.pump(1)
                while len(self.variant.usage.observations) <= samples:
                    self.rpc.pump(1)
                self.variant.require_usage_since(samples)
            self.recover()
        if len(self.variant.compactions) != self.config.cycles:
            raise HarnessError('more compactions than requested; recovery evidence is incomplete')

    def close(self):
        self.status_phase = 'closing'
        try:
            self.transport.close(self.variant.thread_id, self.active_turn or self.variant.active_turn, self.notify)
        except BaseException as exc:
            self.status_error = type(exc).__name__
            raise
        else:
            self.status_phase = 'closed'
        finally:
            try:
                self.report_status(force=True, diagnostic_only=True)
            except Exception:
                pass
            self.log.close()


def replay(config, fixture, variants, budget):
    payload = json.loads(config.args.replay_events.read_text())
    if not isinstance(payload, dict) or not isinstance(payload.get('events'), list):
        raise HarnessError('invalid replay envelope')
    if payload.get('schema') != SCHEMA:
        raise HarnessError('replay schema mismatch')
    fixture.check_hash(payload.get('split_hash'))
    if payload.get('compact_limit') != config.compact_limit or payload.get('scope') != config.scope:
        raise HarnessError('replay threshold/scope mismatch')
    for entry in payload.get('events', []):
        if not isinstance(entry, dict):
            raise HarnessError('invalid replay record')
        budget.check()
        name = entry.get('variant')
        if name not in variants:
            raise HarnessError('replay variant unavailable')
        variant = variants[name]
        kind = entry.get('kind', 'event')
        if kind == 'thread':
            variant.start_thread(entry['thread_id'])
        elif kind == 'segment':
            variant.start_segment(entry['id'], entry['baseline'], entry['reason'])
        elif kind == 'probe_start':
            variant.begin_probe(entry['cycle'], entry['turn_id'])
        elif kind == 'probe_result':
            variant.finish_probe(entry['cycle'], entry['turn_id'], entry['response'], entry.get('effects'))
        elif kind == 'event':
            variant.event(entry['event'])
        else:
            raise HarnessError('unknown replay record')


def live(config, fixture, variants, budget, metadata):
    args = config.args
    protected = {Path.home() / '.codex/config.toml', Path(os.environ.get('CODEX_HOME', Path.home() / '.codex')) / 'config.toml'}
    global_before = {str(p): digest(p.read_bytes()) if p.is_file() else None for p in protected}
    homes, baselines = {}, {}
    release_exposures = []
    errors = metadata['finalization_errors'] = []
    metadata['home_finalization'] = {}
    unsafe_homes = set()
    primary_error = None
    deferred_control_flow = []

    def finalize(stage, operation):
        try:
            operation()
            return {'status': 'complete'}
        except BaseException as exc:
            # Mandatory cleanup continues on interruption; re-raise its original
            # control-flow exception after all attempts unless a run error is active.
            if not isinstance(exc, Exception):
                deferred_control_flow.append(exc)
            error = {'stage': stage, 'error': str(redact(str(exc))) or type(exc).__name__}
            errors.append(error)
            return {'status': 'failed', 'error': error['error']}

    try:
        metadata['activation_canary'] = activation_check(config, budget)
        if not metadata['activation_canary'].get('verified'):
            raise HarnessError('local activation canary failed before paid work')
        homes = {name: getattr(args, name + '_home').resolve() for name in VARIANTS}
        states = {name: validate_home(home) for name, home in homes.items()}
        release_exposures = sorted({item for state in states.values() for item in state.get('release_exposures', [])})
        if config.mode == 'release' and fixture.rubric_hash in release_exposures:
            raise HarnessError('these release cases were already exposed in these homes; not fresh held-out evidence')
        envs = {name: process_env(home) for name, home in homes.items()}
        for name, home in homes.items():
            baselines[name] = prepare_home(home, config.output)
        metadata['host'] = schema_preflight(args.codex, config.output / 'host-schema', budget, envs['native'])
        metadata['candidate'] = stage_plugin(config, budget)
        data = Path(metadata['candidate']['data'])
        meter = install_meter(data)
        metadata['meter_sha256'] = digest((data / 'bin/om').read_bytes())
        unsafe_homes.add('om')  # Trust setup also owns App Server processes.
        config.activation = prepare_live_activation(config, metadata['candidate'], budget, envs['om'])
        unsafe_homes.discard('om')
        metadata['activation_binding'] = config.activation
        metadata['meter_staging'] = {'command': 'scoped PATH assignment to metered om',
            'owned_stop_prompt': 'exact per-session emitted-to-native mapping before hook delivery',
            'candidate_bytes_unchanged': True}
        clients = []
        try:
            for name in VARIANTS:
                workspace = (config.output / name / 'workspace').resolve()
                writable = [workspace] + ([data] if name == 'om' else [])
                unsafe_homes.add(name)  # Remains unsafe until shutdown returns successfully.
                client = LiveVariant(config, variants[name], workspace, envs[name], writable)
                clients.append(client)
                client.preflight()
            # Validate the exact staged integration before excluding its entries.
            compared = {name: effective_comparison(variants[name].effective['config'], name,
                        config.output / 'candidate-market', config.activation['hooks'] if name == 'om' else None) for name in VARIANTS}
            if compared['native'] != compared['om']:
                raise HarnessError('native and OM effective configurations are not equivalent')
            if variants['native'].effective['base_skills'] != variants['om'].effective['base_skills']:
                raise HarnessError('native and OM base skill/tool opportunities differ')
            if config.mode == 'release':
                release_exposures.append(fixture.rubric_hash)
            clients[1].begin_fixture()
            metadata['om_activation'] = clients[1].activation_evidence
            if not (metadata['om_activation'] or {}).get('verified'):
                raise HarnessError('OM activation failed before matched workload batches')
            for client in clients:
                client.drive()
        finally:
            for client in clients:
                if finalize(client.variant.name + ' shutdown', client.close)['status'] == 'complete':
                    unsafe_homes.discard(client.variant.name)

            def measurements():
                calls = [json.loads(line) for line in meter.read_text().splitlines()]
                write_json(config.output / 'om-calls.json', calls)
                metadata['om_measurements'] = {'calls': len(calls), 'input_bytes': sum(c['input_bytes'] for c in calls),
                                              'output_bytes': sum(c['output_bytes'] for c in calls),
                                              'hook_calls': sum(c['hook'] for c in calls),
                                              'provenance': 'temporary candidate binary wrapper; includes hook maintenance'}
                metadata['om_plugin_verified'] = (any(c['hook'] for c in calls)
                    and all(c['exit_code'] == 0 and not c.get('hook_unavailable') for c in calls if c['hook']))

            finalize('OM measurements', measurements)
            # Detect candidate tampering or accidental drift during a long run.
            def candidate_check():
                if digest(args.om_binary.read_bytes()) != metadata['om_binary_sha256']:
                    raise HarnessError('candidate binary changed during run')
                if tree_hash(args.plugin_root) != metadata['plugin_sha256']:
                    raise HarnessError('candidate plugin changed during run')

            finalize('candidate integrity', candidate_check)
    except BaseException as exc:
        primary_error = exc
        if metadata.get('om_activation'):
            metadata['om_activation']['verified'] = False
            metadata['om_activation']['error'] = 'live intervention run did not complete successfully'
        raise  # Preserve the original failure, including operator interruption.
    finally:
        for name, home in homes.items():
            if name in unsafe_homes:
                metadata['home_finalization'][name] = {'status': 'failed', 'error': 'host shutdown was not verified; ownership remains incomplete'}
            elif name in baselines:
                metadata['home_finalization'][name] = finalize(name + ' home',
                    lambda: finish_home(home, baselines[name], config.output, release_exposures))
            else:
                metadata['home_finalization'][name] = {'status': 'not_prepared'}

        def global_check():
            global_after = {str(p): digest(p.read_bytes()) if p.is_file() else None for p in protected}
            metadata['global_config_unchanged'] = global_before == global_after
            if global_before != global_after:
                raise HarnessError('global Codex configuration changed during evaluation')

        metadata['global_config_unchanged'] = None
        metadata['global_config_finalization'] = finalize('global configuration', global_check)
        if deferred_control_flow and primary_error is None:
            raise deferred_control_flow[0]
        if errors and primary_error is None:
            raise HarnessError('evaluation finalization failed: ' + '; '.join(e['stage'] + ': ' + e['error'] for e in errors))


def output_results(config, fixture, variants, budget, metadata, error):
    invalid_activation = config.mode in ('pilot', 'release') and not (metadata.get('om_activation') or {}).get('verified')
    if invalid_activation:
        error = error or 'OM activation not verified; intervention is invalid'
    comparison = quality(variants['native'], variants['om'], config.cycles)
    if invalid_activation:
        comparison['quality_pass'] = False
        comparison['comparison'] = 'not established'
        comparison['eligibility_reasons'].append('OM intervention validity was not established')
    counts_complete = all(v.scorecard()['actual_compactions'] == config.cycles and
                          v.scorecard()['scored_recoveries'] == config.cycles for v in variants.values())
    status = 'failed' if error else ('complete' if counts_complete else 'incomplete')
    reasons = list(comparison['eligibility_reasons'])
    if error:
        reasons.append(error)
    if config.mode != 'release':
        reasons.append(config.mode + ' is diagnostic only; not release evidence')
    if config.args.force_compaction:
        reasons.append('forced compaction pilot is ineligible')
    if config.mode == 'release' and not metadata.get('om_plugin_verified'):
        reasons.append('OM hook execution not verified')
    if config.mode == 'release':
        if not (variants['om'].deferral_evidence or {}).get('verified'):
            reasons.append('OM explicit deferred-log recovery setup not verified')
        for name, variant in variants.items():
            if not any(i['status'] == 'interrupted' for i in variant.interruptions):
                reasons.append(name + ': interruption scenario was not observed')
            if any(not c['configured_policy_compaction_verified'] for c in variant.compaction_evidence(config.compact_limit)):
                reasons.append(name + ': configured-policy compaction/context-reduction evidence incomplete or ambiguous')
    native_tokens = variants['native'].usage.totals()['inputTokens']
    om_tokens = variants['om'].usage.totals()['inputTokens']
    overhead = om_tokens - native_tokens if native_tokens is not None and om_tokens is not None else None
    timing = budget.timing()
    result = {'schema': SCHEMA, 'mode': config.mode, 'status': status,
              'release_pass': config.mode == 'release' and counts_complete and not reasons,
              'compaction_measurement_contract': 'configured-policy-and-observed-reduction-v2',
              'quality': comparison, 'eligibility_reasons': reasons, 'metadata': metadata,
              'variants': {name: v.result(config.compact_limit) for name, v in variants.items()},
              'budget': {'max_seconds': budget.max_seconds, 'max_input_tokens': budget.max_input_tokens,
                         'observed_input_tokens': budget.input_tokens if any(v.usage.observations for v in variants.values()) else None,
                         'input_overshoot': budget.overshoot, 'wall_seconds': timing.get('elapsed_seconds'),
                         'timing': timing,
                         'scope': 'whole run, both variants and all observed maintenance'},
              'input_token_overhead': overhead,
              'workload_exposure': {name: {'batches': len(v.workload_batches),
                                           'bytes': sum(b['bytes'] for b in v.workload_batches),
                                           'model_visible_bytes': sum(b['model_visible_bytes'] for b in v.workload_batches)}
                                    for name, v in variants.items()},
              'measurement_notes': ['Cumulative total input counts cached input already; reasoning output is not added to output.',
                                    'last contains request usage or a context-only estimate; neither exposes the exact automatic compaction trigger. Configured-policy lifecycle and context reduction are graded separately from the literal prior-request threshold comparison.',
                                    'Absent measurements are null. Overshoot between observations is possible.',
                                    'No subscription allowance, dollar conversion, or universal savings claim.',
                                    'Usage compares reaching the same compaction target; automatic thresholds may consume different lengths of the same generated workload sequence. See workload_exposure.']}
    write_json(config.output / 'results.json', result)
    lines = ['# Native compaction / OM evaluation', '', f'Status: **{status}**. Release PASS: **{result["release_pass"]}**.',
             '', f'Mode: {config.mode}; split: {fixture.split}; seed: {fixture.seed}; threshold: {config.compact_limit}/{config.scope}.',
             '', 'Compaction evidence grades configured-policy host compaction and observed context reduction. '
             'Exact trigger tokens and cause are unavailable; the prior request total is a separate observation.',
             '', '| Variant | Actual compactions | Scored recoveries | Critical failures | Noncritical accuracy | Input tokens |',
             '| --- | ---: | ---: | ---: | ---: | ---: |']
    for name, variant in variants.items():
        score = variant.scorecard()
        lines.append(f'| {name} | {score["actual_compactions"]} | {score["scored_recoveries"]} | '
                     f'{score["critical_failures"]} | {score["noncritical_accuracy"]} | {variant.usage.totals()["inputTokens"]} |')
    lines.extend(['', f'Input overhead (OM − native): {overhead}. Observed overshoot: {budget.overshoot}.',
                  '', 'Eligibility:', '', *['- ' + r for r in reasons], '', 'Frozen identities:', '',
                  *['- ' + k + ': `' + str(v) + '`' for k, v in metadata.items() if k.endswith('sha256')],
                  '', 'Cumulative input includes cached input; output and reasoning are reported separately. '
                  'Unavailable values are null. This is an observed ceiling, not a hard request cap. '
                  'A dry-run/replay/pilot never supplies release evidence. Equal quality is not superiority.', ''])
    (config.output / 'report.md').write_text('\n'.join(lines))
    return result


def main(argv=None):
    try:
        config = parse_args(argv)
        if config.mode == 'activation-check':
            config.output.mkdir(parents=True, exist_ok=False)
            try:
                result = activation_check(config, RunBudget(min(120, config.args.max_seconds or 120), None))
            except (HarnessError, OSError, ValueError, KeyError, TypeError) as exc:
                result = {'verified': False, 'kind': 'unauthenticated-local-stub', 'error': str(exc),
                          'inference_requests': 0, 'synthetic_usage_excluded': True}
            write_json(config.output / 'activation.json', result)
            print(str(config.output / 'activation.json'))
            return 0 if result['verified'] else 1
        budget = RunBudget(config.args.max_seconds, config.args.max_input_tokens)
        fixture = Fixture.load(config.args.fixture.resolve(), config.split)
        if config.args.frozen_split_hash:
            fixture.check_hash(config.args.frozen_split_hash)
    except (HarnessError, OSError, ValueError, KeyError, TypeError) as exc:
        print('evaluation refused: ' + str(redact(str(exc))), file=sys.stderr)
        return 2
    try:
        config.output.mkdir(parents=True, exist_ok=False)
    except OSError as exc:
        print('evaluation refused: cannot create fresh output directory', file=sys.stderr)
        return 2
    variants = {name: VariantRun(name, fixture, budget) for name in VARIANTS}
    metadata = {'fixture_sha256': fixture.fixture_hash, 'split_sha256': fixture.split_hash, 'rubric_sha256': fixture.rubric_hash,
                'runner_sha256': digest(Path(__file__).read_bytes()), 'seed': fixture.seed,
                'om_binary_sha256': None, 'plugin_sha256': None,
                'protocol': 2, 'host': None,
                'official_app_server_reference': 'https://learn.chatgpt.com/docs/app-server'}
    error = None
    try:
        metadata['om_binary_sha256'] = digest(config.args.om_binary.read_bytes()) if config.args.om_binary else None
        metadata['plugin_sha256'] = tree_hash(config.args.plugin_root) if config.args.plugin_root else None
        if config.args.om_binary:
            vcs = re.search(rb'vcs.revision=([0-9a-f]{40})', config.args.om_binary.read_bytes())
            metadata['candidate_git_revision'] = vcs.group(1).decode() if vcs else None
        for name in VARIANTS:
            fixture.prepare(config.output / name / 'workspace')
        metadata['initial_workspace_sha256'] = tree_hash(config.output / 'native/workspace')
        if metadata['initial_workspace_sha256'] != tree_hash(config.output / 'om/workspace'):
            raise HarnessError('initial workspaces differ')
        write_json(config.output / 'frozen-manifest.json', metadata)
        if config.mode == 'replay':
            replay(config, fixture, variants, budget)
        elif config.mode in ('pilot', 'release'):
            live(config, fixture, variants, budget, metadata)
        else:
            write_json(config.output / 'preview.json', {'schema': SCHEMA, 'split': fixture.split,
                       'split_sha256': fixture.split_hash, 'variants': list(VARIANTS),
                       'cycles': config.cycles, 'compact_limit': config.compact_limit, 'scope': config.scope,
                       'model_processes_started': 0, 'model': config.args.model, 'reasoning': config.args.reasoning,
                       'maximum_seconds': config.args.max_seconds, 'maximum_input_tokens': config.args.max_input_tokens,
                       'live_prerequisites': ['explicit model/reasoning', 'normal sign-in in separate unused homes',
                                              'candidate binary/plugin', 'run-wide wall/input limits',
                                              'frozen split hash', '--allow-paid-inference'],
                       'first_probe': fixture.probe_prompt(1),
                       'first_batch_sha256': digest(fixture.batch(1).encode())})
    except (HarnessError, OSError, ValueError, KeyError, TypeError) as exc:
        error = str(redact(str(exc)))
    except KeyboardInterrupt:
        error = 'operator interrupted evaluation'
    result = output_results(config, fixture, variants, budget, metadata, error)
    print(str(config.output / 'results.json'))
    return 1 if error else (1 if config.mode == 'release' and not result['release_pass'] else 0)


if __name__ == '__main__':
    sys.exit(main())
