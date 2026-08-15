import SwiftUI
import UIKit
import BackgroundTasks

@main
struct WebTagShareApp: App {
    @UIApplicationDelegateAdaptor(WebTagShareAppDelegate.self) private var appDelegate

    var body: some Scene {
        WindowGroup {
            ContentView()
        }
    }
}

final class WebTagShareAppDelegate: NSObject, UIApplicationDelegate {
    private let repository: AppGroupQueueRepository?
    private let coordinator: ShareSubmissionCoordinator?
    private let wakeScheduler: DurableQueueWakeScheduler
    private var wakeObserver: NSObjectProtocol?

    override init() {
        let wakeScheduler = DurableQueueWakeScheduler.shared
        let repository = try? AppGroupQueueRepository()
        self.repository = repository
        self.wakeScheduler = wakeScheduler
        self.coordinator = repository.map { ShareSubmissionCoordinator(repository: $0, wakeScheduler: wakeScheduler) }
        super.init()
        wakeObserver = NotificationCenter.default.addObserver(
            forName: .webTagQueueWakeDue,
            object: nil,
            queue: .main
        ) { [weak self] _ in self?.reconcileAndDrain() }
        BackgroundUploadSessionController.shared.taskCompletionHandler = { [weak self] result in
            await self?.handleBackgroundCompletion(result)
        }
    }

    deinit {
        if let wakeObserver { NotificationCenter.default.removeObserver(wakeObserver) }
    }

    func application(_ application: UIApplication, didFinishLaunchingWithOptions launchOptions: [UIApplication.LaunchOptionsKey: Any]? = nil) -> Bool {
        _ = BackgroundTasksQueueWakeAdapter.shared.register { [weak self] task in
            self?.handleDurableWake(task)
        }
        repairActiveIdentity()
        reconcileAndDrain()
        return true
    }

    func applicationDidBecomeActive(_ application: UIApplication) {
        reconcileAndDrain()
    }

    func application(_ application: UIApplication, handleEventsForBackgroundURLSession identifier: String, completionHandler: @escaping () -> Void) {
        BackgroundUploadSessionController.shared.handleEvents(identifier: identifier, completionHandler: completionHandler)
    }

    private func reconcileAndDrain() {
        guard let coordinator, let repository else { return }
        Task {
            let active = await BackgroundUploadSessionController.shared.reconcile(
                repository: repository,
                now: Date()
            )
            await coordinator.reconcileAndDrain(excluding: active)
        }
    }

    private func repairActiveIdentity() {
        guard let repository else { return }
        do {
            guard let stored = try KeychainCredentialStore().loadConfig() else { return }
            // Activation revision is an authorization boundary. A credential
            // mismatch must remain inert until the user activates explicitly.
            guard try repository.activeSessionSnapshot() == stored.activation else { return }
        } catch {
            // A mismatched or unreadable credential is intentionally left unusable.
        }
    }

    private func handleBackgroundCompletion(_ result: BackgroundUploadResult) async {
        await coordinator?.handleBackgroundCompletion(result)
    }

    private func handleDurableWake(_ backgroundTask: BGAppRefreshTask) {
        wakeScheduler.markDurableWakeDelivered()
        let state = DurableDrainState()
        let workHolder = DurableDrainTaskHolder()
        backgroundTask.expirationHandler = {
            state.expire()
            workHolder.cancel()
        }
        guard let coordinator, let repository else {
            backgroundTask.setTaskCompleted(success: false)
            return
        }
        let work = Task {
            let active = await BackgroundUploadSessionController.shared.reconcile(repository: repository, now: Date())
            await coordinator.reconcileAndDrain(
                excluding: active,
                maxItems: 8,
                shouldContinue: { !state.isExpired }
            )
            backgroundTask.setTaskCompleted(success: !state.isExpired)
        }
        workHolder.store(work)
    }
}

private final class DurableDrainTaskHolder {
    private let lock = NSLock()
    private var task: Task<Void, Never>?
    private var cancelled = false

    func store(_ task: Task<Void, Never>) {
        lock.lock()
        self.task = task
        let shouldCancel = cancelled
        lock.unlock()
        if shouldCancel { task.cancel() }
    }

    func cancel() {
        lock.lock()
        cancelled = true
        let task = task
        lock.unlock()
        task?.cancel()
    }
}

private final class DurableDrainState {
    private let lock = NSLock()
    private var expired = false

    var isExpired: Bool {
        lock.lock(); defer { lock.unlock() }
        return expired
    }

    func expire() {
        lock.lock(); expired = true; lock.unlock()
    }
}
