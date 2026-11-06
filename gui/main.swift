import SwiftUI
import AppKit
import Foundation
import CryptoKit
import Darwin
import UniformTypeIdentifiers

// /stats returns rows and window_health_hours for the selected window. Dates are
// RFC3339; health Name is node-<SHA256 UTF-8 node name>-gstatic/ai.
// Historical unclassified records are never attributed to a known node.
struct TrafficTotals: Decodable { let upload: UInt64; let download: UInt64 }
struct TrafficSample {
    let started: String; let upload: UInt64; let download: UInt64; let time: TimeInterval
    func rate(since previous: TrafficSample) -> (Double, Double)? {
        let elapsed = time - previous.time
        guard started == previous.started, elapsed > 0, elapsed <= 15,
              upload >= previous.upload, download >= previous.download else { return nil }
        return (Double(upload - previous.upload) / elapsed, Double(download - previous.download) / elapsed)
    }
}
struct RoutingMode: Decodable, Equatable {
    let desiredMode: String?; let actualMode: String?; let warning: String?
    enum CodingKeys: String, CodingKey { case desiredMode = "desired_mode", actualMode = "actual_mode", warning }
}
struct MenuStatistics: Decodable {
    let p95Upper: UInt64?; let p95Samples: UInt64?; let trafficTotals: TrafficTotals?; let processStartedAt: String?
    enum CodingKeys: String, CodingKey { case p95Upper = "p95_60s_ms_upper", p95Samples = "p95_60s_samples", trafficTotals = "traffic_totals", processStartedAt = "process_started_at" }
}
struct RouteRate: Decodable {
    let uploadBPS: Double; let downloadBPS: Double
    enum CodingKeys: String, CodingKey { case uploadBPS = "upload_bps", downloadBPS = "download_bps" }
}
struct RouteTrafficResponse: Decodable {
    let normal: RouteRate; let ai: RouteRate; let updatedAt: String
    enum CodingKeys: String, CodingKey { case normal, ai, updatedAt = "updated_at" }
}
struct NetworkPoint: Decodable, Equatable {
    let start: String?; let upload: UInt64; let download: UInt64
    enum CodingKeys: String, CodingKey { case start, upload, download }
}
struct Statistics: Decodable {
    var updatedAt: String?
    var rows: [StatsRow]?
    var domainRanking: [DomainRank]?
    var domainHistory: [DomainRank]?
    var healthHours: [StatsHour]?
    var windowHealthHours: [StatsHour]?
    var portRows: [StatsPortRow]?
    var sourceRows: [SourceTrafficRow]?
    var trafficSeries: [NetworkPoint]?
    var sourceUnknownUpload: UInt64?
    var sourceUnknownDownload: UInt64?
    var p95Upper: UInt64?
    var p95Samples: UInt64?
    var trafficTotals: TrafficTotals?
    var processStartedAt: String?
    enum CodingKeys: String, CodingKey { case updatedAt = "updated_at", rows, domainRanking = "domain_ranking", domainHistory = "domain_history", healthHours = "health_hours", windowHealthHours = "window_health_hours", portRows = "port_rows", sourceRows = "source_rows", trafficSeries = "traffic_series", sourceUnknownUpload = "source_unknown_upload", sourceUnknownDownload = "source_unknown_download", p95Upper = "p95_60s_ms_upper", p95Samples = "p95_60s_samples", trafficTotals = "traffic_totals", processStartedAt = "process_started_at" }
    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        updatedAt = try? c.decode(String.self, forKey: .updatedAt)
        rows = (try? c.decode([Lossy<StatsRow>].self, forKey: .rows))?.compactMap(\.value)
        domainRanking = try? c.decode([DomainRank].self, forKey: .domainRanking)
        domainHistory = try? c.decode([DomainRank].self, forKey: .domainHistory)
        healthHours = (try? c.decode([Lossy<StatsHour>].self, forKey: .healthHours))?.compactMap(\.value)
        windowHealthHours = (try? c.decode([Lossy<StatsHour>].self, forKey: .windowHealthHours))?.compactMap(\.value)
        portRows = (try? c.decode([Lossy<StatsPortRow>].self, forKey: .portRows))?.compactMap(\.value)
        // Older backends predate source attribution; absence stays nil so the
        // UI can say "unavailable" instead of showing fabricated zeros.
        sourceRows = (try? c.decode([Lossy<SourceTrafficRow>].self, forKey: .sourceRows))?.compactMap(\.value)
        trafficSeries = (try? c.decode([Lossy<NetworkPoint>].self, forKey: .trafficSeries))?.compactMap(\.value)
        sourceUnknownUpload = try? c.decode(UInt64.self, forKey: .sourceUnknownUpload)
        sourceUnknownDownload = try? c.decode(UInt64.self, forKey: .sourceUnknownDownload)
        p95Upper = try? c.decode(UInt64.self, forKey: .p95Upper)
        p95Samples = try? c.decode(UInt64.self, forKey: .p95Samples)
        trafficTotals = try? c.decode(TrafficTotals.self, forKey: .trafficTotals)
        processStartedAt = try? c.decode(String.self, forKey: .processStartedAt)
    }
}
struct NodePriorityRow: Decodable { let node: String; let priority: String; let override: String; let enabled: Bool }
struct SourcePolicy: Decodable { let id: String; let defaultPriority: String; let downgradePatterns: [String]
    enum CodingKeys: String, CodingKey { case id; case defaultPriority = "default_priority", downgradePatterns = "downgrade_patterns" }
}
struct NodePrioritiesResponse: Decodable { let nodes: [NodePriorityRow]; let sources: [SourcePolicy] }
struct NodePriorityUpdate: Encodable { let node: String; var priority: String? = nil; var enabled: Bool? = nil }
struct Lossy<T: Decodable>: Decodable {
    let value: T?
    init(from decoder: Decoder) throws { value = try? T(from: decoder) }
}
struct StatsPortRow: Decodable {
    var port: UInt16?; var unknown: Bool?; var upload: UInt64?; var download: UInt64?
}
struct DomainRank: Decodable, Identifiable {
    let target: String; let upload: UInt64; let download: UInt64; let detailStartedAt: String?
    let attempts: UInt64?; let attemptsStartedAt: String?; let attemptsComplete: Bool?
    var id: String { target }
    enum CodingKeys: String, CodingKey { case target, upload, download, attempts, detailStartedAt = "detail_started_at", attemptsStartedAt = "attempts_started_at", attemptsComplete = "attempts_complete" }
}
struct StatsRow: Decodable {
    var Target: String?; var Node: String?; var NodeUnknown: Bool?
    var detailStartedAt: String?
    enum CodingKeys: String, CodingKey { case Target, Node, NodeUnknown, Attempts, Successes, Failures, Upload, Download, P95MSUpper, detailStartedAt = "detail_started_at" }
    var Attempts: UInt64?; var Successes: UInt64?; var Failures: UInt64?
    var Upload: UInt64?; var Download: UInt64?; var P95MSUpper: UInt64?
}
struct StatsHour: Decodable {
    var Hour: String?; var Name: String?; var Probe: String?
    var Attempts: UInt64?; var Successes: UInt64?; var Failures: UInt64?
    var Upload: UInt64?; var Download: UInt64?; var P95MSUpper: UInt64?
}
// Backend contract (parallel development): GET /api/sources returns
// {sources:[{id,label,type:profile|subscription|vpn,enabled,node_count,status,
// last_error}], base_rules_source, traffic:{rows:[{id,label,upload,download}],
// unknown_upload, unknown_download, note}} on the same listener and token as
// /api/nodes. PUT accepts {"sources":[{"id","enabled"}...]} where array order
// encodes priority. /stats carries source_rows with the same row shape.
// A malformed source row fails the whole decode: silently dropping a row from
// a subsequent PUT could disable or delete it server-side.
struct SourceRow: Decodable, Equatable, Identifiable {
    let id: String; let label: String; let type: String
    var enabled: Bool
    var defaultPriority: String?
    let nodeCount: Int?; let status: String?; let lastError: String?
    let canEditSubscription: Bool?
    var includeKeywords: [String]?; var excludeKeywords: [String]?
    enum CodingKeys: String, CodingKey { case id, label, type, enabled, status; case includeKeywords = "include_keywords", excludeKeywords = "exclude_keywords"; case nodeCount = "node_count", lastError = "last_error", defaultPriority = "default_priority", canEditSubscription = "can_edit_subscription" }
}
struct SourceTrafficRow: Decodable, Equatable, Identifiable {
    var id: String; var label: String; var upload: UInt64?; var download: UInt64?
}
struct SourceTraffic: Decodable, Equatable {
    var rows: [SourceTrafficRow]?; var unknownUpload: UInt64?; var unknownDownload: UInt64?; var note: String?
    enum CodingKeys: String, CodingKey { case rows, note; case unknownUpload = "unknown_upload", unknownDownload = "unknown_download" }
}
struct SourcesResponse: Decodable, Equatable {
    var sources: [SourceRow]; var baseRulesSource: String?; var traffic: SourceTraffic?
    var priorityActive: Bool?; var priorityStatus: String?
    enum CodingKeys: String, CodingKey { case sources, traffic; case baseRulesSource = "base_rules_source", priorityActive = "priority_active", priorityStatus = "priority_status" }
}
struct SourceOrderEntry: Encodable, Equatable {
    let id: String; let enabled: Bool; var defaultPriority: String?; var downgradePatterns: [String]?; var includeKeywords: [String]?; var excludeKeywords: [String]?
    enum CodingKeys: String, CodingKey { case id, enabled; case defaultPriority = "default_priority", downgradePatterns = "downgrade_patterns", includeKeywords = "include_keywords", excludeKeywords = "exclude_keywords" }
}
struct SourceSavePayload: Encodable { let sources: [SourceOrderEntry] }
struct SubscriptionActionResult: Decodable { let id: String; let status: String }
// Reordering stays a pure function so priority edits are testable off the UI.
func movedSources(_ sources: [SourceRow], from index: Int, offset: Int) -> [SourceRow]? {
    let target = index + offset
    guard sources.indices.contains(index), sources.indices.contains(target) else { return nil }
    var moved = sources
    moved.swapAt(index, target)
    return moved
}
func vpnSource(_ response: SourcesResponse?) -> SourceRow? {
    response?.sources.first { $0.type == "vpn" }
}
// Status strings beyond "ok" are not pinned down by the contract yet; map the
// known failure spellings, pass anything else through verbatim, and never
// invent an interpretation.
func sourceStatusText(_ status: String?, lastError: String?) -> String {
    let reason = lastError.flatMap { $0.isEmpty ? nil : $0 }
    switch status {
    case "ok": return "正常"
    case "unavailable", "error", "failed": return reason.map { "不可用：\($0)" } ?? "不可用"
    case "empty": return "无可用节点"
    case "disabled": return "已停用"
    case "unknown": return "状态未知"
    case nil, "": return "状态未知"
    case let other?: return reason.map { "\(other)：\($0)" } ?? other
    }
}
func sourceStatusFailed(_ status: String?) -> Bool {
    ["unavailable", "error", "failed"].contains(status ?? "")
}
func sourceTypeLabel(_ type: String) -> String {
    switch type { case "profile": return "配置"; case "subscription": return "订阅"; case "vpn": return "VPN"; default: return type } 
}
struct NodeLabel: Decodable, Equatable { let profile: String; let name: String }
struct NodeLabels: Decodable { let nodes: [String: NodeLabel] }

// A generated name authenticates its display label independently of mutable
// sidecar/config files. Old names and malformed or mismatched labels fail closed.
func verifiedNodeLabel(_ key: String, _ label: NodeLabel) -> Bool {
    guard !label.profile.isEmpty, !label.name.isEmpty else { return false }
    let profile = Data(label.profile.utf8)
    var length = UInt64(profile.count).bigEndian
    var input = Data("srhop-label-v1\0".utf8)
    withUnsafeBytes(of: &length) { input.append(contentsOf: $0) }
    input.append(profile)
    input.append(contentsOf: label.name.utf8)
    let digest = SHA256.hash(data: input).map { String(format: "%02x", $0) }.joined()
    return key.hasSuffix("-lbl-" + digest)
}
func verifiedLabels(_ data: Data) -> [String: NodeLabel] {
    guard let decoded = try? JSONDecoder().decode(NodeLabels.self, from: data) else { return [:] }
    return decoded.nodes.filter { verifiedNodeLabel($0.key, $0.value) }
}
struct CaptureEvent: Decodable {
    let time: String; let kind: String; let port: String; let result: String
    let durationMS: Int64; let upload: Int64; let download: Int64; let method: String?; let status: Int?
    enum CodingKeys: String, CodingKey { case time, kind, port, result, method, status; case durationMS = "duration_ms", upload = "upload_bytes", download = "download_bytes" }
}
struct CaptureStatus: Decodable {
    let enabled: Bool; let domain: String; let expires: String; let maxEvents: Int; let events: [CaptureEvent]; let storageError: Bool; let decryptSupported: Bool
    enum CodingKeys: String, CodingKey { case enabled, domain, expires, events; case maxEvents = "max_events", storageError = "storage_error", decryptSupported = "decrypt_supported" }
}
struct CaptureConfig: Encodable {
    let enabled: Bool; let domain: String; let decrypt = false; let durationSeconds: Int; let maxEvents: Int
    enum CodingKeys: String, CodingKey { case enabled, domain, decrypt; case durationSeconds = "duration_seconds", maxEvents = "max_events" }
}
struct BlockedDomains: Codable { let domains: [String] }
struct ProxyHistory: Decodable, Equatable { let delay: Int?; let time: String? }
struct ProxyInfo: Decodable, Equatable { let type: String?; let now: String?; let history: [ProxyHistory]?; let all: [String]? }
struct ProxiesResponse: Decodable { let proxies: [String: ProxyInfo] }
final class LocalAPIDelegate: NSObject, URLSessionTaskDelegate {
    func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse, newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) { completionHandler(nil) }
}
let localAPIDelegate = LocalAPIDelegate()
let localAPISession: URLSession = {
    let config = URLSessionConfiguration.ephemeral
    config.connectionProxyDictionary = ["HTTPEnable": 0, "HTTPSEnable": 0, "SOCKSEnable": 0]
    return URLSession(configuration: config, delegate: localAPIDelegate, delegateQueue: nil)
}()
// Bound the entire mutation, not just idle time between response bytes.
let nodeMutationSession: URLSession = {
    let config = URLSessionConfiguration.ephemeral
    config.connectionProxyDictionary = ["HTTPEnable": 0, "HTTPSEnable": 0, "SOCKSEnable": 0]
    config.timeoutIntervalForResource = 8
    return URLSession(configuration: config, delegate: localAPIDelegate, delegateQueue: nil)
}()
struct NodeGroup: Decodable, Equatable {
    let group: String; let mode: String; let selected: String?; let manualNode: String?; let nodes: [String]
    let desiredMode: String?; let warning: String?
    let recoveryStatus: String?; let recoveryReason: String?
    enum CodingKeys: String, CodingKey {
        case group, mode, selected, nodes, warning
        case manualNode = "manual_node", desiredMode = "desired_mode"
        case recoveryStatus = "recovery_status", recoveryReason = "recovery_reason"
    }
}
// Recovery is a point-in-time backend result, not proof of future reachability.
func manualRecoveryNotice(_ status: String?, reason: String?) -> String {
    let detail: String
    switch reason {
    case "routing preference unavailable or direct": detail = "路由模式不可用或为直连"
    case "controller routing mode unavailable or differs from preference": detail = "控制器路由模式不可用或与配置不一致"
    case "AI selector is not used in global mode": detail = "全局模式不使用 openai 选择器"
    case "global selector is not using the primary group": detail = "全局选择器未使用主代理"
    case "shutting down": detail = "服务正在关闭"
    case "routing changed or recovery already running": detail = "路由已变化或恢复正在进行"
    case "selected AI node probe failed or inconclusive": detail = "openai 节点探测失败或结果不确定"
    case "selected node probe failed or inconclusive": detail = "节点探测失败或结果不确定"
    case "verification timed out or canceled": detail = "验证超时或已取消"
    case "routing changed during verification": detail = "验证期间路由发生变化"
    case "manual preference changed during verification": detail = "验证期间手动偏好发生变化"
    case "selected node could not be confirmed": detail = "无法确认选择器节点"
    default:
        if let reason, reason.hasPrefix("primary route probe failed:") { detail = "主线路探测失败（\(reason)）" }
        else if let reason, !reason.isEmpty { detail = reason }
        else { detail = "原因未知" }
    }
    switch status {
    case "switched_unverified_path": return "选择器已确认，已切回主线路；实际路径和连通性尚未验证，请查看线路状态。"
    case "recovered": return "手动偏好已保存；后端报告切回验证通过，请查看当前线路状态。"
    case "failed": return "手动偏好已保存；切回验证失败或未确认：\(detail)。请查看实际线路。"
    case "not_attempted": return "手动偏好已保存；未尝试切回：\(detail)。请查看实际线路。"
    case "not_needed": return "手动偏好已保存；无需切回或备用状态已变化，请查看实际线路。"
    default: return "手动偏好已保存；切回结果未知，请查看实际线路。"
    }
}
struct NodeGroups: Decodable { let groups: [NodeGroup] }
struct NodeProbe: Decodable, Equatable {
    let status: String; let delayMS: Int?; let checkedAt: String?; let expiresAt: String?; let stale: Bool?; let error: String?
    enum CodingKeys: String, CodingKey { case status, stale, error; case delayMS = "delay_ms", checkedAt = "checked_at", expiresAt = "expires_at" }
}
struct NodeHealth: Decodable, Equatable {
    let name: String; let normal: NodeProbe?; let ai: NodeProbe?; let chatgpt: NodeProbe?
    // Per-provider results for the configured ai_targets selection.
    var targets: [String: NodeProbe]?
    enum CodingKeys: String, CodingKey { case name, normal, ai, chatgpt, targets }
    func probe(for target: String) -> NodeProbe? {
        if let t = targets, let p = t[target] { return p }
        switch target {
        case "openai": return ai
        case "chatgpt": return chatgpt
        default: return nil
        }
    }
}
struct NodeTrafficEntry: Decodable, Equatable {
    let node: String; let upload: UInt64; let download: UInt64; let uploadBPS: Double?; let downloadBPS: Double?; let rateAvailable: Bool
    enum CodingKeys: String, CodingKey { case node, upload, download, uploadBPS = "upload_bps", downloadBPS = "download_bps", rateAvailable = "rate_available" }
}
struct NodeTrafficResponse: Decodable, Equatable {
    let updatedAt: String?; let coverage: String; let samplingStatus: String; let windowMinutes: Int; let nodes: [NodeTrafficEntry]
    enum CodingKeys: String, CodingKey { case updatedAt = "updated_at", coverage, samplingStatus = "sampling_status", windowMinutes = "window_minutes", nodes }
}
// Only observed positive traffic is evidence of use; probes and zero counters are not.
func usedHealthNodes(_ traffic: NodeTrafficResponse?, knownNodes: Set<String>, minutes: Int = 2880) -> [String] {
    guard let traffic, traffic.coverage == "partial", traffic.windowMinutes == minutes else { return [] }
    return Array(Set(traffic.nodes.filter { ($0.upload > 0 || $0.download > 0) && knownNodes.contains($0.node) }.map(\.node)))
        .sorted { $0.localizedStandardCompare($1) == .orderedAscending }
}
struct SourceUpdateProgress: Decodable {
    let running: Bool; let total: Int; let remaining: Int; let percent: Int
}
struct NodeHealthResponse: Decodable {
    let nodes: [NodeHealth]; let running: Bool; let error: String?; let fallbackProbe: NodeProbe?
    let progressDone: Int?; let progressTotal: Int?
    enum CodingKeys: String, CodingKey { case nodes, running, error, fallbackProbe = "fallback_probe", progressDone = "progress_done", progressTotal = "progress_total" }
}

let nonNodeTypes: Set<String> = ["urltest", "fallback", "selector", "loadbalance", "relay", "compatible", "direct", "reject", "pass", "rejectdrop", "smart"]
func isConcreteNode(_ name: String, in proxies: [String: ProxyInfo]) -> Bool {
    guard let info = proxies[name], let type = info.type, !type.isEmpty else { return false }
    return !nonNodeTypes.contains(type.lowercased()) && info.all == nil
}
// nil means the controller did not provide a usable aggregate, not an empty pool.
func japanCandidates(_ proxies: [String: ProxyInfo]) -> [String]? {
    guard let group = proxies["JP-ALL"], group.type?.lowercased() == "urltest", let all = group.all else { return nil }
    guard all.filter({ $0 != "FALLBACK-LOCAL" && $0 != "PASS-RULE" }).allSatisfy({ proxies[$0]?.type?.isEmpty == false }) else { return nil }
    return Array(Set(all.filter { $0 != "FALLBACK-LOCAL" && $0 != "PASS-RULE" && isConcreteNode($0, in: proxies) }))
        .sorted { $0.localizedStandardCompare($1) == .orderedAscending }
}
struct SrhopState: Decodable, Equatable {
    let active: Bool?; let ready: Bool?; let pid: Int?; let enginePid: Int?
    let mode: String?; let upstream: String?; let aiCode: Int?; let aiMs: Int?
    let egress: String?; let egressOk: Bool?; let aiHealthy: Bool?; let updatedAt: String?
    let fallbackReason: String?; let fallbackFailedAt: String?; let nextRetry: String?
    enum CodingKeys: String, CodingKey {
        case active, ready, pid, mode, upstream, egress
        case updatedAt = "updated_at", fallbackReason = "fallback_reason", fallbackFailedAt = "fallback_failed_at", nextRetry = "next_retry"
        case enginePid = "engine_pid", aiCode = "ai_code", aiMs = "ai_ms", egressOk = "egress_ok", aiHealthy = "ai_healthy"
    }
}
func routeQuality(_ probe: NodeProbe?, running: Bool, now: Date = Date()) -> String {
    guard !running, let probe, probe.stale != true, let checked = parseTime(probe.checkedAt),
          checked <= now, let expires = parseTime(probe.expiresAt), expires > now else { return "未知" }
    if probe.status == "failed" { return "不通" }
    guard probe.status == "ok", let delay = probe.delayMS, delay >= 0 else { return "未知" }
    return delay <= 500 ? "通畅" : "较慢"
}
func parseTime(_ text: String?) -> Date? {
    guard let text else { return nil }
    // Formatters are mutable; a per-thread pair avoids cross-thread sharing.
    let cache = Thread.current.threadDictionary
    let key = "clashhub.iso8601.formatters"
    let formatters: [ISO8601DateFormatter]
    if let existing = cache[key] as? [ISO8601DateFormatter] { formatters = existing }
    else {
        let fractional = ISO8601DateFormatter()
        fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        formatters = [fractional, ISO8601DateFormatter()]
        cache[key] = formatters
    }
    return formatters[0].date(from: text) ?? formatters[1].date(from: text)
}

func parseEngineValues(_ text: String) -> [String: String] {
    var values: [String: String] = [:]
    for line in text.split(separator: "\n") {
        let s = line.trimmingCharacters(in: .whitespaces)
        for key in ["secret", "external-controller"] where values[key] == nil && s.hasPrefix(key + ":") {
            values[key] = String(s.dropFirst(key.count + 1)).trimmingCharacters(in: .whitespaces).trimmingCharacters(in: CharacterSet(charactersIn: "\"'"))
        }
    }
    return values
}

let ClashHubDomainViewIndex = 2
let ClashHubSettingsViewIndex = 3

enum StatsWindow: Int, CaseIterable {
    case hour = 60, day = 1440, twoDays = 2880
    var label: String { switch self { case .hour: return "1小时"; case .day: return "1天"; case .twoDays: return "2天" } }
}

@MainActor final class Model: ObservableObject {
    // Accessed exclusively on snapshotQueue.
    private final class FileParseCache: @unchecked Sendable {
        var lastLabelsData: Data?; var labels: [String: NodeLabel] = [:]
        var lastStateData: Data?; var state: SrhopState?
        var lastEngineText: String?; var engineValues: [String: String] = [:]
    }
    @Published var selectedPage = 0
    @Published var state: SrhopState?
    @Published var proxies: [String: ProxyInfo] = [:] { didSet { scheduleHealthSnapshot() } }
    private(set) var nodeNames: [String] = []
    private var knownNodes: Set<String> = []
    @Published var nodeError: String?
    @Published var nodeGroups: [String: NodeGroup] = [:]
    @Published var nodePriorities: [String: NodePriorityRow] = [:]
    @Published private(set) var sourcePolicies: [String: SourcePolicy] = [:]
    @Published private(set) var sourcePolicyPriorities: [String: String] = [:]
    @Published private(set) var sourcePolicyPatterns: [String: String] = [:]
    @Published var includeDraft: [String: String] = [:]
    @Published var excludeDraft: [String: String] = [:]
    private var editedSourcePriorities: Set<String> = []
    private var editedSourcePatterns: Set<String> = []
    @Published var prioritiesLoaded = false
    @Published var prioritiesBusy = false
    @Published var prioritiesError: String?
    @Published var prioritiesNotice: String?
    @Published var cfgAggregateKeywords = ""
    @Published var nodeControlError: String?
    @Published var nodeControlNotice: String?
    @Published var nodeControlBusy = false
    @Published var readingNodeGroups = false
    @Published var nodeHealth: [String: NodeHealth] = [:]
    @Published var fallbackProbe: NodeProbe?
    @Published var healthError: String?
    @Published var healthRunning = false
    @Published var healthStarting = false
    @Published var healthProgressDone = 0
    @Published var healthProgressTotal = 0
    private var readingHealth = false
    @Published var nodesUpdated: Date?
    @Published var stats: Statistics? { didSet { scheduleHealthSnapshot() } }
    @Published var statsWindow: StatsWindow = .twoDays { didSet {
        guard statsWindow != oldValue else { return }
        statsGeneration += 1; refreshingStats = false; statsError = nil
        refreshStats()
    } }
    @Published private(set) var displayedStatsWindow: StatsWindow?
    @Published private(set) var statsWindowTraffic: NodeTrafficResponse?
    @Published var statsError: String?
    @Published var statsReadAt: Date?
    @Published var captureStatus: CaptureStatus?
    @Published var captureError: String?
    @Published var captureBusy = false
    private var readingCapture = false
    @Published var blockedDomains: Set<String>?
    @Published var blockError: String?
    @Published var blockNotice: String?
    @Published var blockBusy = false
    @Published var nodeTraffic: NodeTrafficResponse?
    @Published private(set) var routeTraffic: RouteTrafficResponse?
    private var readingRouteTraffic = false
    private var lastRouteTrafficAt: TimeInterval = 0
    private var trafficByNode: [String: NodeTrafficEntry] = [:]
    private var readingNodeTraffic = false
    @Published var menuP95: UInt64?
    @Published private(set) var menuTrafficTotals: TrafficTotals?
    @Published var uploadRate: Double?
    @Published var downloadRate: Double?
    @Published var routingMode: RoutingMode?
    @Published var modeError: String?
    @Published var operationError: String?
    @Published var operationBusy = false
    @Published var modeBusy = false
    private var readingMode = false
    var readingModeForMenu: Bool { readingMode }
    private var trafficSample: TrafficSample?
    private var refreshingMenuP95 = false
    @Published var configError: String?
    @Published var message: String?
    @Published var cfgPrimary = ""; @Published var cfgFallback = ""
    @Published var cfgFallbackList: [String] = []
    @Published var subscriptionURLDrafts: [String: String] = [:]
    @Published var subscriptionURLBusy: Set<String> = []
    @Published var subscriptionURLNotice: String?
    @Published var subscriptionURLError: String?
    @Published var cfgController = ""; @Published var cfgHTTP = ""; @Published var cfgSOCKS = ""
    @Published var cfgProbe = ""; @Published var cfgAIProbe = ""
    @Published var cfgAITargets: [String] = []
    @Published var cfgOffice91Enabled = false
    @Published private(set) var office91ModuleBusy = false
    @Published private(set) var deletingSourceID: String?
    @Published private(set) var sourcesUpdateBusy = false
    @Published private(set) var sourcesUpdatePercent: Int?
    @Published var cfgRequireJP = false; @Published var cfgNotify = false
    @Published var strategyPrimary = "stability"; @Published var strategyAI = "latency"
    @Published var strategyLatencyGap = "150"; @Published var strategyBalanceGap = "500"
    @Published private(set) var strategyLoading = false
    @Published private(set) var strategySaving = false
    @Published var strategyError: String?
    @Published var strategyNotice: String?
    @Published private(set) var strategyLoaded = false
    @Published var labels: [String: NodeLabel] = [:]
    @Published private(set) var subscriptions: [SubscriptionAllowance] = []
    @Published private(set) var subscriptionsLoaded = false
    @Published private(set) var subscriptionsRefreshing = false
    @Published private(set) var subscriptionsError: String?
    @Published var newSubscriptionName = ""
    @Published var newSubscriptionURL = ""
    @Published private(set) var addingSubscription = false
    @Published private(set) var addSubscriptionMessage: String?
    @Published var editingSubscriptionID: String?
    @Published private(set) var jiuyiEnabled = false
    @Published private(set) var jiuyiOfflineStatus: JiuyiOfflineStatus?
    @Published private(set) var jiuyiOfflineMode: String?
    @Published private(set) var jiuyiImportBusy = false
    @Published private(set) var jiuyiImportNotice: String?
    @Published private(set) var jiuyiImportError: String?
    private var jiuyiAttemptBaseline: String?
    private var jiuyiPollDeadline: TimeInterval = 0
    private var jiuyiPollGeneration = 0
    private var jiuyiImportAcknowledged = false

    private var readingSubscriptions = false
    private var nextSubscriptionRefresh: TimeInterval = 0
    private var subscriptionPollScheduled = false
    // Node source list, priority draft and XLinker import state. The draft is a
    // working copy of sources; it only follows the server while unmodified.
    @Published private(set) var sources: SourcesResponse?
    @Published private(set) var sourcesLoaded = false
    @Published private(set) var sourcesError: String?
    @Published private(set) var sourceDraft: [SourceRow] = []
    @Published private(set) var sourcesDirty = false
    @Published private(set) var sourcesSaveBusy = false
    @Published private(set) var sourcesSaveError: String?
    @Published private(set) var sourcesNotice: String?
    @Published private(set) var xlinkerImportBusy = false
    @Published private(set) var xlinkerNotice: String?
    @Published private(set) var xlinkerError: String?
    private var readingSources = false
    private var nextSourcesRefresh: TimeInterval = 0
    var xlinkerSource: SourceRow? { vpnSource(sources) }
    @Published private(set) var healthSnapshot = HealthSnapshot()
    private(set) var healthRevision = 0
    @Published private(set) var targetRows: [TargetTraffic] = []
    @Published private(set) var sortedPorts: [StatsPortRow] = []
    @Published private(set) var rankedDomains: [DomainRank] = []
    private var healthGeneration = 0
    private let healthQueue = DispatchQueue(label: "clashhub.health.snapshot", qos: .userInitiated)
    private var healthWork: DispatchWorkItem?
    private let snapshotQueue = DispatchQueue(label: "clashhub.files.snapshot", qos: .utility)
    private let fileCache = FileParseCache()
    private func scheduleHealthSnapshot() {
        healthGeneration += 1
        let generation = healthGeneration, records = stats?.windowHealthHours ?? [], known = knownNodes, traffic = statsWindowTraffic, previous = healthSnapshot
        let minutes = displayedStatsWindow?.rawValue ?? 60, now = parseTime(stats?.updatedAt) ?? Date()
        let aiTargets = cfgAITargets
        healthWork?.cancel()
        let work = DispatchWorkItem { [weak self] in
            let names = usedHealthNodes(traffic, knownNodes: known, minutes: minutes)
            let snapshot = buildHealthSnapshot(records: records, nodeNames: names, now: now, minutes: minutes, aiTargets: aiTargets)
            let changed = previous != snapshot
            Task { @MainActor in
                guard let self, self.healthGeneration == generation else { return }
                if changed { self.healthRevision += 1; self.healthSnapshot = snapshot }
            }
        }
        healthWork = work
        healthQueue.async(execute: work)
    }
    private var apiToken: String?
    private var engineValues: [String: String] = [:]
    private var readingCredentials = false
    private var configLoaded = false
    private var timer: Timer?
    private var refreshing = false
    private var readingSnapshot = false
    private var refreshingStats = false
    private var statsGeneration = 0
    weak var window: NSWindow?
    private let home = FileManager.default.homeDirectoryForCurrentUser
    var support: URL {
        let url = home.appendingPathComponent("Library/Application Support/ClashHub")
        let legacy = home.appendingPathComponent("Library/Application Support/srhop")
        let newConfig = url.appendingPathComponent("config.json")
        let oldConfig = legacy.appendingPathComponent("config.json")
        if !FileManager.default.fileExists(atPath: newConfig.path),
           FileManager.default.fileExists(atPath: oldConfig.path) {
            if FileManager.default.fileExists(atPath: url.path) {
                try? FileManager.default.removeItem(at: url)
            }
            try? FileManager.default.moveItem(at: legacy, to: url)
        }
        return url
    }
    var stateURL: URL { support.appendingPathComponent("state.json") }
    var configURL: URL { support.appendingPathComponent("config.json") }
    let statsURL = URL(string: "http://127.0.0.1:33335/stats?minutes=60")!
    var engineURL: URL { support.appendingPathComponent("mihomo/config.yaml") }
    init() {
        loadConfig()
        refreshCredentials()
        timer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in
            Task { @MainActor in
                self?.refreshCredentials()
                self?.refreshMenuP95()
                self?.refresh()
                self?.refreshRoutingMode()
                self?.refreshNodeTraffic()
                self?.refreshNodeHealth()
            }
        }
        if let timer { RunLoop.main.add(timer, forMode: .common) }
        refreshMenuP95()
        refresh()
        refreshRoutingMode()
        refreshNodeTraffic()
        refreshNodeHealth()
    }
    // Explicit menu refresh remains available while the main window is hidden.
    // These readers only fetch snapshots; they do not enable or alter the proxy.
    func refreshMenu() {
        refresh(includePage: false)
        refreshMenuP95()
        refreshRoutingMode()
        refreshNodeGroups()
        refreshNodeTraffic()
    }
    func refreshIfVisible() {
        guard let window, window.isVisible, !window.isMiniaturized, !NSApp.isHidden else { return }
        refresh()
    }
    func refresh(includePage: Bool = true) {
        refreshRouteTraffic()
        if includePage, let window, window.isVisible, !window.isMiniaturized, !NSApp.isHidden {
            if selectedPage == 0 { refreshNodeGroups(); if !prioritiesLoaded { refreshNodePriorities() } }
            if selectedPage == 1 { refreshStats(); refreshBlockedDomains(); refreshCapture() }
            if selectedPage == ClashHubDomainViewIndex { refreshBlockedDomains() }
            if selectedPage == ClashHubSettingsViewIndex { refreshSubscriptions(force: (subscriptionsRefreshing || jiuyiImportBusy) && !subscriptionPollScheduled); refreshSources(); if !prioritiesLoaded { refreshNodePriorities() } }
        }
        guard !refreshing else { return }
        // File parsing and label verification must not block menu interaction.
        if !readingSnapshot {
            readingSnapshot = true
            let labelsURL = support.appendingPathComponent("node-labels.json")
            let stateURL = self.stateURL
            let cache = fileCache
            snapshotQueue.async { [weak self] in
                let labelsData = try? Data(contentsOf: labelsURL)
                if labelsData != cache.lastLabelsData {
                    cache.lastLabelsData = labelsData
                    cache.labels = labelsData.map(verifiedLabels) ?? [:]
                }
                let stateData = try? Data(contentsOf: stateURL)
                if stateData != cache.lastStateData {
                    cache.lastStateData = stateData
                    cache.state = stateData.flatMap { try? JSONDecoder().decode(SrhopState.self, from: $0) }
                }
                let labels = cache.labels, state = cache.state
                Task { @MainActor in
                    guard let self else { return }; if self.labels != labels { self.labels = labels }; if self.state != state { self.state = state }; self.readingSnapshot = false
                }
            }
        }
        guard let url = URL(string: "http://\(controller())/proxies"),
              let host = url.host, ["localhost", "127.0.0.1", "::1"].contains(host) else {
            nodeError = "控制器地址无效或不是本机地址"; return
        }
        var request = URLRequest(url: url)
        let secret = engineValue("secret")
        if !secret.isEmpty { request.setValue("Bearer \(secret)", forHTTPHeaderField: "Authorization") }
        request.timeoutInterval = 5
        refreshing = true
        URLSession.shared.dataTask(with: request) { [weak self] data, response, error in
            let result: Result<ProxiesResponse, Error>
            if let error { result = .failure(error) }
            else if let http = response as? HTTPURLResponse, !(200..<300).contains(http.statusCode) {
                result = .failure(NSError(domain: "HTTP", code: http.statusCode, userInfo: [NSLocalizedDescriptionKey: "HTTP \(http.statusCode)"]))
            } else {
                do { result = .success(try JSONDecoder().decode(ProxiesResponse.self, from: data ?? Data())) }
                catch { result = .failure(error) }
            }
            let indexes = (try? result.get()).map { response in
                (japanCandidates(response.proxies) ?? [], Set(response.proxies.keys.filter { isConcreteNode($0, in: response.proxies) }))
            }
            Task { @MainActor in
                guard let self else { return }
                self.refreshing = false
                switch result {
                case .success(let response):
                    if self.proxies != response.proxies {
                        self.nodeNames = indexes?.0 ?? []; self.knownNodes = indexes?.1 ?? []
                        self.proxies = response.proxies
                    }
                    self.nodesUpdated = Date(); self.nodeError = nil
                case .failure(let error): self.nodeError = "节点读取失败：\(error.localizedDescription)"
                }
            }
        }.resume()
    }
    // Backend contract: authenticated GET/PUT /api/routing-mode on the same
    // listener as /api/nodes; PUT {mode: rule|global|direct}, response reports
    // desired_mode, actual_mode and warning. Never assume a requested mode took effect.
    func routingRequest(_ method: String) throws -> URLRequest {
        var request = try nodeRequest(method: method)
        request.url = request.url?.deletingLastPathComponent().appendingPathComponent("routing-mode")
        return request
    }
    func refreshRoutingMode() {
        guard !readingMode, !modeBusy else { return }
        do {
            let request = try routingRequest("GET")
            readingMode = true
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let result: Result<RoutingMode, Error>
                if let error { result = .failure(error) }
                else if let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) {
                    result = Result { try JSONDecoder().decode(RoutingMode.self, from: data ?? Data()) }
                } else { result = .failure(NSError(domain: "HTTP", code: (response as? HTTPURLResponse)?.statusCode ?? 0, userInfo: [NSLocalizedDescriptionKey: "HTTP \((response as? HTTPURLResponse)?.statusCode ?? 0)"])) }
                Task { @MainActor in
                    guard let self else { return }; self.readingMode = false
                    switch result {
                    case .success(let mode): if self.routingMode != mode { self.routingMode = mode }; if self.modeError != nil { self.modeError = nil }
                    case .failure(let error): self.routingMode = nil; self.modeError = "模式不可用：\(error.localizedDescription)"
                    }
                }
            }.resume()
        } catch { routingMode = nil; modeError = "模式不可用：\(error.localizedDescription)" }
    }
    func setRoutingMode(_ mode: String) {
        guard ["rule", "global", "direct"].contains(mode), !modeBusy, !readingMode, routingMode != nil else { return }
        do {
            var request = try routingRequest("PUT")
            request.httpBody = try JSONEncoder().encode(["mode": mode])
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            modeBusy = true; modeError = nil
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let result: Result<RoutingMode, Error>
                if let error { result = .failure(error) }
                else if let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) {
                    result = Result { try JSONDecoder().decode(RoutingMode.self, from: data ?? Data()) }
                } else {
                    let code = (response as? HTTPURLResponse)?.statusCode ?? 0
                    let detail = data.flatMap { String(data: $0.prefix(1024), encoding: .utf8) } ?? ""
                    result = .failure(NSError(domain: "HTTP", code: code, userInfo: [NSLocalizedDescriptionKey: "HTTP \(code)：\(detail)"]))
                }
                Task { @MainActor in
                    guard let self else { return }; self.modeBusy = false
                    switch result {
                    case .success(let value): self.routingMode = value
                    case .failure(let error): self.routingMode = nil; self.modeError = "模式操作结果未确认：\(error.localizedDescription)"
                    }
                }
            }.resume()
        } catch { modeError = "模式操作未提交：\(error.localizedDescription)" }
    }
    // Run the existing CLI, which owns proxy restoration and shutdown safety.
    // Capture output outside the main thread; a non-zero exit never terminates the GUI.
    func runCLI(_ command: String, quitOnSuccess: Bool = false) {
        guard !operationBusy, ["on", "off", "sysproxy"].contains(command) else { return }
        operationBusy = true; operationError = nil
        // Prefer a bundled daemon (standalone .app) and fall back to the
        // conventional ~/clashhub checkout used by development installs.
        let bundled = Bundle.main.resourceURL?.appendingPathComponent("clashhub")
        let executable = (bundled.map { FileManager.default.isExecutableFile(atPath: $0.path) } == true)
            ? bundled! : home.appendingPathComponent("clashhub/clashhub")
        var environment = ProcessInfo.processInfo.environment
        if let script = Bundle.main.resourceURL?.appendingPathComponent("gen_config.py"),
           FileManager.default.fileExists(atPath: script.path) {
            environment["CLASHHUB_GEN_CONFIG"] = script.path
        }
        DispatchQueue.global(qos: .userInitiated).async { [weak self] in
            let process = Process(); process.executableURL = executable; process.arguments = [command]
            process.environment = environment
            let pipe = Pipe(); process.standardOutput = pipe; process.standardError = pipe
            var failure: String?
            do {
                try process.run()
                let output = pipe.fileHandleForReading.readDataToEndOfFile()
                process.waitUntilExit()
                if process.terminationReason != .exit || process.terminationStatus != 0 {
                    let text = String(data: output.suffix(8192), encoding: .utf8)?.trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
                    failure = "命令 \(command) 失败（\(process.terminationStatus)）：\(text.isEmpty ? "无错误详情" : text)"
                }
            } catch { failure = "无法运行 \(executable.path)：\(error.localizedDescription)" }
            let result = failure
            Task { @MainActor in
                guard let self else { return }; self.operationBusy = false
                if let result { self.operationError = result }
                else if quitOnSuccess { NSApp.terminate(nil) }
                self.refresh(); self.refreshRoutingMode(); self.refreshMenuP95()
            }
        }
    }
    func confirmQuit() {
        let alert = NSAlert()
        alert.messageText = "退出 ClashHub 界面？"
        alert.informativeText = "仅退出界面会保持代理运行；停用并退出会先恢复系统代理，失败时界面将保留。"
        alert.addButton(withTitle: "仅退出界面")
        alert.addButton(withTitle: "停用并退出")
        alert.addButton(withTitle: "取消")
        switch alert.runModal() {
        case .alertFirstButtonReturn: NSApp.terminate(nil)
        case .alertSecondButtonReturn: runCLI("off", quitOnSuccess: true)
        default: break
        }
    }
    func captureRequest(_ method: String) throws -> URLRequest {
        var request = try nodeRequest(method: method)
        request.url = request.url?.deletingLastPathComponent().appendingPathComponent("capture")
        return request
    }
    func refreshCapture() {
        guard !readingCapture, !captureBusy else { return }
        do {
            let request = try captureRequest("GET")
            readingCapture = true
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let result: Result<CaptureStatus, Error>
                if let error { result = .failure(error) }
                else if let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) {
                    result = Result { try JSONDecoder().decode(CaptureStatus.self, from: data ?? Data()) }
                } else { result = .failure(NSError(domain: "HTTP", code: (response as? HTTPURLResponse)?.statusCode ?? 0, userInfo: [NSLocalizedDescriptionKey: "HTTP \((response as? HTTPURLResponse)?.statusCode ?? 0)"])) }
                Task { @MainActor in
                    guard let self else { return }; self.readingCapture = false
                    switch result {
                    case .success(let status): self.captureStatus = status; self.captureError = nil
                    case .failure(let error): self.captureStatus = nil; self.captureError = "抓包状态不可用：\(error.localizedDescription)"
                    }
                }
            }.resume()
        } catch { captureStatus = nil; captureError = "抓包状态不可用：\(error.localizedDescription)" }
    }
    func changeCapture(_ config: CaptureConfig?) {
        guard !captureBusy, !readingCapture, captureStatus != nil else { return }
        do {
            var request = try captureRequest(config == nil ? "DELETE" : "PUT")
            if let config {
                request.httpBody = try JSONEncoder().encode(config)
                request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            }
            captureBusy = true; captureError = nil
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                var failure: String?
                if let error { failure = error.localizedDescription }
                else if let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) {} 
                else {
                    let code = (response as? HTTPURLResponse)?.statusCode ?? 0
                    let detail = data.flatMap { String(data: $0.prefix(512), encoding: .utf8) } ?? ""
                    failure = "HTTP \(code)：\(detail)"
                }
                let result = failure
                Task { @MainActor in
                    guard let self else { return }; self.captureBusy = false; self.captureStatus = nil
                    if let result { self.captureError = "抓包操作结果未确认：\(result)" }
                    else { self.refreshCapture() }
                }
            }.resume()
        } catch { captureError = "抓包操作未提交：\(error.localizedDescription)" }
    }
    func refreshNodePriorities() { requestNodePriorities(nil) }
    func setNodePriority(node: String, priority: String) {
        guard prioritiesLoaded, ["highest", "high", "medium", "low", "lowest", "default"].contains(priority), nodePriorities[node] != nil else { return }
        requestNodePriorities(NodePriorityUpdate(node: node, priority: priority))
    }
    func setNodeEnabled(node: String, enabled: Bool) {
        guard prioritiesLoaded, !sourcesSaveBusy, nodePriorities[node] != nil else { return }
        requestNodePriorities(NodePriorityUpdate(node: node, enabled: enabled))
    }
    private func requestNodePriorities(_ update: NodePriorityUpdate?) {
        guard !prioritiesBusy, !sourcesSaveBusy else { return }
        do {
            var request = try nodeRequest(method: update == nil ? "GET" : "PUT")
            request.url = request.url?.deletingLastPathComponent().appendingPathComponent("node-priorities")
            if let update {
                request.httpBody = try JSONEncoder().encode(update)
                request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            }
            prioritiesBusy = true; prioritiesError = nil; prioritiesNotice = nil
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let result: Result<NodePrioritiesResponse, Error> = Result {
                    if let error { throw error }
                    guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode), let data else {
                        throw NSError(domain: "clashhub", code: 1, userInfo: [NSLocalizedDescriptionKey: "优先级接口未返回成功状态"])
                    }
                    let value = try JSONDecoder().decode(NodePrioritiesResponse.self, from: data)
                    let levels = ["highest", "high", "medium", "low", "lowest"]
                    guard value.nodes.allSatisfy({ !$0.node.isEmpty && levels.contains($0.priority) && (["default"] + levels).contains($0.override) }),
                          Set(value.nodes.map(\.node)).count == value.nodes.count,
                          value.sources.allSatisfy({ !$0.id.isEmpty && levels.contains($0.defaultPriority) }),
                          Set(value.sources.map(\.id)).count == value.sources.count else { throw CocoaError(.fileReadCorruptFile) }
                    if let update, !value.nodes.contains(where: { row in
                        row.node == update.node && (update.priority == nil || row.override == update.priority) && (update.enabled == nil || row.enabled == update.enabled)
                    }) {
                        throw NSError(domain: "clashhub", code: 2, userInfo: [NSLocalizedDescriptionKey: "服务端未确认节点策略"])
                    }
                    return value
                }
                Task { @MainActor in
                    guard let self else { return }; self.prioritiesBusy = false
                    switch result {
                    case .success(let value):
                        self.nodePriorities = Dictionary(uniqueKeysWithValues: value.nodes.map { ($0.node, $0) })
                        self.sourcePolicies = Dictionary(uniqueKeysWithValues: value.sources.map { ($0.id, $0) })
                        self.syncSourcePolicyDraft()
                        self.prioritiesLoaded = true
                        if update != nil { self.prioritiesNotice = "节点策略已由服务端确认" }
                    case .failure(let error):
                        self.prioritiesLoaded = false
                        self.prioritiesError = "优先级未确认：\(error.localizedDescription)，请刷新。"
                    }
                }
            }.resume()
        } catch { prioritiesError = error.localizedDescription }
    }
    func nodeRequest(method: String) throws -> URLRequest {
        let address = cfgHTTP.isEmpty ? "127.0.0.1:33333" : cfgHTTP
        guard let url = URL(string: "http://\(address)/api/nodes"), let host = url.host,
              ["localhost", "127.0.0.1", "::1", "[::1]"].contains(host), url.user == nil, url.password == nil,
              url.path == "/api/nodes", url.query == nil, url.fragment == nil,
              let port = url.port, (1...65535).contains(port) else {
            throw NSError(domain: "clashhub", code: 1, userInfo: [NSLocalizedDescriptionKey: "HTTP 监听地址不是有效的本机地址"])
        }
        guard let token = apiToken, !token.isEmpty, !token.contains("\n"), !token.contains("\r") else { throw NSError(domain: "clashhub", code: 2, userInfo: [NSLocalizedDescriptionKey: "API 令牌无效"]) }
        var request = URLRequest(url: url)
        request.httpMethod = method; request.timeoutInterval = 5; request.cachePolicy = .reloadIgnoringLocalCacheData
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        return request
    }
    func refreshNodeGroups() {
        guard !readingNodeGroups, !nodeControlBusy else { return }
        do {
            let request = try nodeRequest(method: "GET")
            readingNodeGroups = true
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let result: Result<NodeGroups, Error>
                if let error { result = .failure(error) }
                else if let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) {
                    result = Result { try JSONDecoder().decode(NodeGroups.self, from: data ?? Data()) }
                } else { result = .failure(NSError(domain: "HTTP", code: (response as? HTTPURLResponse)?.statusCode ?? 0, userInfo: [NSLocalizedDescriptionKey: "接口返回 HTTP \((response as? HTTPURLResponse)?.statusCode ?? 0)"])) }
                Task { @MainActor in
                    guard let self else { return }; self.readingNodeGroups = false
                    switch result {
                    case .success(let groups):
                        let updated = Dictionary(groups.groups.map { ($0.group, $0) }, uniquingKeysWith: { _, last in last }); if self.nodeGroups != updated { self.nodeGroups = updated }; if self.nodeControlError != nil { self.nodeControlError = nil }
                    case .failure(let error): self.nodeGroups = [:]; self.nodeControlError = "节点控制不可用：\(error.localizedDescription)"
                    }
                }
            }.resume()
        } catch { nodeGroups = [:]; nodeControlError = "节点控制不可用：\(error.localizedDescription)" }
    }
    func setNode(group: String, node: String?) {
        guard !nodeControlBusy, !readingNodeGroups, let current = nodeGroups[group], node == nil || current.nodes.contains(node!) else { return }
        do {
            var request = try nodeRequest(method: "PUT")
            // Use the normal five-second local API timeout; no health probe is awaited.
            var payload = ["group": group, "mode": node == nil ? "auto" : "manual"]
            if let node { payload["node"] = node }
            request.httpBody = try JSONSerialization.data(withJSONObject: payload)
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            nodeControlBusy = true; nodeControlError = nil; nodeControlNotice = nil
            nodeMutationSession.dataTask(with: request) { [weak self] data, response, error in
                let result: Result<NodeGroup, Error>
                if let error { result = .failure(error) }
                else if let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) {
                    result = Result { try JSONDecoder().decode(NodeGroup.self, from: data ?? Data()) }
                } else {
                    let code = (response as? HTTPURLResponse)?.statusCode ?? 0
                    let detail = data.flatMap { String(data: $0.prefix(1024), encoding: .utf8) }?.trimmingCharacters(in: .whitespacesAndNewlines)
                    result = .failure(NSError(domain: "HTTP", code: code, userInfo: [NSLocalizedDescriptionKey: "HTTP \(code)" + (detail.map { $0.isEmpty ? "" : "：\($0)" } ?? "")]))
                }
                Task { @MainActor in
                    guard let self else { return }; self.nodeControlBusy = false
                    switch result {
                    case .success(let updated) where updated.group == group:
                        self.nodeGroups[group] = updated; self.nodeControlError = nil
                        self.nodeControlNotice = node == nil ? "已请求恢复自动；当前选择可能保持不变，等待后端重新评估。" : manualRecoveryNotice(updated.recoveryStatus, reason: updated.recoveryReason)
                        self.refreshIfVisible()
                    case .success:
                        self.nodeGroups = [:]; self.nodeControlError = "操作结果与请求不匹配，实际状态未知；请刷新确认。"
                    case .failure(let error):
                        self.nodeGroups = [:]; self.nodeControlError = "操作失败或结果未确认：\(error.localizedDescription)。请求最长等待约 8 秒；偏好可能已保存，实际选择未知，请刷新确认。"
                    }
                }
            }.resume()
        } catch { nodeControlError = "无法操作：\(error.localizedDescription)" }
    }
    // Mutations are per-domain and idempotent; never replace another writer's list.
    func blockedRequest(_ method: String) throws -> URLRequest {
        var request = try nodeRequest(method: method)
        guard let url = request.url?.deletingLastPathComponent().appendingPathComponent("blocked-domains") else {
            throw NSError(domain: "clashhub", code: 1, userInfo: [NSLocalizedDescriptionKey: "HTTP 监听地址无效"])
        }
        request.url = url
        return request
    }
    func readBlocked(_ completion: @escaping (Result<BlockedDomains, Error>) -> Void) {
        do {
            let request = try blockedRequest("GET")
            localAPISession.dataTask(with: request) { data, response, error in
                if let error { completion(.failure(error)); return }
                guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) else {
                    completion(.failure(NSError(domain: "HTTP", code: (response as? HTTPURLResponse)?.statusCode ?? 0))); return
                }
                completion(Result { try JSONDecoder().decode(BlockedDomains.self, from: data ?? Data()) })
            }.resume()
        } catch { completion(.failure(error)) }
    }
    func refreshBlockedDomains() {
        guard !blockBusy else { return }
        blockBusy = true
        readBlocked { [weak self] result in
            Task { @MainActor in
                guard let self else { return }; self.blockBusy = false
                switch result {
                case .success(let list): self.blockedDomains = Set(list.domains); self.blockError = nil
                case .failure(let error): self.blockedDomains = nil; self.blockError = "屏蔽列表读取失败：\(error.localizedDescription)"
                }
            }
        }
    }
    func setBlocked(_ domain: String, blocked: Bool) {
        guard !blockBusy, isDNSDomain(domain) else { return }
        blockBusy = true; blockError = nil; blockNotice = nil
        readBlocked { [weak self] result in
            Task { @MainActor in
                guard let self else { return }
                switch result {
                case .failure(let error):
                    self.blockBusy = false; self.blockedDomains = nil
                    self.blockError = "操作前刷新失败，未提交：\(error.localizedDescription)"
                case .success(let list):
                    _ = list
                    do {
                        var request = try self.blockedRequest(blocked ? "POST" : "DELETE")
                        request.httpBody = try JSONEncoder().encode(["domain": domain])
                        request.setValue("application/json", forHTTPHeaderField: "Content-Type")
                        localAPISession.dataTask(with: request) { [weak self] data, response, error in
                            let result: Result<BlockedDomains, Error>
                            if let error { result = .failure(error) }
                            else if let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) {
                                result = Result { try JSONDecoder().decode(BlockedDomains.self, from: data ?? Data()) }
                            } else { result = .failure(NSError(domain: "HTTP", code: (response as? HTTPURLResponse)?.statusCode ?? 0)) }
                            Task { @MainActor in
                                guard let self else { return }; self.blockBusy = false
                                switch result {
                                case .success(let updated):
                                    self.blockedDomains = Set(updated.domains)
                                    if self.blockedDomains?.contains(domain) == blocked {
                                        self.blockNotice = "服务端已返回更新后的屏蔽列表。"
                                    } else { self.blockError = "返回状态与请求不一致，请刷新核对。" }
                                case .failure(let error):
                                    self.blockedDomains = nil; self.blockError = "操作结果未确认：\(error.localizedDescription)，请刷新核对。"
                                }
                            }
                        }.resume()
                    } catch { self.blockBusy = false; self.blockedDomains = nil; self.blockError = "未能提交：\(error.localizedDescription)" }
                }
            }
        }
    }
    func healthRequest(path: String, method: String) throws -> URLRequest {
        var request = try nodeRequest(method: method)
        request.url = URL(string: "http://127.0.0.1:33335\(path)")!
        return request
    }
    func refreshNodeTraffic() {
        guard !readingNodeTraffic else { return }
        do {
            let request = try healthRequest(path: "/api/node-traffic?minutes=2880", method: "GET")
            readingNodeTraffic = true
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                var decoded: NodeTrafficResponse?
                if error == nil, let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode), let data {
                    decoded = try? JSONDecoder().decode(NodeTrafficResponse.self, from: data)
                }
                let result = decoded
                Task { @MainActor in
                    guard let self else { return }
                    if self.nodeTraffic != result {
                        self.trafficByNode = Dictionary((result?.nodes ?? []).map { ($0.node, $0) }, uniquingKeysWith: { first, _ in first })
                        self.nodeTraffic = result
                    }
                    self.readingNodeTraffic = false
                }
            }.resume()
        } catch { if nodeTraffic != nil { trafficByNode = [:]; nodeTraffic = nil } }
    }
    // Live split of OpenAI-bound vs other traffic. The backend counts bytes in
    // clashhub's own listeners, so the numbers stay separate even when both routes
    // share one upstream node or port.
    func refreshRouteTraffic() {
        let now = ProcessInfo.processInfo.systemUptime
        guard !readingRouteTraffic, now - lastRouteTrafficAt >= 2 else { return }
        lastRouteTrafficAt = now
        do {
            var request = try nodeRequest(method: "GET")
            request.url = request.url?.deletingLastPathComponent().appendingPathComponent("route-traffic")
            readingRouteTraffic = true
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                var decoded: RouteTrafficResponse?
                if error == nil, let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode), let data {
                    decoded = try? JSONDecoder().decode(RouteTrafficResponse.self, from: data)
                }
                Task { @MainActor in
                    guard let self else { return }
                    self.routeTraffic = decoded
                    self.readingRouteTraffic = false
                }
            }.resume()
        } catch { readingRouteTraffic = false }
    }
    var aiRouteTitle: String { cfgAITargets.joined(separator: "、") }
    func routeRateText(_ kind: String) -> String {
        guard let traffic = routeTraffic,
              let updated = parseTime(traffic.updatedAt), abs(updated.timeIntervalSinceNow) < 15 else { return "↑ —  ↓ —" }
        let rate = kind == "ai" ? traffic.ai : traffic.normal
        return "↑ \(rateText(rate.uploadBPS))  ↓ \(rateText(rate.downloadBPS))"
    }
    func refreshNodeHealth() {
        guard !readingHealth else { return }
        do {
            let request = try healthRequest(path: "/api/node-health", method: "GET")
            readingHealth = true
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let result: Result<NodeHealthResponse, Error>
                if let error { result = .failure(error) }
                else if let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) {
                    result = Result { try JSONDecoder().decode(NodeHealthResponse.self, from: data ?? Data()) }
                } else { result = .failure(NSError(domain: "HTTP", code: (response as? HTTPURLResponse)?.statusCode ?? 0, userInfo: [NSLocalizedDescriptionKey: "接口返回 HTTP \((response as? HTTPURLResponse)?.statusCode ?? 0)"])) }
                Task { @MainActor in
                    guard let self else { return }; self.readingHealth = false
                    switch result {
                    case .success(let health):
                        let updated = Dictionary(health.nodes.map { ($0.name, $0) }, uniquingKeysWith: { _, last in last })
                        if self.nodeHealth != updated { self.nodeHealth = updated }
                        if self.healthRunning != health.running { self.healthRunning = health.running }; if self.healthError != health.error { self.healthError = health.error }; if self.fallbackProbe != health.fallbackProbe { self.fallbackProbe = health.fallbackProbe }
                        self.healthProgressDone = health.progressDone ?? 0; self.healthProgressTotal = health.progressTotal ?? 0
                        if health.running {
                            DispatchQueue.main.asyncAfter(deadline: .now() + 2) { [weak self] in
                                guard let self, self.selectedPage == 0, let window = self.window, window.isVisible, !window.isMiniaturized, !NSApp.isHidden else { return }
                                self.refreshNodeHealth()
                            }
                        }
                    case .failure(let error): self.nodeHealth = [:]; self.fallbackProbe = nil; self.healthRunning = false; self.healthError = "检测状态不可用：\(error.localizedDescription)"
                    }
                }
            }.resume()
        } catch { nodeHealth = [:]; fallbackProbe = nil; healthRunning = false; healthError = "检测状态不可用：\(error.localizedDescription)" }
    }
    // Subscription address editing. The value lives in ClashHub's own config as
    // an override; FlClash's database and files are never modified.
    func loadSubscriptionURL(_ id: String) {
        guard subscriptionURLDrafts[id] == nil else { return }
        do {
            var request = try nodeRequest(method: "GET")
            request.url = request.url?.deletingLastPathComponent().appendingPathComponent("source-subscription").appending(queryItems: [URLQueryItem(name: "id", value: id)])
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                guard let self, error == nil, let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode), let data else { return }
                struct Payload: Decodable { let url: String? }
                guard let payload = try? JSONDecoder().decode(Payload.self, from: data) else { return }
                Task { @MainActor in if self.subscriptionURLDrafts[id] == nil { self.subscriptionURLDrafts[id] = payload.url ?? "" } }
            }.resume()
        } catch {}
    }
    func saveSubscriptionURL(_ id: String) {
        guard !subscriptionURLBusy.contains(id) else { return }
        let url = (subscriptionURLDrafts[id] ?? "").trimmingCharacters(in: .whitespacesAndNewlines)
        do {
            var request = try nodeRequest(method: "PUT")
            request.url = request.url?.deletingLastPathComponent().appendingPathComponent("source-subscription")
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONEncoder().encode(["id": id, "url": url])
            subscriptionURLBusy.insert(id); subscriptionURLError = nil; subscriptionURLNotice = nil
            localAPISession.dataTask(with: request) { [weak self] _, response, error in
                Task { @MainActor in
                    guard let self else { return }
                    self.subscriptionURLBusy.remove(id)
                    if let error { self.subscriptionURLError = "订阅地址未保存：\(error.localizedDescription)"; return }
                    guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) else {
                        self.subscriptionURLError = "订阅地址保存失败：HTTP \((response as? HTTPURLResponse)?.statusCode ?? 0)"
                        return
                    }
                    self.subscriptionURLNotice = "订阅地址已保存；优先直连，失败会自动经当前代理重试"
                    self.subscriptionURLDrafts[id] = url
                }
            }.resume()
        } catch { subscriptionURLError = "订阅地址未保存：\(error.localizedDescription)" }
    }
    var healthProgressLabel: String {
        guard healthRunning, healthProgressTotal > 0 else { return "检测中…" }
        let pct = min(100, Int((Double(healthProgressDone) / Double(healthProgressTotal) * 100).rounded()))
        return "检测中 \(pct)%"
    }
    func startNodeHealth() {
        guard !healthStarting, !healthRunning else { return }
        do {
            let request = try healthRequest(path: "/api/node-health/probe", method: "POST")
            healthStarting = true
            localAPISession.dataTask(with: request) { [weak self] _, response, error in
                Task { @MainActor in
                    guard let self else { return }; self.healthStarting = false
                    if let error { self.healthError = "检测请求未确认：\(error.localizedDescription)" }
                    else if let http = response as? HTTPURLResponse, http.statusCode == 202 || http.statusCode == 409 { self.healthError = nil }
                    else { self.healthError = "检测请求失败：HTTP \((response as? HTTPURLResponse)?.statusCode ?? 0)" }
                    self.refreshNodeHealth()
                }
            }.resume()
        } catch { healthError = "无法启动检测：\(error.localizedDescription)" }
    }
    // The menu bar remains visible with the window closed. Its lightweight
    // P95 refresh is independent of the window-only statistics refresh.
    func refreshMenuP95() {
        guard !refreshingMenuP95 else { return }
        refreshingMenuP95 = true
        var request = URLRequest(url: statsURL)
        request.timeoutInterval = 5
        request.cachePolicy = .reloadIgnoringLocalCacheData
        localAPISession.dataTask(with: request) { [weak self] data, response, error in
            let result: MenuStatistics?
            if error == nil, let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode), let data {
                result = try? JSONDecoder().decode(MenuStatistics.self, from: data)
            } else { result = nil }
            let now = ProcessInfo.processInfo.systemUptime
            Task { @MainActor in
                guard let self else { return }
                let p95 = (result?.p95Samples ?? 0) > 0 ? result?.p95Upper : nil
                if self.menuP95 != p95 { self.menuP95 = p95 }
                self.menuTrafficTotals = result?.trafficTotals
                if let totals = result?.trafficTotals, let started = result?.processStartedAt, !started.isEmpty {
                    let sample = TrafficSample(started: started, upload: totals.upload, download: totals.download, time: now)
                    let rates = self.trafficSample.flatMap { sample.rate(since: $0) }
                    self.uploadRate = rates?.0; self.downloadRate = rates?.1
                    self.trafficSample = sample
                } else {
                    self.trafficSample = nil; self.uploadRate = nil; self.downloadRate = nil
                }
                self.refreshingMenuP95 = false
            }
        }.resume()
    }
    func refreshStats() {
        guard !refreshingStats, (selectedPage == 1 || selectedPage == 0), let window, window.isVisible, !window.isMiniaturized, !NSApp.isHidden else { return }
        refreshingStats = true
        statsGeneration += 1
        let generation = statsGeneration, selected = statsWindow
        var request = URLRequest(url: URL(string: "http://127.0.0.1:33335/stats?minutes=\(selected.rawValue)")!)
        request.timeoutInterval = 5
        request.cachePolicy = .reloadIgnoringLocalCacheData
        // Node usage is fetched separately from the always-on real-time rate poll.
        // Both responses belong to this generation and are published together.
        let group = DispatchGroup()
        let lock = NSLock()
        var statsResult: Result<Statistics, Error>?
        var usage: NodeTrafficResponse?
        group.enter()
        localAPISession.dataTask(with: request) { data, response, error in
            let result: Result<Statistics, Error>
            if let error { result = .failure(error) }
            else if let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) {
                result = Result {
                    let decoded = try JSONDecoder().decode(Statistics.self, from: data ?? Data())
                    guard decoded.rows != nil, decoded.portRows != nil, decoded.windowHealthHours != nil else { throw CocoaError(.fileReadCorruptFile) }
                    return decoded
                }
            } else { result = .failure(NSError(domain: "HTTP", code: (response as? HTTPURLResponse)?.statusCode ?? 0, userInfo: [NSLocalizedDescriptionKey: "统计接口 HTTP \((response as? HTTPURLResponse)?.statusCode ?? 0)"])) }
            lock.lock(); statsResult = result; lock.unlock(); group.leave()
        }.resume()
        if var usageRequest = try? healthRequest(path: "/api/node-traffic?minutes=\(selected.rawValue)", method: "GET") {
            usageRequest.timeoutInterval = 5
            group.enter()
            localAPISession.dataTask(with: usageRequest) { data, response, error in
                let value: NodeTrafficResponse?
                if error == nil, let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode), let data {
                    value = try? JSONDecoder().decode(NodeTrafficResponse.self, from: data)
                } else { value = nil }
                lock.lock(); usage = value; lock.unlock(); group.leave()
            }.resume()
        }
        group.notify(queue: .global(qos: .userInitiated)) { [weak self] in
            lock.lock(); let result = statsResult; let traffic = usage; lock.unlock()
            let targets = (try? result?.get()).map { targetTraffic($0.rows ?? []) } ?? []
            let ports = (try? result?.get())?.portRows?.sorted { trafficGreater($0.upload, $0.download, $1.upload, $1.download) } ?? []
            let ranking = sortedDomainRanking((try? result?.get())?.domainRanking ?? [])
            Task { @MainActor in
                guard let self, self.statsGeneration == generation, self.statsWindow == selected else { return }
                self.refreshingStats = false
                switch result {
                case .success(let stats):
                    self.targetRows = targets; self.sortedPorts = ports; self.rankedDomains = ranking
                    self.displayedStatsWindow = selected; self.statsWindowTraffic = traffic
                    self.stats = stats; self.statsReadAt = Date(); self.statsError = nil
                case .failure(let error): self.statsError = "统计接口不可用或缺少窗口健康数据：\(error.localizedDescription)"
                case nil: self.statsError = "统计接口未返回数据"
                }
            }
        }
    }
    func refreshSubscriptions(force: Bool = false) {
        guard selectedPage == ClashHubSettingsViewIndex, let window, window.isVisible, !window.isMiniaturized, !NSApp.isHidden,
              !readingSubscriptions, force || ProcessInfo.processInfo.systemUptime >= nextSubscriptionRefresh else { return }
        do {
            var request = try nodeRequest(method: "GET")
            request.url = request.url?.deletingLastPathComponent().appendingPathComponent("subscriptions")
            readingSubscriptions = true
            let importPollGeneration = jiuyiPollGeneration
            nextSubscriptionRefresh = ProcessInfo.processInfo.systemUptime + 60
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let result: Result<SubscriptionResponse, Error>
                if let error { result = .failure(error) }
                else if let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) {
                    result = Result { try JSONDecoder().decode(SubscriptionResponse.self, from: data ?? Data()) }
                } else {
                    let code = (response as? HTTPURLResponse)?.statusCode ?? 0
                    result = .failure(NSError(domain: "HTTP", code: code, userInfo: [NSLocalizedDescriptionKey: "HTTP \(code)"]))
                }
                Task { @MainActor in
                    guard let self else { return }
                    self.readingSubscriptions = false
                    switch result {
                    case .success(let value):
                        self.subscriptions = value.profiles.map(SubscriptionAllowance.init)
                        self.subscriptionsLoaded = true
                        self.subscriptionsRefreshing = value.refreshing
                        self.subscriptionsError = nil
                        self.jiuyiEnabled = value.office91Enabled == true
                        self.jiuyiOfflineStatus = value.refreshStatuses?["office91"]
                        self.jiuyiOfflineMode = value.office91Mode
                        self.observeJiuyiImport(value, generation: importPollGeneration)
                        if value.refreshing || self.jiuyiImportBusy { self.scheduleSubscriptionPoll() }
                    case .failure(let error):
                        self.jiuyiEnabled = false
                        self.subscriptionsRefreshing = false
                        self.subscriptionsError = "订阅数据读取失败：\(error.localizedDescription)"
                        self.retryJiuyiStatusRead()
                    }
                }
            }.resume()
        } catch {
            nextSubscriptionRefresh = ProcessInfo.processInfo.systemUptime + 60
            subscriptionsRefreshing = false
            subscriptionsError = "订阅数据读取失败：\(error.localizedDescription)"
            retryJiuyiStatusRead()
        }
    }
    func chooseJiuyiOfflineProfile() {
        let panel = NSOpenPanel()
        panel.title = "选择玖壹本机 profile"
        panel.prompt = "导入"
        panel.canChooseDirectories = false; panel.allowsMultipleSelection = false
        panel.allowedContentTypes = [UTType(filenameExtension: "yaml"), UTType(filenameExtension: "yml")].compactMap { $0 }
        panel.directoryURL = home.appendingPathComponent("Library/Application Support/com.abysswalker.office91/profiles")
        if panel.runModal() == .OK, let file = panel.url { importJiuyiOffline(profilePath: file.path) }
    }
    func importJiuyiOffline(profilePath: String? = nil) {
        guard !jiuyiImportBusy else { return }
        jiuyiImportError = nil; jiuyiImportNotice = nil
        do {
            var request = try nodeRequest(method: "POST")
            request.url = request.url?.deletingLastPathComponent().appendingPathComponent("subscriptions")
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONSerialization.data(withJSONObject: jiuyiOfflinePayload(profilePath))
            jiuyiImportBusy = true
            jiuyiImportAcknowledged = false
            jiuyiAttemptBaseline = jiuyiOfflineStatus?.lastAttempt
            jiuyiPollDeadline = ProcessInfo.processInfo.systemUptime + 90
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let code = (response as? HTTPURLResponse)?.statusCode ?? 0
                let accepted = data.flatMap { try? JSONDecoder().decode(JiuyiOfflineAccepted.self, from: $0) }
                let errorClass = data.flatMap { String(data: $0.prefix(256), encoding: .utf8) }?.trimmingCharacters(in: .whitespacesAndNewlines)
                Task { @MainActor in
                    guard let self else { return }
                    if error == nil && code == 202 && accepted?.sourceID == "office91" && ["queued", "already_running"].contains(accepted?.status ?? "") {
                        self.jiuyiPollGeneration += 1
                        self.jiuyiImportAcknowledged = true
                        if accepted?.status == "already_running" { self.jiuyiAttemptBaseline = nil }
                        self.jiuyiImportNotice = accepted?.status == "already_running" ? "已有导入任务，正在读取结果…" : "已提交，正在等待校验与重载结果…"
                        self.refreshSubscriptions(force: true)
                        self.scheduleSubscriptionPoll()
                    } else {
                        self.jiuyiImportBusy = false
                        self.jiuyiImportError = error != nil ? "无法连接本机服务，请确认服务已启用。" : jiuyiOfflineErrorText(errorClass, httpCode: code)
                    }
                }
            }.resume()
        } catch {
            jiuyiImportBusy = false
            jiuyiImportError = "导入请求未提交，请确认本机服务可用。"
        }
    }
    private func retryJiuyiStatusRead() {
        guard jiuyiImportBusy else { return }
        if ProcessInfo.processInfo.systemUptime >= jiuyiPollDeadline {
            jiuyiImportBusy = false; jiuyiImportNotice = nil
            jiuyiImportError = "暂未读到导入结果，请恢复本机服务后刷新状态。"
        } else { scheduleSubscriptionPoll() }
    }
    private func observeJiuyiImport(_ response: SubscriptionResponse, generation: Int) {
        guard jiuyiImportBusy, jiuyiImportAcknowledged, generation == jiuyiPollGeneration else { return }
        let current = response.refreshStatuses?["office91"]
        // A 202 only acknowledges a task. A new attempt and terminal state
        // from the backend are required before reporting completion.
        if let current, current.lastAttempt != nil, current.lastAttempt != jiuyiAttemptBaseline,
           response.office91OfflineRunning != true, current.status != "running" {
            jiuyiImportBusy = false
            jiuyiImportNotice = nil
            if current.status == "error" { jiuyiImportError = jiuyiOfflineErrorText(current.error) }
            else if current.status == "ok" && response.office91Mode == "offline" { jiuyiImportNotice = "本机节点已导入并完成引擎重载。"; refreshSources(force: true) }
            else if current.status == "cached" { jiuyiImportNotice = "缓存已更新，尚未确认引擎重载。" }
            else { jiuyiImportNotice = "导入结果尚未确认，请刷新状态。" }
        } else if ProcessInfo.processInfo.systemUptime >= jiuyiPollDeadline {
            jiuyiImportBusy = false; jiuyiImportNotice = nil
            jiuyiImportError = "暂未确认导入结果，请刷新状态；服务端任务可能仍在运行。"
        }
    }
    private func scheduleSubscriptionPoll() {
        guard !subscriptionPollScheduled else { return }
        subscriptionPollScheduled = true
        DispatchQueue.main.asyncAfter(deadline: .now() + 2) { [weak self] in
            guard let self else { return }
            self.subscriptionPollScheduled = false
            self.refreshSubscriptions(force: true)
        }
    }
    // Backend contract: GET/PUT /api/sources is served on the statistics
    // listener (33335), not the proxy listener. PUT body is
    // {"sources":[{"id","enabled"}...]}; the array order encodes priority. The
    // PUT response shape is not specified, so a 2xx is treated as "accepted"
    // only and the effective state is re-read.
    func sourcesRequest(_ method: String) throws -> URLRequest {
        var request = try healthRequest(path: "/api/sources", method: method)
        request.timeoutInterval = 5
        return request
    }
    func addSubscription() {
        guard !addingSubscription, !sourcesDirty else { return }
        let address = newSubscriptionURL.trimmingCharacters(in: .whitespacesAndNewlines)
        let name = newSubscriptionName.trimmingCharacters(in: .whitespacesAndNewlines)
        guard (editingSubscriptionID != nil && address.isEmpty) || (URLComponents(string: address).map { ["http", "https"].contains($0.scheme?.lowercased() ?? "") && $0.host?.isEmpty == false } ?? false) else {
            addSubscriptionMessage = "请输入有效的 HTTP 或 HTTPS 订阅链接。"
            return
        }
        do {
            var request = try healthRequest(path: "/api/sources/subscriptions", method: "POST")
            request.timeoutInterval = 30
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            var payload = ["action": "save", "label": name, "url": address]
            if let id = editingSubscriptionID { payload["id"] = id }
            request.httpBody = try JSONSerialization.data(withJSONObject: payload)
            addingSubscription = true
            addSubscriptionMessage = nil
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let code = (response as? HTTPURLResponse)?.statusCode ?? 0
                let acknowledged = data.flatMap { try? JSONDecoder().decode(SubscriptionActionResult.self, from: $0) }
                Task { @MainActor in
                    guard let self else { return }
                    self.addingSubscription = false
                    if error == nil && (200..<300).contains(code), let acknowledged, !acknowledged.id.isEmpty, !acknowledged.status.isEmpty {
                        self.newSubscriptionURL = ""
                        self.newSubscriptionName = ""
                        self.editingSubscriptionID = nil
                        self.addSubscriptionMessage = "已添加订阅，正在更新节点；请在来源列表查看状态。"
                        self.refreshSources(force: true)
                        self.refreshSubscriptions(force: true)
                    } else {
                        // Do not display an error containing the secret URL.
                        self.addSubscriptionMessage = code == 409 ? "该订阅已存在，请检查来源列表。" : "添加失败，请检查链接和服务状态后重试。"
                    }
                }
            }.resume()
        } catch { addSubscriptionMessage = "无法连接本地服务，请确认代理已启动。" }
    }
    func editSubscription(_ row: SourceRow) {
        editingSubscriptionID = row.id
        newSubscriptionName = row.label
        newSubscriptionURL = ""
        addSubscriptionMessage = nil
    }
    func cancelSubscriptionEdit() {
        editingSubscriptionID = nil; newSubscriptionName = ""; newSubscriptionURL = ""; addSubscriptionMessage = nil
    }
    func refreshSubscription(_ id: String) {
        guard !addingSubscription, !sourcesDirty else { return }
        do {
            var request = try healthRequest(path: "/api/sources/subscriptions", method: "POST")
            request.timeoutInterval = 30
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONSerialization.data(withJSONObject: ["action": "refresh", "id": id])
            addingSubscription = true; addSubscriptionMessage = nil
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let code = (response as? HTTPURLResponse)?.statusCode ?? 0
                let acknowledged = data.flatMap { try? JSONDecoder().decode(SubscriptionActionResult.self, from: $0) }
                Task { @MainActor in
                    guard let self else { return }
                    self.addingSubscription = false
                    self.addSubscriptionMessage = error == nil && (200..<300).contains(code) && acknowledged?.id == id && !(acknowledged?.status.isEmpty ?? true) ? "已请求更新，请在来源列表查看状态。" : "更新请求未确认，请刷新来源列表后重试。"
                    self.refreshSources(force: true)
                }
            }.resume()
        } catch { addSubscriptionMessage = "无法连接本地服务，请确认代理已启动。" }
    }
    // Optional backend capability. When the endpoint does not exist yet the
    // failure must surface as backend-unavailable; never report success.
    func xlinkerImportRequest() throws -> URLRequest {
        var request = try healthRequest(path: "/api/sources/xlinker/import", method: "POST")
        request.timeoutInterval = 5
        return request
    }
    // Rate-limited like subscriptions and only while the settings page is
    // visible, so the 3-second tick never adds sources traffic elsewhere.
    func refreshSources(force: Bool = false) {
        guard selectedPage == ClashHubSettingsViewIndex, let window, window.isVisible, !window.isMiniaturized, !NSApp.isHidden,
              !readingSources, !sourcesSaveBusy,
              force || ProcessInfo.processInfo.systemUptime >= nextSourcesRefresh else { return }
        do {
            let request = try sourcesRequest("GET")
            readingSources = true
            nextSourcesRefresh = ProcessInfo.processInfo.systemUptime + 60
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let result: Result<SourcesResponse, Error>
                if let error { result = .failure(error) }
                else if let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) {
                    result = Result { try JSONDecoder().decode(SourcesResponse.self, from: data ?? Data()) }
                } else {
                    let code = (response as? HTTPURLResponse)?.statusCode ?? 0
                    result = .failure(NSError(domain: "HTTP", code: code, userInfo: [NSLocalizedDescriptionKey: "HTTP \(code)"]))
                }
                Task { @MainActor in
                    guard let self else { return }
                    self.readingSources = false
                    switch result {
                    case .success(let value):
                        self.sources = value
                        self.sourcesLoaded = true
                        self.sourcesError = nil
                        if !self.sourcesDirty { self.sourceDraft = value.sources; self.syncFilterDraft() }
                    case .failure(let error):
                        self.sourcesError = "来源数据读取失败：\(error.localizedDescription)"
                    }
                }
            }.resume()
        } catch {
            nextSourcesRefresh = ProcessInfo.processInfo.systemUptime + 60
            sourcesError = "来源数据读取失败：\(error.localizedDescription)"
        }
    }
    func moveSource(at index: Int, offset: Int) {
        guard !sourcesSaveBusy, let moved = movedSources(sourceDraft, from: index, offset: offset) else { return }
        sourceDraft = moved
        sourcesDirty = true
        sourcesNotice = nil
    }
    func setSourceEnabled(at index: Int, enabled: Bool) {
        guard !sourcesSaveBusy, sourceDraft.indices.contains(index), sourceDraft[index].enabled != enabled else { return }
        sourceDraft[index].enabled = enabled
        sourcesDirty = true
        sourcesNotice = nil
    }
    private func syncSourcePolicyDraft() {
        for (id, policy) in sourcePolicies {
            if !editedSourcePriorities.contains(id) { sourcePolicyPriorities[id] = policy.defaultPriority }
            if !editedSourcePatterns.contains(id) { sourcePolicyPatterns[id] = policy.downgradePatterns.joined(separator: "\n") }
        }
    }
    func setSourcePriority(id: String, value: String) {
        guard !sourcesSaveBusy, sourcePolicies[id] != nil, ["highest", "high", "medium", "low", "lowest"].contains(value) else { return }
        sourcePolicyPriorities[id] = value; editedSourcePriorities.insert(id); sourcesDirty = true; sourcesNotice = nil
    }
    func setSourcePatterns(id: String, value: String) {
        guard !sourcesSaveBusy, sourcePolicies[id] != nil else { return }
        sourcePolicyPatterns[id] = value; editedSourcePatterns.insert(id); sourcesDirty = true; sourcesNotice = nil
    }
    func syncFilterDraft() {
        includeDraft = Dictionary(uniqueKeysWithValues: sourceDraft.map { ($0.id, ($0.includeKeywords ?? []).joined(separator: ", ")) })
        excludeDraft = Dictionary(uniqueKeysWithValues: sourceDraft.map { ($0.id, ($0.excludeKeywords ?? []).joined(separator: ", ")) })
    }
    func setFilter(id: String, value: String, include: Bool) {
        if include { includeDraft[id] = value } else { excludeDraft[id] = value }; sourcesDirty = true
    }
    func resetSourceDraft() {
        guard !sourcesSaveBusy, let value = sources?.sources else { return }
        sourceDraft = value; syncFilterDraft()
        editedSourcePriorities.removeAll(); editedSourcePatterns.removeAll(); syncSourcePolicyDraft()
        sourcesDirty = false
        sourcesSaveError = nil
        sourcesNotice = nil
    }
    func saveSources() {
        guard !sourcesSaveBusy, !readingSources, !prioritiesBusy, sourcesDirty else { return }
        let payload = SourceSavePayload(sources: sourceDraft.map { row in
            let priority = editedSourcePriorities.contains(row.id) ? sourcePolicyPriorities[row.id] : nil
            let patterns: [String]? = editedSourcePatterns.contains(row.id)
                ? (sourcePolicyPatterns[row.id] ?? "").components(separatedBy: .newlines).map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }.filter { !$0.isEmpty }
                : nil
            let parse: (String) -> [String] = { $0.components(separatedBy: CharacterSet(charactersIn: ",，、\n")).map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }.filter { !$0.isEmpty } }
            return SourceOrderEntry(id: row.id, enabled: row.enabled, defaultPriority: priority, downgradePatterns: patterns, includeKeywords: row.includeKeywords == nil ? nil : parse(includeDraft[row.id] ?? ""), excludeKeywords: row.excludeKeywords == nil ? nil : parse(excludeDraft[row.id] ?? ""))
        })
        do {
            var request = try sourcesRequest("PUT")
            // The server regenerates and reloads the engine before replying.
            request.timeoutInterval = 120
            request.httpBody = try JSONEncoder().encode(payload)
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            sourcesSaveBusy = true; sourcesSaveError = nil; sourcesNotice = nil
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                var failure: String?
                if let error { failure = error.localizedDescription }
                else if let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) {}
                else {
                    let code = (response as? HTTPURLResponse)?.statusCode ?? 0
                    let detail = data.flatMap { String(data: $0.prefix(512), encoding: .utf8) } ?? ""
                    failure = "HTTP \(code)\(detail.isEmpty ? "" : "：\(detail)")"
                }
                let result = failure
                Task { @MainActor in
                    guard let self else { return }
                    if let result {
                        self.sourcesSaveBusy = false
                        self.sourcesSaveError = "保存结果未确认：\(result)。草稿已保留，请刷新核对。"
                    } else {
                        self.sourcesNotice = "保存请求已接受，正在回读核对。"
                        self.confirmSourcesSave(payload)
                    }
                }
            }.resume()
        } catch { sourcesSaveError = "保存未提交：\(error.localizedDescription)" }
    }
    private func confirmSourcesSave(_ payload: SourceSavePayload) {
        do {
            let policyRequest = try nodeRequest(method: "GET")
            guard let base = policyRequest.url else { throw CocoaError(.fileReadUnknown) }
            var request = policyRequest
            request.url = base.deletingLastPathComponent().appendingPathComponent("node-priorities")
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let result: Result<NodePrioritiesResponse, Error> = Result {
                    if let error { throw error }
                    guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode), let data else { throw CocoaError(.fileReadUnknown) }
                    let value = try JSONDecoder().decode(NodePrioritiesResponse.self, from: data)
                    guard Set(value.sources.map(\.id)).count == value.sources.count,
                          value.sources.allSatisfy({ !$0.id.isEmpty && ["highest", "high", "medium", "low", "lowest"].contains($0.defaultPriority) }) else { throw CocoaError(.fileReadCorruptFile) }
                    for entry in payload.sources {
                        guard let policy = value.sources.first(where: { $0.id == entry.id }),
                              entry.defaultPriority == nil || policy.defaultPriority == entry.defaultPriority,
                              entry.downgradePatterns == nil || policy.downgradePatterns == entry.downgradePatterns else {
                            throw NSError(domain: "clashhub", code: 1, userInfo: [NSLocalizedDescriptionKey: "来源策略与提交值不一致或来源缺失"])
                        }
                    }
                    return value
                }
                Task { @MainActor in
                    guard let self else { return }
                    switch result {
                    case .success(let value):
                        self.sourcePolicies = Dictionary(uniqueKeysWithValues: value.sources.map { ($0.id, $0) })
                        self.editedSourcePriorities.removeAll(); self.editedSourcePatterns.removeAll()
                        self.syncSourcePolicyDraft()
                        // Keep order/enabled draft until the sources endpoint also confirms it.
                        self.confirmSourceOrder(payload)
                    case .failure(let error):
                        self.sourcesSaveBusy = false; self.sourcesNotice = nil
                        self.sourcesSaveError = "来源策略回读未确认：\(error.localizedDescription)。草稿已保留。"
                    }
                }
            }.resume()
        } catch {
            sourcesSaveBusy = false; sourcesNotice = nil
            sourcesSaveError = "来源策略回读未提交：\(error.localizedDescription)。草稿已保留。"
        }
    }
    private func confirmSourceOrder(_ payload: SourceSavePayload) {
        do {
            let request = try sourcesRequest("GET")
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let result: Result<SourcesResponse, Error> = Result {
                    if let error { throw error }
                    guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode), let data else { throw CocoaError(.fileReadUnknown) }
                    let value = try JSONDecoder().decode(SourcesResponse.self, from: data)
                    guard value.sources.count == payload.sources.count,
                          zip(value.sources, payload.sources).allSatisfy({ $0.id == $1.id && $0.enabled == $1.enabled && ($1.includeKeywords == nil || $0.includeKeywords == $1.includeKeywords) && ($1.excludeKeywords == nil || $0.excludeKeywords == $1.excludeKeywords) }) else {
                        throw NSError(domain: "clashhub", code: 2, userInfo: [NSLocalizedDescriptionKey: "来源顺序或启用状态与提交值不一致"])
                    }
                    return value
                }
                Task { @MainActor in
                    guard let self else { return }; self.sourcesSaveBusy = false
                    switch result {
                    case .success(let value):
                        self.sources = value; self.sourcesLoaded = true; self.sourcesError = nil
                        self.sourceDraft = value.sources; self.syncFilterDraft(); self.sourcesDirty = false
                        self.sourcesNotice = "来源顺序、启用和策略已回读确认。"
                        self.refreshNodePriorities()
                    case .failure(let error):
                        self.sourcesNotice = nil
                        self.sourcesSaveError = "来源列表回读未确认：\(error.localizedDescription)。草稿已保留。"
                    }
                }
            }.resume()
        } catch {
            sourcesSaveBusy = false; sourcesNotice = nil
            sourcesSaveError = "来源列表回读未提交：\(error.localizedDescription)。草稿已保留。"
        }
    }
    func importXlinkerNodes() {
        guard !xlinkerImportBusy else { return }
        do {
            let request = try xlinkerImportRequest()
            xlinkerImportBusy = true; xlinkerError = nil; xlinkerNotice = nil
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                var failure: String?
                if let error { failure = "导入请求未确认：\(error.localizedDescription)" }
                else if let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) {}
                else {
                    let code = (response as? HTTPURLResponse)?.statusCode ?? 0
                    let detail = data.flatMap { String(data: $0.prefix(512), encoding: .utf8) } ?? ""
                    failure = "后端不可用：HTTP \(code)\(detail.isEmpty ? "" : "：\(detail)")"
                }
                let result = failure
                Task { @MainActor in
                    guard let self else { return }
                    self.xlinkerImportBusy = false
                    if let result { self.xlinkerError = result }
                    else {
                        self.xlinkerNotice = "导入请求已提交；是否新增节点以来源列表的返回为准。"
                        self.refreshSources(force: true)
                    }
                }
            }.resume()
        } catch { xlinkerError = "导入请求未提交：\(error.localizedDescription)" }
    }
    // Credentials are read off the main thread and replaced on every poll, including
    // failures, so deleted or invalid tokens never remain usable indefinitely.
    private func refreshCredentials() {
        guard !readingCredentials else { return }
        readingCredentials = true
        let tokenURL = support.appendingPathComponent("api-token"), engineURL = self.engineURL
        let cache = fileCache
        snapshotQueue.async { [weak self] in
            let token = (try? String(contentsOf: tokenURL, encoding: .utf8))?.trimmingCharacters(in: .whitespacesAndNewlines)
            let validToken = token.flatMap { !$0.isEmpty && !$0.contains("\n") && !$0.contains("\r") ? $0 : nil }
            let text = try? String(contentsOf: engineURL, encoding: .utf8)
            if text != cache.lastEngineText { cache.lastEngineText = text; cache.engineValues = parseEngineValues(text ?? "") }
            let values = cache.engineValues
            Task { @MainActor in
                guard let self else { return }
                let changed = self.apiToken != validToken || self.engineValues != values
                self.apiToken = validToken; self.engineValues = values; self.readingCredentials = false
                if changed { self.refresh(); self.refreshRoutingMode(); self.refreshNodeTraffic(); self.refreshNodeHealth() }
            }
        }
    }
    func engineValue(_ key: String) -> String { engineValues[key] ?? "" }
    func controller() -> String {
        let value = engineValue("external-controller")
        return !value.isEmpty ? value : (!cfgController.isEmpty ? cfgController : "127.0.0.1:19090")
    }
    // Statistics may refer to nodes outside the current Japan aggregate.
    var healthNodeNames: [String] { usedHealthNodes(nodeTraffic, knownNodes: knownNodes) }
    var proxyStatus: String {
        guard let state, let updated = parseTime(state.updatedAt), abs(updated.timeIntervalSinceNow) < 15 else { return "状态未知" }
        if state.active == false { return "已停用" }
        guard state.active == true else { return "状态未知" }
        if state.ready != true { return "正在启动" }
        return state.mode == "fallback" ? "运行中 · 备用线路" : "运行中"
    }
    // A missing/stale observation is unknown, not zero. These are lower-bound
    // sampled rates and cannot be substituted with process-wide traffic.
    func nodeRateText(_ node: String?) -> String {
        guard let node, let traffic = nodeTraffic, traffic.coverage == "partial", traffic.samplingStatus == "ok",
              let date = parseTime(traffic.updatedAt), abs(date.timeIntervalSinceNow) < 15,
              let entry = trafficByNode[node], entry.rateAvailable,
              let upload = entry.uploadBPS, let download = entry.downloadBPS,
              upload.isFinite, download.isFinite, upload >= 0, download >= 0 else { return "↑ —  ↓ —" }
        return "↑ \(rateText(upload))  ↓ \(rateText(download))"
    }
    func nodeRateShort(_ node: String?) -> String {
        guard let node, let traffic = nodeTraffic, traffic.coverage == "partial", traffic.samplingStatus == "ok",
              let date = parseTime(traffic.updatedAt), abs(date.timeIntervalSinceNow) < 15,
              let entry = trafficByNode[node], entry.rateAvailable,
              let upload = entry.uploadBPS, let download = entry.downloadBPS,
              upload.isFinite, download.isFinite, upload >= 0, download >= 0 else { return "—" }
        return "\(compactRate(upload))/s · \(compactRate(download))/s"
    }
    // One combined figure for all traffic clashhub carries (ordinary and AI).
    func globalRateCompact() -> String {
        guard let up = uploadRate, let down = downloadRate,
              up.isFinite, down.isFinite, up >= 0, down >= 0 else { return "↑— ↓—" }
        return "↑\(compactRate(up)) ↓\(compactRate(down))"
    }
    var currentRouteQuality: String {
        guard let node = selectedNode("主代理"), healthError == nil else { return "未知" }
        let probe = node == "FALLBACK-LOCAL" ? fallbackProbe : nodeHealth[node]?.normal
        return routeQuality(probe, running: healthRunning || healthStarting)
    }
    var fallbackRouteLabel: String {
        guard state?.active == true, state?.ready == true, state?.mode == "fallback",
              let updated = parseTime(state?.updatedAt), abs(updated.timeIntervalSinceNow) < 15,
              let address = state?.upstream, !address.isEmpty else {
            return "外部备用代理 · 生效地址未知"
        }
        return address + " · 外部代理，具体节点未知"
    }
    func displayName(_ name: String) -> String {
        if name == "FALLBACK-LOCAL" { return fallbackRouteLabel }
        guard let label = labels[name], !label.profile.isEmpty, !label.name.isEmpty else { return name }
        return "\(label.profile)：\(label.name)"
    }
    // Follow only the controller's current selection, never infer from candidates.
    func selectedNode(_ group: String, ai: Bool = false) -> String? {
        guard state?.active == true, state?.ready == true, let updated = parseTime(state?.updatedAt), abs(updated.timeIntervalSinceNow) < 15 else { return nil }
        if state?.mode == "fallback" { return state?.upstream?.isEmpty == false ? "FALLBACK-LOCAL" : nil }
        guard state?.mode == "primary" else { return nil }
        // Probe health is independent of the controller's actual selected route.
        // Health describes probe results, not which route the controller selected.
        guard nodeError == nil else { return nil }
        let groups: Set<String> = ["URLTest", "Fallback", "Selector", "LoadBalance", "Relay", "Smart"]
        var current = group
        var seen = Set<String>()
        while seen.insert(current).inserted {
            guard let info = proxies[current], let type = info.type else { return nil }
            if groups.contains(type) {
                guard let next = info.now, !next.isEmpty else { return nil }
                current = next
            } else {
                guard current == "FALLBACK-LOCAL" || nodeNames.contains(current) else { return nil }
                return current
            }
        }
        return nil
    }
    func nodeStatus(_ name: String) -> (String, Color) {
        guard let latest = proxies[name]?.history?.last else { return ("未检测", .secondary) }
        guard let time = parseTime(latest.time) else { return ("检测时间未知", .secondary) }
        let age = Date().timeIntervalSince(time)
        guard age >= -300, age <= 600 else { return ("检测已过时", .orange) }
        guard let delay = latest.delay else { return ("检测结果未知", .secondary) }
        guard delay > 0 else { return ("检测失败", .red) }
        return ("\(delay) ms", delay < 350 ? .green : (delay < 900 ? .orange : .red))
    }
    func loadConfig() {
        do {
            let data = try Data(contentsOf: configURL)
            guard let d = try JSONSerialization.jsonObject(with: data) as? [String: Any] else { throw CocoaError(.fileReadCorruptFile) }
            cfgAggregateKeywords = (d["aggregate_keywords"] as? [String] ?? []).joined(separator: "\n")
            cfgPrimary = d["primary_addr"] as? String ?? ""; cfgFallback = d["fallback_addr"] as? String ?? ""
            let list = (d["fallback_addrs"] as? [String] ?? []).map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
            cfgFallbackList = list.isEmpty ? (cfgFallback.isEmpty ? [] : [cfgFallback]) : list
            cfgController = d["controller"] as? String ?? ""; cfgHTTP = d["http_listen"] as? String ?? ""; cfgSOCKS = d["socks_listen"] as? String ?? ""
            cfgProbe = d["probe_url"] as? String ?? ""; cfgAIProbe = d["ai_probe_url"] as? String ?? ""
            let targets = (d["ai_targets"] as? [String] ?? []).map { $0.trimmingCharacters(in: .whitespaces).lowercased() }.filter { ["openai","claude","gemini","grok"].contains($0) }
            cfgAITargets = targets.isEmpty ? ["openai"] : targets
            cfgOffice91Enabled = d["office91_enabled"] as? Bool ?? false
            jiuyiEnabled = cfgOffice91Enabled
            cfgRequireJP = d["require_japan_egress"] as? Bool ?? false; cfgNotify = d["notify"] as? Bool ?? false
            configLoaded = true; configError = nil
        } catch { configLoaded = false; configError = "无法读取配置：\(error.localizedDescription)" }
    }
    func validFallbackAddress(_ value: String) -> Bool {
        let value = value.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let url = URL(string: "http://" + value), let host = url.host, !host.isEmpty,
              let port = url.port, (1...65535).contains(port), url.user == nil, url.password == nil,
              url.query == nil, url.fragment == nil, url.path.isEmpty else {
            return false
        }
        return true
    }
    var fallbackAddressWarning: String? {
        if cfgFallbackList.isEmpty { return "至少需要一个备用代理地址。" }
        if cfgFallbackList.contains(where: { !validFallbackAddress($0) }) {
            return "备用地址应为 host:port；请检查格式，仍可保存。"
        }
        let unique = Set(cfgFallbackList.map { $0.trimmingCharacters(in: .whitespaces) })
        if unique.count != cfgFallbackList.count { return "备用地址有重复项，保存时会自动去重。" }
        return nil
    }
    func saveConfig() {
        guard configLoaded else { configError = "配置未成功读取，不能保存"; return }
        do {
            let data = try Data(contentsOf: configURL)
            guard var d = try JSONSerialization.jsonObject(with: data) as? [String: Any] else { throw CocoaError(.fileReadCorruptFile) }
            let keywords = cfgAggregateKeywords.components(separatedBy: .newlines).map { $0.trimmingCharacters(in: .whitespacesAndNewlines) }.filter { !$0.isEmpty }
            if keywords.isEmpty { d.removeValue(forKey: "aggregate_keywords") }
            else { d["aggregate_keywords"] = keywords }
            d["primary_addr"] = cfgPrimary.trimmingCharacters(in: .whitespacesAndNewlines)
            var fallbackList: [String] = []
            for addr in cfgFallbackList {
                let trimmed = addr.trimmingCharacters(in: .whitespacesAndNewlines)
                if !trimmed.isEmpty && !fallbackList.contains(trimmed) { fallbackList.append(trimmed) }
            }
            if fallbackList.isEmpty { fallbackList = [cfgFallback.trimmingCharacters(in: .whitespacesAndNewlines)] }
            if !fallbackList.isEmpty {
                d["fallback_addr"] = fallbackList[0]
                d["fallback_addrs"] = fallbackList
                cfgFallback = fallbackList[0]
                cfgFallbackList = fallbackList
            }
            d["controller"] = cfgController.trimmingCharacters(in: .whitespacesAndNewlines)
            d["http_listen"] = cfgHTTP.trimmingCharacters(in: .whitespacesAndNewlines)
            d["socks_listen"] = cfgSOCKS.trimmingCharacters(in: .whitespacesAndNewlines)
            d["probe_url"] = cfgProbe.trimmingCharacters(in: .whitespacesAndNewlines)
            d["ai_probe_url"] = cfgAIProbe.trimmingCharacters(in: .whitespacesAndNewlines)
            var targets = cfgAITargets.filter { ["openai","claude","gemini","grok"].contains($0) }
            if targets.isEmpty { targets = ["openai"] }
            d["ai_targets"] = targets
            d["office91_enabled"] = cfgOffice91Enabled
            d["require_japan_egress"] = cfgRequireJP; d["notify"] = true
            try JSONSerialization.data(withJSONObject: d, options: [.prettyPrinted, .sortedKeys]).write(to: configURL, options: .atomic)
            configError = nil
            message = "配置已保存；修改代理地址后需重启服务。" + (fallbackAddressWarning.map { " " + $0 } ?? "")
        } catch { configError = "保存失败：\(error.localizedDescription)" }
    }

    // The 玖壹 integration is opt-in and local-only. The daemon applies the flag
    // and regenerates the engine in one transaction, so disabling it removes the
    // 玖壹 nodes immediately instead of leaving them in the running pool.
    func setOffice91Module(_ enabled: Bool) {
        guard configLoaded else {
            cfgOffice91Enabled = !enabled
            configError = "配置未成功读取，不能保存"
            return
        }
        guard !office91ModuleBusy else { return }
        do {
            var request = try healthRequest(path: "/api/office91-module", method: "POST")
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONSerialization.data(withJSONObject: ["enabled": enabled])
            request.timeoutInterval = 180
            office91ModuleBusy = true
            configError = nil
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let ok = error == nil && (response as? HTTPURLResponse).map { (200..<300).contains($0.statusCode) } == true
                Task { @MainActor in
                    guard let self else { return }
                    self.office91ModuleBusy = false
                    guard ok else {
                        self.cfgOffice91Enabled = !enabled
                        self.configError = "切换失败：\(error?.localizedDescription ?? "服务未确认")"
                        return
                    }
                    self.cfgOffice91Enabled = enabled
                    self.jiuyiEnabled = enabled
                    self.message = enabled ? "玖壹模块已启用，节点已接入；可在下方设置筛选与优先级。" : "玖壹模块已停用，相关节点已移出。"
                    self.refreshSources(force: true)
                    self.refreshSubscriptions(force: true)
                }
            }.resume()
        } catch {
            cfgOffice91Enabled = !enabled
            configError = "切换失败：\(error.localizedDescription)"
        }
    }

    // Fetch every enabled subscription now: each refresher tries a direct
    // connection first and only then falls back to the configured local proxies.
    // The daemon reports a real completion percentage while the batch runs.
    func updateAllSources() {
        guard !sourcesUpdateBusy else { return }
        do {
            var request = try healthRequest(path: "/api/sources/update", method: "POST")
            request.timeoutInterval = 30
            sourcesUpdateBusy = true
            sourcesUpdatePercent = 0
            sourcesError = nil
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let code = (response as? HTTPURLResponse)?.statusCode ?? 0
                Task { @MainActor in
                    guard let self else { return }
                    guard error == nil, code == 202 else {
                        self.sourcesUpdateBusy = false
                        self.sourcesUpdatePercent = nil
                        self.sourcesError = "更新配置失败：\(error?.localizedDescription ?? "HTTP \(code)")"
                        return
                    }
                    self.message = "正在更新订阅配置；直连失败会自动改用当前节点。"
                    self.pollSourceUpdate()
                }
            }.resume()
        } catch {
            sourcesUpdateBusy = false
            sourcesUpdatePercent = nil
            sourcesError = "更新配置失败：\(error.localizedDescription)"
        }
    }
    private func pollSourceUpdate() {
        guard sourcesUpdateBusy else { return }
        do {
            var request = try healthRequest(path: "/api/sources/update", method: "GET")
            request.timeoutInterval = 10
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let code = (response as? HTTPURLResponse)?.statusCode ?? 0
                let value = (code == 200 && data != nil) ? try? JSONDecoder().decode(SourceUpdateProgress.self, from: data!) : nil
                Task { @MainActor in
                    guard let self else { return }
                    guard let value else {
                        self.sourcesUpdateBusy = false
                        self.sourcesUpdatePercent = nil
                        self.sourcesError = "更新进度不可用：\(error?.localizedDescription ?? "HTTP \(code)")"
                        return
                    }
                    self.sourcesUpdatePercent = value.percent
                    if value.running {
                        DispatchQueue.main.asyncAfter(deadline: .now() + 0.7) { [weak self] in self?.pollSourceUpdate() }
                        return
                    }
                    self.sourcesUpdatePercent = 100
                    self.sourcesUpdateBusy = false
                    self.message = value.total > 0 ? "配置更新完成。" : "没有可更新的启用来源。"
                    self.refreshSources(force: true)
                    self.refreshSubscriptions(force: true)
                    self.refreshNodePriorities()
                }
            }.resume()
        } catch {
            sourcesUpdateBusy = false
            sourcesUpdatePercent = nil
            sourcesError = "更新进度不可用：\(error.localizedDescription)"
        }
    }

    // Only user-added subscriptions are removable; built-in profile and 玖壹
    // entries keep their own lifecycle.
    func deleteSource(_ row: SourceRow) {
        guard row.id.hasPrefix("custom-sub-"), deletingSourceID == nil else { return }
        do {
            let path = "/api/sources/subscriptions?id=" + (row.id.addingPercentEncoding(withAllowedCharacters: .urlQueryAllowed) ?? row.id)
            var request = try healthRequest(path: path, method: "DELETE")
            request.timeoutInterval = 60
            deletingSourceID = row.id
            sourcesError = nil
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let code = (response as? HTTPURLResponse)?.statusCode ?? 0
                Task { @MainActor in
                    guard let self else { return }
                    self.deletingSourceID = nil
                    guard error == nil, (200..<300).contains(code) else {
                        self.sourcesError = "删除失败：\(error?.localizedDescription ?? "HTTP \(code)")"
                        return
                    }
                    self.sourceDraft.removeAll { $0.id == row.id }
                    self.includeDraft.removeValue(forKey: row.id)
                    self.excludeDraft.removeValue(forKey: row.id)
                    self.sourcesDirty = false
                    self.message = "已删除「\(row.label.isEmpty ? row.id : row.label)」，节点已移出。"
                    self.refreshSources(force: true)
                    self.refreshSubscriptions(force: true)
                }
            }.resume()
        } catch {
            sourcesError = "删除失败：\(error.localizedDescription)"
        }
    }

    // ---- 节点选择策略（/api/strategy；修改立即生效，无需重启） ----
    static func validStrategy(_ v: String) -> String? {
        switch v { case "stability", "balanced", "latency": return v; default: return nil }
    }

    func loadStrategy() {
        do {
            var request = try nodeRequest(method: "GET")
            request.url = request.url?.deletingLastPathComponent().appendingPathComponent("strategy")
            strategyLoading = true; strategyError = nil
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let result: Result<StrategyConfig, Error> = Result {
                    if let error { throw error }
                    guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode), let data else {
                        throw NSError(domain: "clashhub", code: 1, userInfo: [NSLocalizedDescriptionKey: "策略接口未返回成功状态"])
                    }
                    return try JSONDecoder().decode(StrategyConfig.self, from: data)
                }
                Task { @MainActor in
                    guard let self else { return }
                    self.strategyLoading = false
                    switch result {
                    case .success(let value):
                        self.strategyPrimary = Model.validStrategy(value.primary_strategy) ?? "stability"
                        self.strategyAI = Model.validStrategy(value.ai_strategy) ?? "latency"
                        self.strategyLatencyGap = String(max(10, value.latency_gap_ms))
                        self.strategyBalanceGap = String(max(10, value.balance_gap_ms))
                        self.strategyLoaded = true
                        self.strategyError = nil
                    case .failure(let err):
                        self.strategyError = "读取策略失败：\(err.localizedDescription)"
                    }
                }
            }.resume()
        } catch { strategyLoading = false; strategyError = error.localizedDescription }
    }

    func saveStrategy() {
        guard let latency = Int(strategyLatencyGap.trimmingCharacters(in: .whitespaces)), (10...10000).contains(latency) else {
            strategyError = "低延迟切换阈值需为 10–10000 的整数（毫秒）"; return
        }
        guard let balance = Int(strategyBalanceGap.trimmingCharacters(in: .whitespaces)), (10...10000).contains(balance) else {
            strategyError = "均衡切换阈值需为 10–10000 的整数（毫秒）"; return
        }
        do {
            var request = try nodeRequest(method: "PUT")
            request.url = request.url?.deletingLastPathComponent().appendingPathComponent("strategy")
            request.httpBody = try JSONEncoder().encode(StrategyConfig(primary_strategy: strategyPrimary, ai_strategy: strategyAI, latency_gap_ms: latency, balance_gap_ms: balance))
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            strategySaving = true; strategyError = nil; strategyNotice = nil
            localAPISession.dataTask(with: request) { [weak self] data, response, error in
                let result: Result<StrategyConfig, Error> = Result {
                    if let error { throw error }
                    guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode), let data else {
                        throw NSError(domain: "clashhub", code: (response as? HTTPURLResponse)?.statusCode ?? 1, userInfo: [NSLocalizedDescriptionKey: "策略接口返回 HTTP \((response as? HTTPURLResponse)?.statusCode ?? 0)"])
                    }
                    return try JSONDecoder().decode(StrategyConfig.self, from: data)
                }
                Task { @MainActor in
                    guard let self else { return }
                    self.strategySaving = false
                    switch result {
                    case .success(let value):
                        self.strategyPrimary = Model.validStrategy(value.primary_strategy) ?? self.strategyPrimary
                        self.strategyAI = Model.validStrategy(value.ai_strategy) ?? self.strategyAI
                        self.strategyLatencyGap = String(max(10, value.latency_gap_ms))
                        self.strategyBalanceGap = String(max(10, value.balance_gap_ms))
                        self.strategyLoaded = true
                        self.strategyNotice = "已保存并立即生效（普通流量：\(self.strategyLabel(self.strategyPrimary)) · AI流量：\(self.strategyLabel(self.strategyAI))）"
                        self.strategyError = nil
                    case .failure(let err):
                        self.strategyError = "保存失败：\(err.localizedDescription)"
                    }
                }
            }.resume()
        } catch { strategySaving = false; strategyError = error.localizedDescription }
    }

    func strategyLabel(_ v: String) -> String {
        switch v { case "stability": return "稳定优先"; case "balanced": return "均衡"; case "latency": return "低延迟优先"; default: return v }
    }
}

struct StrategyConfig: Codable {
    var primary_strategy: String
    var ai_strategy: String
    var latency_gap_ms: Int
    var balance_gap_ms: Int
}

struct WindowTracker: NSViewRepresentable {
    let model: Model
    func makeNSView(context: Context) -> NSView {
        let view = NSView()
        DispatchQueue.main.async { model.window = view.window; model.refreshIfVisible() }
        return view
    }
    func updateNSView(_ view: NSView, context: Context) {
        DispatchQueue.main.async {
            if model.window !== view.window { model.window = view.window; model.refreshIfVisible() }
        }
    }
}
// Shared type scale keeps data tables and operational notes readable at normal
// macOS display scaling; smaller text is reserved for the heatmap time axis.
private enum InterfaceType {
    static let body = Font.system(size: 14)
    static let detail = Font.system(size: 12)
    static let label = Font.system(size: 12, weight: .medium)
}
struct PageHeader: View {
    let title: String; let subtitle: String; let icon: String
    var body: some View {
        HStack(spacing: 12) {
            Image(systemName: icon).font(.title3).foregroundStyle(.blue)
                .frame(width: 38, height: 38).background(Color.blue.opacity(0.1), in: RoundedRectangle(cornerRadius: 10))
            VStack(alignment: .leading, spacing: 3) {
                Text(title).font(.system(size: 23, weight: .semibold))
                Text(subtitle).font(InterfaceType.body).foregroundStyle(.secondary)
            }
            Spacer()
        }
    }
}
struct Surface<Content: View>: View {
    @ViewBuilder var content: Content
    var body: some View {
        content.padding(18).frame(maxWidth: .infinity, alignment: .leading)
            .background(Color(nsColor: .controlBackgroundColor), in: RoundedRectangle(cornerRadius: 18))
            .overlay(RoundedRectangle(cornerRadius: 18).strokeBorder(Color.primary.opacity(0.07)))
    }
}
struct RouteBadge: View {
    let text: String; let color: Color
    var body: some View {
        Text(text).font(.system(size: 12, weight: .semibold)).foregroundStyle(color)
            .padding(.horizontal, 9).padding(.vertical, 4)
            .background(color.opacity(0.12), in: Capsule())
    }
}
func sourceTypeColor(_ type: String) -> Color {
    switch type { case "profile": return .blue; case "subscription": return .purple; case "vpn": return .orange; default: return .secondary }
}
struct NodesView: View {
    @EnvironmentObject var model: Model
    var body: some View {
        let normal = model.selectedNode("主代理")
        let ai = model.selectedNode("AI主代理", ai: true)
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                HStack(alignment: .top) {
                    PageHeader(title: "线路", subtitle: "当前连接与代理控制", icon: "network")
                }
                HStack(alignment: .top, spacing: 14) {
                    Surface {
                        HStack(spacing: 18) {
                            VStack(alignment: .leading, spacing: 8) {
                                HStack(spacing: 8) {
                                    Circle().fill(model.state?.active == true ? Color.green : Color.orange).frame(width: 8, height: 8)
                                    Text(model.proxyStatus).font(.system(size: 14, weight: .medium))
                                    Text(modeLabel(model.routingMode?.actualMode ?? "rule")).font(InterfaceType.detail).foregroundStyle(.secondary)
                                }
                                Text("ClashHub 代理")
                                    .font(.system(size: 20, weight: .semibold))
                                Text("普通流量与 AI 流量独立选路；节点详情见下方当前线路").font(InterfaceType.detail).foregroundStyle(.secondary)
                            }
                            Spacer(minLength: 10)
                            Button {
                                model.runCLI(model.state?.active == true ? "off" : "on")
                            } label: {
                                Image(systemName: model.state?.active == true ? "pause.fill" : "play.fill")
                                    .font(.system(size: 26, weight: .bold)).foregroundStyle(.white)
                                    .frame(width: 68, height: 68)
                                    .background(Circle().fill(Color.blue))
                            }.buttonStyle(.plain).disabled(model.operationBusy)
                                .help(model.state?.active == true ? "停用代理" : "启用代理")
                        }
                    }.frame(minWidth: 340, maxWidth: .infinity).layoutPriority(2)
                    Surface {
                        VStack(alignment: .leading, spacing: 6) {
                            HStack(spacing: 6) { Image(systemName: "arrow.up"); Text("上传") }
                                .font(InterfaceType.label).foregroundStyle(.secondary)
                            Text(rateText(model.uploadRate)).font(.system(size: 22, weight: .semibold, design: .rounded)).monospacedDigit()
                                .lineLimit(1).minimumScaleFactor(0.6)
                            Text("累计 \(bytes(model.menuTrafficTotals?.upload))").font(InterfaceType.detail).foregroundStyle(.secondary)
                        }
                    }.frame(width: 170)
                    Surface {
                        VStack(alignment: .leading, spacing: 6) {
                            HStack(spacing: 6) { Image(systemName: "arrow.down"); Text("下载") }
                                .font(InterfaceType.label).foregroundStyle(.secondary)
                            Text(rateText(model.downloadRate)).font(.system(size: 22, weight: .semibold, design: .rounded)).monospacedDigit()
                                .lineLimit(1).minimumScaleFactor(0.6)
                            Text("累计 \(bytes(model.menuTrafficTotals?.download))").font(InterfaceType.detail).foregroundStyle(.secondary)
                        }
                    }.frame(width: 170)
                }
                HStack(alignment: .top, spacing: 14) {
                    Surface {
                        VStack(alignment: .leading, spacing: 10) {
                            Text("出站模式").font(InterfaceType.label).foregroundStyle(.secondary)
                            HStack(spacing: 8) {
                                ForEach(["rule", "global", "direct"], id: \.self) { mode in
                                    Button { model.setRoutingMode(mode) } label: {
                                        Text(modeLabel(mode)).font(.system(size: 13, weight: .medium))
                                            .frame(maxWidth: .infinity).padding(.vertical, 7)
                                            .background(model.routingMode?.actualMode == mode ? Color.blue : Color.white.opacity(0.07), in: Capsule())
                                            .foregroundStyle(model.routingMode?.actualMode == mode ? Color.white : Color.primary)
                                    }
                                        .buttonStyle(.plain)
                                        .accessibilityValue(model.routingMode?.actualMode == mode ? "当前模式" : "未选择")
                                        .disabled(model.modeBusy || model.readingModeForMenu || model.routingMode == nil)
                                }
                            }
                            if model.routingMode == nil { Text("模式不可用，请检查服务是否运行").font(InterfaceType.detail).foregroundStyle(.secondary) }
                            if let desired = model.routingMode?.desiredMode, desired != model.routingMode?.actualMode { Text("期望模式：\(modeLabel(desired))，实际模式尚未确认").font(InterfaceType.detail).foregroundStyle(.orange) }
                            if let warning = model.routingMode?.warning, !warning.isEmpty { Text(warning).font(InterfaceType.detail).foregroundStyle(.orange) }
                            if let error = model.modeError { Text(error).font(InterfaceType.detail).foregroundStyle(.orange).textSelection(.enabled) }
                            if let error = model.operationError { Text(error).font(InterfaceType.detail).foregroundStyle(.orange).textSelection(.enabled) }
                            if model.operationBusy { Text("操作进行中…").font(InterfaceType.detail).foregroundStyle(.secondary) }
                        }
                    }.layoutPriority(2)
                    Surface {
                        VStack(alignment: .leading, spacing: 8) {
                            Text("本地代理").font(InterfaceType.label).foregroundStyle(.secondary)
                            HStack { Text("HTTP"); Spacer(); Text(model.cfgHTTP).font(.system(size: 12, design: .monospaced)) }
                            HStack { Text("SOCKS"); Spacer(); Text(model.cfgSOCKS).font(.system(size: 12, design: .monospaced)) }
                        }.font(InterfaceType.detail)
                    }.frame(minWidth: 210)
                }
                Surface {
                    VStack(alignment: .leading, spacing: 14) {
                        HStack {
                            Text("当前线路").font(.headline)
                            Spacer()
                            Text("分线路已观测速率").font(InterfaceType.detail).foregroundStyle(.secondary)
                                .help("普通流量与 AI 流量分别统计 ClashHub 实际转发的字节；共用同一节点或端口时也分开显示，短连接可能漏采。")
                        }
                        ViewThatFits(in: .horizontal) {
                            HStack(alignment: .top, spacing: 12) {
                                route("普通流量", normal, .blue).frame(minWidth: 255)
                                route("AI流量", ai, .purple).frame(minWidth: 255)
                            }
                            VStack(spacing: 12) {
                                route("普通流量", normal, .blue)
                                route("AI流量", ai, .purple)
                            }
                        }
                        if model.state?.mode == "fallback", model.state?.active == true, let updated = parseTime(model.state?.updatedAt), abs(updated.timeIntervalSinceNow) < 15 {
                            Text("备用线路 · \(model.state?.fallbackReason.flatMap { $0.isEmpty ? nil : $0 } ?? "原因未知")")
                                .font(InterfaceType.detail).foregroundStyle(.orange).lineLimit(2)
                                .help(model.state?.fallbackReason ?? "回退原因未知")
                        }
                    }
                }
                if let error = model.nodeError { Text(error + " · 当前节点可能未知").foregroundStyle(.orange).font(InterfaceType.detail) }
                HStack {
                    Text("自动选择：健康节点按 最高 → 高 → 中 → 低 → 最低 跨来源排序；同档保留来源与粘性规则，手动模式不受影响。")
                        .font(InterfaceType.detail).foregroundStyle(.secondary)
                }
                if let error = model.prioritiesError { Text(error).font(InterfaceType.detail).foregroundStyle(.orange).textSelection(.enabled) }
                if let notice = model.prioritiesNotice { Text(notice).font(InterfaceType.detail).foregroundStyle(.secondary) }
                if model.nodeControlBusy { Text("切换中…").font(InterfaceType.detail).foregroundStyle(.secondary) }
                if let error = model.nodeControlError { Text(error).font(InterfaceType.detail).foregroundStyle(.orange).textSelection(.enabled) }
                if let notice = model.nodeControlNotice { Text(notice).font(InterfaceType.detail).foregroundStyle(.secondary) }
                Text("节点切换不迁移已有连接；新连接的实际线路以当前状态为准。")
                    .font(InterfaceType.detail).foregroundStyle(.secondary)
                if let error = model.healthError { Text(error).font(InterfaceType.detail).foregroundStyle(.orange).textSelection(.enabled) }
                HStack(spacing: 10) {
                    Text("聚合候选").font(.headline)
                    Spacer()
                    if let updated = model.nodesUpdated { Text("更新于 \(updated.formatted(date: .omitted, time: .standard))").font(InterfaceType.detail).foregroundStyle(.secondary) }
                    Button(model.healthRunning || model.healthStarting ? model.healthProgressLabel : "批量检测") { model.startNodeHealth() }
                        .disabled(model.healthRunning || model.healthStarting)
                        .help("对普通、openai.com 和 chatgpt.com 分别检测；结果为最近一次探测，不保证后续连接成功。")
                    Button { model.refreshIfVisible() } label: { Label("刷新", systemImage: "arrow.clockwise") }
                }
                Surface {
                    if model.nodesUpdated == nil || japanCandidates(model.proxies) == nil {
                        Text(model.nodeError == nil && model.nodesUpdated == nil ? "尚未获取节点数据" : "聚合候选不可用（JP-ALL 缺失或数据不完整）").foregroundStyle(.secondary).frame(maxWidth: .infinity, minHeight: 90)
                    } else if model.nodeNames.isEmpty {
                        Text("聚合候选为空").foregroundStyle(.secondary).frame(maxWidth: .infinity, minHeight: 90)
                    }
                    else {
                        // Keep the node table inside a real viewport.  A GeometryReader
                        // around the scroll view lets the lazy content report its full
                        // height to the enclosing page, so long probe/error rows can
                        // paint over the fallback section below the card.  The table is
                        // intentionally scrollable in both directions instead.
                        let tableHeight = CGFloat(min(460, max(240, 48 + model.nodeNames.count * 58)))
                        ScrollView(.vertical, showsIndicators: true) {
                        VStack(alignment: .leading, spacing: 0) {
                            HStack(spacing: 12) {
                                Text("节点").frame(maxWidth: .infinity, alignment: .leading)
                                Text("优先级").frame(width: 108)
                                Text("普通流量").frame(width: 96).help("点击数值可把普通流量固定到该节点")
                                ForEach(model.cfgAITargets, id: \.self) { t in
                                    Text("AI流量·\(t)").frame(width: 96).help("点击数值可把 AI流量固定到此节点")
                                }
                            }.font(.system(size: 12, weight: .semibold)).foregroundStyle(.secondary).padding(.bottom, 8)
                            Divider()
                            ForEach(sourceGroups(model.nodeNames), id: \.0) { groupLabel, groupNames in
                            HStack(spacing: 6) {
                                Text(groupLabel).font(.system(size: 12, weight: .semibold))
                                Text("\(groupNames.count) 个节点").font(InterfaceType.detail).foregroundStyle(.secondary)
                                Spacer()
                            }.padding(.top, 10).padding(.bottom, 2)
                            ForEach(groupNames, id: \.self) { name in
                                let row = model.nodePriorities[name]
                                let normalSelected = name == normal
                                let aiSelected = name == ai
                                let canNormal = model.nodeGroups["主代理"]?.nodes.contains(name) == true || model.proxies[name] != nil
                                let canAI = model.nodeGroups["AI主代理"]?.nodes.contains(name) == true || model.proxies[name] != nil
                                HStack(spacing: 12) {
                                    Toggle("启用", isOn: Binding(
                                        get: { row?.enabled ?? false },
                                        set: { model.setNodeEnabled(node: name, enabled: $0) }
                                    )).labelsHidden().toggleStyle(.checkbox)
                                        .disabled(!model.prioritiesLoaded || model.prioritiesBusy || row == nil)
                                        .help("取消勾选后此节点不参与自动选路")
                                    VStack(alignment: .leading, spacing: 3) {
                                        HStack(spacing: 6) {
                                            Text(model.displayName(name)).font(InterfaceType.body)
                                                .lineLimit(1).truncationMode(.middle).textSelection(.enabled)
                                                .help(model.displayName(name))
                                            if normalSelected { RouteBadge(text: "普通流量", color: .blue) }
                                            if aiSelected { RouteBadge(text: "AI流量", color: .purple) }
                                        }
                                    }.frame(maxWidth: .infinity, alignment: .leading)
                                    Picker("优先级", selection: Binding(
                                        get: { model.prioritiesLoaded ? (row?.override ?? "unknown") : "unknown" },
                                        set: { model.setNodePriority(node: name, priority: $0) }
                                    )) {
                                        if !model.prioritiesLoaded || row == nil { Text("未知").tag("unknown") }
                                        Text(row.map { $0.override == "default" ? "默认（\(priorityLabel($0.priority))）" : "默认" } ?? "默认").tag("default")
                                        Text("最高").tag("highest")
                                        Text("高").tag("high")
                                        Text("中").tag("medium")
                                        Text("低").tag("low")
                                        Text("最低").tag("lowest")
                                    }.labelsHidden().frame(width: 108)
                                        .disabled(!model.prioritiesLoaded || model.prioritiesBusy || row == nil)
                                        .help("默认跟随配置来源；选择后覆盖来源规则")
                                    probeCell(model.nodeHealth[name]?.normal, width: 96, selectable: canNormal && !model.nodeControlBusy) {
                                        model.setNode(group: "主代理", node: name)
                                    }
                                    ForEach(model.cfgAITargets, id: \.self) { t in
                                        let first = t == model.cfgAITargets.first
                                        probeCell(model.nodeHealth[name]?.probe(for: t), width: 96, selectable: canAI && !model.nodeControlBusy) {
                                            if first { model.setNode(group: "AI主代理", node: name) }
                                        }
                                    }
                                }.padding(.vertical, 9)
                                if name != groupNames.last { Divider() }
                            }
                            }
                        }.controlSize(.small)
                            .frame(maxWidth: .infinity, alignment: .leading)
                        }
                        .frame(maxWidth: .infinity, minHeight: tableHeight, maxHeight: tableHeight, alignment: .topLeading)
                        .clipped()
                    }
                }
                Text("备用线路").font(.headline)
                Text("备用路径已观测流量每累计 1 GiB 会请求节点扫描，ClashHub 处于备用模式时还会请求主线路恢复探测；引擎采样可能漏掉短连接或连接尾部，并非真实流量的硬阈值。周期探测仍会继续。")
                    .font(InterfaceType.detail).foregroundStyle(.secondary)
                Surface {
                    HStack {
                        Text(model.fallbackRouteLabel).frame(maxWidth: .infinity, alignment: .leading)
                        if normal == "FALLBACK-LOCAL" { RouteBadge(text: "普通流量", color: .blue) }
                        if ai == "FALLBACK-LOCAL" { RouteBadge(text: "AI流量", color: .purple) }
                        if model.proxies["FALLBACK-LOCAL"] != nil {
                            let status = model.nodeStatus("FALLBACK-LOCAL")
                            Text(status.0).font(InterfaceType.detail.monospacedDigit()).foregroundStyle(status.1).frame(width: 90, alignment: .trailing)
                        } else {
                            Text("状态未知").font(InterfaceType.detail).foregroundStyle(.secondary).frame(width: 90, alignment: .trailing)
                        }
                    }
                }
            }.font(InterfaceType.body).padding(24).frame(maxWidth: 1100)
        }
        .onAppear { model.refreshStats() }
    }
    // Aggregated candidates are grouped by their configured source, in source
    // priority order, so the list reads like the settings table.
    func sourceGroups(_ names: [String]) -> [(String, [String])] {
        var byProfile: [String: [String]] = [:]
        for name in names {
            let profile = model.labels[name]?.profile ?? "未归属来源"
            byProfile[profile.isEmpty ? "未归属来源" : profile, default: []].append(name)
        }
        var out: [(String, [String])] = []
        for row in model.sourceDraft {
            let label = row.label.isEmpty ? row.id : row.label
            if let group = byProfile.removeValue(forKey: label), !group.isEmpty {
                out.append((label, group))
            }
        }
        for key in byProfile.keys.sorted() {
            if let group = byProfile[key], !group.isEmpty { out.append((key, group)) }
        }
        return out
    }
    func priorityLabel(_ value: String) -> String {
        switch value {
        case "highest": return "最高"
        case "high": return "高"
        case "medium": return "中"
        case "low": return "低"
        case "lowest": return "最低"
        default: return "默认"
        }
    }
    func prioritySummary(_ row: NodePriorityRow?, manualGroup: String) -> String {
        guard let row else { return "优先级未知" }
        var parts = ["生效 " + priorityLabel(row.priority)]
        if !row.enabled { parts.append("已禁用") }
        if !manualGroup.isEmpty { parts.append("手动 " + manualGroup) }
        return parts.joined(separator: " · ")
    }
    // Compact probe cell: the dot carries health, the text carries the value.
    @ViewBuilder func probeCell(_ probe: NodeProbe?, width: CGFloat, selectable: Bool, action: @escaping () -> Void) -> some View {
        let expired = probe?.stale == true || (parseTime(probe?.expiresAt).map { $0 <= Date() } ?? false)
        let text: String = {
            guard let probe else { return "—" }
            switch probe.status {
            case "ok": return probe.delayMS.map { "\($0) ms" } ?? "通"
            case "failed": return "失败"
            case "error": return "错误"
            default: return "未知"
            }
        }()
        let color: Color = probe == nil ? .secondary : (probe?.status == "ok" ? .green : (probe?.status == "error" ? .orange : .red))
        let detail = [probe?.error, probe?.checkedAt.map { "检测时间：\($0)" }, probe?.expiresAt.map { "有效期至：\($0)" }]
            .compactMap { $0 }.joined(separator: "\n")
        Button(action: action) {
            HStack(spacing: 5) {
                Circle().fill(expired ? Color.orange.opacity(0.6) : color).frame(width: 7, height: 7)
                Text(expired ? text + " ·旧" : text).font(InterfaceType.detail).foregroundStyle(expired ? Color.orange : color)
            }.frame(width: width, alignment: .leading).contentShape(Rectangle())
        }
        .buttonStyle(.plain)
        .disabled(!selectable)
        .help(selectable ? "点击把流量固定到该节点\n" + detail : (detail.isEmpty ? "暂无检测详情" : detail))
    }
    @ViewBuilder func probeStatus(_ probe: NodeProbe?) -> some View {
        let expired = probe?.stale == true || (parseTime(probe?.expiresAt).map { $0 <= Date() } ?? false)
        let text = probe.map { p in
            (expired ? "已过时 · " : "") + (p.status == "ok" ? (p.delayMS.map { "\($0) ms" } ?? "成功") : (p.status == "failed" ? "失败" : (p.status == "error" ? "错误" : "未知")))
        } ?? "未检测"
        let detail = [probe?.error, probe?.checkedAt.map { "检测时间：\($0)" }, probe?.expiresAt.map { "有效期至：\($0)" }].compactMap { $0 }.joined(separator: "\n")
        Text(text).font(InterfaceType.detail).foregroundStyle(expired ? Color.orange : (probe?.status == "ok" ? Color.green : (probe == nil ? Color.secondary : Color.red)))
            .help(detail.isEmpty ? "暂无检测详情" : detail)
        if let reason = probe?.error, !reason.isEmpty { Text(reason).font(InterfaceType.detail).foregroundStyle(.secondary).lineLimit(2).help(reason) }
    }
    @ViewBuilder func route(_ title: String, _ name: String?, _ color: Color) -> some View {
        VStack(alignment: .leading, spacing: 5) {
            let group = title == "普通流量" ? "主代理" : "AI主代理"
            let info = model.nodeGroups[group]
            HStack {
                RouteBadge(text: title, color: color)
                if group == "AI主代理" {
                    Text(model.aiRouteTitle).font(InterfaceType.detail).foregroundStyle(.secondary).lineLimit(1)
                }
                Spacer()
                Text(info?.mode == "manual" ? "手动" : (info?.mode == "auto" ? "自动" : "状态未知"))
                    .font(InterfaceType.detail).foregroundStyle(.secondary)
            }
            Text(name.map(model.displayName) ?? "实际线路未知")
                .font(.system(size: 15, weight: .semibold)).textSelection(.enabled)
                .lineLimit(2).frame(minHeight: 20, alignment: .topLeading)
            // Selector line only when it differs: it adds information solely
            // when the selector points at an aggregate group or a mismatch.
            if info?.selected != nil && info?.selected != name {
                Text("选择器：" + (info?.selected.map(model.displayName) ?? "未知"))
                    .font(InterfaceType.detail).foregroundStyle(.secondary).textSelection(.enabled)
            }
            if name == "FALLBACK-LOCAL", model.state?.mode == "fallback", info?.mode == "manual" || info?.desiredMode == "manual" {
                Text("当前处于备用线路，手动偏好尚未用于实际线路。")
                    .font(InterfaceType.detail).foregroundStyle(.orange)
            }
            HStack {
                Text(model.routeRateText(group == "AI主代理" ? "ai" : "normal"))
                    .font(InterfaceType.detail.monospacedDigit()).foregroundStyle(.secondary)
                    .help("分别统计 OpenAI 域名与其他流量，按 ClashHub 实际转发的字节计算；可能漏采短连接。")
                Spacer()
                let locked = info?.mode == "manual" || info?.desiredMode == "manual"
                Button {
                    model.setNode(group: group, node: locked ? nil : name)
                } label: {
                    Label(locked ? "解锁" : "锁定", systemImage: locked ? "lock.open" : "lock")
                }
                .controlSize(.small)
                .help(locked ? "解锁后恢复自动选路" : "锁定到当前节点，不再自动切换")
                .disabled(model.nodeControlBusy || model.readingNodeGroups || name == nil)
            }
            if let info, let warning = info.warning, !warning.isEmpty {
                Text(warning).font(InterfaceType.detail).foregroundStyle(.orange).textSelection(.enabled)
            }
            if let info, info.mode == "manual" || info.desiredMode == "manual",
               info.manualNode == nil || info.selected != info.manualNode || !info.nodes.contains(info.manualNode ?? "") || info.mode != "manual" {
                Text("手动偏好未确认生效").font(InterfaceType.detail).foregroundStyle(.orange)
                    .help("手动偏好：\(info.manualNode.map(model.displayName) ?? "未知")；选择器：\(info.selected.map(model.displayName) ?? "未知")")
            }
        }
        .padding(11).frame(maxWidth: .infinity, alignment: .leading)
        .background(color.opacity(0.045), in: RoundedRectangle(cornerRadius: 10))
        .overlay(RoundedRectangle(cornerRadius: 10).strokeBorder(color.opacity(0.13)))
    }
}
// UTC-aligned buckets match the backend's hourly aggregation, including the
// current partial hour. No samples in a bucket means unknown, not 0%.
struct HealthKey: Hashable { let node: String; let probe: String; let hour: Int64 }
struct HealthCount: Equatable { var attempts = 0.0; var successes = 0.0 }
struct HealthSnapshot: Equatable {
    var nodes: [String] = []
    var indexed: [HealthKey: HealthCount] = [:]
    var hour: Int64 = 0
    var firstHour: Int64 = 0
}
func buildHealthSnapshot(records: [StatsHour], nodeNames: [String], now: Date, minutes: Int = 2880, aiTargets: [String] = ["openai"]) -> HealthSnapshot {
    let currentHour = Int64(floor(now.timeIntervalSince1970 / 3600))
    let firstHour = Int64(floor((now.timeIntervalSince1970 - Double(minutes * 60)) / 3600))
    var hashes: [String: String] = [:]
    for node in nodeNames {
        let hash = SHA256.hash(data: Data(node.utf8)).map { String(format: "%02x", $0) }.joined()
        hashes[hash] = node
    }
    let fractional = ISO8601DateFormatter()
    fractional.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    let standard = ISO8601DateFormatter()
    var dates: [String: Int64] = [:]
    var indexed: [HealthKey: HealthCount] = [:]
    for record in records {
        guard let name = record.Name else { continue }
        let parts = name.split(separator: "-", omittingEmptySubsequences: false)
        var probeKind = parts.count == 3 ? String(parts[2]) : ""
        if probeKind != "gstatic" && aiTargets.contains(probeKind) { probeKind = "ai" }
        guard parts.count == 3, parts[0] == "node", let node = hashes[String(parts[1])],
              probeKind == "gstatic" || probeKind == "ai", let text = record.Hour,
              let attempts = record.Attempts, let successes = record.Successes,
              attempts > 0, successes <= attempts else { continue }
        let hour: Int64
        if let cached = dates[text] { hour = cached }
        else {
            guard let date = fractional.date(from: text) ?? standard.date(from: text) else { continue }
            hour = Int64(floor(date.timeIntervalSince1970 / 3600)); dates[text] = hour
        }
        guard hour >= firstHour, hour <= currentHour else { continue }
        let key = HealthKey(node: node, probe: probeKind, hour: hour)
        var count = indexed[key] ?? HealthCount()
        count.attempts += Double(attempts); count.successes += Double(successes)
        indexed[key] = count
    }
    return HealthSnapshot(nodes: Array(Set(nodeNames)).sorted { $0.localizedStandardCompare($1) == .orderedAscending }, indexed: indexed, hour: currentHour, firstHour: firstHour)
}
// Per-bucket upload/download trend. Hover reports the exact bucket instead of
// relying on tiny hit areas.
struct NetworkSeriesChart: View {
    let points: [NetworkPoint]
    let bucketMinutes: Int
    @State private var hoverIndex: Int?
    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            GeometryReader { geo in
                let width = max(1, geo.size.width)
                let height = max(1, geo.size.height)
                let maximum = max(1.0, points.map { Double($0.upload) + Double($0.download) }.max() ?? 1)
                ZStack(alignment: .topLeading) {
                    linePath(width: width, height: height, maximum: maximum, upload: false)
                        .stroke(Color.blue.opacity(0.85), lineWidth: 1.5)
                    linePath(width: width, height: height, maximum: maximum, upload: true)
                        .stroke(Color.orange.opacity(0.9), lineWidth: 1.5)
                    if let index = hoverIndex, index < points.count {
                        let x = points.count > 1 ? width * CGFloat(index) / CGFloat(points.count - 1) : 0
                        Rectangle().fill(Color.secondary.opacity(0.35))
                            .frame(width: 1, height: height)
                            .position(x: x, y: height / 2)
                    }
                }
                .contentShape(Rectangle())
                .onContinuousHover { phase in
                    switch phase {
                    case .active(let location):
                        guard points.count > 1, width > 0 else { hoverIndex = nil; return }
                        let ratio = min(max(location.x / width, 0), 1)
                        let index = Int((ratio * CGFloat(points.count - 1)).rounded())
                        hoverIndex = min(max(index, 0), points.count - 1)
                    case .ended:
                        hoverIndex = nil
                    }
                }
            }.frame(height: 130)
            if let index = hoverIndex, index < points.count {
                let point = points[index]
                Text("\(parseTime(point.start)?.formatted(date: .abbreviated, time: .shortened) ?? point.start ?? "未知时间") · 上传 \(bytes(point.upload)) · 下载 \(bytes(point.download))")
                    .font(InterfaceType.detail).monospacedDigit()
            } else {
                Text("橙色=上传 蓝色=下载；悬停查看每 \(bucketMinutes == 60 ? "小时" : "10 分钟")明细").font(InterfaceType.detail).foregroundStyle(.secondary)
            }
        }
    }
    private func linePath(width: CGFloat, height: CGFloat, maximum: Double, upload: Bool) -> Path {
        Path { path in
            for (index, point) in points.enumerated() {
                let value = Double(upload ? point.upload : point.download)
                let x = points.count > 1 ? width * CGFloat(index) / CGFloat(points.count - 1) : 0
                let y = height - CGFloat(value / maximum) * height
                if index == 0 { path.move(to: CGPoint(x: x, y: y)) } else { path.addLine(to: CGPoint(x: x, y: y)) }
            }
        }
    }
}
struct HealthGrid: View, Equatable {
    let snapshot: HealthSnapshot
    let revision: Int
    let labels: [String: NodeLabel]
    let usageAvailable: Bool
    let windowLabel: String
    static func == (lhs: HealthGrid, rhs: HealthGrid) -> Bool {
        lhs.revision == rhs.revision && lhs.labels == rhs.labels && lhs.usageAvailable == rhs.usageAvailable && lhs.windowLabel == rhs.windowLabel
    }
    private var hours: [Int64] { Array(snapshot.firstHour...snapshot.hour) }
    private func displayName(_ node: String) -> String {
        guard let label = labels[node], !label.profile.isEmpty, !label.name.isEmpty else { return node }
        return "\(label.profile)：\(label.name)"
    }
    private func label(_ hour: Int64) -> String {
        Date(timeIntervalSince1970: Double(hour) * 3600).formatted(date: .abbreviated, time: .shortened)
    }
    private func detail(_ count: HealthCount?) -> String {
        guard let count, count.attempts > 0 else { return "未知（无有效样本）" }
        return String(format: "%.1f%% · 成功 %.0f / 探测 %.0f", count.successes / count.attempts * 100, count.successes, count.attempts)
    }
    private func color(_ count: HealthCount?) -> Color {
        guard let count, count.attempts > 0 else { return .gray.opacity(0.25) }
        if count.successes == count.attempts { return .green }
        return count.successes == 0 ? .red : .orange
    }
    var body: some View {
        let nodes = snapshot.nodes
        let indexed = snapshot.indexed
        VStack(alignment: .leading, spacing: 8) {
            Text("近 \(windowLabel) · 按小时分桶，从左到右由旧到新（首尾可能为不完整小时）").font(InterfaceType.detail).foregroundStyle(.secondary)
            Text("绿色：全部成功 · 橙色：部分成功 · 红色：全部失败 · 灰色：未知；悬停查看次数和时间").font(InterfaceType.detail).foregroundStyle(.secondary)
            if hours.count > 26, let first = hours.first, let last = hours.last {
                Text("覆盖 \(label(first)) 至 \(label(last))").font(InterfaceType.detail).foregroundStyle(.secondary)
            }
            if nodes.isEmpty {
                Text(usageAvailable ? "当前数据周期内暂无可确认使用的节点" : "节点使用记录不可用，暂无法展示健康度")
                    .foregroundStyle(.secondary)
            } else {
                // Every row and every hour fits the panel: cells share the width
                // evenly, so nothing needs horizontal or inner vertical scrolling.
                VStack(alignment: .leading, spacing: 3) {
                    HStack(spacing: 2) {
                        Text("节点 / 探测").frame(width: 168, alignment: .leading)
                        ForEach(hours, id: \.self) { hour in
                            Text(hours.count <= 26 && hour % 6 == 0 ? Date(timeIntervalSince1970: Double(hour) * 3600).formatted(.dateTime.hour()) : "")
                                .font(.system(size: 9)).lineLimit(1).frame(maxWidth: .infinity).help(label(hour))
                        }
                    }.font(InterfaceType.detail)
                    ForEach(nodes, id: \.self) { node in
                        ForEach(["gstatic", "ai"], id: \.self) { probe in
                            HStack(spacing: 2) {
                                Text("\(displayName(node)) · \(probe)").font(InterfaceType.detail)
                                    .lineLimit(1).truncationMode(.middle)
                                    .frame(width: 168, alignment: .leading).help(displayName(node) + " · " + probe)
                                ForEach(hours, id: \.self) { hour in
                                    let count = indexed[HealthKey(node: node, probe: probe, hour: hour)]
                                    RoundedRectangle(cornerRadius: 2).fill(color(count))
                                        .frame(maxWidth: .infinity).frame(height: 12)
                                        .contentShape(Rectangle())
                                        .help("\(node) · \(probe) · \(label(hour))：\(detail(count))")
                                        .accessibilityLabel("\(node)，\(probe)，\(label(hour))，\(detail(count))")
                                }
                            }.frame(height: 18)
                        }
                    }
                }.padding(.vertical, 2)
            }
            Text("仅显示当前数据周期内观测到流量且仍可匹配的节点；采样可能漏掉短连接，旧记录无法确认使用时不展示。") .font(InterfaceType.detail).foregroundStyle(.secondary)
        }.frame(maxWidth: .infinity, alignment: .leading)
    }
}
// Missing byte counters sort after complete totals; overflow is compared exactly.
func trafficGreater(_ au: UInt64?, _ ad: UInt64?, _ bu: UInt64?, _ bd: UInt64?) -> Bool {
    guard let au, let ad else { return false }
    guard let bu, let bd else { return true }
    let a = au.addingReportingOverflow(ad), b = bu.addingReportingOverflow(bd)
    return a.overflow != b.overflow ? a.overflow : a.partialValue > b.partialValue
}
func bytes(_ n: UInt64?) -> String {
    guard let n else { return "未知" }
    if n == 0 { return "0 B" }
    return n > UInt64(Int64.max) ? "\(n) 字节" : ByteCountFormatter.string(fromByteCount: Int64(n), countStyle: .file)
}
// The statistics API supplies normalized hostnames or IP literals, not URLs.
// Never silently present an incomplete or overflowing aggregate as a complete total.
struct TargetTraffic: Identifiable {
    let id: String
    var validTarget: Bool = true
    var upload: UInt64? = 0
    var download: UInt64? = 0
    var detailStartedAt: String?
    var detailStartInvalid = false
    var attempts: UInt64? = 0
    var total: Decimal? {
        guard let upload, let download else { return nil }
        return Decimal(upload) + Decimal(download)
    }
}
func targetTraffic(_ rows: [StatsRow]) -> [TargetTraffic] {
    var groups: [String: TargetTraffic] = [:]
    func sum(_ a: UInt64?, _ b: UInt64?) -> UInt64? {
        guard let a, let b else { return nil }
        let result = a.addingReportingOverflow(b)
        return result.overflow ? nil : result.partialValue
    }
    for row in rows {
        let target = (row.Target ?? "").trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
        let key = target
        var item = groups[key] ?? TargetTraffic(id: key, validTarget: !key.isEmpty)
        if let start = row.detailStartedAt, let date = parseTime(start), date.timeIntervalSince1970 > -62135596800 {
            if let existing = item.detailStartedAt, let previous = parseTime(existing) {
                if date != previous { item.detailStartInvalid = true }
            } else { item.detailStartedAt = start }
        } else { item.detailStartInvalid = true }
        item.upload = sum(item.upload, row.Upload)
        item.download = sum(item.download, row.Download)
        item.attempts = sum(item.attempts, row.Attempts)
        groups[key] = item
    }
    return groups.values.sorted {
        switch ($0.total, $1.total) {
        case let (a?, b?) where a != b: return a > b
        case (_?, nil): return true
        case (nil, _?): return false
        default: return $0.id < $1.id
        }
    }
}
func isIPTarget(_ target: String) -> Bool {
    var v4 = in_addr(); var v6 = in6_addr()
    return target.withCString { inet_pton(AF_INET, $0, &v4) == 1 || inet_pton(AF_INET6, $0, &v6) == 1 }
}
func isDNSDomain(_ target: String) -> Bool {
    guard !isIPTarget(target), target.utf8.count <= 253, !target.isEmpty else { return false }
    let labels = target.split(separator: ".", omittingEmptySubsequences: false)
    return labels.allSatisfy { label in
        guard !label.isEmpty, label.utf8.count <= 63, label.first != "-", label.last != "-" else { return false }
        return label.utf8.allSatisfy { (97...122).contains($0) || (48...57).contains($0) || $0 == 45 }
    }
}
func totalBytes(_ traffic: TargetTraffic) -> String {
    guard let upload = traffic.upload, let download = traffic.download else { return "未知" }
    let sum = upload.addingReportingOverflow(download)
    if sum.overflow { return "\(NSDecimalNumber(decimal: traffic.total!).stringValue) 字节" }
    return bytes(sum.partialValue)
}
func sortedDomainRanking(_ rows: [DomainRank]) -> [DomainRank] {
    rows.sorted {
        let a = Decimal($0.upload) + Decimal($0.download)
        let b = Decimal($1.upload) + Decimal($1.download)
        return a == b ? $0.target < $1.target : a > b
    }
}
func attemptCountText(_ count: UInt64?, startedAt: String?, complete: Bool?) -> String {
    guard let count else { return "连接尝试次数：未知" }
    let start = parseTime(startedAt)
    let validStart = start.map { $0.timeIntervalSince1970 > -62135596800 } == true
    let status = complete == true && validStart ? "接口标记完整" : "覆盖可能不完整"
    let coverage = validStart ? "次数统计开始于 \(parseTime(startedAt)!.formatted(date: .numeric, time: .standard))" : "次数统计开始时间未知"
    return "连接尝试次数：\(count)（\(status)；\(coverage)）"
}
func detailStartText(_ text: String?) -> String {
    guard let date = parseTime(text), date.timeIntervalSince1970 > -62135596800 else { return "明细开始时间未知" }
    return "明细开始于 \(date.formatted(date: .numeric, time: .standard))"
}
struct BlockAction: Identifiable {
    let domain: String; let blocked: Bool
    var id: String { domain }
}

struct DomainView: View {
    @EnvironmentObject var model: Model
    @State private var page = 0
    @State private var customBlockInput = ""
    @State private var pendingBlock: BlockAction?
    private let pageSize = 20
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                PageHeader(title: "域名", subtitle: "历史域名累计与屏蔽规则", icon: "globe")
                if let error = model.blockError { Text(error).font(InterfaceType.detail).foregroundStyle(.orange).textSelection(.enabled) }
                if let notice = model.blockNotice { Text(notice).font(InterfaceType.detail).foregroundStyle(.secondary) }
                Surface {
                    VStack(alignment: .leading, spacing: 12) {
                        Text("历史域名（累计）").font(.headline)
                        Text("自统计启用以来的累计上传 + 下载，按总量降序；不受当前统计周期影响，升级前的历史最多回溯 48 小时。分钟桶边界可能包含额外时间，采样也可能不完整。")
                            .font(InterfaceType.detail).foregroundStyle(.secondary)
                        if let history = model.stats?.domainHistory {
                            let total = history.count
                            let pageCount = max(1, (total + pageSize - 1) / pageSize)
                            let current = min(page, pageCount - 1)
                            let pageRows = Array(history[(current * pageSize)..<min(total, (current + 1) * pageSize)])
                            if pageRows.isEmpty {
                                Text("暂无历史域名记录").foregroundStyle(.secondary)
                            } else {
                                LazyVStack(alignment: .leading, spacing: 0) {
                                    ForEach(Array(pageRows.enumerated()), id: \.offset) { index, row in
                                        HStack(alignment: .center, spacing: 12) {
                                            Text("\(current * pageSize + index + 1)").font(InterfaceType.detail).foregroundStyle(.secondary).frame(width: 28, alignment: .trailing)
                                            VStack(alignment: .leading, spacing: 2) {
                                                Text(row.target).lineLimit(1).truncationMode(.middle).textSelection(.enabled)
                                                Text("上传 \(bytes(row.upload)) · 下载 \(bytes(row.download))").font(InterfaceType.detail).foregroundStyle(.secondary)
                                            }.frame(maxWidth: .infinity, alignment: .leading)
                                            Text(totalBytes(TargetTraffic(id: row.target, upload: row.upload, download: row.download))).monospacedDigit().fixedSize()
                                            Button(model.blockedDomains?.contains(row.target) == true ? "解除" : "屏蔽") {
                                                pendingBlock = BlockAction(domain: row.target, blocked: model.blockedDomains?.contains(row.target) != true)
                                            }.buttonStyle(.plain).font(InterfaceType.detail)
                                                .disabled(model.blockBusy || model.blockedDomains == nil || !isDNSDomain(row.target))
                                        }.font(InterfaceType.body).padding(.vertical, 4)
                                        Divider()
                                    }
                                }
                                HStack(spacing: 10) {
                                    Button("上一页") { if current > 0 { page = current - 1 } }.disabled(current == 0)
                                    Text("第 \(current + 1) / \(pageCount) 页").font(InterfaceType.detail).foregroundStyle(.secondary)
                                    Button("下一页") { if current + 1 < pageCount { page = current + 1 } }.disabled(current + 1 >= pageCount)
                                    Spacer()
                                    Text("每页 20 条 · 共 \(total) 条").font(InterfaceType.detail).foregroundStyle(.secondary)
                                }
                            }
                        } else { Text("历史域名数据不可用").foregroundStyle(.secondary) }
                    }
                }
                Surface {
                    VStack(alignment: .leading, spacing: 8) {
                        HStack {
                            Text("持久屏蔽域名").font(.headline)
                            Spacer()
                            Button("刷新列表") { model.refreshBlockedDomains() }.disabled(model.blockBusy)
                        }
                        Text("仅精确匹配 ASCII 域名，不包含子域名；IP 和通配符不可屏蔽。仅影响经过 ClashHub 的新请求或新隧道；不检查隧道内目标，也不会关闭已有隧道。")
                            .font(InterfaceType.detail).foregroundStyle(.orange)
                        HStack(spacing: 8) {
                            TextField("添加域名，例如 example.com", text: $customBlockInput)
                                .textFieldStyle(.roundedBorder)
                            Button("添加") {
                                let domain = customBlockInput.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
                                guard isDNSDomain(domain) else { return }
                                model.setBlocked(domain, blocked: true)
                                customBlockInput = ""
                                model.refreshBlockedDomains()
                            }.disabled(!isDNSDomain(customBlockInput.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()))
                        }
                        if let domains = model.blockedDomains {
                            if domains.isEmpty { Text("没有持久屏蔽的域名").foregroundStyle(.secondary) }
                            LazyVStack(alignment: .leading, spacing: 0) {
                                ForEach(domains.sorted(), id: \.self) { domain in
                                    HStack {
                                        Text(domain).textSelection(.enabled)
                                        Spacer()
                                        Button("解除") { model.setBlocked(domain, blocked: false) }
                                            .disabled(model.blockBusy || !isDNSDomain(domain))
                                    }.padding(.vertical, 2)
                                    Divider()
                                }
                            }
                        } else {
                            Text(model.blockBusy ? "正在读取屏蔽列表…" : "屏蔽列表不可用，请刷新").foregroundStyle(.secondary)
                        }
                    }.frame(maxWidth: .infinity, alignment: .leading)
                }
            }
        }
        .onAppear { model.refreshStats(); model.refreshBlockedDomains() }
        .alert(item: $pendingBlock) { action in
            Alert(title: Text(action.blocked ? "确认屏蔽域名？" : "确认解除屏蔽？"),
                  message: Text("\(action.domain)\n仅精确匹配此域名，不包含子域名；不影响 IP 目标。仅影响经过 ClashHub 的新请求或新隧道；不检查隧道内目标，也不会关闭已有隧道。"),
                  primaryButton: .destructive(Text(action.blocked ? "屏蔽" : "解除")) { model.setBlocked(action.domain, blocked: action.blocked) },
                  secondaryButton: .cancel())
        }
    }
}

// Null or invalid counters remain unknown; never silently wrap an aggregate.
struct SubscriptionInfo: Decodable {
    let upload: UInt64?; let download: UInt64?; let total: UInt64?; let expire: Int64?
}
struct SubscriptionProfile: Decodable {
    let source: String; let id: String; let name: String?; let subscriptionInfo: SubscriptionInfo
    let lastUpdateDate: Int64?; let status: String; let sourceID: String?
    enum CodingKeys: String, CodingKey { case source, id, name, status; case sourceID = "source_id", subscriptionInfo = "subscription_info", lastUpdateDate = "last_update_date" }
}
struct SubscriptionResponse: Decodable {
    let profiles: [SubscriptionProfile]; let refreshing: Bool; let updatedAt: String?
    let refreshStatuses: [String: JiuyiOfflineStatus]?
    let office91Mode: String?; let office91OfflineRunning: Bool?; let office91Enabled: Bool?
    enum CodingKeys: String, CodingKey { case profiles, refreshing; case updatedAt = "updated_at", refreshStatuses = "refresh_statuses", office91Mode = "office91_mode", office91OfflineRunning = "office91_offline_running", office91Enabled = "office91_enabled" }
}
struct JiuyiOfflineStatus: Decodable {
    let status: String; let error: String?; let lastAttempt: String?; let lastSuccess: String?
    enum CodingKeys: String, CodingKey { case status, error; case lastAttempt = "last_attempt", lastSuccess = "last_success" }
}
struct JiuyiOfflineAccepted: Decodable {
    let status: String; let sourceID: String
    enum CodingKeys: String, CodingKey { case status; case sourceID = "source_id" }
}
func jiuyiOfflinePayload(_ profilePath: String?) -> [String: String] {
    var payload = ["source_id": "office91", "mode": "offline"]
    if let profilePath { payload["profile_path"] = profilePath }
    return payload
}
func jiuyiOfflineErrorText(_ error: String?, httpCode: Int = 0) -> String {
    switch error {
    case "offline-profile-identity-ambiguous": return "检测到多个本机 profile，请选择要导入的文件。"
    case "offline-profile-unavailable": return "未找到可用的本机 profile，请选择文件。"
    case "offline-source-disabled": return "请先启用玖壹来源并保存，再导入节点。"
    case "offline-key-unavailable", "offline-key-bootstrap-unavailable": return "本机解密凭据尚未就绪，请检查服务的可信导入组件。"
    case "offline-key-bootstrap-failed": return "本机解密初始化失败，请检查可信导入组件及 Python 依赖。"
    case "offline-key-bootstrap-identity-mismatch": return "本机解密工具版本无法验证，请更新可信导入组件。"
    case "offline-python-unavailable", "offline-python-requires-absolute-path": return "本机解密依赖不可用，请检查服务安装。"
    case "offline-rollback-failed", "offline-mode-rollback-failed": return "导入未完成，旧配置恢复未获确认，请检查服务状态。"
    case "offline-reload-failed", "offline-generation-failed": return "节点生成或引擎重载失败，未确认导入生效。"
    case "timeout", "canceled": return "导入已超时或取消，未确认节点生效。"
    case "offline-runtime-unavailable": return "服务正在停止，请重新启用后再试。"
    default: return httpCode > 0 ? "导入请求未获接受（HTTP \(httpCode)），请检查本机服务。" : "认证或配置校验未通过，未确认节点生效。"
    }
}
func jiuyiOfflineStateText(_ mode: String?, _ status: JiuyiOfflineStatus?) -> String {
    guard mode == "offline" else { return mode == "online" ? "当前使用在线来源" : "离线导入状态未知" }
    switch status?.status {
    case "running": return "正在校验并更新本机节点…"
    case "ok": return "本机节点已导入并完成引擎重载"
    case "cached": return "缓存已更新，尚未确认引擎重载"
    case "error": return jiuyiOfflineErrorText(status?.error)
    default: return "离线导入结果尚未确认"
    }
}
func subscriptionDate(_ value: Int64?) -> Date? {
    guard let value, value > 0 else { return nil }
    // Profile database dates are often milliseconds; subscription expiry is typically seconds.
    let seconds = value >= 100_000_000_000 ? Double(value) / 1000 : Double(value)
    guard seconds <= 253_402_300_799 else { return nil }
    return Date(timeIntervalSince1970: seconds)
}
// One labelled progress bar; callers supply an honest fraction (nil = unknown).
struct UsageBar: View {
    let title: String
    let fraction: Double?
    let detail: String
    let tint: Color
    var body: some View {
        VStack(alignment: .leading, spacing: 3) {
            HStack(spacing: 6) {
                Text(title).font(InterfaceType.detail).foregroundStyle(.secondary)
                Text(detail).font(InterfaceType.detail).monospacedDigit()
                Spacer()
                if let fraction { Text(String(format: "%.0f%%", fraction * 100)).font(InterfaceType.detail).foregroundStyle(.secondary) }
            }
            GeometryReader { geo in
                ZStack(alignment: .leading) {
                    Capsule().fill(Color.primary.opacity(0.08))
                    if let fraction {
                        Capsule().fill(tint).frame(width: max(2, geo.size.width * min(max(fraction, 0), 1)))
                    }
                }
            }.frame(height: 6)
        }
    }
}
struct SubscriptionAllowance: Identifiable {
    let id: String
    let name: String
    let sourceID: String?
    let used: UInt64?
    let total: UInt64?
    let remaining: UInt64?
    let expiresAt: Date?
    let updatedAt: Date?
    let status: String
    init(_ profile: SubscriptionProfile) {
        id = profile.source + ":" + profile.id
        sourceID = profile.sourceID
        name = profile.name.flatMap { $0.isEmpty ? nil : $0 } ?? "\(profile.source) · \(profile.id)"
        let info = profile.subscriptionInfo
        if let upload = info.upload, let download = info.download {
            let sum = upload.addingReportingOverflow(download)
            used = sum.overflow ? nil : sum.partialValue
        } else { used = nil }
        total = info.total
        if let total, let used { remaining = total >= used ? total - used : 0 }
        else { remaining = nil }
        expiresAt = subscriptionDate(info.expire)
        updatedAt = subscriptionDate(profile.lastUpdateDate)
        status = profile.status
    }
}
struct StatsView: View {
    @EnvironmentObject var model: Model
    @State private var trendBucket = 10
    private var healthPanel: some View {
        Surface {
            VStack(alignment: .leading, spacing: 8) {
                HStack {
                    Text("节点健康度").font(.headline)
                    Spacer()
                    periodPicker
                }
                HealthGrid(snapshot: model.healthSnapshot, revision: model.healthRevision, labels: model.labels, usageAvailable: model.statsWindowTraffic?.windowMinutes == model.displayedStatsWindow?.rawValue && model.statsWindowTraffic?.coverage == "partial", windowLabel: model.displayedStatsWindow?.label ?? "暂无").equatable()
            }
        }
    }
    private var nodeRankingPanel: some View {
        Surface {
            VStack(alignment: .leading, spacing: 8) {
                Text("节点流量 TOP 10").font(.headline)
                Text("所选周期 · 上传 + 下载降序 · 仅已观测流量，可能漏采短连接")
                    .font(InterfaceType.detail).foregroundStyle(.secondary)
                if model.displayedStatsWindow != model.statsWindow {
                    Text("等待所选周期数据").foregroundStyle(.secondary)
                } else if let traffic = model.statsWindowTraffic, traffic.windowMinutes == model.statsWindow.rawValue {
                    Text("覆盖：\(traffic.coverage == "partial" ? "部分覆盖" : traffic.coverage) · 采样状态：\(traffic.samplingStatus)")
                        .font(InterfaceType.detail).foregroundStyle(traffic.samplingStatus == "ok" ? Color.secondary : Color.orange)
                    if let updated = parseTime(traffic.updatedAt) {
                        Text("采样更新于 \(updated.formatted())").font(InterfaceType.detail).foregroundStyle(.secondary)
                    }
                    let rows = traffic.nodes.filter { $0.upload > 0 || $0.download > 0 }.sorted {
                        let a = Decimal($0.upload) + Decimal($0.download)
                        let b = Decimal($1.upload) + Decimal($1.download)
                        return a == b ? $0.node < $1.node : a > b
                    }.prefix(10)
                    if rows.isEmpty {
                        Text(traffic.samplingStatus == "ok" ? "暂无观测到的节点流量" : "采样不可用或尚无观测记录")
                            .foregroundStyle(.secondary)
                    } else {
                        ForEach(Array(rows.enumerated()), id: \.offset) { index, row in
                            HStack(alignment: .top, spacing: 8) {
                                Text("\(index + 1)").font(InterfaceType.detail).foregroundStyle(.secondary).frame(width: 20, alignment: .trailing)
                                VStack(alignment: .leading, spacing: 2) {
                                    Text(model.displayName(row.node)).lineLimit(1).truncationMode(.middle).help(model.displayName(row.node))
                                    Text("上传 \(bytes(row.upload)) · 下载 \(bytes(row.download))").font(InterfaceType.detail).foregroundStyle(.secondary)
                                }.frame(maxWidth: .infinity, alignment: .leading)
                                Text(totalBytes(TargetTraffic(id: row.node, upload: row.upload, download: row.download))).monospacedDigit().fixedSize()
                            }.font(InterfaceType.body)
                            if index + 1 < rows.count { Divider() }
                        }
                    }
                } else {
                    Text("节点流量数据不可用").foregroundStyle(.secondary)
                }
            }.frame(maxWidth: .infinity, alignment: .leading)
        }
    }
    private func hourlyPoints(_ points: [NetworkPoint]) -> [NetworkPoint] {
        var order: [Int64] = []
        var sums: [Int64: (UInt64, UInt64)] = [:]
        for point in points {
            guard let date = parseTime(point.start) else { continue }
            let hour = Int64(date.timeIntervalSince1970 / 3600)
            if sums[hour] == nil { order.append(hour) }
            let current = sums[hour] ?? (0, 0)
            sums[hour] = (current.0 &+ point.upload, current.1 &+ point.download)
        }
        return order.sorted().compactMap { hour in
            guard let sum = sums[hour] else { return nil }
            let start = ISO8601DateFormatter().string(from: Date(timeIntervalSince1970: TimeInterval(hour * 3600)))
            return NetworkPoint(start: start, upload: sum.0, download: sum.1)
        }
    }
    private var trafficPanel: some View {
        Surface {
            VStack(alignment: .leading, spacing: 8) {
                HStack {
                    Text("流量趋势").font(.headline)
                    Spacer()
                    Picker("粒度", selection: $trendBucket) {
                        Text("10 分钟").tag(10)
                        Text("1 小时").tag(60)
                    }.pickerStyle(.segmented).labelsHidden().frame(width: 150)
                }
                if let series = model.stats?.trafficSeries, !series.isEmpty {
                    let points = trendBucket == 60 ? hourlyPoints(series) : series
                    NetworkSeriesChart(points: points, bucketMinutes: trendBucket)
                } else {
                    Text("所选周期暂无分钟级流量数据").foregroundStyle(.secondary)
                }
            }.frame(maxWidth: .infinity, alignment: .leading)
        }
    }
    private var sourceRankingPanel: some View {
        Surface {
            VStack(alignment: .leading, spacing: 8) {
                Text("配置流量 TOP 10").font(.headline)
                Text("所选周期 · 按配置分组，备用线路单列 · 已观测流量").font(InterfaceType.detail).foregroundStyle(.secondary)
                if let rows = model.stats?.sourceRows {
                    let sorted = rows.filter { ($0.upload ?? 0) > 0 || ($0.download ?? 0) > 0 }.sorted {
                        let a = Decimal($0.upload ?? 0) + Decimal($0.download ?? 0)
                        let b = Decimal($1.upload ?? 0) + Decimal($1.download ?? 0)
                        return a == b ? $0.id < $1.id : a > b
                    }.prefix(10)
                    if sorted.isEmpty {
                        Text("暂无配置流量").foregroundStyle(.secondary)
                    }
                    ForEach(Array(sorted.enumerated()), id: \.offset) { index, row in
                        HStack(alignment: .top, spacing: 8) {
                            Text("\(index + 1)").font(InterfaceType.detail).foregroundStyle(.secondary).frame(width: 20, alignment: .trailing)
                            VStack(alignment: .leading, spacing: 2) {
                                Text(row.label.isEmpty ? row.id : row.label).lineLimit(1).truncationMode(.middle)
                                    .help(row.label.isEmpty ? row.id : row.label)
                                Text("上传 \(bytes(row.upload)) · 下载 \(bytes(row.download))").font(InterfaceType.detail).foregroundStyle(.secondary)
                            }.frame(maxWidth: .infinity, alignment: .leading)
                            Text(totalBytes(TargetTraffic(id: row.id, upload: row.upload, download: row.download))).monospacedDigit().fixedSize()
                        }.font(InterfaceType.body)
                        if index + 1 < sorted.count { Divider() }
                    }
                } else {
                    Text("配置流量数据不可用").foregroundStyle(.secondary)
                }
            }.frame(maxWidth: .infinity, alignment: .leading)
        }
    }
    private var windowDomainRows: [TargetTraffic] {
        targetTraffic(model.stats?.rows ?? [])
            .filter { isDNSDomain($0.id) }
            .sorted {
                let a = Decimal($0.upload ?? 0) + Decimal($0.download ?? 0)
                let b = Decimal($1.upload ?? 0) + Decimal($1.download ?? 0)
                return a == b ? $0.id < $1.id : a > b
            }
    }
    private var domainRankingPanel: some View {
        Surface {
            VStack(alignment: .leading, spacing: 8) {
                Text("域名流量 TOP 10").font(.headline)
                Text("所选周期 · 上传 + 下载降序 · 仅已观测流量").font(InterfaceType.detail).foregroundStyle(.secondary)
                if model.stats?.rows != nil {
                    let rows = Array(windowDomainRows.prefix(10))
                    if rows.isEmpty { Text("暂无域名排行数据").foregroundStyle(.secondary) }
                    ForEach(Array(rows.enumerated()), id: \.offset) { index, row in
                        HStack(alignment: .top, spacing: 8) {
                            Text("\(index + 1)").font(InterfaceType.detail).foregroundStyle(.secondary).frame(width: 20, alignment: .trailing)
                            VStack(alignment: .leading, spacing: 2) {
                                Text(row.id).lineLimit(1).truncationMode(.middle).help(row.id)
                                Text("上传 \(bytes(row.upload)) · 下载 \(bytes(row.download))").font(InterfaceType.detail).foregroundStyle(.secondary)
                            }.frame(maxWidth: .infinity, alignment: .leading)
                            Text(totalBytes(row)).monospacedDigit().fixedSize()
                        }.font(InterfaceType.body)
                        if index + 1 < rows.count { Divider() }
                    }
                    Text("仅统计已观测流量，可能漏采短连接").font(InterfaceType.detail).foregroundStyle(.secondary)
                } else {
                    Text("域名排行不可用或格式无效").foregroundStyle(.secondary)
                }
            }.frame(maxWidth: .infinity, alignment: .leading)
        }
    }
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                HStack(alignment: .top) {
                    PageHeader(title: "统计与健康度", subtitle: "流量、连接表现与节点探测", icon: "chart.bar.xaxis")
                    ViewThatFits(in: .horizontal) {
                        HStack(spacing: 10) {
                            refreshButton
                        }
                        refreshButton
                    }
                }
                LazyVGrid(columns: [GridItem(.adaptive(minimum: 165))], spacing: 12) {
                    metric("进程累计上传", bytes(model.stats?.trafficTotals?.upload), "自当前代理进程启动")
                    metric("进程累计下载", bytes(model.stats?.trafficTotals?.download), "自当前代理进程启动")
                    metric("当前上传", rateText(model.uploadRate), "代理总速率")
                    metric("当前下载", rateText(model.downloadRate), "代理总速率")
                }
                if let error = model.statsError { Text(error + (model.stats == nil ? "" : " · 显示上次读取的数据")).foregroundStyle(.orange).font(InterfaceType.body) }
                VStack(alignment: .leading, spacing: 4) {
                    Label("当前选择：\(model.statsWindow.label) · 显示数据：\(model.displayedStatsWindow?.label ?? "暂无")", systemImage: "clock")
                    if model.displayedStatsWindow != model.statsWindow { Text("正在等待所选周期数据").foregroundStyle(.orange) }
                    if let read = model.statsReadAt { Text("读取于 \(read.formatted())") }
                }.font(InterfaceType.detail).foregroundStyle(.secondary)
                healthPanel
                trafficPanel
                LazyVGrid(columns: [GridItem(.adaptive(minimum: 300), alignment: .top)], alignment: .leading, spacing: 12) {
                    sourceRankingPanel
                    nodeRankingPanel
                    domainRankingPanel
                }
                Text("数据来源：http://127.0.0.1:33335/stats?minutes=\(String(model.displayedStatsWindow?.rawValue ?? model.statsWindow.rawValue))").font(InterfaceType.detail).foregroundStyle(.secondary).textSelection(.enabled)
            }.font(InterfaceType.body).padding(24).frame(maxWidth: 1100)
        }
    }
    private var periodPicker: some View {
        Picker("统计周期", selection: $model.statsWindow) {
            ForEach(StatsWindow.allCases, id: \.self) { window in Text(window.label).tag(window) }
        }.pickerStyle(.segmented).frame(width: 230)
    }
    private var refreshButton: some View {
        Button { model.refreshIfVisible() } label: { Label("刷新", systemImage: "arrow.clockwise") }
    }
    private func metric(_ title: String, _ value: String, _ detail: String) -> some View {
        Surface {
            VStack(alignment: .leading, spacing: 6) {
                Text(title).font(InterfaceType.detail).foregroundStyle(.secondary)
                Text(value).font(.system(size: 21, weight: .semibold, design: .rounded)).monospacedDigit()
                Text(detail).font(InterfaceType.detail).foregroundStyle(.secondary)
            }
        }
    }
}

struct SettingsView: View {
    @EnvironmentObject var model: Model
    @State private var expandedSources: Set<String> = []
    @State private var addingAirport = false
    @State private var pendingDeleteSource: SourceRow?
    var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 16) {
                PageHeader(title: "设置", subtitle: "管理代理地址与探测选项", icon: "slider.horizontal.3")
                Surface {
                    VStack(alignment: .leading, spacing: 14) {
                        HStack {
                            Label("机场配置", systemImage: "server.rack").font(.headline)
                            Spacer()
                            if model.sourcesUpdateBusy {
                                ProgressView(value: Double(model.sourcesUpdatePercent ?? 0), total: 100)
                                    .frame(width: 110)
                                Text("\(model.sourcesUpdatePercent ?? 0)%")
                                    .font(InterfaceType.detail.monospacedDigit()).foregroundStyle(.secondary)
                            }
                            Button(model.sourcesUpdateBusy ? "更新中…" : "更新配置") { model.updateAllSources() }
                                .disabled(model.sourcesSaveBusy || model.sourcesUpdateBusy)
                                .help("按订阅地址重新拉取全部配置：先直连，失败后用当前节点拉取")
                            Button("添加机场") { model.cancelSubscriptionEdit(); addingAirport.toggle() }
                                .buttonStyle(.borderedProminent)
                        }
                        HStack(spacing: 10) {
                            Toggle("玖壹模块（本机）", isOn: Binding(
                                get: { model.cfgOffice91Enabled },
                                set: { model.setOffice91Module($0) }
                            )).toggleStyle(.checkbox).disabled(model.office91ModuleBusy)
                            Text(model.office91ModuleBusy ? "正在应用并重建节点池…" : "默认关闭；勾选后读取你本机提供的玖壹配置，不联网获取凭证。")
                                .font(InterfaceType.detail).foregroundStyle(.secondary)
                            Spacer()
                        }
                        if addingAirport {
                            VStack(alignment: .leading, spacing: 10) {
                                Text("添加机场").font(.headline)
                                TextField("机场名称", text: $model.newSubscriptionName)
                                SecureField("粘贴订阅链接", text: $model.newSubscriptionURL)
                                HStack {
                                    Button("取消") { addingAirport = false; model.cancelSubscriptionEdit() }
                                    Button(model.addingSubscription ? "添加中…" : "添加") { model.addSubscription() }
                                        .disabled(model.addingSubscription || model.sourcesDirty || model.newSubscriptionURL.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty)
                                }
                            }.padding(14).background(Color.primary.opacity(0.035), in: RoundedRectangle(cornerRadius: 10))
                        }
                        if let error = model.sourcesError { Text(error).foregroundStyle(.orange).font(InterfaceType.detail) }
                        if let error = model.sourcesSaveError { Text(error).foregroundStyle(.orange).font(InterfaceType.detail) }
                        if let message = model.addSubscriptionMessage { Text(message).foregroundStyle(.secondary).font(InterfaceType.detail) }
                        if model.sourceDraft.isEmpty { Text(model.sourcesLoaded ? "暂无机场配置" : "正在读取机场配置…").foregroundStyle(.secondary) }
                        ForEach(Array(model.sourceDraft.enumerated()), id: \.element.id) { index, row in
                            if row.id != "xlinker" && (row.id != "office91" || model.jiuyiEnabled) { sourceCard(index: index, row: row) }
                        }
                        HStack {
                            if model.sourcesSaveBusy { Text("正在保存并更新节点配置…").foregroundStyle(.secondary).font(InterfaceType.detail) }
                            else if model.sourcesDirty { Text("有未保存的更改").foregroundStyle(.orange).font(InterfaceType.detail) }
                            Spacer()
                            Button("撤销更改") { model.resetSourceDraft() }.disabled(!model.sourcesDirty || model.sourcesSaveBusy)
                            Button(model.sourcesSaveBusy ? "保存中…" : "保存机场配置") { model.saveSources() }.buttonStyle(.borderedProminent).disabled(!model.sourcesDirty || model.sourcesSaveBusy || model.prioritiesBusy)
                        }
                    }
                }
                Surface {
                    VStack(alignment: .leading, spacing: 14) {
                        Label("备用线路", systemImage: "arrow.triangle.branch").font(.headline)
                        VStack(alignment: .leading, spacing: 6) {
                            HStack {
                                Text("备用代理地址（按顺序尝试）").font(InterfaceType.detail).foregroundStyle(.secondary)
                                Spacer()
                                Button("添加地址") { model.cfgFallbackList.append("") }
                            }
                            ForEach(Array(model.cfgFallbackList.enumerated()), id: \.offset) { index, _ in
                                HStack(spacing: 8) {
                                    Text("\(index + 1)").font(InterfaceType.detail.monospacedDigit()).foregroundStyle(.secondary).frame(width: 18)
                                    TextField("host:port", text: Binding(
                                        get: { model.cfgFallbackList.indices.contains(index) ? model.cfgFallbackList[index] : "" },
                                        set: { if model.cfgFallbackList.indices.contains(index) { model.cfgFallbackList[index] = $0 } }
                                    )).textFieldStyle(.roundedBorder).frame(maxWidth: 260)
                                    if index == 0 { RouteBadge(text: "首选", color: .blue) }
                                    Button { if model.cfgFallbackList.indices.contains(index) { model.cfgFallbackList.remove(at: index) } } label: { Image(systemName: "minus.circle") }
                                        .buttonStyle(.plain)
                                        .disabled(model.cfgFallbackList.count <= 1)
                                        .help("删除该备用地址")
                                }
                            }
                        }
                        Text("主线路不可用时按顺序尝试这些地址；修改后需重启服务生效。已观测流量按端口统计，不受列表变化影响。").font(InterfaceType.detail).foregroundStyle(.secondary)
                        if let warning = model.fallbackAddressWarning {
                            Text(warning).font(InterfaceType.detail).foregroundStyle(.orange)
                        }
                    }
                }
                Surface {
                    VStack(alignment: .leading, spacing: 14) {
                        Label("本地代理服务", systemImage: "network").font(.headline)
                        Text("本机应用使用的代理地址；通常无需修改。更改后需重启服务。")
                            .font(InterfaceType.detail).foregroundStyle(.secondary)
                        field("HTTP 代理", $model.cfgHTTP)
                        field("SOCKS5 代理", $model.cfgSOCKS)
                        DisclosureGroup("高级地址") {
                            VStack(spacing: 10) {
                                field("内核代理", $model.cfgPrimary)
                                field("内核控制", $model.cfgController)
                            }.padding(.top, 8)
                        }

                    }
                }
                Surface {
                    VStack(alignment: .leading, spacing: 14) {
                        Label("节点选择策略", systemImage: "target").font(.headline)
                        Text("自动选择节点时的策略。稳定优先：节点健康就沿用，不追延迟；均衡：延迟比最优差超过阈值才切换；低延迟优先：贴近最快节点，差距超过阈值即切换。修改立即生效，无需重启。")
                            .font(InterfaceType.detail).foregroundStyle(.secondary)
                        HStack(spacing: 32) {
                            VStack(alignment: .leading, spacing: 6) {
                                Text("普通流量").font(.system(size: 14, weight: .medium))
                                Picker("普通流量策略", selection: $model.strategyPrimary) {
                                    Text("稳定优先").tag("stability")
                                    Text("均衡").tag("balanced")
                                    Text("低延迟优先").tag("latency")
                                }.pickerStyle(.menu).frame(width: 180)
                                .accessibilityLabel("普通流量选择策略")
                            }
                            VStack(alignment: .leading, spacing: 6) {
                                Text("AI流量").font(.system(size: 14, weight: .medium))
                                Picker("AI流量策略", selection: $model.strategyAI) {
                                    Text("稳定优先").tag("stability")
                                    Text("均衡").tag("balanced")
                                    Text("低延迟优先").tag("latency")
                                }.pickerStyle(.menu).frame(width: 180)
                                .accessibilityLabel("AI流量选择策略")
                            }
                        }
                        HStack {
                            Spacer()
                            Button("重新读取") { model.loadStrategy() }.disabled(model.strategyLoading)
                            Button("保存并立即生效") { model.saveStrategy() }
                                .keyboardShortcut(.defaultAction).buttonStyle(.borderedProminent)
                                .disabled(model.strategySaving)
                        }
                        if model.strategyLoading { Text("读取中…").font(InterfaceType.detail).foregroundStyle(.secondary) }
                        if model.strategySaving { Text("保存中…").font(InterfaceType.detail).foregroundStyle(.secondary) }
                        if let notice = model.strategyNotice { Text(notice).font(InterfaceType.detail).foregroundStyle(.green) }
                        if let error = model.strategyError { Text(error).font(InterfaceType.detail).foregroundStyle(.red).textSelection(.enabled) }
                    }
                }
                Surface {
                    VStack(alignment: .leading, spacing: 14) {
                        Label("AI 线路检测", systemImage: "waveform.path.ecg").font(.headline)
                        HStack(spacing: 14) {
                            Text("检测目标").font(InterfaceType.detail).foregroundStyle(.secondary)
                            ForEach(["openai", "claude", "gemini", "grok"], id: \.self) { t in
                                Toggle(t.capitalized, isOn: Binding(
                                    get: { model.cfgAITargets.contains(t) },
                                    set: { on in
                                        if on { if !model.cfgAITargets.contains(t) { model.cfgAITargets.append(t) } }
                                        else if model.cfgAITargets.count > 1 { model.cfgAITargets.removeAll { $0 == t } }
                                    }
                                )).toggleStyle(.checkbox)
                            }
                            Spacer()
                        }.help("多选；每个选中的服务按其官方域名独立检测，全部通过该线路才可用。至少保留一个。")
                        Text("所有线路切换都会弹出系统通知。")
                            .font(InterfaceType.detail).foregroundStyle(.secondary)
                    }
                }
                if let error = model.configError { Text(error).foregroundStyle(.red) }
                if let message = model.message { Text(message).foregroundStyle(.secondary) }
                HStack { Spacer(); Button("重新读取") { model.loadConfig() }; Button("保存设置") { model.saveConfig() }.keyboardShortcut(.defaultAction).buttonStyle(.borderedProminent) }
            }.font(InterfaceType.body).padding(24).frame(maxWidth: 1000)
        }
        .onAppear { if !model.strategyLoaded { model.loadStrategy() } }
        .alert("删除机场配置？", isPresented: Binding(
            get: { pendingDeleteSource != nil },
            set: { if !$0 { pendingDeleteSource = nil } }
        ), presenting: pendingDeleteSource) { row in
            Button("删除", role: .destructive) {
                model.deleteSource(row)
                pendingDeleteSource = nil
            }
            Button("取消", role: .cancel) { pendingDeleteSource = nil }
        } message: { row in
            Text("将移除「\(row.label.isEmpty ? row.id : row.label)」及其节点缓存，不影响 FlClash 原配置。")
        }
    }
    func field(_ title: String, _ value: Binding<String>) -> some View {
        HStack(spacing: 12) {
            Text(title).font(InterfaceType.body).foregroundStyle(.secondary).frame(width: 90, alignment: .leading)
            TextField(title, text: value).textFieldStyle(.roundedBorder)
                .font(.system(.callout, design: .monospaced))
                .accessibilityLabel(title)
        }
    }
    func sourceCard(index: Int, row: SourceRow) -> some View {
        let name = row.label.isEmpty ? row.id : row.label
        let allowance = model.subscriptions.first { $0.sourceID == row.id }
        let expanded = expandedSources.contains(row.id)
        return VStack(alignment: .leading, spacing: 0) {
            // Whole header is the disclosure target; the explicit 展开/收起
            // label on the right replaces the tiny built-in chevron.
            Button {
                if expanded {
                    expandedSources.remove(row.id)
                } else {
                    expandedSources.insert(row.id)
                    model.loadSubscriptionURL(row.id)
                }
            } label: {
                HStack(alignment: .top, spacing: 10) {
                    VStack(alignment: .leading, spacing: 6) {
                        Text(name).font(.system(size: 15, weight: .semibold))
                        Text("\(row.nodeCount.map(String.init) ?? "—") 个节点 · \(sourceStatusText(row.status, lastError: row.lastError))")
                            .font(InterfaceType.detail).foregroundStyle(.secondary)
                        if let allowance {
                            let fraction: Double? = {
                                guard let used = allowance.used, let total = allowance.total, total > 0 else { return nil }
                                return min(1, Double(used) / Double(total))
                            }()
                            UsageBar(title: "流量", fraction: fraction,
                                     detail: "\(bytes(allowance.used)) / \(bytes(allowance.total)) · 余 \(bytes(allowance.remaining))",
                                     tint: (fraction ?? 0) > 0.9 ? .red : .green)
                            HStack(spacing: 8) {
                                Text(allowance.expiresAt.map { "到期 \($0.formatted(date: .numeric, time: .omitted))" } ?? "到期时间未知")
                                if let days = allowance.expiresAt.map({ Int(($0.timeIntervalSinceNow / 86400).rounded()) }) {
                                    Text(days >= 0 ? "剩 \(days) 天" : "已过期")
                                        .foregroundStyle(days < 3 ? Color.red : (days < 14 ? Color.orange : Color.secondary))
                                }
                            }.font(InterfaceType.detail).foregroundStyle(.secondary)
                        } else {
                            Text("用量与到期未知").font(InterfaceType.detail).foregroundStyle(.secondary)
                        }
                    }
                    Spacer(minLength: 8)
                    VStack(alignment: .trailing, spacing: 6) {
                        if !row.enabled { RouteBadge(text: "已停用", color: .secondary) }
                        HStack(spacing: 4) {
                            Text(expanded ? "收起" : "展开")
                            Image(systemName: expanded ? "chevron.up" : "chevron.down")
                        }.font(InterfaceType.detail).foregroundStyle(.secondary)
                    }
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(.plain)
            if expanded {
                VStack(alignment: .leading, spacing: 12) {
                    if row.type != "vpn" {
                        VStack(alignment: .leading, spacing: 5) {
                            Text("订阅地址").font(InterfaceType.detail).foregroundStyle(.secondary)
                            HStack(spacing: 8) {
                                TextField("https://…", text: Binding(
                                    get: { model.subscriptionURLDrafts[row.id] ?? "" },
                                    set: { model.subscriptionURLDrafts[row.id] = $0 }
                                )).textFieldStyle(.roundedBorder).font(.system(size: 11, design: .monospaced))
                                Button("保存并更新") { model.saveSubscriptionURL(row.id) }
                                    .disabled(model.subscriptionURLBusy.contains(row.id) || model.sourcesDirty)
                            }
                            Text("保存在 ClashHub 自己的配置里，不改 FlClash；留空表示沿用 FlClash 中的地址。")
                                .font(InterfaceType.detail).foregroundStyle(.secondary)
                        }
                        if let notice = model.subscriptionURLNotice { Text(notice).font(InterfaceType.detail).foregroundStyle(.secondary) }
                        if let error = model.subscriptionURLError { Text(error).font(InterfaceType.detail).foregroundStyle(.orange) }
                    }
                    if row.canEditSubscription == true {
                        if model.editingSubscriptionID == row.id {
                            TextField("机场名称", text: $model.newSubscriptionName)
                            SecureField("新订阅链接（留空保持不变）", text: $model.newSubscriptionURL)
                            HStack {
                                Button("取消") { model.cancelSubscriptionEdit() }
                                Button("保存名称和链接") { model.addSubscription() }.disabled(model.addingSubscription)
                            }
                        } else {
                            HStack {
                                Button("编辑名称和链接") { model.editSubscription(row) }.disabled(model.addingSubscription || model.sourcesDirty)
                                Button("更新订阅") { model.refreshSubscription(row.id) }.disabled(model.addingSubscription || model.sourcesDirty || !row.enabled)
                                if row.id.hasPrefix("custom-sub-") {
                                    Button(model.deletingSourceID == row.id ? "删除中…" : "删除", role: .destructive) {
                                        pendingDeleteSource = row
                                    }.disabled(model.addingSubscription || model.sourcesDirty || model.deletingSourceID != nil)
                                }
                            }
                        }
                    } else if row.type == "vpn" {
                        Text("VPN 型来源，无订阅地址").font(InterfaceType.detail).foregroundStyle(.secondary)
                    }
                    HStack {
                        Text("节点优先级")
                        Picker("节点优先级", selection: Binding(
                            get: { model.sourcePolicyPriorities[row.id] ?? "medium" },
                            set: { model.setSourcePriority(id: row.id, value: $0) }
                        )) {
                            Text("最高").tag("highest"); Text("高").tag("high"); Text("中").tag("medium")
                            Text("低").tag("low"); Text("最低").tag("lowest")
                        }.labelsHidden().frame(width: 120).disabled(model.sourcesSaveBusy || model.sourcePolicies[row.id] == nil)
                        Spacer()
                        Toggle("启用", isOn: Binding(
                            get: { model.sourceDraft.indices.contains(index) ? model.sourceDraft[index].enabled : false },
                            set: { model.setSourceEnabled(at: index, enabled: $0) }
                        )).toggleStyle(.switch).disabled(model.sourcesSaveBusy)
                    }
                    if row.includeKeywords != nil && row.excludeKeywords != nil { VStack(alignment: .leading, spacing: 6) {
                        Text("节点筛选").font(.system(size: 14, weight: .medium))
                        TextField("包含关键词（留空表示全部，逗号分隔）", text: Binding(get: { model.includeDraft[row.id] ?? "" }, set: { model.setFilter(id: row.id, value: $0, include: true) }))
                        TextField("排除关键词（逗号分隔）", text: Binding(get: { model.excludeDraft[row.id] ?? "" }, set: { model.setFilter(id: row.id, value: $0, include: false) }))
                        Text("包含任一关键词即可；排除优先，不区分大小写。").font(InterfaceType.detail).foregroundStyle(.secondary)
                    } }
                    HStack {
                        Text("同优先级时按顺序选择").font(InterfaceType.detail).foregroundStyle(.secondary)
                        Spacer()
                        Button("上移") { model.moveSource(at: index, offset: -1) }.disabled(index == 0 || model.sourcesSaveBusy)
                        Button("下移") { model.moveSource(at: index, offset: 1) }.disabled(index == model.sourceDraft.count - 1 || model.sourcesSaveBusy)
                    }
                }.padding(.top, 12)
            }
        }.padding(14).background(Color.primary.opacity(0.035), in: RoundedRectangle(cornerRadius: 10))
    }

}
@MainActor final class AppDelegate: NSObject, NSApplicationDelegate {
    let model = Model()
    var reopenWindow: (() -> Void)?
    func applicationDidFinishLaunching(_ notification: Notification) {
        NSApp.activate(ignoringOtherApps: true)
    }
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if !flag { reopenWindow?() }
        return true
    }
}
struct SidebarView: View, Equatable {
    let selected: Int
    let select: (Int) -> Void
    static func == (lhs: SidebarView, rhs: SidebarView) -> Bool { lhs.selected == rhs.selected }
    private let pages = [("节点", "network"), ("统计", "chart.bar.xaxis"), ("域名", "globe"), ("设置", "gearshape")]
    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 10) {
                Image(systemName: "network").font(.title2).foregroundStyle(.blue)
                VStack(alignment: .leading) { Text("ClashHub").font(.title3.bold()); Text("网络控制台").font(InterfaceType.detail).foregroundStyle(.secondary) }
            }.padding(.horizontal, 12).padding(.top, 28).padding(.bottom, 24)
            ForEach(0..<pages.count, id: \.self) { index in
                Button { select(index) } label: {
                    Label(pages[index].0, systemImage: pages[index].1)
                        .font(.callout.weight(selected == index ? .semibold : .regular))
                        .frame(maxWidth: .infinity, alignment: .leading).padding(10)
                        .background(selected == index ? Color.blue.opacity(0.13) : Color.clear, in: RoundedRectangle(cornerRadius: 8))
                        .foregroundStyle(selected == index ? Color.blue : Color.primary)
                }.buttonStyle(.plain)
                .accessibilityAddTraits(selected == index ? [.isSelected] : [])
            }
            Spacer()
            Text("本地代理监控").font(InterfaceType.detail).foregroundStyle(.secondary).padding(10)
        }.padding(12).frame(width: 180).frame(maxHeight: .infinity)
            .background(Color(nsColor: .windowBackgroundColor))
    }
}
struct MainView: View {
    @EnvironmentObject var model: Model
    var body: some View {
        HStack(spacing: 0) {
            SidebarView(selected: model.selectedPage) { model.selectedPage = $0 }.equatable()
            Divider()
            Group {
                switch model.selectedPage {
                case 1: StatsView()
                case ClashHubDomainViewIndex: DomainView()
                case 3: SettingsView()
                default: NodesView()
                }
            }.frame(maxWidth: .infinity, maxHeight: .infinity)
                .background(Color(nsColor: .underPageBackgroundColor).opacity(0.45))
        }
        .onChange(of: model.selectedPage) { _ in model.refreshIfVisible() }
        .frame(minWidth: 650, minHeight: 480)
        .background(WindowTracker(model: model).frame(width: 0, height: 0))
        .onReceive(NotificationCenter.default.publisher(for: NSWindow.didBecomeKeyNotification)) { note in
            if let window = note.object as? NSWindow, window === model.window { model.refreshIfVisible() }
        }
        .onReceive(NotificationCenter.default.publisher(for: NSWindow.didDeminiaturizeNotification)) { note in
            if let window = note.object as? NSWindow, window === model.window { model.refreshIfVisible() }
        }
    }
}
func modeLabel(_ mode: String?) -> String {
    switch mode { case "rule": return "规则"; case "global": return "全局"; case "direct": return "直连"; default: return "未知" }
}
struct MenuPanel: View {
    @EnvironmentObject var model: Model
    let openMain: () -> Void
    var body: some View {
        VStack(alignment: .leading, spacing: 11) {
            HStack {
                Image(systemName: "network").font(.title2).foregroundStyle(.blue)
                VStack(alignment: .leading, spacing: 3) {
                    Text("ClashHub").font(.headline)
                    Text(model.proxyStatus).font(InterfaceType.detail).foregroundStyle(.secondary)
                }
                Spacer()
                RouteBadge(text: "线路" + model.currentRouteQuality, color: qualityColor)
            }
            VStack(spacing: 10) {
                route("普通流量", node: model.selectedNode("主代理"), color: .blue)
                Divider()
                route("AI流量", node: model.selectedNode("AI主代理", ai: true), color: .purple)
            }.padding(12).background(Color.primary.opacity(0.035), in: RoundedRectangle(cornerRadius: 10))
            HStack {
                Text("系统代理").font(.system(size: 14, weight: .medium))
                Spacer()
                if model.state?.active == true {
                    Button { model.runCLI("sysproxy") } label: { Label("修复系统代理", systemImage: "wrench.and.screwdriver") }
                        .disabled(model.operationBusy)
                        .help("重新把系统 HTTP/HTTPS/SOCKS 代理指向 ClashHub；其他客户端或半途失败后可用")
                    Button { model.runCLI("off") } label: { Label("停用", systemImage: "stop.circle") }.disabled(model.operationBusy)
                } else {
                    Button { model.runCLI("on") } label: { Label("启用", systemImage: "play.circle") }.tint(.green).disabled(model.operationBusy)
                }
            }
            if model.operationBusy { notice("操作进行中…") }
            if let error = model.operationError { notice(error, warning: true) }
            Divider()
            HStack {
                Text("路由模式").font(.system(size: 14, weight: .medium))
                Spacer()
                Text(modeLabel(model.routingMode?.actualMode)).font(InterfaceType.detail).foregroundStyle(.secondary)
            }
            HStack(spacing: 6) {
                ForEach(["rule", "global", "direct"], id: \.self) { mode in
                    Button { model.setRoutingMode(mode) } label: {
                        Text(modeLabel(mode)).frame(maxWidth: .infinity).padding(.vertical, 7)
                            .background(model.routingMode?.actualMode == mode ? Color.accentColor.opacity(0.15) : Color.primary.opacity(0.04), in: RoundedRectangle(cornerRadius: 7))
                    }.buttonStyle(.plain)
                        .accessibilityValue(model.routingMode?.actualMode == mode ? "当前模式" : "未选择")
                        .disabled(model.modeBusy || model.readingModeForMenu || model.routingMode == nil)
                }
            }
            if model.routingMode == nil { notice("模式不可用，请检查服务是否运行") }
            if let desired = model.routingMode?.desiredMode, desired != model.routingMode?.actualMode { notice("期望：" + modeLabel(desired), warning: true) }
            if let warning = model.routingMode?.warning, !warning.isEmpty { notice(warning, warning: true) }
            if let error = model.modeError { notice(error, warning: true) }
            VStack(alignment: .leading, spacing: 3) {
                Text("代理总速率 ↑" + rateText(model.uploadRate) + " ↓" + rateText(model.downloadRate)).monospacedDigit()
                Text("分线路已观测速率可能漏采短连接")
            }.font(InterfaceType.detail).foregroundStyle(.secondary)
            Divider()
            HStack {
                Button("打开 ClashHub", action: openMain)
                Button { model.refreshMenu() } label: { Image(systemName: "arrow.clockwise") }
                    .help("刷新状态").accessibilityLabel("刷新状态")
                Spacer()
                Button("退出") { model.confirmQuit() }.disabled(model.operationBusy)
            }
        }.padding(16).frame(width: 340)
    }
    private var qualityColor: Color {
        switch model.currentRouteQuality { case "通畅": return .green; case "较慢": return .orange; case "不通": return .red; default: return .secondary }
    }
    private func notice(_ text: String, warning: Bool = false) -> some View {
        Text(text).font(InterfaceType.detail).foregroundStyle(warning ? Color.orange : Color.secondary).textSelection(.enabled).fixedSize(horizontal: false, vertical: true)
    }
    private func route(_ title: String, node: String?, color: Color) -> some View {
        VStack(alignment: .leading, spacing: 5) {
            HStack {
                Text(title).font(InterfaceType.label).foregroundStyle(color)
                Spacer()
                Text(model.routeRateText(title == "普通流量" ? "normal" : "ai")).font(InterfaceType.detail.monospacedDigit()).foregroundStyle(.secondary)
            }
            Text(node.map(model.displayName) ?? "未知").font(.system(size: 14, weight: .medium)).lineLimit(2).help(node ?? "未知")
        }
    }
}
func compactRate(_ rate: Double) -> String {
    if rate >= 1_000_000 { return String(format: "%.1fM", rate / 1_000_000) }
    if rate >= 1000 { return String(format: "%.0fK", rate / 1000) }
    return String(format: "%.0fB", rate)
}
func rateText(_ rate: Double?) -> String {
    guard let rate, rate.isFinite, rate >= 0 else { return "—" }
    if rate >= 1_000_000 { return String(format: "%.1f MB/s", rate / 1_000_000) }
    if rate >= 1000 { return String(format: "%.1f KB/s", rate / 1000) }
    return String(format: "%.0f B/s", rate)
}
struct MenuStatusLabel: View {
    @EnvironmentObject var model: Model
    var body: some View {
        HStack(spacing: 4) {
            Image(systemName: "circle.fill")
                .foregroundStyle(model.currentRouteQuality == "通畅" ? Color.green : (model.currentRouteQuality == "较慢" ? Color.orange : (model.currentRouteQuality == "不通" ? Color.red : Color.gray)))
                .accessibilityLabel("线路\(model.currentRouteQuality)")
            Text(model.globalRateCompact())
        }
        .help("线路\(model.currentRouteQuality)；上行/下行：普通流量与 AI 流量合计的已观测速率，同一节点只算一次，可能漏采短连接。")
    }
}
@main struct SrhopBarApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    @Environment(\.openWindow) private var openWindow
    var body: some Scene {
        Window("ClashHub", id: "main") {
            MainView().environmentObject(appDelegate.model)
                .onAppear { appDelegate.reopenWindow = { openWindow(id: "main"); NSApp.activate(ignoringOtherApps: true) } }
        }
        MenuBarExtra {
            MenuPanel {
                openWindow(id: "main"); NSApp.activate(ignoringOtherApps: true)
            }.environmentObject(appDelegate.model)
        } label: {
            MenuStatusLabel().environmentObject(appDelegate.model)
        }.menuBarExtraStyle(.window)
    }
}
