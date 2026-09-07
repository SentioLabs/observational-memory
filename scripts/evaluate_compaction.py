#!/usr/bin/env python3
"""Opt-in, synthetic native/OM evaluation. Python 3.10+, standard library only.

No subprocess is reachable in dry-run/replay. Live modes own only new evaluation
threads. Scores are deterministic exact-match rubric checks, never model judges.
"""
from __future__ import annotations

import argparse
import copy
from dataclasses import dataclass, field
import hashlib
import json
import os
from pathlib import Path
import random
import re
import selectors
import shutil
import subprocess
import sys
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


@dataclass
class Fixture:
    split: str
    seed: int
    cases: list
    workload: dict
    fixture_hash: str
    split_hash: str

    @classmethod
    def load(cls, path, split):
        raw = path.read_bytes()
        data = json.loads(raw)
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
        # Only selected cases/workload leave validation; no held-out scoring in pilot.
        cases = [c for c in data['cases'] if c['split'] == split]
        workload = data['workloads'][split]
        if not cases or not any(c['critical'] for c in cases):
            raise HarnessError('empty rubric')
        frozen = {'seed': data['seed'], 'cases': cases, 'workload': workload,
                  'generator': 'service-incident-v1', 'schema': SCHEMA}
        return cls(split, data['seed'], cases, workload, digest(raw), digest(canonical(frozen)))

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
            rows.append({'request': f'{self.split}-{index:05d}-{n:04d}', 'region': region,
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
    clock: Callable = time.monotonic
    input_tokens: int = 0
    overshoot: int = 0
    started: float = field(init=False)

    def __post_init__(self):
        self.started = self.clock()

    def remaining(self):
        if self.max_seconds is None:
            return 60.0
        return max(0.0, self.max_seconds - (self.clock() - self.started))

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
        if type(baseline.get('inputTokens')) is not int:
            raise HarnessError('usage segment needs observed input baseline')
        self.segments.append(UsageSegment(name, baseline, reason))

    def observe(self, value, turn_id):
        amount = self.segments[-1].observe(value.get('total', {}))
        self.observations.append({'turn_id': turn_id, 'total': value.get('total'),
                                  'active_context': value.get('last'),
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
        self.usage = Usage()
        self.compactions, self.seen, self.probes = [], set(), {}
        self.turns, self.messages, self.commands = {}, {}, []
        self.effective = {}
        self.active_turn = None
        self.reroutes = []

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
        if method == 'model/rerouted':
            self.reroutes.append(redact(p))
            raise HarnessError('model rerouted away from authorized matched selection')
        if method == 'turn/started':
            self.active_turn = p.get('turn', {}).get('id')
        if method == 'thread/tokenUsage/updated':
            self.budget.charge(self.usage.observe(p.get('tokenUsage', {}), p.get('turnId')))
        if method == 'item/completed':
            item = p.get('item', {})
            if item.get('type') == 'contextCompaction':
                if not isinstance(item.get('id'), str) or not item['id']:
                    raise HarnessError('completed compaction has no stable item identity')
                key = (self.thread_id, item['id'])
                if key not in self.seen:
                    self.seen.add(key)
                    self.compactions.append({'thread_id': self.thread_id, 'item_id': item['id'],
                        'turn_id': p.get('turnId'), 'completed_at_ms': p.get('completedAtMs'),
                        'before_usage': copy.deepcopy(self.usage.observations[-1]) if self.usage.observations else None,
                        'after_usage': None})
            elif item.get('type') == 'agentMessage':
                self.messages.setdefault(p.get('turnId'), {})[item.get('id')] = item.get('text', '')
            elif item.get('type') == 'commandExecution':
                self.commands.append({'turn_id': p.get('turnId'), 'item': item})
        if method == 'turn/completed':
            turn = p.get('turn', {})
            self.turns[turn.get('id')] = turn
        if method == 'error' and not p.get('willRetry', False):
            raise HarnessError('host reported a non-retryable turn error')
        if method == 'thread/tokenUsage/updated':
            for cycle in self.compactions:
                if cycle['after_usage'] is None:
                    cycle['after_usage'] = copy.deepcopy(self.usage.observations[-1])

    def require_usage_since(self, samples):
        observations = self.usage.observations
        baseline = observations[samples - 1]['total']['inputTokens'] if samples else 0
        if len(observations) <= samples or observations[-1]['total']['inputTokens'] <= baseline:
            raise HarnessError('usage telemetry missing or stale for completed work; stopping before another request')

    def begin_probe(self, cycle, turn_id):
        if cycle != len(self.compactions) or cycle != len(self.probes) + 1:
            raise HarnessError('unprobed compaction: each cycle requires its own immediate recovery')
        if not turn_id or any(p['turn_id'] == turn_id for p in self.probes.values()):
            raise HarnessError('probe turn must have a distinct identity')
        self.probes[cycle] = {'turn_id': turn_id, 'scores': None}

    def finish_probe(self, cycle, turn_id, response):
        probe = self.probes.get(cycle)
        if not probe or probe['turn_id'] != turn_id or probe['scores'] is not None:
            raise HarnessError('probe response identity mismatch or duplicate score')
        # A compaction during the probe itself makes the interrupted cycle unscorable.
        if len(self.compactions) != cycle:
            raise HarnessError('compaction occurred before its predecessor recovery completed')
        answers = response.get('answers', {}) if isinstance(response, dict) else {}
        scores = []
        for case in self.fixture.cases:
            answer = answers.get(case['id'], {})
            text = answer.get('answer', '')
            stale = any(word.casefold() in str(text).casefold() for word in case.get('forbidden', []))
            repeated = answer.get('action') == case.get('forbidden_action') if 'forbidden_action' in case else False
            if case.get('forbidden_command'):
                repeated |= any(case['forbidden_command'] in c['item'].get('command', '') for c in self.commands)
            correct = (isinstance(text, str) and ('expected' not in case or text.strip() == case['expected'])
                       and ('expected_action' not in case or answer.get('action') == case['expected_action'])
                       and ('source' not in case or answer.get('source') == case['source'])
                       and not stale and not repeated)
            if case.get('expected_command'):
                correct &= any(c['turn_id'] == turn_id and case['expected_command'] in c['item'].get('command', '')
                               and c['item'].get('exitCode') == 0 for c in self.commands)
            scores.append({'case_id': case['id'], 'critical': case['critical'], 'correct': correct,
                           'stale': stale, 'repeated': repeated, 'exact_source': 'source' in case})
        probe['scores'] = scores
        probe['response'] = response

    def scorecard(self):
        scores = [s for p in self.probes.values() for s in (p['scores'] or [])]
        noncritical = [s for s in scores if not s['critical']]
        return {'actual_compactions': len(self.compactions),
                'scored_recoveries': sum(p['scores'] is not None for p in self.probes.values()),
                'critical_failures': sum(s['critical'] and not s['correct'] for s in scores),
                'stale_corrections': sum(s['stale'] for s in scores),
                'repeated_completed_actions': sum(s['repeated'] for s in scores),
                'noncritical_accuracy': (sum(s['correct'] for s in noncritical) / len(noncritical)
                                          if noncritical else None),
                'exact_source_checks': sum(s['exact_source'] for s in scores),
                'exact_source_failures': sum(s['exact_source'] and not s['correct'] for s in scores)}

    def result(self):
        return {'scorecard': self.scorecard(), 'compactions': self.compactions, 'probes': self.probes,
                'usage': self.usage.totals(), 'usage_segments': [vars(s) for s in self.usage.segments],
                'usage_observations': self.usage.observations, 'effective': self.effective, 'reroutes': self.reroutes,
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

    def send_request(self, method, params):
        self.budget.check()
        ident = self.next_id
        self.next_id += 1
        self.pending.add(ident)
        self.send({'method': method, 'id': ident, 'params': params})
        return ident

    def pump(self, timeout=30):
        self.budget.check()
        event = self.receive(min(timeout, self.budget.remaining()))
        if event is None:
            return
        if not isinstance(event, dict):
            raise HarnessError('invalid RPC message')
        if 'method' in event:
            if 'id' in event:
                self.send({'id': event['id'], 'error': {'code': -32601, 'message': 'Evaluation client refuses server request'}})
                raise HarnessError('unexpected server request; no approval granted')
            self.notify(event)
        elif 'id' in event:
            ident = event['id']
            if type(ident) is not int or ident not in self.pending or ident in self.replies:
                raise HarnessError('unsolicited or duplicate RPC reply ID')
            self.replies[ident] = event
        else:
            raise HarnessError('unclassified RPC message')

    def wait_response(self, ident, timeout=60):
        deadline = time.monotonic() + timeout
        while ident not in self.replies:
            if time.monotonic() >= deadline:
                raise HarnessError('RPC response deadline reached')
            self.pump(min(1, deadline - time.monotonic()))
        event = self.replies.pop(ident)
        self.pending.remove(ident)
        if 'error' in event or 'result' not in event:
            raise HarnessError('RPC method failed')
        return event['result']

    def request(self, method, params):
        return self.wait_response(self.send_request(method, params))


class ProcessTransport:
    """Bounded binary line reader; drains stderr without pipe deadlock."""
    def __init__(self, argv, env, cwd):
        self.process = subprocess.Popen(argv, env=env, cwd=cwd, stdin=subprocess.PIPE,
                                        stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.selector = selectors.DefaultSelector()
        self.selector.register(self.process.stdout, selectors.EVENT_READ, 'stdout')
        self.selector.register(self.process.stderr, selectors.EVENT_READ, 'stderr')
        self.buffer = b''

    def send(self, event):
        self.process.stdin.write(canonical(event) + b'\n')
        self.process.stdin.flush()

    def receive(self, timeout):
        deadline = time.monotonic() + timeout
        while b'\n' not in self.buffer:
            ready = self.selector.select(max(0, deadline - time.monotonic()))
            if not ready:
                return None
            for key, _ in ready:
                data = os.read(key.fileobj.fileno(), 65536)
                if not data:
                    self.selector.unregister(key.fileobj)
                    if key.data == 'stdout':
                        raise HarnessError('App Server closed its transport')
                elif key.data == 'stdout':
                    self.buffer += data
                    if len(self.buffer) > 16_000_000:
                        raise HarnessError('App Server event exceeded transport bound')
                # stderr may contain host paths/auth diagnostics: drain, never persist.
        line, self.buffer = self.buffer.split(b'\n', 1)
        try:
            return json.loads(line)
        except (UnicodeError, ValueError) as exc:
            raise HarnessError('invalid App Server JSON line') from exc

    def close(self, thread_id=None, turn_id=None, notify=None):
        if self.process.poll() is None:
            if thread_id and turn_id:
                try:
                    self.send({'method': 'turn/interrupt', 'id': 2_000_000_000,
                               'params': {'threadId': thread_id, 'turnId': turn_id}})
                    deadline = time.monotonic() + 3
                    while time.monotonic() < deadline:
                        event = self.receive(0.2)
                        if event and notify and 'method' in event and 'id' not in event:
                            try:
                                notify(event)
                            except HarnessError:
                                pass  # Still retain final usage/overshoot during shutdown.
                        if event and event.get('method') == 'turn/completed':
                            break
                except (OSError, HarnessError):
                    pass
            self.process.stdin.close()
            try:
                self.process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                self.process.terminate()
                try:
                    self.process.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    self.process.kill()
                    self.process.wait(timeout=3)
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
    parser.add_argument('--mode', choices=('dry-run', 'replay', 'pilot', 'release'), required=True)
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
            if not home.is_dir() or not os.access(home, os.W_OK):
                raise HarnessError('evaluation homes must already exist and be writable after normal Codex sign-in')
            if any((home / p).exists() for p in ('sessions', 'memories', 'skills', 'plugins', 'AGENTS.md')):
                raise HarnessError('evaluation homes must be dedicated and unused (no sessions/memory/skills/plugins)')
        if not args.om_binary.is_file() or not os.access(args.om_binary, os.X_OK) or not args.plugin_root.is_dir():
            raise HarnessError('candidate binary/plugin path unavailable')
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
    for method in ('initialize', 'thread/start', 'turn/start', 'turn/interrupt', 'config/read', 'model/list', 'account/read'):
        if '"' + method + '"' not in requests:
            raise HarnessError('installed host lacks required method: ' + method)
    for method in ('item/completed', 'turn/completed', 'thread/tokenUsage/updated'):
        if '"' + method + '"' not in notifications:
            raise HarnessError('installed host lacks required event: ' + method)
    item = schema('ItemCompletedNotification')
    definitions = item['definitions']['ThreadItem']['oneOf']
    compaction = next((v for v in definitions if v.get('properties', {}).get('type', {}).get('enum') == ['contextCompaction']), None)
    if not compaction or 'id' not in compaction.get('required', []) or 'threadId' not in item.get('required', []):
        raise HarnessError('stable completed compaction identity unsupported')
    usage = schema('ThreadTokenUsageUpdatedNotification')['definitions']
    if 'inputTokens' not in usage['TokenUsageBreakdown'].get('required', []) or 'total' not in usage['ThreadTokenUsage'].get('required', []):
        raise HarnessError('cumulative input telemetry unsupported')
    config = schema('ConfigReadResponse')['definitions']['Config']['properties']
    for key in ('model_auto_compact_token_limit', 'model_auto_compact_token_limit_scope', 'memories'):
        if key not in config:
            raise HarnessError('effective compaction/memory configuration cannot be verified')
    start = schema('ThreadStartParams')
    # Current generated schema uses workspace-write; earlier examples used a different spelling.
    if 'workspace-write' not in start['definitions']['SandboxMode']['enum']:
        raise HarnessError('workspace-write sandbox unsupported')
    if 'effort' not in schema('TurnStartParams')['properties']:
        raise HarnessError('explicit turn reasoning effort unsupported')
    return {'schema_sha256': tree_hash(directory), 'version': run_local([codex, '--version'], budget, env).strip()}


def process_env(home):
    env = dict(os.environ, CODEX_HOME=str(home))
    # Only the requested home/normal sign-in supplies auth and provider selection.
    for key in ('OPENAI_API_KEY', 'OPENAI_BASE_URL', 'CODEX_ACCESS_TOKEN', 'CODEX_REMOTE_TOKEN'):
        env.pop(key, None)
    return env


def settings(config):
    return {'model': config.args.model, 'model_reasoning_effort': config.args.reasoning,
            'model_auto_compact_token_limit': config.compact_limit,
            'model_auto_compact_token_limit_scope': config.scope,
            'memories.use_memories': False, 'memories.generate_memories': False,
            'approval_policy': 'never', 'sandbox_mode': 'workspace-write',
            'sandbox_workspace_write.network_access': False, 'web_search': 'disabled',
            'features.multi_agent': False}


def verify_effective(value, config):
    effective = value.get('config', {})
    if effective.get('model_auto_compact_token_limit') != config.compact_limit or effective.get('model_auto_compact_token_limit_scope') != config.scope:
        raise HarnessError('effective native compaction threshold/scope mismatch')
    memory = effective.get('memories', {})
    if memory.get('use_memories') is not False or memory.get('generate_memories') is not False:
        raise HarnessError('effective native memory isolation unavailable')
    for key in ('compact_prompt', 'experimental_compact_prompt_file', 'model_context_window', 'developer_instructions'):
        if effective.get(key) is not None:
            raise HarnessError('evaluation requires native context window/compaction prompt and no extra instructions')
    if effective.get('model_provider') not in (None, 'openai') or effective.get('mcp_servers'):
        raise HarnessError('custom providers/MCP tools are outside matched evaluation scope')
    return effective


def stage_plugin(config, budget):
    """Use supplied candidate only. Version substitution is confined to staging."""
    args, output = config.args, config.output
    capabilities = json.loads(run_local([str(args.om_binary.resolve()), 'capabilities'], budget))
    version = capabilities.get('version', '')
    if not re.fullmatch(r'[0-9]+\.[0-9]+\.[0-9]+(?:[-+][A-Za-z0-9.-]+)?', version):
        raise HarnessError('candidate must have explicit semantic version')
    run_local([str(args.om_binary.resolve()), 'check-compatibility', '--protocol', '2', '--client', 'codex'], budget)
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
    env = process_env(args.om_home.resolve())
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


def install_meter(data):
    """Meter every candidate invocation, including native hook maintenance.

    This transparent temporary wrapper never changes the CLI input/output contract.
    Its metadata (not text or auth) is append-only and not in the model workspace.
    """
    binary = data / 'bin/om'
    real = data / 'bin/om-candidate'
    binary.rename(real)
    meter = data / 'calls.jsonl'
    meter.write_text('')
    wrapper = '''#!{python}
import json, os, subprocess, sys, time
args = sys.argv[1:]
needs_input = any(a in ('hook', 'capture', 'apply') for a in args)
raw = sys.stdin.buffer.read() if needs_input else b''
started = time.monotonic()
r = subprocess.run([{real}] + args, input=raw, capture_output=True)
entry = {{'input_bytes': len(raw), 'output_bytes': len(r.stdout), 'stderr_bytes': len(r.stderr),
          'seconds': time.monotonic()-started, 'exit_code': r.returncode,
          'hook': 'hook' in args}}
fd = os.open({meter}, os.O_APPEND | os.O_WRONLY)
os.write(fd, (json.dumps(entry)+'\\n').encode()); os.close(fd)
sys.stdout.buffer.write(r.stdout); sys.stderr.buffer.write(r.stderr)
sys.exit(r.returncode)
'''.format(python=sys.executable, real=repr(str(real)), meter=repr(str(meter)))
    binary.write_text(wrapper)
    binary.chmod(0o755)
    return meter


class LiveVariant:
    def __init__(self, config, variant, workspace, env, writable):
        self.config, self.variant, self.workspace = config, variant, workspace
        options = settings(config)
        options['sandbox_workspace_write.writable_roots'] = [str(p) for p in writable]
        argv = [config.args.codex, 'app-server']
        for key, value in options.items():
            argv.extend(['-c', key + '=' + json.dumps(value)])
        self.transport = ProcessTransport(argv, env, workspace)
        self.active_turn = None
        self.log = (config.output / (variant.name + '-events.jsonl')).open('w')
        self.rpc = RpcClient(self.transport.receive, self.transport.send, variant.budget, self.notify)

    def notify(self, event):
        p = event.get('params', {})
        # Store only newly owned synthetic thread events, never account/config payloads.
        if self.variant.thread_id and p.get('threadId') == self.variant.thread_id:
            self.log.write(json.dumps(redact(event), ensure_ascii=False) + '\n')
            self.log.flush()
        self.variant.event(event)

    def preflight(self):
        self.rpc.request('initialize', {'clientInfo': {'name': 'om_eval', 'title': 'OM evaluation', 'version': '1'}})
        self.transport.send({'method': 'initialized', 'params': {}})
        account = self.rpc.request('account/read', {'refreshToken': False}).get('account')
        if not account or account.get('type') != 'chatgpt':
            raise HarnessError('normal Codex ChatGPT sign-in required in each dedicated evaluation home')
        effective = verify_effective(self.rpc.request('config/read', {'cwd': str(self.workspace), 'includeLayers': True}), self.config)
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
                                  'reasoning': self.config.args.reasoning, 'auth': 'dedicated Codex ChatGPT sign-in'}
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

    def turn(self, prompt, cycle=None, interrupt=False):
        before = len(self.variant.usage.observations)
        response = self.rpc.request('turn/start', {'threadId': self.variant.thread_id,
                   'input': [{'type': 'text', 'text': prompt}], 'effort': self.config.args.reasoning})
        turn_id = response.get('turn', {}).get('id')
        if not turn_id:
            raise HarnessError('turn/start omitted turn identity')
        self.active_turn = turn_id
        if cycle is not None:
            self.variant.begin_probe(cycle, turn_id)
        if interrupt:
            # Interrupt only after an actually charged response, while the analysis
            # may still be using tools. Never infer zero cost from an early cancel.
            while turn_id not in self.variant.turns:
                observed = self.variant.usage.observations
                baseline = observed[before - 1]['total']['inputTokens'] if before else 0
                if len(observed) > before and observed[-1]['total']['inputTokens'] > baseline:
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
        # Notifications may race with turn completion. Drain boundedly for usage;
        # never start the next paid turn while usage is unknown.
        grace = time.monotonic() + 2
        while time.monotonic() < grace:
            try:
                self.variant.require_usage_since(before)
                break
            except HarnessError:
                self.rpc.pump(0.1)
        self.variant.require_usage_since(before)
        if cycle is not None:
            values = list(self.variant.messages.get(turn_id, {}).values())
            try:
                answer = json.loads(values[-1]) if values else {}
            except ValueError:
                answer = {}  # Invalid JSON is a failed actual probe, not a retry with hints.
            self.variant.finish_probe(cycle, turn_id, answer)
        return turn_id

    def recover(self):
        if len(self.variant.compactions) > len(self.variant.probes):
            cycle = len(self.variant.compactions)
            self.turn(self.variant.fixture.probe_prompt(cycle), cycle=cycle)

    def drive(self):
        self.start()
        for event in self.variant.fixture.workload['events']:
            prompt = event['text']
            if self.variant.name == 'om':
                prompt += '\nUse the installed $observational-memory skill for this task. ' \
                          'Bulky tool logs may be explicitly deferred after identifying their source/span; ' \
                          'keep their searchable evidence and honest coverage.'
            self.turn(prompt, interrupt=event.get('interrupt', False))
            self.recover()
        index = 0
        while len(self.variant.compactions) < self.config.cycles:
            self.variant.budget.check()
            index += 1
            batch = self.variant.fixture.batch(index)
            relative = f'batches/incident-{index:05d}.jsonl'
            (self.workspace / relative).write_text(batch)
            self.turn(f'Analyze new incident batch {index}. Read all of {relative} with sufficient tool output allowance. '
                      f'Compute per-region request counts, failures and p95 latency_ms; compare to your previous incident report. '
                      f'Write reports/incident-{index:05d}.json and record which region needs investigation. '
                      'Keep the accepted project constraints and completed steps. Do not rerun migrations. '
                      'Use local tools; these are fresh observations, not a replay of earlier incidents.')
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
        self.transport.close(self.variant.thread_id, self.active_turn or self.variant.active_turn, self.notify)
        self.log.close()


def replay(config, fixture, variants, budget):
    payload = json.loads(config.args.replay_events.read_text())
    if payload.get('schema') != SCHEMA:
        raise HarnessError('replay schema mismatch')
    fixture.check_hash(payload.get('split_hash'))
    if payload.get('compact_limit') != config.compact_limit or payload.get('scope') != config.scope:
        raise HarnessError('replay threshold/scope mismatch')
    for entry in payload.get('events', []):
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
            variant.finish_probe(entry['cycle'], entry['turn_id'], entry['response'])
        elif kind == 'event':
            variant.event(entry['event'])
        else:
            raise HarnessError('unknown replay record')


def live(config, fixture, variants, budget, metadata):
    args = config.args
    homes = {name: getattr(args, name + '_home').resolve() for name in VARIANTS}
    envs = {name: process_env(home) for name, home in homes.items()}
    metadata['host'] = schema_preflight(args.codex, config.output / 'host-schema', budget, envs['native'])
    metadata['candidate'] = stage_plugin(config, budget)
    data = Path(metadata['candidate']['data'])
    meter = install_meter(data)
    metadata['meter_sha256'] = digest((data / 'bin/om').read_bytes())
    clients = []
    try:
        for name in VARIANTS:
            workspace = (config.output / name / 'workspace').resolve()
            writable = [workspace] + ([data] if name == 'om' else [])
            client = LiveVariant(config, variants[name], workspace, envs[name], writable)
            clients.append(client)
            client.preflight()
        # Match all effective configuration except the intentionally installed plugin,
        # dedicated paths, and its necessary writable store.
        def comparable(value):
            value = copy.deepcopy(value)
            for key in ('plugins', 'plugin_marketplaces', 'sandbox_workspace_write', 'projects'):
                value.pop(key, None)
            return value
        if comparable(variants['native'].effective['config']) != comparable(variants['om'].effective['config']):
            raise HarnessError('native and OM effective configurations are not equivalent')
        for client in clients:
            client.drive()
    finally:
        for client in clients:
            client.close()
        calls = [json.loads(line) for line in meter.read_text().splitlines()]
        write_json(config.output / 'om-calls.json', calls)
        metadata['om_measurements'] = {'calls': len(calls), 'input_bytes': sum(c['input_bytes'] for c in calls),
                                      'output_bytes': sum(c['output_bytes'] for c in calls),
                                      'hook_calls': sum(c['hook'] for c in calls),
                                      'provenance': 'temporary candidate binary wrapper; includes hook maintenance'}
        if not any(c['hook'] for c in calls):
            metadata['om_plugin_verified'] = False
        else:
            metadata['om_plugin_verified'] = all(c['exit_code'] == 0 for c in calls if c['hook'])
        # Detect candidate tampering or accidental drift during a long run.
        if digest(args.om_binary.read_bytes()) != metadata['om_binary_sha256']:
            raise HarnessError('candidate binary changed during run')
        if tree_hash(args.plugin_root) != metadata['plugin_sha256']:
            raise HarnessError('candidate plugin changed during run')


def output_results(config, fixture, variants, budget, metadata, error):
    comparison = quality(variants['native'], variants['om'], config.cycles)
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
        for name, variant in variants.items():
            if any(c['before_usage'] is None or c['after_usage'] is None for c in variant.compactions):
                reasons.append(name + ': compaction usage observations incomplete')
    native_tokens = variants['native'].usage.totals()['inputTokens']
    om_tokens = variants['om'].usage.totals()['inputTokens']
    overhead = om_tokens - native_tokens if native_tokens is not None and om_tokens is not None else None
    result = {'schema': SCHEMA, 'mode': config.mode, 'status': status,
              'release_pass': config.mode == 'release' and counts_complete and not reasons,
              'quality': comparison, 'eligibility_reasons': reasons, 'metadata': metadata,
              'variants': {name: v.result() for name, v in variants.items()},
              'budget': {'max_seconds': budget.max_seconds, 'max_input_tokens': budget.max_input_tokens,
                         'observed_input_tokens': budget.input_tokens if any(v.usage.observations for v in variants.values()) else None,
                         'input_overshoot': budget.overshoot, 'wall_seconds': budget.clock() - budget.started,
                         'scope': 'whole run, both variants and all observed maintenance'},
              'input_token_overhead': overhead,
              'measurement_notes': ['Cumulative total input counts cached input already; reasoning output is not added to output.',
                                    'last is a host active-context observation, not additional billable usage.',
                                    'Absent measurements are null. Overshoot between observations is possible.',
                                    'No subscription allowance, dollar conversion, or universal savings claim.']}
    write_json(config.output / 'results.json', result)
    lines = ['# Native compaction / OM evaluation', '', f'Status: **{status}**. Release PASS: **{result["release_pass"]}**.',
             '', f'Mode: {config.mode}; split: {fixture.split}; seed: {fixture.seed}; threshold: {config.compact_limit}/{config.scope}.',
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
        fixture = Fixture.load(config.args.fixture.resolve(), config.split)
        if config.args.frozen_split_hash:
            fixture.check_hash(config.args.frozen_split_hash)
    except (HarnessError, OSError, ValueError) as exc:
        print('evaluation refused: ' + str(redact(str(exc))), file=sys.stderr)
        return 2
    config.output.mkdir(parents=True, exist_ok=False)
    budget = RunBudget(config.args.max_seconds, config.args.max_input_tokens)
    variants = {name: VariantRun(name, fixture, budget) for name in VARIANTS}
    metadata = {'fixture_sha256': fixture.fixture_hash, 'split_sha256': fixture.split_hash,
                'runner_sha256': digest(Path(__file__).read_bytes()), 'seed': fixture.seed,
                'om_binary_sha256': digest(config.args.om_binary.read_bytes()) if config.args.om_binary else None,
                'plugin_sha256': tree_hash(config.args.plugin_root) if config.args.plugin_root else None,
                'protocol': 2, 'host': None,
                'official_app_server_reference': 'https://learn.chatgpt.com/docs/app-server'}
    error = None
    try:
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
    except (HarnessError, OSError, ValueError, KeyError) as exc:
        error = str(redact(str(exc)))
    except KeyboardInterrupt:
        error = 'operator interrupted evaluation'
    result = output_results(config, fixture, variants, budget, metadata, error)
    print(str(config.output / 'results.json'))
    return 1 if error else (1 if config.mode == 'release' and not result['release_pass'] else 0)


if __name__ == '__main__':
    sys.exit(main())
