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


def usage(thread, amount, **fields):
    return {'method': 'thread/tokenUsage/updated', 'params': {'threadId': thread, 'turnId': 'turn',
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
            v.finish_probe(i, f'p{i}', answers)

    def test_missing_cycles_single_failure_and_tie(self):
        n, o = self.variant(), self.variant('om')
        self.score_cycles(n)
        self.score_cycles(o, 49)
        self.assertFalse(evalmod.quality(n, o)['quality_pass'])
        o.event(compact(o.thread_id, 'c50')); o.begin_probe(50, 'p50')
        o.commands.append({'turn_id': 'p50', 'item': {'command': 'python3 actions/audit.py', 'exitCode': 0}})
        o.finish_probe(50, 'p50', self.correct_answers())
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


if __name__ == '__main__':
    unittest.main()
