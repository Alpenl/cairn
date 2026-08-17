import Foundation
import XCTest
@testable import WebTagShare

private final class WebTagURLProtocol: URLProtocol {
    struct Reply {
        let status: Int
        let headers: [String: String]
        let body: Data
    }

    static var reply: ((URLRequest) -> Reply)?
    static var failure: Error?
    static var failNextRequest = false
    static var requestObserver: ((URLRequest) -> Void)?
    static var requestCount = 0

    static func reset() {
        reply = nil
        failure = nil
        failNextRequest = false
        requestObserver = nil
        requestCount = 0
    }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        Self.requestCount += 1
        // Status, headers and call count only. `URLSession` strips the body
        // from the request it hands to a `URLProtocol`, so anything that has to
        // assert on the bytes uses `RecordingTransport` instead.
        Self.requestObserver?(request)
        if Self.failNextRequest {
            Self.failNextRequest = false
            client?.urlProtocol(
                self,
                didFailWithError: NSError(domain: NSURLErrorDomain, code: NSURLErrorNetworkConnectionLost)
            )
            return
        }
        if let failure = Self.failure {
            client?.urlProtocol(self, didFailWithError: failure)
            return
        }
        guard let reply = Self.reply?(request), let url = request.url,
              let response = HTTPURLResponse(url: url, statusCode: reply.status, httpVersion: "HTTP/1.1", headerFields: reply.headers) else {
            client?.urlProtocol(self, didFailWithError: CoreError.invalidResponse)
            return
        }
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: reply.body)
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}

/// The transport the client sends through, held still so the test can read it.
///
/// This sits directly beneath `WebTagAPIClient` and above `URLSession`, so the
/// `URLRequest` recorded here is the one the client built, body included. A
/// custom `URLProtocol` cannot serve the same purpose: by the time `URLSession`
/// hands the request over, the body has been taken off it.
private final class RecordingTransport: HTTPTransport {
    private let lock = NSLock()
    private var recorded: [URLRequest] = []
    private var parked: [CheckedContinuation<(Data, URLResponse), Error>] = []
    private let pauseAfterRequest: Bool
    private let onRequest: ((URLRequest) -> Void)?
    private let reply: (URLRequest) throws -> (Data, URLResponse)

    /// - Parameter pauseAfterRequest: accept the request and never answer it,
    ///   which is the only way to observe what a client does when its own
    ///   deadline expires while the request is still outstanding.
    init(
        pauseAfterRequest: Bool = false,
        onRequest: ((URLRequest) -> Void)? = nil,
        reply: @escaping (URLRequest) throws -> (Data, URLResponse) = { _ in throw CoreError.invalidResponse }
    ) {
        self.pauseAfterRequest = pauseAfterRequest
        self.onRequest = onRequest
        self.reply = reply
    }

    /// Every request the client handed over, in order.
    var requests: [URLRequest] {
        lock.lock()
        defer { lock.unlock() }
        return recorded
    }

    func send(_ request: URLRequest) async throws -> (Data, URLResponse) {
        record(request)
        onRequest?(request)
        guard pauseAfterRequest else { return try reply(request) }
        return try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<(Data, URLResponse), Error>) in
            self.park(continuation)
        }
    }

    /// Fails every parked request, so no continuation outlives the test.
    func releaseParked() {
        lock.lock()
        let waiting = parked
        parked = []
        lock.unlock()
        for continuation in waiting { continuation.resume(throwing: CancellationError()) }
    }

    /// Locking lives in these synchronous frames on purpose: `NSLock` is
    /// unavailable from an asynchronous one, and a scoped lock around a single
    /// mutation is the whole critical section either way.
    private func record(_ request: URLRequest) {
        lock.lock()
        recorded.append(request)
        lock.unlock()
    }

    private func park(_ continuation: CheckedContinuation<(Data, URLResponse), Error>) {
        lock.lock()
        parked.append(continuation)
        lock.unlock()
    }
}

private struct StaticCredentialStore: CredentialConfigLoading {
    let config: CredentialConfig?

    func loadConfig() throws -> CredentialConfig? { config }
}

private final class RecordingUploadScheduler: QueueUploadScheduling {
    private(set) var scheduledEntry: QueueEntry?
    private(set) var scheduledOwner: String?
    private(set) var claims: Set<BackgroundUploadClaim> = []

    func schedule(entry: QueueEntry, identity: SessionIdentity, installationToken: String, owner: String) throws {
        scheduledEntry = entry
        scheduledOwner = owner
        claims.insert(BackgroundUploadClaim(
            queueID: entry.id,
            owner: owner,
            identityRevision: entry.identityRevision
        ))
        _ = identity
        _ = installationToken
    }

    func activeClaims() async -> Set<BackgroundUploadClaim> { claims }

    func cancel(claim: BackgroundUploadClaim) {
        claims.remove(claim)
    }

    func reconcile(repository: AppGroupQueueRepository, now: Date) async -> Set<BackgroundUploadClaim> {
        claims = Set(claims.filter {
            guard let result = try? repository.reconcileBackgroundClaim($0, now: now),
                  case .matched = result else { return false }
            return true
        })
        return claims
    }
}

private final class RecordingWakeScheduler: QueueWakeScheduling {
    private(set) var deadlines: [Date?] = []

    func schedule(deadline: Date?) {
        deadlines.append(deadline)
    }
}

private final class RecordingDurableWakeAdapter: DurableQueueWakeAdapting {
    private(set) var replacements: [Date?] = []

    func replace(deadline: Date?) throws {
        replacements.append(deadline)
    }
}

private final class LockedExpirationSignal {
    private let lock = NSLock()
    private var expired = false

    var isExpired: Bool {
        lock.lock()
        defer { lock.unlock() }
        return expired
    }

    func expire() {
        lock.lock()
        expired = true
        lock.unlock()
    }
}

private final class RecoverableMigrationQueueStore: IdentityMigrationQueuePersisting {
    let source: ActivationIdentity
    let target: ActivationIdentity
    var active: ActivationIdentity
    var failRecentOnce = false
    private(set) var queueWrites = 0
    private(set) var recentCalls = 0
    private(set) var migratedIDs = Set<String>()
    private let sourceEntries: [QueueEntry]

    init(source: ActivationIdentity, target: ActivationIdentity, entryIDs: [String]) {
        self.source = source
        self.target = target
        active = target
        sourceEntries = entryIDs.map {
            migrationQueueEntry(id: $0, activation: source)
        }
    }

    func activeSessionSnapshot() throws -> ActiveSessionSnapshot? { active }
    func list(identity: QueueIdentity?) throws -> [QueueEntry] { sourceEntries }

    func entry(id: String) throws -> QueueEntry? {
        guard sourceEntries.contains(where: { $0.id == id }) else { return nil }
        return migrationQueueEntry(id: id, activation: migratedIDs.contains(id) ? target : source)
    }

    func hasMigratableRecent(from source: ActivationIdentity, to target: ActivationIdentity) throws -> Bool {
        source == self.source && target == self.target
    }

    func recentMigrationCandidates(to target: ActivationIdentity) throws -> [RecentMigrationCandidate] {
        guard target == self.target else { return [] }
        return [RecentMigrationCandidate(identity: source.queueIdentity, revision: source.revision, createdAt: Date())]
    }

    func migrateIdentity(
        id: String,
        from source: ActivationIdentity,
        to target: ActivationIdentity,
        now: Date
    ) throws -> Bool {
        guard active == target, source == self.source, target == self.target else { return false }
        guard !migratedIDs.contains(id) else { return true }
        queueWrites += 1
        migratedIDs.insert(id)
        _ = now
        return true
    }

    func migrateRecent(from source: ActivationIdentity, to target: ActivationIdentity) throws -> Bool {
        guard active == target, source == self.source, target == self.target else { return false }
        recentCalls += 1
        if failRecentOnce {
            failRecentOnce = false
            throw CoreError.database
        }
        return true
    }

    func withActiveTargetFence(_ target: ActivationIdentity, perform: () throws -> Bool) throws -> Bool {
        guard active == target else { return false }
        return try perform()
    }
}

private final class RecoverableMigrationTodoStore: IdentityMigrationTodoPersisting {
    let candidate: CompanionTodoMigrationCandidate
    private(set) var migrationCalls = 0
    private(set) var writes = 0
    private var migrated = false

    init(source: ActivationIdentity) {
        candidate = CompanionTodoMigrationCandidate(
            identity: source.queueIdentity,
            revision: source.revision,
            itemCount: 2,
            operationCount: 3
        )
    }

    func migrationCandidate(
        identity sourceIdentity: QueueIdentity,
        revision sourceRevision: Int64,
        to target: ActivationIdentity
    ) throws -> CompanionTodoMigrationCandidate? {
        guard sourceIdentity == candidate.identity,
              sourceRevision == candidate.revision,
              target.queueIdentity == candidate.identity else { return nil }
        return candidate
    }

    func migratePartition(
        identity sourceIdentity: QueueIdentity,
        fromRevision sourceRevision: Int64,
        to target: ActivationIdentity,
        now: Date
    ) throws -> Bool {
        guard sourceIdentity == candidate.identity,
              sourceRevision == candidate.revision,
              target.queueIdentity == candidate.identity else { return false }
        migrationCalls += 1
        if !migrated {
            writes += 1
            migrated = true
        }
        _ = now
        return true
    }
}

private final class UnavailableMigrationTodoStore: IdentityMigrationTodoPersisting {
    func migrationCandidate(
        identity sourceIdentity: QueueIdentity,
        revision sourceRevision: Int64,
        to target: ActivationIdentity
    ) throws -> CompanionTodoMigrationCandidate? {
        throw TodoStateError.unavailable
    }

    func migratePartition(
        identity sourceIdentity: QueueIdentity,
        fromRevision sourceRevision: Int64,
        to target: ActivationIdentity,
        now: Date
    ) throws -> Bool {
        throw TodoStateError.unavailable
    }
}

private final class BlockingCompanionTodoCipher: CompanionTodoStateCrypting {
    private let base: CompanionTodoAESGCMCipher
    private let stateLock = NSLock()
    private var shouldBlock = true
    let sealEntered = DispatchSemaphore(value: 0)
    let releaseSeal = DispatchSemaphore(value: 0)

    init(keyData: Data) throws {
        base = try CompanionTodoAESGCMCipher(keyData: keyData)
    }

    func seal(_ plaintext: Data) throws -> Data {
        stateLock.lock()
        let block = shouldBlock
        shouldBlock = false
        stateLock.unlock()
        if block {
            sealEntered.signal()
            releaseSeal.wait()
        }
        return try base.seal(plaintext)
    }

    func open(_ ciphertext: Data) throws -> Data {
        try base.open(ciphertext)
    }
}

private final class FailOnceCompanionTodoCipher: CompanionTodoStateCrypting {
    private let base: CompanionTodoAESGCMCipher
    private var shouldFail = false

    init(keyData: Data) throws {
        base = try CompanionTodoAESGCMCipher(keyData: keyData)
    }

    func failNextSeal() { shouldFail = true }

    func seal(_ plaintext: Data) throws -> Data {
        if shouldFail {
            shouldFail = false
            throw TodoStateError.unavailable
        }
        return try base.seal(plaintext)
    }

    func open(_ ciphertext: Data) throws -> Data {
        try base.open(ciphertext)
    }
}

/// A clock the test moves by hand, so deadline behaviour is decided by the
/// assertions instead of by how busy the machine is.
private final class FakeMonotonicClock: ShareMonotonicClock {
    private let lock = NSLock()
    private var seconds: TimeInterval = 0
    private var scheduled: [(fireAt: TimeInterval, body: () -> Void)] = []
    private var armed: [TimeInterval] = []

    var nowSeconds: TimeInterval {
        lock.lock()
        defer { lock.unlock() }
        return seconds
    }

    /// Every deadline ever armed on this clock, as an absolute instant, kept
    /// after firing. How many timers a component arms - and not merely when the
    /// earliest of them goes off - is the only way to tell one shared deadline
    /// from several coincident per-item ones.
    var armedDeadlines: [TimeInterval] {
        lock.lock()
        defer { lock.unlock() }
        return armed
    }

    func schedule(after delay: TimeInterval, _ body: @escaping () -> Void) {
        lock.lock()
        let fireAt = seconds + max(0, delay)
        scheduled.append((fireAt, body))
        armed.append(fireAt)
        lock.unlock()
    }

    /// Moves the monotonic clock without dispatching due timers. A busy run
    /// loop can delay timer delivery in production, but it must not extend an
    /// absolute deadline.
    func elapseWithoutFiring(to target: TimeInterval) {
        lock.lock()
        seconds = max(seconds, target)
        lock.unlock()
    }

    /// Moves time forward and runs everything that has come due, so a test can
    /// stand 1ms short of a deadline and then step exactly onto it.
    func advance(to target: TimeInterval) {
        while true {
            lock.lock()
            seconds = max(seconds, target)
            guard let index = scheduled.firstIndex(where: { $0.fireAt <= target }) else {
                lock.unlock()
                return
            }
            let due = scheduled.remove(at: index)
            lock.unlock()
            due.body()
        }
    }
}

private final class FakeLoadCancellation: ShareLoadCancelling {
    private(set) var cancelCount = 0

    func cancelLoad() { cancelCount += 1 }
}

private func activate(_ repository: AppGroupQueueRepository, _ identity: QueueIdentity) throws {
    _ = try repository.activate(session: SessionIdentity(
        origin: identity.origin,
        namespace: identity.namespace,
        representationContract: "v3"
    ))
}

private func activation(_ identity: QueueIdentity, revision: Int64 = 1) -> ActivationIdentity {
    ActivationIdentity(
        identity: SessionIdentity(
            origin: identity.origin,
            namespace: identity.namespace,
            representationContract: "v3"
        ),
        revision: revision
    )
}

private final class FakeItemProvider: ShareRepresentationLoading {
    enum Response {
        case immediate(String?)
        /// Answers only when the test says so - possibly never.
        case deferred
    }

    let declaredRepresentations: [ShareRepresentationKind]
    private let responses: [ShareRepresentationKind: Response]
    private(set) var startedKinds: [ShareRepresentationKind] = []
    private(set) var cancellations: [ShareRepresentationKind: FakeLoadCancellation] = [:]
    private var pending: [ShareRepresentationKind: (String?) -> Void] = [:]

    init(_ responses: [(ShareRepresentationKind, Response)]) {
        declaredRepresentations = responses.map(\.0)
        self.responses = Dictionary(uniqueKeysWithValues: responses)
    }

    func loadRepresentation(
        _ kind: ShareRepresentationKind,
        completion: @escaping (String?) -> Void
    ) -> ShareLoadCancelling? {
        startedKinds.append(kind)
        let cancellation = FakeLoadCancellation()
        cancellations[kind] = cancellation
        switch responses[kind] ?? .deferred {
        case .immediate(let value): completion(value)
        case .deferred: pending[kind] = completion
        }
        return cancellation
    }

    /// Delivers a callback the collector may or may not still be waiting for;
    /// the second case is exactly what a late provider looks like.
    func complete(_ kind: ShareRepresentationKind, with value: String?) {
        pending[kind]?(value)
    }
}

/// One shared item, standing in for an `NSExtensionItem` that no test can
/// populate with stub providers.
private struct FakeInputItem: ShareInputItem {
    let shareAttachments: [ShareRepresentationLoading]

    init(_ attachments: [ShareRepresentationLoading]) {
        shareAttachments = attachments
    }
}

final class WebTagShareTests: XCTestCase {
    override func setUp() {
        super.setUp()
        WebTagURLProtocol.reset()
    }

    override func tearDown() {
        WebTagURLProtocol.reset()
        super.tearDown()
    }

    func testAppGroupAndKeychainIdentifiersAreInjectedByTheBuildConfiguration() {
        XCTAssertTrue(AppIdentifiers.appGroup.hasPrefix("group."))
        XCTAssertTrue(AppIdentifiers.keychainAccessGroup.contains("com.alpenl.webtag.share"))
    }

    func testSharedFixturesProduceTheRecordedCandidatesAndOutcome() throws {
        let sourceURL = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .deletingLastPathComponent()
            .appendingPathComponent("shared/fixtures/share-payloads.json")
        let data = try Data(contentsOf: sourceURL)
        let root = try XCTUnwrap(try JSONSerialization.jsonObject(with: data) as? [String: Any])
        let cases = try XCTUnwrap(root["cases"] as? [[String: Any]])
        XCTAssertGreaterThanOrEqual(cases.count, 200)
        for fixture in cases {
            let id = try XCTUnwrap(fixture["id"] as? String)
            let structured = try XCTUnwrap(fixture["structured_urls"] as? [String])
            let text = fixture["plain_text"] as? String
            let actual = URLCandidateExtractor.extract(SharePayload(structuredURLs: structured, plainText: text)).map(\.submissionValue)
            let expected = try XCTUnwrap(fixture["expected_candidates"] as? [String])
            XCTAssertEqual(actual, expected, id)
            let outcome = try XCTUnwrap(fixture["expected_outcome"] as? String)
            let actualOutcome: String
            switch actual.count {
            case 0: actualOutcome = "reject"
            case 1: actualOutcome = "submit"
            default: actualOutcome = "choose"
            }
            XCTAssertEqual(actualOutcome, outcome, id)
            XCTAssertEqual(actual.count > 1, outcome == "choose", id)
        }
    }

    func testOriginNormalizerRejectsCrossOriginInputs() throws {
        XCTAssertEqual(try OriginNormalizer.normalize(" HTTPS://Example.org/ "), "https://example.org")
        XCTAssertEqual(try OriginNormalizer.normalize("https://example.org:8443"), "https://example.org:8443")
        XCTAssertThrowsError(try OriginNormalizer.normalize("http://example.org"))
        XCTAssertThrowsError(try OriginNormalizer.normalize("https://user:pass@example.org"))
        XCTAssertThrowsError(try OriginNormalizer.normalize("https://example.org/path"))
        XCTAssertThrowsError(try OriginNormalizer.normalize("https://example.org/?q=1"))
    }

    func testRetryPolicyKeepsTrustFailuresPermanentAndCapsDelay() {
        XCTAssertEqual(RetryPolicy.retryAfter("1", now: Date(timeIntervalSince1970: 0)), 60)
        XCTAssertTrue(RetryPolicy.delay(attempt: 20) <= RetryPolicy.sixHours)
        XCTAssertTrue(RetryPolicy.shouldExpire(firstFailedAt: Date(timeIntervalSince1970: 0), now: Date(timeIntervalSince1970: RetryPolicy.sevenDays)))
    }

    func testQueueFailurePolicyCoversAllFrozenStateBranches() {
        let now = Date(timeIntervalSince1970: 0)
        XCTAssertEqual(QueueFailurePolicy.state(for: .auth, firstFailedAt: now, now: now), .blockedAuth)
        XCTAssertEqual(QueueFailurePolicy.state(for: .forbidden, firstFailedAt: now, now: now), .blockedAuth)
        XCTAssertEqual(QueueFailurePolicy.state(for: .identityMismatch, firstFailedAt: now, now: now), .blockedIdentity)
        XCTAssertEqual(QueueFailurePolicy.state(for: .tlsTrustFailure, firstFailedAt: now, now: now), .failedPermanent)
        XCTAssertEqual(QueueFailurePolicy.state(for: .server, firstFailedAt: now, now: now), .retryWait)
        XCTAssertEqual(
            QueueFailurePolicy.state(
                for: .server,
                firstFailedAt: now.addingTimeInterval(-RetryPolicy.sevenDays),
                now: now
            ),
            .expired
        )
        XCTAssertNil(QueueFailurePolicy.nextAttemptAt(for: .forbidden, attempt: 1, retryAfter: "60", firstFailedAt: now, now: now))
        XCTAssertNotNil(QueueFailurePolicy.nextAttemptAt(for: .rateLimit, attempt: 1, retryAfter: "1", firstFailedAt: now, now: now))
    }

    func testErrorClassifierSeparatesDeadlineDNSAndTLS() {
        XCTAssertEqual(ErrorClassifier.transport(NSError(domain: NSURLErrorDomain, code: NSURLErrorTimedOut)).category, .clientDeadline)
        XCTAssertEqual(ErrorClassifier.transport(NSError(domain: NSURLErrorDomain, code: NSURLErrorDNSLookupFailed)).category, .dnsTimeout)
        XCTAssertEqual(ErrorClassifier.transport(NSError(domain: NSURLErrorDomain, code: NSURLErrorSecureConnectionFailed)).category, .tlsTrustFailure)
    }

    func testErrorClassifierScansUnderlyingTransportErrors() {
        let wrapped = NSError(
            domain: "WebTagShareTest",
            code: 1,
            userInfo: [NSUnderlyingErrorKey: NSError(domain: NSURLErrorDomain, code: NSURLErrorServerCertificateUntrusted)]
        )

        XCTAssertEqual(ErrorClassifier.transport(wrapped).category, .tlsTrustFailure)
    }

    func testRepositoryLeaseExpiryAndAtomicSuccess() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "n", count: 43))
        try activate(repository, identity)
        let start = Date(timeIntervalSince1970: 1_000)
        let entry = try repository.enqueue(url: "https://example.org/article", identity: identity, now: start)
        XCTAssertTrue(try repository.claim(id: entry.id, owner: "owner-a", now: start, leaseDuration: 10))
        XCTAssertFalse(try repository.claim(id: entry.id, owner: "owner-b", now: start.addingTimeInterval(1), leaseDuration: 10))
        XCTAssertEqual(try repository.entry(id: entry.id)?.leaseOwner, "owner-a")
        XCTAssertEqual(try repository.due(now: start.addingTimeInterval(11)).map(\.id), [entry.id])
        try repository.releaseExpiredLeases(now: start.addingTimeInterval(11))
        XCTAssertTrue(try repository.claim(id: entry.id, owner: "owner-b", now: start.addingTimeInterval(11), leaseDuration: 10))
        let claimed = try XCTUnwrap(try repository.entry(id: entry.id))
        let response = SubmitResponse(linkID: "11111111-1111-1111-1111-111111111111", status: "pending", jobID: nil)
        try repository.finishSuccess(entry: claimed, owner: "owner-b", response: response, now: start.addingTimeInterval(12))
        XCTAssertTrue(try repository.list().isEmpty)
        let recent = try XCTUnwrap(try repository.recent(identity: identity))
        XCTAssertEqual(recent.linkID, response.linkID)
        XCTAssertFalse(recent.isIdentityMismatch)
    }

    func testFailedSubmitStatusIsStoredAsRecentWithoutLeavingAQueueRow() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "f", count: 43))
        try activate(repository, identity)
        let start = Date(timeIntervalSince1970: 1_100)
        let entry = try repository.enqueue(url: "https://example.org/failed", identity: identity, now: start)
        XCTAssertTrue(try repository.claim(id: entry.id, owner: "failed-owner", now: start))
        let claimed = try XCTUnwrap(try repository.entry(id: entry.id))
        let response = SubmitResponse(linkID: "22222222-2222-2222-2222-222222222222", status: "failed", jobID: nil)

        try repository.finishSuccess(entry: claimed, owner: "failed-owner", response: response, now: start.addingTimeInterval(1))

        XCTAssertTrue(try repository.list().isEmpty)
        XCTAssertEqual(try repository.recent(identity: identity)?.status, "failed")
        XCTAssertEqual(try repository.recent(identity: identity)?.linkID, response.linkID)
    }

    func testFailedSubmitDoesNotImplicitlyRefresh() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let namespace = String(repeating: "g", count: 43)
        let identity = SessionIdentity(origin: "https://example.org", namespace: namespace, representationContract: "v3")
        let queue = try AppGroupQueueRepository(containerURL: directory)
        try queue.activate(session: identity)
        let credentials = StaticCredentialStore(
            config: CredentialConfig(identity: identity, installationToken: "test-token")
        )

        WebTagURLProtocol.requestCount = 0
        WebTagURLProtocol.failure = nil
        WebTagURLProtocol.reply = { request in
            XCTAssertEqual(request.url?.path, "/api/links")
            return WebTagURLProtocol.Reply(
                status: 202,
                headers: ["X-WebTag-Data-Namespace": namespace],
                body: Data(#"{"link_id":"66666666-6666-6666-6666-666666666666","status":"failed"}"#.utf8)
            )
        }
        defer {
            WebTagURLProtocol.reply = nil
            WebTagURLProtocol.failure = nil
            WebTagURLProtocol.requestCount = 0
        }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [WebTagURLProtocol.self]
        let api = WebTagAPIClient(session: URLSession(configuration: configuration))
        let coordinator = ShareSubmissionCoordinator(
            repository: queue,
            credentials: credentials,
            api: api,
            wakeScheduler: RecordingWakeScheduler()
        )

        let outcome = await coordinator.submit(url: "https://example.org/failed", identity: identity, now: Date(timeIntervalSince1970: 1_200))

        guard case .submitted(let response) = outcome else {
            XCTFail("expected failed submit to be a completed result")
            return
        }
        XCTAssertEqual(response.status, "failed")
        XCTAssertEqual(WebTagURLProtocol.requestCount, 1)
        XCTAssertTrue(try queue.list().isEmpty)
        XCTAssertEqual(try queue.recent(identity: QueueIdentity(origin: identity.origin, namespace: identity.namespace))?.status, "failed")
    }

    func testRepositoryClaimRejectsFutureRetryAndTerminalRows() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "q", count: 43))
        try activate(repository, identity)
        let now = Date(timeIntervalSince1970: 1_200)

        let future = try repository.enqueue(url: "https://example.org/future", identity: identity, now: now)
        XCTAssertTrue(try repository.claim(id: future.id, owner: "future-owner", now: now))
        let claimedFuture = try XCTUnwrap(try repository.entry(id: future.id))
        try repository.applyFailure(
            entry: claimedFuture,
            owner: "future-owner",
            state: .retryWait,
            category: .server,
            errorCode: nil,
            status: 503,
            nextAttemptAt: now.addingTimeInterval(60),
            firstFailedAt: now,
            now: now.addingTimeInterval(1)
        )
        XCTAssertFalse(try repository.claim(id: future.id, owner: "early-owner", now: now.addingTimeInterval(2)))

        let terminal = try repository.enqueue(url: "https://example.org/terminal", identity: identity, now: now)
        XCTAssertTrue(try repository.claim(id: terminal.id, owner: "terminal-owner", now: now))
        let claimedTerminal = try XCTUnwrap(try repository.entry(id: terminal.id))
        try repository.applyFailure(
            entry: claimedTerminal,
            owner: "terminal-owner",
            state: .failedPermanent,
            category: .tlsTrustFailure,
            errorCode: nil,
            status: nil,
            nextAttemptAt: nil,
            firstFailedAt: now,
            now: now.addingTimeInterval(1)
        )
        XCTAssertFalse(try repository.claim(id: terminal.id, owner: "terminal-retry", now: now.addingTimeInterval(2)))
    }

    func testRepositoryFindReusableKeepsPendingRetryIdentityAndKeyTogether() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "u", count: 43))
        try activate(repository, identity)
        let start = Date(timeIntervalSince1970: 1_500)
        let url = "https://example.org/reuse"
        let entry = try repository.enqueue(url: url, identity: identity, now: start)

        let reusablePending = try XCTUnwrap(try repository.findReusable(url: url, identity: identity))
        XCTAssertEqual(reusablePending.id, entry.id)
        XCTAssertEqual(reusablePending.idempotencyKey, entry.idempotencyKey)

        XCTAssertTrue(try repository.claim(id: entry.id, owner: "retry-owner", now: start, leaseDuration: 10))
        let claimed = try XCTUnwrap(try repository.entry(id: entry.id))
        try repository.applyFailure(
            entry: claimed,
            owner: "retry-owner",
            state: .retryWait,
            category: .server,
            errorCode: nil,
            status: 503,
            nextAttemptAt: start.addingTimeInterval(60),
            firstFailedAt: start,
            now: start.addingTimeInterval(1)
        )

        let reusableRetry = try XCTUnwrap(try repository.findReusable(url: url, identity: identity))
        XCTAssertEqual(reusableRetry.id, entry.id)
        XCTAssertEqual(reusableRetry.idempotencyKey, entry.idempotencyKey)
        XCTAssertEqual(reusableRetry.nextAttemptAt, start.addingTimeInterval(60))
        XCTAssertNil(try repository.findReusable(url: url, identity: QueueIdentity(origin: identity.origin, namespace: String(repeating: "v", count: 43))))
    }

    func testRepositoryEnqueueOrReuseAtomicallyKeepsTheExistingIdempotencyKey() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "a", count: 43))
        try activate(repository, identity)
        let url = "https://example.org/atomic-reuse"

        let first = try repository.enqueueOrReuse(url: url, identity: identity, now: Date(timeIntervalSince1970: 1_600))
        let second = try repository.enqueueOrReuse(url: url, identity: identity, now: Date(timeIntervalSince1970: 1_601))

        XCTAssertFalse(first.reused)
        XCTAssertTrue(second.reused)
        XCTAssertEqual(first.entry.id, second.entry.id)
        XCTAssertEqual(first.entry.idempotencyKey, second.entry.idempotencyKey)
        XCTAssertEqual(try repository.list().count, 1)
    }

    func testSuccessRollsBackWhenLeaseWasReclaimed() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "r", count: 43))
        try activate(repository, identity)
        let start = Date(timeIntervalSince1970: 2_000)
        let entry = try repository.enqueue(url: "https://example.org/reclaimed", identity: identity, now: start)
        XCTAssertTrue(try repository.claim(id: entry.id, owner: "owner-a", now: start, leaseDuration: 1))
        XCTAssertTrue(try repository.claim(id: entry.id, owner: "owner-b", now: start.addingTimeInterval(2), leaseDuration: 10))
        let stale = try XCTUnwrap(try repository.entry(id: entry.id))

        XCTAssertEqual(try repository.finishSuccess(
            entry: stale,
            owner: "owner-a",
            response: SubmitResponse(linkID: "55555555-5555-5555-5555-555555555555", status: "pending", jobID: nil),
            now: start.addingTimeInterval(3)
        ), .staleClaim)
        XCTAssertEqual(try repository.entry(id: entry.id)?.leaseOwner, "owner-b")
        XCTAssertNil(try repository.recent())
    }

    func testExpiredOwnerCannotCommitSuccessBeforeItIsReclaimed() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "e", count: 43))
        try activate(repository, identity)
        let start = Date(timeIntervalSince1970: 2_500)
        let entry = try repository.enqueue(url: "https://example.org/expired", identity: identity, now: start)
        XCTAssertTrue(try repository.claim(id: entry.id, owner: "expired-owner", now: start, leaseDuration: 1))
        let expired = try XCTUnwrap(try repository.entry(id: entry.id))

        XCTAssertEqual(try repository.finishSuccess(
            entry: expired,
            owner: "expired-owner",
            response: SubmitResponse(linkID: "66666666-6666-6666-6666-666666666666", status: "pending", jobID: nil),
            now: start.addingTimeInterval(1)
        ), .staleClaim)
        XCTAssertEqual(try repository.entry(id: entry.id)?.leaseOwner, "expired-owner")
        XCTAssertNil(try repository.recent())
    }

    func testBackgroundCompletionRejectsAReclaimedLeaseOwner() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "b", count: 43))
        try activate(repository, identity)
        let start = Date(timeIntervalSince1970: 2_700)
        let entry = try repository.enqueue(url: "https://example.org/background", identity: identity, now: start)
        XCTAssertTrue(try repository.claim(id: entry.id, owner: "old-owner", now: start, leaseDuration: 1))
        XCTAssertTrue(try repository.claim(id: entry.id, owner: "new-owner", now: start.addingTimeInterval(2), leaseDuration: 10))

        let response = HTTPURLResponse(
            url: URL(string: "https://example.org/api/links")!,
            statusCode: 202,
            httpVersion: "HTTP/1.1",
            headerFields: ["X-WebTag-Data-Namespace": identity.namespace]
        )!
        let result = BackgroundUploadResult(
            queueID: entry.id,
            owner: "old-owner",
            data: Data(#"{"link_id":"88888888-8888-8888-8888-888888888888","status":"done"}"#.utf8),
            response: response,
            error: nil
        )

        let wakeScheduler = RecordingWakeScheduler()
        let coordinator = ShareSubmissionCoordinator(repository: repository, wakeScheduler: wakeScheduler)
        await coordinator.handleBackgroundCompletion(result, now: start.addingTimeInterval(3))

        XCTAssertEqual(try repository.entry(id: entry.id)?.leaseOwner, "new-owner")
        XCTAssertNil(try repository.recent())
        // A callback that lost its claim still recomputes what is due, so the
        // rejection cannot leave the queue without an alarm. The row is held by
        // a live lease, so that lease's expiry is the next thing to wake for.
        XCTAssertEqual(wakeScheduler.deadlines.count, 1)
        XCTAssertEqual(wakeScheduler.deadlines.last!, start.addingTimeInterval(12))
    }

    func testRetryableForegroundFailurePersistsDeadlineBeforeAnyBackgroundWake() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let namespace = String(repeating: "h", count: 43)
        let identity = SessionIdentity(origin: "https://example.org", namespace: namespace, representationContract: "v3")
        let repository = try AppGroupQueueRepository(containerURL: directory)
        try repository.activate(session: identity)
        let credentials = StaticCredentialStore(
            config: CredentialConfig(identity: identity, installationToken: "test-token")
        )

        WebTagURLProtocol.requestCount = 0
        WebTagURLProtocol.reply = { _ in
            WebTagURLProtocol.Reply(
                status: 503,
                headers: ["X-WebTag-Data-Namespace": namespace],
                body: Data(#"{"error":{"error_code":"internal_error"}}"#.utf8)
            )
        }
        defer {
            WebTagURLProtocol.reply = nil
            WebTagURLProtocol.requestCount = 0
        }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [WebTagURLProtocol.self]
        let api = WebTagAPIClient(session: URLSession(configuration: configuration))
        let scheduler = RecordingUploadScheduler()
        let wakeScheduler = RecordingWakeScheduler()
        let coordinator = ShareSubmissionCoordinator(
            repository: repository,
            credentials: credentials,
            api: api,
            background: scheduler,
            wakeScheduler: wakeScheduler
        )

        let outcome = await coordinator.submit(
            url: "https://example.org/foreground-first",
            identity: identity,
            now: Date(timeIntervalSince1970: 8_000)
        )

        guard case .queued(.server, let deadline) = outcome else {
            XCTFail("expected retryable foreground failure to persist a retry deadline")
            return
        }
        XCTAssertEqual(WebTagURLProtocol.requestCount, 1)
        let entry = try XCTUnwrap(try repository.list().first)
        XCTAssertEqual(entry.state, .retryWait)
        XCTAssertEqual(entry.nextAttemptAt, deadline)
        XCTAssertNil(entry.leaseOwner)
        XCTAssertNil(scheduler.scheduledEntry)
        XCTAssertEqual(wakeScheduler.deadlines.last!, deadline)
    }

    func testResponseLossReplaysWithTheSameIdempotencyKeyAndCommitsOnce() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let namespace = String(repeating: "p", count: 43)
        let identity = SessionIdentity(origin: "https://example.org", namespace: namespace, representationContract: "v3")
        let repository = try AppGroupQueueRepository(containerURL: directory)
        try repository.activate(session: identity)
        let credentials = StaticCredentialStore(
            config: CredentialConfig(identity: identity, installationToken: "test-token")
        )

        var requestKeys: [String] = []
        WebTagURLProtocol.requestCount = 0
        WebTagURLProtocol.failNextRequest = true
        WebTagURLProtocol.failure = nil
        WebTagURLProtocol.requestObserver = { request in
            requestKeys.append(request.value(forHTTPHeaderField: "Idempotency-Key") ?? "")
        }
        WebTagURLProtocol.reply = { _ in
            WebTagURLProtocol.Reply(
                status: 202,
                headers: ["X-WebTag-Data-Namespace": namespace],
                body: Data(#"{"link_id":"55555555-5555-5555-5555-555555555555","status":"pending"}"#.utf8)
            )
        }
        defer {
            WebTagURLProtocol.reply = nil
            WebTagURLProtocol.failNextRequest = false
            WebTagURLProtocol.requestObserver = nil
            WebTagURLProtocol.requestCount = 0
        }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [WebTagURLProtocol.self]
        let api = WebTagAPIClient(session: URLSession(configuration: configuration))
        let coordinator = ShareSubmissionCoordinator(
            repository: repository,
            credentials: credentials,
            api: api,
            wakeScheduler: RecordingWakeScheduler()
        )
        let start = Date(timeIntervalSince1970: 8_500)

        let first = await coordinator.submit(url: "https://example.org/response-loss", identity: identity, now: start)
        guard case .queued = first else {
            XCTFail("expected the lost response to remain retryable")
            return
        }

        let drained = await coordinator.drainOne(now: start.addingTimeInterval(120))
        XCTAssertTrue(drained)
        XCTAssertEqual(requestKeys.count, 2)
        XCTAssertEqual(requestKeys[0], requestKeys[1])
        let entry = try XCTUnwrap(try repository.recent(identity: QueueIdentity(origin: identity.origin, namespace: identity.namespace)))
        XCTAssertTrue(try repository.list().isEmpty)
        XCTAssertEqual(entry.linkID, "55555555-5555-5555-5555-555555555555")
        XCTAssertFalse(requestKeys[0].isEmpty)
    }

    func testRecentIdentityMismatchIsRedactedButDeletable() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let storedIdentity = QueueIdentity(origin: "https://old.example", namespace: String(repeating: "o", count: 43))
        let activeIdentity = QueueIdentity(origin: "https://new.example", namespace: String(repeating: "n", count: 43))
        try activate(repository, storedIdentity)
        let entry = try repository.enqueue(url: "https://old.example/private", identity: storedIdentity)
        XCTAssertTrue(try repository.claim(id: entry.id, owner: "owner"))
        let claimed = try XCTUnwrap(try repository.entry(id: entry.id))
        try repository.finishSuccess(
            entry: claimed,
            owner: "owner",
            response: SubmitResponse(linkID: "22222222-2222-2222-2222-222222222222", status: "done", jobID: nil)
        )
        try activate(repository, activeIdentity)
        let redacted = try XCTUnwrap(try repository.recent(identity: activeIdentity))
        XCTAssertTrue(redacted.isIdentityMismatch)
        XCTAssertEqual(redacted.url, "")
        XCTAssertEqual(redacted.linkID, "")
        try repository.clearRecent()
        XCTAssertNil(try repository.recent())
    }

    func testResetForRetryRejectsPendingAndIdentityStates() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "t", count: 43))
        try activate(repository, identity)
        let now = Date(timeIntervalSince1970: 3_000)

        let pending = try repository.enqueue(url: "https://example.org/pending", identity: identity, now: now)
        XCTAssertFalse(try repository.resetForRetry(id: pending.id, now: now))

        let identityEntry = try repository.enqueue(url: "https://example.org/identity", identity: identity, now: now)
        XCTAssertTrue(try repository.claim(id: identityEntry.id, owner: "identity-owner", now: now))
        let claimedIdentityEntry = try XCTUnwrap(try repository.entry(id: identityEntry.id))
        try repository.applyFailure(
            entry: claimedIdentityEntry,
            owner: "identity-owner",
            state: .blockedIdentity,
            category: .identityMismatch,
            errorCode: nil,
            status: nil,
            nextAttemptAt: nil,
            firstFailedAt: now,
            now: now
        )
        XCTAssertFalse(try repository.resetForRetry(id: identityEntry.id, now: now))

        let retryEntry = try repository.enqueue(url: "https://example.org/retry", identity: identity, now: now)
        XCTAssertTrue(try repository.claim(id: retryEntry.id, owner: "retry-owner", now: now))
        let claimedRetryEntry = try XCTUnwrap(try repository.entry(id: retryEntry.id))
        try repository.applyFailure(
            entry: claimedRetryEntry,
            owner: "retry-owner",
            state: .retryWait,
            category: .server,
            errorCode: nil,
            status: 500,
            nextAttemptAt: now.addingTimeInterval(60),
            firstFailedAt: now,
            now: now
        )
        XCTAssertTrue(try repository.resetForRetry(id: retryEntry.id, now: now))
    }

    func testIdentityMigrationPreservesAuditFieldsAndResetsTransientState() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let oldIdentity = QueueIdentity(origin: "https://old.example", namespace: String(repeating: "o", count: 43))
        let newIdentity = QueueIdentity(origin: "https://new.example", namespace: String(repeating: "n", count: 43))
        try activate(repository, oldIdentity)
        let now = Date(timeIntervalSince1970: 4_000)
        let entry = try repository.enqueue(url: "https://example.org/migrate", identity: oldIdentity, now: now)

        XCTAssertTrue(try repository.claim(id: entry.id, owner: "migration-owner", now: now, leaseDuration: 10))
        let claimed = try XCTUnwrap(try repository.entry(id: entry.id))
        try repository.applyFailure(
            entry: claimed,
            owner: "migration-owner",
            state: .blockedIdentity,
            category: .identityMismatch,
            errorCode: "namespace_changed",
            status: 409,
            nextAttemptAt: now.addingTimeInterval(60),
            firstFailedAt: now.addingTimeInterval(-10),
            now: now.addingTimeInterval(1)
        )
        let before = try XCTUnwrap(try repository.entry(id: entry.id))

        try activate(repository, newIdentity)
        XCTAssertTrue(try repository.migrateIdentity(id: entry.id, to: newIdentity, now: now.addingTimeInterval(2)))

        let migrated = try XCTUnwrap(try repository.entry(id: entry.id))
        XCTAssertEqual(migrated.id, before.id)
        XCTAssertEqual(migrated.createdAt, before.createdAt)
        XCTAssertEqual(migrated.url, before.url)
        XCTAssertEqual(migrated.requestFingerprint, before.requestFingerprint)
        XCTAssertNotEqual(migrated.idempotencyKey, before.idempotencyKey)
        XCTAssertEqual(migrated.identity, newIdentity)
        XCTAssertEqual(migrated.state, .pendingSubmit)
        XCTAssertNil(migrated.firstFailedAt)
        XCTAssertEqual(migrated.attemptCount, 0)
        XCTAssertNil(migrated.nextAttemptAt)
        XCTAssertNil(migrated.lastError)
        XCTAssertNil(migrated.lastErrorCode)
        XCTAssertNil(migrated.lastHTTPStatus)
        XCTAssertNil(migrated.linkID)
        XCTAssertNil(migrated.jobID)
        XCTAssertNil(migrated.leaseOwner)
        XCTAssertNil(migrated.leaseExpiresAt)
    }

    func testIdentityMigrationRejectsActiveLeaseAndSameIdentity() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let oldIdentity = QueueIdentity(origin: "https://old.example", namespace: String(repeating: "l", count: 43))
        let newIdentity = QueueIdentity(origin: "https://new.example", namespace: String(repeating: "m", count: 43))
        try activate(repository, oldIdentity)
        let now = Date(timeIntervalSince1970: 4_500)
        let entry = try repository.enqueue(url: "https://example.org/leased", identity: oldIdentity, now: now)

        XCTAssertTrue(try repository.claim(id: entry.id, owner: "active-owner", now: now, leaseDuration: 10))
        XCTAssertFalse(try repository.migrateIdentity(id: entry.id, to: newIdentity, now: now.addingTimeInterval(1)))
        XCTAssertFalse(try repository.migrateIdentity(id: entry.id, to: oldIdentity, now: now.addingTimeInterval(1)))

        let unchanged = try XCTUnwrap(try repository.entry(id: entry.id))
        XCTAssertEqual(unchanged.identity, oldIdentity)
        XCTAssertEqual(unchanged.leaseOwner, "active-owner")
        XCTAssertEqual(unchanged.url, "https://example.org/leased")
    }

    func testIdentityMatchedCredentialBlocksAutomaticallyReturnToPending() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "i", count: 43))
        try activate(repository, identity)
        let now = Date(timeIntervalSince1970: 4_800)

        for (index, category) in [ErrorCategory.auth, ErrorCategory.forbidden].enumerated() {
            let entry = try repository.enqueue(url: "https://example.org/recovery-\(index)", identity: identity, now: now)
            XCTAssertTrue(try repository.claim(id: entry.id, owner: "recovery-owner-\(index)", now: now))
            let claimed = try XCTUnwrap(try repository.entry(id: entry.id))
            try repository.applyFailure(
                entry: claimed,
                owner: "recovery-owner-\(index)",
                state: .blockedAuth,
                category: category,
                errorCode: nil,
                status: category == .auth ? 401 : 403,
                nextAttemptAt: nil,
                firstFailedAt: now,
                now: now
            )
        }

        XCTAssertEqual(try repository.retryIdentityBlocked(identity: identity, now: now.addingTimeInterval(1)), 2)
        XCTAssertTrue(try repository.list().allSatisfy { $0.state == .pendingSubmit })
    }

    func testBackgroundSubmitResponseMatrixUsesStrict202AndNamespaceGate() {
        let api = WebTagAPIClient()
        let url = URL(string: "https://example.org/api/links")!
        let namespace = String(repeating: "a", count: 43)
        let validBody = Data(#"{"link_id":"33333333-3333-3333-3333-333333333333","status":"pending"}"#.utf8)
        let cases: [(Int, String?, ErrorCategory)] = [
            (200, namespace, .invalidSuccessPayload),
            (202, "wrong", .identityMismatch),
            (301, namespace, .invalidClientResponse),
            (401, namespace, .auth),
            (403, namespace, .forbidden),
            (408, namespace, .http408),
            (425, namespace, .http425),
            (429, namespace, .rateLimit),
            (500, namespace, .server),
        ]
        for (status, responseNamespace, expected) in cases {
            var headers: [String: String] = [:]
            if let responseNamespace { headers["X-WebTag-Data-Namespace"] = responseNamespace }
            let response = HTTPURLResponse(url: url, statusCode: status, httpVersion: "HTTP/1.1", headerFields: headers)!
            let errorCode = status == 429 ? "rate_limit_exceeded" : "request_forbidden"
            let body = status == 401 || status == 403 || status == 429
                ? Data("{\"error\":{\"error_code\":\"\(errorCode)\"}}".utf8)
                : validBody
            let result = api.decodeBackgroundSubmit(data: body, response: response, error: nil, expectedNamespace: namespace)
            guard case .failure(let failure) = result else {
                XCTFail("expected failure for HTTP \(status)")
                continue
            }
            XCTAssertEqual(failure.category, expected, "HTTP \(status)")
        }
    }

    func testTruncated202WithTransportErrorRemainsRetryableButStrongSuccessWins() throws {
        let api = WebTagAPIClient()
        let namespace = String(repeating: "z", count: 43)
        let response = try XCTUnwrap(HTTPURLResponse(
            url: URL(string: "https://example.org/api/links")!,
            statusCode: 202,
            httpVersion: "HTTP/1.1",
            headerFields: ["X-WebTag-Data-Namespace": namespace]
        ))
        let connectionLost = NSError(domain: NSURLErrorDomain, code: NSURLErrorNetworkConnectionLost)
        let truncated = Data(#"{"link_id":"aaaaaaaa-aaaa"#.utf8)
        guard case .failure(let retryable) = api.decodeBackgroundSubmit(
            data: truncated,
            response: response,
            error: connectionLost,
            expectedNamespace: namespace
        ) else {
            return XCTFail("a truncated acknowledgement must not become success")
        }
        XCTAssertEqual(retryable.category, .connectionReset)

        guard case .failure(let malformed) = api.decodeBackgroundSubmit(
            data: truncated,
            response: response,
            error: nil,
            expectedNamespace: namespace
        ) else {
            return XCTFail("malformed 202 without transport evidence must fail")
        }
        XCTAssertEqual(malformed.category, .invalidSuccessPayload)

        let complete = Data(#"{"link_id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa","status":"done"}"#.utf8)
        guard case .success(let success) = api.decodeBackgroundSubmit(
            data: complete,
            response: response,
            error: connectionLost,
            expectedNamespace: namespace
        ) else {
            return XCTFail("complete identity-bound success must win")
        }
        XCTAssertEqual(success.linkID, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")

        let wrongIdentityResponse = try XCTUnwrap(HTTPURLResponse(
            url: response.url!, statusCode: 202, httpVersion: "HTTP/1.1",
            headerFields: ["X-WebTag-Data-Namespace": String(repeating: "x", count: 43)]
        ))
        guard case .failure(let mismatch) = api.decodeBackgroundSubmit(
            data: truncated,
            response: wrongIdentityResponse,
            error: connectionLost,
            expectedNamespace: namespace
        ) else { return XCTFail("identity mismatch must fail closed") }
        XCTAssertEqual(mismatch.category, .identityMismatch)
    }

    func testTruncatedBackground202PersistsRetryDeadlineAndOriginalIdentity() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "y", count: 43))
        let active = try repository.activate(session: activation(identity).identity)
        let start = Date(timeIntervalSince1970: 9_000)
        let queued = try repository.enqueue(url: "https://example.org/ambiguous", identity: identity, now: start)
        XCTAssertTrue(try repository.claim(id: queued.id, owner: "background-owner", now: start))
        let claimed = try XCTUnwrap(try repository.entry(id: queued.id))
        let originalKey = claimed.idempotencyKey
        let response = try XCTUnwrap(HTTPURLResponse(
            url: URL(string: "https://example.org/api/links")!, statusCode: 202,
            httpVersion: "HTTP/1.1", headerFields: ["X-WebTag-Data-Namespace": identity.namespace]
        ))
        let wake = RecordingWakeScheduler()
        let coordinator = ShareSubmissionCoordinator(repository: repository, wakeScheduler: wake)

        await coordinator.handleBackgroundCompletion(BackgroundUploadResult(
            queueID: queued.id,
            owner: "background-owner",
            identityRevision: active.revision,
            data: Data(#"{"link_id":"partial"#.utf8),
            response: response,
            error: NSError(domain: NSURLErrorDomain, code: NSURLErrorNetworkConnectionLost)
        ), now: start.addingTimeInterval(1))

        let retry = try XCTUnwrap(try repository.entry(id: queued.id))
        XCTAssertEqual(retry.id, queued.id)
        XCTAssertEqual(retry.url, queued.url)
        XCTAssertEqual(retry.requestFingerprint, queued.requestFingerprint)
        XCTAssertEqual(retry.state, .retryWait)
        XCTAssertEqual(retry.lastError, .connectionReset)
        XCTAssertEqual(retry.firstFailedAt, start.addingTimeInterval(1))
        XCTAssertEqual(retry.attemptCount, 1)
        XCTAssertNil(retry.leaseOwner)
        XCTAssertEqual(retry.idempotencyKey, originalKey)
        let deadline = try XCTUnwrap(retry.nextAttemptAt)
        XCTAssertGreaterThan(deadline, start.addingTimeInterval(1))
        XCTAssertEqual(wake.deadlines.last!, deadline)
        XCTAssertTrue(try repository.due(now: deadline.addingTimeInterval(-0.001)).isEmpty)
        XCTAssertEqual(try repository.due(now: deadline).map(\.id), [queued.id])

        let responseBody = Data(#"{"link_id":"abababab-abab-abab-abab-abababababab","status":"done"}"#.utf8)
        let replayTransport = RecordingTransport { request in
            let response = HTTPURLResponse(
                url: try XCTUnwrap(request.url),
                statusCode: 202,
                httpVersion: "HTTP/1.1",
                headerFields: ["X-WebTag-Data-Namespace": identity.namespace]
            )!
            return (responseBody, response)
        }
        let credentials = StaticCredentialStore(
            config: CredentialConfig(activation: active, installationToken: "token")
        )
        let replayAPI = WebTagAPIClient(transport: replayTransport)
        let secondRepository = try AppGroupQueueRepository(containerURL: directory)
        let firstReplay = ShareSubmissionCoordinator(
            repository: repository,
            credentials: credentials,
            api: replayAPI,
            wakeScheduler: RecordingWakeScheduler()
        )
        let secondReplay = ShareSubmissionCoordinator(
            repository: secondRepository,
            credentials: credentials,
            api: replayAPI,
            wakeScheduler: RecordingWakeScheduler()
        )
        let beforeDeadline = await firstReplay.drainOne(now: deadline.addingTimeInterval(-0.001))
        XCTAssertFalse(beforeDeadline)
        XCTAssertTrue(replayTransport.requests.isEmpty)

        async let firstDrain = firstReplay.drainOne(now: deadline)
        async let secondDrain = secondReplay.drainOne(now: deadline)
        let (firstDrainResult, secondDrainResult) = await (firstDrain, secondDrain)
        let drainResults = [firstDrainResult, secondDrainResult]

        XCTAssertEqual(drainResults.filter { $0 }.count, 1)
        XCTAssertEqual(replayTransport.requests.count, 1)
        let replayRequest = try XCTUnwrap(replayTransport.requests.first)
        XCTAssertEqual(replayRequest.value(forHTTPHeaderField: "Idempotency-Key"), originalKey)
        XCTAssertEqual(
            try XCTUnwrap(JSONSerialization.jsonObject(with: try XCTUnwrap(replayRequest.httpBody)) as? [String: String]),
            ["url": queued.url]
        )
        XCTAssertNil(try repository.entry(id: queued.id))
        XCTAssertEqual(try repository.recent(identity: identity)?.linkID, "abababab-abab-abab-abab-abababababab")
    }

    func testSubmitUsesStableIdempotencyHeaderAndMinimalBody() async throws {
        let namespace = String(repeating: "s", count: 43)
        let token = "test-token"
        let url = URL(string: "https://example.org/api/links")!
        let transport = RecordingTransport { (_: URLRequest) -> (Data, URLResponse) in
            let response: URLResponse = HTTPURLResponse(
                url: url,
                statusCode: 202,
                httpVersion: "HTTP/1.1",
                headerFields: ["X-WebTag-Data-Namespace": namespace]
            )!
            return (Data(#"{"link_id":"44444444-4444-4444-4444-444444444444","status":"done"}"#.utf8), response)
        }
        let api = WebTagAPIClient(transport: transport)
        let identity = SessionIdentity(origin: "https://example.org", namespace: namespace, representationContract: "v3")
        let result = await api.submit(identity: identity, installationToken: token, url: "https://example.org/article", idempotencyKey: "stable-key")
        guard case .success(let response) = result else {
            XCTFail("expected successful submit")
            return
        }
        XCTAssertEqual(response.status, "done")
        // Asserted after the fact rather than inside a reply closure: an
        // assertion that only runs if the closure runs cannot tell a wrong
        // request apart from a request that was never sent.
        XCTAssertEqual(transport.requests.count, 1)
        let request = try XCTUnwrap(transport.requests.first)
        XCTAssertEqual(request.url, url)
        XCTAssertEqual(request.httpMethod, "POST")
        XCTAssertEqual(request.value(forHTTPHeaderField: "Authorization"), "Bearer \(token)")
        XCTAssertEqual(request.value(forHTTPHeaderField: "Idempotency-Key"), "stable-key")
        XCTAssertEqual(request.value(forHTTPHeaderField: "Content-Type"), "application/json")
        XCTAssertEqual(request.value(forHTTPHeaderField: "Accept"), "application/json")
        let body = try XCTUnwrap(request.httpBody)
        let object = try XCTUnwrap(try JSONSerialization.jsonObject(with: body) as? [String: String])
        XCTAssertEqual(object, ["url": "https://example.org/article"])
    }

    func testForegroundSubmitResponseMatrixUsesStrict202AndNamespaceGate() async {
        let namespace = String(repeating: "m", count: 43)
        let identity = SessionIdentity(origin: "https://example.org", namespace: namespace, representationContract: "v3")
        let validBody = "{\"link_id\":\"77777777-7777-7777-7777-777777777777\",\"status\":\"pending\"}"
        let cases: [(Int, String?, String, ErrorCategory)] = [
            (200, namespace, validBody, .invalidSuccessPayload),
            (202, "wrong", validBody, .identityMismatch),
            (202, namespace, "not-json", .invalidSuccessPayload),
            (301, namespace, validBody, .invalidClientResponse),
            (401, namespace, "{\"error\":{\"error_code\":\"invalid_token\"}}", .auth),
            (403, namespace, "{\"error\":{\"error_code\":\"request_forbidden\"}}", .forbidden),
            (429, namespace, "{\"error\":{\"error_code\":\"rate_limit_exceeded\"}}", .rateLimit),
            (500, namespace, validBody, .server),
        ]
        defer {
            WebTagURLProtocol.reply = nil
            WebTagURLProtocol.failure = nil
        }
        for (status, responseNamespace, body, expected) in cases {
            WebTagURLProtocol.failure = nil
            WebTagURLProtocol.reply = { _ in
                var headers: [String: String] = [:]
                if let responseNamespace { headers["X-WebTag-Data-Namespace"] = responseNamespace }
                return WebTagURLProtocol.Reply(status: status, headers: headers, body: Data(body.utf8))
            }
            let configuration = URLSessionConfiguration.ephemeral
            configuration.protocolClasses = [WebTagURLProtocol.self]
            let api = WebTagAPIClient(session: URLSession(configuration: configuration))
            let result = await api.submit(identity: identity, installationToken: "test-token", url: "https://example.org/article", idempotencyKey: "matrix-key")
            guard case .failure(let failure) = result else {
                XCTFail("expected failure for HTTP \(status)")
                continue
            }
            XCTAssertEqual(failure.category, expected, "HTTP \(status)")
        }
    }

    func testForegroundSubmitSeparatesTimeoutAndTLSTransportFailures() async {
        let namespace = String(repeating: "t", count: 43)
        let identity = SessionIdentity(origin: "https://example.org", namespace: namespace, representationContract: "v3")
        let errors: [(Int, ErrorCategory)] = [
            (NSURLErrorTimedOut, .clientDeadline),
            (NSURLErrorSecureConnectionFailed, .tlsTrustFailure),
        ]
        defer { WebTagURLProtocol.failure = nil }
        for (code, expected) in errors {
            WebTagURLProtocol.reply = nil
            WebTagURLProtocol.failure = NSError(domain: NSURLErrorDomain, code: code)
            let configuration = URLSessionConfiguration.ephemeral
            configuration.protocolClasses = [WebTagURLProtocol.self]
            let api = WebTagAPIClient(session: URLSession(configuration: configuration))
            let result = await api.submit(identity: identity, installationToken: "test-token", url: "https://example.org/article", idempotencyKey: "transport-key")
            guard case .failure(let failure) = result else {
                XCTFail("expected transport failure")
                continue
            }
            XCTAssertEqual(failure.category, expected)
        }
    }

    func testRefreshRejectsNonUUIDBeforeCreatingARequest() async {
        let namespace = String(repeating: "q", count: 43)
        let identity = SessionIdentity(origin: "https://example.org", namespace: namespace, representationContract: "v3")
        let result = await WebTagAPIClient().refresh(identity: identity, installationToken: "test-token", linkID: "not-a-uuid")
        guard case .failure(let failure) = result else {
            XCTFail("expected invalid link ID")
            return
        }
        XCTAssertEqual(failure.category, .invalidClientResponse)
    }

    func testRefreshRejectsAResponseForADifferentLink() async {
        let namespace = String(repeating: "w", count: 43)
        let identity = SessionIdentity(origin: "https://example.org", namespace: namespace, representationContract: "v3")
        WebTagURLProtocol.reply = { _ in
            WebTagURLProtocol.Reply(
                status: 202,
                headers: ["X-WebTag-Data-Namespace": namespace],
                body: Data(#"{"link_id":"22222222-2222-2222-2222-222222222222","status":"processing"}"#.utf8)
            )
        }
        defer { WebTagURLProtocol.reply = nil }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [WebTagURLProtocol.self]
        let api = WebTagAPIClient(session: URLSession(configuration: configuration))

        let result = await api.refresh(
            identity: identity,
            installationToken: "test-token",
            linkID: "11111111-1111-1111-1111-111111111111"
        )

        guard case .failure(let failure) = result else {
            XCTFail("expected mismatched refresh identifier to be rejected")
            return
        }
        XCTAssertEqual(failure.category, .invalidSuccessPayload)
    }

    func testSessionRejectsInvalidNamespaceCharacters() async {
        let namespace = String(repeating: "n", count: 42) + "!"
        WebTagURLProtocol.reply = { _ in
            WebTagURLProtocol.Reply(
                status: 200,
                headers: ["X-WebTag-Data-Namespace": namespace],
                body: Data("{\"client_data_namespace\":\"\(namespace)\",\"representation_contract\":\"v3\"}".utf8)
            )
        }
        defer { WebTagURLProtocol.reply = nil }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [WebTagURLProtocol.self]
        let api = WebTagAPIClient(session: URLSession(configuration: configuration))

        let result = await api.validateSession(origin: "https://example.org", installationToken: "test-token")
        guard case .failure(let failure) = result else {
            XCTFail("expected invalid namespace")
            return
        }
        XCTAssertEqual(failure.category, .invalidSuccessPayload)
    }

    func testSessionNormalizesOriginBeforeRequestAndIdentityCreation() async {
        let namespace = String(repeating: "o", count: 43)
        WebTagURLProtocol.reply = { request in
            XCTAssertEqual(request.url?.absoluteString, "https://example.org/api/session")
            return WebTagURLProtocol.Reply(
                status: 200,
                headers: ["X-WebTag-Data-Namespace": namespace],
                body: Data("{\"client_data_namespace\":\"\(namespace)\",\"representation_contract\":\"v3\"}".utf8)
            )
        }
        defer { WebTagURLProtocol.reply = nil }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [WebTagURLProtocol.self]
        let api = WebTagAPIClient(session: URLSession(configuration: configuration))

        let result = await api.validateSession(origin: " HTTPS://Example.ORG/ ", installationToken: "test-token")
        guard case .success(let identity) = result else {
            XCTFail("expected normalized session identity")
            return
        }
        XCTAssertEqual(identity.origin, "https://example.org")
    }

    func testSessionRejectsBlankInstallationTokenBeforeCreatingARequest() async {
        WebTagURLProtocol.requestCount = 0
        WebTagURLProtocol.reply = { _ in
            XCTFail("blank installation token must not create a request")
            return WebTagURLProtocol.Reply(status: 200, headers: [:], body: Data())
        }
        defer {
            WebTagURLProtocol.reply = nil
            WebTagURLProtocol.requestCount = 0
        }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [WebTagURLProtocol.self]
        let api = WebTagAPIClient(session: URLSession(configuration: configuration))

        let result = await api.validateSession(origin: "https://example.org", installationToken: " \t")

        guard case .failure(let failure) = result else {
            XCTFail("expected blank installation token failure")
            return
        }
        XCTAssertEqual(failure.category, .invalidClientResponse)
        XCTAssertEqual(WebTagURLProtocol.requestCount, 0)
    }

    func testSubmitRejectsBlankIdempotencyKeyAndNonCanonicalOriginBeforeCreatingARequest() async {
        let namespace = String(repeating: "g", count: 43)
        WebTagURLProtocol.requestCount = 0
        WebTagURLProtocol.reply = { _ in
            XCTFail("invalid submit arguments must not create a request")
            return WebTagURLProtocol.Reply(status: 202, headers: [:], body: Data())
        }
        defer {
            WebTagURLProtocol.reply = nil
            WebTagURLProtocol.requestCount = 0
        }
        let configuration = URLSessionConfiguration.ephemeral
        configuration.protocolClasses = [WebTagURLProtocol.self]
        let api = WebTagAPIClient(session: URLSession(configuration: configuration))
        let identity = SessionIdentity(origin: "https://example.org", namespace: namespace, representationContract: "v3")

        let blankKey = await api.submit(identity: identity, installationToken: "test-token", url: "https://example.org/a", idempotencyKey: " ")
        let invalidOrigin = await api.submit(
            identity: SessionIdentity(origin: "https://example.org/path", namespace: namespace, representationContract: "v3"),
            installationToken: "test-token",
            url: "https://example.org/a",
            idempotencyKey: "stable-key"
        )

        guard case .failure(let blankFailure) = blankKey,
              case .failure(let originFailure) = invalidOrigin else {
            XCTFail("expected invalid submit arguments")
            return
        }
        XCTAssertEqual(blankFailure.category, .invalidClientResponse)
        XCTAssertEqual(originFailure.category, .invalidClientResponse)
        XCTAssertEqual(WebTagURLProtocol.requestCount, 0)
    }

    func testBackgroundSubmitRejectsNonCanonicalResponseIdentifier() {
        let namespace = String(repeating: "c", count: 43)
        let url = URL(string: "https://example.org/api/links")!
        let response = HTTPURLResponse(
            url: url,
            statusCode: 202,
            httpVersion: "HTTP/1.1",
            headerFields: ["X-WebTag-Data-Namespace": namespace]
        )!
        let result = WebTagAPIClient().decodeBackgroundSubmit(
            data: Data("{\"link_id\":\"not-a-uuid\",\"status\":\"pending\"}".utf8),
            response: response,
            error: nil,
            expectedNamespace: namespace
        )
        guard case .failure(let failure) = result else {
            XCTFail("expected invalid response identifier")
            return
        }
        XCTAssertEqual(failure.category, .invalidSuccessPayload)
    }

    // MARK: - Item provider collection

    private func submissionValues(fromSharedText text: String) async -> [String] {
        let provider = FakeItemProvider([(.plainText, .immediate(text))])
        let run = ShareCandidateCollector.start(items: [[provider]], clock: FakeMonotonicClock())
        return await run.value().candidates.map(\.submissionValue)
    }

    func testCollectorStartsEveryDeclaredRepresentationBeforeTheFirstAwait() {
        let clock = FakeMonotonicClock()
        // A start instant that is neither zero nor the budget, so an arming bug
        // cannot coincide with the right answer.
        clock.advance(to: 5)
        let first = FakeItemProvider([(.url, .deferred), (.plainText, .deferred)])
        let second = FakeItemProvider([(.plainText, .deferred)])
        let third = FakeItemProvider([(.url, .deferred)])

        let run = ShareCandidateCollector.start(items: [[first, second], [third]], clock: clock)

        // Nothing has been awaited yet, so every load must already be in flight.
        XCTAssertEqual(first.startedKinds, [.url, .plainText])
        XCTAssertEqual(second.startedKinds, [.plainText])
        XCTAssertEqual(third.startedKinds, [.url])
        XCTAssertEqual(
            run.startedRequests,
            [
                ShareRepresentationRequest(itemIndex: 0, attachmentIndex: 0, kind: .url),
                ShareRepresentationRequest(itemIndex: 0, attachmentIndex: 0, kind: .plainText),
                ShareRepresentationRequest(itemIndex: 0, attachmentIndex: 1, kind: .plainText),
                ShareRepresentationRequest(itemIndex: 1, attachmentIndex: 0, kind: .url),
            ]
        )
        // Four loads in flight, one timer: the budget belongs to the collection
        // and not to each representation. Arming it per slot would leave four
        // entries here even though they all fall due at the same instant.
        XCTAssertEqual(clock.armedDeadlines, [run.startedAt + ShareCandidateCollector.collectionBudget])
        XCTAssertEqual(run.startedAt, 5)
    }

    func testCollectorStillCollectsLaterProvidersWhenTheFirstNeverAnswers() async {
        let clock = FakeMonotonicClock()
        let stuck = FakeItemProvider([(.url, .deferred)])
        let quick = FakeItemProvider([(.plainText, .immediate("read https://example.org/second"))])

        let run = ShareCandidateCollector.start(items: [[stuck, quick]], clock: clock)
        clock.advance(to: ShareCandidateCollector.collectionBudget)
        let collection = await run.value()

        XCTAssertEqual(collection.candidates.map(\.submissionValue), ["https://example.org/second"])
        XCTAssertTrue(collection.reachedDeadline)
        XCTAssertEqual(
            collection.completedRequests,
            [ShareRepresentationRequest(itemIndex: 0, attachmentIndex: 1, kind: .plainText)]
        )
        // The wait ended on the one deadline the collector armed for the whole
        // run, not on a second per-item one. Asserted on what was armed rather
        // than on where the clock ended up: the test moved the clock itself, so
        // its position proves nothing about the collector.
        XCTAssertEqual(clock.armedDeadlines, [run.startedAt + ShareCandidateCollector.collectionBudget])
    }

    func testCollectorCancelsOnlyTheLoadsStillRunningAtTheDeadline() async {
        let clock = FakeMonotonicClock()
        let stuck = FakeItemProvider([(.url, .deferred)])
        let quick = FakeItemProvider([(.plainText, .immediate("https://example.org/done"))])

        let run = ShareCandidateCollector.start(items: [[stuck, quick]], clock: clock)
        clock.advance(to: ShareCandidateCollector.collectionBudget)
        _ = await run.value()

        XCTAssertEqual(stuck.cancellations[.url]?.cancelCount, 1)
        XCTAssertEqual(quick.cancellations[.plainText]?.cancelCount, 0)
    }

    func testCollectorLoadsBothRepresentationsOfOneProviderWithoutSuppression() async {
        let provider = FakeItemProvider([
            (.url, .immediate("https://example.org/structured")),
            (.plainText, .immediate("also see https://example.org/from-text")),
        ])

        let run = ShareCandidateCollector.start(items: [[provider]], clock: FakeMonotonicClock())
        let collection = await run.value()

        XCTAssertEqual(
            collection.candidates.map(\.submissionValue),
            ["https://example.org/structured", "https://example.org/from-text"]
        )
        XCTAssertFalse(collection.reachedDeadline)
    }

    func testCollectorKeepsTheTextLinkWhenTheURLRepresentationIsNotHTTP() async {
        let provider = FakeItemProvider([
            (.url, .immediate("ftp://example.org/file")),
            (.plainText, .immediate("mirror at https://example.org/file")),
        ])

        let run = ShareCandidateCollector.start(items: [[provider]], clock: FakeMonotonicClock())
        let collection = await run.value()

        XCTAssertEqual(collection.candidates.map(\.submissionValue), ["https://example.org/file"])
    }

    func testCollectorOutputFollowsInputOrderWhenCallbacksCompleteInReverse() async {
        let first = FakeItemProvider([(.url, .deferred), (.plainText, .deferred)])
        let second = FakeItemProvider([(.plainText, .deferred)])
        let third = FakeItemProvider([(.url, .deferred)])
        let run = ShareCandidateCollector.start(items: [[first, second], [third]], clock: FakeMonotonicClock())

        third.complete(.url, with: "https://example.org/fourth")
        second.complete(.plainText, with: "see https://example.org/third")
        first.complete(.plainText, with: "see https://example.org/second")
        first.complete(.url, with: "https://example.org/first")
        let collection = await run.value()

        XCTAssertEqual(
            collection.candidates.map(\.submissionValue),
            [
                "https://example.org/first",
                "https://example.org/fourth",
                "https://example.org/second",
                "https://example.org/third",
            ]
        )
        XCTAssertFalse(collection.reachedDeadline)
        XCTAssertEqual(collection.completedRequests, run.startedRequests)
    }

    func testCollectorTakesResultsUpToTheDeadlineAndIgnoresLateOrRepeatedOnes() async {
        let clock = FakeMonotonicClock()
        let early = FakeItemProvider([(.url, .deferred)])
        let late = FakeItemProvider([(.url, .deferred)])
        let run = ShareCandidateCollector.start(items: [[early, late]], clock: clock)

        clock.advance(to: ShareCandidateCollector.collectionBudget - 0.001)
        early.complete(.url, with: "https://example.org/in-time")
        clock.advance(to: ShareCandidateCollector.collectionBudget)
        // Both of these are late: the deadline already decided the run. The
        // first is stopped by `isDecided`, the second by the slot's own closed
        // flag - neither reaches the gate, and neither may add a candidate.
        late.complete(.url, with: "https://example.org/too-late")
        late.complete(.url, with: "https://example.org/too-late")
        let collection = await run.value()

        XCTAssertEqual(collection.candidates.map(\.submissionValue), ["https://example.org/in-time"])
        XCTAssertTrue(collection.reachedDeadline)
        XCTAssertEqual(
            collection.completedRequests,
            [ShareRepresentationRequest(itemIndex: 0, attachmentIndex: 0, kind: .url)]
        )
    }

    func testCollectorRejectsALateCallbackWhenDeadlineTimerDeliveryIsDelayed() async {
        let clock = FakeMonotonicClock()
        let provider = FakeItemProvider([(.url, .deferred)])
        let run = ShareCandidateCollector.start(items: [[provider]], clock: clock)

        clock.elapseWithoutFiring(to: ShareCandidateCollector.collectionBudget)
        provider.complete(.url, with: "https://example.org/too-late")
        clock.advance(to: ShareCandidateCollector.collectionBudget)
        let collection = await run.value()

        XCTAssertEqual(collection.candidates, [])
        XCTAssertTrue(collection.reachedDeadline)
        XCTAssertEqual(collection.completedRequests, [])
    }

    func testCollectorAcceptsOnlyExplicitHTTPSubstringsOfTheOriginalText() async {
        var values = await submissionValues(fromSharedText: "visit example.org or www.example.net today")
        XCTAssertEqual(values, [], "a detector-synthesised scheme is not something the user shared")

        values = await submissionValues(fromSharedText: "write to someone@example.org or dial tel:+1-555-0100")
        XCTAssertEqual(values, [])

        values = await submissionValues(fromSharedText: "(https://example.org/a) and https://example.org/b.")
        XCTAssertEqual(values, ["https://example.org/a", "https://example.org/b"])

        values = await submissionValues(fromSharedText: "参考 https://example.org/guide 谢谢")
        XCTAssertEqual(values, ["https://example.org/guide"])

        values = await submissionValues(fromSharedText: "https://one.example.org/1 then https://two.example.org/2")
        XCTAssertEqual(values, ["https://one.example.org/1", "https://two.example.org/2"])

        // Split, because one assertion over both would stay green if only one of
        // them were rejected - and would not say which.
        values = await submissionValues(fromSharedText: "broken https:///no-host here")
        XCTAssertEqual(values, [], "a URL without a host must never reach submission")

        // The scanner does hand this one on; it is candidate validation that
        // throws it out. Without this the assertion below could pass merely
        // because the detector never matched it in the first place.
        XCTAssertFalse(
            ShareTextLinkScanner.explicitHTTPSubstrings(in: "login at https://user:pass@example.org/x here").isEmpty,
            "the detector must reach this text for the rejection below to mean anything"
        )
        values = await submissionValues(fromSharedText: "login at https://user:pass@example.org/x here")
        XCTAssertEqual(values, [], "embedded credentials must never reach submission")

        values = await submissionValues(fromSharedText: "HTTPS://Example.org/Path is fine")
        XCTAssertEqual(values, ["HTTPS://Example.org/Path"], "the shared casing is what gets submitted")
    }

    func testCollectorDeduplicationKeepsTheFirstSubmissionString() async {
        let provider = FakeItemProvider([
            (.url, .immediate("https://Example.org/Article?ref=Share")),
            (.plainText, .immediate("again: https://example.org/Article?ref=Share")),
        ])

        let run = ShareCandidateCollector.start(items: [[provider]], clock: FakeMonotonicClock())
        let collection = await run.value()

        XCTAssertEqual(collection.candidates.map(\.submissionValue), ["https://Example.org/Article?ref=Share"])
    }

    func testDisplayLabelShowsHostAndPathOnly() {
        XCTAssertEqual(URLDisplayLabel.of("https://Example.ORG/Path%20A?token=secret#frag"), "example.org/Path%20A")
        XCTAssertEqual(URLDisplayLabel.of("https://example.org"), "example.org/")
        XCTAssertEqual(URLDisplayLabel.of("https://example.org:443/a"), "example.org/a")
        XCTAssertEqual(URLDisplayLabel.of("http://example.org:80/a"), "example.org/a")
        XCTAssertEqual(URLDisplayLabel.of("https://example.org:8443/a"), "example.org:8443/a")
        XCTAssertEqual(URLDisplayLabel.of("http://[2001:db8::1]:8080/a"), "[2001:db8::1]:8080/a")
        XCTAssertEqual(URLDisplayLabel.of("https://[2001:db8::1]"), "[2001:db8::1]/")
        XCTAssertNil(URLDisplayLabel.of("mailto:someone@example.org"))
    }

    func testPresenterResolvesASelectionToItsExactSubmissionValue() {
        // Two candidates that differ only in their query share one label, so a
        // presenter that resolved a choice by label would submit the wrong URL.
        let candidates = [
            URLCandidate(submissionValue: "https://example.org/A?ref=first", displayLabel: "example.org/A"),
            URLCandidate(submissionValue: "https://example.org/A?ref=second", displayLabel: "example.org/A"),
        ]

        XCTAssertTrue(ShareCandidatePresenter.requiresSelection(candidates))
        XCTAssertFalse(ShareCandidatePresenter.requiresSelection([candidates[0]]))
        XCTAssertEqual(ShareCandidatePresenter.displayLabel(candidates, at: 1), "example.org/A")
        XCTAssertEqual(ShareCandidatePresenter.submissionValue(candidates, at: 1), "https://example.org/A?ref=second")
        XCTAssertNil(ShareCandidatePresenter.submissionValue(candidates, at: 2))
        XCTAssertNil(ShareCandidatePresenter.displayLabel(candidates, at: -1))
    }

    func testCandidateSubmissionValueIsNotRebuiltFromItsLabel() async {
        let shared = "https://Example.ORG/Path%20A/x?q=%E4%B8%AD&b=1#frag"
        let provider = FakeItemProvider([(.url, .immediate(shared))])

        let run = ShareCandidateCollector.start(items: [[provider]], clock: FakeMonotonicClock())
        let candidate = await run.value().candidates.first

        XCTAssertEqual(candidate?.submissionValue, shared)
        XCTAssertEqual(candidate?.displayLabel, "example.org/Path%20A/x")
    }

    // MARK: - Share flow

    func testFlowKeepsOneAttachmentListPerItemIncludingTheEmptyOnes() {
        let carrier = FakeItemProvider([(.url, .deferred)])
        let items: [ShareInputItem] = [FakeInputItem([]), FakeInputItem([carrier])]

        let attachments = ShareFlowCoordinator.attachments(of: items)

        XCTAssertEqual(attachments.map(\.count), [0, 1])
        let run = ShareCandidateCollector.start(items: attachments, clock: FakeMonotonicClock())
        // The item that carried nothing still holds index 0, so the loaded one
        // is item 1. Dropping it, or flattening both levels into one, would
        // renumber every attachment behind it.
        XCTAssertEqual(
            run.startedRequests,
            [ShareRepresentationRequest(itemIndex: 1, attachmentIndex: 0, kind: .url)]
        )
    }

    func testFlowMeasuresTheInteractionDeadlineFromWhereCollectionStarted() async {
        let clock = FakeMonotonicClock()
        clock.advance(to: 7)
        let flow = ShareFlowCoordinator(clock: clock)
        let provider = FakeItemProvider([(.url, .deferred)])

        let run = flow.start(items: [[provider]])
        // Collection itself spends 1.5s of the budget. What is left for the
        // foreground request is the remainder, never a fresh budget.
        clock.advance(to: 8.5)
        provider.complete(.url, with: "https://example.org/only")
        let collection = await flow.collect()

        XCTAssertEqual(run.startedAt, 7)
        guard case .automatic(let request) = flow.presentation(for: collection.candidates) else {
            XCTFail("a single candidate is submitted without asking")
            return
        }
        XCTAssertEqual(request.value, "https://example.org/only")
        XCTAssertEqual(request.deadline, 7 + ShareCandidateCollector.interactionBudget)
    }

    func testFlowDropsTheInteractionDeadlineOnceTheUserHasToChoose() async throws {
        let clock = FakeMonotonicClock()
        clock.advance(to: 3)
        let flow = ShareFlowCoordinator(clock: clock)
        let provider = FakeItemProvider([
            (.url, .immediate("https://example.org/first")),
            (.plainText, .immediate("also see https://example.org/second")),
        ])

        flow.start(items: [[provider]])
        let collection = await flow.collect()

        guard case .selection(let candidates) = flow.presentation(for: collection.candidates) else {
            XCTFail("two candidates must be offered to the user")
            return
        }
        XCTAssertEqual(
            candidates.map(\.submissionValue),
            ["https://example.org/first", "https://example.org/second"]
        )
        let chosen = try XCTUnwrap(flow.selection(candidates, at: 1))
        // The value comes from the row, not from its label.
        XCTAssertEqual(chosen.value, "https://example.org/second")
        // Reading time is not part of the budget, and the budget is not reopened
        // for the tap either: the deadline is gone for good.
        XCTAssertNil(chosen.deadline)
        XCTAssertNil(flow.selection(candidates, at: candidates.count))
    }

    func testFlowReportsNoCandidateWhenNothingSharedCarriedALink() async {
        let flow = ShareFlowCoordinator(clock: FakeMonotonicClock())
        let provider = FakeItemProvider([(.plainText, .immediate("nothing to see here"))])

        flow.start(items: [[provider]])
        let collection = await flow.collect()

        XCTAssertEqual(
            flow.presentation(for: collection.candidates),
            ShareFlowCoordinator.Presentation.noCandidate
        )
    }

    func testFlowSubmitsOnceHoweverManyRowsAreTapped() {
        let flow = ShareFlowCoordinator(clock: FakeMonotonicClock())
        let first = ShareFlowCoordinator.SubmissionRequest(value: "https://example.org/a", deadline: nil)
        let second = ShareFlowCoordinator.SubmissionRequest(value: "https://example.org/b", deadline: 9)

        XCTAssertEqual(flow.beginSubmission(first), first)
        // A second tap - on another row or on the same one - must not start a
        // second submission, however fast the finger was.
        XCTAssertNil(flow.beginSubmission(second))
        XCTAssertNil(flow.beginSubmission(first))
    }

    func testTerminalMessageClosesTheSheetOnlyForOutcomesThatAreOver() {
        func message(_ outcome: SubmissionOutcome) -> ShareTerminalMessage.Message {
            ShareTerminalMessage.of(outcome)
        }
        func expected(_ text: String, closesSheet: Bool) -> ShareTerminalMessage.Message {
            ShareTerminalMessage.Message(text: text, completesRequest: closesSheet)
        }
        func submitted(_ status: String) -> SubmissionOutcome {
            .submitted(SubmitResponse(linkID: "9f1c0f1e", status: status, jobID: nil))
        }

        XCTAssertEqual(message(submitted("pending")), expected("已收藏", closesSheet: true))
        XCTAssertEqual(message(submitted("processing")), expected("已收藏", closesSheet: true))
        XCTAssertEqual(message(submitted("done")), expected("已在库中", closesSheet: true))
        XCTAssertEqual(message(submitted("failed")), expected("已在库中，解析失败", closesSheet: true))
        // An unknown status is still a durable row, so the share is over; it
        // just refuses to claim the link was saved.
        XCTAssertEqual(message(submitted("brand-new")), expected("提交失败", closesSheet: true))

        XCTAssertEqual(message(.scheduled), expected("已加入队列", closesSheet: true))
        XCTAssertEqual(message(.queued(.noNetwork, nil)), expected("已加入队列", closesSheet: true))

        // Everything below needs the user to go and change something, so the
        // sheet stays open and says what.
        XCTAssertEqual(message(.blocked(.blockedAuth, .auth)), expected("凭证无效，请检查设置", closesSheet: false))
        XCTAssertEqual(message(.blocked(.blockedAuth, .forbidden)), expected("凭证无效，请检查设置", closesSheet: false))
        XCTAssertEqual(
            message(.blocked(.blockedIdentity, .identityMismatch)),
            expected("身份已变更", closesSheet: false)
        )
        XCTAssertEqual(
            message(.blocked(.failedPermanent, .invalidClientResponse)),
            expected("提交失败", closesSheet: false)
        )
        XCTAssertEqual(
            message(.configurationRequired),
            expected("请先打开 Cairn 完成设置", closesSheet: false)
        )
        XCTAssertEqual(message(.noCandidate), expected("没找到链接", closesSheet: false))
    }

    func testInteractionDeadlineHandsOffTheDurableRowInsteadOfReopeningTheBudget() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let namespace = String(repeating: "d", count: 43)
        let identity = SessionIdentity(origin: "https://example.org", namespace: namespace, representationContract: "v3")
        let repository = try AppGroupQueueRepository(containerURL: directory)
        try repository.activate(session: identity)
        let credentials = StaticCredentialStore(
            config: CredentialConfig(identity: identity, installationToken: "test-token")
        )

        let shared = "https://Example.ORG/Path%20A/x?q=%E4%B8%AD&b=1#frag"
        let clock = FakeMonotonicClock()
        let provider = FakeItemProvider([(.url, .immediate(shared))])
        let run = ShareCandidateCollector.start(items: [[provider]], clock: clock)
        // Hoisted out of XCTUnwrap: its argument is a non-async autoclosure.
        let collected = await run.value()
        let candidate = try XCTUnwrap(collected.candidates.first)
        let interactionDeadline = run.startedAt + ShareCandidateCollector.interactionBudget

        let requestStarted = expectation(description: "the foreground request reached the transport")
        // Accepted and never answered, so the interaction deadline is what ends
        // the foreground attempt - with the request still in flight.
        let transport = RecordingTransport(pauseAfterRequest: true, onRequest: { _ in
            requestStarted.fulfill()
        })
        defer { transport.releaseParked() }
        let scheduler = RecordingUploadScheduler()
        let coordinator = ShareSubmissionCoordinator(
            repository: repository,
            credentials: credentials,
            api: WebTagAPIClient(transport: transport),
            background: scheduler,
            clock: clock,
            wakeScheduler: RecordingWakeScheduler()
        )

        let submission = Task {
            await coordinator.submit(
                url: candidate.submissionValue,
                identity: identity,
                interactionDeadline: interactionDeadline,
                now: Date(timeIntervalSince1970: 9_000)
            )
        }
        await fulfillment(of: [requestStarted], timeout: 5)
        clock.advance(to: interactionDeadline)
        let outcome = await submission.value

        guard case .scheduled = outcome else {
            XCTFail("an exhausted interaction budget must hand off, not fail")
            return
        }
        let sent = transport.requests
        let bodies = try sent.map { request -> [String: String] in
            let body = try XCTUnwrap(request.httpBody)
            return try XCTUnwrap(try JSONSerialization.jsonObject(with: body) as? [String: String])
        }
        XCTAssertEqual(bodies, [["url": shared]])
        XCTAssertEqual(sent.count, 1, "the budget must not be reopened")
        let entry = try XCTUnwrap(try repository.list().first)
        XCTAssertEqual(entry.url, shared)
        XCTAssertEqual(entry.state, .pendingSubmit)
        XCTAssertEqual(scheduler.scheduledEntry?.id, entry.id)
        XCTAssertEqual(scheduler.scheduledOwner, entry.leaseOwner)
    }

    func testBackgroundInventoryUsesExactOwnerAndRenewsOnlyAtThreshold() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "w", count: 43))
        try activate(repository, identity)
        let start = Date(timeIntervalSince1970: 10_000)
        let entry = try repository.enqueue(url: "https://example.org/renew", identity: identity, now: start)
        XCTAssertTrue(try repository.claim(id: entry.id, owner: "owner-1", now: start))
        let firstClaim = BackgroundUploadClaim(queueID: entry.id, owner: "owner-1")

        XCTAssertEqual(
            try repository.reconcileBackgroundClaim(firstClaim, now: start.addingTimeInterval(239)),
            .matched(leaseExpiresAt: start.addingTimeInterval(QueueLeasePolicy.duration), renewed: false)
        )
        XCTAssertEqual(
            try repository.reconcileBackgroundClaim(firstClaim, now: start.addingTimeInterval(240)),
            .matched(leaseExpiresAt: start.addingTimeInterval(540), renewed: true)
        )
        XCTAssertEqual(
            try repository.reconcileBackgroundClaim(firstClaim, now: start.addingTimeInterval(240)),
            .matched(leaseExpiresAt: start.addingTimeInterval(540), renewed: false)
        )

        let reclaimed = try repository.enqueue(url: "https://example.org/reclaimed-owner", identity: identity, now: start)
        XCTAssertTrue(try repository.claim(id: reclaimed.id, owner: "old-owner", now: start, leaseDuration: 1))
        XCTAssertTrue(try repository.claim(id: reclaimed.id, owner: "new-owner", now: start.addingTimeInterval(2)))
        XCTAssertEqual(
            try repository.reconcileBackgroundClaim(BackgroundUploadClaim(queueID: reclaimed.id, owner: "old-owner"), now: start.addingTimeInterval(3)),
            .stale
        )
        XCTAssertEqual(try repository.entry(id: reclaimed.id)?.leaseOwner, "new-owner")
    }

    func testIdentityRevisionRejectsAThenBThenACompletion() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identityA = QueueIdentity(origin: "https://a.example", namespace: String(repeating: "a", count: 43))
        let identityB = QueueIdentity(origin: "https://b.example", namespace: String(repeating: "b", count: 43))
        try activate(repository, identityA)
        let entry = try repository.enqueue(url: "https://example.org/a", identity: identityA)
        XCTAssertTrue(try repository.claim(id: entry.id, owner: "owner-a"))
        let claimed = try XCTUnwrap(try repository.entry(id: entry.id))
        let firstRevision = claimed.identityRevision

        try activate(repository, identityB)
        try activate(repository, identityA)
        let active = try XCTUnwrap(try repository.activeSessionSnapshot())
        XCTAssertGreaterThan(active.revision, firstRevision)
        XCTAssertEqual(
            try repository.finishSuccess(
                entry: claimed,
                owner: "owner-a",
                response: SubmitResponse(linkID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", status: "done", jobID: nil)
            ),
            .identityChanged
        )
        XCTAssertNil(try repository.recent(identity: identityA))
        XCTAssertEqual(try repository.entry(id: entry.id)?.identityRevision, firstRevision)
        XCTAssertTrue(try repository.due().isEmpty)
    }

    func testSameNamespaceActivationFreezesQueueAndRecentUntilExplicitMigration() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "s", count: 43))
        let session = activation(identity).identity
        let first = try repository.activate(session: session)
        let completed = try repository.enqueue(url: "https://example.org/recent-r1", identity: identity)
        XCTAssertTrue(try repository.claim(id: completed.id, owner: "recent-r1"))
        XCTAssertEqual(try repository.finishSuccess(
            entry: try XCTUnwrap(try repository.entry(id: completed.id)),
            owner: "recent-r1",
            response: SubmitResponse(linkID: "cccccccc-cccc-cccc-cccc-cccccccccccc", status: "done", jobID: nil)
        ), .applied)
        let frozen = try repository.enqueue(url: "https://example.org/queue-r1", identity: identity)

        let second = try repository.activate(session: session)

        XCTAssertGreaterThan(second.revision, first.revision)
        XCTAssertTrue(try repository.due().isEmpty)
        XCTAssertEqual(try repository.entry(id: frozen.id)?.identityRevision, first.revision)
        let redacted = try XCTUnwrap(try repository.recent(identity: identity))
        XCTAssertTrue(redacted.isIdentityMismatch)
        XCTAssertEqual(redacted.url, "")

        XCTAssertTrue(try repository.migrateIdentity(id: frozen.id, to: identity))
        XCTAssertEqual(try repository.due().map(\.id), [frozen.id])
        XCTAssertTrue(try repository.migrateRecent(from: first, to: identity))
        let migratedRecent = try XCTUnwrap(try repository.recent(identity: identity))
        XCTAssertFalse(migratedRecent.isIdentityMismatch)
        XCTAssertEqual(migratedRecent.identityRevision, second.revision)
    }

    func testProductMigrationPlanMovesQueueRecentAndTodoOnceAfterConfirmation() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let queue = try AppGroupQueueRepository(containerURL: directory)
        let todo = try CompanionTodoRepository(
            containerURL: directory,
            cipher: CompanionTodoAESGCMCipher(keyData: Data(repeating: 41, count: 32))
        )
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "p", count: 43))
        let session = activation(identity).identity
        let first = try queue.activate(session: session)

        let completed = try queue.enqueue(url: "https://example.org/recent-r1", identity: identity)
        XCTAssertTrue(try queue.claim(id: completed.id, owner: "recent-r1"))
        XCTAssertEqual(try queue.finishSuccess(
            entry: try XCTUnwrap(try queue.entry(id: completed.id)),
            owner: "recent-r1",
            response: SubmitResponse(linkID: "11111111-1111-1111-1111-111111111111", status: "done", jobID: nil)
        ), .applied)

        let start = Date(timeIntervalSince1970: 80_000)
        let frozen = try queue.enqueue(url: "https://example.org/queue-r1", identity: identity, now: start)
        XCTAssertTrue(try queue.claim(id: frozen.id, owner: "queue-r1", now: start))
        XCTAssertEqual(try queue.applyFailure(
            entry: try XCTUnwrap(try queue.entry(id: frozen.id)),
            owner: "queue-r1",
            state: .retryWait,
            category: .server,
            errorCode: "temporary",
            status: 503,
            nextAttemptAt: start.addingTimeInterval(300),
            firstFailedAt: start,
            now: start
        ), .applied)
        let frozenBeforeMigration = try XCTUnwrap(try queue.entry(id: frozen.id))
        let secondQueued = try queue.enqueue(url: "https://example.org/second-r1", identity: identity, now: start)
        let secondBeforeMigration = try XCTUnwrap(try queue.entry(id: secondQueued.id))

        let localID = try todo.stageCreate(
            activation: first,
            request: CompanionTodoCreate(text: "offline create", dueAt: nil),
            now: start
        )
        _ = try todo.stagePatch(
            activation: first,
            todoID: localID,
            patch: CompanionTodoPatch(text: "offline edit", dueAt: nil, dueAtSet: false, done: nil, expectedHostRevision: nil),
            now: start.addingTimeInterval(1)
        )
        _ = try todo.stageDelete(
            activation: first,
            todoID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
            now: start.addingTimeInterval(2)
        )
        try todo.recordCapabilities(
            activation: first,
            capabilities: CompanionTodoCapabilities(todos: true, home: true, inbox: true)
        )
        let oldTodoClaim = try XCTUnwrap(todo.claimDue(activation: first, now: start.addingTimeInterval(3)))

        let second = try queue.activate(session: session)
        _ = try todo.stageCreate(
            activation: second,
            request: CompanionTodoCreate(text: "new revision item", dueAt: nil),
            now: start.addingTimeInterval(4)
        )
        try todo.recordCapabilities(
            activation: second,
            capabilities: CompanionTodoCapabilities(todos: false, home: false, inbox: false)
        )
        let targetBeforePrepare = try todo.snapshot(activation: second)
        let orchestrator = IdentityMigrationOrchestrator(queue: queue, todo: todo)
        let plan = try XCTUnwrap(orchestrator.prepare(source: first))

        XCTAssertEqual(plan.target, second)
        XCTAssertEqual(Set(plan.queueEntryIDs), Set([frozen.id, secondQueued.id]))
        XCTAssertTrue(plan.includesRecent)
        XCTAssertEqual(plan.todo?.operationCount, 3)
        XCTAssertEqual(try queue.recentMigrationCandidates(to: second).map(\.revision), [first.revision])
        // Preparing the confirmation is read-only. Dismissing it performs no
        // migration and no network work.
        XCTAssertEqual(try queue.entry(id: frozen.id)?.idempotencyKey, frozenBeforeMigration.idempotencyKey)
        XCTAssertEqual(try todo.snapshot(activation: second), targetBeforePrepare)
        XCTAssertTrue(try XCTUnwrap(try queue.recent(identity: identity)).isIdentityMismatch)

        let migrated = orchestrator.execute(plan, now: start.addingTimeInterval(10))

        XCTAssertTrue(migrated.isComplete)
        let queueAfterMigration = try XCTUnwrap(try queue.entry(id: frozen.id))
        XCTAssertEqual(queueAfterMigration.identityRevision, second.revision)
        XCTAssertNotEqual(queueAfterMigration.idempotencyKey, frozenBeforeMigration.idempotencyKey)
        XCTAssertEqual(queueAfterMigration.state, .pendingSubmit)
        XCTAssertEqual(queueAfterMigration.attemptCount, 0)
        XCTAssertNil(queueAfterMigration.nextAttemptAt)
        XCTAssertNil(queueAfterMigration.lastError)
        XCTAssertNil(queueAfterMigration.leaseOwner)
        XCTAssertNil(queueAfterMigration.leaseExpiresAt)
        let secondAfterMigration = try XCTUnwrap(try queue.entry(id: secondQueued.id))
        XCTAssertEqual(secondAfterMigration.identityRevision, second.revision)
        XCTAssertNotEqual(secondAfterMigration.idempotencyKey, secondBeforeMigration.idempotencyKey)
        XCTAssertNil(secondAfterMigration.leaseOwner)
        XCTAssertNil(secondAfterMigration.leaseExpiresAt)
        XCTAssertEqual(try queue.recent(identity: identity)?.identityRevision, second.revision)

        let targetSnapshot = try todo.snapshot(activation: second)
        XCTAssertEqual(targetSnapshot.pendingOperations, 4)
        XCTAssertEqual(Set(targetSnapshot.items.map(\.text)), Set(["offline edit", "new revision item"]))
        XCTAssertNil(targetSnapshot.todosEnabled)
        XCTAssertTrue(try todo.migrationCandidates(to: second).isEmpty)
        XCTAssertTrue(try queue.recentMigrationCandidates(to: second).isEmpty)

        // A crash after all component commits may replay the same plan. Receipts
        // make that replay a no-op and must not replace a newer target recent row.
        let newer = try queue.enqueue(url: "https://example.org/recent-r2", identity: identity)
        XCTAssertTrue(try queue.claim(id: newer.id, owner: "recent-r2"))
        XCTAssertEqual(try queue.finishSuccess(
            entry: try XCTUnwrap(try queue.entry(id: newer.id)),
            owner: "recent-r2",
            response: SubmitResponse(linkID: "22222222-2222-2222-2222-222222222222", status: "done", jobID: nil)
        ), .applied)
        try queue.delete(id: frozen.id)
        try queue.delete(id: secondQueued.id)
        let replay = orchestrator.execute(plan, now: start.addingTimeInterval(20))
        XCTAssertTrue(replay.isComplete)
        XCTAssertEqual(replay.migratedQueueEntries, 2, "durable receipts survive target-row completion or deletion")
        XCTAssertEqual(try todo.snapshot(activation: second).pendingOperations, 4)
        XCTAssertEqual(try queue.recent(identity: identity)?.linkID, "22222222-2222-2222-2222-222222222222")

        let newTodoClaim = try XCTUnwrap(todo.claimDue(activation: second, now: start.addingTimeInterval(20)))
        XCTAssertNotEqual(newTodoClaim.operation.id, oldTodoClaim.operation.id)
        XCTAssertNotEqual(newTodoClaim.operation.targetTodoID, oldTodoClaim.operation.targetTodoID)
        XCTAssertEqual(newTodoClaim.operation.attemptCount, 0)
        XCTAssertNil(newTodoClaim.operation.nextAttemptAt)
        XCTAssertNil(newTodoClaim.operation.lastError)
    }

    func testProductMigrationSelectsOnlyOriginalARevisionAfterAThenBThenA() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let queue = try AppGroupQueueRepository(containerURL: directory)
        let todo = try CompanionTodoRepository(
            containerURL: directory,
            cipher: CompanionTodoAESGCMCipher(keyData: Data(repeating: 42, count: 32))
        )
        let identityA = QueueIdentity(origin: "https://a.example", namespace: String(repeating: "a", count: 43))
        let identityB = QueueIdentity(origin: "https://b.example", namespace: String(repeating: "b", count: 43))
        let firstA = try queue.activate(session: activation(identityA).identity)
        let queueA = try queue.enqueue(url: "https://example.org/a-r1", identity: identityA)
        _ = try todo.stageCreate(activation: firstA, request: CompanionTodoCreate(text: "A R1", dueAt: nil))

        let activationB = try queue.activate(session: activation(identityB).identity)
        let queueB = try queue.enqueue(url: "https://example.org/b-r2", identity: identityB)
        _ = try todo.stageCreate(activation: activationB, request: CompanionTodoCreate(text: "B R2", dueAt: nil))
        let thirdA = try queue.activate(session: activation(identityA).identity)

        let orchestrator = IdentityMigrationOrchestrator(queue: queue, todo: todo)
        let plan = try XCTUnwrap(orchestrator.prepare(source: firstA))
        XCTAssertEqual(plan.target, thirdA)
        XCTAssertEqual(plan.queueEntryIDs, [queueA.id])
        XCTAssertTrue(orchestrator.execute(plan).isComplete)

        XCTAssertEqual(try queue.entry(id: queueA.id)?.identityRevision, thirdA.revision)
        XCTAssertEqual(try queue.entry(id: queueB.id)?.identityRevision, activationB.revision)
        XCTAssertEqual(try todo.snapshot(activation: thirdA).items.map(\.text), ["A R1"])
        XCTAssertEqual(try todo.snapshot(activation: activationB).items.map(\.text), ["B R2"])
    }

    func testProductMigrationCancellationStaleTargetAndPartialRetryAreWriteSafe() throws {
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "x", count: 43))
        let source = activation(identity, revision: 1)
        let target = activation(identity, revision: 2)
        let queue = RecoverableMigrationQueueStore(source: source, target: target, entryIDs: ["one", "two"])
        let todo = RecoverableMigrationTodoStore(source: source)
        let orchestrator = IdentityMigrationOrchestrator(queue: queue, todo: todo)
        let plan = try XCTUnwrap(orchestrator.prepare(source: source))

        // Product cancellation discards this read-only plan. No execute call means
        // no queue/recent/Todo write and no upload handoff.
        XCTAssertEqual(queue.queueWrites, 0)
        XCTAssertEqual(queue.recentCalls, 0)
        XCTAssertEqual(todo.writes, 0)

        queue.active = activation(identity, revision: 3)
        let stale = orchestrator.execute(plan)
        XCTAssertTrue(stale.targetChanged)
        XCTAssertEqual(queue.queueWrites, 0)
        XCTAssertEqual(queue.recentCalls, 0)
        XCTAssertEqual(todo.writes, 0)

        queue.active = target
        queue.failRecentOnce = true
        let partial = orchestrator.execute(plan)
        XCTAssertFalse(partial.isComplete)
        XCTAssertEqual(partial.migratedQueueEntries, 2)
        XCTAssertEqual(partial.recent, .failed)
        XCTAssertEqual(partial.todo, .migrated)
        XCTAssertEqual(queue.queueWrites, 2)
        XCTAssertEqual(todo.writes, 1)

        let recovered = orchestrator.execute(plan)
        XCTAssertTrue(recovered.isComplete)
        XCTAssertEqual(queue.queueWrites, 2, "already migrated queue rows must not be rewritten")
        XCTAssertEqual(todo.writes, 1, "Todo receipt must make retry idempotent")
        XCTAssertEqual(queue.recentCalls, 2)
    }

    func testProductMigrationKeepsLiveQueueLeaseFrozenUntilClaimResolution() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let queue = try AppGroupQueueRepository(containerURL: directory)
        let todo = try CompanionTodoRepository(
            containerURL: directory,
            cipher: CompanionTodoAESGCMCipher(keyData: Data(repeating: 44, count: 32))
        )
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "l", count: 43))
        let session = activation(identity).identity
        let source = try queue.activate(session: session)
        let start = Date(timeIntervalSince1970: 90_000)
        let entry = try queue.enqueue(url: "https://example.org/live-lease", identity: identity, now: start)
        XCTAssertTrue(try queue.claim(id: entry.id, owner: "in-flight", now: start))
        let claimed = try XCTUnwrap(try queue.entry(id: entry.id))
        let target = try queue.activate(session: session)
        let orchestrator = IdentityMigrationOrchestrator(queue: queue, todo: todo)
        let plan = try XCTUnwrap(orchestrator.prepare(source: source))

        let blocked = orchestrator.execute(plan, now: start.addingTimeInterval(1))

        XCTAssertFalse(blocked.isComplete)
        XCTAssertEqual(blocked.migratedQueueEntries, 0)
        XCTAssertEqual(try queue.entry(id: entry.id)?.identityRevision, source.revision)
        XCTAssertEqual(try queue.entry(id: entry.id)?.leaseOwner, "in-flight")
        XCTAssertTrue(try queue.releaseClaim(entry: claimed, owner: "in-flight", now: start.addingTimeInterval(2)))

        let recovered = orchestrator.execute(plan, now: start.addingTimeInterval(3))
        XCTAssertTrue(recovered.isComplete)
        XCTAssertEqual(recovered.migratedQueueEntries, 1)
        XCTAssertEqual(try queue.entry(id: entry.id)?.identityRevision, target.revision)
    }

    func testProductMigrationContinuesQueueAndRecentWhenTodoStoreIsUnavailable() throws {
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "u", count: 43))
        let source = activation(identity, revision: 1)
        let target = activation(identity, revision: 2)
        let queue = RecoverableMigrationQueueStore(source: source, target: target, entryIDs: ["queue"])
        let orchestrator = IdentityMigrationOrchestrator(queue: queue, todo: UnavailableMigrationTodoStore())
        let plan = try XCTUnwrap(orchestrator.prepare(source: source))

        XCTAssertTrue(plan.todoReadFailed)
        let result = orchestrator.execute(plan)

        XCTAssertEqual(result.migratedQueueEntries, 1)
        XCTAssertEqual(result.recent, .migrated)
        XCTAssertEqual(result.todo, .failed)
        XCTAssertFalse(result.isComplete)
        XCTAssertEqual(queue.queueWrites, 1)
        XCTAssertEqual(queue.recentCalls, 1)
    }

    func testTodoMigrationFenceBlocksConcurrentActivationUntilFileCommitReturns() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let firstRepository = try AppGroupQueueRepository(containerURL: directory)
        let secondRepository = try AppGroupQueueRepository(containerURL: directory)
        // Reading the fenced state needs a handle of its own: the fence holds
        // the first repository for as long as its body runs, and the second is
        // held by the activation waiting on that fence, so asking either one
        // what is active would block until the fence it is waiting for is over.
        let observerRepository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "f", count: 43))
        let target = try firstRepository.activate(session: activation(identity).identity)
        let enteredFence = expectation(description: "entered target fence")
        let fenceFinished = expectation(description: "target fence finished")
        let activationFinished = expectation(description: "concurrent activation finished")
        let releaseFence = DispatchSemaphore(value: 0)
        let resultLock = NSLock()
        var errors: [Error] = []
        var fenceResult = false
        var activationDidFinish = false

        DispatchQueue.global().async {
            do {
                let result = try firstRepository.withActiveTargetFence(target) {
                    enteredFence.fulfill()
                    releaseFence.wait()
                    return true
                }
                resultLock.lock(); fenceResult = result; resultLock.unlock()
            } catch {
                resultLock.lock(); errors.append(error); resultLock.unlock()
            }
            fenceFinished.fulfill()
        }
        wait(for: [enteredFence], timeout: 2)

        DispatchQueue.global().async {
            do {
                _ = try secondRepository.activate(session: target.identity)
            } catch {
                resultLock.lock(); errors.append(error); resultLock.unlock()
            }
            resultLock.lock(); activationDidFinish = true; resultLock.unlock()
            activationFinished.fulfill()
        }
        Thread.sleep(forTimeInterval: 0.05)
        resultLock.lock(); let finishedWhileFenced = activationDidFinish; resultLock.unlock()
        XCTAssertFalse(finishedWhileFenced)
        XCTAssertEqual(try observerRepository.activeSessionSnapshot(), target)
        releaseFence.signal()
        wait(for: [fenceFinished, activationFinished], timeout: 2)
        XCTAssertTrue(errors.isEmpty)
        XCTAssertTrue(fenceResult)
        XCTAssertNotEqual(try firstRepository.activeSessionSnapshot(), target)
    }

    func testCredentialRevisionMismatchBlocksEveryQueueNetworkEntry() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "q", count: 43))
        let session = activation(identity).identity
        let first = try repository.activate(session: session)
        _ = try repository.activate(session: session)
        let transport = RecordingTransport { _ in
            throw CoreError.invalidResponse
        }
        let coordinator = ShareSubmissionCoordinator(
            repository: repository,
            credentials: StaticCredentialStore(config: CredentialConfig(activation: first, installationToken: "old-token")),
            api: WebTagAPIClient(transport: transport),
            wakeScheduler: RecordingWakeScheduler()
        )

        let outcome = await coordinator.submit(url: "https://example.org/must-not-send", identity: session)

        guard case .configurationRequired = outcome else {
            return XCTFail("a stale credential revision must fail closed")
        }
        XCTAssertTrue(transport.requests.isEmpty)
        let drained = await coordinator.drainOne()
        XCTAssertFalse(drained)
        XCTAssertTrue(transport.requests.isEmpty)
    }

    func testRefreshCASRejectsReplacedLinkAndIdentityChangeWithoutSideEffects() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "c", count: 43))
        try activate(repository, identity)
        let first = try repository.enqueue(url: "https://example.org/one", identity: identity)
        XCTAssertTrue(try repository.claim(id: first.id, owner: "first"))
        XCTAssertEqual(try repository.finishSuccess(entry: try XCTUnwrap(try repository.entry(id: first.id)), owner: "first", response: SubmitResponse(linkID: "11111111-1111-1111-1111-111111111111", status: "done", jobID: nil)), .applied)
        let capture = try XCTUnwrap(try repository.refreshCapture(identity: identity))

        let second = try repository.enqueue(url: "https://example.org/two", identity: identity)
        XCTAssertTrue(try repository.claim(id: second.id, owner: "second"))
        XCTAssertEqual(try repository.finishSuccess(entry: try XCTUnwrap(try repository.entry(id: second.id)), owner: "second", response: SubmitResponse(linkID: "22222222-2222-2222-2222-222222222222", status: "done", jobID: nil)), .applied)
        XCTAssertEqual(
            try repository.recordRefreshBlocked(capture: capture, notBefore: Date().addingTimeInterval(60), reason: "cooldown_active"),
            .recentReplaced
        )
        XCTAssertNil(try repository.recent(identity: identity)?.refreshNotBefore)

        let secondCapture = try XCTUnwrap(try repository.refreshCapture(identity: identity))
        try activate(repository, identity)
        XCTAssertEqual(
            try repository.recordRefreshSuccess(capture: secondCapture, response: SubmitResponse(linkID: "22222222-2222-2222-2222-222222222222", status: "done", jobID: nil)),
            .identityChanged
        )
    }

    func testRetryDeadlineSurvivesRepositoryRecoveryAndDoesNotSendEarly() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "d", count: 43))
        let start = Date(timeIntervalSince1970: 20_000)
        let first = try AppGroupQueueRepository(containerURL: directory)
        try activate(first, identity)
        let entry = try first.enqueue(url: "https://example.org/retry", identity: identity, now: start)
        XCTAssertTrue(try first.claim(id: entry.id, owner: "retry", now: start))
        let deadline = start.addingTimeInterval(120)
        XCTAssertEqual(try first.applyFailure(entry: try XCTUnwrap(try first.entry(id: entry.id)), owner: "retry", state: .retryWait, category: .server, errorCode: nil, status: 503, nextAttemptAt: deadline, firstFailedAt: start, now: start), .applied)
        XCTAssertEqual(try first.earliestWake(now: start), deadline)

        let recovered = try AppGroupQueueRepository(containerURL: directory)
        XCTAssertTrue(try recovered.due(now: deadline.addingTimeInterval(-0.001)).isEmpty)
        XCTAssertEqual(try recovered.due(now: deadline).map(\.id), [entry.id])
    }

    /// One alarm, for the earliest durable deadline. It has to move when a
    /// nearer row appears and to disappear - not slide into the future - when
    /// the last row goes.
    func testEarliestWakeFollowsTheNearestDeadlineAndClearsWithTheQueue() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "k", count: 43))
        try activate(repository, identity)
        let start = Date(timeIntervalSince1970: 30_000)
        // An empty queue asks for no alarm at all rather than a guessed one.
        XCTAssertNil(try repository.earliestWake(now: start))

        let late = try repository.enqueue(url: "https://example.org/late", identity: identity, now: start)
        XCTAssertTrue(try repository.claim(id: late.id, owner: "late-owner", now: start))
        XCTAssertEqual(
            try repository.applyFailure(
                entry: try XCTUnwrap(try repository.entry(id: late.id)), owner: "late-owner",
                state: .retryWait, category: .server, errorCode: nil, status: 503,
                nextAttemptAt: start.addingTimeInterval(600), firstFailedAt: start, now: start
            ),
            .applied
        )
        XCTAssertEqual(try repository.earliestWake(now: start), start.addingTimeInterval(600))

        let early = try repository.enqueue(url: "https://example.org/early", identity: identity, now: start.addingTimeInterval(1))
        XCTAssertTrue(try repository.claim(id: early.id, owner: "early-owner", now: start.addingTimeInterval(1)))
        XCTAssertEqual(
            try repository.applyFailure(
                entry: try XCTUnwrap(try repository.entry(id: early.id)), owner: "early-owner",
                state: .retryWait, category: .rateLimit, errorCode: "rate_limited", status: 429,
                nextAttemptAt: start.addingTimeInterval(120), firstFailedAt: start.addingTimeInterval(1),
                now: start.addingTimeInterval(1)
            ),
            .applied
        )
        // The nearer deadline replaces the pending one instead of joining it.
        XCTAssertEqual(try repository.earliestWake(now: start.addingTimeInterval(1)), start.addingTimeInterval(120))
        XCTAssertTrue(try repository.due(now: start.addingTimeInterval(119)).isEmpty)

        try repository.delete(id: early.id)
        XCTAssertEqual(try repository.earliestWake(now: start.addingTimeInterval(1)), start.addingTimeInterval(600))
        try repository.delete(id: late.id)
        XCTAssertNil(try repository.earliestWake(now: start.addingTimeInterval(1)))
    }

    func testDurableWakeAdapterReplacesEarlierDeadlineCancelsAndDeduplicates() {
        let adapter = RecordingDurableWakeAdapter()
        let scheduler = DurableQueueWakeScheduler(durable: adapter)
        let late = Date(timeIntervalSince1970: 50_000)
        let early = late.addingTimeInterval(-300)

        scheduler.schedule(deadline: late)
        scheduler.schedule(deadline: late)
        scheduler.schedule(deadline: early)
        scheduler.schedule(deadline: nil)
        scheduler.schedule(deadline: nil)

        XCTAssertEqual(adapter.replacements.count, 3)
        XCTAssertEqual(adapter.replacements[0], late)
        XCTAssertEqual(adapter.replacements[1], early)
        XCTAssertNil(adapter.replacements[2])
    }

    func testBoundedDrainStopsAtTheRequestedLimitAndRecomputesWake() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let namespace = String(repeating: "r", count: 43)
        let session = SessionIdentity(origin: "https://example.org", namespace: namespace, representationContract: "v3")
        let active = try repository.activate(session: session)
        for index in 0..<3 {
            _ = try repository.enqueue(url: "https://example.org/bounded-\(index)", identity: active.queueIdentity)
        }
        let credentials = StaticCredentialStore(config: CredentialConfig(activation: active, installationToken: "token"))
        let responseURL = URL(string: "https://example.org/api/links")!
        let transport = RecordingTransport { _ in
            let response = HTTPURLResponse(
                url: responseURL, statusCode: 202, httpVersion: "HTTP/1.1",
                headerFields: ["X-WebTag-Data-Namespace": namespace]
            )!
            return (Data(#"{"link_id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb","status":"done"}"#.utf8), response)
        }
        let wake = RecordingWakeScheduler()
        let coordinator = ShareSubmissionCoordinator(
            repository: repository,
            credentials: credentials,
            api: WebTagAPIClient(transport: transport),
            wakeScheduler: wake
        )

        await coordinator.reconcileAndDrain(maxItems: 2)

        XCTAssertEqual(transport.requests.count, 2)
        XCTAssertEqual(try repository.list().count, 1)
        XCTAssertNotNil(wake.deadlines.last!)
    }

    func testExpiredDrainReleasesExactClaimAndRecomputesWakeWithoutCommittingResponse() async throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let namespace = String(repeating: "e", count: 43)
        let session = SessionIdentity(origin: "https://example.org", namespace: namespace, representationContract: "v3")
        let active = try repository.activate(session: session)
        let start = Date()
        let queued = try repository.enqueue(url: "https://example.org/expired-drain", identity: active.queueIdentity, now: start)
        let expiration = LockedExpirationSignal()
        let responseURL = URL(string: "https://example.org/api/links")!
        let transport = RecordingTransport(onRequest: { _ in expiration.expire() }) { _ in
            let response = HTTPURLResponse(
                url: responseURL, statusCode: 202, httpVersion: "HTTP/1.1",
                headerFields: ["X-WebTag-Data-Namespace": namespace]
            )!
            return (Data(#"{"link_id":"eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee","status":"done"}"#.utf8), response)
        }
        let wake = RecordingWakeScheduler()
        let coordinator = ShareSubmissionCoordinator(
            repository: repository,
            credentials: StaticCredentialStore(config: CredentialConfig(activation: active, installationToken: "token")),
            api: WebTagAPIClient(transport: transport),
            wakeScheduler: wake
        )

        await coordinator.reconcileAndDrain(
            now: start,
            maxItems: 8,
            shouldContinue: { !expiration.isExpired }
        )

        XCTAssertEqual(transport.requests.count, 1)
        let preserved = try XCTUnwrap(try repository.entry(id: queued.id))
        XCTAssertEqual(preserved.idempotencyKey, queued.idempotencyKey)
        XCTAssertEqual(preserved.requestFingerprint, queued.requestFingerprint)
        XCTAssertEqual(preserved.state, .pendingSubmit)
        XCTAssertEqual(preserved.attemptCount, 0)
        XCTAssertNil(preserved.leaseOwner)
        XCTAssertNil(preserved.leaseExpiresAt)
        XCTAssertNil(try repository.recent(identity: active.queueIdentity))
        XCTAssertEqual(wake.deadlines.last!, start)
    }

    /// Two rows that come due at the same instant still drain in one order, and
    /// it is the order they were created in - never whatever SQLite felt like.
    func testDueOrdersByDeadlineAndBreaksTiesByCreation() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "o", count: 43))
        try activate(repository, identity)
        let start = Date(timeIntervalSince1970: 40_000)
        let tieFirst = try repository.enqueue(url: "https://example.org/tie-first", identity: identity, now: start)
        let nearest = try repository.enqueue(url: "https://example.org/nearest", identity: identity, now: start.addingTimeInterval(1))
        let tieLast = try repository.enqueue(url: "https://example.org/tie-last", identity: identity, now: start.addingTimeInterval(2))
        let plan: [(QueueEntry, String, TimeInterval)] = [
            (tieFirst, "tie-first", 300),
            (nearest, "nearest", 120),
            (tieLast, "tie-last", 300),
        ]
        for (entry, owner, offset) in plan {
            XCTAssertTrue(try repository.claim(id: entry.id, owner: owner, now: start.addingTimeInterval(3)))
            XCTAssertEqual(
                try repository.applyFailure(
                    entry: try XCTUnwrap(try repository.entry(id: entry.id)), owner: owner,
                    state: .retryWait, category: .server, errorCode: nil, status: 503,
                    nextAttemptAt: start.addingTimeInterval(offset), firstFailedAt: start.addingTimeInterval(3),
                    now: start.addingTimeInterval(3)
                ),
                .applied
            )
        }

        XCTAssertEqual(
            try repository.due(now: start.addingTimeInterval(600)).map(\.id),
            [nearest.id, tieFirst.id, tieLast.id]
        )
    }

    /// A refresh answer that arrives after the recent link was replaced is
    /// `recent_replaced` whichever of the three answers it is, and none of them
    /// may touch the link that took its place.
    func testEveryRefreshResultClassReportsRecentReplacedAfterTheLinkChanged() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "y", count: 43))
        try activate(repository, identity)
        let first = try repository.enqueue(url: "https://example.org/refresh-first", identity: identity)
        XCTAssertTrue(try repository.claim(id: first.id, owner: "refresh-first"))
        XCTAssertEqual(
            try repository.finishSuccess(
                entry: try XCTUnwrap(try repository.entry(id: first.id)), owner: "refresh-first",
                response: SubmitResponse(linkID: "11111111-1111-1111-1111-111111111111", status: "done", jobID: nil)
            ),
            .applied
        )
        let capture = try XCTUnwrap(try repository.refreshCapture(identity: identity))

        let second = try repository.enqueue(url: "https://example.org/refresh-second", identity: identity)
        XCTAssertTrue(try repository.claim(id: second.id, owner: "refresh-second"))
        XCTAssertEqual(
            try repository.finishSuccess(
                entry: try XCTUnwrap(try repository.entry(id: second.id)), owner: "refresh-second",
                response: SubmitResponse(linkID: "22222222-2222-2222-2222-222222222222", status: "processing", jobID: nil)
            ),
            .applied
        )

        XCTAssertEqual(
            try repository.recordRefreshSuccess(
                capture: capture,
                response: SubmitResponse(linkID: "11111111-1111-1111-1111-111111111111", status: "done", jobID: nil)
            ),
            .recentReplaced
        )
        XCTAssertEqual(
            try repository.recordRefreshBlocked(capture: capture, notBefore: Date(timeIntervalSince1970: 50_000), reason: "cooldown_active"),
            .recentReplaced
        )
        let recent = try XCTUnwrap(try repository.recent(identity: identity))
        XCTAssertEqual(recent.linkID, "22222222-2222-2222-2222-222222222222")
        XCTAssertEqual(recent.status, "processing")
        XCTAssertNil(recent.refreshNotBefore)
        XCTAssertNil(recent.refreshBlockReason)
    }

    /// The other side of the lease boundary. The expired-owner test pins the
    /// instant of expiry as stale; this pins one millisecond earlier as still
    /// committable, which is what makes that instant a boundary rather than a
    /// rounded-off window.
    func testCompletionOneMillisecondInsideTheLeaseStillCommits() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "z", count: 43))
        try activate(repository, identity)
        let start = Date(timeIntervalSince1970: 50_000)
        let entry = try repository.enqueue(url: "https://example.org/boundary", identity: identity, now: start)
        XCTAssertTrue(try repository.claim(id: entry.id, owner: "boundary", now: start, leaseDuration: 60))
        XCTAssertEqual(
            try repository.finishSuccess(
                entry: try XCTUnwrap(try repository.entry(id: entry.id)), owner: "boundary",
                response: SubmitResponse(linkID: "99999999-9999-9999-9999-999999999999", status: "pending", jobID: nil),
                now: start.addingTimeInterval(59.999)
            ),
            .applied
        )
        XCTAssertTrue(try repository.list().isEmpty)
        XCTAssertEqual(try repository.recent(identity: identity)?.linkID, "99999999-9999-9999-9999-999999999999")
    }

    // MARK: - Settings queue grouping, recent projection and lifecycle

    func testSettingsQueueGroupingFollowsTheSharedFixture() throws {
        let data = try Data(contentsOf: settingsQueueFixtureURL())
        let fixture = try XCTUnwrap(try JSONSerialization.jsonObject(with: data) as? [String: Any])
        // The same file drives the Android JVM test. A platform that quietly stops
        // reading it would keep passing on its own hand-written expectations.
        let declaredGroups = try XCTUnwrap(fixture["groups"] as? [[String: Any]])
        XCTAssertEqual(
            declaredGroups.map { $0["key"] as? String },
            SettingsQueueGroup.allCases.map(\.rawValue),
            "the fixture's section order is the frozen render order"
        )
        for group in declaredGroups {
            let key = try XCTUnwrap(group["key"] as? String)
            let states = try XCTUnwrap(group["states"] as? [String])
            for rawState in states {
                let state = try XCTUnwrap(QueueState(rawValue: rawState))
                XCTAssertEqual(SettingsQueueGroup.group(for: state).rawValue, key, rawState)
            }
        }

        let cases = try XCTUnwrap(fixture["cases"] as? [[String: Any]])
        XCTAssertFalse(cases.isEmpty)
        for testCase in cases {
            let caseID = try XCTUnwrap(testCase["id"] as? String)
            let rows = try XCTUnwrap(testCase["rows"] as? [[String: Any]])
            let entries = try rows.map { try settingsQueueEntry(from: $0) }
            let projection = SettingsQueuePresenter.project(entries)

            XCTAssertEqual(projection.total, testCase["expected_total"] as? Int, caseID)
            let expectedGroups = try XCTUnwrap(testCase["expected_groups"] as? [[String: Any]])
            XCTAssertEqual(
                projection.groups.map(\.group.rawValue),
                expectedGroups.map { $0["key"] as? String },
                "\(caseID): only non-empty sections render, in the frozen order"
            )
            for (rendered, expected) in zip(projection.groups, expectedGroups) {
                XCTAssertEqual(rendered.count, expected["count"] as? Int, caseID)
                XCTAssertEqual(
                    rendered.rows.map(\.id),
                    expected["row_ids"] as? [String],
                    "\(caseID): repository order is preserved inside a section"
                )
            }
        }
    }

    func testSettingsQueueTotalCountsRowsInHiddenSectionsToo() throws {
        // Guards the difference between "what is stored" and "what is on screen":
        // the header must not shrink when a section is hidden.
        let entries = [
            settingsEntry(id: "a", state: .expired),
            settingsEntry(id: "b", state: .expired),
        ]
        let projection = SettingsQueuePresenter.project(entries)
        XCTAssertEqual(projection.total, 2)
        XCTAssertEqual(projection.groups.count, 1)
        XCTAssertEqual(SettingsQueuePresenter.project([]).total, 0)
        XCTAssertTrue(SettingsQueuePresenter.project([]).groups.isEmpty)
    }

    func testSettingsTimeFormatterIsAbsoluteLocaleAwareAndSilentOnMissingValues() {
        let locale = Locale(identifier: "zh_Hans_CN")
        let shanghai = TimeZone(identifier: "Asia/Shanghai")!
        let utc = TimeZone(identifier: "UTC")!
        let moment = Date(timeIntervalSince1970: 1_754_881_200)

        // Expectations are rendered, never spelled out: CLDR data changes between
        // OS releases, so a literal like "2026/8/11 11:00" pins the OS, not the code.
        let reference = DateFormatter()
        reference.locale = locale
        reference.timeZone = shanghai
        reference.dateStyle = .medium
        reference.timeStyle = .short
        XCTAssertEqual(
            SettingsTimeFormatter.absolute(moment, locale: locale, timeZone: shanghai),
            reference.string(from: moment)
        )
        // A time zone that does not reach the output would make the assertion above
        // pass against a formatter that ignores both arguments.
        XCTAssertNotEqual(
            SettingsTimeFormatter.absolute(moment, locale: locale, timeZone: shanghai),
            SettingsTimeFormatter.absolute(moment, locale: locale, timeZone: utc)
        )
        // Absolute, not relative: the date part has to be there.
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = shanghai
        let year = String(calendar.component(.year, from: moment))
        XCTAssertEqual(SettingsTimeFormatter.absolute(moment, locale: locale, timeZone: shanghai)?.contains(year), true)
        // No placeholder. A missing timestamp renders nothing at all.
        XCTAssertNil(SettingsTimeFormatter.absolute(nil, locale: locale, timeZone: shanghai))
    }

    func testRecentRefreshGateRedactsAndDisablesOnIdentityMismatch() {
        let now = Date(timeIntervalSince1970: 20_000)
        let matched = settingsRecent(mismatch: false, notBefore: nil, reason: nil)
        XCTAssertEqual(
            SettingsRefreshGatePolicy.evaluate(recent: matched, now: now),
            SettingsRecentRefreshGate(isEnabled: true, cooldownUntil: nil, blockReason: nil)
        )
        // A mismatched row keeps its blocked state visible but nothing else: no
        // refresh, no deadline, no reason string that could describe another
        // identity's data.
        let mismatched = settingsRecent(mismatch: true, notBefore: now.addingTimeInterval(60), reason: "cooldown_active")
        XCTAssertEqual(SettingsRefreshGatePolicy.evaluate(recent: mismatched, now: now), .unavailable)
        XCTAssertEqual(SettingsRefreshGatePolicy.evaluate(recent: nil, now: now), .unavailable)
    }

    func testRecentRefreshGateIgnoresAReasonWithoutAFutureDeadline() {
        let now = Date(timeIntervalSince1970: 30_000)
        let recent = settingsRecent(mismatch: false, notBefore: nil, reason: "server_hint")
        let gate = SettingsRefreshGatePolicy.evaluate(recent: recent, now: now)
        XCTAssertTrue(gate.isEnabled)
        XCTAssertNil(gate.cooldownUntil)
        XCTAssertNil(gate.blockReason)

        let clock = FakeSettingsClock(now: now)
        let timer = SettingsCooldownTimer(clock: clock)
        timer.arm(until: gate.cooldownUntil) { XCTFail("a reason without a deadline must not arm a timer") }
        XCTAssertEqual(clock.pendingCount, 0)
    }

    func testCooldownUnlocksExactlyAtTheDeadlineWithoutReadingTheDatabase() {
        let start = Date(timeIntervalSince1970: 40_000)
        let deadline = start.addingTimeInterval(120)
        let recent = settingsRecent(mismatch: false, notBefore: deadline, reason: SettingsRefreshGatePolicy.cooldownReason)
        let repository = CountingSettingsRepository()
        let clock = FakeSettingsClock(now: start)
        let timer = SettingsCooldownTimer(clock: clock)

        var gate = SettingsRefreshGatePolicy.evaluate(recent: recent, now: clock.now)
        XCTAssertFalse(gate.isEnabled)
        XCTAssertEqual(gate.cooldownUntil, deadline)
        timer.arm(until: gate.cooldownUntil) {
            gate = SettingsRefreshGatePolicy.evaluate(recent: recent, now: clock.now)
        }

        clock.advance(to: deadline.addingTimeInterval(-0.001))
        XCTAssertFalse(gate.isEnabled, "one millisecond before the deadline the block still holds")
        clock.advance(to: deadline)
        XCTAssertTrue(gate.isEnabled, "the exact deadline unlocks without anything else happening")
        XCTAssertNil(gate.cooldownUntil)
        // The deadline changes what is displayed, not what is stored.
        XCTAssertEqual(repository.reads, 0)
        XCTAssertEqual(repository.writes, 0)
    }

    func testCooldownTimerIgnoresAnArmingThatWasReplacedOrDisposed() {
        let start = Date(timeIntervalSince1970: 50_000)
        let clock = FakeSettingsClock(now: start)
        let timer = SettingsCooldownTimer(clock: clock)
        var fired: [String] = []

        // Replacement: the recent row changed while the first deadline was pending.
        timer.arm(until: start.addingTimeInterval(60)) { fired.append("first") }
        timer.arm(until: start.addingTimeInterval(90)) { fired.append("second") }
        clock.advance(to: start.addingTimeInterval(60))
        XCTAssertEqual(fired, [], "the replaced arming must not fire")
        clock.advance(to: start.addingTimeInterval(90))
        XCTAssertEqual(fired, ["second"])

        // Dispose: the screen went away while a deadline was pending.
        fired.removeAll()
        timer.arm(until: start.addingTimeInterval(200)) { fired.append("disposed") }
        timer.invalidate()
        clock.advance(to: start.addingTimeInterval(200))
        XCTAssertEqual(fired, [])

        // A deadline already in the past is not worth arming for.
        timer.arm(until: start) { fired.append("past") }
        XCTAssertEqual(clock.pendingCount, 0)
        XCTAssertEqual(fired, [])
    }

    func testForegroundReadShowsWhatAnotherRepositoryWroteWhileInactive() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "l", count: 43))
        try activate(repository, identity)
        let loader = SettingsSnapshotLoader(repository: repository)
        _ = try repository.enqueue(url: "https://example.org/a", identity: identity)
        XCTAssertEqual(loader.loadProjection()?.queue.total, 1)

        // The share extension writes through its own repository handle and cannot
        // invalidate anything in this process, so only a fresh read can see B.
        let other = try AppGroupQueueRepository(containerURL: directory)
        _ = try other.enqueue(url: "https://example.org/b", identity: identity)

        let afterForeground = try XCTUnwrap(loader.loadProjection())
        XCTAssertEqual(afterForeground.queue.total, 2)
        XCTAssertEqual(
            afterForeground.queue.groups.flatMap(\.rows).map(\.url),
            ["https://example.org/a", "https://example.org/b"]
        )
    }

    func testSecondReadAfterDrainShowsWhatTheDrainChanged() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "m", count: 43))
        try activate(repository, identity)
        let loader = SettingsSnapshotLoader(repository: repository)
        let b = try repository.enqueue(url: "https://example.org/b", identity: identity)

        let first = try XCTUnwrap(loader.loadProjection())
        XCTAssertEqual(first.queue.groups.flatMap(\.rows).map(\.id), [b.id])

        // Stands in for the drain: it is the second read, not the first, that has to
        // show what reconcile/drain did.
        let c = try repository.enqueue(url: "https://example.org/c", identity: identity)
        try repository.delete(id: b.id)

        let second = try XCTUnwrap(loader.loadProjection())
        XCTAssertEqual(second.queue.groups.flatMap(\.rows).map(\.id), [c.id])
    }

    func testALateSnapshotFromAReplacedIdentityIsNotCommitted() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let repository = try AppGroupQueueRepository(containerURL: directory)
        let first = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "n", count: 43))
        let second = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "o", count: 43))
        try activate(repository, first)
        _ = try repository.enqueue(url: "https://example.org/first", identity: first)
        let loader = SettingsSnapshotLoader(repository: repository)

        // A read that started under the first identity and has not been committed yet.
        let stale = try loader.snapshot()

        try activate(repository, second)
        _ = try repository.enqueue(url: "https://example.org/second", identity: second)
        let current = try loader.snapshot()
        XCTAssertTrue(loader.commit(current))

        // Released only now, after the newer one already landed.
        XCTAssertFalse(loader.commit(stale), "a snapshot from a replaced identity must not repaint the screen")

        // A → B → A gets a third revision, so an in-flight A load is still stale.
        try activate(repository, first)
        XCTAssertFalse(loader.commit(stale))
    }

    func testCompanionTodoPresenterBuildsSevenDayAndStableSections() {
        var calendar = Calendar(identifier: .gregorian)
        calendar.timeZone = TimeZone(secondsFromGMT: 0)!
        let now = Date(timeIntervalSince1970: 1_753_747_200) // 2025-07-29 00:00:00Z
        let items = [
            companionTodo(id: "00000000-0000-0000-0000-000000000001", text: "overdue", dueAt: now.addingTimeInterval(-86_400)),
            companionTodo(id: "00000000-0000-0000-0000-000000000002", text: "today", dueAt: now.addingTimeInterval(18 * 3_600)),
            companionTodo(id: "00000000-0000-0000-0000-000000000003", text: "later", dueAt: now.addingTimeInterval(2 * 86_400)),
            companionTodo(id: "00000000-0000-0000-0000-000000000004", text: "none", dueAt: nil),
            companionTodo(id: "00000000-0000-0000-0000-000000000005", text: "done", dueAt: now, done: true),
        ]

        let projection = CompanionTodoPresenter.project(items, now: now, calendar: calendar)

        XCTAssertEqual(projection.days.count, 7)
        XCTAssertEqual(projection.openCount, 4)
        XCTAssertEqual(projection.overdueCount, 1)
        XCTAssertEqual(projection.todayCount, 1)
        XCTAssertEqual(projection.sections.map(\.section), CompanionTodoSection.allCases)
        XCTAssertEqual(projection.sections.map { $0.items.map(\.text) }, [["overdue"], ["today"], ["later"], ["none"], ["done"]])
    }

    func testCompanionTodoStateIsEncryptedAndPersistsDesiredState() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let key = Data(repeating: 7, count: 32)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "t", count: 43))
        let active = activation(identity)
        let repository = try CompanionTodoRepository(
            containerURL: directory,
            cipher: CompanionTodoAESGCMCipher(keyData: key)
        )

        let localID = try repository.stageCreate(
            activation: active,
            request: CompanionTodoCreate(text: "private offline task", dueAt: nil),
            now: Date(timeIntervalSince1970: 1_000)
        )
        let first = try repository.snapshot(activation: active)
        XCTAssertEqual(first.items.map(\.id), [localID])
        XCTAssertEqual(first.items.first?.text, "private offline task")
        XCTAssertTrue(first.items.first?.localOnly == true)
        XCTAssertEqual(first.pendingOperations, 1)

        let disk = try Data(contentsOf: directory.appendingPathComponent("companion-todo-state.v1"))
        XCTAssertNil(String(data: disk, encoding: .utf8)?.range(of: "private offline task"))

        let reopened = try CompanionTodoRepository(
            containerURL: directory,
            cipher: CompanionTodoAESGCMCipher(keyData: key)
        )
        XCTAssertEqual(try reopened.snapshot(activation: active), first)
    }

    func testCompanionTodoRepositoriesShareOneLinearizableStateTransaction() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let key = Data(repeating: 31, count: 32)
        let first = try CompanionTodoRepository(containerURL: directory, cipher: CompanionTodoAESGCMCipher(keyData: key))
        let second = try CompanionTodoRepository(containerURL: directory, cipher: CompanionTodoAESGCMCipher(keyData: key))
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "j", count: 43))
        let active = activation(identity)
        let resultLock = NSLock()
        var errors: [Error] = []

        DispatchQueue.concurrentPerform(iterations: 40) { index in
            do {
                _ = try (index.isMultiple(of: 2) ? first : second).stageCreate(
                    activation: active,
                    request: CompanionTodoCreate(text: "operation-\(index)", dueAt: nil),
                    now: Date(timeIntervalSince1970: TimeInterval(index))
                )
            } catch {
                resultLock.lock(); errors.append(error); resultLock.unlock()
            }
        }

        XCTAssertTrue(errors.isEmpty)
        XCTAssertEqual(try first.snapshot(activation: active).items.count, 40)
        XCTAssertTrue(FileManager.default.fileExists(atPath: directory.appendingPathComponent("companion-todo-state.v1.lock").path))
    }

    func testCompanionTodoPausedSealSerializesSecondRepositoryWrite() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let key = Data(repeating: 34, count: 32)
        let blockingCipher = try BlockingCompanionTodoCipher(keyData: key)
        let first = try CompanionTodoRepository(containerURL: directory, cipher: blockingCipher)
        let second = try CompanionTodoRepository(
            containerURL: directory,
            cipher: CompanionTodoAESGCMCipher(keyData: key)
        )
        let active = activation(QueueIdentity(origin: "https://example.org", namespace: String(repeating: "b", count: 43)))
        let firstFinished = expectation(description: "first write finished")
        let secondStarted = expectation(description: "second write started")
        let secondFinished = expectation(description: "second write finished")
        let resultLock = NSLock()
        var errors: [Error] = []
        var secondDidFinish = false

        DispatchQueue.global().async {
            do {
                _ = try first.stageCreate(
                    activation: active,
                    request: CompanionTodoCreate(text: "scene A", dueAt: nil)
                )
            } catch {
                resultLock.lock(); errors.append(error); resultLock.unlock()
            }
            firstFinished.fulfill()
        }
        XCTAssertEqual(blockingCipher.sealEntered.wait(timeout: .now() + 2), .success)

        DispatchQueue.global().async {
            secondStarted.fulfill()
            do {
                _ = try second.stageCreate(
                    activation: active,
                    request: CompanionTodoCreate(text: "scene B", dueAt: nil)
                )
            } catch {
                resultLock.lock(); errors.append(error); resultLock.unlock()
            }
            resultLock.lock(); secondDidFinish = true; resultLock.unlock()
            secondFinished.fulfill()
        }
        wait(for: [secondStarted], timeout: 2)
        Thread.sleep(forTimeInterval: 0.05)
        resultLock.lock(); let finishedBeforeRelease = secondDidFinish; resultLock.unlock()
        XCTAssertFalse(finishedBeforeRelease)

        blockingCipher.releaseSeal.signal()
        wait(for: [firstFinished, secondFinished], timeout: 2)
        XCTAssertTrue(errors.isEmpty)
        let reopened = try CompanionTodoRepository(
            containerURL: directory,
            cipher: CompanionTodoAESGCMCipher(keyData: key)
        )
        let snapshot = try reopened.snapshot(activation: active)
        XCTAssertEqual(Set(snapshot.items.map(\.text)), Set(["scene A", "scene B"]))
        XCTAssertEqual(snapshot.pendingOperations, 2)
    }

    func testCompanionTodoSealFailurePreservesOldStateAndReleasesTransactionLock() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let key = Data(repeating: 35, count: 32)
        let cipher = try FailOnceCompanionTodoCipher(keyData: key)
        let repository = try CompanionTodoRepository(containerURL: directory, cipher: cipher)
        let active = activation(QueueIdentity(origin: "https://example.org", namespace: String(repeating: "e", count: 43)))
        _ = try repository.stageCreate(
            activation: active,
            request: CompanionTodoCreate(text: "durable before failure", dueAt: nil)
        )
        let before = try repository.snapshot(activation: active)

        cipher.failNextSeal()
        XCTAssertThrowsError(try repository.stageCreate(
            activation: active,
            request: CompanionTodoCreate(text: "must roll back", dueAt: nil)
        ))
        let reopened = try CompanionTodoRepository(
            containerURL: directory,
            cipher: CompanionTodoAESGCMCipher(keyData: key)
        )
        XCTAssertEqual(try reopened.snapshot(activation: active), before)

        _ = try repository.stageCreate(
            activation: active,
            request: CompanionTodoCreate(text: "lock recovered", dueAt: nil)
        )
        XCTAssertEqual(try repository.snapshot(activation: active).items.count, 2)
    }

    func testCompanionTodoCommitNotificationCanReloadSiblingRepository() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let key = Data(repeating: 36, count: 32)
        let first = try CompanionTodoRepository(containerURL: directory, cipher: CompanionTodoAESGCMCipher(keyData: key))
        let second = try CompanionTodoRepository(containerURL: directory, cipher: CompanionTodoAESGCMCipher(keyData: key))
        let active = activation(QueueIdentity(origin: "https://example.org", namespace: String(repeating: "n", count: 43)))
        let reloaded = expectation(description: "sibling repository reloaded after commit")
        reloaded.assertForOverFulfill = false
        let resultLock = NSLock()
        var observed: CompanionTodoSnapshot?
        var observedError: Error?
        let observer = NotificationCenter.default.addObserver(
            forName: CompanionTodoTransactionCoordinator.stateDidChange,
            object: nil,
            queue: nil
        ) { _ in
            resultLock.lock()
            defer { resultLock.unlock() }
            guard observed == nil, observedError == nil else { return }
            do {
                observed = try second.snapshot(activation: active)
            } catch {
                observedError = error
            }
            reloaded.fulfill()
        }
        defer { NotificationCenter.default.removeObserver(observer) }

        _ = try first.stageCreate(
            activation: active,
            request: CompanionTodoCreate(text: "visible in scene B", dueAt: nil)
        )
        wait(for: [reloaded], timeout: 2)
        XCTAssertNil(observedError)
        XCTAssertEqual(observed?.items.map(\.text), ["visible in scene B"])
    }

    func testCompanionTodoConcurrentClaimHasExactlyOneOwner() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let key = Data(repeating: 32, count: 32)
        let first = try CompanionTodoRepository(containerURL: directory, cipher: CompanionTodoAESGCMCipher(keyData: key))
        let second = try CompanionTodoRepository(containerURL: directory, cipher: CompanionTodoAESGCMCipher(keyData: key))
        let active = activation(QueueIdentity(origin: "https://example.org", namespace: String(repeating: "k", count: 43)))
        _ = try first.stageCreate(activation: active, request: CompanionTodoCreate(text: "claim once", dueAt: nil))
        let resultLock = NSLock()
        var claims: [ClaimedCompanionTodoOperation] = []
        var errors: [Error] = []

        DispatchQueue.concurrentPerform(iterations: 2) { index in
            do {
                if let claim = try (index == 0 ? first : second).claimDue(activation: active) {
                    resultLock.lock(); claims.append(claim); resultLock.unlock()
                }
            } catch {
                resultLock.lock(); errors.append(error); resultLock.unlock()
            }
        }

        XCTAssertTrue(errors.isEmpty)
        XCTAssertEqual(claims.count, 1)
    }

    func testCompanionTodoRevisionIsFrozenUntilExplicitMigration() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try CompanionTodoRepository(
            containerURL: directory,
            cipher: CompanionTodoAESGCMCipher(keyData: Data(repeating: 33, count: 32))
        )
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "m", count: 43))
        let firstActivation = activation(identity, revision: 1)
        let secondActivation = activation(identity, revision: 2)
        _ = try repository.stageCreate(
            activation: firstActivation,
            request: CompanionTodoCreate(text: "frozen", dueAt: nil)
        )
        let oldClaim = try XCTUnwrap(repository.claimDue(activation: firstActivation))

        XCTAssertEqual(try repository.snapshot(activation: secondActivation), .empty)
        XCTAssertNil(try repository.claimDue(activation: secondActivation))
        XCTAssertTrue(try repository.migratePartition(
            identity: identity,
            fromRevision: 1,
            to: secondActivation
        ))
        let migrated = try XCTUnwrap(repository.claimDue(activation: secondActivation))
        XCTAssertNotEqual(migrated.operation.id, oldClaim.operation.id)
        XCTAssertNotEqual(migrated.owner, oldClaim.owner)
        XCTAssertEqual(migrated.operation.attemptCount, 0)
        XCTAssertNil(migrated.operation.nextAttemptAt)
        XCTAssertNil(migrated.operation.lastError)
    }

    func testCompanionTodoLeaseExpiresRebindsAndRejectsStaleOwner() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try CompanionTodoRepository(
            containerURL: directory,
            cipher: CompanionTodoAESGCMCipher(keyData: Data(repeating: 8, count: 32))
        )
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "u", count: 43))
        let active = activation(identity)
        let start = Date(timeIntervalSince1970: 2_000)
        let localID = try repository.stageCreate(
            activation: active,
            request: CompanionTodoCreate(text: "created offline", dueAt: nil),
            now: start
        )
        _ = try repository.stagePatch(
            activation: active,
            todoID: localID,
            patch: CompanionTodoPatch(text: "edited offline", dueAt: nil, dueAtSet: false, done: nil, expectedHostRevision: nil),
            now: start.addingTimeInterval(1)
        )
        let first = try XCTUnwrap(repository.claimDue(activation: active, now: start, leaseDuration: 10))
        XCTAssertNil(
            try repository.claimDue(activation: active, now: start.addingTimeInterval(1), leaseDuration: 10),
            "a later operation for the same TODO must not overtake its leased create"
        )
        XCTAssertThrowsError(try repository.completeCreate(
            activation: active,
            operationID: first.operation.id,
            owner: first.owner,
            created: companionTodo(id: "10000000-0000-0000-0000-000000000001", text: "expired", dueAt: nil),
            now: start.addingTimeInterval(10)
        )) { error in
            XCTAssertEqual(error as? TodoStateError, .staleClaim)
        }
        let second = try XCTUnwrap(repository.claimDue(activation: active, now: start.addingTimeInterval(11), leaseDuration: 10))
        XCTAssertEqual(first.operation.id, second.operation.id)
        XCTAssertNotEqual(first.owner, second.owner)

        XCTAssertThrowsError(try repository.completeCreate(
            activation: active,
            operationID: first.operation.id,
            owner: first.owner,
            created: companionTodo(id: "10000000-0000-0000-0000-000000000001", text: "server", dueAt: nil)
        )) { error in
            XCTAssertEqual(error as? TodoStateError, .staleClaim)
        }

        let server = companionTodo(
            id: "10000000-0000-0000-0000-000000000001",
            text: "server",
            dueAt: nil
        )
        try repository.completeCreate(
            activation: active,
            operationID: second.operation.id,
            owner: second.owner,
            created: server,
            now: start.addingTimeInterval(12)
        )
        let snapshot = try repository.snapshot(activation: active)
        XCTAssertEqual(snapshot.items.map(\.id), [server.id])
        XCTAssertEqual(snapshot.items.first?.text, "edited offline")
        XCTAssertTrue(snapshot.items.first?.pending == true)
        XCTAssertEqual(snapshot.pendingOperations, 1)
    }

    func testCompanionTodoConflictRebasesDesiredDoneWithANewOperationID() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }
        let repository = try CompanionTodoRepository(
            containerURL: directory,
            cipher: CompanionTodoAESGCMCipher(keyData: Data(repeating: 9, count: 32))
        )
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "w", count: 43))
        let active = activation(identity)
        let item = CompanionTodoItem(
            id: "30000000-0000-0000-0000-000000000001",
            text: "projected",
            dueAt: nil,
            done: false,
            originKind: .note,
            originHostKind: "note",
            originHostID: "note-one",
            hostRevision: 2,
            createdAt: Date(timeIntervalSince1970: 3_000),
            updatedAt: Date(timeIntervalSince1970: 3_000)
        )
        try repository.replaceServerSnapshot(activation: active, items: [item])
        let firstCreatedAt = Date(timeIntervalSince1970: 3_010)
        let originalID = try repository.stagePatch(
            activation: active,
            todoID: item.id,
            patch: CompanionTodoPatch(text: nil, dueAt: nil, dueAtSet: false, done: true, expectedHostRevision: 2),
            now: firstCreatedAt
        )
        let laterID = try repository.stagePatch(
            activation: active,
            todoID: item.id,
            patch: CompanionTodoPatch(text: nil, dueAt: nil, dueAtSet: false, done: false, expectedHostRevision: 2),
            now: firstCreatedAt
        )
        let original = try XCTUnwrap(repository.claimDue(activation: active))
        let refreshed = CompanionTodoItem(
            id: item.id,
            text: item.text,
            dueAt: nil,
            done: false,
            originKind: .note,
            originHostKind: "note",
            originHostID: "note-one",
            hostRevision: 3,
            createdAt: item.createdAt,
            updatedAt: Date(timeIntervalSince1970: 3_100)
        )

        try repository.rebaseDoneConflict(
            activation: active,
            operationID: original.operation.id,
            owner: original.owner,
            desiredDone: true,
            current: refreshed,
            snapshot: [refreshed],
            now: firstCreatedAt.addingTimeInterval(2)
        )

        let rebased = try XCTUnwrap(repository.claimDue(activation: active))
        XCTAssertEqual(original.operation.id, originalID)
        XCTAssertNotEqual(rebased.operation.id, original.operation.id)
        XCTAssertNotEqual(rebased.operation.id, laterID, "a later operation must not overtake a rebased conflict")
        XCTAssertEqual(rebased.operation.createdAt, firstCreatedAt)
        XCTAssertEqual(rebased.operation.patch?.done, true)
        XCTAssertEqual(rebased.operation.patch?.expectedHostRevision, 3)

        let completed = CompanionTodoItem(
            id: refreshed.id,
            text: refreshed.text,
            dueAt: nil,
            done: true,
            originKind: .note,
            originHostKind: "note",
            originHostID: "note-one",
            hostRevision: 4,
            createdAt: refreshed.createdAt,
            updatedAt: firstCreatedAt.addingTimeInterval(3)
        )
        try repository.completePatch(
            activation: active,
            operationID: rebased.operation.id,
            owner: rebased.owner,
            updated: completed
        )
        let later = try XCTUnwrap(repository.claimDue(activation: active))
        XCTAssertEqual(later.operation.id, laterID)
        XCTAssertEqual(later.operation.patch?.expectedHostRevision, 4)
    }

    func testCompanionTodoAPIUsesPaginationNamespaceAndStableIdempotencyKey() async throws {
        let namespace = String(repeating: "v", count: 43)
        let identity = SessionIdentity(
            origin: "https://example.org",
            namespace: namespace,
            representationContract: "v3"
        )
        let firstItem = companionTodo(id: "20000000-0000-0000-0000-000000000001", text: "first", dueAt: nil)
        let secondItem = companionTodo(id: "20000000-0000-0000-0000-000000000002", text: "second", dueAt: nil)
        let encoder = JSONEncoder()
        encoder.dateEncodingStrategy = .iso8601
        let transport = RecordingTransport { request in
            let response = try XCTUnwrap(HTTPURLResponse(
                url: try XCTUnwrap(request.url),
                statusCode: 200,
                httpVersion: "HTTP/1.1",
                headerFields: ["X-WebTag-Data-Namespace": namespace]
            ))
            let after = URLComponents(url: try XCTUnwrap(request.url), resolvingAgainstBaseURL: false)?
                .queryItems?.first(where: { $0.name == "after" })?.value
            let page: [String: Any]
            if after == nil {
                page = ["items": [try jsonObject(firstItem, encoder: encoder)], "next_after": "page-two"]
            } else {
                XCTAssertEqual(after, "page-two")
                page = ["items": [try jsonObject(secondItem, encoder: encoder)]]
            }
            return (try JSONSerialization.data(withJSONObject: page), response)
        }
        let api = CompanionTodoAPIClient(transport: transport)

        let listed = try await api.listTodos(identity: identity, installationToken: "secret").get()

        XCTAssertEqual(listed.map(\.id), [firstItem.id, secondItem.id])
        XCTAssertEqual(transport.requests.count, 2)
        XCTAssertEqual(transport.requests.first?.url?.path, "/api/todos")
        XCTAssertEqual(
            URLComponents(url: try XCTUnwrap(transport.requests.first?.url), resolvingAgainstBaseURL: false)?
                .queryItems?.first(where: { $0.name == "limit" })?.value,
            "200"
        )

        let createTransport = RecordingTransport { request in
            XCTAssertEqual(request.value(forHTTPHeaderField: "Idempotency-Key"), "offline-operation-id")
            XCTAssertEqual(request.httpMethod, "POST")
            let response = try XCTUnwrap(HTTPURLResponse(
                url: try XCTUnwrap(request.url),
                statusCode: 201,
                httpVersion: "HTTP/1.1",
                headerFields: ["X-WebTag-Data-Namespace": namespace]
            ))
            return (try encoder.encode(firstItem), response)
        }
        let createAPI = CompanionTodoAPIClient(transport: createTransport)
        _ = try await createAPI.createTodo(
            identity: identity,
            installationToken: "secret",
            request: CompanionTodoCreate(text: "first", dueAt: nil),
            idempotencyKey: "offline-operation-id"
        ).get()
        XCTAssertEqual(createTransport.requests.count, 1)
    }

    @MainActor
    func testLifecycleReloadDoesNotOverwriteUnsavedCredentialDrafts() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: directory) }

        let keychain = KeychainCredentialStore()
        keychain.clear()
        defer { keychain.clear() }

        let repository = try AppGroupQueueRepository(containerURL: directory)
        let identity = QueueIdentity(origin: "https://example.org", namespace: String(repeating: "p", count: 43))
        try activate(repository, identity)
        let model = WebTagSettingsModel(clock: FakeSettingsClock(now: Date(timeIntervalSince1970: 60_000)), repository: repository)

        model.origin = "https://half-typed.example"
        model.installationToken = "half-typed-token"
        _ = try repository.enqueue(url: "https://example.org/queued", identity: identity)

        model.reload()

        XCTAssertEqual(model.projection.queue.total, 1, "the reload really did replace the projection")
        XCTAssertEqual(model.origin, "https://half-typed.example")
        XCTAssertEqual(model.installationToken, "half-typed-token")
    }
}

private func companionTodo(
    id: String,
    text: String,
    dueAt: Date?,
    done: Bool = false
) -> CompanionTodoItem {
    CompanionTodoItem(
        id: id,
        text: text,
        dueAt: dueAt,
        done: done,
        originKind: .standalone,
        hostRevision: 0,
        completedAt: done ? Date(timeIntervalSince1970: 4_000) : nil,
        createdAt: Date(timeIntervalSince1970: 3_000),
        updatedAt: Date(timeIntervalSince1970: 4_000)
    )
}

private func migrationQueueEntry(id: String, activation: ActivationIdentity) -> QueueEntry {
    QueueEntry(
        id: id,
        schemaVersion: 1,
        url: "https://example.org/\(id)",
        idempotencyKey: "key-\(id)-r\(activation.revision)",
        requestFingerprint: "fingerprint-\(id)",
        identity: activation.queueIdentity,
        identityRevision: activation.revision,
        state: .pendingSubmit,
        createdAt: Date(timeIntervalSince1970: 1_000),
        firstFailedAt: nil,
        attemptCount: 0,
        nextAttemptAt: nil,
        lastError: nil,
        lastErrorCode: nil,
        lastHTTPStatus: nil,
        linkID: nil,
        jobID: nil,
        leaseOwner: nil,
        leaseExpiresAt: nil
    )
}

private func jsonObject(_ item: CompanionTodoItem, encoder: JSONEncoder) throws -> [String: Any] {
    try XCTUnwrap(try JSONSerialization.jsonObject(with: encoder.encode(item)) as? [String: Any])
}

// MARK: - Settings projection test doubles

private func settingsQueueFixtureURL() -> URL {
    URL(fileURLWithPath: #filePath)
        .deletingLastPathComponent()
        .deletingLastPathComponent()
        .deletingLastPathComponent()
        .appendingPathComponent("shared/fixtures/queue-states.json")
}

private let settingsFixtureIdentity = QueueIdentity(
    origin: "https://example.org",
    namespace: String(repeating: "q", count: 43)
)

private func settingsEntry(
    id: String,
    state: QueueState,
    url: String = "https://example.org/row",
    firstFailedAt: Date? = nil,
    nextAttemptAt: Date? = nil
) -> QueueEntry {
    QueueEntry(
        id: id,
        schemaVersion: 1,
        url: url,
        idempotencyKey: "key-\(id)",
        requestFingerprint: "fingerprint-\(id)",
        identity: settingsFixtureIdentity,
        identityRevision: 1,
        state: state,
        createdAt: Date(timeIntervalSince1970: 1_000),
        firstFailedAt: firstFailedAt,
        attemptCount: 0,
        nextAttemptAt: nextAttemptAt,
        lastError: nil,
        lastErrorCode: nil,
        lastHTTPStatus: nil,
        linkID: nil,
        jobID: nil,
        leaseOwner: nil,
        leaseExpiresAt: nil
    )
}

/// Milliseconds in the shared fixture, seconds in `Date`. Kept in one place so the
/// two platforms cannot drift into disagreeing about the unit.
private func settingsFixtureDate(_ milliseconds: Any?) -> Date? {
    guard let milliseconds = milliseconds as? Double else { return nil }
    return Date(timeIntervalSince1970: milliseconds / 1000)
}

private func settingsQueueEntry(from row: [String: Any]) throws -> QueueEntry {
    let id = try XCTUnwrap(row["id"] as? String)
    let state = try XCTUnwrap(QueueState(rawValue: try XCTUnwrap(row["state"] as? String)))
    return settingsEntry(
        id: id,
        state: state,
        url: (row["url"] as? String) ?? "",
        firstFailedAt: settingsFixtureDate(row["first_failed_at"]),
        nextAttemptAt: settingsFixtureDate(row["next_attempt_at"])
    )
}

private func settingsRecent(mismatch: Bool, notBefore: Date?, reason: String?) -> RecentResult {
    RecentResult(
        url: mismatch ? "" : "https://example.org/recent",
        linkID: mismatch ? "" : "44444444-4444-4444-4444-444444444444",
        jobID: mismatch ? nil : "job-1",
        status: "failed",
        createdAt: Date(timeIntervalSince1970: 5_000),
        identity: settingsFixtureIdentity,
        identityRevision: 1,
        refreshNotBefore: notBefore,
        refreshBlockReason: reason,
        isIdentityMismatch: mismatch
    )
}

/// Counts every touch so "the deadline performs no database work" can be asserted
/// rather than assumed. There is no write path here at all: reaching for one would
/// not compile, which is a stronger statement than a counter that stays at zero.
private final class CountingSettingsRepository: SettingsSnapshotReading {
    var snapshot: ActiveSessionSnapshot?
    var entries: [QueueEntry] = []
    var recentResult: RecentResult?
    private(set) var reads = 0
    private(set) var writes = 0

    func activeSessionSnapshot() throws -> ActiveSessionSnapshot? {
        reads += 1
        return snapshot
    }

    func list(identity: QueueIdentity?) throws -> [QueueEntry] {
        reads += 1
        return entries
    }

    func recent(identity: QueueIdentity?) throws -> RecentResult? {
        reads += 1
        return recentResult
    }
}

/// Wall clock under test control. `advance(to:)` runs exactly the callbacks whose
/// deadline the move crossed, so "one millisecond before" and "at the deadline" are
/// two different observations rather than one racy one.
private final class FakeSettingsClock: SettingsWallClock {
    private(set) var now: Date
    private var pending: [(deadline: Date, body: () -> Void)] = []

    init(now: Date) {
        self.now = now
    }

    var pendingCount: Int { pending.count }

    func schedule(after delay: TimeInterval, _ body: @escaping () -> Void) {
        pending.append((now.addingTimeInterval(delay), body))
    }

    func advance(to date: Date) {
        now = date
        let due = pending.filter { $0.deadline <= date }
        pending.removeAll { $0.deadline <= date }
        for item in due { item.body() }
    }
}
