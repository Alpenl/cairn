import SwiftUI
import CryptoKit
import Foundation

private func sameLinkID(_ left: String, _ right: String) -> Bool {
    guard let leftUUID = UUID(uuidString: left), let rightUUID = UUID(uuidString: right) else { return false }
    return leftUUID == rightUUID
}

protocol IdentityMigrationQueuePersisting: AnyObject {
    func activeSessionSnapshot() throws -> ActiveSessionSnapshot?
    func list(identity: QueueIdentity?) throws -> [QueueEntry]
    func entry(id: String) throws -> QueueEntry?
    func hasMigratableRecent(from source: ActivationIdentity, to target: ActivationIdentity) throws -> Bool
    func recentMigrationCandidates(to target: ActivationIdentity) throws -> [RecentMigrationCandidate]
    func migrateIdentity(id: String, from source: ActivationIdentity, to target: ActivationIdentity, now: Date) throws -> Bool
    func migrateRecent(from source: ActivationIdentity, to target: ActivationIdentity) throws -> Bool
    func withActiveTargetFence(_ target: ActivationIdentity, perform: () throws -> Bool) throws -> Bool
}

extension AppGroupQueueRepository: IdentityMigrationQueuePersisting {}

protocol IdentityMigrationTodoPersisting: AnyObject {
    func migrationCandidate(
        identity sourceIdentity: QueueIdentity,
        revision sourceRevision: Int64,
        to target: ActivationIdentity
    ) throws -> CompanionTodoMigrationCandidate?
    func migratePartition(
        identity sourceIdentity: QueueIdentity,
        fromRevision sourceRevision: Int64,
        to target: ActivationIdentity,
        now: Date
    ) throws -> Bool
}

extension CompanionTodoRepository: IdentityMigrationTodoPersisting {}

struct IdentityMigrationPlan: Equatable {
    let source: ActivationIdentity
    let target: ActivationIdentity
    let queueEntryIDs: [String]
    let includesRecent: Bool
    let todo: CompanionTodoMigrationCandidate?
    let todoReadFailed: Bool

    var hasWork: Bool { !queueEntryIDs.isEmpty || includesRecent || todo != nil }
}

enum IdentityMigrationStepResult: Equatable {
    case notPresent
    case migrated
    case failed
}

struct IdentityMigrationResult: Equatable {
    let plan: IdentityMigrationPlan
    let migratedQueueEntries: Int
    let recent: IdentityMigrationStepResult
    let todo: IdentityMigrationStepResult
    let targetChanged: Bool

    var isComplete: Bool {
        !targetChanged &&
            migratedQueueEntries == plan.queueEntryIDs.count &&
            recent != .failed &&
            todo != .failed
    }
}

/// Cross-store atomicity is unavailable between SQLite and the encrypted Todo
/// file. Each component is therefore an idempotent step, and the plan remains
/// retryable until every step has committed under the exact confirmed target.
final class IdentityMigrationOrchestrator {
    private let queue: IdentityMigrationQueuePersisting
    private let todo: IdentityMigrationTodoPersisting?

    init(queue: IdentityMigrationQueuePersisting, todo: IdentityMigrationTodoPersisting?) {
        self.queue = queue
        self.todo = todo
    }

    func prepare(source: ActivationIdentity) throws -> IdentityMigrationPlan? {
        guard let target = try queue.activeSessionSnapshot(),
              source.revision >= 0,
              source.queueIdentity == target.queueIdentity,
              source != target else { return nil }
        let queueEntryIDs = try queue.list(identity: nil).filter {
            $0.identity == source.queueIdentity && $0.identityRevision == source.revision
        }.map(\.id)
        let includesRecent = try queue.hasMigratableRecent(from: source, to: target)
        let todoCandidate: CompanionTodoMigrationCandidate?
        var todoReadFailed = false
        if let todo {
            do {
                todoCandidate = try todo.migrationCandidate(
                    identity: source.queueIdentity,
                    revision: source.revision,
                    to: target
                )
            } catch {
                todoCandidate = nil
                todoReadFailed = true
            }
        } else {
            todoCandidate = nil
            todoReadFailed = true
        }
        let plan = IdentityMigrationPlan(
            source: source,
            target: target,
            queueEntryIDs: queueEntryIDs,
            includesRecent: includesRecent,
            todo: todoCandidate,
            todoReadFailed: todoReadFailed
        )
        return plan.hasWork ? plan : nil
    }

    func execute(
        _ plan: IdentityMigrationPlan,
        now: Date = Date()
    ) -> IdentityMigrationResult {
        let targetMatches: Bool
        do {
            if let active = try queue.activeSessionSnapshot() {
                targetMatches = active == plan.target
            } else {
                targetMatches = false
            }
        } catch {
            targetMatches = false
        }
        guard targetMatches else {
            return IdentityMigrationResult(
                plan: plan,
                migratedQueueEntries: 0,
                recent: plan.includesRecent ? .failed : .notPresent,
                todo: plan.todo == nil ? .notPresent : .failed,
                targetChanged: true
            )
        }

        let todoResult: IdentityMigrationStepResult
        if plan.todoReadFailed {
            todoResult = .failed
        } else if let candidate = plan.todo, let todo {
            todoResult = ((try? queue.withActiveTargetFence(plan.target) {
                try todo.migratePartition(
                    identity: candidate.identity,
                    fromRevision: candidate.revision,
                    to: plan.target,
                    now: now
                )
            }) == true) ? .migrated : .failed
        } else {
            todoResult = .notPresent
        }

        let recentResult: IdentityMigrationStepResult
        if plan.includesRecent {
            recentResult = ((try? queue.migrateRecent(from: plan.source, to: plan.target)) == true)
                ? .migrated
                : .failed
        } else {
            recentResult = .notPresent
        }

        var migratedQueueEntries = 0
        for id in plan.queueEntryIDs {
            if (try? queue.migrateIdentity(
                id: id,
                from: plan.source,
                to: plan.target,
                now: now
            )) == true {
                migratedQueueEntries += 1
            }
        }
        let targetChanged = (try? queue.activeSessionSnapshot()) != plan.target
        return IdentityMigrationResult(
            plan: plan,
            migratedQueueEntries: migratedQueueEntries,
            recent: recentResult,
            todo: todoResult,
            targetChanged: targetChanged
        )
    }
}

@MainActor
final class WebTagSettingsModel: ObservableObject {
    @Published var origin = ""
    @Published var installationToken = ""
    @Published var tokenVisible = false
    @Published var status = ""
    @Published var isBusy = false
    @Published var showClearConfirmation = false
    @Published var showPermanentRetryConfirmation = false
    @Published var showIdentityMigrationConfirmation = false
    @Published var refreshBusy = false
    @Published private(set) var todoMigrationCandidates: [CompanionTodoMigrationCandidate] = []
    @Published private(set) var recentMigrationCandidates: [RecentMigrationCandidate] = []
    @Published private(set) var migrationRecoveryText = ""

    /// Everything the queue and recent sections render, always from one durable
    /// read. `origin` and `installationToken` are deliberately outside it: a lifecycle reload
    /// replaces this value wholesale, and an unsaved credential draft must survive
    /// that, so it is unrepresentable here rather than merely carefully avoided.
    @Published private(set) var projection: SettingsProjection = .empty
    /// Recomputed from `projection.recent` and the clock, and re-evaluated by the
    /// cooldown timer at the exact deadline so the button unlocks on its own.
    @Published private(set) var refreshGate: SettingsRecentRefreshGate = .unavailable

    var queue: [QueueEntry] { projection.queue.groups.flatMap(\.rows) }
    var recent: RecentResult? { projection.recent }

    private let repository: AppGroupQueueRepository?
    private let coordinator: ShareSubmissionCoordinator?
    private let keychain = KeychainCredentialStore()
    private let api = WebTagAPIClient()
    private let todoRepository: CompanionTodoRepository?
    private let migrationOrchestrator: IdentityMigrationOrchestrator?
    private var migrationTarget: ActivationIdentity?
    private var pendingIdentityMigration: IdentityMigrationPlan?
    private let loader: SettingsSnapshotLoader?
    private let clock: SettingsWallClock
    private let cooldownTimer: SettingsCooldownTimer

    /// `repository` is injectable so a test can drive a real repository in a
    /// temporary directory. Without it the only way in is the app group container,
    /// which a unit test host does not reliably have.
    init(
        clock: SettingsWallClock = SystemSettingsWallClock(),
        repository injected: AppGroupQueueRepository? = nil,
        todoRepository injectedTodos: CompanionTodoRepository? = nil
    ) {
        let resolved = injected ?? (try? AppGroupQueueRepository())
        let resolvedTodos = injectedTodos ?? (try? CompanionTodoRepository())
        repository = resolved
        todoRepository = resolvedTodos
        migrationOrchestrator = resolved.map { IdentityMigrationOrchestrator(queue: $0, todo: resolvedTodos) }
        coordinator = resolved.map {
            ShareSubmissionCoordinator(repository: $0, wakeScheduler: DurableQueueWakeScheduler.shared)
        }
        loader = resolved.map { SettingsSnapshotLoader(repository: $0) }
        self.clock = clock
        cooldownTimer = SettingsCooldownTimer(clock: clock)
        if let repository {
            do {
                if let stored = try keychain.loadConfig() {
                    let active = try repository.activeSessionSnapshot()
                    origin = stored.identity.origin
                    installationToken = stored.installationToken
                    status = active == stored.activation && stored.identity.isSupported
                        ? "连接正常"
                        : "身份已变更"
                }
            } catch {
                status = ""
            }
        }
        reload()
    }

    var activeIdentity: QueueIdentity? {
        guard let repository else { return nil }
        do { return try repository.activeIdentity() } catch { return nil }
    }

    /// One identity-fenced read, committed only if it is still the current one.
    ///
    /// A snapshot that lost the race — taken under an identity that has since been
    /// replaced, or overtaken by a newer read — is dropped rather than shown. The
    /// old failure mode was the opposite: whichever read finished last won, so a
    /// slow load from the previous identity could repaint the screen with its data.
    func reload() {
        guard let loader, let projection = loader.loadProjection() else {
            migrationTarget = nil
            todoMigrationCandidates = []
            recentMigrationCandidates = []
            return
        }
        apply(projection)
        reloadMigrationSources()
    }

    /// The foreground contract: read once on becoming active, let the existing
    /// drain/reconcile run, then read once more so anything it changed is on
    /// screen. Two reads exactly — no polling loop, no retry.
    func onForegroundActive() {
        reload()
        guard let coordinator else { return }
        Task {
            await coordinator.reconcileAndDrain()
            reload()
        }
    }

    /// Drops any armed cooldown timer. The screen going away must not leave a
    /// callback holding this model alive to update something nobody is looking at.
    func dispose() {
        cooldownTimer.invalidate()
    }

    private func apply(_ projection: SettingsProjection) {
        self.projection = projection
        refreshGate = SettingsRefreshGatePolicy.evaluate(recent: projection.recent, now: clock.now)
        // Re-arming on every projection means identity switches and recent
        // replacements invalidate the previous deadline for free: `arm` bumps the
        // generation, so the timer that was in flight for the old row is ignored.
        // The timer callback is not actor-isolated, so the hop back to the main
        // actor is explicit. The work itself is view state only: the deadline says
        // nothing new about the database, so recomputing it reads and writes none.
        cooldownTimer.arm(until: refreshGate.cooldownUntil) { [weak self] in
            Task { @MainActor in
                guard let self else { return }
                self.refreshGate = SettingsRefreshGatePolicy.evaluate(recent: self.projection.recent, now: self.clock.now)
            }
        }
    }

    func saveAndTest() {
        guard !isBusy else { return }
        var previousConfiguration: CredentialConfig?
        var previousActivation: ActivationIdentity?
        if let repository {
            previousConfiguration = try? keychain.loadConfig()
            previousActivation = try? repository.activeSessionSnapshot()
        }
        var activated = false
        isBusy = true
        status = ""
        Task {
            do {
                let normalized = try OriginNormalizer.normalize(origin)
                let result = await api.validateSession(origin: normalized, installationToken: installationToken)
                switch result {
                    case .success(let identity):
                    if !identity.isSupported {
                        status = "服务器版本不受支持"
                    } else {
                        guard let repository else { throw CoreError.database }
                        let newActivation = try repository.activate(session: identity)
                        let newConfiguration = CredentialConfig(activation: newActivation, installationToken: installationToken)
                        do {
                            try keychain.save(config: newConfiguration)
                        } catch {
                            try? repository.restoreActivation(previousActivation)
                            if let previousConfiguration {
                                try? keychain.save(config: previousConfiguration)
                            } else {
                                keychain.clear()
                            }
                            throw error
                        }
                        activated = true
                        _ = try? repository.retryIdentityBlocked(identity: QueueIdentity(origin: identity.origin, namespace: identity.namespace))
                        origin = normalized
                        status = "连接正常"
                    }
                case .failure(let failure):
                    status = statusText(for: failure.category)
                }
            } catch CoreError.invalidOrigin {
                status = "无法连接服务器"
            } catch {
                status = "无法连接服务器"
            }
            if !activated, let previousConfiguration {
                origin = previousConfiguration.identity.origin
                installationToken = previousConfiguration.installationToken
            } else if !activated {
                origin = ""
                installationToken = ""
            }
            isBusy = false
            reload()
        }
    }

    func retry(_ entry: QueueEntry) {
        if entry.state == .failedPermanent {
            pendingRetryID = entry.id
            showPermanentRetryConfirmation = true
            return
        }
        guard [.retryWait, .blockedAuth, .expired].contains(entry.state) else { return }
        performRetry(id: entry.id)
    }

    func confirmPermanentRetry() {
        guard let pendingRetryID else { return }
        self.pendingRetryID = nil
        performRetry(id: pendingRetryID)
    }

    var identityMigrationMessage: String {
        guard let plan = pendingIdentityMigration else {
            return "请先完成新的身份配置。"
        }
        let sourceRevision = plan.source.revision == 0 ? "旧格式" : "R\(plan.source.revision)"
        let recentCount = plan.includesRecent ? 1 : 0
        let todoItems = plan.todo?.itemCount ?? 0
        let todoOperations = plan.todo?.operationCount ?? 0
        let recentEffect = plan.includesRecent ? "旧最近结果会替换当前最近结果。\n" : ""
        let todoEffect = plan.todoReadFailed ? "待办当前无法读取，将与其他数据独立重试。\n" : ""
        return "\(sourceRevision) -> R\(plan.target.revision)\n" +
            "队列 \(plan.queueEntryIDs.count) 条，最近结果 \(recentCount) 条，待办 \(todoItems) 项 / \(todoOperations) 个操作。\n" +
            recentEffect +
            todoEffect +
            "迁入操作会生成新的幂等身份；取消不会修改数据或发起网络请求。"
    }

    func requestIdentityMigration(_ entry: QueueEntry) {
        guard canMigrate(entry) else { return }
        requestIdentityMigration(identity: entry.identity, revision: entry.identityRevision)
    }

    func requestIdentityMigration(_ candidate: CompanionTodoMigrationCandidate) {
        guard let target = migrationTarget,
              candidate.identity == target.queueIdentity,
              candidate.revision != target.revision else { return }
        requestIdentityMigration(identity: candidate.identity, revision: candidate.revision)
    }

    func requestIdentityMigration(_ candidate: RecentMigrationCandidate) {
        guard let target = migrationTarget,
              candidate.identity == target.queueIdentity,
              candidate.revision != target.revision else { return }
        requestIdentityMigration(identity: candidate.identity, revision: candidate.revision)
    }

    func canMigrate(_ entry: QueueEntry) -> Bool {
        guard let target = migrationTarget else { return false }
        return entry.state == .blockedIdentity &&
            entry.identity == target.queueIdentity &&
            entry.identityRevision != target.revision
    }

    func confirmIdentityMigration() {
        guard let plan = pendingIdentityMigration else { return }
        showIdentityMigrationConfirmation = false
        performIdentityMigration(plan)
    }

    func cancelIdentityMigration() {
        pendingIdentityMigration = nil
        showIdentityMigrationConfirmation = false
        migrationRecoveryText = ""
    }

    var canRetryIdentityMigration: Bool {
        !migrationRecoveryText.isEmpty && pendingIdentityMigration != nil
    }

    func retryIdentityMigration() {
        guard let plan = pendingIdentityMigration else { return }
        performIdentityMigration(plan)
    }

    private var pendingRetryID: String?

    private func requestIdentityMigration(identity: QueueIdentity, revision: Int64) {
        guard let target = migrationTarget,
              let migrationOrchestrator,
              identity == target.queueIdentity,
              revision >= 0,
              revision != target.revision else { return }
        do {
            let source = ActivationIdentity(identity: target.identity, revision: revision)
            guard let plan = try migrationOrchestrator.prepare(source: source) else {
                status = "没有可迁移的旧版本数据"
                return
            }
            pendingIdentityMigration = plan
            migrationRecoveryText = ""
            showIdentityMigrationConfirmation = true
        } catch {
            status = "无法读取旧版本数据"
        }
    }

    private func performIdentityMigration(_ plan: IdentityMigrationPlan) {
        guard let migrationOrchestrator else { return }
        let result = migrationOrchestrator.execute(plan)
        if result.targetChanged {
            pendingIdentityMigration = nil
            migrationRecoveryText = ""
            status = "身份已再次变更，请重新选择旧版本"
        } else if result.isComplete {
            pendingIdentityMigration = nil
            migrationRecoveryText = ""
            status = "旧版本数据已迁移，等待同步"
        } else {
            pendingIdentityMigration = plan
            let queueText = "队列 \(result.migratedQueueEntries)/\(plan.queueEntryIDs.count)"
            let recentText = result.recent == .failed ? "最近结果待重试" : "最近结果已处理"
            let todoText = result.todo == .failed ? "待办待重试" : "待办已处理"
            migrationRecoveryText = "\(queueText)，\(recentText)，\(todoText)"
            status = "部分迁移未完成"
        }
        reload()
        if result.isComplete { drain() }
    }

    private func reloadMigrationSources() {
        guard let repository else {
            migrationTarget = nil
            todoMigrationCandidates = []
            recentMigrationCandidates = []
            return
        }
        do {
            guard let active = try repository.activeSessionSnapshot(),
                  let stored = try keychain.loadConfig(),
                  stored.activation == active else {
                migrationTarget = nil
                todoMigrationCandidates = []
                recentMigrationCandidates = []
                return
            }
            migrationTarget = active
            recentMigrationCandidates = try repository.recentMigrationCandidates(to: active)
            if let todoRepository {
                todoMigrationCandidates = (try? todoRepository.migrationCandidates(to: active)) ?? []
            } else {
                todoMigrationCandidates = []
            }
        } catch {
            migrationTarget = nil
            todoMigrationCandidates = []
            recentMigrationCandidates = []
        }
    }

    private func performRetry(id: String) {
        guard let repository else { return }
        BackgroundUploadSessionController.shared.cancelAll(queueID: id)
        try? repository.resetForRetry(id: id)
        reload()
        drain()
    }

    func delete(_ entry: QueueEntry) {
        guard let repository else { return }
        BackgroundUploadSessionController.shared.cancelAll(queueID: entry.id)
        try? repository.delete(id: entry.id)
        reload()
        drain()
    }

    func retryRecoverable() {
        queue.filter { [.retryWait, .blockedAuth, .expired].contains($0.state) }
            .forEach { try? repository?.resetForRetry(id: $0.id) }
        reload()
        drain()
    }

    func clearQueue() {
        queue.forEach { BackgroundUploadSessionController.shared.cancelAll(queueID: $0.id) }
        try? repository?.clearQueue()
        reload()
        drain()
    }

    func clearRecent() {
        try? repository?.clearRecent()
        reload()
    }

    func refreshRecent() {
        guard !refreshBusy, let recent, !recent.isIdentityMismatch, let identity = activeIdentity, recent.identity == identity, let repository else { return }
        let storedConfiguration: CredentialConfig?
        do { storedConfiguration = try keychain.loadConfig() } catch { return }
        guard let storedConfiguration else { return }
        let activeSession: SessionIdentity?
        let activeActivation: ActivationIdentity?
        do { activeActivation = try repository.activeSessionSnapshot() } catch { return }
        activeSession = activeActivation?.identity
        guard let activeSession, let activeActivation,
              storedConfiguration.activation == activeActivation,
              activeSession.isSupported else { return }
        guard let capture = try? repository.refreshCapture(identity: identity) else { return }
        let storedToken = storedConfiguration.installationToken
        refreshBusy = true
        Task {
            let session = await api.validateSession(origin: identity.origin, installationToken: storedToken)
            switch session {
            case .failure(let failure):
                status = statusText(for: failure.category)
            case .success(let verified):
                guard verified.isSupported else {
                    status = "服务器版本不受支持"
                    refreshBusy = false
                    reload()
                    return
                }
                guard QueueIdentity(origin: verified.origin, namespace: verified.namespace) == identity else {
                    status = "身份已变更"
                    refreshBusy = false
                    reload()
                    return
                }
                let result = await api.refresh(identity: verified, installationToken: storedToken, linkID: recent.linkID)
                switch result {
                case .success(let response):
                    if sameLinkID(recent.linkID, response.linkID),
                       (try? repository.recordRefreshSuccess(capture: capture, response: response)) == .applied {
                        status = "连接正常"
                    } else {
                        status = "身份或最近结果已变更"
                    }
                case .failure(let failure):
                    let now = Date()
                    switch failure.category {
                    case .cooldown:
                        let delay = RetryPolicy.retryAfter(failure.retryAfter, now: now) ?? RetryPolicy.minimumRetryAfter
                        _ = try? repository.recordRefreshBlocked(capture: capture, notBefore: now.addingTimeInterval(delay), reason: "cooldown_active")
                    default:
                        break
                    }
                    status = statusText(for: failure.category)
                }
            }
            refreshBusy = false
            reload()
        }
    }

    private func statusText(for category: ErrorCategory) -> String {
        switch category {
        case .auth: return "凭证无效"
        case .forbidden: return "服务器拒绝请求"
        case .cooldown: return "请稍后重新解析"
        case .identityMismatch: return "身份已变更"
        default: return "无法连接服务器"
        }
    }

    private func drain() {
        guard let coordinator else { return }
        Task { await coordinator.reconcileAndDrain() }
    }
}

struct SettingsScreen: View {
    @ObservedObject var model: WebTagSettingsModel
    @Environment(\.scenePhase) private var scenePhase
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                VStack(alignment: .leading, spacing: 4) {
                    Text("WebTag Share")
                        .font(.largeTitle.weight(.semibold))
                    Text("分享即收藏")
                        .foregroundStyle(.secondary)
                }

                GroupBox("服务器配置") {
                    VStack(spacing: 12) {
                        TextField("https://example.com", text: $model.origin)
                            .textInputAutocapitalization(.never)
                            .autocorrectionDisabled()
                            .keyboardType(.URL)
                            .textContentType(.URL)
                            .accessibilityIdentifier("settings.origin")
                            .disabled(model.isBusy)
                        HStack {
                            Group {
                                if model.tokenVisible {
                                    TextField("安装令牌", text: $model.installationToken)
                                        .accessibilityIdentifier("settings.installation-token")
                                } else {
                                    SecureField("安装令牌", text: $model.installationToken)
                                        .accessibilityIdentifier("settings.installation-token")
                                }
                            }
                            .textInputAutocapitalization(.never)
                            .autocorrectionDisabled()
                            .textContentType(.password)
                            .disabled(model.isBusy)
                            Button {
                                model.tokenVisible.toggle()
                            } label: {
                                Image(systemName: model.tokenVisible ? "eye.slash" : "eye")
                            }
                            .accessibilityLabel(model.tokenVisible ? "隐藏安装令牌" : "显示安装令牌")
                        }
                        Button("保存并测试连接") { model.saveAndTest() }
                            .buttonStyle(.borderedProminent)
                            .frame(maxWidth: .infinity)
                            .accessibilityIdentifier("settings.save")
                            .disabled(model.isBusy || model.origin.isEmpty || model.installationToken.isEmpty)
                        if model.isBusy { ProgressView() }
                        if !model.status.isEmpty {
                            Text(model.status)
                                .font(.footnote)
                                .foregroundStyle(model.status == "连接正常" ? .green : .red)
                                .frame(maxWidth: .infinity, alignment: .leading)
                        }
                        if !model.migrationRecoveryText.isEmpty {
                            Text(model.migrationRecoveryText)
                                .font(.footnote)
                                .foregroundStyle(.secondary)
                                .frame(maxWidth: .infinity, alignment: .leading)
                        }
                        if model.canRetryIdentityMigration {
                            Button("重试未完成迁移") { model.retryIdentityMigration() }
                        }
                    }
                }

                // The header counts every durable row, including rows in sections
                // that are hidden, so it stays a count of what is stored rather
                // than of what happens to be on screen.
                if model.projection.queue.total > 0 {
                    GroupBox("待处理 · \(model.projection.queue.total) 条") {
                        VStack(alignment: .leading, spacing: 16) {
                            ForEach(model.projection.queue.groups, id: \.group) { section in
                                VStack(alignment: .leading, spacing: 10) {
                                    Text("\(section.group.title) · \(section.count) 条")
                                        .font(.subheadline.weight(.semibold))
                                        .foregroundStyle(.secondary)
                                        .accessibilityIdentifier("settings.queue.group.\(section.group.rawValue)")
                                    ForEach(section.rows, id: \.id) { entry in
                                        QueueEntryRow(
                                            entry: entry,
                                            retry: { model.retry(entry) },
                                            canMigrate: model.canMigrate(entry),
                                            migrate: { model.requestIdentityMigration(entry) },
                                            delete: { model.delete(entry) }
                                        )
                                    }
                                }
                            }
                            HStack {
                                Button("重试可恢复条目") { model.retryRecoverable() }
                                Spacer()
                                Button("清空", role: .destructive) { model.showClearConfirmation = true }
                            }
                        }
                    }
                }

                if let recent = model.recent {
                    GroupBox("最近一条结果") {
                        VStack(alignment: .leading, spacing: 10) {
                            if recent.isIdentityMismatch {
                                Text("身份已变更")
                                    .foregroundStyle(.red)
                            } else {
                                Text(recent.url)
                                    .lineLimit(3)
                                    .textSelection(.enabled)
                                Text(statusText(recent.status))
                                    .foregroundStyle(recent.status == "failed" ? .red : .green)
                            }
                            // Everything below the identity check is redacted on a
                            // mismatch: the link ID and result time describe another
                            // identity's data.
                            if !recent.isIdentityMismatch {
                                Text("链接 ID：\(recent.linkID)")
                                    .font(.caption)
                                    .foregroundStyle(.secondary)
                                    .textSelection(.enabled)
                                    .accessibilityIdentifier("settings.recent.link-id")
                                if let resultTime = SettingsTimeFormatter.absolute(recent.createdAt) {
                                    Text("结果时间：\(resultTime)")
                                        .font(.caption)
                                        .foregroundStyle(.secondary)
                                        .accessibilityIdentifier("settings.recent.result-time")
                                }
                            }
                            if !recent.isIdentityMismatch, let reason = model.refreshGate.blockReason {
                                VStack(alignment: .leading, spacing: 2) {
                                    Text(reason == SettingsRefreshGatePolicy.cooldownReason ? "重新解析冷却中" : reason)
                                    if let until = SettingsTimeFormatter.absolute(model.refreshGate.cooldownUntil) {
                                        Text("可在 \(until) 后重试")
                                    }
                                }
                                .font(.footnote)
                                .foregroundStyle(.red)
                            }
                            if recent.status == "failed", !recent.isIdentityMismatch {
                                // The gate is recomputed by the cooldown timer at the
                                // exact deadline, so this unlocks on its own instead
                                // of waiting for the next redraw to notice.
                                Button("重新解析") { model.refreshRecent() }
                                    .disabled(model.refreshBusy || !model.refreshGate.isEnabled)
                            }
                            Button {
                                model.clearRecent()
                            } label: {
                                Image(systemName: "trash")
                            }
                            .accessibilityLabel("删除最近结果")
                        }
                    }
                }

                if !model.recentMigrationCandidates.isEmpty {
                    GroupBox("旧最近结果") {
                        VStack(alignment: .leading, spacing: 12) {
                            ForEach(model.recentMigrationCandidates) { candidate in
                                HStack(alignment: .center, spacing: 8) {
                                    VStack(alignment: .leading, spacing: 2) {
                                        Text(candidate.revision == 0 ? "旧格式" : "R\(candidate.revision)")
                                            .font(.subheadline.weight(.semibold))
                                        if let resultTime = SettingsTimeFormatter.absolute(candidate.createdAt) {
                                            Text("结果时间：\(resultTime)")
                                                .font(.caption)
                                                .foregroundStyle(.secondary)
                                        }
                                    }
                                    Spacer(minLength: 8)
                                    Button("迁移") { model.requestIdentityMigration(candidate) }
                                }
                            }
                        }
                    }
                }

                if !model.todoMigrationCandidates.isEmpty {
                    GroupBox("旧待办数据") {
                        VStack(alignment: .leading, spacing: 12) {
                            ForEach(model.todoMigrationCandidates) { candidate in
                                HStack(alignment: .center, spacing: 8) {
                                    VStack(alignment: .leading, spacing: 2) {
                                        Text(candidate.revision == 0 ? "旧格式" : "R\(candidate.revision)")
                                            .font(.subheadline.weight(.semibold))
                                        Text("\(candidate.itemCount) 项待办 · \(candidate.operationCount) 个离线操作")
                                            .font(.caption)
                                            .foregroundStyle(.secondary)
                                    }
                                    Spacer(minLength: 8)
                                    Button("迁移") { model.requestIdentityMigration(candidate) }
                                }
                            }
                        }
                    }
                }

                Text("在任意 APP 中点击分享，选择 WebTag 即可收藏")
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                Text("v0.1.0")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }
                .padding(20)
            }
            .navigationTitle("设置")
            .toolbar {
                ToolbarItem(placement: .confirmationAction) {
                    Button("完成") { dismiss() }
                }
            }
            // Becoming active is the only reload trigger. A background change made
            // by the share extension or a background completion leaves no signal in
            // this process, so without this the screen can sit on a stale snapshot
            // for as long as it stays open.
            .onAppear { model.onForegroundActive() }
            .onChange(of: scenePhase) { phase in
                if phase == .active { model.onForegroundActive() }
            }
            .onDisappear { model.dispose() }
            .confirmationDialog("清空待处理条目？", isPresented: $model.showClearConfirmation) {
                Button("确认", role: .destructive) { model.clearQueue() }
                Button("取消", role: .cancel) {}
            } message: {
                Text("这会删除本地队列中的 \(model.projection.queue.total) 条记录。")
            }
            .confirmationDialog("再次提交失败条目？", isPresented: $model.showPermanentRetryConfirmation) {
                Button("确认重试") { model.confirmPermanentRetry() }
                Button("取消", role: .cancel) {}
            } message: {
                Text("这会复用原 URL，但会重新进入提交流程；服务端失败链接不会被隐式解析。")
            }
            .confirmationDialog("迁移旧版本数据？", isPresented: $model.showIdentityMigrationConfirmation) {
                Button("确认迁移全部数据") { model.confirmIdentityMigration() }
                Button("取消", role: .cancel) { model.cancelIdentityMigration() }
            } message: {
                Text(model.identityMigrationMessage)
            }
        }
    }

    private func statusText(_ status: String) -> String {
        switch status {
        case "pending", "processing": return "已收藏"
        case "done": return "已在库中"
        case "failed": return "已在库中，解析失败"
        default: return "提交失败"
        }
    }
}

struct QueueEntryRow: View {
    let entry: QueueEntry
    let retry: () -> Void
    let canMigrate: Bool
    let migrate: () -> Void
    let delete: () -> Void

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            VStack(alignment: .leading, spacing: 4) {
                Text(entry.state == .blockedIdentity ? "身份已变更" : entry.url)
                    .lineLimit(3)
                Text(label(for: entry.state))
                    .font(.caption)
                    .foregroundStyle(.secondary)
                // Only rendered when the field exists. A placeholder would sit in
                // the same position as a real timestamp and read as if the value
                // were known and empty.
                if let firstFailed = SettingsTimeFormatter.absolute(entry.firstFailedAt) {
                    Text("首次失败：\(firstFailed)")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
                if let nextRetry = SettingsTimeFormatter.absolute(entry.nextAttemptAt) {
                    Text("下次重试：\(nextRetry)")
                        .font(.caption2)
                        .foregroundStyle(.secondary)
                }
            }
            Spacer(minLength: 4)
            if entry.state == .blockedIdentity {
                if canMigrate {
                    Button("迁移", action: migrate)
                        .accessibilityLabel("迁移此版本数据")
                }
            } else {
                Button(action: retry) {
                    Image(systemName: "arrow.clockwise")
                }
                .accessibilityLabel("重试")
                .disabled(entry.state == .pendingSubmit)
            }
            Button(role: .destructive, action: delete) {
                Image(systemName: "trash")
            }
            .accessibilityLabel("删除")
        }
        .padding(.vertical, 4)
    }

    private func label(for state: QueueState) -> String {
        switch state {
        case .pendingSubmit: return "待提交"
        case .retryWait: return "等待重试"
        case .blockedAuth: return "凭证无效"
        case .blockedIdentity: return "身份已变更"
        case .failedPermanent: return "提交失败"
        case .expired: return "已过期"
        }
    }
}

private func namespaceFingerprint(_ namespace: String) -> String {
    let digest = SHA256.hash(data: Data(namespace.utf8))
    return digest.prefix(4).map { String(format: "%02x", Int($0)) }.joined()
}
