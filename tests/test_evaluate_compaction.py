"""Behavior checks for the opt-in harness; all events are local synthetic data."""
import copy
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('evaluate_compaction', ROOT / 'scripts/evaluate_compaction.py')
evalmod = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = evalmod
spec.loader.exec_module(evalmod)


def usage(thread, amount, turn='turn', **fields):
    return {'method': 'thread/tokenUsage/updated', 'params': {'threadId': thread, 'turnId': turn,
            'tokenUsage': {'total': {'inputTokens': amount, **fields}, 'last': {'totalTokens': amount}}}}


def compact(thread, ident, kind='contextCompaction'):
    return {'method': 'item/completed', 'params': {'threadId': thread, 'turnId': 'turn',
            'item': {'id': ident, 'type': kind}}}


def fake_hook_listing(workspace, installed, trusted=False):
    hooks = []
    for event, key, timeout, limit in (
        ('sessionStart', 'session_start', 5, 2500), ('userPromptSubmit', 'user_prompt_submit', 5, None),
        ('postToolUse', 'post_tool_use', 5, None), ('stop', 'stop', 5, None), ('interrupt', 'interrupt', 3, None)):
        hooks.append({'key': f'observational-memory@om-evaluation:hooks/hooks.json:{key}:0:0',
            'eventName': event, 'handlerType': 'command',
            'command': f'sh "{installed.resolve()}/scripts/run.sh" hook --client codex',
            'async': False, 'matcher': None, 'timeoutSec': timeout, 'statusMessage': None,
            'additionalContextLimit': limit, 'sourcePath': str(installed.resolve() / 'hooks/hooks.json'),
            'source': 'plugin', 'pluginId': 'observational-memory@om-evaluation', 'displayOrder': len(hooks),
            'enabled': True, 'isManaged': False, 'currentHash': 'sha256:' + str(len(hooks)) * 64,
            'trustStatus': 'trusted' if trusted else 'untrusted'})
    return {'data': [{'cwd': str(workspace), 'hooks': hooks, 'warnings': [], 'errors': []}]}


class HarnessTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        self.fixture = evalmod.Fixture.load(ROOT / 'eval/fixtures/continuity.json', 'release')

    def budget(self, limit=1000, seconds=100, clock=lambda: 0):
        return evalmod.RunBudget(seconds, limit, clock)

    def variant(self, name='native', budget=None):
        v = evalmod.VariantRun(name, self.fixture, budget or self.budget())
        v.start_thread(name + '-thread')
        return v

    def test_count_only_distinct_completed_compactions_in_owned_thread(self):
        v = self.variant()
        for event in [compact('native-thread', 'one'), compact('native-thread', 'one'),
                      compact('foreign', 'two'), compact('native-thread', 'three', 'agentMessage'),
                      {'method': 'thread/compacted', 'params': {'threadId': 'native-thread'}}]:
            v.event(event)
        self.assertEqual([c['item_id'] for c in v.compactions], ['one'])
        with self.assertRaises(evalmod.HarnessError):
            v.event(compact('native-thread', None))

    def test_usage_is_cumulative_not_repeated_or_double_counted(self):
        b = self.budget()
        v = self.variant(budget=b)
        for n in (100, 100, 250):
            v.event(usage(v.thread_id, n, cachedInputTokens=80, outputTokens=40, reasoningOutputTokens=30))
        self.assertEqual(b.input_tokens, 250)
        self.assertEqual(v.usage.totals()['outputTokens'], 40)
        self.assertEqual(v.usage.totals()['reasoningOutputTokens'], 30)
        self.assertIsNone(v.usage.totals()['cacheWriteInputTokens'])

    def test_missing_usage_stops_and_null_is_not_zero(self):
        v = self.variant()
        self.assertIsNone(v.usage.totals()['inputTokens'])
        with self.assertRaisesRegex(evalmod.HarnessError, 'unavailable'):
            v.event(usage(v.thread_id, None))
        with self.assertRaisesRegex(evalmod.HarnessError, 'usage'):
            v.require_usage_since(0)
        self.assertIsNone(evalmod.observed_delta(None, 10))

    def test_counter_reset_requires_explicit_segment(self):
        v = self.variant()
        v.event(usage(v.thread_id, 90))
        with self.assertRaisesRegex(evalmod.HarnessError, 'reset'):
            v.event(usage(v.thread_id, 10))
        v.start_segment('documented-reset', {'inputTokens': 0}, 'replay test: host restarted its counter')
        v.event(usage(v.thread_id, 20))
        self.assertEqual(v.budget.input_tokens, 110)
        self.assertEqual(v.usage.totals()['inputTokens'], 110)

    def test_budget_and_overshoot_shared_by_variants_and_maintenance(self):
        b = self.budget(limit=100)
        n, o = self.variant('native', b), self.variant('om', b)
        n.event(usage(n.thread_id, 60))
        o.event(usage(o.thread_id, 30))
        o.event(compact(o.thread_id, 'maintenance'))
        with self.assertRaisesRegex(evalmod.HarnessError, 'input ceiling'):
            o.event(usage(o.thread_id, 55))
        self.assertEqual(b.input_tokens, 115)
        self.assertEqual(b.overshoot, 15)
        with self.assertRaises(evalmod.HarnessError):
            b.check()

    def test_wall_deadline_not_reset_for_second_variant(self):
        now = [5]
        b = self.budget(seconds=10, clock=lambda: now[0])
        self.variant('native', b)
        now[0] = 14
        self.variant('om', b)
        now[0] = 15
        with self.assertRaisesRegex(evalmod.HarnessError, 'wall'):
            b.check()

    def test_rpc_exact_ids_out_of_order_notifications_and_error(self):
        events = iter([{'id': 2, 'result': {'second': True}}, usage('native-thread', 15),
                       {'id': 1, 'result': {'first': True}}, {'id': 3, 'error': {'message': 'bad'}}])
        v, sent = self.variant(), []
        rpc = evalmod.RpcClient(lambda timeout: next(events), sent.append, v.budget, v.event)
        rpc.send_request('first', {})
        rpc.send_request('second', {})
        self.assertEqual(rpc.wait_response(1), {'first': True})
        self.assertEqual(rpc.wait_response(2), {'second': True})
        self.assertEqual(v.budget.input_tokens, 15)
        rpc.send_request('bad', {})
        with self.assertRaisesRegex(evalmod.HarnessError, 'RPC'):
            rpc.wait_response(3)

    def test_server_request_never_mistaken_for_response_or_approved(self):
        for method in ['item/commandExecution/requestApproval', 'item/tool/requestUserInput', 'unknown']:
            sent = []
            rpc = evalmod.RpcClient(lambda timeout: {'id': 1, 'method': method, 'params': {}},
                                     sent.append, self.budget(), lambda _: None)
            rpc.send_request('initialize', {})
            with self.assertRaisesRegex(evalmod.HarnessError, 'server request'):
                rpc.wait_response(1)
            self.assertFalse(any('result' in e for e in sent))
            self.assertIn('error', sent[-1])

    def test_split_freeze_and_equivalent_workspaces_no_gold(self):
        pilot = evalmod.Fixture.load(ROOT / 'eval/fixtures/continuity.json', 'pilot')
        self.assertFalse({c['id'] for c in pilot.cases} & {c['id'] for c in self.fixture.cases})
        self.assertNotEqual(pilot.split_hash, self.fixture.split_hash)
        paths = [self.path / x for x in ('native', 'om')]
        for p in paths:
            self.fixture.prepare(p)
        self.assertEqual(evalmod.tree_hash(paths[0]), evalmod.tree_hash(paths[1]))
        text = '\n'.join(p.read_text() for p in paths[0].rglob('*') if p.is_file())
        self.assertNotIn('critical', text)
        self.assertNotIn('forbidden_action', text)
        self.assertNotIn('PILOT_193', text)
        with self.assertRaisesRegex(evalmod.HarnessError, 'frozen'):
            self.fixture.check_hash('0' * 64)
        altered = json.loads((ROOT / 'eval/fixtures/continuity.json').read_text())
        altered['cases'][0]['expected'] += 'changed'
        q = self.path / 'altered.json'; q.write_text(json.dumps(altered))
        self.assertNotEqual(evalmod.Fixture.load(q, 'release').split_hash, self.fixture.split_hash)

    def test_meaningful_batches_are_unique_and_reproducible(self):
        a = self.fixture.batch(1)
        b = self.fixture.batch(2)
        self.assertNotEqual(a, b)
        self.assertEqual(a, self.fixture.batch(1))
        self.assertIn('latency_ms', a)
        self.assertIn('request', a)
        self.assertGreater(len(a), 10000)

    def correct_answers(self):
        # Explicit gold is only used by this independent local scorer test.
        answers = {}
        for case in self.fixture.cases:
            answer = {'answer': case.get('expected', ''), 'action': case.get('expected_action', 'none')}
            if 'source' in case:
                answer['source'] = copy.deepcopy(case['source'])
            answers[case['id']] = answer
        return {'answers': answers}

    def score_cycles(self, v, count=50, fail=None):
        for i in range(1, count + 1):
            v.event(compact(v.thread_id, f'c{i}'))
            v.begin_probe(i, f'p{i}')
            answers = self.correct_answers()
            if i == fail:
                answers['answers']['backend-correction']['answer'] = 'Stoolap'
            v.commands.append({'turn_id': f'p{i}', 'item': {'command': 'python3 actions/audit.py', 'exitCode': 0}})
            v.finish_probe(i, f'p{i}', answers, {'completed-step': {'performed': True, 'unchanged': True}})

    def test_missing_cycles_single_failure_and_tie(self):
        n, o = self.variant(), self.variant('om')
        self.score_cycles(n)
        self.score_cycles(o, 49)
        self.assertFalse(evalmod.quality(n, o)['quality_pass'])
        o.event(compact(o.thread_id, 'c50')); o.begin_probe(50, 'p50')
        o.commands.append({'turn_id': 'p50', 'item': {'command': 'python3 actions/audit.py', 'exitCode': 0}})
        o.finish_probe(50, 'p50', self.correct_answers(), {'completed-step': {'performed': True, 'unchanged': True}})
        self.assertTrue(evalmod.quality(n, o)['quality_pass'])
        self.assertEqual(evalmod.quality(n, o)['comparison'], 'equal quality')
        bad = self.variant('om')
        self.score_cycles(bad, fail=27)
        self.assertFalse(evalmod.quality(n, bad)['quality_pass'])
        self.assertEqual(bad.scorecard()['critical_failures'], 1)
        self.assertEqual(bad.scorecard()['stale_corrections'], 1)

    def test_no_copied_recovery_for_multiple_compactions(self):
        v = self.variant()
        v.event(compact(v.thread_id, 'a')); v.event(compact(v.thread_id, 'b'))
        with self.assertRaisesRegex(evalmod.HarnessError, 'unprobed'):
            v.begin_probe(2, 'probe')
        self.assertFalse(evalmod.quality(v, self.variant('om'))['quality_pass'])

    def test_probe_binding_exact_source_and_real_action_failure(self):
        v = self.variant(); v.event(compact(v.thread_id, 'a')); v.begin_probe(1, 'p')
        with self.assertRaises(evalmod.HarnessError):
            v.finish_probe(1, 'wrong', self.correct_answers())
        answers = self.correct_answers()
        answers['answers']['deferred-error']['source']['text'] += ' invented'
        v.finish_probe(1, 'p', answers)
        self.assertGreater(v.scorecard()['critical_failures'], 0)
        with self.assertRaises(evalmod.HarnessError):
            v.finish_probe(1, 'p', self.correct_answers())

    def test_cli_refuses_invalid_live_settings_before_subprocess(self):
        base = ['--mode', 'release', '--output', str(self.path / 'out')]
        with patch.object(evalmod.subprocess, 'Popen', side_effect=AssertionError('spawn')):
            for extras in [[], ['--cycles', '49'], ['--compact-limit', '1000'], ['--scope', 'body_after_prefix']]:
                self.assertNotEqual(evalmod.main(base + extras), 0)

    def test_dry_run_and_replay_never_spawn(self):
        replay = self.path / 'replay.json'
        replay.write_text(json.dumps({'schema': 1, 'split_hash': self.fixture.split_hash,
                                     'compact_limit': 200000, 'scope': 'total', 'events': []}))
        with patch.object(evalmod.subprocess, 'Popen', side_effect=AssertionError('spawn')), \
             patch.object(evalmod.subprocess, 'run', side_effect=AssertionError('spawn')):
            self.assertEqual(evalmod.main(['--mode', 'dry-run', '--output', str(self.path / 'dry')]), 0)
            self.assertEqual(evalmod.main(['--mode', 'replay', '--output', str(self.path / 'replayed'),
                                           '--replay-events', str(replay)]), 0)
        result = json.loads((self.path / 'replayed/results.json').read_text())
        self.assertFalse(result['release_pass'])
        self.assertEqual(result['status'], 'incomplete')
        self.assertTrue((self.path / 'dry/report.md').exists())
        payload = json.loads(replay.read_text()); payload['compact_limit'] = 1000
        replay.write_text(json.dumps(payload))
        self.assertNotEqual(evalmod.main(['--mode', 'replay', '--output', str(self.path / 'bad'),
                                          '--replay-events', str(replay)]), 0)

class ResumeRegressionTests(unittest.TestCase):
    setUp = HarnessTests.setUp
    variant = HarnessTests.variant
    budget = HarnessTests.budget
    def test_started_compaction_preserves_true_before_usage(self):
        v = self.variant(budget=self.budget(limit=1_000_000)); v.event(usage(v.thread_id, 210000))
        event = compact(v.thread_id, 'c')
        v.event({**event, 'method': 'item/started'})
        after = usage(v.thread_id, 210100)
        after['params']['tokenUsage']['last'] = {'totalTokens': 5570, 'inputTokens': 0, 'outputTokens': 0}
        v.event(after)
        v.event(event)
        self.assertEqual(v.compactions[0]['before_usage']['total']['inputTokens'], 210000)
        self.assertEqual(v.compactions[0]['after_usage']['total']['inputTokens'], 210100)

    def test_actions_need_fresh_effect_not_a_command_mention(self):
        workspace = self.path / 'workspace'; self.fixture.prepare(workspace)
        evidence = evalmod.action_evidence(self.fixture, workspace, 1, before=True)
        after = evalmod.action_evidence(self.fixture, workspace, 1, prior=evidence)
        self.assertFalse(after['completed-step']['performed'])
        case = next(c for c in self.fixture.cases if c['id'] == 'completed-step')
        target = workspace / case['effect']['path'].format(cycle=1)
        target.write_text(json.dumps(case['effect']['content']))
        after = evalmod.action_evidence(self.fixture, workspace, 1, prior=evidence)
        self.assertTrue(after['completed-step']['performed'])
        reused = evalmod.action_evidence(self.fixture, workspace, 1, before=True)
        self.assertFalse(evalmod.action_evidence(self.fixture, workspace, 1, prior=reused)['completed-step']['performed'])

    def test_fixture_rejects_cross_split_content_and_source_mismatch(self):
        data = json.loads((ROOT / 'eval/fixtures/continuity.json').read_text())
        data['workloads']['release']['files']['evidence/extra'] = 'PILOT_193'
        path = self.path / 'bad.json'; path.write_text(json.dumps(data))
        with self.assertRaisesRegex(evalmod.HarnessError, 'split'):
            evalmod.Fixture.load(path, 'release')
        data['workloads']['release']['files'].pop('evidence/extra')
        data['cases'][2]['source']['line'] = 1
        path.write_text(json.dumps(data))
        with self.assertRaisesRegex(evalmod.HarnessError, 'source'):
            evalmod.Fixture.load(path, 'release')

    def test_optional_measurement_gaps_stay_null(self):
        v = self.variant()
        v.event(usage(v.thread_id, 10, outputTokens=2))
        v.event(usage(v.thread_id, 20))
        v.event(usage(v.thread_id, 30, outputTokens=7))
        self.assertEqual(v.budget.input_tokens, 30)
        self.assertIsNone(v.usage.totals()['outputTokens'])

    def test_uncompleted_probe_cannot_be_followed_by_another(self):
        v = self.variant(); v.event(compact(v.thread_id, 'a')); v.begin_probe(1, 'p1')
        v.event(compact(v.thread_id, 'b'))
        with self.assertRaises(evalmod.HarnessError):
            v.begin_probe(2, 'p2')

class FakeHost:
    """Independent scripted App Server transport; no model/subprocess or gold access."""
    def __init__(self, argv, env, cwd):
        self.queue, self.sent, self.closed = [], [], False
        self.skills, self.hooks = [], []
        self.cwd, self.input, self.turn_count = Path(cwd).resolve(), 0, 0
        self.thread = 'test-thread'
        self.config = {}
        for i, arg in enumerate(argv):
            if arg == '-c':
                key, raw = argv[i + 1].split('=', 1)
                value = json.loads(raw)
                if '.' in key:
                    root, key = key.split('.', 1)
                    self.config.setdefault(root, {})[key] = value
                else:
                    self.config[key] = value
        self.config.update({'compact_prompt': None, 'model_context_window': None, 'plugins': {}, 'marketplaces': {}, 'hooks': None})

    def send(self, request):
        self.sent.append(request)
        method, p = request.get('method'), request.get('params', {})
        result = {}
        if method == 'initialized':
            return
        if method == 'account/read':
            result = {'account': {'type': 'chatgpt', 'email': 'never-persist@example.test'}}
        elif method == 'config/read':
            result = {'config': self.config}
        elif method == 'skills/list':
            result = {'data': [{'skills': self.skills, 'errors': []}]}
        elif method == 'hooks/list':
            result = {'data': [{'cwd': str(self.cwd), 'hooks': self.hooks, 'warnings': [], 'errors': []}]}
        elif method == 'model/list':
            result = {'data': [{'model': 'test-model', 'hidden': False,
                               'supportedReasoningEfforts': [{'reasoningEffort': 'high'}]}]}
        elif method == 'thread/start':
            result = {'thread': {'id': self.thread}, 'model': 'test-model', 'reasoningEffort': 'high',
                      'cwd': str(self.cwd), 'approvalPolicy': 'never', 'instructionSources': [str(self.cwd / 'AGENTS.md')]}
        elif method == 'turn/start':
            self.turn_count += 1
            tid = f't{self.turn_count}'
            result = {'turn': {'id': tid}}
            prompt = p['input'][0]['text']
            self.queue.append({'method': 'turn/started', 'params': {'threadId': self.thread, 'turn': {'id': tid}}})
            if 'Analyze new incident' in prompt:
                event = compact(self.thread, f'compact-{tid}'); event['params']['turnId'] = tid
                self.queue.append({**event, 'method': 'item/started'})
                self.input += 20
                self.queue.append(usage(self.thread, self.input, turn=tid))
                self.queue.append(event)
            # Stale usage before the response is not new work. Include the actual
            # final usage after the agent item just like the observed native host.
            self.queue.append(usage(self.thread, self.input, turn=tid))
            self.queue.append({'method': 'item/completed', 'params': {'threadId': self.thread, 'turnId': tid,
                'item': {'type': 'agentMessage', 'id': f'm{tid}', 'text': '{"answers": {}}'}}})
            self.input += 100
            self.queue.append(usage(self.thread, self.input, turn=tid))
            self.queue.append({'method': 'turn/completed', 'params': {'threadId': self.thread,
                                'turn': {'id': tid, 'status': 'completed'}}})
        if 'id' in request:
            # Replies follow some notifications deliberately; exact IDs must work.
            self.queue.append({'id': request['id'], 'result': result})

    def receive(self, timeout):
        if not self.queue:
            return None
        return self.queue.pop(0)

    def close(self, thread_id=None, turn_id=None, notify=None):
        self.closed = True


class DriverTests(unittest.TestCase):
    setUp = HarnessTests.setUp

    def config(self):
        import argparse
        args = argparse.Namespace(codex='unused', model='test-model', reasoning='high', force_compaction=False)
        return evalmod.RunConfig('pilot', self.path, 2, 200000, 'total', 'pilot', args)

    def test_full_native_driver_orders_rpc_and_scores_actual_bad_answers(self):
        fixture = evalmod.Fixture.load(ROOT / 'eval/fixtures/continuity.json', 'pilot')
        workspace = self.path / 'workspace'; fixture.prepare(workspace)
        variant = evalmod.VariantRun('native', fixture, evalmod.RunBudget(30, 10000))
        client = evalmod.LiveVariant(self.config(), variant, workspace, {}, [workspace], FakeHost)
        try:
            client.preflight(); client.drive()
            self.assertEqual(len(variant.compactions), 2)
            self.assertEqual(variant.scorecard()['scored_recoveries'], 2)
            self.assertEqual(variant.scorecard()['critical_failures'], 2)
            self.assertEqual(variant.budget.input_tokens, 540)
            self.assertEqual([p['turn_id'] for p in variant.probes.values()], ['t3', 't5'])
            self.assertNotIn('thread/compact/start', [r.get('method') for r in client.transport.sent])
            self.assertNotIn('never-persist', json.dumps(variant.result()))
        finally:
            client.close()
        self.assertTrue(client.transport.closed)

    def test_input_budget_can_interrupt_while_waiting_for_turn_reply(self):
        fixture = evalmod.Fixture.load(ROOT / 'eval/fixtures/continuity.json', 'pilot')
        workspace = self.path / 'workspace'; fixture.prepare(workspace)
        variant = evalmod.VariantRun('native', fixture, evalmod.RunBudget(30, 90))
        client = evalmod.LiveVariant(self.config(), variant, workspace, {}, [workspace], FakeHost)
        try:
            client.preflight()
            with self.assertRaisesRegex(evalmod.HarnessError, 'input ceiling'):
                client.drive()
            self.assertEqual(variant.budget.overshoot, 10)
            self.assertEqual(len(variant.probes), 0)
        finally:
            client.close()

    def test_pilot_then_release_home_cleanup_preserves_auth_and_refuses_edits(self):
        home = self.path / 'home'; home.mkdir()
        auth = home / 'auth.json'; auth.write_text('private-test-signin')
        baseline = evalmod.prepare_home(home)
        (home / 'sessions').mkdir(); (home / 'sessions/owned').write_text('pilot evidence')
        (home / 'plugins').mkdir(); (home / 'plugins/owned').write_text('pilot store')
        evalmod.finish_home(home, baseline, self.path / 'pilot')
        evalmod.prepare_home(home)
        self.assertEqual(auth.read_text(), 'private-test-signin')
        self.assertFalse((home / 'sessions').exists())
        self.assertFalse((home / 'plugins').exists())
        (home / 'sessions').mkdir(); (home / 'sessions/owned').write_text('release evidence')
        evalmod.finish_home(home, baseline, self.path / 'release')
        (home / 'sessions/owned').write_text('external change')
        with self.assertRaisesRegex(evalmod.HarnessError, 'changed'):
            evalmod.prepare_home(home)
        self.assertEqual((home / 'sessions/owned').read_text(), 'external change')

    def test_forbidden_execution_in_failed_workload_stays_failed_after_undo(self):
        v = evalmod.VariantRun('om', self.fixture, evalmod.RunBudget(30, 1000)); v.start_thread('thread')
        for command in ['cat actions/migrate.py', 'echo "python3 actions/migrate.py"', 'rg migrate.py evidence']:
            self.assertFalse(evalmod.executes_script(command, 'actions/migrate.py'), command)
        for command in ['python3 actions/migrate.py', 'cd . && python3 ./actions/migrate.py',
                        'bash -c "python3 actions/migrate.py; exit 1"']:
            self.assertTrue(evalmod.executes_script(command, 'actions/migrate.py'), command)
        v.event({'method': 'item/started', 'params': {'threadId': 'thread', 'turnId': 'ordinary',
                 'item': {'id': 'attempt', 'type': 'commandExecution', 'command': 'python3 actions/migrate.py'}}})
        self.assertEqual(v.scorecard()['repeated_completed_actions'], 1)

    def test_rerouted_model_and_effective_memory_mismatch_fail(self):
        v = evalmod.VariantRun('native', self.fixture, evalmod.RunBudget(30, 1000)); v.start_thread('thread')
        with self.assertRaisesRegex(evalmod.HarnessError, 'rerouted'):
            v.event({'method': 'model/rerouted', 'params': {'threadId': 'thread', 'fromModel': 'selected', 'toModel': 'other'}})
        with self.assertRaisesRegex(evalmod.HarnessError, 'memory'):
            evalmod.verify_effective({'config': {'model_auto_compact_token_limit': 200000,
                'model_auto_compact_token_limit_scope': 'total', 'memories': {'use_memories': True, 'generate_memories': False}}}, self.config())

class HomeLifecycleTests(unittest.TestCase):
    setUp = HarnessTests.setUp

    def home(self, name='home', login=False):
        home = self.path / name; home.mkdir()
        (home / 'auth.json').write_text('private-normal-login')
        if login:
            for name in ('log', 'tmp'):
                (home / name).mkdir()
                (home / name / 'login-artifact').write_text('preserve unowned login state')
        return home

    def test_normal_login_artifacts_survive_cache_cleanup_and_reuse(self):
        home = self.home(login=True)
        baseline = evalmod.prepare_home(home)
        (home / 'cache').mkdir(); (home / 'cache/owned').write_text('host cache')
        evalmod.finish_home(home, baseline, self.path / 'run')
        state = evalmod.validate_home(home)
        self.assertEqual(set(state['created']), {'cache'})
        self.assertNotIn('auth.json', state['created'])
        evalmod.prepare_home(home)
        self.assertFalse((home / 'cache').exists())
        for name in ('log', 'tmp'):
            self.assertEqual((home / name / 'login-artifact').read_text(), 'preserve unowned login state')
        self.assertEqual((home / 'auth.json').read_text(), 'private-normal-login')

    def test_incomplete_run_refuses_reuse_even_before_host_creates_state(self):
        home = self.home()
        evalmod.prepare_home(home)
        self.assertTrue((home / evalmod.HOME_MARKER).is_file())
        with self.assertRaisesRegex(evalmod.HarnessError, 'incomplete'):
            evalmod.prepare_home(home)
        self.assertEqual((home / 'auth.json').read_text(), 'private-normal-login')

    def test_unknown_state_and_symlinks_are_never_adopted(self):
        for name in ('sessions', 'config.toml', 'cache', 'unrelated'):
            home = self.home(name)
            (home / name).write_text('unowned')
            with self.subTest(name=name), self.assertRaises(evalmod.HarnessError):
                evalmod.prepare_home(home)
            self.assertEqual((home / name).read_text(), 'unowned')
        for name in ('auth.json', 'log', 'tmp', evalmod.HOME_MARKER):
            home = self.home('link-' + name)
            target = self.path / ('target-' + name); target.write_text('unowned')
            (home / name).unlink(missing_ok=True); (home / name).symlink_to(target)
            with self.subTest(link=name), self.assertRaises(evalmod.HarnessError):
                evalmod.prepare_home(home)
            self.assertEqual(target.read_text(), 'unowned')
        home = self.home('nested', login=True)
        (home / 'log/link').symlink_to(self.path / 'missing')
        with self.assertRaises(evalmod.HarnessError):
            evalmod.prepare_home(home)

    def test_failed_finalizer_leaves_incomplete_ownership(self):
        home = self.home()
        baseline = evalmod.prepare_home(home)
        (home / 'unrelated').write_text('do not remove')
        with self.assertRaises(evalmod.HarnessError):
            evalmod.finish_home(home, baseline, self.path / 'run')
        self.assertTrue((home / evalmod.HOME_MARKER).is_file())
        with self.assertRaisesRegex(evalmod.HarnessError, 'incomplete'):
            evalmod.prepare_home(home)
        self.assertEqual((home / 'unrelated').read_text(), 'do not remove')

    def test_finalizer_requires_the_prepared_run_identity(self):
        home = self.home()
        output = self.path / 'prepared-run'; output.mkdir()
        baseline = evalmod.prepare_home(home, output)
        marker = home / evalmod.HOME_MARKER
        before = marker.read_bytes()
        with self.assertRaisesRegex(evalmod.HarnessError, 'run identity'):
            evalmod.finish_home(home, baseline, self.path / 'other-run')
        self.assertEqual(marker.read_bytes(), before)
        with self.assertRaisesRegex(evalmod.HarnessError, 'incomplete'):
            evalmod.validate_home(home)
        alias = self.path / 'same-run'; alias.symlink_to(output, target_is_directory=True)
        evalmod.finish_home(home, baseline, alias)
        self.assertEqual(evalmod.validate_home(home)['run'], str(output.resolve()))

    def test_baseline_and_owned_symlinks_are_refused_after_run(self):
        for name in ('auth.json', 'log', 'cache'):
            home = self.home('changed-' + name, login=True)
            baseline = evalmod.prepare_home(home)
            if name == 'cache':
                (home / name).mkdir()
            evalmod.finish_home(home, baseline, self.path / 'run')
            target = self.path / ('outside-' + name); target.write_text('private unowned state')
            if (home / name).is_dir():
                (home / name / 'foreign').symlink_to(target)
            else:
                (home / name).unlink(); (home / name).symlink_to(target)
            with self.subTest(name=name), self.assertRaises(evalmod.HarnessError):
                evalmod.prepare_home(home)
            self.assertEqual(target.read_text(), 'private unowned state')

    def test_primary_failure_survives_both_home_and_global_finalization(self):
        import argparse
        native, om = self.home('native'), self.home('om')
        args = argparse.Namespace(native_home=native, om_home=om, codex='unused')
        config = evalmod.RunConfig('pilot', self.path, 1, 200000, 'total', 'pilot', args)
        fixture = evalmod.Fixture.load(ROOT / 'eval/fixtures/continuity.json', 'pilot')
        budget = evalmod.RunBudget(30, 10000)
        variants = {name: evalmod.VariantRun(name, fixture, budget) for name in evalmod.VARIANTS}
        metadata = {}
        def fail(*args):
            (native / 'unrelated').write_text('retain')
            (om / 'cache').mkdir(); (om / 'cache/owned').write_text('host state')
            raise evalmod.HarnessError('original preflight failure')
        with patch.object(evalmod, 'schema_preflight', side_effect=fail), \
             patch.object(evalmod, 'activation_check', return_value={'verified': True}), \
             patch.object(Path, 'home', return_value=self.path / 'synthetic-user'), \
             patch.dict(evalmod.os.environ, {'CODEX_HOME': str(self.path / 'synthetic-user/.codex')}), \
             patch.object(evalmod.subprocess, 'Popen', side_effect=AssertionError('model spawn')):
            with self.assertRaisesRegex(evalmod.HarnessError, '^original preflight failure$'):
                evalmod.live(config, fixture, variants, budget, metadata)
        self.assertTrue(metadata['global_config_unchanged'])
        self.assertEqual(set(metadata['home_finalization']), {'native', 'om'})
        self.assertEqual(metadata['home_finalization']['native']['status'], 'failed')
        self.assertEqual(metadata['home_finalization']['om']['status'], 'complete')
        self.assertTrue(metadata['finalization_errors'])
        self.assertEqual(set(evalmod.validate_home(om)['created']), {'cache'})
        with self.assertRaisesRegex(evalmod.HarnessError, 'incomplete'):
            evalmod.validate_home(native)
        self.assertIsNone(variants['native'].usage.totals()['inputTokens'])
        for home in (native, om):
            self.assertEqual((home / 'auth.json').read_text(), 'private-normal-login')

    def test_failed_activation_shutdown_leaves_om_ownership_incomplete(self):
        import argparse
        native, om = self.home('trust-native'), self.home('trust-om')
        data = self.path / 'staged'; (data / 'bin').mkdir(parents=True)
        (data / 'bin/om').write_text('meter')
        config = evalmod.RunConfig('pilot', self.path, 1, 200000, 'total', 'pilot',
            argparse.Namespace(native_home=native, om_home=om, codex='unused'))
        metadata = {}
        with patch.object(evalmod, 'activation_check', return_value={'verified': True}), \
             patch.object(evalmod, 'schema_preflight', return_value={}), \
             patch.object(evalmod, 'stage_plugin', return_value={'data': str(data)}), \
             patch.object(evalmod, 'install_meter', return_value=data / 'calls.jsonl'), \
             patch.object(evalmod, 'prepare_live_activation', side_effect=KeyboardInterrupt('trust shutdown')), \
             patch.object(Path, 'home', return_value=self.path / 'synthetic-user'), \
             patch.dict(evalmod.os.environ, {'CODEX_HOME': str(self.path / 'synthetic-user/.codex')}):
            with self.assertRaisesRegex(KeyboardInterrupt, 'trust shutdown'):
                evalmod.live(config, self.fixture, {}, evalmod.RunBudget(20, 1000), metadata)
        self.assertEqual(metadata['home_finalization']['native']['status'], 'complete')
        self.assertEqual(metadata['home_finalization']['om']['status'], 'failed')
        self.assertTrue(metadata['global_config_unchanged'])
        with self.assertRaisesRegex(evalmod.HarnessError, 'incomplete'):
            evalmod.validate_home(om)

    def test_both_home_errors_and_global_drift_remain_visible_with_primary_error(self):
        import argparse, os
        native, om = self.home('native'), self.home('om')
        protected = self.path / 'synthetic-user/.codex'; protected.mkdir(parents=True)
        global_config = protected / 'config.toml'; global_config.write_text('before')
        args = argparse.Namespace(native_home=native, om_home=om, codex='unused')
        config = evalmod.RunConfig('pilot', self.path, 1, 200000, 'total', 'pilot', args)
        fixture = evalmod.Fixture.load(ROOT / 'eval/fixtures/continuity.json', 'pilot')
        budget = evalmod.RunBudget(30, 10000)
        variants = {name: evalmod.VariantRun(name, fixture, budget) for name in evalmod.VARIANTS}
        metadata = {}
        def fail(*args):
            for home in (native, om):
                (home / 'unrelated').write_text('retain')
            global_config.write_text('simulated external drift')
            raise evalmod.HarnessError('original failure')
        with patch.object(evalmod, 'schema_preflight', side_effect=fail), \
             patch.object(evalmod, 'activation_check', return_value={'verified': True}), \
             patch.object(Path, 'home', return_value=protected.parent), \
             patch.dict(os.environ, {'CODEX_HOME': str(protected)}), \
             patch.object(evalmod.subprocess, 'Popen', side_effect=AssertionError('model spawn')):
            with self.assertRaisesRegex(evalmod.HarnessError, '^original failure$'):
                evalmod.live(config, fixture, variants, budget, metadata)
        self.assertFalse(metadata['global_config_unchanged'])
        self.assertEqual(metadata['global_config_finalization']['status'], 'failed')
        self.assertEqual({e['stage'] for e in metadata['finalization_errors']},
                         {'native home', 'om home', 'global configuration'})
        for home in (native, om):
            with self.assertRaisesRegex(evalmod.HarnessError, 'incomplete'):
                evalmod.validate_home(home)
            self.assertEqual((home / 'unrelated').read_text(), 'retain')


class OMAndShutdownTests(unittest.TestCase):
    setUp = HarnessTests.setUp

    def test_deferral_proof_requires_actual_successful_apply_and_retained_source(self):
        from types import SimpleNamespace
        data = self.path / 'data'; data.mkdir()
        (data / 'calls.jsonl').write_text(json.dumps({'session': 'owned', 'deferrals': ['s-log']}) + '\n')
        v = evalmod.VariantRun('om', self.fixture, evalmod.RunBudget(30, 1000)); v.start_thread('owned')
        c = SimpleNamespace(variant=v, data=data, config=SimpleNamespace(args=SimpleNamespace(om_home=self.path)))
        line = self.fixture.workload['files']['evidence/build.log']
        page = {'page': {'items': [{'evidence': {'source_id': 's-log', 'start_byte': 0, 'text': line,
                        'review_state': 'deferred', 'deferral_reason': 'bulky build output', 'source_incomplete': False}}]}}
        def pages(*args, **kwargs):
            offset = 10000 * pages.n
            unit = copy.deepcopy(page['page']['items'][0]['evidence']); unit['text'] = line[offset:offset + 10000]; unit['start_byte'] = len(line[:offset].encode())
            unit['end_byte'] = unit['start_byte'] + len(unit['text'].encode()); unit['kind'] = 'tool'
            pages.n += 1
            return json.dumps({'page': {'items': [{'evidence': unit}], 'next_cursor': str(pages.n) if offset + 10000 < len(line) else ''}}, ensure_ascii=False)
        pages.n = 0
        with patch.object(evalmod, 'run_local', side_effect=pages):
            evalmod.verify_deferrals(c)
            self.assertTrue(v.deferral_evidence['verified'])
        page['page']['items'][0]['evidence']['deferral_reason'] = ''
        pages.n = 0
        with patch.object(evalmod, 'run_local', side_effect=pages):
            with self.assertRaisesRegex(evalmod.HarnessError, 'deferred'):
                evalmod.verify_deferrals(c)
        (data / 'calls.jsonl').write_text(json.dumps({'session': 'other', 'deferrals': ['s-log']}) + '\n')
        with patch.object(evalmod, 'run_local', side_effect=AssertionError('wrong session read')):
            with self.assertRaises(evalmod.HarnessError):
                evalmod.verify_deferrals(c)

    def test_shutdown_keeps_final_usage_after_ceiling_and_owns_interrupt(self):
        from unittest.mock import Mock
        v = evalmod.VariantRun('native', self.fixture, evalmod.RunBudget(30, 100)); v.start_thread('owned')
        with self.assertRaises(evalmod.HarnessError):
            v.event(usage('owned', 110))
        transport = object.__new__(evalmod.ProcessTransport)
        transport.process = Mock()
        transport.process.poll.return_value = None
        transport.selector = Mock()
        transport.send = Mock()
        events = iter([usage('owned', 135), {'method': 'turn/completed', 'params': {'threadId': 'owned',
                       'turn': {'id': 'running', 'status': 'interrupted'}}}])
        transport.receive = lambda timeout: next(events, None)
        transport.close('owned', 'running', v.event)
        self.assertEqual(v.budget.input_tokens, 135)
        self.assertEqual(v.budget.overshoot, 35)
        sent = transport.send.call_args.args[0]
        self.assertEqual(sent['method'], 'turn/interrupt')
        self.assertEqual(sent['params'], {'threadId': 'owned', 'turnId': 'running'})
        transport.process.terminate.assert_not_called()

    def test_stale_usage_after_final_message_is_insufficient(self):
        v = evalmod.VariantRun('native', self.fixture, evalmod.RunBudget(30, 1000)); v.start_thread('owned')
        v.event(usage('owned', 10))
        v.event({'method': 'item/completed', 'params': {'threadId': 'owned', 'turnId': 't',
                 'item': {'id': 'm', 'type': 'agentMessage', 'text': 'done'}}})
        with self.assertRaisesRegex(evalmod.HarnessError, 'usage'):
            v.require_usage_since(0)
        v.event(usage('owned', 20, turn='t'))
        v.require_usage_since(0)

    def test_missing_or_malformed_replay_preserves_failure_artifact(self):
        replay = self.path / 'bad.json'; replay.write_text('{"schema":1,"events":[null]}')
        self.assertEqual(evalmod.main(['--mode', 'replay', '--output', str(self.path / 'out'),
                                      '--replay-events', str(replay)]), 1)
        result = json.loads((self.path / 'out/results.json').read_text())
        self.assertEqual(result['status'], 'failed')
        self.assertFalse(result['release_pass'])

class ActivationTests(unittest.TestCase):
    setUp = HarnessTests.setUp

    def listing(self):
        installed = self.path / 'plugin'; installed.mkdir()
        workspace = self.path / 'workspace'; workspace.mkdir()
        return fake_hook_listing(workspace, installed), workspace, installed

    def test_only_exact_five_candidate_hooks_can_be_trusted(self):
        listing, workspace, installed = self.listing()
        hashes = evalmod.validate_hooks(listing, 'om', workspace, installed)
        self.assertEqual(len(hashes), 5)
        for mutate in (lambda h: h.pop(), lambda h: h.append(copy.deepcopy(h[0])),
                       lambda h: h[0].update({'enabled': False}), lambda h: h[0].update({'trustStatus': 'modified'}),
                       lambda h: h[0].update({'command': 'sh /other/run.sh'}),
                       lambda h: h[0].update({'sourcePath': '/other/hooks.json'}),
                       lambda h: h[0].update({'timeoutSec': 6}), lambda h: h[0].update({'async': True}),
                       lambda h: h[0].update({'matcher': 'startup'}), lambda h: h[0].update({'pluginId': 'other'})):
            changed = copy.deepcopy(listing); mutate(changed['data'][0]['hooks'])
            with self.assertRaises(evalmod.HarnessError):
                evalmod.validate_hooks(changed, 'om', workspace, installed)
        with self.assertRaises(evalmod.HarnessError):
            evalmod.validate_hooks(listing, 'native', workspace)
        listing['data'][0]['hooks'] = []
        self.assertEqual(evalmod.validate_hooks(listing, 'native', workspace), {})

    def test_revalidation_never_accepts_modified_or_missing_trust(self):
        listing, workspace, installed = self.listing()
        expected = evalmod.validate_hooks(listing, 'om', workspace, installed)
        with self.assertRaises(evalmod.HarnessError):
            evalmod.validate_hooks(listing, 'om', workspace, installed, expected)
        for hook in listing['data'][0]['hooks']:
            hook['trustStatus'] = 'trusted'
        self.assertEqual(evalmod.validate_hooks(listing, 'om', workspace, installed, expected), expected)
        listing['data'][0]['hooks'][0]['currentHash'] = 'sha256:' + 'f' * 64
        with self.assertRaises(evalmod.HarnessError):
            evalmod.validate_hooks(listing, 'om', workspace, installed, expected)

    def test_missing_activation_is_a_failure_in_both_paid_modes(self):
        for mode in ('pilot', 'release'):
            config = DriverTests.config(self); config.mode = mode
            variants = {name: evalmod.VariantRun(name, self.fixture, evalmod.RunBudget(30, 10000)) for name in evalmod.VARIANTS}
            result = evalmod.output_results(config, self.fixture, variants, variants['om'].budget,
                                           {'om_plugin_verified': False}, None)
            self.assertEqual(result['status'], 'failed')
            self.assertFalse(result['quality']['quality_pass'])
            self.assertTrue(any('activation' in reason for reason in result['eligibility_reasons']))

    def test_canary_failure_checks_global_config_without_preparing_live_homes(self):
        config = DriverTests.config(self)
        global_dir = self.path / 'global'; global_dir.mkdir()
        global_config = global_dir / 'config.toml'; global_config.write_text('original')
        def failed(*args):
            global_config.write_text('changed')
            raise evalmod.HarnessError('primary canary failure')
        metadata = {}
        with patch.object(Path, 'home', return_value=self.path / 'synthetic-user'), \
             patch.dict(evalmod.os.environ, {'CODEX_HOME': str(global_dir)}), \
             patch.object(evalmod, 'activation_check', side_effect=failed), \
             patch.object(evalmod, 'prepare_home', side_effect=AssertionError('live home touched')):
            with self.assertRaisesRegex(evalmod.HarnessError, 'primary canary failure'):
                evalmod.live(config, self.fixture, {}, evalmod.RunBudget(20, 1000), metadata)
        self.assertFalse(metadata['global_config_unchanged'])
        self.assertEqual(metadata['global_config_finalization']['status'], 'failed')
        self.assertEqual(metadata['home_finalization'], {})

    def test_initialize_failure_always_closes_new_transport_and_preserves_failure(self):
        from unittest.mock import Mock
        for close_error in (None, KeyboardInterrupt()):
            transport = Mock()
            transport.close.side_effect = close_error
            with patch.object(evalmod, 'ProcessTransport', return_value=transport), \
                 patch.object(evalmod.RpcClient, 'request', side_effect=evalmod.HarnessError('initialize failed')):
                with self.assertRaisesRegex(evalmod.HarnessError, 'initialize failed'):
                    evalmod.ActivationSession('unused', self.path, {}, {}, evalmod.RunBudget(5, None))
            transport.close.assert_called_once()

    def test_binding_rejects_each_changed_component(self):
        installed = self.path / 'plugin'; installed.mkdir()
        script = installed / 'run.sh'; script.write_text('reviewed script')
        data = self.path / 'data'; (data / 'bin').mkdir(parents=True)
        candidate = data / 'bin/om-candidate'; candidate.write_text('candidate')
        meter = data / 'bin/om'; meter.write_text('meter')
        binding = {'installed': str(installed), 'data': str(data), 'plugin_sha256': evalmod.tree_hash(installed),
                   'candidate_sha256': evalmod.digest(candidate.read_bytes()), 'meter_sha256': evalmod.digest(meter.read_bytes())}
        evalmod.verify_activation_bytes(binding)
        for path in (script, candidate, meter):
            original = path.read_bytes(); path.write_bytes(b'changed')
            with self.assertRaisesRegex(evalmod.HarnessError, 'bytes changed'):
                evalmod.verify_activation_bytes(binding)
            path.write_bytes(original)

    def test_complete_hook_set_is_checked_before_any_trust_write(self):
        from unittest.mock import Mock
        listing, workspace, installed = self.listing()
        listing['data'][0]['hooks'].pop()
        session = Mock(); session.rpc.request.return_value = listing
        with patch.object(evalmod, 'verify_activation_bytes'), \
             patch.object(evalmod, 'ActivationSession', return_value=session):
            with self.assertRaises(evalmod.HarnessError):
                evalmod.trust_candidate(DriverTests.config(self), {'installed': str(installed)},
                    evalmod.RunBudget(5, None), {'CODEX_HOME': str(self.path)}, workspace, {})
        self.assertEqual([call.args[0] for call in session.rpc.request.call_args_list], ['hooks/list'])
        session.close.assert_called_once()

    def test_intervention_requires_owned_native_and_metered_success(self):
        binding = {'installed': '/isolated/plugin'}
        event = {'method': 'hook/completed', 'params': {'threadId': 'owned', 'run': {
            'eventName': 'userPromptSubmit', 'source': 'plugin', 'sourcePath': '/isolated/plugin/hooks/hooks.json',
            'handlerType': 'command', 'executionMode': 'sync', 'status': 'completed', 'entries': []}}}
        call = {'session': 'owned', 'hook': True, 'hook_event': 'UserPromptSubmit', 'exit_code': 0}
        self.assertTrue(evalmod.verify_intervention([event], [call], binding, 'owned', {'userPromptSubmit'})['verified'])
        for events, calls in (([], [call]), ([event], []), ([event], [{**call, 'session': 'other'}]),
                              ([event], [{**call, 'exit_code': 1}]),
                              ([event], [call, {'session': 'owned', 'command': 'prime', 'prime_valid': False}])):
            with self.assertRaises(evalmod.HarnessError):
                evalmod.verify_intervention(events, calls, binding, 'owned', {'userPromptSubmit'})
        with self.assertRaisesRegex(evalmod.HarnessError, 'hook-delivered prime'):
            evalmod.verify_intervention([event], [call, {'session': 'owned', 'command': 'prime', 'prime_valid': True}],
                                        binding, 'owned', {'userPromptSubmit'}, prime=True)

    def test_public_activation_timeout_and_separate_result(self):
        binary = self.path / 'om'; binary.write_text('candidate'); binary.chmod(0o700)
        plugin = self.path / 'plugin'; plugin.mkdir()
        output = self.path / 'proof'
        def check(config, budget):
            self.assertEqual(budget.max_seconds, 7)
            self.assertIsNone(budget.max_input_tokens)
            return {'verified': True, 'synthetic_usage_excluded': True}
        with patch.object(evalmod, 'activation_check', side_effect=check):
            self.assertEqual(evalmod.main(['--mode', 'activation-check', '--output', str(output),
                '--om-binary', str(binary), '--plugin-root', str(plugin), '--max-seconds', '7']), 0)
        self.assertTrue((output / 'activation.json').is_file())
        self.assertFalse((output / 'results.json').exists())

    def test_nullable_command_output_never_substitutes_for_prime_evidence(self):
        binding = {'installed': '/isolated/plugin'}
        hook = {'method': 'hook/completed', 'params': {'threadId': 'owned', 'run': {
            'eventName': 'sessionStart', 'source': 'plugin', 'sourcePath': '/isolated/plugin/hooks/hooks.json',
            'handlerType': 'command', 'executionMode': 'sync', 'status': 'completed',
            'entries': [{'kind': 'context', 'text': 'Ledger command: om --session owned. Review one page.'}]}}}
        prime = {'method': 'item/completed', 'params': {'threadId': 'owned', 'item': {
            'type': 'commandExecution', 'exitCode': 0, 'aggregatedOutput': 'Session: "owned"',
            'commandActions': [{'command': 'om --session owned prime'}]}}}
        calls = [{'session': 'owned', 'hook': True, 'hook_event': 'SessionStart', 'exit_code': 0},
                 {'session': 'owned', 'command': 'prime', 'prime_valid': True}]
        def verify(events):
            return evalmod.verify_intervention(events, calls, binding, 'owned', {'sessionStart'}, prime=True)
        for output in ({}, {'aggregatedOutput': None}):
            unrelated = {'method': 'item/completed', 'params': {'threadId': 'owned', 'item': {
                'type': 'commandExecution', 'exitCode': 0, 'commandActions': [], **output}}}
            for events in ([hook, unrelated, prime], [hook, prime, unrelated]):
                with self.subTest(output=output, prime_last=events[-1] is prime):
                    self.assertTrue(verify(events)['prime_verified'])
        for output in ({}, {'aggregatedOutput': None}, {'aggregatedOutput': ''},
                       {'aggregatedOutput': 'Session: "other"'}, {'aggregatedOutput': ['Session: "owned"']}):
            invalid = copy.deepcopy(prime)
            del invalid['params']['item']['aggregatedOutput']
            invalid['params']['item'].update(output)
            with self.subTest(prime_output=output), self.assertRaisesRegex(evalmod.HarnessError, 'hook-delivered prime'):
                verify([hook, invalid])
        wrong = copy.deepcopy(prime); wrong['params']['item']['commandActions'] = [{'command': 'other prime'}]
        for events in ([hook], [hook, wrong]):
            with self.assertRaisesRegex(evalmod.HarnessError, 'hook-delivered prime'):
                verify(events)


class EffectiveComparisonTests(unittest.TestCase):
    setUp = HarnessTests.setUp

    def configs(self):
        market = self.path / 'candidate-market'; market.mkdir(exist_ok=True)
        native = {'model': 'test-model', 'plugins': {}, 'marketplaces': {},
                  'sandbox_workspace_write': {'writable_roots': ['/native'], 'network_access': False}}
        om = copy.deepcopy(native)
        om['sandbox_workspace_write']['writable_roots'] = ['/om', '/store']
        om['plugins'] = {'observational-memory@om-evaluation': {'enabled': True}}
        # Exact local registration shape observed in the Codex 0.153.4 pilot.
        om['marketplaces'] = {'om-evaluation': {'source_type': 'local', 'source': str(market.resolve()),
            'ref': None, 'last_revision': None, 'last_updated': None, 'sparse_paths': None}}
        return native, om, market

    def test_actual_serialized_hook_defaults_normalize_only_reviewed_trust(self):
        native, om, market = self.configs()
        native['hooks'] = None
        hashes = {'reviewed': 'sha256:' + 'a' * 64}
        om['hooks'] = {event: [] for event in ('Interrupt', 'PermissionRequest', 'PostCompact', 'PostToolUse',
            'PreCompact', 'PreToolUse', 'SessionEnd', 'SessionStart', 'Stop', 'SubagentStart', 'SubagentStop', 'UserPromptSubmit')}
        om['hooks']['state'] = {'reviewed': {'trusted_hash': hashes['reviewed']}}
        expected = evalmod.effective_comparison(native, 'native', market)
        self.assertEqual(expected, evalmod.effective_comparison(om, 'om', market, hashes))
        for mutate in (lambda h: h.update({'unknown': []}), lambda h: h['SessionStart'].append({'command': 'extra'}),
                       lambda h: h['state'].update({'other': {'trusted_hash': 'extra'}})):
            changed = copy.deepcopy(om); mutate(changed['hooks'])
            self.assertNotEqual(expected, evalmod.effective_comparison(changed, 'om', market, hashes))
        for value in (None, {}, {'reviewed': {'trusted_hash': 'changed'}},
                      {'reviewed': {'trusted_hash': hashes['reviewed'], 'enabled': True}}):
            changed = copy.deepcopy(om); changed['hooks']['state'] = value
            with self.assertRaises(evalmod.HarnessError):
                evalmod.effective_comparison(changed, 'om', market, hashes)

    def test_only_intended_entries_are_normalized_without_mutating_inputs(self):
        native, om, market = self.configs()
        # An identical unrelated marketplace remains part of strict comparison.
        for config in (native, om):
            config['marketplaces']['shared'] = {'source': '/shared', 'source_type': 'local'}
        originals = copy.deepcopy((native, om))
        compared = [evalmod.effective_comparison(c, name, market)
                    for c, name in ((native, 'native'), (om, 'om'))]
        self.assertEqual(compared[0], compared[1])
        self.assertEqual(compared[1]['marketplaces'], native['marketplaces'])
        self.assertEqual((native, om), originals)
        om['marketplaces']['shared']['source'] = '/changed'
        self.assertNotEqual(evalmod.effective_comparison(native, 'native', market),
                            evalmod.effective_comparison(om, 'om', market))

    def test_canonical_alias_and_omitted_null_metadata_are_allowed(self):
        native, om, market = self.configs()
        alias = self.path / 'market-alias'; alias.symlink_to(market, target_is_directory=True)
        om['marketplaces']['om-evaluation'] = {'source_type': 'local', 'source': str(alias)}
        self.assertEqual(evalmod.effective_comparison(native, 'native', market),
                         evalmod.effective_comparison(om, 'om', market))

    def test_invalid_marketplace_registrations_are_refused(self):
        native, om, market = self.configs()
        entry = om['marketplaces']['om-evaluation']
        invalid = [None, [], '', {}, {'source_type': 'local'}, {'source': str(market)},
                   *[{**entry, 'source': source} for source in (None, [], '', 'candidate-market',
                       '/other/candidate-market', str(self.path / 'prior-run/candidate-market'),
                       str(market) + '\x00', 'https://example.test/candidate-market')],
                   {**entry, 'source_type': 'git'}, {**entry, 'unknown': None},
                   *[{**entry, key: value} for key, value in (('ref', 'main'), ('last_revision', 'abc'),
                       ('last_updated', 1), ('sparse_paths', []))]]
        for bad in invalid:
            with self.subTest(entry=bad):
                config = copy.deepcopy(om); config['marketplaces']['om-evaluation'] = bad
                with self.assertRaises(evalmod.HarnessError):
                    evalmod.effective_comparison(config, 'om', market)
        for markets in (None, [], {}, {'wrong-identity': entry}):
            with self.subTest(markets=markets):
                config = copy.deepcopy(om); config['marketplaces'] = markets
                with self.assertRaises(evalmod.HarnessError):
                    evalmod.effective_comparison(config, 'om', market)
        native['marketplaces']['om-evaluation'] = entry
        with self.assertRaises(evalmod.HarnessError):
            evalmod.effective_comparison(native, 'native', market)

    def test_invalid_plugin_registrations_are_refused(self):
        native, om, market = self.configs()
        plugin_id = 'observational-memory@om-evaluation'
        invalid = [None, [], {}, {'wrong@om-evaluation': {'enabled': True}},
                   {plugin_id: {'enabled': True}, 'extra@market': {'enabled': False}},
                   *[{plugin_id: value} for value in (None, [], {}, {'enabled': False}, {'enabled': 1},
                       {'enabled': 'true'}, {'enabled': True, 'extra': None})]]
        for plugins in invalid:
            with self.subTest(plugins=plugins):
                config = copy.deepcopy(om); config['plugins'] = plugins
                with self.assertRaises(evalmod.HarnessError):
                    evalmod.effective_comparison(config, 'om', market)
        for plugins in (None, [], {plugin_id: {'enabled': True}}, {'unrelated@market': {'enabled': False}}):
            with self.subTest(native_plugins=plugins):
                config = copy.deepcopy(native); config['plugins'] = plugins
                with self.assertRaises(evalmod.HarnessError):
                    evalmod.effective_comparison(config, 'native', market)
        for name, original in (('native', native), ('om', om)):
            for key in ('plugins', 'marketplaces'):
                config = copy.deepcopy(original); del config[key]
                with self.subTest(variant=name, missing=key), self.assertRaises(evalmod.HarnessError):
                    evalmod.effective_comparison(config, name, market)


class MatchedRunTests(unittest.TestCase):
    setUp = HarnessTests.setUp

    def run_matched(self, mutate=None, error=None, finalization=False,
                    exception_type=evalmod.HarnessError, interrupted_cleanup=False):
        import argparse
        fixture = evalmod.Fixture.load(ROOT / 'eval/fixtures/continuity.json', 'pilot')
        native_home, om_home = self.path / 'native-home', self.path / 'om-home'
        native_home.mkdir(exist_ok=True); om_home.mkdir(exist_ok=True)
        for home in (native_home, om_home):
            (home / 'auth.json').write_text('private-fake-auth')
        candidate = self.path / 'candidate'; candidate.write_text('candidate bytes')
        plugin = self.path / 'plugin'; plugin.mkdir(exist_ok=True); (plugin / 'identity').write_text('plugin bytes')
        args = argparse.Namespace(native_home=native_home, om_home=om_home, codex='unused', om_binary=candidate,
                                  plugin_root=plugin, model='test-model', reasoning='high', force_compaction=False)
        for run_name in ('pilot-one', 'pilot-two'):
            output = Path(tempfile.mkdtemp(prefix=run_name, dir=self.path))
            config = evalmod.RunConfig('pilot', output, 2, 200000, 'total', 'pilot', args)
            for name in evalmod.VARIANTS:
                fixture.prepare(output / name / 'workspace')
            budget = evalmod.RunBudget(30, 10000)
            variants = {n: evalmod.VariantRun(n, fixture, budget) for n in evalmod.VARIANTS}
            meta = {'om_binary_sha256': evalmod.digest(candidate.read_bytes()), 'plugin_sha256': evalmod.tree_hash(plugin)}
            real_client = evalmod.LiveVariant

            class OMHost(FakeHost):
                def __init__(self, argv, env, cwd):
                    super().__init__(argv, env, cwd)
                    # config/read shape from the actual Codex 0.153.4 pilot.
                    self.config['plugins'] = {'observational-memory@om-evaluation': {'enabled': True}}
                    self.config['marketplaces'] = {'om-evaluation': {
                        'source_type': 'local', 'source': str((output / 'candidate-market').resolve()),
                        'ref': None, 'last_revision': None, 'last_updated': None, 'sparse_paths': None}}

                    self.skills = [{'name': 'observational-memory:observational-memory',
                                    'pluginId': 'observational-memory@om-evaluation', 'scope': 'user', 'enabled': True}]
                    self.hooks = fake_hook_listing(self.cwd, plugin, True)['data'][0]['hooks']
                    self.config['hooks'] = {event: [] for event in ('Interrupt', 'PermissionRequest', 'PostCompact', 'PostToolUse',
                        'PreCompact', 'PreToolUse', 'SessionEnd', 'SessionStart', 'Stop', 'SubagentStart', 'SubagentStop', 'UserPromptSubmit')}
                    self.config['hooks']['state'] = {h['key']: {'trusted_hash': h['currentHash']} for h in self.hooks}

                def send(self, request):
                    super().send(request)
                    if request.get('method') != 'turn/start':
                        return
                    tid = f't{self.turn_count}'
                    prime = self.turn_count == 1 or 'Analyze new incident' in request['params']['input'][0]['text']
                    names = [('sessionStart', 'SessionStart')] if prime else []
                    names += [('userPromptSubmit', 'UserPromptSubmit'), ('postToolUse', 'PostToolUse'), ('stop', 'Stop')]
                    omit_post = getattr(self, 'omit_later_post', None) if self.turn_count > 1 else None
                    if getattr(self, 'no_tool_post', False) and not prime:
                        names.remove(('postToolUse', 'PostToolUse'))
                    data = om_home / 'plugins/data/store'
                    command = f"PATH='{data}/bin' om --store '{data}' --session '{self.thread}'"
                    records, events = [], []
                    for event, raw in names:
                        run = {'eventName': event, 'source': 'plugin', 'sourcePath': str(plugin.resolve() / 'hooks/hooks.json'),
                               'handlerType': 'command', 'executionMode': 'sync', 'status': 'completed',
                               'entries': [{'kind': 'context', 'text': 'Ledger command: ' + command + '. Review one page.'}] if event == 'sessionStart' else []}
                        events.append({'method': 'hook/completed', 'params': {'threadId': self.thread, 'turnId': tid, 'run': run}})
                        records.append({'session': self.thread, 'turn_id': tid, 'command': 'hook', 'hook': True, 'hook_event': raw,
                                        'exit_code': 0, 'hook_unavailable': False, 'input_bytes': 5, 'output_bytes': 7})
                    if prime and not getattr(self, 'omit_prime', False):
                        events.append({'method': 'item/completed', 'params': {'threadId': self.thread, 'turnId': tid,
                            'item': {'id': 'prime-' + tid, 'type': 'commandExecution', 'exitCode': 0,
                                     'aggregatedOutput': 'Session: ' + json.dumps(self.thread),
                                     'commandActions': [{'command': command + ' prime'}]}}})
                        records.append({'session': self.thread, 'command': 'prime', 'prime_valid': True,
                                        'hook': False, 'exit_code': 0, 'input_bytes': 0, 'output_bytes': 100})
                    if omit_post in ('both', 'native'):
                        events = [e for e in events if e.get('params', {}).get('run', {}).get('eventName') != 'postToolUse']
                    if omit_post in ('both', 'meter'):
                        records = [c for c in records if c.get('hook_event') != 'PostToolUse']
                    if omit_post == 'stale_native':
                        for event in events:
                            if event.get('params', {}).get('run', {}).get('eventName') == 'postToolUse':
                                event['params']['turnId'] = 't1'
                    if omit_post == 'stale_meter':
                        for record in records:
                            if record.get('hook_event') == 'PostToolUse':
                                record['turn_id'] = 't1'
                    if getattr(self, 'null_output_command', False) and prime:
                        events.append({'method': 'item/completed', 'params': {'threadId': self.thread, 'turnId': tid,
                            'item': {'id': 'silent-' + tid, 'type': 'commandExecution', 'exitCode': 0,
                                     'aggregatedOutput': None, 'commandActions': []}}})
                    self.queue[0:0] = events
                    with (data / 'calls.jsonl').open('a') as log:
                        for record in records:
                            log.write(json.dumps(record) + '\n')

            clients = []

            def make_client(cfg, variant, workspace, env, writable):
                client = real_client(cfg, variant, workspace, env, writable,
                                     FakeHost if variant.name == 'native' else OMHost)
                clients.append(client)
                if mutate:
                    mutate(variant.name, client.transport)
                return client

            def stage(cfg, run_budget):
                data = om_home / 'plugins/data/store'; (data / 'bin').mkdir(parents=True)
                (data / 'bin/om').write_text('meter')
                (data / 'bin/om-candidate').write_bytes(candidate.read_bytes())
                return {'data': str(data)}

            def activation(cfg, staged, run_budget, env):
                data = Path(staged['data'])
                return {'installed': str(plugin.resolve()), 'data': str(data.resolve()),
                        'plugin_sha256': evalmod.tree_hash(plugin), 'candidate_sha256': evalmod.digest(candidate.read_bytes()),
                        'meter_sha256': evalmod.digest((data / 'bin/om').read_bytes()),
                        'hooks': {h['key']: h['currentHash'] for h in fake_hook_listing(self.path, plugin, True)['data'][0]['hooks']}}

            def meter(data):
                path = data / 'calls.jsonl'
                path.write_text('' if error else json.dumps({'input_bytes': 5, 'output_bytes': 7, 'hook': True, 'exit_code': 0}) + '\n')
                return path

            with patch.object(evalmod, 'schema_preflight', return_value={'version': 'fake'}), \
                 patch.object(Path, 'home', return_value=self.path / 'synthetic-user'), \
                 patch.dict(evalmod.os.environ, {'CODEX_HOME': str(self.path / 'synthetic-user/.codex')}), \
                 patch.object(evalmod, 'activation_check', return_value={'verified': True, 'synthetic_usage_excluded': True}), \
                 patch.object(evalmod, 'prepare_live_activation', side_effect=activation), \
                 patch.object(evalmod, 'stage_plugin', side_effect=stage), \
                 patch.object(evalmod, 'install_meter', side_effect=meter), \
                 patch.object(evalmod, 'LiveVariant', side_effect=make_client), \
                 patch.object(evalmod.subprocess, 'Popen', side_effect=AssertionError('subprocess')):
                if error:
                    with self.assertRaisesRegex(exception_type, error) as caught:
                        evalmod.live(config, fixture, variants, budget, meta)
                    result = evalmod.output_results(config, fixture, variants, budget, meta, str(caught.exception))
                    self.assertEqual(result['status'], 'failed')
                    self.assertEqual(len(clients), 2)
                    if error == 'OM activation missing successful hook-delivered prime command':
                        self.assertGreater(result['budget']['observed_input_tokens'], 0)
                        self.assertIsNone(variants['native'].usage.totals()['inputTokens'])
                        self.assertEqual(variants['om'].workload_batches, [])
                        self.assertEqual(variants['native'].workload_batches, [])
                        self.assertFalse(result['quality']['quality_pass'])
                        self.assertFalse({'thread/start', 'turn/start'} &
                                         {r.get('method') for r in clients[0].transport.sent})
                    elif error == 'OM activation missing required native/metered lifecycle evidence':
                        self.assertGreater(variants['om'].usage.totals()['inputTokens'], 100)
                        self.assertEqual(clients[1].transport.turn_count, 2)
                        self.assertEqual(len(variants['om'].workload_batches), 1)
                        self.assertEqual(len(variants['om'].probes), 0)
                        self.assertEqual(result['budget']['observed_input_tokens'],
                                         sum(v.usage.totals()['inputTokens'] for v in variants.values()))
                        self.assertFalse(meta['om_activation']['verified'])
                        self.assertFalse(result['quality']['quality_pass'])
                    elif finalization:
                        self.assertEqual(result['budget']['observed_input_tokens'], 1080)
                        self.assertEqual([len(v.probes) for v in variants.values()], [2, 2])
                        self.assertTrue(meta['finalization_errors'])
                        self.assertEqual(meta['home_finalization']['native']['status'], 'failed')
                        self.assertEqual(meta['home_finalization']['om']['status'], 'complete')
                    else:
                        self.assertIsNone(result['budget']['observed_input_tokens'])
                        self.assertFalse(result['metadata']['om_plugin_verified'])
                        self.assertEqual(json.loads((output / 'om-calls.json').read_text()), [])
                        for v in result['variants'].values():
                            self.assertIsNone(v['usage']['inputTokens'])
                            self.assertEqual(v['compactions'], [])
                        for client in clients:
                            self.assertFalse({'thread/start', 'turn/start', 'thread/compact/start'} &
                                             {r.get('method') for r in client.transport.sent})
                else:
                    evalmod.live(config, fixture, variants, budget, meta)
                    self.assertEqual(budget.input_tokens, 1080)
                    self.assertEqual([len(v.probes) for v in variants.values()], [2, 2])
                    for client in clients:
                        self.assertIn('thread/start', [r.get('method') for r in client.transport.sent])
                if interrupted_cleanup:
                    self.assertTrue(clients[1].transport.closed)
                    self.assertTrue(meta['finalization_errors'])
                else:
                    self.assertTrue(all(c.transport.closed for c in clients))
            self.assertTrue(meta['global_config_unchanged'])
            for home in (native_home, om_home):
                self.assertEqual((home / 'auth.json').read_text(), 'private-fake-auth')
                if (finalization or interrupted_cleanup) and home == native_home:
                    with self.assertRaisesRegex(evalmod.HarnessError, 'incomplete'):
                        evalmod.validate_home(home)
                else:
                    evalmod.validate_home(home)
            if finalization or interrupted_cleanup:
                break  # Incomplete ownership must prevent another run.
        return meta

    def test_both_live_variants_share_budget_and_can_reuse_homes(self):
        self.run_matched()

    def test_missing_actual_prime_stops_before_native_work_and_retains_om_input(self):
        def mutate(name, host):
            if name == 'om':
                host.omit_prime = True
        self.run_matched(mutate, 'OM activation missing successful hook-delivered prime command')

    def test_later_tool_turn_missing_posttool_stops_and_keeps_usage(self):
        for missing in ('both', 'native', 'meter', 'stale_native', 'stale_meter'):
            with self.subTest(missing=missing):
                def mutate(name, host):
                    if name == 'om':
                        host.omit_later_post = missing
                self.run_matched(mutate, 'OM activation missing required native/metered lifecycle evidence')

    def test_no_tool_turn_does_not_require_posttool(self):
        def mutate(name, host):
            if name == 'om':
                host.no_tool_post = True
        self.run_matched(mutate)

    def test_nullable_unrelated_command_allows_recovery_with_usage_retained(self):
        def mutate(name, host):
            if name == 'om':
                host.null_output_command = True
        self.run_matched(mutate)

    def test_finalization_failure_cannot_turn_completed_work_into_success(self):
        def mutate(name, host):
            if name == 'native':
                (self.path / 'native-home/unrelated').write_text('preserve unowned state')
            else:
                (self.path / 'om-home/cache').mkdir()
        self.run_matched(mutate, 'evaluation finalization failed: native home', finalization=True)

    def test_shutdown_failure_keeps_home_incomplete_and_preserves_measurements(self):
        def mutate(name, host):
            if name == 'native':
                close = host.close
                def fail_close(*args, **kwargs):
                    close(*args, **kwargs)
                    raise OSError('shutdown verification failed')
                host.close = fail_close
        self.run_matched(mutate, 'evaluation finalization failed: native shutdown', finalization=True)

    def interrupted_shutdown(self, exception, primary=False):
        attempts = []
        def mutate(name, host):
            close = host.close
            def interrupt_close(*args, **kwargs):
                attempts.append(name)
                if name == 'native':
                    raise exception
                close(*args, **kwargs)
            host.close = interrupt_close
            if primary and name == 'om':
                host.config['marketplaces'].clear()
        meta = self.run_matched(mutate, 'marketplace registration' if primary else str(exception),
            finalization=not primary, exception_type=evalmod.HarnessError if primary else type(exception),
            interrupted_cleanup=True)
        self.assertEqual(attempts, ['native', 'om'])
        self.assertEqual(meta['home_finalization']['native']['status'], 'failed')
        self.assertEqual(meta['home_finalization']['om']['status'], 'complete')
        self.assertEqual(meta['global_config_finalization']['status'], 'complete')
        self.assertIn('native shutdown', [e['stage'] for e in meta['finalization_errors']])

    def test_keyboard_interrupt_during_shutdown_is_deferred_until_finalization(self):
        self.interrupted_shutdown(KeyboardInterrupt('cleanup interrupted'))

    def test_system_exit_during_shutdown_is_deferred_until_finalization(self):
        self.interrupted_shutdown(SystemExit(23))

    def test_primary_failure_survives_shutdown_control_flow_exception(self):
        self.interrupted_shutdown(KeyboardInterrupt('cleanup interrupted'), primary=True)

    def test_home_finalizer_interrupt_still_attempts_other_home_and_global(self):
        finish = evalmod.finish_home
        attempts = []
        def interrupt_finish(home, *args, **kwargs):
            attempts.append(home.name)
            if home.name == 'native-home':
                raise KeyboardInterrupt('home finalizer interrupted')
            return finish(home, *args, **kwargs)
        with patch.object(evalmod, 'finish_home', side_effect=interrupt_finish):
            meta = self.run_matched(error='home finalizer interrupted', finalization=True,
                                    exception_type=KeyboardInterrupt, interrupted_cleanup=True)
        self.assertEqual(attempts, ['native-home', 'om-home'])
        self.assertEqual(meta['global_config_finalization']['status'], 'complete')

    def test_configuration_drift_never_launches_a_model(self):
        controls = {
            'unrelated marketplace': lambda c: c['marketplaces'].update({'unrelated': {'source_type': 'local', 'source': '/other'}}),
            'extra plugin': lambda c: c['plugins'].update({'hook-only@other': {'enabled': True}}),
            'wrong source': lambda c: c['marketplaces']['om-evaluation'].update({'source': '/other/candidate-market'}),
            'missing marketplace': lambda c: c['marketplaces'].clear(),
            'model': lambda c: c.update({'model': 'other-model'}),
            'effort': lambda c: c.update({'model_reasoning_effort': 'low'}),
            'setting': lambda c: c.update({'allow_login_shell': False}),
            'writable roots': lambda c: c['sandbox_workspace_write']['writable_roots'].append('/other'),
        }
        for label, mutate in controls.items():
            with self.subTest(label=label):
                self.run_matched(lambda name, host: mutate(host.config) if name == 'om' else None,
                                 'registration|marketplace source|not equivalent|workspace/store permissions')

    def test_base_skill_content_drift_never_launches_a_model(self):
        def mutate(name, host):
            skill = self.path / (name + '-SKILL.md'); skill.write_text('different ' + name)
            host.skills.append({'name': 'system-skill', 'scope': 'system', 'enabled': True, 'path': str(skill)})
        self.run_matched(mutate, 'base skill/tool opportunities differ')


class InterruptDriverTests(unittest.TestCase):
    setUp = HarnessTests.setUp

    def test_interrupt_is_sent_during_owned_command_and_usage_is_retained(self):
        class InterruptHost(FakeHost):
            def send(self, request):
                if request.get('method') == 'turn/start':
                    self.sent.append(request)
                    self.queue.extend([
                        {'method': 'turn/started', 'params': {'threadId': self.thread, 'turn': {'id': 'slow'}}},
                        usage(self.thread, 100, turn='slow'),
                        {'method': 'item/started', 'params': {'threadId': self.thread, 'turnId': 'slow',
                         'item': {'id': 'cmd', 'type': 'commandExecution', 'command': 'python3 diagnostic.py'}}},
                        {'id': request['id'], 'result': {'turn': {'id': 'slow'}}}])
                elif request.get('method') == 'turn/interrupt':
                    self.sent.append(request)
                    self.queue.extend([{'id': request['id'], 'result': {}},
                        {'method': 'turn/completed', 'params': {'threadId': self.thread,
                         'turn': {'id': 'slow', 'status': 'interrupted'}}}])
                else:
                    super().send(request)
        workspace = self.path / 'workspace'; self.fixture.prepare(workspace)
        v = evalmod.VariantRun('native', self.fixture, evalmod.RunBudget(30, 1000))
        config = DriverTests.config(self)
        c = evalmod.LiveVariant(config, v, workspace, {}, [workspace], InterruptHost)
        try:
            c.preflight(); c.start(); c.turn('run diagnostic', interrupt=True)
            self.assertEqual(v.interruptions, [{'turn_id': 'slow', 'status': 'interrupted'}])
            self.assertEqual(v.budget.input_tokens, 100)
            sent = [r for r in c.transport.sent if r.get('method') == 'turn/interrupt']
            self.assertEqual(sent[0]['params'], {'threadId': 'test-thread', 'turnId': 'slow'})
        finally:
            c.close()


class ClockSafetyTests(unittest.TestCase):
    setUp = HarnessTests.setUp

    def clocks(self, seconds=1000):
        values = {'elapsed': 0., 'epoch': 10000., 'active': 0.}
        budget = evalmod.RunBudget(seconds, 1000000, lambda: values['elapsed'],
                                   epoch_clock=lambda: values['epoch'], active_clock=lambda: values['active'])
        return values, budget

    def test_epoch_jumps_do_not_change_elapsed_budget(self):
        values, budget = self.clocks()
        for epoch in (1000000., -1000000.):
            values.update(elapsed=10., active=10., epoch=epoch)
            self.assertEqual(budget.remaining(), 990.)
            budget.check()
            snapshot = budget.timing()
            self.assertEqual(snapshot['elapsed_seconds'], 10.)
            self.assertTrue(snapshot['clock_discrepancy_warning'])
            self.assertEqual(snapshot['epoch_minus_elapsed_seconds'], epoch - 10000. - 10.)

    def test_suspend_counts_toward_wall_ceiling_and_retains_received_usage(self):
        values, budget = self.clocks(seconds=100)
        v = evalmod.VariantRun('native', self.fixture, budget); v.start_thread('owned')
        def receive(timeout):
            values.update(elapsed=101., active=1., epoch=10101.)
            return usage('owned', 123)
        rpc = evalmod.RpcClient(receive, lambda e: None, budget, v.event)
        with self.assertRaisesRegex(evalmod.HarnessError, 'wall deadline'):
            rpc.pump()
        self.assertEqual(v.usage.totals()['inputTokens'], 123)
        self.assertEqual(budget.input_tokens, 123)
        self.assertEqual(budget.timing()['elapsed_minus_active_seconds'], 100.)

    def test_delayed_reply_warns_without_silence_cancellation(self):
        values = {'elapsed': 0., 'epoch': 10000., 'active': 0.}
        budget = evalmod.RunBudget(1000, 1000000, lambda: values['elapsed'])
        class SlowHost(FakeHost):
            def send(self, request):
                super().send(request)
                if request.get('method') == 'turn/start':
                    self.delay = 22
            def receive(self, timeout):
                if getattr(self, 'delay', 0):
                    self.delay -= 1
                    values['elapsed'] += 30; values['active'] += 30; values['epoch'] += 30
                    return None
                return super().receive(timeout)
        workspace = self.path / 'workspace'; self.fixture.prepare(workspace)
        v = evalmod.VariantRun('native', self.fixture, budget)
        with patch.object(evalmod.time, 'monotonic', side_effect=lambda: values['active']):
            client = evalmod.LiveVariant(DriverTests.config(self), v, workspace, {}, [workspace], SlowHost)
            try:
                client.preflight(); client.start(); client.turn('ordinary slow reply')
                status = json.loads((self.path / 'native-status.json').read_text())
                self.assertTrue(status['silence_warning_seen'])
                self.assertEqual(v.usage.totals()['inputTokens'], 100)
                self.assertNotIn('turn/interrupt', [r.get('method') for r in client.transport.sent])
            finally:
                client.close()

    def test_status_is_bounded_and_local_receipt_not_remote_epoch_driven(self):
        values, budget = self.clocks()
        workspace = self.path / 'workspace'; self.fixture.prepare(workspace)
        v = evalmod.VariantRun('native', self.fixture, budget)
        client = evalmod.LiveVariant(DriverTests.config(self), v, workspace, {}, [workspace], FakeHost)
        self.addCleanup(client.close); client.preflight(); client.start()
        values['elapsed'] = 5
        client.notify({'method': 'item/started', 'emittedAtMs': -90000000,
                       'params': {'threadId': v.thread_id, 'turnId': 'local',
                                  'item': {'id': 'cmd', 'type': 'commandExecution', 'command': 'PRIVATE' * 10000}}})
        values['elapsed'] = 10
        client.report_status(force=True)
        path = self.path / 'native-status.json'; raw = path.read_text(); status = json.loads(raw)
        self.assertEqual(status['last_event_age_seconds'], 5)
        self.assertEqual(status['pending_command_count'], 1)
        self.assertLess(len(raw.encode()), 8192)
        self.assertNotIn('PRIVATE', raw)
        self.assertFalse(list(self.path.glob('*status.json.tmp')))

    def test_owned_process_exit_and_status_write_failure_preserve_primary_error(self):
        values, budget = self.clocks()
        workspace = self.path / 'workspace'; self.fixture.prepare(workspace)
        v = evalmod.VariantRun('native', self.fixture, budget)
        client = evalmod.LiveVariant(DriverTests.config(self), v, workspace, {}, [workspace], FakeHost)
        self.addCleanup(client.close)
        from unittest.mock import Mock
        client.transport.process = Mock(); client.transport.process.poll.return_value = 23
        with self.assertRaisesRegex(evalmod.HarnessError, 'owned App Server exited'):
            client.rpc.pump()
        status = json.loads((self.path / 'native-status.json').read_text())
        self.assertEqual(status['host_exit_code'], 23)
        with patch.object(evalmod, 'write_json', side_effect=OSError('status disk denied')):
            with self.assertRaisesRegex(evalmod.HarnessError, 'owned App Server exited'):
                client.rpc.pump()

    def test_unavailable_clock_refuses_before_live_work(self):
        with patch.object(evalmod, 'clock_source', side_effect=evalmod.HarnessError('suspend-aware clock unavailable')):
            with self.assertRaisesRegex(evalmod.HarnessError, 'clock unavailable'):
                evalmod.RunBudget(100, 1000)

    def test_late_live_usage_is_retained_before_deadline_blocks_next_request(self):
        values, budget = self.clocks(seconds=100)
        workspace = self.path / 'workspace'; self.fixture.prepare(workspace)
        v = evalmod.VariantRun('native', self.fixture, budget)
        client = evalmod.LiveVariant(DriverTests.config(self), v, workspace, {}, [workspace], FakeHost)
        self.addCleanup(client.close); client.preflight(); client.start()
        def receive(timeout):
            values['elapsed'] = 101
            return usage(v.thread_id, 321)
        client.rpc.receive = receive
        sent = len(client.transport.sent)
        with self.assertRaisesRegex(evalmod.HarnessError, 'wall deadline'):
            client.rpc.pump()
        self.assertEqual(budget.input_tokens, 321)
        self.assertEqual(v.usage.totals()['inputTokens'], 321)
        self.assertIn('tokenUsage', (self.path / 'native-events.jsonl').read_text())
        with self.assertRaisesRegex(evalmod.HarnessError, 'wall deadline'):
            client.rpc.send_request('turn/start', {})
        self.assertEqual(len(client.transport.sent), sent)

    def test_exited_host_retains_buffered_usage_before_terminal_failure(self):
        from unittest.mock import Mock
        values, budget = self.clocks()
        workspace = self.path / 'workspace'; self.fixture.prepare(workspace)
        v = evalmod.VariantRun('native', self.fixture, budget)
        client = evalmod.LiveVariant(DriverTests.config(self), v, workspace, {}, [workspace], FakeHost)
        self.addCleanup(client.close); client.preflight(); client.start()
        client.transport.queue.append(usage(v.thread_id, 456))
        client.transport.process = Mock(); client.transport.process.poll.return_value = 23
        with self.assertRaisesRegex(evalmod.HarnessError, 'owned App Server exited'):
            client.rpc.pump()
        self.assertEqual(budget.input_tokens, 456)
        self.assertEqual(v.usage.totals()['inputTokens'], 456)
        self.assertIn('tokenUsage', (self.path / 'native-events.jsonl').read_text())
        status = json.loads((self.path / 'native-status.json').read_text())
        self.assertEqual(status['reported_input_tokens'], 456)
        self.assertEqual(status['host_exit_code'], 23)

    def test_status_throttles_events_and_reports_cleanup_without_masking_failure(self):
        values, budget = self.clocks()
        workspace = self.path / 'workspace'; self.fixture.prepare(workspace)
        v = evalmod.VariantRun('native', self.fixture, budget)
        client = evalmod.LiveVariant(DriverTests.config(self), v, workspace, {}, [workspace], FakeHost)
        client.preflight(); client.start()
        with patch.object(evalmod, 'write_json', wraps=evalmod.write_json) as write:
            client.report_status(force=True)
            for _ in range(100):
                client.notify({'method': 'item/agentMessage/delta', 'params': {'threadId': v.thread_id}})
                client.report_status()
            self.assertEqual(write.call_count, 1)
        with patch.object(client.transport, 'close', side_effect=RuntimeError('primary shutdown failure')), \
             patch.object(client, 'report_status', side_effect=OSError('diagnostic failure')):
            with self.assertRaisesRegex(RuntimeError, 'primary shutdown failure'):
                client.close()
        self.assertTrue(client.log.closed)
        self.assertEqual(client.status_phase, 'closing')

    def test_invalid_clock_final_diagnostics_preserve_original_failure(self):
        values, budget = self.clocks()
        values['elapsed'] = float('nan')
        with self.assertRaisesRegex(evalmod.HarnessError, 'clock invalid'):
            budget.check()
        config = DriverTests.config(self)
        variants = {name: evalmod.VariantRun(name, self.fixture, budget) for name in ('native', 'om')}
        result = evalmod.output_results(config, self.fixture, variants, budget, {}, 'original failure')
        self.assertIn('original failure', result['eligibility_reasons'])
        self.assertIsNone(result['budget']['wall_seconds'])
        self.assertIn('clock_error', result['budget']['timing'])

    def test_clock_unavailable_cli_refuses_before_output_or_live(self):
        output = self.path / 'new-output'
        with patch.object(evalmod, 'clock_source', side_effect=OSError('no supported clock')), \
             patch.object(evalmod, 'live') as live:
            self.assertEqual(evalmod.main(['--mode', 'dry-run', '--output', str(output)]), 2)
        live.assert_not_called()
        self.assertFalse(output.exists())

    def test_expired_budget_cleanup_drains_already_exited_real_host(self):
        import os
        values, budget = self.clocks(seconds=1)
        workspace = self.path / 'workspace'; self.fixture.prepare(workspace)
        v = evalmod.VariantRun('native', self.fixture, budget); v.start_thread('owned')
        emitted = json.dumps(usage('owned', 456)) + '\n'
        def factory(argv, env, cwd):
            return evalmod.ProcessTransport([sys.executable, '-c', 'import sys; sys.stdout.write(' + repr(emitted) + ')'],
                                            os.environ.copy(), cwd)
        client = evalmod.LiveVariant(DriverTests.config(self), v, workspace, {}, [workspace], factory)
        client.transport.process.wait(timeout=5)
        values['elapsed'] = 2
        with self.assertRaisesRegex(evalmod.HarnessError, 'wall deadline'):
            try:
                client.rpc.pump()
            finally:
                client.close()
        self.assertEqual(v.usage.totals()['inputTokens'], 456)
        self.assertEqual(budget.input_tokens, 456)
        self.assertIn('tokenUsage', (self.path / 'native-events.jsonl').read_text())
        self.assertIsNotNone(client.transport.process.poll())
        self.assertTrue(client.log.closed)

    def test_clock_failure_during_real_cleanup_still_reaps_owned_host(self):
        import os, threading, time
        transport = evalmod.ProcessTransport([sys.executable, '-c', 'import time; time.sleep(30)'],
                                            os.environ.copy(), self.path)
        watchdog = threading.Timer(5, transport.process.kill); watchdog.start()
        started = time.monotonic()
        try:
            with patch.object(evalmod, 'continuous_time', side_effect=evalmod.HarnessError('clock failed')):
                transport.close('owned', 'active', lambda e: None)
            self.assertIsNotNone(transport.process.poll())
            self.assertLess(time.monotonic() - started, 3)
        finally:
            watchdog.cancel()
            if transport.process.poll() is None:
                transport.process.kill(); transport.process.wait()

    def test_pending_rpc_diagnostics_allowlist_without_changing_requests(self):
        _, budget = self.clocks()
        workspace = self.path / 'workspace'; self.fixture.prepare(workspace)
        v = evalmod.VariantRun('native', self.fixture, budget)
        client = evalmod.LiveVariant(DriverTests.config(self), v, workspace, {}, [workspace], FakeHost)
        self.addCleanup(client.close)
        sent = []
        client.rpc.send = sent.append
        private_method = 'PRIVATE_RPC_LABEL_' + 'secret' * 500
        ident = client.rpc.send_request(private_method, {'content': 'PRIVATE_REQUEST_BODY'})
        client.rpc.send_request('model/list', {})
        client.report_status(force=True)
        raw = (self.path / 'native-status.json').read_text()
        self.assertNotIn('PRIVATE_RPC_LABEL', raw)
        self.assertNotIn('PRIVATE_REQUEST_BODY', raw)
        self.assertEqual(json.loads(raw)['pending_rpc_methods'], ['model/list', 'other-rpc'])
        self.assertEqual(sent[0], {'method': private_method, 'id': ident,
                                   'params': {'content': 'PRIVATE_REQUEST_BODY'}})
        client.rpc.receive = lambda timeout: {'id': ident, 'result': {'accepted': True}}
        self.assertEqual(client.rpc.wait_response(ident), {'accepted': True})


class CompactionPolicyTests(unittest.TestCase):
    setUp = HarnessTests.setUp

    def run_boundary(self, mutate=None):
        class BoundaryHost(FakeHost):
            def send(self, request):
                super().send(request)
                if request.get('method') == 'turn/start':
                    tid = 't1'
                    before = usage(self.thread, 198041, turn=tid)
                    before['params']['tokenUsage']['last'] = {'totalTokens': 198870, 'inputTokens': 198041, 'outputTokens': 829}
                    after = usage(self.thread, 198041, turn=tid)
                    after['params']['tokenUsage']['last'] = {'totalTokens': 85412, 'inputTokens': 0, 'outputTokens': 0}
                    complete = compact(self.thread, 'actual'); complete['params']['turnId'] = tid
                    events = [before, {**complete, 'method': 'item/started'}, after, complete]
                    if mutate:
                        mutate(events)
                    # The RPC reply deliberately follows notifications, as with FakeHost.
                    self.queue[1:1] = events
                    for event in self.queue[1 + len(events):]:
                        if event.get('method') == 'thread/tokenUsage/updated':
                            event['params']['tokenUsage']['total']['inputTokens'] += 198041
                    self.input += 198041
        workspace = self.path / 'workspace'; self.fixture.prepare(workspace)
        v = evalmod.VariantRun('native', self.fixture, evalmod.RunBudget(30, 1000000))
        client = evalmod.LiveVariant(DriverTests.config(self), v, workspace, {}, [workspace], BoundaryHost)
        self.addCleanup(client.close)
        client.preflight(); client.start(); client.turn('ordinary fixture work')
        return v, client

    def test_below_threshold_request_has_separate_owned_policy_and_reduction_evidence(self):
        v, client = self.run_boundary()
        evidence = v.compaction_evidence(200000)[0]
        self.assertTrue(evidence['configured_policy_compaction_verified'])
        self.assertTrue(evidence['context_reduction_verified'])
        self.assertFalse(evidence['observed_request_at_or_above_threshold'])
        self.assertEqual(evidence['expected_policy'], {'threshold': 200000, 'scope': 'total'})
        self.assertEqual(evidence['observed_effective_policy'], evidence['expected_policy'])
        self.assertEqual(evidence['before_last_total_tokens'], 198870)
        self.assertEqual(evidence['after_context_estimate_tokens'], 85412)
        self.assertIsNone(evidence['trigger_context_tokens'])
        self.assertIsNone(evidence['trigger_reason'])
        self.assertEqual(v.usage.totals()['inputTokens'], 198141)
        self.assertNotIn('thread/compact/start', [r.get('method') for r in client.transport.sent])

    def test_higher_request_still_does_not_disclose_exact_trigger(self):
        def change(events):
            events[0]['params']['tokenUsage']['last'].update(totalTokens=221895, inputTokens=221000)
        v, _ = self.run_boundary(change)
        evidence = v.compaction_evidence(200000)[0]
        self.assertTrue(evidence['observed_request_at_or_above_threshold'])
        self.assertTrue(evidence['configured_policy_compaction_verified'])
        self.assertIsNone(evidence['trigger_context_tokens'])
        self.assertIsNone(evidence['trigger_reason'])

    def test_manual_forced_replay_and_unowned_cannot_claim_configured_policy(self):
        v, client = self.run_boundary()
        client.rpc.request('thread/compact/start', {'threadId': v.thread_id})
        self.assertFalse(v.compaction_evidence(200000)[0]['configured_policy_compaction_verified'])
        v.manual_compaction_requested = False
        for change in (lambda: setattr(v, 'live_thread_id', None),
                       lambda: v.requested_turn_ids.clear(),
                       lambda: v.effective['config'].update(model_auto_compact_token_limit=199999),
                       lambda: v.effective['config'].update(model_auto_compact_token_limit_scope='body_after_prefix'),
                       lambda: v.reroutes.append({'toModel': 'other'})):
            with self.subTest(change=change):
                saved = copy.deepcopy((v.live_thread_id, v.requested_turn_ids, v.effective, v.reroutes))
                change()
                self.assertFalse(v.compaction_evidence(200000)[0]['configured_policy_compaction_verified'])
                v.live_thread_id, v.requested_turn_ids, v.effective, v.reroutes = saved
        client.config.args.force_compaction = True
        # Forced mode is an exclusion even when no manual request was yet needed.
        other = evalmod.VariantRun('native', self.fixture, v.budget)
        forced = evalmod.LiveVariant(client.config, other, client.workspace, {}, [client.workspace], FakeHost)
        self.addCleanup(forced.close)
        self.assertTrue(other.manual_compaction_requested)
        replay = evalmod.VariantRun('native', self.fixture, v.budget)
        replay.start_thread(v.thread_id); replay.compactions = copy.deepcopy(v.compactions)
        replay.effective = copy.deepcopy(v.effective)
        replay_evidence = replay.compaction_evidence(200000)[0]
        self.assertFalse(replay_evidence['configured_policy_compaction_verified'])
        self.assertIn('unverified', replay_evidence['provenance'])
        replay.effective = {}
        self.assertEqual(replay.compaction_evidence(200000)[0]['observed_effective_policy'], {'threshold': None, 'scope': None})

    def test_result_gate_uses_policy_grade_not_prior_request_threshold(self):
        v, _ = self.run_boundary()
        variants = {name: copy.deepcopy(v) for name in evalmod.VARIANTS}
        for name, variant in variants.items():
            variant.name = name
            variant.begin_probe(1, 'recovery')
            variant.finish_probe(1, 'recovery', HarnessTests.correct_answers(self),
                                 {'completed-step': {'performed': True, 'unchanged': True}})
            variant.interruptions = [{'status': 'interrupted'}]
            variant.deferral_evidence = {'verified': True}
        config = DriverTests.config(self); config.mode = 'release'; config.cycles = 1
        metadata = {'om_plugin_verified': True, 'om_activation': {'verified': True}}
        result = evalmod.output_results(config, self.fixture, variants, v.budget, metadata, None)
        self.assertTrue(result['release_pass'])  # CLI separately fixes release at 50 cycles.
        for variant in result['variants'].values():
            evidence = variant['compaction_evidence'][0]
            self.assertFalse(evidence['observed_request_at_or_above_threshold'])
            self.assertIsNone(evidence['trigger_threshold_verified'])
        variants['native'].requested_turn_ids.clear()
        result = evalmod.output_results(config, self.fixture, variants, v.budget, metadata, None)
        self.assertFalse(result['release_pass'])
        self.assertTrue(any('configured-policy' in reason for reason in result['eligibility_reasons']))

    def test_missing_wrong_or_ambiguous_lifecycle_cannot_certify_reduction(self):
        mutations = {
            'no-start': lambda e: e.pop(1),
            'wrong-completion-turn': lambda e: e[3].update(params={**e[3]['params'], 'turnId': 'other'}),
            'wrong-completion-item': lambda e: e[3].update(params={**e[3]['params'], 'item': {'id': 'other', 'type': 'contextCompaction'}}),
            'wrong-after-turn': lambda e: e[2]['params'].update(turnId='other'),
            'no-after': lambda e: e.pop(2),
            'nonreducing': lambda e: e[2]['params']['tokenUsage']['last'].update(totalTokens=198870),
            'request-after': lambda e: e[2]['params']['tokenUsage']['last'].update(inputTokens=85412),
            'two-estimates': lambda e: e.insert(3, {**copy.deepcopy(e[2]), 'params': {**copy.deepcopy(e[2]['params']), 'tokenUsage': {**e[2]['params']['tokenUsage'], 'last': {'totalTokens': 80000, 'inputTokens': 0, 'outputTokens': 0}}}}),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                # Give each independent transport its own fresh fixture/output path.
                prior = self.path; self.path = prior / name; self.path.mkdir()
                try:
                    v, _ = self.run_boundary(mutate)
                    evidence = v.compaction_evidence(200000)[0]
                    self.assertFalse(evidence['configured_policy_compaction_verified'])
                    self.assertFalse(evidence['context_reduction_verified'])
                    self.assertEqual(v.usage.totals()['inputTokens'], 198141)
                finally:
                    self.path = prior


class CorrectiveRegressionTests(unittest.TestCase):
    setUp = HarnessTests.setUp
    variant = HarnessTests.variant
    budget = HarnessTests.budget

    def context_usage(self, v, amount, context, turn='turn'):
        event = usage(v.thread_id, amount)
        event['params']['turnId'] = turn
        event['params']['tokenUsage']['last'] = {'totalTokens': context, 'inputTokens': 0, 'outputTokens': 0}
        return event

    def test_compaction_context_supports_both_notification_orders(self):
        for after_completion in (False, True):
            with self.subTest(after_completion=after_completion):
                v = self.variant(budget=self.budget(limit=1_000_000))
                v.event(usage(v.thread_id, 210000))
                event = compact(v.thread_id, 'c')
                v.event({**event, 'method': 'item/started'})
                v.event(usage(v.thread_id, 210000))  # repeated pre-compaction sample
                if after_completion:
                    v.event(event)
                    self.assertIsNone(v.compactions[0]['after_usage'])
                v.event(self.context_usage(v, 210100, 5570))
                if not after_completion:
                    v.event(event)
                cycle = v.compactions[0]
                self.assertEqual(cycle['before_usage']['total']['inputTokens'], 210000)
                self.assertEqual(cycle['after_usage']['active_context']['totalTokens'], 5570)
                self.assertNotEqual(cycle['before_usage']['observation_index'], cycle['after_usage']['observation_index'])
                self.assertTrue(v.compaction_evidence(200000)[0]['context_reduction_verified'])

    def test_conflicting_return_to_pre_context_is_ambiguous_after_post_sample(self):
        for repeated in (10000, 8000):
            with self.subTest(repeated=repeated):
                v = self.variant(budget=self.budget(limit=1000))
                before = self.context_usage(v, 100, 10000)
                v.event(before)
                event = compact(v.thread_id, 'c')
                v.event({**event, 'method': 'item/started'})
                v.event(before)  # Repeated pre-estimate alone is not a post sample.
                self.assertIsNone(v.pending_compaction['after_usage'])
                v.event(self.context_usage(v, 100, 8000))
                v.event(self.context_usage(v, 100, repeated))
                v.event(event)
                self.assertEqual(v.compactions[0]['after_usage']['active_context']['totalTokens'], 8000)
                self.assertEqual(v.compaction_evidence(200000)[0]['context_reduction_verified'], repeated == 8000)
                self.assertEqual(v.compactions[0]['context_ambiguous'], repeated != 8000)
                self.assertEqual(v.usage.totals()['inputTokens'], 100)

    def test_context_not_invented_from_request_or_lifetime_totals(self):
        v = self.variant(budget=self.budget(limit=1_000_000))
        v.event(self.context_usage(v, 900000, 3000))
        event = compact(v.thread_id, 'c'); v.event({**event, 'method': 'item/started'})
        request = usage(v.thread_id, 900100)
        request['params']['tokenUsage']['last'] = {'totalTokens': 110, 'inputTokens': 100, 'outputTokens': 10}
        v.event(request); v.event(event)
        self.assertIsNone(v.compactions[0]['after_usage'])
        self.assertFalse(v.compaction_evidence(200000)[0]['configured_policy_compaction_verified'])
        # Even real context reduction cannot prove configured-policy ownership or a trigger from lifetime usage.
        v.event(self.context_usage(v, 900100, 2000))
        self.assertTrue(v.compaction_evidence(200000)[0]['context_reduction_verified'])
        self.assertIsNone(v.compaction_evidence(200000)[0]['observed_request_at_or_above_threshold'])
        self.assertIsNone(v.compaction_evidence(200000)[0]['trigger_context_tokens'])
        self.assertFalse(v.compaction_evidence(200000)[0]['configured_policy_compaction_verified'])

    def test_post_compaction_sample_cannot_cross_work_or_turn_boundaries(self):
        for boundary in ('agentMessage', 'new-compaction', 'wrong-turn'):
            v = self.variant(budget=self.budget(limit=1_000_000))
            v.event(usage(v.thread_id, 210000))
            event = compact(v.thread_id, 'c'); v.event({**event, 'method': 'item/started'}); v.event(event)
            if boundary == 'agentMessage':
                v.event(compact(v.thread_id, 'm', 'agentMessage'))
            elif boundary == 'new-compaction':
                v.event({**compact(v.thread_id, 'next'), 'method': 'item/started'})
            v.event(self.context_usage(v, 210100, 5000, 'foreign' if boundary == 'wrong-turn' else 'turn'))
            self.assertIsNone(v.compactions[0]['after_usage'], boundary)

    def test_final_duplicate_and_wrong_turn_usage_do_not_pay_for_final_work(self):
        v = self.variant()
        v.event(usage(v.thread_id, 100))
        v.event(compact(v.thread_id, 'm', 'agentMessage'))
        v.event(usage(v.thread_id, 100))
        with self.assertRaisesRegex(evalmod.HarnessError, 'usage'):
            v.require_usage_since(0, 'turn')
        wrong = usage(v.thread_id, 200); wrong['params']['turnId'] = 'other'; v.event(wrong)
        with self.assertRaisesRegex(evalmod.HarnessError, 'usage'):
            v.require_usage_since(0, 'turn')
        v.event(usage(v.thread_id, 300)); v.require_usage_since(0, 'turn')

    def test_actual_python_compilation_and_equivalent_execution_with_undo(self):
        import subprocess
        for arguments, repeats in ((['-m', 'py_compile', 'actions/migrate.py'], False),
                                   (['actions//migrate.py'], True),
                                   (['actions/../actions/migrate.py'], True),
                                   (['-B', '--', './actions/migrate.py'], True)):
            with self.subTest(arguments=arguments):
                workspace = self.path / str(len(list(self.path.iterdir())))
                self.fixture.prepare(workspace)
                record = workspace / 'state/migrations.txt'
                case = next(c for c in self.fixture.cases if c['id'] == 'completed-step')
                record = workspace / case['effect']['unchanged']
                original = record.read_bytes()
                prior = evalmod.action_evidence(self.fixture, workspace, 1, before=True)
                subprocess.run([sys.executable, *arguments], cwd=workspace, check=True, capture_output=True)
                self.assertEqual(record.read_bytes() != original, repeats)
                record.write_bytes(original)
                target = workspace / case['effect']['path'].format(cycle=1)
                target.parent.mkdir(parents=True, exist_ok=True); target.write_text(json.dumps(case['effect']['content']))
                v = self.variant(); v.event(compact(v.thread_id, 'c')); v.begin_probe(1, 'p')
                import shlex
                v.event({'method': 'item/completed', 'params': {'threadId': v.thread_id, 'turnId': 'p',
                    'item': {'id': 'cmd', 'type': 'commandExecution', 'cwd': str(workspace),
                             'command': shlex.join([sys.executable, *arguments])}}})
                answers = {c['id']: {'answer': c.get('expected', ''), 'action': c.get('expected_action', 'none'),
                                     **({'source': c['source']} if 'source' in c else {})} for c in self.fixture.cases}
                v.finish_probe(1, 'p', {'answers': answers}, evalmod.action_evidence(self.fixture, workspace, 1, prior=prior))
                self.assertEqual(v.scorecard()['repeated_completed_actions'], int(repeats))
                self.assertEqual(v.scorecard()['critical_failures'], int(repeats))

    def test_real_transport_blocked_stdin_obeys_shared_deadline(self):
        import os, threading, time
        t = evalmod.ProcessTransport([sys.executable, '-c', 'import time; time.sleep(30)'], os.environ.copy(), self.path)
        # Watchdog bounds the regression itself on a broken implementation.
        watchdog = threading.Timer(2, t.process.kill); watchdog.start()
        started = time.monotonic()
        try:
            t.budget = evalmod.RunBudget(0.15, 1000)
            rpc = evalmod.RpcClient(t.receive, t.send, t.budget, lambda e: None)
            with self.assertRaisesRegex(evalmod.HarnessError, 'deadline'):
                rpc.request('turn/start', {'text': 'x' * 2_000_000})
            self.assertLess(time.monotonic() - started, 1)
        finally:
            watchdog.cancel(); t.process.kill(); t.close()

    def test_real_transport_noisy_stderr_cannot_extend_receive(self):
        import os, threading, time
        t = evalmod.ProcessTransport([sys.executable, '-c', 'import os\nwhile True: os.write(2, b"x"*4096)'], os.environ.copy(), self.path)
        watchdog = threading.Timer(2, t.process.kill); watchdog.start()
        started = time.monotonic()
        try:
            self.assertIsNone(t.receive(0.15))
            self.assertLess(time.monotonic() - started, 1)
        finally:
            watchdog.cancel(); t.process.kill(); t.close()

    def test_real_transport_shutdown_reaches_kill_with_full_stdin(self):
        import os, threading, time
        t = evalmod.ProcessTransport([sys.executable, '-c',
             'import os,signal\nsignal.signal(signal.SIGTERM, signal.SIG_IGN)\nwhile True: os.write(2,b"x"*4096)'], os.environ.copy(), self.path)
        os.set_blocking(t.process.stdin.fileno(), False)
        try:
            while True: os.write(t.process.stdin.fileno(), b'x' * 4096)
        except BlockingIOError:
            pass
        watchdog = threading.Timer(6, t.process.kill); watchdog.start()
        started = time.monotonic()
        try:
            t.close('owned', 'active')
            self.assertLess(time.monotonic() - started, 4.5)
            self.assertIsNotNone(t.process.poll())
        finally:
            watchdog.cancel()
            if t.process.poll() is None: t.process.kill(); t.process.wait()


    def test_installed_host_compaction_request_then_context_then_final_order(self):
        v = self.variant(budget=self.budget(limit=1_000_000))
        v.event(usage(v.thread_id, 210000, turn='previous', outputTokens=10))
        event = compact(v.thread_id, 'c'); v.event({**event, 'method': 'item/started'})
        v.event(usage(v.thread_id, 210000, outputTokens=10))
        request = usage(v.thread_id, 210100, outputTokens=20)
        request['params']['tokenUsage']['last'] = {'totalTokens': 110, 'inputTokens': 100, 'outputTokens': 10}
        v.event(request)
        estimate = self.context_usage(v, 210100, 5570)
        estimate['params']['tokenUsage']['total']['outputTokens'] = 20
        v.event(estimate); v.event(event)
        v.event(compact(v.thread_id, 'final', 'agentMessage'))
        v.event(estimate)  # duplicate compaction usage does not cover final work
        with self.assertRaisesRegex(evalmod.HarnessError, 'usage'):
            v.require_usage_since(1, 'turn')
        final = usage(v.thread_id, 210200, outputTokens=30)
        final['params']['tokenUsage']['last'] = {'totalTokens': 110, 'inputTokens': 100, 'outputTokens': 10}
        v.event(final); v.require_usage_since(1, 'turn')
        self.assertEqual(v.compactions[0]['after_usage']['active_context']['totalTokens'], 5570)
        self.assertEqual(v.usage.totals()['inputTokens'], 210200)
        self.assertEqual(v.usage.totals()['outputTokens'], 30)
        self.assertTrue(v.compaction_evidence(200000)[0]['context_reduction_verified'])

    def test_duplicate_final_usage_stops_driver_before_next_turn(self):
        class DuplicateHost(FakeHost):
            def send(self, request):
                super().send(request)
                if request.get('method') == 'turn/start':
                    # Intermediate response spent 50 input tokens; final snapshot
                    # repeats those 50 rather than reporting the outstanding work.
                    for event in self.queue:
                        if event.get('method') == 'thread/tokenUsage/updated':
                            event['params']['tokenUsage']['total']['inputTokens'] = 50
        workspace = self.path / 'workspace'; self.fixture.prepare(workspace)
        v = evalmod.VariantRun('native', self.fixture, evalmod.RunBudget(30, 1000))
        client = evalmod.LiveVariant(DriverTests.config(self), v, workspace, {}, [workspace], DuplicateHost)
        try:
            client.preflight()
            with self.assertRaisesRegex(evalmod.HarnessError, 'usage'):
                client.drive()
            self.assertEqual(sum(r.get('method') == 'turn/start' for r in client.transport.sent), 1)
        finally:
            client.close()

    def test_duplicate_context_snapshot_is_unavailable(self):
        v = self.variant(budget=self.budget(limit=1_000_000))
        sample = self.context_usage(v, 210000, 210010)
        v.event(sample)
        event = compact(v.thread_id, 'c'); v.event({**event, 'method': 'item/started'})
        v.event(sample); v.event(event)
        self.assertIsNone(v.compactions[0]['after_usage'])
        self.assertFalse(v.compaction_evidence(200000)[0]['configured_policy_compaction_verified'])

    def test_ordinary_python_script_selection_and_paths(self):
        for command in ('python3 -m py_compile actions/migrate.py', 'python3 -c "print(1)" actions/migrate.py',
                        'python3 other.py actions/migrate.py', 'python3 -W actions/migrate.py other.py',
                        'python3 notactions/migrate.py'):
            self.assertFalse(evalmod.executes_script(command, 'actions/migrate.py'), command)
        for command in ('python3 -m actions.migrate', 'python3 -B actions//migrate.py',
                        'cd actions && python3 migrate.py',
                        "zsh -lc 'python3 actions/../actions/migrate.py'",
                        "python3 -c 'import runpy; runpy.run_path(\"actions//migrate.py\")'"):
            self.assertTrue(evalmod.executes_script(command, 'actions/migrate.py'), command)


    def test_missing_context_cannot_certify_otherwise_complete_release(self):
        native, om = self.variant(), self.variant('om')
        for variant in (native, om):
            for i in range(1, 51):
                variant.event(compact(variant.thread_id, str(i)))
                variant.begin_probe(i, 'p' + str(i))
                variant.finish_probe(i, 'p' + str(i), HarnessTests.correct_answers(self),
                                     {'completed-step': {'performed': True, 'unchanged': True}})
            variant.interruptions = [{'status': 'interrupted'}]
        om.deferral_evidence = {'verified': True}
        config = DriverTests.config(self); config.mode = 'release'; config.cycles = 50
        result = evalmod.output_results(config, self.fixture, {'native': native, 'om': om},
                                       self.budget(), {'om_plugin_verified': True, 'om_activation': {'verified': True}}, None)
        self.assertTrue(result['quality']['quality_pass'])
        self.assertFalse(result['release_pass'])
        self.assertEqual(len(result['eligibility_reasons']), 2)
        self.assertTrue(all('configured-policy compaction/context-reduction' in reason for reason in result['eligibility_reasons']))

    def test_script_execution_in_explicit_subdirectory(self):
        workspace = self.path / 'workspace'; self.fixture.prepare(workspace)
        v = self.variant(); v.workspace = workspace
        v.event({'method': 'item/started', 'params': {'threadId': v.thread_id, 'turnId': 'work',
            'item': {'id': 'cmd', 'type': 'commandExecution', 'cwd': str(workspace / 'actions'),
                     'command': 'python3 migrate.py'}}})
        self.assertEqual(v.scorecard()['repeated_completed_actions'], 1)


class StagedCandidateTests(unittest.TestCase):
    """Real candidate executable, metered commands and public ledger effects."""
    @classmethod
    def setUpClass(cls):
        import os, subprocess
        cls.build = tempfile.TemporaryDirectory(prefix='om-eval-candidate-')
        cls.candidate = Path(cls.build.name) / 'om'
        supplied = os.environ.get('OM_EVAL_TEST_BINARY')
        if supplied:
            import shutil
            shutil.copy2(supplied, cls.candidate)
        else:
            subprocess.run(['go', 'build', '-ldflags',
                '-X github.com/sentiolabs/observational-memory/internal/cli.Version=0.4.0-eval-test',
                '-o', str(cls.candidate), './cmd/om'], cwd=ROOT, check=True, capture_output=True, timeout=120)

    @classmethod
    def tearDownClass(cls):
        cls.build.cleanup()

    def setUp(self):
        import shutil
        self.temp = tempfile.TemporaryDirectory(prefix="om staging '雪 ")
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        self.data = self.path / "data 'λ"
        (self.data / 'bin').mkdir(parents=True)
        shutil.copy2(self.candidate, self.data / 'bin/om')
        self.meter = evalmod.install_meter(self.data)
        self.assertEqual((self.data / 'bin/om-candidate').read_bytes(), self.candidate.read_bytes())
        self.session = "owned '雪 session"
        self.fixture = evalmod.Fixture.load(ROOT / 'eval/fixtures/continuity.json', 'release')
        self.log = self.fixture.workload['files']['evidence/build.log']
        self.base = [str(self.data / 'bin/om'), '--store', str(self.data), '--session', self.session]

    def call(self, args, payload=None, shell=False):
        import subprocess
        result = subprocess.run(args, input=json.dumps(payload, ensure_ascii=False).encode() if payload is not None else b'',
                                shell=shell, capture_output=True, check=True, timeout=10)
        self.assertLessEqual(len(result.stdout), 12000)
        return result.stdout

    def hook(self, name, **fields):
        return json.loads(self.call(self.base + ['hook', '--client', 'codex'],
            {'hook_event_name': name, 'session_id': self.session, **fields}))

    def command(self, response):
        context = response.get('reason') or response['hookSpecificOutput']['additionalContext']
        return context.split('Ledger command: ', 1)[1].split('. Review at most', 1)[0]

    def calls(self):
        return [json.loads(line) for line in self.meter.read_text().splitlines()]

    def capture(self, mode, text=None, incomplete=False):
        text = self.log if text is None else text
        if mode == 'native':
            self.hook('PostToolUse', turn_id='root', tool_use_id='build-log', tool_name='Bash',
                      tool_input={'command': 'cat evidence/build.log'}, tool_response=text, source_incomplete=incomplete)
        else:
            self.call(self.base + ['capture'], {'kind': 'tool', 'text': text, 'key': 'manual-log',
                                               'source_incomplete': incomplete})
        page = json.loads(self.call(self.base + ['pending']))
        return page, page['page']['items'][0]['source_id']

    def defer(self, command, page, source):
        return self.call(command + ' apply', {'expected_through': page['through'], 'expected_revision': page['revision'],
            'acknowledge': [], 'defer_sources': [{'source_id': source, 'reason': 'Retain full log for later exact recovery.'}]}, shell=True)

    def audit(self):
        from types import SimpleNamespace
        variant = evalmod.VariantRun('om', self.fixture, evalmod.RunBudget(30, 1000)); variant.start_thread(self.session)
        client = SimpleNamespace(variant=variant, data=self.data,
                                 config=SimpleNamespace(args=SimpleNamespace(om_home=self.path / 'home')))
        evalmod.verify_deferrals(client)
        return variant.deferral_evidence

    def test_healthy_guidance_and_structural_unavailable_advisory(self):
        healthy = self.hook('SessionStart', source='startup')
        self.assertIn('If unavailable, skip memory', healthy['hookSpecificOutput']['additionalContext'])
        self.assertFalse(self.calls()[-1]['hook_unavailable'])
        blocked = self.path / 'not-a-directory'; blocked.write_text('owned test fixture')
        advisory = json.loads(self.call([str(self.data / 'bin/om'), '--store', str(blocked), 'hook', '--client', 'codex'],
                                       {'hook_event_name': 'SessionStart', 'session_id': self.session, 'source': 'startup'}))
        self.assertIn('systemMessage', advisory)
        self.assertTrue(self.calls()[-1]['hook_unavailable'])

    def test_exact_advertised_prime_pending_apply_are_metered(self):
        command = self.command(self.hook('SessionStart', source='startup'))
        count = len(self.calls()); prime = self.call(command + ' prime', shell=True)
        self.assertEqual(len(self.calls()), count + 1)
        self.assertEqual(self.calls()[-1]['output_bytes'], len(prime))
        self.assertEqual(self.calls()[-1]['session'], self.session)
        page, source = self.capture('native')
        count = len(self.calls()); page = json.loads(self.call(command + ' pending', shell=True))
        self.assertEqual(len(self.calls()), count + 1)
        self.defer(command, page, source)
        self.assertIn(source, self.calls()[-1]['deferrals'])
        self.assertGreater(self.calls()[-1]['input_bytes'], 0)
        self.assertTrue(self.audit()['verified'])

    def test_native_and_raw_complete_sources_are_accepted(self):
        command = self.command(self.hook('SessionStart', source='startup'))
        for mode in ('native', 'raw'):
            with self.subTest(mode=mode):
                self.session = mode + " '雪"
                self.base[-1] = self.session
                command = self.command(self.hook('SessionStart', source='startup'))
                page, source = self.capture(mode)
                self.defer(command, page, source)
                evidence = self.audit()
                self.assertTrue(evidence['verified'])
                self.assertEqual(evidence['sources'][0]['source_id'], source)

    def test_truncated_wrong_and_incomplete_sources_are_rejected(self):
        for name, text, incomplete in (('truncated', self.log[:10000], False),
                                       ('wrong', self.log.replace('E_PIPE_742', 'E_PIPE_WRONG'), False),
                                       ('incomplete', self.log, True),
                                       ('contains-only', 'unrelated prefix\n' + self.log, False)):
            with self.subTest(name=name):
                self.session = name; self.base[-1] = name
                command = self.command(self.hook('SessionStart', source='startup'))
                page, source = self.capture('native', text, incomplete)
                self.defer(command, page, source)
                with self.assertRaisesRegex(evalmod.HarnessError, 'deferred'):
                    self.audit()

    def test_deferred_native_recall_rejects_wrong_source_identity(self):
        command = self.command(self.hook('SessionStart', source='startup'))
        page, source = self.capture('native'); self.defer(command, page, source)
        actual = evalmod.run_local
        def wrong_source(*args, **kwargs):
            page = json.loads(actual(*args, **kwargs))
            for item in page['page']['items']:
                if item.get('evidence'):
                    item['evidence']['source_id'] = 's-unrelated'
            return json.dumps(page)
        with patch.object(evalmod, 'run_local', side_effect=wrong_source):
            with self.assertRaisesRegex(evalmod.HarnessError, 'deferred'):
                self.audit()

    def test_self_commands_and_exact_stop_continuation_are_preserved(self):
        command = self.command(self.hook('SessionStart', source='startup'))
        self.hook('UserPromptSubmit', turn_id='root', prompt='Do the synthetic build.')
        status = json.loads(self.call(self.base + ['status']))
        self.hook('PostToolUse', turn_id='root', tool_use_id='self', tool_name='Bash',
                  tool_input={'command': command + ' prime'}, tool_response=self.call(command + ' prime', shell=True).decode())
        self.assertEqual(json.loads(self.call(self.base + ['status']))['source_count'], status['source_count'])
        self.hook('PostToolUse', turn_id='root', tool_use_id='large', tool_name='Bash',
                  tool_input={'command': 'cat evidence/build.log'}, tool_response=self.log)
        stop = self.hook('Stop', turn_id='root', last_assistant_message='Initial final.')
        self.assertEqual(stop['decision'], 'block')
        command = self.command(stop)
        before = json.loads(self.call(self.base + ['status']))
        self.hook('UserPromptSubmit', turn_id='continuation', prompt=stop['reason'])
        self.assertEqual(json.loads(self.call(self.base + ['status']))['source_count'], before['source_count'])
        self.call(command + ' pending', shell=True)
        after = self.hook('Stop', turn_id='continuation', last_assistant_message='Continuation final.')
        self.assertNotIn('decision', after)
        self.assertEqual(json.loads(self.call(self.base + ['status']))['source_count'], before['source_count'] + 1)
        # Similar user prose is ordinary evidence, not mapped to a synthetic turn.
        normal = 'Discuss this:\n' + stop['reason']
        self.hook('UserPromptSubmit', turn_id='real-next', prompt=normal)
        self.assertFalse(self.calls()[-1]['owned_prompt_restored'])
        self.assertEqual(json.loads(self.call(self.base + ['status']))['source_count'], before['source_count'] + 2)
        self.assertEqual(json.loads(self.call(self.base + ['status']))['oldest_pending_age_turns'], before['oldest_pending_age_turns'])
        cursor, captured = None, {}
        while True:
            args = self.base + ['pending'] + (['--cursor', cursor] if cursor else [])
            page = json.loads(self.call(args))['page']
            for unit in page['items']:
                if unit['kind'] == 'user':
                    captured.setdefault(unit['source_id'], []).append(unit['text'])
            cursor = page.get('next_cursor')
            if not cursor:
                break
        self.assertIn(normal, [''.join(parts) for parts in captured.values()])
        # The same emitted reason in another session is not our owned continuation.
        self.session = 'other-session'; self.base[-1] = self.session
        self.hook('UserPromptSubmit', turn_id='other-root', prompt=stop['reason'])
        self.assertFalse(self.calls()[-1]['owned_prompt_restored'])
        pending = json.loads(self.call(self.base + ['pending']))['page']['items']
        self.assertEqual(''.join(u['text'] for u in pending), stop['reason'])


if __name__ == '__main__':
    unittest.main()
