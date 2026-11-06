import hashlib
import importlib.util
import json
import os
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location('gen_config', os.path.join(os.path.dirname(__file__), 'gen_config.py'))
g = importlib.util.module_from_spec(spec)
spec.loader.exec_module(g)


def write_profile(path, names, rules=None):
    with open(path, 'w') as f:
        g.yaml.safe_dump({
            'proxies': [{'name': n, 'type': 'http', 'server': 'example.invalid', 'port': 80} for n in names],
            'rules': rules or [],
        }, f)


class GeneratorTest(unittest.TestCase):
    def test_empty_sources_build_without_private_defaults(self):
        with tempfile.TemporaryDirectory() as d:
            settings = os.path.join(d, 'settings.json')
            config = os.path.join(d, 'config.yaml')
            with open(settings, 'w') as f:
                json.dump({'sources': []}, f)
            with mock.patch.multiple(g, MIHOMO_DIR=d, SRHOP_DIR=d, CONFIG_PATH=config, SRHOP_CONFIG=settings), mock.patch.dict(os.environ, {'SRHOP_SOURCE_PROFILES': '[]'}, clear=True), mock.patch.object(g.subprocess, 'run', return_value=mock.Mock(returncode=0)):
                g.build()
            with open(config) as f:
                cfg = g.yaml.safe_load(f)
            self.assertEqual([p['name'] for p in cfg['proxies']], ['FALLBACK-33210'])
            self.assertTrue(cfg['rules'])

    def test_per_source_include_exclude_filters(self):
        nodes = {"proxies": [
            {"name": "JP Tokyo", "type": "ss", "server": "a", "port": 1},
            {"name": "jp Osaka HOME", "type": "ss", "server": "b", "port": 2},
            {"name": "US", "type": "ss", "server": "c", "port": 3},
        ]}
        jp, other = g.take(nodes, "source", include=["JP"], exclude=["home"])
        self.assertEqual(len(jp), 1)
        self.assertEqual(len(other), 0)
        jp, other = g.take(nodes, "source", include=[], exclude=["HOME"])
        self.assertEqual((len(jp), len(other)), (1, 1))
    def test_office91_requires_explicit_module_opt_in(self):
        settings = {'sources': [{'id': 'office91', 'label': 'Office', 'type': 'subscription', 'enabled': True},
                                {'id': 'xlinker', 'label': 'XLinker', 'type': 'vpn', 'enabled': True},
                                {'id': 'other', 'label': 'Other', 'type': 'subscription', 'enabled': True}]}
        with mock.patch.object(g, 'discover_flclash_profiles', create=True):
            active, metadata = g.resolve_sources(settings)
            self.assertEqual([s['id'] for s in active], ['other'])
            self.assertEqual([s['id'] for s in metadata], ['other'])
            settings['office91_enabled'] = True
            active, _ = g.resolve_sources(settings)
            self.assertEqual([s['id'] for s in active], ['office91', 'other'])
    def setUp(self):
        self.fixture_home = self.enterContext(tempfile.TemporaryDirectory())
        self.enterContext(mock.patch.object(g, 'FLCLASH_DIR', os.path.join(self.fixture_home, 'flclash')))

    def test_ai_rules_precede_source_rules_without_routing_shared_domains(self):
        with tempfile.TemporaryDirectory() as d:
            source = os.path.join(d, 'source.yaml')
            write_profile(source, ['Japan'], ['DOMAIN-SUFFIX,openai.com,DIRECT', 'MATCH,DIRECT'])
            config = os.path.join(d, 'config.yaml')
            settings = os.path.join(d, 'settings.json')
            with open(settings, 'w') as f:
                json.dump({'source_profiles': [source]}, f)
            with mock.patch.multiple(g, MIHOMO_DIR=d, SRHOP_DIR=d, CONFIG_PATH=config, SRHOP_CONFIG=settings), mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(g.subprocess, 'run', return_value=mock.Mock(returncode=0)):
                g.build()
            with open(config) as f:
                rules = g.yaml.safe_load(f)['rules']
            expected = [
                'DOMAIN-SUFFIX,openai.com,AI主代理',
                'DOMAIN-SUFFIX,chatgpt.com,AI主代理',
                'DOMAIN-SUFFIX,oaistatic.com,AI主代理',
                'DOMAIN-SUFFIX,oaiusercontent.com,AI主代理',
                'DOMAIN-SUFFIX,openaiapi-site.azureedge.net,AI主代理',
            ]
            self.assertEqual(rules[:len(expected)], expected)
            self.assertEqual(rules[len(expected):], ['DOMAIN-SUFFIX,openai.com,DIRECT', 'MATCH,主代理'])

            def ai_matches(host):
                return any(host == domain or host.endswith('.' + domain)
                           for domain in (rule.split(',')[1] for rule in expected))

            for host in ('openai.com', 'api.openai.com', 'auth.openai.com',
                         'chat.openai.com', 'chatgpt.com', 'cdn.oaistatic.com',
                         'files.oaiusercontent.com', 'openaiapi-site.azureedge.net'):
                with self.subTest(host=host):
                    self.assertTrue(ai_matches(host))
            for host in ('notopenai.com', 'openai.com.example.org', 'auth0.com',
                         'login.microsoftonline.com', 'other.azureedge.net', 'azureedge.net'):
                with self.subTest(host=host):
                    self.assertFalse(ai_matches(host))

    def test_sources_groups_chains_and_labels_contract(self):
        with tempfile.TemporaryDirectory() as d:
            big = os.path.join(d, 'big.yaml')
            ktm = os.path.join(d, 'ktm.yaml')
            write_profile(big, ['日本 one', '香港 two'], ['DOMAIN-SUFFIX,from-big.example,直接', 'MATCH,DIRECT'])
            write_profile(ktm, ['东京 three'], ['DOMAIN-SUFFIX,from-ktm.example,直接', 'MATCH,DIRECT'])
            sub = os.path.join(d, 'sources', 'office91.yaml')
            os.makedirs(os.path.dirname(sub))
            write_profile(sub, ['日本 four'])
            config = os.path.join(d, 'config.yaml')
            settings = os.path.join(d, 'settings.json')
            with open(settings, 'w') as f:
                json.dump({
                    'office91_enabled': True,
                    'sources': [
                        {'id': 'big-airport', 'label': '大机场 Big Airport', 'type': 'profile', 'path': big, 'enabled': True},
                        {'id': 'ktm-cloud', 'label': 'KTM Cloud', 'type': 'profile', 'path': ktm, 'enabled': True},
                        {'id': 'office91', 'label': '玖壹 Office91', 'type': 'subscription', 'enabled': True},
                        {'id': 'xlinker', 'label': 'XLinker · 速联', 'type': 'vpn', 'enabled': True},
                        {'id': 'disabled-one', 'label': 'Disabled', 'type': 'profile', 'path': os.path.join(d, 'missing.yaml'), 'enabled': False},
                    ],
                    'base_rules_source': 'ktm-cloud',
                }, f)
            with mock.patch.multiple(g, MIHOMO_DIR=d, SRHOP_DIR=d, CONFIG_PATH=config, SRHOP_CONFIG=settings), mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(g.subprocess, 'run', return_value=mock.Mock(returncode=0)):
                g.build()
            with open(config) as f:
                cfg = g.yaml.safe_load(f)
            groups = {x['name']: x for x in cfg['proxy-groups']}
            # Per-source url-test groups in priority order; vpn and disabled
            # sources produce none.
            self.assertEqual([n for n in groups if n.startswith('JP-') and n != 'JP-ALL'],
                             ['JP-big-airport', 'JP-ktm-cloud', 'JP-office91'])
            self.assertNotIn('JP-xlinker', groups)
            self.assertNotIn('JP-disabled-one', groups)
            # The automatic chains are fallback groups over per-source groups.
            self.assertEqual(groups['BOOT-JP-ALL']['type'], 'fallback')
            self.assertEqual(groups['BOOT-JP-ALL']['proxies'],
                             ['JP-big-airport', 'JP-ktm-cloud', 'JP-office91'])
            self.assertEqual(groups['BOOT-AI-JP-ALL']['proxies'],
                             ['AI-JP-big-airport', 'AI-JP-ktm-cloud', 'AI-JP-office91'])
            self.assertEqual(groups['BOOT-AI-JP-ALL']['type'], 'fallback')
            # Aggregates consumed by the runtime/GUI are retained.
            self.assertEqual(groups['主代理']['proxies'][0], 'BOOT-JP-ALL')
            self.assertEqual(groups['AI主代理']['proxies'][0], 'BOOT-AI-JP-ALL')
            self.assertIn('JP-ALL', groups)
            self.assertIn('AI-JP-ALL', groups)
            # Rules come from base_rules_source regardless of ordering.
            self.assertIn('DOMAIN-SUFFIX,from-ktm.example,直接', cfg['rules'])
            self.assertNotIn('DOMAIN-SUFFIX,from-big.example,直接', cfg['rules'])
            # Labels contract: source_id per entry, reserved top-level keys.
            with open(os.path.join(d, 'node-labels.json')) as f:
                labels_doc = json.load(f)
            labels = labels_doc['nodes']
            by_source = {}
            for value in labels.values():
                by_source.setdefault(value['source_id'], set()).add(value['name'])
            self.assertEqual(by_source['big-airport'], {'日本 one', '香港 two'})
            self.assertEqual(by_source['ktm-cloud'], {'东京 three'})
            self.assertEqual(by_source['office91'], {'日本 four'})
            for internal, value in labels.items():
                profile = value['profile'].encode('utf-8')
                data = b'srhop-label-v1\0' + len(profile).to_bytes(8, 'big') + profile + value['name'].encode('utf-8')
                self.assertTrue(internal.endswith('-lbl-' + hashlib.sha256(data).hexdigest()))
            self.assertEqual(labels_doc['_sources'],
                             {'big-airport': '大机场 Big Airport', 'ktm-cloud': 'KTM Cloud',
                              'office91': '玖壹 Office91'})
            with open(config, 'rb') as f:
                digest = hashlib.sha256(f.read()).hexdigest()
            self.assertEqual(labels_doc['_generation'], digest[:16])
            self.assertEqual(labels_doc['config_sha256'], digest)

    def test_non_japanese_nodes_participate_in_both_routes(self):
        with tempfile.TemporaryDirectory() as d:
            source = os.path.join(d, 'source.yaml')
            write_profile(source, ['Hong Kong'])
            config = os.path.join(d, 'config.yaml')
            settings = os.path.join(d, 'settings.json')
            with open(settings, 'w') as f:
                json.dump({'source_profiles': [source]}, f)
            with mock.patch.multiple(g, MIHOMO_DIR=d, SRHOP_DIR=d, CONFIG_PATH=config, SRHOP_CONFIG=settings), mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(g.subprocess, 'run', return_value=mock.Mock(returncode=0)):
                g.build()
            with open(config) as f:
                cfg = g.yaml.safe_load(f)
            groups = {item['name']: item for item in cfg['proxy-groups']}
            self.assertIn('BOOT-JP-ALL', groups)
            self.assertIn('BOOT-AI-JP-ALL', groups)
            node = groups['JP-ALL']['proxies'][0]
            self.assertIn(node, groups['主代理']['proxies'])
            self.assertIn(node, groups['AI主代理']['proxies'])
            self.assertIn(node, groups['AI-JP-ALL']['proxies'])

    def test_subscription_cache_missing_skips_source(self):
        with tempfile.TemporaryDirectory() as d:
            big = os.path.join(d, 'big.yaml')
            write_profile(big, ['日本 one'])
            config = os.path.join(d, 'config.yaml')
            settings = os.path.join(d, 'settings.json')
            with open(settings, 'w') as f:
                json.dump({'office91_enabled': True, 'sources': [
                    {'id': 'big-airport', 'label': '大机场 Big Airport', 'type': 'profile', 'path': big, 'enabled': True},
                    {'id': 'office91', 'label': '玖壹 Office91', 'type': 'subscription', 'enabled': True},
                ]}, f)
            with mock.patch.multiple(g, MIHOMO_DIR=d, SRHOP_DIR=d, CONFIG_PATH=config, SRHOP_CONFIG=settings), mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(g.subprocess, 'run', return_value=mock.Mock(returncode=0)):
                result = g.build()
            with open(config) as f:
                cfg = g.yaml.safe_load(f)
            groups = {x['name']: x for x in cfg['proxy-groups']}
            self.assertNotIn('JP-office91', groups)
            self.assertEqual(groups['BOOT-JP-ALL']['proxies'], ['JP-big-airport'])
            with open(os.path.join(d, 'node-labels.json')) as f:
                labels_doc = json.load(f)
            # Enabled-but-missing subscription still shows up in metadata.
            self.assertIn('office91', labels_doc['_sources'])
            self.assertEqual([s['id'] for s in result['sources']], ['big-airport'])

    def test_base_rules_source_missing_or_disabled_fails(self):
        with tempfile.TemporaryDirectory() as d:
            big = os.path.join(d, 'big.yaml')
            write_profile(big, ['日本 one'])
            config = os.path.join(d, 'config.yaml')
            settings = os.path.join(d, 'settings.json')
            with open(settings, 'w') as f:
                json.dump({'sources': [
                    {'id': 'big-airport', 'label': '大机场 Big Airport', 'type': 'profile', 'path': big, 'enabled': True},
                    {'id': 'off', 'label': 'Off', 'type': 'profile', 'path': os.path.join(d, 'off.yaml'), 'enabled': False},
                ], 'base_rules_source': 'off'}, f)
            with mock.patch.multiple(g, MIHOMO_DIR=d, SRHOP_DIR=d, CONFIG_PATH=config, SRHOP_CONFIG=settings), mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(g.subprocess, 'run', return_value=mock.Mock(returncode=0)):
                with self.assertRaises(ValueError):
                    g.build()
            with open(settings, 'w') as f:
                json.dump({'sources': [
                    {'id': 'big-airport', 'label': '大机场 Big Airport', 'type': 'profile', 'path': big, 'enabled': True},
                ], 'base_rules_source': 'nope'}, f)
            with mock.patch.multiple(g, MIHOMO_DIR=d, SRHOP_DIR=d, CONFIG_PATH=config, SRHOP_CONFIG=settings), mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(g.subprocess, 'run', return_value=mock.Mock(returncode=0)):
                with self.assertRaises(ValueError):
                    g.build()

    def test_duplicate_source_id_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            big = os.path.join(d, 'big.yaml')
            write_profile(big, ['日本 one'])
            settings = os.path.join(d, 'settings.json')
            with open(settings, 'w') as f:
                json.dump({'sources': [
                    {'id': 'dup', 'label': 'A', 'type': 'profile', 'path': big, 'enabled': True},
                    {'id': 'dup', 'label': 'B', 'type': 'profile', 'path': big, 'enabled': True},
                ]}, f)
            with mock.patch.multiple(g, MIHOMO_DIR=d, SRHOP_DIR=d, CONFIG_PATH=os.path.join(d, 'config.yaml'), SRHOP_CONFIG=settings), mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(g.subprocess, 'run', return_value=mock.Mock(returncode=0)):
                with self.assertRaises(ValueError):
                    g.build()

    def test_aggregation_and_validation_before_replace(self):
        with tempfile.TemporaryDirectory() as d:
            paths = []
            for i, name in enumerate(['Japan one', '日本 two']):
                p = os.path.join(d, str(i)+'.yaml')
                write_profile(p, [name])
                paths.append(p)
            config = os.path.join(d, 'config.yaml')
            settings = os.path.join(d, 'settings.json')
            with open(settings, 'w') as f: json.dump({'source_profiles': paths}, f)
            with mock.patch.multiple(g, MIHOMO_DIR=d, SRHOP_DIR=d, CONFIG_PATH=config, SRHOP_CONFIG=settings), mock.patch.dict(os.environ, {}, clear=True):
                with mock.patch.object(g.subprocess, 'run', return_value=mock.Mock(returncode=0)):
                    g.build()
                with open(config) as f: cfg = g.yaml.safe_load(f)
                groups = {x['name']: x for x in cfg['proxy-groups']}
                self.assertEqual(len(groups['JP-ALL']['proxies']), 2)
                self.assertEqual(sorted(n for n in groups if n.startswith('JP-') and n != 'JP-ALL'), ['JP-0', 'JP-1'])
                self.assertEqual(groups['主代理']['proxies'][0], 'BOOT-JP-ALL')
                self.assertEqual(groups['AI主代理']['proxies'][0], 'BOOT-AI-JP-ALL')
                self.assertEqual(groups['BOOT-JP-ALL']['proxies'], ['JP-0', 'JP-1'])
                self.assertEqual(groups['BOOT-AI-JP-ALL']['proxies'], ['AI-JP-0', 'AI-JP-1'])
                with open(os.path.join(d, 'node-labels.json')) as f: labels = json.load(f)['nodes']
                self.assertEqual({v['name'] for v in labels.values()}, {'Japan one', '日本 two'})
                self.assertEqual({v['profile'] for v in labels.values()}, {'0.yaml', '1.yaml'})
                self.assertEqual({v['source_id'] for v in labels.values()}, {'0', '1'})
                for internal, value in labels.items():
                    profile = value['profile'].encode('utf-8')
                    data = b'srhop-label-v1\0' + len(profile).to_bytes(8, 'big') + profile + value['name'].encode('utf-8')
                    self.assertTrue(internal.endswith('-lbl-' + hashlib.sha256(data).hexdigest()))
                self.assertEqual(os.stat(os.path.join(d, 'node-labels.json')).st_mode & 0o777, 0o600)
                with open(config, 'rb') as f: before = f.read()
                with mock.patch.object(g.subprocess, 'run', return_value=mock.Mock(returncode=1, stdout='', stderr='bad')):
                    with self.assertRaises(ValueError): g.build()
                with open(config, 'rb') as f: self.assertEqual(before, f.read())

    def test_env_profiles_gate_profile_sources(self):
        with tempfile.TemporaryDirectory() as d:
            big = os.path.join(d, 'big.yaml')
            ktm = os.path.join(d, 'ktm.yaml')
            write_profile(big, ['日本 one'])
            write_profile(ktm, ['东京 two'])
            config = os.path.join(d, 'config.yaml')
            settings = os.path.join(d, 'settings.json')
            with open(settings, 'w') as f:
                json.dump({'sources': [
                    {'id': 'big-airport', 'label': '大机场 Big Airport', 'type': 'profile', 'path': big, 'enabled': True},
                    {'id': 'ktm-cloud', 'label': 'KTM Cloud', 'type': 'profile', 'path': ktm, 'enabled': True},
                ]}, f)
            with mock.patch.multiple(g, MIHOMO_DIR=d, SRHOP_DIR=d, CONFIG_PATH=config, SRHOP_CONFIG=settings), mock.patch.dict(os.environ, {'SRHOP_SOURCE_PROFILES': json.dumps([big])}, clear=True), mock.patch.object(g.subprocess, 'run', return_value=mock.Mock(returncode=0)):
                g.build()
            with open(config) as f:
                groups = {x['name'] for x in g.yaml.safe_load(f)['proxy-groups']}
            self.assertIn('JP-big-airport', groups)
            self.assertNotIn('JP-ktm-cloud', groups)

    def test_sorting_does_not_change_rules(self):
        # Reordering sources must not change the rules base: it is pinned to
        # base_rules_source, not to list order.
        with tempfile.TemporaryDirectory() as d:
            big = os.path.join(d, 'big.yaml')
            ktm = os.path.join(d, 'ktm.yaml')
            write_profile(big, ['日本 one'], ['DOMAIN-SUFFIX,big.example,DIRECT', 'MATCH,DIRECT'])
            write_profile(ktm, ['东京 two'], ['DOMAIN-SUFFIX,ktm.example,DIRECT', 'MATCH,DIRECT'])
            config = os.path.join(d, 'config.yaml')
            settings = os.path.join(d, 'settings.json')
            sources = [
                {'id': 'big-airport', 'label': '大机场 Big Airport', 'type': 'profile', 'path': big, 'enabled': True},
                {'id': 'ktm-cloud', 'label': 'KTM Cloud', 'type': 'profile', 'path': ktm, 'enabled': True},
            ]
            rules_seen = []
            for order in (sources, [sources[1], sources[0]]):
                with open(settings, 'w') as f:
                    json.dump({'sources': order, 'base_rules_source': 'big-airport'}, f)
                with mock.patch.multiple(g, MIHOMO_DIR=d, SRHOP_DIR=d, CONFIG_PATH=config, SRHOP_CONFIG=settings), mock.patch.dict(os.environ, {}, clear=True), mock.patch.object(g.subprocess, 'run', return_value=mock.Mock(returncode=0)):
                    g.build()
                with open(config) as f:
                    rules_seen.append(g.yaml.safe_load(f)['rules'])
            self.assertEqual(rules_seen[0], rules_seen[1])
            self.assertIn('DOMAIN-SUFFIX,big.example,DIRECT', rules_seen[0])


if __name__ == '__main__': unittest.main()

class RoutingSettingsTest(unittest.TestCase):
    def test_custom_keywords_partition(self):
        conf = {'proxies': [{'name': n, 'type': 'http', 'server': 'localhost', 'port': 80} for n in ['Japan', 'Hong Kong', 'HK VIP']]}
        selected, other = g.take(conf, 'source', keywords=['hk', 'Hong Kong'])
        self.assertEqual(len(selected), 2)
        self.assertEqual(len(other), 1)

    def test_fallback_address(self):
        self.assertEqual(g.parse_fallback_address('[::1]:8080'), ('::1', 8080))
        for value in ['host:0', 'host:65536', ':8080', '::1:8080', 'host:bad', None]:
            with self.assertRaises(ValueError):
                g.parse_fallback_address(value)
