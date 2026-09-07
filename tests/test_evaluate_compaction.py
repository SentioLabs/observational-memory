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
        self.config.update({'compact_prompt': None, 'model_context_window': None})

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
            result = {'data': [{'skills': [], 'errors': []}]}
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

class MatchedRunTests(unittest.TestCase):
    setUp = HarnessTests.setUp

    def test_both_live_variants_share_budget_and_can_reuse_homes(self):
        import argparse
        fixture = evalmod.Fixture.load(ROOT / 'eval/fixtures/continuity.json', 'pilot')
        native_home, om_home = self.path / 'native-home', self.path / 'om-home'
        native_home.mkdir(); om_home.mkdir()
        for home in (native_home, om_home):
            (home / 'auth.json').write_text('private-fake-auth')
        candidate = self.path / 'candidate'; candidate.write_text('candidate bytes')
        plugin = self.path / 'plugin'; plugin.mkdir(); (plugin / 'identity').write_text('plugin bytes')
        args = argparse.Namespace(native_home=native_home, om_home=om_home, codex='unused', om_binary=candidate,
                                  plugin_root=plugin, model='test-model', reasoning='high', force_compaction=False)
        for run_name in ('pilot-one', 'pilot-two'):
            output = self.path / run_name; output.mkdir()
            config = evalmod.RunConfig('pilot', output, 2, 200000, 'total', 'pilot', args)
            for name in evalmod.VARIANTS:
                fixture.prepare(output / name / 'workspace')
            budget = evalmod.RunBudget(30, 10000)
            variants = {n: evalmod.VariantRun(n, fixture, budget) for n in evalmod.VARIANTS}
            meta = {'om_binary_sha256': evalmod.digest(candidate.read_bytes()), 'plugin_sha256': evalmod.tree_hash(plugin)}
            real_client = evalmod.LiveVariant

            class OMHost(FakeHost):
                def send(self, request):
                    if request.get('method') == 'skills/list':
                        self.sent.append(request)
                        self.queue.append({'id': request['id'], 'result': {'data': [{'errors': [], 'skills': [{
                            'name': 'observational-memory:observational-memory', 'pluginId': 'observational-memory@om-evaluation',
                            'scope': 'user', 'enabled': True}]}]}})
                    else:
                        super().send(request)

            def make_client(cfg, variant, workspace, env, writable):
                return real_client(cfg, variant, workspace, env, writable,
                                   FakeHost if variant.name == 'native' else OMHost)

            def stage(cfg, run_budget):
                data = om_home / 'plugins/data/store'; (data / 'bin').mkdir(parents=True)
                (data / 'bin/om').write_text('meter')
                return {'data': str(data)}

            def meter(data):
                path = data / 'calls.jsonl'
                path.write_text(json.dumps({'input_bytes': 5, 'output_bytes': 7, 'hook': True, 'exit_code': 0}) + '\n')
                return path

            with patch.object(evalmod, 'schema_preflight', return_value={'version': 'fake'}), \
                 patch.object(evalmod, 'stage_plugin', side_effect=stage), \
                 patch.object(evalmod, 'install_meter', side_effect=meter), \
                 patch.object(evalmod, 'LiveVariant', side_effect=make_client), \
                 patch.object(evalmod.subprocess, 'Popen', side_effect=AssertionError('subprocess')):
                evalmod.live(config, fixture, variants, budget, meta)
            self.assertEqual(budget.input_tokens, 1080)
            self.assertTrue(meta['global_config_unchanged'])
            self.assertEqual([len(v.probes) for v in variants.values()], [2, 2])
            for home in (native_home, om_home):
                self.assertEqual((home / 'auth.json').read_text(), 'private-fake-auth')
                evalmod.validate_home(home)


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
                self.assertTrue(v.compaction_evidence(200000)[0]['verified'])

    def test_context_not_invented_from_request_or_lifetime_totals(self):
        v = self.variant(budget=self.budget(limit=1_000_000))
        v.event(self.context_usage(v, 900000, 3000))
        event = compact(v.thread_id, 'c'); v.event({**event, 'method': 'item/started'})
        request = usage(v.thread_id, 900100)
        request['params']['tokenUsage']['last'] = {'totalTokens': 110, 'inputTokens': 100, 'outputTokens': 10}
        v.event(request); v.event(event)
        self.assertIsNone(v.compactions[0]['after_usage'])
        self.assertFalse(v.compaction_evidence(200000)[0]['verified'])
        # Even real context reduction cannot prove the 200K threshold from 900K lifetime usage.
        v.event(self.context_usage(v, 900100, 2000))
        self.assertFalse(v.compaction_evidence(200000)[0]['verified'])

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
        self.assertTrue(v.compaction_evidence(200000)[0]['verified'])

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
        self.assertFalse(v.compaction_evidence(200000)[0]['verified'])

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
                                       self.budget(), {'om_plugin_verified': True}, None)
        self.assertTrue(result['quality']['quality_pass'])
        self.assertFalse(result['release_pass'])
        self.assertEqual(len(result['eligibility_reasons']), 2)
        self.assertTrue(all('context/threshold' in reason for reason in result['eligibility_reasons']))

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
