import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class LabelVerificationTests(unittest.TestCase):
    def test_manual_recovery_response_feedback(self):
        swift = shutil.which("swift")
        if not swift:
            self.skipTest("Swift unavailable")
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        definitions = source[source.index("struct NodeGroup:"):source.index("struct NodeProbe:")]
        script = '''import Foundation
''' + definitions + '''
func decode(_ extra: String) -> NodeGroup {
    let json = "{\\"group\\":\\"主代理\\",\\"mode\\":\\"manual\\",\\"selected\\":\\"n\\",\\"manual_node\\":\\"n\\",\\"nodes\\":[\\"n\\"]" + extra + "}"
    return try! JSONDecoder().decode(NodeGroup.self, from: Data(json.utf8))
}
let old = decode("")
precondition(old.recoveryStatus == nil && old.recoveryReason == nil)
precondition(manualRecoveryNotice(old.recoveryStatus, reason: old.recoveryReason).contains("结果未知"))
let switched = decode(",\\"recovery_status\\":\\"switched_unverified_path\\",\\"recovery_reason\\":\\"selector confirmed; node health, actual path and destination reachability are not verified\\"")
let switchedNotice = manualRecoveryNotice(switched.recoveryStatus, reason: switched.recoveryReason)
precondition(switchedNotice.contains("选择器已确认") && switchedNotice.contains("尚未验证"))
precondition(!switchedNotice.contains("验证通过"))
let recovered = decode(",\\"recovery_status\\":\\"recovered\\",\\"recovery_reason\\":\\"selected node and primary probe succeeded; destination reachability is not guaranteed\\"")
precondition(manualRecoveryNotice(recovered.recoveryStatus, reason: recovered.recoveryReason).contains("验证通过"))
let failed = decode(",\\"recovery_status\\":\\"failed\\",\\"recovery_reason\\":\\"selected node probe failed or inconclusive\\"")
precondition(manualRecoveryNotice(failed.recoveryStatus, reason: failed.recoveryReason).contains("探测失败"))
precondition(manualRecoveryNotice("not_attempted", reason: "AI selector is not used in global mode").contains("全局模式"))
precondition(manualRecoveryNotice("not_attempted", reason: "controller routing mode unavailable or differs from preference").contains("不一致"))
precondition(manualRecoveryNotice("not_attempted", reason: "global selector is not using the primary group").contains("未使用主代理"))
precondition(manualRecoveryNotice("not_attempted", reason: "shutting down").contains("正在关闭"))
precondition(manualRecoveryNotice("not_needed", reason: nil).contains("实际线路"))
precondition(manualRecoveryNotice("future_status", reason: nil).contains("结果未知"))
precondition(manualRecoveryNotice("failed", reason: "new failure").contains("new failure"))
'''
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory, "recovery.swift")
            path.write_text(script, encoding="utf-8")
            subprocess.run([swift, str(path)], check=True, timeout=90)
        switch = source[source.index("    func setNode(group:"):source.index("    // Mutations are per-domain")]
        self.assertIn("manualRecoveryNotice(updated.recoveryStatus, reason: updated.recoveryReason)", switch)
        self.assertIn("nodeMutationSession.dataTask(with: request)", switch)
        self.assertIn("config.timeoutIntervalForResource = 8", source)
        self.assertIn("request.timeoutInterval = 5", source[source.index("    func nodeRequest(method:"):source.index("    func refreshNodeGroups()")])
        self.assertNotIn("timeoutInterval = 120", switch)
        self.assertIn("偏好可能已保存，实际选择未知", switch)
        self.assertIn("self.refreshIfVisible()", switch)

    def test_node_switch_does_not_invalidate_app_scene(self):
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        app = source[source.index("@main struct SrhopBarApp: App"):]
        delegate = source[source.index("final class AppDelegate:"):source.index("struct SidebarView:")]
        label = source[source.index("struct MenuStatusLabel:"):source.index("@main struct SrhopBarApp:")]
        switch = source[source.index("    func setNode(group:"):source.index("    // Mutations are per-domain")]
        self.assertIn("let model = Model()", delegate)
        self.assertNotIn("@StateObject", app)
        self.assertNotIn("@ObservedObject", app)
        self.assertIn("MainView().environmentObject(appDelegate.model)", app)
        self.assertIn("MenuStatusLabel().environmentObject(appDelegate.model)", app)
        self.assertIn("@EnvironmentObject var model: Model", label)
        self.assertIn("model.currentRouteQuality", label)
        self.assertIn("model.globalRateCompact()", label)
        self.assertIn("nodeControlBusy = true", switch)
        self.assertIn("self.nodeGroups[group] = updated", switch)
        self.assertIn("self.refreshIfVisible()", switch)
        nodes = source[source.index("struct NodesView:"):source.index("// UTC-aligned buckets")]
        for text in ("切换中…", "当前线路", "当前处于备用线路，手动偏好尚未用于实际线路。"):
            self.assertIn(text, nodes)
        self.assertIn('model.state?.mode == "fallback"', nodes)
        self.assertIn('Label(locked ? "解锁" : "锁定"', nodes)
        self.assertIn('Text("普通流量").frame(width: 96)', nodes)
        self.assertIn('Text("AI流量·\\(t)")', nodes)
        for forbidden in ("openWindow", "activate(", "makeKey", "orderFront"):
            self.assertNotIn(forbidden, switch)

    def test_capture_contract(self):
        swift = shutil.which("swift")
        if not swift:
            self.skipTest("Swift unavailable")
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        definitions = source[source.index("struct CaptureEvent:"):source.index("struct BlockedDomains:")]
        script = '''import Foundation
''' + definitions + '''
let config = CaptureConfig(enabled: true, domain: "example.com", durationSeconds: 300, maxEvents: 100)
let payload = try JSONSerialization.jsonObject(with: JSONEncoder().encode(config)) as! [String: Any]
precondition(payload["enabled"] as? Bool == true && payload["decrypt"] as? Bool == false)
precondition(payload["domain"] as? String == "example.com" && payload["duration_seconds"] as? Int == 300 && payload["max_events"] as? Int == 100)
let data = Data(#"{"enabled":false,"domain":"","expires":"0001-01-01T00:00:00Z","max_events":0,"events":[],"storage_error":false,"decrypt_supported":false}"#.utf8)
let status = try JSONDecoder().decode(CaptureStatus.self, from: data)
precondition(!status.enabled && !status.decryptSupported && status.events.isEmpty)
'''
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory, "capture.swift")
            path.write_text(script, encoding="utf-8")
            subprocess.run([swift, str(path)], check=True, timeout=90)
        # The capture controls were removed from the UI; only the decoders stay.

    def test_menu_decode_measurement(self):
        swift = shutil.which("swiftc")
        if not swift:
            self.skipTest("Swift unavailable")
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        definitions = source[source.index("struct TrafficTotals:"):source.index("struct NodeLabel:")]
        script = '''import Foundation
''' + definitions + '''
let rows = (0..<10000).map { i in "{\\\"Target\\\":\\\"domain\\(i).example\\\",\\\"Upload\\\":1,\\\"Download\\\":2}" }.joined(separator: ",")
let data = Data(("{\\\"rows\\\":[" + rows + "],\\\"p95_60s_samples\\\":1,\\\"p95_60s_ms_upper\\\":10}").utf8)
func measure<T: Decodable>(_ type: T.Type) -> Double {
    let start = Date(); for _ in 0..<10 { _ = try! JSONDecoder().decode(type, from: data) }
    return Date().timeIntervalSince(start) * 100
}
let full = measure(Statistics.self), menu = measure(MenuStatistics.self)
print(String(format: "synthetic 10000 rows: full %.2f ms, menu %.2f ms per decode", full, menu))
precondition(menu < full)
'''
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory, "bench.swift"); executable = Path(directory, "bench")
            path.write_text(script, encoding="utf-8")
            subprocess.run([swift, "-O", str(path), "-o", str(executable)], check=True, timeout=90)
            subprocess.run([str(executable)], check=True, timeout=90)

    def test_domain_detail_connected_to_ranking(self):
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        view = source[source.index("struct StatsView:"):source.index("struct SettingsView:")]
        self.assertIn("域名流量 TOP 10", view)
        self.assertIn("ForEach(Array(rows.enumerated()), id: \\.offset)", view)
        self.assertIn("sortedDomainRanking((try? result?.get())?.domainRanking", source)
        self.assertIn("仅统计已观测流量，可能漏采短连接", view)
        self.assertIn("域名排行不可用或格式无效", view)
        self.assertNotIn("上游端口流量", view)
        domain = source[source.index("struct DomainView:"):source.index("struct StatsView:")]
        self.assertIn("历史域名（累计）", domain)
        self.assertIn("model.stats?.domainHistory", domain)
        self.assertIn("自统计启用以来的累计", domain)
        self.assertIn('Text("持久屏蔽域名")', domain)
        self.assertIn('TextField("添加域名，例如 example.com"', domain)
        self.assertIn('Button("上一页")', domain)
        self.assertIn("每页 20 条", domain)

    def test_stats_text_uses_displayed_window_and_plain_url_minutes(self):
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        view = source[source.index("struct StatsView:"):source.index("struct SettingsView:")]
        self.assertIn('当前选择：\\(model.statsWindow.label)', view)
        self.assertNotIn('最近 60 分钟按目标', view)
        self.assertIn('String(model.displayedStatsWindow?.rawValue ?? model.statsWindow.rawValue)', view)
        swift = shutil.which("swift")
        if not swift:
            self.skipTest("Swift unavailable")
        enum = source[source.index("enum StatsWindow:"):source.index("@MainActor final class Model")]
        script = enum + '''
for window in StatsWindow.allCases {
    let url = "http://127.0.0.1:33335/stats?minutes=\\(String(window.rawValue))"
    precondition(!url.contains(","))
    precondition(url.hasSuffix(String(window.rawValue)))
}
precondition(StatsWindow.day.label == "1天" && StatsWindow.twoDays.label == "2天")
'''
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory, "stats-text.swift")
            path.write_text(script, encoding="utf-8")
            subprocess.run([swift, str(path)], check=True, timeout=90)

    def test_cached_engine_values_and_thread_local_dates(self):
        swift = shutil.which("swift")
        if not swift:
            self.skipTest("Swift unavailable")
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        helpers = source[source.index("func parseTime("):source.index("@MainActor final class Model")]
        script = '''import Foundation
''' + helpers + '''
let values = parseEngineValues("secret: 'first'\\nsecret: second\\nexternal-controller: 127.0.0.1:19090\\n")
precondition(values["secret"] == "first" && values["external-controller"] == "127.0.0.1:19090")
precondition(parseEngineValues("").isEmpty)
DispatchQueue.concurrentPerform(iterations: 100) { _ in
    precondition(parseTime("2026-10-30T12:00:00Z") != nil)
    precondition(parseTime("2026-10-30T12:00:00.123Z") != nil)
    precondition(parseTime("invalid") == nil)
}
'''
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory, "cache.swift")
            path.write_text(script, encoding="utf-8")
            subprocess.run([swift, str(path)], check=True, timeout=90)
        self.assertIn("snapshotQueue.async", source[source.index("private func refreshCredentials"):])
        self.assertIn("ForEach(Array(rows.enumerated())", source[source.index("struct StatsView:"):source.index("struct SettingsView:")])

    def test_fallback_threshold_is_explicitly_observed(self):
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        self.assertIn("备用路径已观测流量每累计 1 GiB", source)
        self.assertIn("并非真实流量的硬阈值", source)
        self.assertIn("周期探测仍会继续", source)

    def test_gui_verifier_interoperability_and_fail_closed(self):
        swift = shutil.which("swift")
        if not swift:
            self.skipTest("Swift is unavailable")
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        start = source.index("struct NodeLabel: Decodable")
        end = source.index("struct ProxyHistory: Decodable", start)
        verifier = source[start:end]
        profile, name = "配置 🇯🇵", "东京节点: α"
        p = profile.encode()
        digest = hashlib.sha256(b"srhop-label-v1\0" + len(p).to_bytes(8, "big") + p + name.encode()).hexdigest()
        key = "SRC1-JP-node-lbl-" + digest
        payload = json.dumps({"nodes": {
            key: {"profile": profile, "name": name},
            "old-node": {"profile": profile, "name": name},
            "other-lbl-" + digest: {"profile": profile, "name": "different"},
            key + "-extra": {"profile": profile, "name": name},
            "empty-lbl-" + digest: {"profile": "", "name": name},
        }}, ensure_ascii=False).encode()
        with tempfile.TemporaryDirectory() as directory:
            data_path = Path(directory) / "labels.json"
            data_path.write_bytes(payload)
            script = Path(directory) / "verify.swift"
            script.write_text("import Foundation\nimport CryptoKit\n" + verifier + '''
let data = try Data(contentsOf: URL(fileURLWithPath: CommandLine.arguments[1]))
let labels = verifiedLabels(data)
let expected = CommandLine.arguments[2]
precondition(labels.count == 1 && labels[expected]?.name == "东京节点: α")
precondition(verifiedLabels(Data("not json".utf8)).isEmpty)
''', encoding="utf-8")
            subprocess.run([swift, str(script), str(data_path), key], check=True, timeout=90)


    def test_japan_candidates_and_current_routes(self):
        swift = shutil.which("swift")
        if not swift:
            self.skipTest("Swift is unavailable")
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        definitions = source[source.index("struct ProxyHistory:"):source.index("@MainActor final class Model")]
        selector = source[source.index("    func selectedNode("):source.index("    func nodeStatus(")]
        script_text = '''import Foundation
''' + definitions + '''
struct TestModel {
    var proxies: [String: ProxyInfo]
    var state: SrhopState?
    var nodeError: String?
    var nodeNames: [String] { japanCandidates(proxies) ?? [] }
''' + selector + '''
}
func decode(_ json: String) -> [String: ProxyInfo] {
    try! JSONDecoder().decode(ProxiesResponse.self, from: Data(json.utf8)).proxies
}
let proxies = decode("""
{"proxies":{"JP-ALL":{"type":"URLTest","all":["jp2","jp1","jp1","FALLBACK-LOCAL","PASS-RULE","nested"]},"jp1":{"type":"Http"},"jp2":{"type":"Shadowsocks"},"other":{"type":"Http"},"FALLBACK-LOCAL":{"type":"Http"},"PASS-RULE":{"type":"Pass"},"nested":{"type":"Selector","all":["other"]},"主代理":{"type":"Selector","now":"jp1"},"AI主代理":{"type":"Selector","now":"FALLBACK-LOCAL"}}}
""")
precondition(japanCandidates(proxies) == ["jp1", "jp2"])
precondition(japanCandidates(decode("{\\"proxies\\":{}}")) == nil)
precondition(japanCandidates(decode("{\\"proxies\\":{\\"JP-ALL\\":{\\"type\\":\\"URLTest\\",\\"all\\":[]}}}")) == [])
precondition(japanCandidates(decode("{\\"proxies\\":{\\"JP-ALL\\":{\\"type\\":\\"URLTest\\",\\"all\\":[\\"missing\\"]}}}")) == nil)
let now = ISO8601DateFormatter().string(from: Date())
func state(_ mode: String, _ upstream: String, _ healthy: Bool) -> SrhopState {
    let json = "{\\"active\\":true,\\"ready\\":true,\\"mode\\":\\"\\(mode)\\",\\"upstream\\":\\"\\(upstream)\\",\\"ai_healthy\\":\\(healthy),\\"updated_at\\":\\"\\(now)\\"}"
    return try! JSONDecoder().decode(SrhopState.self, from: Data(json.utf8))
}
var model = TestModel(proxies: proxies, state: state("primary", "localhost:7890", true), nodeError: nil)
precondition(model.selectedNode("主代理") == "jp1")
precondition(model.selectedNode("AI主代理", ai: true) == "FALLBACK-LOCAL")
model.state = state("primary", "localhost:7890", false)
precondition(model.selectedNode("AI主代理", ai: true) == "FALLBACK-LOCAL")
model.nodeError = "offline"
precondition(model.selectedNode("主代理") == nil)
model.state = state("fallback", "127.0.0.1:33210", false)
precondition(model.selectedNode("主代理") == "FALLBACK-LOCAL")
precondition(model.selectedNode("AI主代理", ai: true) == "FALLBACK-LOCAL")
model.state = state("fallback", "192.0.2.10:9999", true)
precondition(model.selectedNode("主代理") == "FALLBACK-LOCAL")
precondition(model.selectedNode("AI主代理", ai: true) == "FALLBACK-LOCAL")
model.state = state("fallback", "[::1]:4567", true)
precondition(model.selectedNode("主代理") == "FALLBACK-LOCAL")
model.state = state("fallback", "", true)
precondition(model.selectedNode("主代理") == nil)
model.state = nil
precondition(model.selectedNode("主代理") == nil)
'''
        with tempfile.TemporaryDirectory(dir="/tmp") as directory:
            script = Path(directory) / "nodes.swift"
            script.write_text(script_text, encoding="utf-8")
            subprocess.run([swift, str(script)], check=True, timeout=90)


    def test_block_api_uses_validated_config_and_list_is_independent(self):
        swift = shutil.which("swift")
        if not swift:
            self.skipTest("Swift is unavailable")
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        node_request = source[source.index("    func nodeRequest(method:"):source.index("    func refreshNodeGroups()")]
        block_request = source[source.index("    func blockedRequest("):source.index("    func readBlocked(")]
        view = source[source.index("struct DomainView:"):source.index("struct StatsView:")]
        self.assertLess(view.index('Text("历史域名（累计）")'), view.index('Text("持久屏蔽域名")'))
        self.assertIn('ForEach(domains.sorted(), id: \\.self)', view)
        self.assertIn('pendingBlock = BlockAction(domain: row.target', view)
        for text in ("经过 ClashHub 的新请求或新隧道", "不检查隧道内目标", "不会关闭已有隧道"):
            self.assertIn(text, view)
        script_text = '''import Foundation
struct TestModel {
    var cfgHTTP: String
    var support: URL
    var apiToken: String? = "test-token"
''' + node_request + block_request + '''
}
let support = URL(fileURLWithPath: CommandLine.arguments[1])
for address in ["127.0.0.1:45678", "localhost:45678", "[::1]:45678"] {
    let model = TestModel(cfgHTTP: address, support: support)
    for method in ["GET", "POST", "DELETE"] {
        let request = try model.blockedRequest(method)
        precondition(request.url!.port == 45678 && request.url!.path == "/api/blocked-domains")
        precondition(request.httpMethod == method && request.value(forHTTPHeaderField: "Authorization") == "Bearer test-token")
    }
}
for token: String? in [nil, "", "bad\\ntoken", "bad\\rtoken"] {
    do { _ = try TestModel(cfgHTTP: "localhost:45678", support: support, apiToken: token).blockedRequest("GET"); fatalError("accepted invalid token") }
    catch { }
}
let defaultRequest = try TestModel(cfgHTTP: "", support: support).blockedRequest("GET")
precondition(defaultRequest.url!.port == 33333)
for address in ["example.com:45678", "localhost:45678@evil.example:80", "localhost:45678/other", "localhost:45678?x=1", "localhost:45678#fragment", "localhost", "localhost:99999"] {
    do { _ = try TestModel(cfgHTTP: address, support: support).blockedRequest("GET"); fatalError("accepted \\(address)") }
    catch { }
}
'''
        with tempfile.TemporaryDirectory(dir="/tmp") as directory:
            Path(directory, "api-token").write_text("test-token", encoding="utf-8")
            script = Path(directory, "block.swift")
            script.write_text(script_text, encoding="utf-8")
            subprocess.run([swift, str(script), directory], check=True, timeout=90)


    def test_target_aggregation_and_domain_validation(self):
        swift = shutil.which("swift")
        if not swift:
            self.skipTest("Swift is unavailable")
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        row = source[source.index("struct DomainRank:"):source.index("struct StatsHour:")]
        helpers = source[source.index("func bytes("):source.index("struct BlockAction:")]
        parse_time = source[source.index("func parseTime("):source.index("func parseEngineValues(")]
        script_text = '''import Foundation
import Darwin
''' + row + parse_time + helpers + '''
func row(_ target: String?, _ node: String, _ upload: UInt64?, _ download: UInt64?) -> StatsRow {
    var r = StatsRow(); r.Target = target; r.Node = node; r.Upload = upload; r.Download = download; return r
}
let result = targetTraffic([row("Example.com", "a", 10, 20), row("example.com", "b", 30, 40), row("1.2.3.4", "a", 20, 0), row("missing", "a", nil, 1)])
precondition(result.map(\\.id) == ["example.com", "1.2.3.4", "missing"])
precondition(result[0].upload == 40 && result[0].download == 60 && result[0].total == 100)
precondition(isIPTarget("1.2.3.4") && isIPTarget("2001:db8::1"))
precondition(!isDNSDomain("1.2.3.4") && !isDNSDomain("*.example.com") && !isDNSDomain("中.example"))
precondition(isDNSDomain("example.com"))
let overflow = targetTraffic([row("overflow", "a", UInt64.max, 1), row("overflow", "b", 1, 0)])
precondition(overflow[0].total == nil)
let large = targetTraffic([row("big", "a", UInt64.max, UInt64.max)])[0]
precondition(totalBytes(large) == "36893488147419103230 字节")
precondition(bytes(1024) == "1.0 KB")
precondition(bytes(1536) == "1.5 KB")
precondition(bytes(1_048_576) == "1.0 MB")
let ranks = try JSONDecoder().decode([DomainRank].self, from: Data("""
[{"target":"a","upload":18446744073709551615,"download":1,"detail_started_at":"2026-01-01T00:00:00Z"},
 {"target":"b","upload":18446744073709551615,"download":2,"detail_started_at":"bad"},
 {"target":"c","upload":2,"download":1}]
""".utf8))
precondition(sortedDomainRanking(ranks).map(\\.target) == ["b", "a", "c"])
precondition(detailStartText(ranks[0].detailStartedAt) != "明细开始时间未知")
precondition(detailStartText(ranks[1].detailStartedAt) == "明细开始时间未知")
let decoded = try JSONDecoder().decode(StatsRow.self, from: Data(#"{"Target":"example.com","Upload":1,"Download":2,"detail_started_at":"2026-01-01T00:00:00Z"}"#.utf8))
precondition(targetTraffic([decoded])[0].detailStartedAt == "2026-01-01T00:00:00Z")
precondition(targetTraffic([decoded, row("example.com", "b", 1, 2)])[0].detailStartInvalid)
precondition(attemptCountText(ranks[0].attempts, startedAt: nil, complete: nil).contains("未知"))
let counted = try JSONDecoder().decode(DomainRank.self, from: Data(#"{"target":"x","upload":1,"download":2,"attempts":7,"attempts_started_at":"2026-01-01T00:00:00Z","attempts_complete":true}"#.utf8))
precondition(attemptCountText(counted.attempts, startedAt: counted.attemptsStartedAt, complete: counted.attemptsComplete).contains("7（接口标记完整"))
precondition(attemptCountText(7, startedAt: nil, complete: true).contains("不完整"))
var first = row("x", "a", 1, 1); first.Attempts = 3
var second = row("x", "b", 1, 1); second.Attempts = 4
precondition(targetTraffic([first, second])[0].attempts == 7)
second.Attempts = nil
precondition(targetTraffic([first, second])[0].attempts == nil)
first.Attempts = UInt64.max; second.Attempts = 1
precondition(targetTraffic([first, second])[0].attempts == nil)
'''
        with tempfile.TemporaryDirectory(dir="/tmp") as directory:
            script = Path(directory) / "traffic.swift"
            script.write_text(script_text, encoding="utf-8")
            subprocess.run([swift, str(script)], check=True, timeout=90)


    def test_jiuyi_offline_contract_and_status(self):
        swift = shutil.which("swift")
        if not swift:
            self.skipTest("Swift unavailable")
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        definitions = source[source.index("struct SubscriptionInfo:"):source.index("func subscriptionDate(")]
        script = "import Foundation\n" + definitions + r'''
let old = try JSONDecoder().decode(SubscriptionResponse.self, from: Data(#"{"profiles":[],"refreshing":false}"#.utf8))
precondition(old.refreshStatuses == nil && old.office91Mode == nil)
precondition(jiuyiOfflineStateText(nil, nil) == "离线导入状态未知")
let response = try JSONDecoder().decode(SubscriptionResponse.self, from: Data(#"{"profiles":[],"refreshing":false,"office91_mode":"offline","office91_offline_running":false,"refresh_statuses":{"office91":{"status":"cached","last_attempt":"2026-10-31T01:00:00Z"}}}"#.utf8))
precondition(response.office91Mode == "offline" && response.office91OfflineRunning == false)
precondition(jiuyiOfflineStateText(response.office91Mode,response.refreshStatuses?["office91"]).contains("尚未确认引擎重载"))
let ok = JiuyiOfflineStatus(status:"ok",error:nil,lastAttempt:"new",lastSuccess:"new")
precondition(jiuyiOfflineStateText("online",ok) == "当前使用在线来源")
precondition(jiuyiOfflineStateText("offline",ok).contains("完成引擎重载"))
let running = JiuyiOfflineStatus(status:"running",error:nil,lastAttempt:"new",lastSuccess:nil)
precondition(!jiuyiOfflineStateText("offline",running).contains("完成"))
precondition(jiuyiOfflineErrorText("offline-profile-identity-ambiguous").contains("选择"))
precondition(!jiuyiOfflineErrorText("https://secret.example/token").contains("secret"))
let auto = jiuyiOfflinePayload(nil)
precondition(auto == ["source_id":"office91","mode":"offline"])
let selected = jiuyiOfflinePayload("/private/selected.yaml")
precondition(selected["profile_path"] == "/private/selected.yaml" && selected.count == 3)
precondition(selected["key_path"] == nil && selected["python_path"] == nil)
let accepted = try JSONDecoder().decode(JiuyiOfflineAccepted.self,from:Data(#"{"source_id":"office91","status":"queued"}"#.utf8))
precondition(accepted.status == "queued")
'''
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory, "offline.swift")
            path.write_text(script, encoding="utf-8")
            subprocess.run([swift, str(path)], check=True, timeout=60)

    def test_node_traffic_rates_and_request(self):
        swift = shutil.which("swift")
        if not swift:
            self.skipTest("Swift is unavailable")
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        definitions = source[source.index("struct NodeTrafficEntry:"):source.index("// Only observed positive traffic")]
        definitions += source[source.index("// Only observed positive traffic"):source.index("struct NodeHealthResponse:")]
        parse_time = source[source.index("func parseTime("):source.index("@MainActor final class Model")]
        rate_text = source[source.index("func rateText("):source.index("struct MenuStatusLabel:")]
        node_rate = source[source.index("    func nodeRateText("):source.index("    func nodeRateShort(")]
        node_request = source[source.index("    func nodeRequest(method:"):source.index("    func refreshNodeGroups()")]
        health_request = source[source.index("    func healthRequest("):source.index("    func refreshNodeTraffic()")]
        script_text = '''import Foundation
''' + definitions + parse_time + rate_text + '''
struct TestModel {
    let cfgHTTP: String
    let support: URL
    var apiToken: String? = "test-token"
    var nodeTraffic: NodeTrafficResponse?
    var trafficByNode: [String: NodeTrafficEntry] { Dictionary((nodeTraffic?.nodes ?? []).map { ($0.node, $0) }, uniquingKeysWith: { first, _ in first }) }
''' + node_request + health_request + node_rate + '''
}
let support = URL(fileURLWithPath: CommandLine.arguments[1])
var model = TestModel(cfgHTTP: "127.0.0.1:33333", support: support, nodeTraffic: nil)
let req = try model.healthRequest(path: "/api/node-traffic?minutes=60", method: "GET")
precondition(req.url!.host == "127.0.0.1" && req.url!.port == 33335 && req.url!.path == "/api/node-traffic" && req.url!.query == "minutes=60")
precondition(req.value(forHTTPHeaderField: "Authorization") == "Bearer test-token")
let now = ISO8601DateFormatter().string(from: Date())
func response(_ status: String, _ date: String, _ available: Bool = true) -> NodeTrafficResponse {
    let json = "{\\"updated_at\\":\\"\\(date)\\",\\"window_minutes\\":2880,\\"coverage\\":\\"partial\\",\\"sampling_status\\":\\"\\(status)\\",\\"nodes\\":[{\\"node\\":\\"internal\\",\\"upload\\":10,\\"download\\":0,\\"upload_bps\\":1024,\\"download_bps\\":2048,\\"rate_available\\":\\(available)}]}"
    return try! JSONDecoder().decode(NodeTrafficResponse.self, from: Data(json.utf8))
}
precondition(rateText(nil) == "—")
precondition(rateText(.nan) == "—")
precondition(rateText(.infinity) == "—")
precondition(rateText(-1) == "—")
precondition(rateText(0) == "0 B/s")
precondition(rateText(999) == "999 B/s")
precondition(rateText(1000) == "1000 B/s")
precondition(rateText(1024) == "1.0 KB/s")
precondition(rateText(1536) == "1.5 KB/s")
precondition(rateText(1_048_576) == "1.0 MB/s")
precondition(rateText(1_572_864) == "1.5 MB/s")
precondition(model.nodeRateText("internal") == "↑ —  ↓ —")
model.nodeTraffic = response("ok", now)
precondition(model.nodeRateText("internal") == "↑ 1.0 KB/s  ↓ 2.0 KB/s")
precondition(model.nodeRateText("other") == "↑ —  ↓ —")
precondition(usedHealthNodes(model.nodeTraffic, knownNodes: ["internal", "unused"]) == ["internal"])
precondition(usedHealthNodes(model.nodeTraffic, knownNodes: ["unused"]).isEmpty)
precondition(usedHealthNodes(nil, knownNodes: ["internal"]).isEmpty)
let zero = try! JSONDecoder().decode(NodeTrafficResponse.self, from: Data(#"{"window_minutes":2880,"coverage":"partial","sampling_status":"ok","nodes":[{"node":"internal","upload":0,"download":0,"rate_available":true}]}"#.utf8))
precondition(usedHealthNodes(zero, knownNodes: ["internal"]).isEmpty)
let unknown = try! JSONDecoder().decode(NodeTrafficResponse.self, from: Data(#"{"window_minutes":2880,"coverage":"partial","sampling_status":"ok","nodes":[{"node":"node-123456","upload":9,"download":0,"rate_available":false}]}"#.utf8))
precondition(usedHealthNodes(unknown, knownNodes: ["internal"]).isEmpty)
let wrongWindow = try! JSONDecoder().decode(NodeTrafficResponse.self, from: Data(#"{"window_minutes":60,"coverage":"partial","sampling_status":"ok","nodes":[{"node":"internal","upload":9,"download":0,"rate_available":false}]}"#.utf8))
precondition(usedHealthNodes(wrongWindow, knownNodes: ["internal"]).isEmpty)
precondition(usedHealthNodes(wrongWindow, knownNodes: ["internal"], minutes: 60) == ["internal"])
model.nodeTraffic = response("unavailable", now)
precondition(usedHealthNodes(model.nodeTraffic, knownNodes: ["internal"]) == ["internal"])
precondition(model.nodeRateText("internal") == "↑ —  ↓ —")
model.nodeTraffic = response("ok", now, false)
precondition(model.nodeRateText("internal") == "↑ —  ↓ —")
model.nodeTraffic = response("ok", "2000-01-01T00:00:00Z")
precondition(model.nodeRateText("internal") == "↑ —  ↓ —")
'''
        with tempfile.TemporaryDirectory(dir="/tmp") as directory:
            Path(directory, "api-token").write_text("test-token", encoding="utf-8")
            script = Path(directory, "node-traffic.swift")
            script.write_text(script_text, encoding="utf-8")
            subprocess.run([swift, str(script), directory], check=True, timeout=90)
        self.assertIn('self?.refreshNodeTraffic()', source)
        self.assertNotIn('Text("速率")', source)
        self.assertIn('"pause.fill" : "play.fill"', source)
        grid = source[source.index('struct HealthGrid:'):source.index('// Missing byte counters')]
        self.assertNotIn('未匹配节点 (SHA256:', grid)
        self.assertNotIn('未归属节点', grid)
        builder = source[source.index('func buildHealthSnapshot('):source.index('struct HealthGrid:')]
        self.assertIn('let node = hashes[String(parts[1])]', builder)
        self.assertNotIn('LazyVStack', grid)
        self.assertIn('frame(maxWidth: .infinity).frame(height: 12)', grid)
        self.assertIn('.equatable()', source)
        self.assertIn('仅显示当前数据周期内观测到流量', grid)
        self.assertIn('window_health_hours', source)
        self.assertIn('self.statsGeneration == generation', source)
        self.assertIn('self.statsWindow == selected', source)
        self.assertIn('/api/node-traffic?minutes=2880', source)
        self.assertIn('/api/node-traffic?minutes=\\(selected.rawValue)', source)

    def test_health_snapshot_correctness_and_measurement(self):
        swiftc = shutil.which("swiftc")
        if not swiftc:
            self.skipTest("Swift compiler unavailable")
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        definitions = source[source.index("struct StatsHour:"):source.index("struct NodeLabel:")]
        builder = source[source.index("struct HealthKey:"):source.index("struct NetworkSeriesChart:")]
        script = '''import Foundation
import CryptoKit
''' + definitions + builder + '''
let now = Date(timeIntervalSince1970: 1735732800)
let fmt = ISO8601DateFormatter()
let nodes = (0..<200).map { "node-\\($0)" }
var records: [StatsHour] = []
for node in nodes {
    let hash = SHA256.hash(data: Data(node.utf8)).map { String(format: "%02x", $0) }.joined()
    for probe in ["gstatic", "ai"] {
        for offset in 0..<48 {
            let hour = fmt.string(from: now.addingTimeInterval(Double(-offset * 3600)))
            records.append(StatsHour(Hour: hour, Name: "node-\\(hash)-\\(probe)", Probe: nil, Attempts: 2, Successes: 1, Failures: nil, Upload: nil, Download: nil, P95MSUpper: nil))
        }
    }
}
let start = ProcessInfo.processInfo.systemUptime
let snapshot = buildHealthSnapshot(records: records, nodeNames: nodes, now: now)
let elapsed = (ProcessInfo.processInfo.systemUptime - start) * 1000
precondition(snapshot.nodes.count == 200 && snapshot.indexed.count == 19200)
precondition(snapshot.indexed.values.allSatisfy { $0.attempts == 2 && $0.successes == 1 })
let short = buildHealthSnapshot(records: records, nodeNames: nodes, now: now.addingTimeInterval(1200), minutes: 60)
precondition(short.hour - short.firstHour == 1 && short.indexed.count == 800)
let day = buildHealthSnapshot(records: records, nodeNames: nodes, now: now.addingTimeInterval(1200), minutes: 1440)
precondition(day.hour - day.firstHour == 24 && day.indexed.count == 10000)
let invalid = StatsHour(Hour: fmt.string(from: now), Name: "node-unknown-ai", Probe: nil, Attempts: 1, Successes: 1, Failures: nil, Upload: nil, Download: nil, P95MSUpper: nil)
precondition(buildHealthSnapshot(records: [invalid], nodeNames: nodes, now: now).indexed.isEmpty)
print(String(format: "health snapshot: 200 nodes, 19200 records: %.1f ms", elapsed))
'''
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory, "bench.swift")
            path.write_text(script, encoding="utf-8")
            executable = Path(directory, "bench")
            subprocess.run([swiftc, "-O", str(path), "-o", str(executable)], check=True, timeout=90)
            subprocess.run([str(executable)], check=True, timeout=90)

    def test_subscription_decoding_and_refresh_contract(self):
        swift = shutil.which("swift")
        if not swift:
            self.skipTest("Swift unavailable")
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        definitions = source[source.index("struct SubscriptionInfo:"):source.index("struct UsageBar:")]
        definitions += source[source.index("struct SubscriptionAllowance:"):source.index("struct StatsView:")]
        script = '''import Foundation
''' + definitions + '''
let payload = #"{"profiles":[{"source":"flclash","id":"1","name":"Example","subscription_info":{"upload":10,"download":20,"total":100,"expire":1700000000},"last_update_date":1700000000000,"status":"ok"},{"source":"jiuyi","id":"1","name":null,"subscription_info":{"upload":null,"download":null,"total":null,"expire":null},"last_update_date":null,"status":"unknown"}],"refreshing":true,"updated_at":null}"#
let response = try JSONDecoder().decode(SubscriptionResponse.self, from: Data(payload.utf8))
let first = SubscriptionAllowance(response.profiles[0]), second = SubscriptionAllowance(response.profiles[1])
precondition(response.refreshing && first.id != second.id && first.used == 30 && first.remaining == 70)
precondition(first.expiresAt?.timeIntervalSince1970 == 1700000000 && first.updatedAt?.timeIntervalSince1970 == 1700000000)
precondition(second.used == nil && second.remaining == nil && second.updatedAt == nil)
let exceeded = try JSONDecoder().decode(SubscriptionProfile.self, from: Data(#"{"source":"x","id":"2","name":null,"subscription_info":{"upload":10,"download":20,"total":5,"expire":0},"last_update_date":0,"status":"ok"}"#.utf8))
precondition(SubscriptionAllowance(exceeded).remaining == 0 && SubscriptionAllowance(exceeded).expiresAt == nil)
'''
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory, "subscriptions.swift")
            path.write_text(script, encoding="utf-8")
            subprocess.run([swift, str(path)], check=True, timeout=90)
        self.assertIn('appendingPathComponent("subscriptions")', source)
        self.assertIn('if value.refreshing || self.jiuyiImportBusy { self.scheduleSubscriptionPoll() }', source)
        self.assertIn('nextSubscriptionRefresh = ProcessInfo.processInfo.systemUptime + 60', source)
        self.assertNotIn('订阅余量接口待接入', source)

    def test_sources_priority_and_traffic_sections(self):
        swift = shutil.which("swift")
        if not swift:
            self.skipTest("Swift unavailable")
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        definitions = source[source.index("struct TrafficTotals:"):source.index("struct NodeLabel:")]
        node_request = source[source.index("    func nodeRequest(method:"):source.index("    func refreshNodeGroups()")]
        health_request = source[source.index("    func healthRequest("):source.index("    func refreshNodeTraffic()")]
        sources_request = source[source.index("    func sourcesRequest("):source.index("    func addSubscription(")]
        xlinker_request = source[source.index("    func xlinkerImportRequest("):source.index("    // Rate-limited like subscriptions")]
        settings = source[source.index("struct SettingsView:"):source.index("final class AppDelegate:")]
        stats_view = source[source.index("struct StatsView:"):source.index("struct SettingsView:")]
        self.assertIn("机场配置", settings)
        self.assertIn("保存机场配置", settings)
        self.assertIn("model.moveSource(at: index, offset: -1)", settings)
        self.assertIn("model.moveSource(at: index, offset: 1)", settings)
        self.assertIn("model.setSourceEnabled(at: index, enabled: $0)", settings)
        self.assertIn("model.saveSources()", settings)
        self.assertIn("model.resetSourceDraft()", settings)
        self.assertIn("model.updateAllSources()", settings)
        self.assertIn('ProgressView(value: Double(model.sourcesUpdatePercent ?? 0), total: 100)', settings)
        self.assertIn("更新配置", settings)
        self.assertIn("/api/sources/update", source)
        self.assertIn("VPN 型来源，无订阅地址", settings)
        self.assertIn('Toggle("玖壹模块（本机）"', settings)
        self.assertIn('"/api/office91-module"', source)
        self.assertIn('func deleteSource(_ row: SourceRow)', source)
        self.assertIn('"custom-sub-"', source)
        # Settings moved to index 3 after the domain page was inserted; the
        # source/subscription refreshers must follow it.
        self.assertEqual(source.count("selectedPage == ClashHubSettingsViewIndex"), 3)
        self.assertNotIn("selectedPage == 2, let window", source)
        self.assertIn("AI 线路检测", settings)
        self.assertIn("配置流量 TOP 10", stats_view)
        self.assertIn("已观测流量", stats_view)
        self.assertIn("model.stats?.sourceRows", stats_view)
        self.assertIn('healthRequest(path: "/api/sources"', source)
        self.assertIn('"/api/sources/xlinker/import"', source)
        self.assertIn("后端不可用", source)
        script_text = '''import Foundation
''' + definitions + '''
struct TestModel {
    let cfgHTTP: String
    let support: URL
    var apiToken: String? = "test-token"
''' + node_request + health_request + sources_request + xlinker_request + '''
}
let model = TestModel(cfgHTTP: "127.0.0.1:33333", support: URL(fileURLWithPath: CommandLine.arguments[1]))
let getRequest = try model.sourcesRequest("GET")
precondition(getRequest.url!.path == "/api/sources" && getRequest.httpMethod == "GET" && getRequest.url!.port == 33335)
precondition(getRequest.value(forHTTPHeaderField: "Authorization") == "Bearer test-token")
let putRequest = try model.sourcesRequest("PUT")
precondition(putRequest.url!.path == "/api/sources" && putRequest.httpMethod == "PUT")
let importRequest = try model.xlinkerImportRequest()
precondition(importRequest.url!.path == "/api/sources/xlinker/import" && importRequest.httpMethod == "POST")
precondition(importRequest.value(forHTTPHeaderField: "Authorization") == "Bearer test-token")
let payload = #"{"sources":[{"id":"jiuyi","label":"玖壹·节点","type":"subscription","enabled":true,"node_count":24,"status":"ok","last_error":null},{"id":"office91-port","label":"玖壹·端口(备用)","type":"profile","enabled":true,"node_count":1,"status":"ok","last_error":""},{"id":"xlinker","label":"XLinker · 速联","type":"vpn","enabled":false,"node_count":0,"status":"error","last_error":"未检测到 VPN 进程"}],"base_rules_source":"big-airport","traffic":{"rows":[{"id":"jiuyi","label":"玖壹·节点","upload":10,"download":20},{"id":"office91-port","label":"玖壹·端口(备用)","upload":1,"download":2}],"unknown_upload":5,"unknown_download":6,"note":"部分覆盖"}}"#
let response = try JSONDecoder().decode(SourcesResponse.self, from: Data(payload.utf8))
precondition(response.sources.count == 3 && response.sources[0].nodeCount == 24 && response.sources[0].enabled)
precondition(response.baseRulesSource == "big-airport" && response.traffic?.unknownUpload == 5 && response.traffic?.unknownDownload == 6)
precondition(response.traffic?.rows?.last?.label == "玖壹·端口(备用)")
precondition(vpnSource(response)?.id == "xlinker" && vpnSource(nil) == nil)
precondition(movedSources(response.sources, from: 2, offset: -1)!.map(\\.id) == ["jiuyi", "xlinker", "office91-port"])
precondition(movedSources(response.sources, from: 0, offset: -1) == nil && movedSources(response.sources, from: 2, offset: 1) == nil)
precondition(sourceTypeLabel("profile") == "配置" && sourceTypeLabel("subscription") == "订阅" && sourceTypeLabel("vpn") == "VPN" && sourceTypeLabel("other") == "other")
precondition(sourceStatusText("ok", lastError: nil) == "正常")
precondition(sourceStatusText("error", lastError: "未检测到 VPN 进程") == "不可用：未检测到 VPN 进程")
precondition(sourceStatusText("failed", lastError: "") == "不可用")
precondition(sourceStatusText(nil, lastError: nil) == "状态未知")
precondition(!sourceStatusFailed("ok") && sourceStatusFailed("error"))
let save = SourceSavePayload(sources: [SourceOrderEntry(id: "jiuyi", enabled: true), SourceOrderEntry(id: "xlinker", enabled: false)])
let object = try JSONSerialization.jsonObject(with: JSONEncoder().encode(save)) as! [String: Any]
let entries = object["sources"] as! [[String: Any]]
precondition(entries.count == 2 && entries[0]["id"] as! String == "jiuyi" && entries[0]["enabled"] as! Bool)
precondition(entries[1]["id"] as! String == "xlinker" && (entries[1]["enabled"] as! Bool) == false)
precondition(Set(entries[0].keys) == ["id", "enabled"])
let stats = try JSONDecoder().decode(Statistics.self, from: Data(#"{"source_rows":[{"id":"office91-port","label":"玖壹·端口(备用)","upload":7,"download":9}],"source_unknown_upload":3,"source_unknown_download":4}"#.utf8))
precondition(stats.sourceRows?.first?.id == "office91-port" && stats.sourceRows?.first?.label == "玖壹·端口(备用)")
precondition(stats.sourceRows?.first?.upload == 7 && stats.sourceUnknownUpload == 3 && stats.sourceUnknownDownload == 4)
let legacy = try JSONDecoder().decode(Statistics.self, from: Data(#"{}"#.utf8))
precondition(legacy.sourceRows == nil && legacy.sourceUnknownUpload == nil && legacy.sourceUnknownDownload == nil)
'''
        with tempfile.TemporaryDirectory(dir="/tmp") as directory:
            script = Path(directory, "sources.swift")
            script.write_text(script_text, encoding="utf-8")
            subprocess.run([swift, str(script), directory], check=True, timeout=120)

    def test_menu_sampling_and_backend_contract(self):
        swift = shutil.which("swift")
        if not swift:
            self.skipTest("Swift is unavailable")
        source = Path(__file__).with_name("main.swift").read_text(encoding="utf-8")
        definitions = source[source.index("struct TrafficTotals:"):source.index("struct NodeLabel:")]
        node_request = source[source.index("    func nodeRequest(method:"):source.index("    func refreshNodeGroups()")]
        routing_request = source[source.index("    func routingRequest("):source.index("    func refreshRoutingMode()")]
        script_text = '''import Foundation
''' + definitions + '''
struct TestModel {
    let cfgHTTP: String
    let support: URL
    var apiToken: String? = "test-token"
''' + node_request + routing_request + '''
}
let model = TestModel(cfgHTTP: "127.0.0.1:33333", support: URL(fileURLWithPath: CommandLine.arguments[1]))
let request = try model.routingRequest("PUT")
precondition(request.url!.path == "/api/routing-mode" && request.httpMethod == "PUT")
precondition(request.value(forHTTPHeaderField: "Authorization") == "Bearer test-token")
let mode = try JSONDecoder().decode(RoutingMode.self, from: Data(#"{"desired_mode":"global","actual_mode":"rule","warning":"pending"}"#.utf8))
precondition(mode.desiredMode == "global" && mode.actualMode == "rule" && mode.warning == "pending")
let stats = try JSONDecoder().decode(Statistics.self, from: Data(#"{"traffic_totals":{"upload":100,"download":200},"process_started_at":"start","p95_60s_samples":1,"p95_60s_ms_upper":64}"#.utf8))
precondition(stats.trafficTotals?.upload == 100 && stats.processStartedAt == "start" && stats.p95Upper == 64)
let first = TrafficSample(started: "a", upload: 100, download: 200, time: 10)
let next = TrafficSample(started: "a", upload: 160, download: 320, time: 13)
let rate = next.rate(since: first)!
precondition(rate.0 == 20 && rate.1 == 40)
precondition(TrafficSample(started: "b", upload: 160, download: 320, time: 13).rate(since: first) == nil)
precondition(TrafficSample(started: "a", upload: 99, download: 320, time: 13).rate(since: first) == nil)
precondition(TrafficSample(started: "a", upload: 160, download: 320, time: 10).rate(since: first) == nil)
precondition(TrafficSample(started: "a", upload: 160, download: 320, time: 26).rate(since: first) == nil)
'''
        with tempfile.TemporaryDirectory(dir="/tmp") as directory:
            Path(directory, "api-token").write_text("test-token", encoding="utf-8")
            script = Path(directory, "menu.swift")
            script.write_text(script_text, encoding="utf-8")
            subprocess.run([swift, str(script), directory], check=True, timeout=90)
        self.assertIn('self?.refreshRoutingMode()', source)
        self.assertIn('self?.refresh()', source[source.index('timer = Timer.scheduledTimer'):source.index('    func refreshIfVisible')])
        self.assertIn('else if quitOnSuccess { NSApp.terminate(nil) }', source)
        self.assertIn('if let result { self.operationError = result }', source)
        self.assertIn('runCLI("off", quitOnSuccess: true)', source)


if __name__ == "__main__":
    unittest.main()
