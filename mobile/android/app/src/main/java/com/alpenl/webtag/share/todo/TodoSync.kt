package com.alpenl.webtag.share.todo

import android.content.Context
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingWorkPolicy
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import com.alpenl.webtag.share.contract.ActiveSessionSnapshot
import com.alpenl.webtag.share.contract.CredentialConfig
import com.alpenl.webtag.share.contract.ErrorKind
import com.alpenl.webtag.share.contract.RetryPolicy
import com.alpenl.webtag.share.data.ClaimedTodoOperation
import com.alpenl.webtag.share.data.TodoCasOutcome
import com.alpenl.webtag.share.data.TodoClaimOutcome
import com.alpenl.webtag.share.data.TodoGateDecision
import com.alpenl.webtag.share.data.TodoOutboxKind
import com.alpenl.webtag.share.data.TodoOutboxOperation
import com.alpenl.webtag.share.data.TodoOutboxState
import com.alpenl.webtag.share.data.TodoRepository
import com.alpenl.webtag.share.data.TodoSyncGateState
import com.alpenl.webtag.share.network.ApiResult
import com.alpenl.webtag.share.network.ClassifiedFailure
import com.alpenl.webtag.share.network.WebTagCompanionApi
import com.alpenl.webtag.share.queue.MobileClock
import com.alpenl.webtag.share.queue.MobileRuntime
import java.util.concurrent.TimeUnit

class TodoSyncScheduler(
    private val context: Context,
    private val repository: TodoRepository,
    private val activeSession: () -> ActiveSessionSnapshot?,
) {
    fun schedule(now: Long = System.currentTimeMillis()) {
        val activation = activeSession() ?: return
        val next = repository.nextScheduleAt(activation, now) ?: return
        val request = OneTimeWorkRequestBuilder<TodoSyncWorker>()
            .setConstraints(Constraints.Builder().setRequiredNetworkType(NetworkType.CONNECTED).build())
            .setInitialDelay(delayMillis(next, now), TimeUnit.MILLISECONDS)
            .build()
        WorkManager.getInstance(context).enqueueUniqueWork(
            UNIQUE_WORK_NAME,
            ExistingWorkPolicy.REPLACE,
            request,
        )
    }

    companion object {
        const val UNIQUE_WORK_NAME = "webtag-todo-sync"

        fun delayMillis(nextAttemptAt: Long, now: Long): Long = (nextAttemptAt - now).coerceAtLeast(0)
    }
}

data class TodoSyncResult(
    val pushed: Int,
    val pulled: Boolean,
    val error: ErrorKind? = null,
    val nextAttemptAt: Long? = null,
    val casOutcome: TodoCasOutcome? = null,
)

class TodoSyncCoordinator(
    private val repository: TodoRepository,
    private val api: WebTagCompanionApi,
    private val activeConfiguration: () -> CredentialConfig?,
    private val activeSession: () -> ActiveSessionSnapshot?,
    private val clock: MobileClock,
) {
    fun synchronize(maxPushes: Int = 32): TodoSyncResult {
        val configuration = activeConfiguration() ?: return TodoSyncResult(0, false, ErrorKind.HTTP_401)
        val activation = activeSession() ?: return TodoSyncResult(0, false, ErrorKind.IDENTITY_MISMATCH)
        if (!configuration.matches(activation)) return TodoSyncResult(0, false, ErrorKind.IDENTITY_MISMATCH)

        val gate = when (val decision = repository.gate(activation, clock.now())) {
            TodoGateDecision.IdentityChanged -> return TodoSyncResult(0, false, ErrorKind.IDENTITY_MISMATCH)
            is TodoGateDecision.Deferred -> return TodoSyncResult(0, false, nextAttemptAt = decision.nextAttemptAt)
            is TodoGateDecision.Blocked -> return TodoSyncResult(0, false, decision.state.errorKind())
            is TodoGateDecision.Ready -> decision
        }

        when (val capabilities = api.capabilities(activation.identity, configuration.installationToken)) {
            is ApiResult.Failure -> {
                val now = clock.now()
                val state = gateStateFor(capabilities.failure.kind)
                val next = if (state == TodoSyncGateState.RETRY_WAIT) {
                    retryAt(gate.attemptCount + 1, now, capabilities.failure.retryAfter)
                } else {
                    null
                }
                val committed = repository.recordCapabilitiesFailure(
                    activation,
                    state,
                    next,
                    capabilities.failure.kind.name,
                )
                return if (committed == TodoCasOutcome.APPLIED) {
                    TodoSyncResult(0, false, capabilities.failure.kind, next)
                } else {
                    rejectedResult(0, committed)
                }
            }
            is ApiResult.Success -> {
                val committed = repository.recordCapabilities(
                    activation,
                    capabilities.value.todos,
                    capabilities.value.home,
                    capabilities.value.inbox,
                )
                if (committed != TodoCasOutcome.APPLIED) {
                    return rejectedResult(0, committed)
                }
                if (!capabilities.value.todos) return TodoSyncResult(0, false)
            }
        }

        var pushed = 0
        while (pushed < maxPushes) {
            val claimed = when (val outcome = repository.claimDue(activation, clock.now(), LEASE_MILLIS)) {
                is TodoClaimOutcome.Claimed -> outcome.value
                TodoClaimOutcome.NoWork -> break
                TodoClaimOutcome.IdentityChanged -> return rejectedResult(pushed, TodoCasOutcome.IDENTITY_CHANGED)
                TodoClaimOutcome.IntegrityFailure -> return rejectedResult(pushed, TodoCasOutcome.INTEGRITY_FAILURE)
            }
            when (val sent = send(configuration, activation, claimed)) {
                TodoSendOutcome.Pushed -> pushed += 1
                TodoSendOutcome.HandledFailure -> break
                is TodoSendOutcome.Rejected -> return rejectedResult(pushed, sent.outcome)
            }
        }

        return when (val todos = api.listTodos(activation.identity, configuration.installationToken)) {
            is ApiResult.Failure -> TodoSyncResult(pushed, false, todos.failure.kind)
            is ApiResult.Success -> when (repository.replaceServerSnapshot(activation, todos.value, clock.now())) {
                TodoCasOutcome.APPLIED -> TodoSyncResult(pushed, true)
                TodoCasOutcome.IDENTITY_CHANGED -> rejectedResult(pushed, TodoCasOutcome.IDENTITY_CHANGED)
                TodoCasOutcome.STALE_CLAIM -> rejectedResult(pushed, TodoCasOutcome.STALE_CLAIM)
                TodoCasOutcome.INTEGRITY_FAILURE -> rejectedResult(pushed, TodoCasOutcome.INTEGRITY_FAILURE)
            }
        }
    }

    private fun send(
        configuration: CredentialConfig,
        activation: ActiveSessionSnapshot,
        claimed: ClaimedTodoOperation,
    ): TodoSendOutcome {
        val operation = claimed.operation
        val result = when (TodoOutboxKind.fromWire(operation.entity.kind)) {
            TodoOutboxKind.CREATE -> api.createTodo(
                activation.identity,
                configuration.installationToken,
                requireNotNull(operation.create),
                operation.entity.operationId,
            )
            TodoOutboxKind.PATCH -> api.patchTodo(
                activation.identity,
                configuration.installationToken,
                operation.entity.targetTodoId,
                requireNotNull(operation.patch),
                operation.entity.operationId,
            )
            TodoOutboxKind.DELETE -> api.deleteTodo(
                activation.identity,
                configuration.installationToken,
                operation.entity.targetTodoId,
                operation.entity.operationId,
            )
        }
        return when (result) {
            is ApiResult.Success -> {
                val now = clock.now()
                val committed = when (TodoOutboxKind.fromWire(operation.entity.kind)) {
                    TodoOutboxKind.CREATE -> repository.completeCreate(
                        operation,
                        claimed.owner,
                        activation,
                        result.value as TodoItem,
                        now,
                    )
                    TodoOutboxKind.PATCH -> repository.completePatch(
                        operation,
                        claimed.owner,
                        activation,
                        result.value as TodoItem,
                        now,
                    )
                    TodoOutboxKind.DELETE -> repository.completeDelete(operation, claimed.owner, activation, now)
                }
                committed.toSendOutcome(pushed = true)
            }
            is ApiResult.Failure -> handleFailure(claimed, result.failure, activation, configuration)
        }
    }

    private fun handleFailure(
        claimed: ClaimedTodoOperation,
        failure: ClassifiedFailure,
        activation: ActiveSessionSnapshot,
        configuration: CredentialConfig,
    ): TodoSendOutcome {
        val operation = claimed.operation
        if (failure.kind == ErrorKind.HTTP_409 && operation.patch?.done != null) {
            val list = api.listTodos(activation.identity, configuration.installationToken)
            if (list is ApiResult.Success) {
                val current = list.value.firstOrNull { it.id == operation.entity.targetTodoId }
                if (TodoConflictPolicy.resolveDesiredDone(operation.patch.done, current) == TodoConflictResolution.CONVERGED) {
                    return repository.completeConvergedConflict(
                        operation,
                        claimed.owner,
                        activation,
                        list.value,
                        clock.now(),
                    ).toSendOutcome(pushed = true)
                }
            }
            return repository.updateOperation(
                operation.entity,
                claimed.owner,
                activation,
                TodoOutboxState.CONFLICT,
                null,
                failure.kind.name,
                clock.now(),
            ).toSendOutcome(pushed = false)
        }
        val state = stateFor(failure.kind)
        val now = clock.now()
        val next = if (state == TodoOutboxState.RETRY_WAIT) {
            retryAt(operation.entity.attemptCount + 1, now, failure.retryAfter)
        } else {
            null
        }
        return repository.updateOperation(
            operation.entity,
            claimed.owner,
            activation,
            state,
            next,
            failure.kind.name,
            now,
        ).toSendOutcome(pushed = false)
    }

    private fun TodoCasOutcome.toSendOutcome(pushed: Boolean): TodoSendOutcome = when (this) {
        TodoCasOutcome.APPLIED -> if (pushed) TodoSendOutcome.Pushed else TodoSendOutcome.HandledFailure
        else -> TodoSendOutcome.Rejected(this)
    }

    private fun rejectedResult(pushed: Int, outcome: TodoCasOutcome): TodoSyncResult = when (outcome) {
        TodoCasOutcome.IDENTITY_CHANGED -> TodoSyncResult(
            pushed,
            false,
            ErrorKind.IDENTITY_MISMATCH,
            casOutcome = outcome,
        )
        TodoCasOutcome.STALE_CLAIM -> TodoSyncResult(pushed, false, casOutcome = outcome)
        TodoCasOutcome.INTEGRITY_FAILURE -> TodoSyncResult(
            pushed,
            false,
            ErrorKind.LOCAL_DATA_UNREADABLE,
            casOutcome = outcome,
        )
        TodoCasOutcome.APPLIED -> error("applied TODO CAS cannot reject synchronization")
    }

    private fun CredentialConfig.matches(snapshot: ActiveSessionSnapshot): Boolean =
        activationRevision == snapshot.activationRevision && sessionIdentity() == snapshot.identity

    companion object {
        const val LEASE_MILLIS = 30_000L

        fun stateFor(kind: ErrorKind): TodoOutboxState = when (kind) {
            ErrorKind.NO_NETWORK,
            ErrorKind.DNS_TIMEOUT,
            ErrorKind.CONNECTION_RESET,
            ErrorKind.CLIENT_DEADLINE,
            ErrorKind.HTTP_408,
            ErrorKind.HTTP_425,
            ErrorKind.HTTP_429_RATE_LIMIT,
            ErrorKind.HTTP_429_COOLDOWN,
            ErrorKind.HTTP_5XX,
            -> TodoOutboxState.RETRY_WAIT
            ErrorKind.HTTP_401, ErrorKind.HTTP_403 -> TodoOutboxState.BLOCKED_AUTH
            ErrorKind.IDENTITY_MISMATCH -> TodoOutboxState.BLOCKED_IDENTITY
            ErrorKind.HTTP_409 -> TodoOutboxState.CONFLICT
            else -> TodoOutboxState.FAILED_PERMANENT
        }

        fun gateStateFor(kind: ErrorKind): TodoSyncGateState = when (stateFor(kind)) {
            TodoOutboxState.RETRY_WAIT -> TodoSyncGateState.RETRY_WAIT
            TodoOutboxState.BLOCKED_AUTH -> TodoSyncGateState.BLOCKED_AUTH
            TodoOutboxState.BLOCKED_IDENTITY -> TodoSyncGateState.BLOCKED_IDENTITY
            else -> TodoSyncGateState.FAILED_PERMANENT
        }

        fun retryAt(attempt: Int, now: Long, retryAfter: String? = null): Long {
            val exponent = (attempt - 1).coerceIn(0, 10)
            val backoff = (30_000L shl exponent).coerceAtMost(TimeUnit.HOURS.toMillis(6))
            val requested = RetryPolicy.retryAfterMillis(retryAfter, now) ?: 0L
            return runCatching { Math.addExact(now, maxOf(backoff, requested)) }.getOrDefault(Long.MAX_VALUE)
        }
    }

    private sealed interface TodoSendOutcome {
        data object Pushed : TodoSendOutcome
        data object HandledFailure : TodoSendOutcome
        data class Rejected(val outcome: TodoCasOutcome) : TodoSendOutcome
    }
}

class TodoSyncWorker(
    context: Context,
    params: WorkerParameters,
) : CoroutineWorker(context, params) {
    override suspend fun doWork(): Result {
        val runtime = MobileRuntime.get(applicationContext)
        runtime.todoSyncCoordinator.synchronize()
        runtime.todoScheduler.schedule()
        // Durable Room deadlines are the sole retry authority. Returning retry() here would add a
        // second, resettable WorkManager backoff state to the same operation.
        return Result.success()
    }
}

private fun TodoSyncGateState.errorKind(): ErrorKind = when (this) {
    TodoSyncGateState.BLOCKED_AUTH -> ErrorKind.HTTP_401
    TodoSyncGateState.BLOCKED_IDENTITY -> ErrorKind.IDENTITY_MISMATCH
    TodoSyncGateState.UNSUPPORTED -> ErrorKind.NONE
    TodoSyncGateState.FAILED_PERMANENT -> ErrorKind.LOCAL_DATA_UNREADABLE
    TodoSyncGateState.READY, TodoSyncGateState.RETRY_WAIT -> ErrorKind.NONE
}
