package com.alpenl.webtag.share.data

import androidx.room.Dao
import androidx.room.ColumnInfo
import androidx.room.Entity
import androidx.room.Index
import androidx.room.Insert
import androidx.room.OnConflictStrategy
import androidx.room.Query
import androidx.room.Update
import androidx.sqlite.db.SupportSQLiteDatabase
import com.alpenl.webtag.share.contract.ActiveSessionSnapshot
import com.alpenl.webtag.share.contract.QueueIdentity
import com.alpenl.webtag.share.contract.newUuid
import com.alpenl.webtag.share.security.AndroidKeystoreCipher
import com.alpenl.webtag.share.security.EncryptedValue
import com.alpenl.webtag.share.todo.TodoCreate
import com.alpenl.webtag.share.todo.TodoItem
import com.alpenl.webtag.share.todo.TodoItemCodec
import com.alpenl.webtag.share.todo.TodoOriginKind
import com.alpenl.webtag.share.todo.TodoPatch
import java.util.UUID
import org.json.JSONObject

const val TODO_OUTBOX_ENVELOPE_VERSION = 3
private const val TODO_OUTBOX_ENVELOPE_V2 = 2

@Entity(
    tableName = "todo_cache",
    primaryKeys = ["apiOrigin", "clientDataNamespace", "activationRevision", "todoId"],
    indices = [Index(value = ["apiOrigin", "clientDataNamespace", "activationRevision", "serverUpdatedAt"])],
)
data class TodoCacheEntity(
    val apiOrigin: String,
    val clientDataNamespace: String,
    val activationRevision: Long,
    val todoId: String,
    val payloadCiphertext: ByteArray,
    val payloadNonce: ByteArray,
    val cryptoVersion: Int,
    val serverUpdatedAt: Long,
    val fetchedAt: Long,
)

@Entity(
    tableName = "todo_outbox",
    indices = [
        Index(value = ["apiOrigin", "clientDataNamespace", "activationRevision", "state", "nextAttemptAt"]),
        Index(value = ["apiOrigin", "clientDataNamespace", "targetTodoId"]),
    ],
)
data class TodoOutboxEntity(
    @androidx.room.PrimaryKey val operationId: String,
    val apiOrigin: String,
    val clientDataNamespace: String,
    val targetTodoId: String,
    val kind: String,
    val payloadCiphertext: ByteArray,
    val payloadNonce: ByteArray,
    val cryptoVersion: Int,
    val state: String,
    val attemptCount: Int,
    val nextAttemptAt: Long?,
    val lastErrorKind: String?,
    val leaseOwner: String?,
    val leaseExpiresAt: Long?,
    val createdAt: Long,
    val updatedAt: Long,
    @ColumnInfo(defaultValue = "0") val activationRevision: Long,
    @ColumnInfo(defaultValue = "1") val envelopeVersion: Int,
    val quarantinedLegacyState: String? = null,
)

@Entity(
    tableName = "todo_sync_state",
    primaryKeys = ["apiOrigin", "clientDataNamespace"],
)
data class TodoSyncStateEntity(
    val apiOrigin: String,
    val clientDataNamespace: String,
    val todosEnabled: Boolean,
    val homeEnabled: Boolean,
    val inboxEnabled: Boolean,
    val lastSyncedAt: Long?,
    @ColumnInfo(defaultValue = "0") val activationRevision: Long,
    @ColumnInfo(defaultValue = "'ready'") val gateState: String,
    @ColumnInfo(defaultValue = "0") val attemptCount: Int,
    val nextAttemptAt: Long?,
    val lastErrorKind: String?,
)

enum class TodoOutboxKind(val wireValue: String) {
    CREATE("create"),
    PATCH("patch"),
    DELETE("delete"),
    ;

    companion object {
        fun fromWire(value: String): TodoOutboxKind = entries.firstOrNull { it.wireValue == value }
            ?: error("unknown TODO outbox kind: $value")
    }
}

enum class TodoOutboxState(val wireValue: String) {
    PENDING("pending"),
    RETRY_WAIT("retry_wait"),
    BLOCKED_AUTH("blocked_auth"),
    BLOCKED_IDENTITY("blocked_identity"),
    CONFLICT("conflict"),
    FAILED_PERMANENT("failed_permanent"),
    QUARANTINED_LEGACY("quarantined_legacy"),
    ;

    companion object {
        fun fromWire(value: String): TodoOutboxState = entries.firstOrNull { it.wireValue == value }
            ?: error("unknown TODO outbox state: $value")
    }
}

enum class TodoSyncGateState(val wireValue: String) {
    READY("ready"),
    RETRY_WAIT("retry_wait"),
    BLOCKED_AUTH("blocked_auth"),
    BLOCKED_IDENTITY("blocked_identity"),
    UNSUPPORTED("unsupported"),
    FAILED_PERMANENT("failed_permanent"),
    ;

    companion object {
        fun fromWire(value: String): TodoSyncGateState = entries.firstOrNull { it.wireValue == value }
            ?: error("unknown TODO sync gate state: $value")
    }
}

enum class TodoCasOutcome {
    APPLIED,
    IDENTITY_CHANGED,
    STALE_CLAIM,
    INTEGRITY_FAILURE,
}

sealed interface TodoClaimOutcome {
    data class Claimed(val value: ClaimedTodoOperation) : TodoClaimOutcome
    data object NoWork : TodoClaimOutcome
    data object IdentityChanged : TodoClaimOutcome
    data object IntegrityFailure : TodoClaimOutcome
}

sealed interface TodoSnapshotOutcome {
    data class Applied(val snapshot: TodoLocalSnapshot) : TodoSnapshotOutcome
    data object IdentityChanged : TodoSnapshotOutcome
    data object IntegrityFailure : TodoSnapshotOutcome
}

sealed interface TodoGateDecision {
    data class Ready(val attemptCount: Int) : TodoGateDecision
    data class Deferred(val nextAttemptAt: Long, val attemptCount: Int) : TodoGateDecision
    data class Blocked(val state: TodoSyncGateState, val attemptCount: Int) : TodoGateDecision
    data object IdentityChanged : TodoGateDecision
}

@Dao
interface TodoDao {
    @Query(
        "SELECT * FROM todo_cache WHERE apiOrigin = :origin AND clientDataNamespace = :namespace " +
            "AND activationRevision = :activationRevision " +
            "ORDER BY serverUpdatedAt DESC, todoId",
    )
    fun listCache(origin: String, namespace: String, activationRevision: Long): List<TodoCacheEntity>

    @Insert(onConflict = OnConflictStrategy.REPLACE)
    fun upsertCache(items: List<TodoCacheEntity>)

    @Insert(onConflict = OnConflictStrategy.REPLACE)
    fun upsertCache(item: TodoCacheEntity)

    @Query(
        "DELETE FROM todo_cache WHERE apiOrigin = :origin AND clientDataNamespace = :namespace " +
            "AND activationRevision = :activationRevision",
    )
    fun deleteCacheForActivation(origin: String, namespace: String, activationRevision: Long)

    @Query(
        "DELETE FROM todo_cache WHERE apiOrigin = :origin AND clientDataNamespace = :namespace " +
            "AND activationRevision = :activationRevision AND todoId = :todoId",
    )
    fun deleteCachedTodo(origin: String, namespace: String, activationRevision: Long, todoId: String)

    @Query(
        "SELECT * FROM todo_outbox WHERE apiOrigin = :origin AND clientDataNamespace = :namespace " +
            "ORDER BY createdAt, operationId",
    )
    fun listOutbox(origin: String, namespace: String): List<TodoOutboxEntity>

    @Query("SELECT * FROM todo_outbox ORDER BY createdAt, operationId")
    fun listAllOutbox(): List<TodoOutboxEntity>

    @Query("SELECT * FROM todo_outbox WHERE operationId = :operationId")
    fun findOperation(operationId: String): TodoOutboxEntity?

    @Insert(onConflict = OnConflictStrategy.ABORT)
    fun insertOutbox(item: TodoOutboxEntity)

    @Update
    fun updateOutbox(item: TodoOutboxEntity): Int

    @Query("DELETE FROM todo_outbox WHERE operationId = :operationId AND leaseOwner = :owner")
    fun deleteClaimedOutbox(operationId: String, owner: String): Int

    @Query(
        "SELECT * FROM todo_outbox WHERE apiOrigin = :origin AND clientDataNamespace = :namespace " +
            "AND activationRevision = :activationRevision AND envelopeVersion = 3 " +
            "AND state IN ('pending', 'retry_wait') " +
            "AND (nextAttemptAt IS NULL OR nextAttemptAt <= :now) " +
            "AND (leaseOwner IS NULL OR leaseExpiresAt IS NULL OR leaseExpiresAt <= :now) " +
            "ORDER BY createdAt, operationId LIMIT 1",
    )
    fun findDueOutbox(origin: String, namespace: String, activationRevision: Long, now: Long): TodoOutboxEntity?

    @Query(
        "SELECT MIN(CASE " +
            "WHEN leaseOwner IS NOT NULL AND leaseExpiresAt > :now THEN leaseExpiresAt " +
            "WHEN nextAttemptAt IS NULL OR nextAttemptAt < :now THEN :now ELSE nextAttemptAt END) " +
            "FROM todo_outbox WHERE apiOrigin = :origin AND clientDataNamespace = :namespace " +
            "AND activationRevision = :activationRevision AND envelopeVersion = 3 " +
            "AND state IN ('pending', 'retry_wait')",
    )
    fun earliestOutboxAt(origin: String, namespace: String, activationRevision: Long, now: Long): Long?

    @Query(
        "UPDATE todo_outbox SET payloadCiphertext = :payloadCiphertext, payloadNonce = :payloadNonce, " +
            "cryptoVersion = :cryptoVersion, leaseOwner = :owner, leaseExpiresAt = :leaseExpiresAt, updatedAt = :now " +
            "WHERE operationId = :operationId AND apiOrigin = :origin AND clientDataNamespace = :namespace " +
            "AND activationRevision = :activationRevision AND envelopeVersion = 3 " +
            "AND updatedAt = :expectedUpdatedAt " +
            "AND state IN ('pending', 'retry_wait') " +
            "AND (nextAttemptAt IS NULL OR nextAttemptAt <= :now) " +
            "AND (leaseOwner IS NULL OR leaseExpiresAt IS NULL OR leaseExpiresAt <= :now) " +
            "AND EXISTS (SELECT 1 FROM active_session WHERE id = 1 " +
            "AND apiOrigin = :origin AND clientDataNamespace = :namespace " +
            "AND activationRevision = :activationRevision)",
    )
    fun claimSealed(
        operationId: String,
        origin: String,
        namespace: String,
        activationRevision: Long,
        expectedUpdatedAt: Long,
        payloadCiphertext: ByteArray,
        payloadNonce: ByteArray,
        cryptoVersion: Int,
        owner: String,
        leaseExpiresAt: Long,
        now: Long,
    ): Int

    @Query(
        "DELETE FROM todo_outbox WHERE operationId = :operationId " +
            "AND apiOrigin = :origin AND clientDataNamespace = :namespace " +
            "AND state = 'quarantined_legacy'",
    )
    fun deleteQuarantined(operationId: String, origin: String, namespace: String): Int

    @Insert(onConflict = OnConflictStrategy.REPLACE)
    fun upsertSyncState(state: TodoSyncStateEntity)

    @Query(
        "SELECT * FROM todo_sync_state WHERE apiOrigin = :origin AND clientDataNamespace = :namespace",
    )
    fun syncState(origin: String, namespace: String): TodoSyncStateEntity?
}

data class TodoOutboxOperation(
    val entity: TodoOutboxEntity,
    val create: TodoCreate? = null,
    val patch: TodoPatch? = null,
)

data class ClaimedTodoOperation(
    val operation: TodoOutboxOperation,
    val owner: String,
)

data class TodoLegacyOperation(
    val operationId: String,
    val kind: TodoOutboxKind?,
    val targetTodoId: String,
    val originalState: TodoOutboxState?,
    val summary: String?,
    val recoverable: Boolean,
)

data class TodoLocalSnapshot(
    val items: List<TodoItem>,
    val pendingOperations: Int,
    val blockedOperations: Int,
    val legacyOperations: List<TodoLegacyOperation>,
    val lastSyncedAt: Long?,
    val todosEnabled: Boolean?,
) {
    companion object {
        val EMPTY = TodoLocalSnapshot(emptyList(), 0, 0, emptyList(), null, null)
    }
}

class TodoRepository(
    private val database: QueueDatabase,
    private val cipher: AndroidKeystoreCipher,
) {
    private val dao = database.todoDao()
    private val queueDao = database.queueDao()

    fun snapshot(activation: ActiveSessionSnapshot): TodoSnapshotOutcome =
        database.runInTransaction(java.util.concurrent.Callable {
            if (!activeSessionMatches(activation)) return@Callable TodoSnapshotOutcome.IdentityChanged
            val identity = activation.queueIdentity()
            val cached = try {
                dao.listCache(identity.origin, identity.namespace, activation.activationRevision).map(::decodeCache)
            } catch (_: Exception) {
                return@Callable TodoSnapshotOutcome.IntegrityFailure
            }
            val outbox = dao.listOutbox(identity.origin, identity.namespace)
            val legacy = outbox
                .filter { it.state == TodoOutboxState.QUARANTINED_LEGACY.wireValue }
                .map(::legacyPreview)
            val operations = try {
                outbox
                    .filter {
                        it.activationRevision == activation.activationRevision &&
                            it.state != TodoOutboxState.QUARANTINED_LEGACY.wireValue
                    }
                    .map(::decodeOperation)
            } catch (_: Exception) {
                return@Callable TodoSnapshotOutcome.IntegrityFailure
            }
            val state = dao.syncState(identity.origin, identity.namespace)
                ?.takeIf { it.activationRevision == activation.activationRevision }
            TodoSnapshotOutcome.Applied(
                TodoLocalSnapshot(
                    items = applyOperations(cached, operations),
                    pendingOperations = operations.count { it.entity.state.isPending() },
                    blockedOperations = operations.count { !it.entity.state.isPending() } + legacy.size,
                    legacyOperations = legacy,
                    lastSyncedAt = state?.lastSyncedAt,
                    todosEnabled = state?.todosEnabled,
                ),
            )
        })

    fun replaceServerSnapshot(
        activation: ActiveSessionSnapshot,
        items: List<TodoItem>,
        now: Long,
    ): TodoCasOutcome = database.runInTransaction(java.util.concurrent.Callable {
        if (!activeSessionMatches(activation)) return@Callable TodoCasOutcome.IDENTITY_CHANGED
        val identity = activation.queueIdentity()
        dao.deleteCacheForActivation(identity.origin, identity.namespace, activation.activationRevision)
        if (items.isNotEmpty()) {
            dao.upsertCache(items.map { encodeCache(activation, it, now) })
        }
        val previous = currentSyncState(activation)
        dao.upsertSyncState(
            (previous ?: freshSyncState(activation)).copy(lastSyncedAt = now),
        )
        TodoCasOutcome.APPLIED
    })

    fun recordCapabilities(
        activation: ActiveSessionSnapshot,
        todos: Boolean,
        home: Boolean,
        inbox: Boolean,
    ): TodoCasOutcome = database.runInTransaction(java.util.concurrent.Callable {
        if (!activeSessionMatches(activation)) return@Callable TodoCasOutcome.IDENTITY_CHANGED
        val previous = currentSyncState(activation)
        dao.upsertSyncState(
            (previous ?: freshSyncState(activation)).copy(
                todosEnabled = todos,
                homeEnabled = home,
                inboxEnabled = inbox,
                gateState = if (todos) TodoSyncGateState.READY.wireValue else TodoSyncGateState.UNSUPPORTED.wireValue,
                attemptCount = 0,
                nextAttemptAt = null,
                lastErrorKind = null,
            ),
        )
        TodoCasOutcome.APPLIED
    })

    fun recordCapabilitiesFailure(
        activation: ActiveSessionSnapshot,
        state: TodoSyncGateState,
        nextAttemptAt: Long?,
        errorKind: String,
    ): TodoCasOutcome = database.runInTransaction(java.util.concurrent.Callable {
        if (!activeSessionMatches(activation)) return@Callable TodoCasOutcome.IDENTITY_CHANGED
        val previous = currentSyncState(activation) ?: freshSyncState(activation)
        dao.upsertSyncState(
            previous.copy(
                gateState = state.wireValue,
                attemptCount = previous.attemptCount + 1,
                nextAttemptAt = nextAttemptAt,
                lastErrorKind = errorKind,
            ),
        )
        TodoCasOutcome.APPLIED
    })

    fun gate(activation: ActiveSessionSnapshot, now: Long): TodoGateDecision =
        database.runInTransaction(java.util.concurrent.Callable {
            if (!activeSessionMatches(activation)) return@Callable TodoGateDecision.IdentityChanged
            val state = currentSyncState(activation) ?: freshSyncState(activation).also(dao::upsertSyncState)
            when (TodoSyncGateState.fromWire(state.gateState)) {
                TodoSyncGateState.READY -> TodoGateDecision.Ready(state.attemptCount)
                TodoSyncGateState.RETRY_WAIT -> {
                    val deadline = state.nextAttemptAt
                    if (deadline == null || deadline <= now) TodoGateDecision.Ready(state.attemptCount)
                    else TodoGateDecision.Deferred(deadline, state.attemptCount)
                }
                else -> TodoGateDecision.Blocked(TodoSyncGateState.fromWire(state.gateState), state.attemptCount)
            }
        })

    fun stageCreate(
        activation: ActiveSessionSnapshot,
        request: TodoCreate,
        now: Long = System.currentTimeMillis(),
    ): String {
        require(request.text.isNotBlank() && request.text.length <= 4096)
        val localTodoId = newUuid()
        insertOperation(
            activation,
            newUuid(),
            localTodoId,
            TodoOutboxKind.CREATE,
            JSONObject().put("text", request.text).put("due_at", request.dueAt ?: JSONObject.NULL).toString(),
            now,
        )
        return localTodoId
    }

    fun stagePatch(
        activation: ActiveSessionSnapshot,
        todoId: String,
        patch: TodoPatch,
        now: Long = System.currentTimeMillis(),
    ): String {
        val operationId = newUuid()
        val payload = JSONObject()
        patch.text?.let { payload.put("text", it) }
        payload.put("due_at_set", patch.dueAtSet)
        if (patch.dueAtSet) payload.put("due_at", patch.dueAt ?: JSONObject.NULL)
        patch.done?.let { payload.put("done", it) }
        patch.expectedHostRevision?.let { payload.put("expected_host_revision", it) }
        require(payload.length() > 1 || !payload.has("due_at_set")) { "empty TODO patch" }
        insertOperation(activation, operationId, todoId, TodoOutboxKind.PATCH, payload.toString(), now)
        return operationId
    }

    fun stageDelete(
        activation: ActiveSessionSnapshot,
        todoId: String,
        now: Long = System.currentTimeMillis(),
    ): String = newUuid().also {
        insertOperation(activation, it, todoId, TodoOutboxKind.DELETE, "{}", now)
    }

    fun claimDue(
        activation: ActiveSessionSnapshot,
        now: Long,
        leaseMillis: Long,
    ): TodoClaimOutcome = database.runInTransaction(java.util.concurrent.Callable {
        if (!activeSessionMatches(activation)) return@Callable TodoClaimOutcome.IdentityChanged
        val identity = activation.queueIdentity()
        val entity = dao.findDueOutbox(identity.origin, identity.namespace, activation.activationRevision, now)
            ?: return@Callable TodoClaimOutcome.NoWork
        val operation = try {
            decodeOperation(entity)
        } catch (_: Exception) {
            return@Callable TodoClaimOutcome.IntegrityFailure
        }
        val owner = newUuid()
        val expiresAt = runCatching { Math.addExact(now, leaseMillis) }.getOrDefault(Long.MAX_VALUE)
        val sealedClaim = try {
            transition(entity, leaseOwner = owner, leaseExpiresAt = expiresAt, now = now)
        } catch (_: Exception) {
            return@Callable TodoClaimOutcome.IntegrityFailure
        }
        if (dao.claimSealed(
                entity.operationId,
                identity.origin,
                identity.namespace,
                activation.activationRevision,
                entity.updatedAt,
                sealedClaim.payloadCiphertext,
                sealedClaim.payloadNonce,
                sealedClaim.cryptoVersion,
                owner,
                expiresAt,
                now,
            ) != 1
        ) {
            return@Callable if (activeSessionMatches(activation)) TodoClaimOutcome.NoWork
            else TodoClaimOutcome.IdentityChanged
        }
        TodoClaimOutcome.Claimed(
            ClaimedTodoOperation(
                operation.copy(entity = sealedClaim),
                owner,
            ),
        )
    })

    fun nextScheduleAt(activation: ActiveSessionSnapshot, now: Long): Long? =
        database.runInTransaction(java.util.concurrent.Callable {
            if (!activeSessionMatches(activation)) return@Callable null
            val identity = activation.queueIdentity()
            val outboxAt = dao.earliestOutboxAt(
                identity.origin,
                identity.namespace,
                activation.activationRevision,
                now,
            ) ?: return@Callable null
            when (val decision = gate(activation, now)) {
                is TodoGateDecision.Ready -> outboxAt
                is TodoGateDecision.Deferred -> maxOf(outboxAt, decision.nextAttemptAt)
                is TodoGateDecision.Blocked, TodoGateDecision.IdentityChanged -> null
            }
        })

    fun completeCreate(
        operation: TodoOutboxOperation,
        owner: String,
        activation: ActiveSessionSnapshot,
        created: TodoItem,
        now: Long,
    ): TodoCasOutcome = database.runInTransaction(java.util.concurrent.Callable {
        val current = claimed(operation.entity, owner, activation, now)
        if (current.outcome != null) return@Callable current.outcome
        val entity = requireNotNull(current.entity)
        val decoded = try {
            decodeOperation(entity)
        } catch (_: Exception) {
            return@Callable TodoCasOutcome.INTEGRITY_FAILURE
        }
        if (TodoOutboxKind.fromWire(entity.kind) != TodoOutboxKind.CREATE || decoded.create == null) {
            return@Callable TodoCasOutcome.INTEGRITY_FAILURE
        }
        val identity = entity.identity()
        val dependencies = dao.listOutbox(identity.origin, identity.namespace).filter {
            it.operationId != entity.operationId &&
                it.activationRevision == activation.activationRevision &&
                it.targetTodoId == entity.targetTodoId
        }
        val rebound = try {
            dependencies.map { transition(it, targetTodoId = created.id, now = now) }
        } catch (_: Exception) {
            return@Callable TodoCasOutcome.INTEGRITY_FAILURE
        }
        dao.upsertCache(encodeCache(activation, created, now))
        rebound.forEach(dao::updateOutbox)
        if (dao.deleteClaimedOutbox(entity.operationId, owner) == 1) TodoCasOutcome.APPLIED
        else TodoCasOutcome.STALE_CLAIM
    })

    fun completePatch(
        operation: TodoOutboxOperation,
        owner: String,
        activation: ActiveSessionSnapshot,
        updated: TodoItem,
        now: Long,
    ): TodoCasOutcome = completeWithCache(operation, owner, activation, updated, now)

    fun completeDelete(
        operation: TodoOutboxOperation,
        owner: String,
        activation: ActiveSessionSnapshot,
        now: Long,
    ): TodoCasOutcome = database.runInTransaction(java.util.concurrent.Callable {
        val current = claimed(operation.entity, owner, activation, now)
        if (current.outcome != null) return@Callable current.outcome
        val entity = requireNotNull(current.entity)
        try {
            decodeOperation(entity)
        } catch (_: Exception) {
            return@Callable TodoCasOutcome.INTEGRITY_FAILURE
        }
        if (TodoOutboxKind.fromWire(entity.kind) != TodoOutboxKind.DELETE) {
            return@Callable TodoCasOutcome.INTEGRITY_FAILURE
        }
        dao.deleteCachedTodo(
            entity.apiOrigin,
            entity.clientDataNamespace,
            activation.activationRevision,
            entity.targetTodoId,
        )
        if (dao.deleteClaimedOutbox(entity.operationId, owner) == 1) TodoCasOutcome.APPLIED
        else TodoCasOutcome.STALE_CLAIM
    })

    fun completeConvergedConflict(
        operation: TodoOutboxOperation,
        owner: String,
        activation: ActiveSessionSnapshot,
        items: List<TodoItem>,
        now: Long,
    ): TodoCasOutcome = database.runInTransaction(java.util.concurrent.Callable {
        val current = claimed(operation.entity, owner, activation, now)
        if (current.outcome != null) return@Callable current.outcome
        try {
            decodeOperation(requireNotNull(current.entity))
        } catch (_: Exception) {
            return@Callable TodoCasOutcome.INTEGRITY_FAILURE
        }
        val identity = activation.queueIdentity()
        dao.deleteCacheForActivation(identity.origin, identity.namespace, activation.activationRevision)
        if (items.isNotEmpty()) {
            dao.upsertCache(items.map { encodeCache(activation, it, now) })
        }
        val previous = currentSyncState(activation) ?: freshSyncState(activation)
        dao.upsertSyncState(previous.copy(lastSyncedAt = now))
        if (dao.deleteClaimedOutbox(operation.entity.operationId, owner) == 1) TodoCasOutcome.APPLIED
        else TodoCasOutcome.STALE_CLAIM
    })

    fun discardOperation(
        operation: TodoOutboxOperation,
        owner: String,
        activation: ActiveSessionSnapshot,
        now: Long,
    ): TodoCasOutcome = database.runInTransaction(java.util.concurrent.Callable {
        val current = claimed(operation.entity, owner, activation, now)
        if (current.outcome != null) return@Callable current.outcome
        try {
            decodeOperation(requireNotNull(current.entity))
        } catch (_: Exception) {
            return@Callable TodoCasOutcome.INTEGRITY_FAILURE
        }
        if (dao.deleteClaimedOutbox(operation.entity.operationId, owner) == 1) TodoCasOutcome.APPLIED
        else TodoCasOutcome.STALE_CLAIM
    })

    fun recoverLegacyOperation(
        activation: ActiveSessionSnapshot,
        operationId: String,
        now: Long = System.currentTimeMillis(),
    ): TodoCasOutcome = database.runInTransaction(java.util.concurrent.Callable {
        if (!activeSessionMatches(activation)) return@Callable TodoCasOutcome.IDENTITY_CHANGED
        val entity = dao.findOperation(operationId) ?: return@Callable TodoCasOutcome.STALE_CLAIM
        val identity = activation.queueIdentity()
        if (entity.apiOrigin != identity.origin || entity.clientDataNamespace != identity.namespace ||
            entity.state != TodoOutboxState.QUARANTINED_LEGACY.wireValue
        ) {
            return@Callable TodoCasOutcome.STALE_CLAIM
        }
        try {
            TodoOutboxState.fromWire(requireNotNull(entity.quarantinedLegacyState)).also {
                require(it != TodoOutboxState.QUARANTINED_LEGACY)
            }
        } catch (_: Exception) {
            return@Callable TodoCasOutcome.INTEGRITY_FAILURE
        }
        val payload = try {
            legacyPayload(entity)
        } catch (_: Exception) {
            return@Callable TodoCasOutcome.INTEGRITY_FAILURE
        }
        val restored = try {
            sealPayload(
                entity.copy(
                    cryptoVersion = 1,
                    state = TodoOutboxState.PENDING.wireValue,
                    attemptCount = 0,
                    nextAttemptAt = null,
                    lastErrorKind = null,
                    leaseOwner = null,
                    leaseExpiresAt = null,
                    updatedAt = now,
                    activationRevision = activation.activationRevision,
                    envelopeVersion = TODO_OUTBOX_ENVELOPE_VERSION,
                    quarantinedLegacyState = null,
                ),
                payload,
            )
        } catch (_: Exception) {
            return@Callable TodoCasOutcome.INTEGRITY_FAILURE
        }
        if (dao.updateOutbox(restored) == 1) TodoCasOutcome.APPLIED else TodoCasOutcome.STALE_CLAIM
    })

    fun deleteLegacyOperation(
        activation: ActiveSessionSnapshot,
        operationId: String,
    ): TodoCasOutcome = database.runInTransaction(java.util.concurrent.Callable {
        if (!activeSessionMatches(activation)) return@Callable TodoCasOutcome.IDENTITY_CHANGED
        val identity = activation.queueIdentity()
        if (dao.deleteQuarantined(operationId, identity.origin, identity.namespace) == 1) {
            TodoCasOutcome.APPLIED
        } else {
            TodoCasOutcome.STALE_CLAIM
        }
    })

    fun updateOperation(
        entity: TodoOutboxEntity,
        owner: String,
        activation: ActiveSessionSnapshot,
        state: TodoOutboxState,
        nextAttemptAt: Long?,
        errorKind: String?,
        now: Long,
    ): TodoCasOutcome = database.runInTransaction(java.util.concurrent.Callable {
        val current = claimed(entity, owner, activation, now)
        if (current.outcome != null) return@Callable current.outcome
        val updated = try {
            transition(
                requireNotNull(current.entity),
                state = state,
                attemptCount = requireNotNull(current.entity).attemptCount + 1,
                nextAttemptAt = nextAttemptAt,
                lastErrorKind = errorKind,
                leaseOwner = null,
                leaseExpiresAt = null,
                now = now,
            )
        } catch (_: Exception) {
            return@Callable TodoCasOutcome.INTEGRITY_FAILURE
        }
        if (dao.updateOutbox(updated) == 1) TodoCasOutcome.APPLIED else TodoCasOutcome.STALE_CLAIM
    })

    private fun completeWithCache(
        operation: TodoOutboxOperation,
        owner: String,
        activation: ActiveSessionSnapshot,
        updated: TodoItem,
        now: Long,
    ): TodoCasOutcome = database.runInTransaction(java.util.concurrent.Callable {
        val current = claimed(operation.entity, owner, activation, now)
        if (current.outcome != null) return@Callable current.outcome
        val entity = requireNotNull(current.entity)
        try {
            decodeOperation(entity)
        } catch (_: Exception) {
            return@Callable TodoCasOutcome.INTEGRITY_FAILURE
        }
        if (TodoOutboxKind.fromWire(entity.kind) != TodoOutboxKind.PATCH) {
            return@Callable TodoCasOutcome.INTEGRITY_FAILURE
        }
        dao.upsertCache(encodeCache(activation, updated, now))
        if (dao.deleteClaimedOutbox(entity.operationId, owner) == 1) TodoCasOutcome.APPLIED
        else TodoCasOutcome.STALE_CLAIM
    })

    private fun insertOperation(
        activation: ActiveSessionSnapshot,
        operationId: String,
        targetTodoId: String,
        kind: TodoOutboxKind,
        payload: String,
        now: Long,
    ) = database.runInTransaction {
        check(activeSessionMatches(activation)) { "TODO activation changed before staging" }
        val identity = activation.queueIdentity()
        val empty = TodoOutboxEntity(
            operationId,
            identity.origin,
            identity.namespace,
            targetTodoId,
            kind.wireValue,
            byteArrayOf(),
            byteArrayOf(),
            1,
            TodoOutboxState.PENDING.wireValue,
            0,
            null,
            null,
            null,
            null,
            now,
            now,
            activation.activationRevision,
            TODO_OUTBOX_ENVELOPE_VERSION,
        )
        val encrypted = cipher.encrypt(payload, TodoOutboxEnvelope.canonicalAad(empty))
        dao.insertOutbox(
            empty.copy(
                payloadCiphertext = encrypted.ciphertext,
                payloadNonce = encrypted.nonce,
                cryptoVersion = encrypted.version,
            ),
        )
    }

    private fun transition(
        entity: TodoOutboxEntity,
        targetTodoId: String = entity.targetTodoId,
        state: TodoOutboxState = TodoOutboxState.fromWire(entity.state),
        attemptCount: Int = entity.attemptCount,
        nextAttemptAt: Long? = entity.nextAttemptAt,
        lastErrorKind: String? = entity.lastErrorKind,
        leaseOwner: String? = entity.leaseOwner,
        leaseExpiresAt: Long? = entity.leaseExpiresAt,
        now: Long,
    ): TodoOutboxEntity {
        val payload = decodePayload(entity)
        val updated = entity.copy(
            targetTodoId = targetTodoId,
            state = state.wireValue,
            attemptCount = attemptCount,
            nextAttemptAt = nextAttemptAt,
            lastErrorKind = lastErrorKind,
            leaseOwner = leaseOwner,
            leaseExpiresAt = leaseExpiresAt,
            updatedAt = now,
        )
        return sealPayload(updated, payload)
    }

    private fun claimed(
        expected: TodoOutboxEntity,
        owner: String,
        activation: ActiveSessionSnapshot,
        now: Long,
    ): ClaimedClassification {
        if (!activeSessionMatches(activation)) return ClaimedClassification(TodoCasOutcome.IDENTITY_CHANGED)
        val current = dao.findOperation(expected.operationId)
            ?: return ClaimedClassification(TodoCasOutcome.STALE_CLAIM)
        if (current.activationRevision != activation.activationRevision ||
            current.leaseOwner != owner || current.leaseExpiresAt == null || current.leaseExpiresAt <= now
        ) {
            return ClaimedClassification(TodoCasOutcome.STALE_CLAIM)
        }
        return ClaimedClassification(entity = current)
    }

    private data class ClaimedClassification(
        val outcome: TodoCasOutcome? = null,
        val entity: TodoOutboxEntity? = null,
    )

    private fun encodeCache(activation: ActiveSessionSnapshot, item: TodoItem, fetchedAt: Long): TodoCacheEntity {
        val identity = activation.queueIdentity()
        val encrypted = cipher.encrypt(
            TodoItemCodec.encode(item),
            cacheAad(item.id, identity, activation.activationRevision),
        )
        return TodoCacheEntity(
            identity.origin,
            identity.namespace,
            activation.activationRevision,
            item.id,
            encrypted.ciphertext,
            encrypted.nonce,
            encrypted.version,
            item.updatedAt,
            fetchedAt,
        )
    }

    private fun decodeCache(entity: TodoCacheEntity): TodoItem {
        val raw = cipher.decrypt(
            EncryptedValue(entity.payloadCiphertext, entity.payloadNonce, entity.cryptoVersion),
            cacheAad(entity.todoId, entity.identity(), entity.activationRevision),
        )
        return TodoItemCodec.decode(raw).also {
            require(it.id == entity.todoId) { "TODO cache identity mismatch" }
        }
    }

    private fun decodePayload(entity: TodoOutboxEntity): String {
        require(entity.envelopeVersion == TODO_OUTBOX_ENVELOPE_VERSION) { "unsupported TODO outbox envelope" }
        require(entity.activationRevision > 0) { "invalid TODO activation revision" }
        TodoOutboxKind.fromWire(entity.kind)
        TodoOutboxState.fromWire(entity.state)
        val payload = cipher.decrypt(
            EncryptedValue(entity.payloadCiphertext, entity.payloadNonce, entity.cryptoVersion),
            TodoOutboxEnvelope.canonicalAad(entity),
        )
        TodoOutboxEnvelope.validatePayload(TodoOutboxKind.fromWire(entity.kind), payload)
        return payload
    }

    private fun decodeOperation(entity: TodoOutboxEntity): TodoOutboxOperation {
        return decodeOperationPayload(entity, decodePayload(entity))
    }

    private fun decodeOperationPayload(entity: TodoOutboxEntity, payload: String): TodoOutboxOperation {
        val json = JSONObject(payload)
        return when (TodoOutboxKind.fromWire(entity.kind)) {
            TodoOutboxKind.CREATE -> TodoOutboxOperation(
                entity,
                create = TodoCreate(
                    json.getString("text"),
                    if (json.isNull("due_at")) null else json.getLong("due_at"),
                ),
            )
            TodoOutboxKind.PATCH -> TodoOutboxOperation(
                entity,
                patch = TodoPatch(
                    text = json.optString("text").takeIf { json.has("text") },
                    dueAt = if (!json.optBoolean("due_at_set") || json.isNull("due_at")) null
                    else json.getLong("due_at"),
                    dueAtSet = json.optBoolean("due_at_set"),
                    done = json.optBoolean("done").takeIf { json.has("done") },
                    expectedHostRevision = json.optLong("expected_host_revision").takeIf {
                        json.has("expected_host_revision")
                    },
                ),
            )
            TodoOutboxKind.DELETE -> TodoOutboxOperation(entity)
        }
    }

    private fun legacyPreview(entity: TodoOutboxEntity): TodoLegacyOperation {
        val decoded = runCatching {
            val originalState = TodoOutboxState.fromWire(requireNotNull(entity.quarantinedLegacyState))
            require(originalState != TodoOutboxState.QUARANTINED_LEGACY)
            val operation = decodeOperationPayload(entity, legacyPayload(entity))
            originalState to operation
        }.getOrNull()
        return TodoLegacyOperation(
            operationId = entity.operationId,
            kind = runCatching { TodoOutboxKind.fromWire(entity.kind) }.getOrNull(),
            targetTodoId = entity.targetTodoId,
            originalState = decoded?.first,
            summary = decoded?.second?.create?.text ?: decoded?.second?.patch?.text,
            recoverable = decoded != null,
        )
    }

    private fun legacyPayload(entity: TodoOutboxEntity): String {
        require(runCatching { UUID.fromString(entity.operationId) }.isSuccess) { "invalid legacy operation ID" }
        require(runCatching { UUID.fromString(entity.targetTodoId) }.isSuccess) { "invalid legacy target ID" }
        val kind = TodoOutboxKind.fromWire(entity.kind)
        val payload = when (entity.envelopeVersion) {
            TODO_OUTBOX_ENVELOPE_VERSION -> decodePayload(entity)
            TODO_OUTBOX_ENVELOPE_V2 -> cipher.decrypt(
                EncryptedValue(entity.payloadCiphertext, entity.payloadNonce, entity.cryptoVersion),
                TodoOutboxEnvelope.canonicalV2Aad(entity),
            )
            1 -> cipher.decrypt(
                EncryptedValue(entity.payloadCiphertext, entity.payloadNonce, entity.cryptoVersion),
                legacyV1Aad(entity.operationId, entity.apiOrigin, entity.clientDataNamespace),
            )
            else -> error("unsupported legacy TODO envelope")
        }
        TodoOutboxEnvelope.validatePayload(kind, payload)
        return payload
    }

    private fun sealPayload(entity: TodoOutboxEntity, payload: String): TodoOutboxEntity {
        require(entity.envelopeVersion == TODO_OUTBOX_ENVELOPE_VERSION)
        require(entity.cryptoVersion == 1)
        val encrypted = cipher.encrypt(payload, TodoOutboxEnvelope.canonicalAad(entity))
        require(encrypted.version == entity.cryptoVersion)
        return entity.copy(
            payloadCiphertext = encrypted.ciphertext,
            payloadNonce = encrypted.nonce,
            cryptoVersion = encrypted.version,
        )
    }

    private fun applyOperations(cached: List<TodoItem>, operations: List<TodoOutboxOperation>): List<TodoItem> {
        val items = cached.associateByTo(linkedMapOf(), TodoItem::id)
        operations.forEach { operation ->
            val entity = operation.entity
            if (entity.state !in setOf("pending", "retry_wait", "blocked_auth")) return@forEach
            when (TodoOutboxKind.fromWire(entity.kind)) {
                TodoOutboxKind.CREATE -> {
                    val request = requireNotNull(operation.create)
                    items[entity.targetTodoId] = TodoItem(
                        entity.targetTodoId,
                        request.text,
                        request.dueAt,
                        false,
                        TodoOriginKind.STANDALONE,
                        null,
                        null,
                        null,
                        0,
                        null,
                        entity.createdAt,
                        entity.updatedAt,
                        false,
                        null,
                        true,
                        true,
                    )
                }
                TodoOutboxKind.PATCH -> items[entity.targetTodoId]?.let { current ->
                    val patch = requireNotNull(operation.patch)
                    items[entity.targetTodoId] = current.copy(
                        text = patch.text ?: current.text,
                        dueAt = if (patch.dueAtSet) patch.dueAt else current.dueAt,
                        done = patch.done ?: current.done,
                        completedAt = when (patch.done) {
                            true -> entity.createdAt
                            false -> null
                            null -> current.completedAt
                        },
                        pending = true,
                    )
                }
                TodoOutboxKind.DELETE -> items.remove(entity.targetTodoId)
            }
        }
        return items.values.toList()
    }

    private fun currentSyncState(activation: ActiveSessionSnapshot): TodoSyncStateEntity? {
        val identity = activation.queueIdentity()
        return dao.syncState(identity.origin, identity.namespace)
            ?.takeIf { it.activationRevision == activation.activationRevision }
    }

    private fun freshSyncState(activation: ActiveSessionSnapshot): TodoSyncStateEntity {
        val identity = activation.queueIdentity()
        return TodoSyncStateEntity(
            identity.origin,
            identity.namespace,
            true,
            false,
            false,
            null,
            activation.activationRevision,
            TodoSyncGateState.READY.wireValue,
            0,
            null,
            null,
        )
    }

    private fun activeSessionMatches(activation: ActiveSessionSnapshot): Boolean {
        val current = queueDao.activeSession() ?: return false
        val identity = activation.identity
        return current.apiOrigin == identity.origin &&
            current.clientDataNamespace == identity.clientDataNamespace &&
            current.representationContract == identity.representationContract &&
            current.activationRevision == activation.activationRevision
    }

    private fun cacheAad(todoId: String, identity: QueueIdentity, activationRevision: Long): String =
        cacheV2Aad(todoId, identity.origin, identity.namespace, activationRevision)

    companion object {
        internal fun prepareActivation(
            dao: TodoDao,
            cipher: AndroidKeystoreCipher,
            activation: ActiveSessionSnapshot,
            now: Long = System.currentTimeMillis(),
        ) {
            dao.listAllOutbox()
                .filter { it.activationRevision != activation.activationRevision && it.state.isPending() }
                .forEach { entity ->
                    if (entity.envelopeVersion != TODO_OUTBOX_ENVELOPE_VERSION) {
                        dao.updateOutbox(
                            entity.copy(
                                state = TodoOutboxState.QUARANTINED_LEGACY.wireValue,
                                quarantinedLegacyState = entity.state,
                                leaseOwner = null,
                                leaseExpiresAt = null,
                                updatedAt = now,
                            ),
                        )
                        return@forEach
                    }
                    runCatching {
                        val payload = cipher.decrypt(
                            EncryptedValue(entity.payloadCiphertext, entity.payloadNonce, entity.cryptoVersion),
                            TodoOutboxEnvelope.canonicalAad(entity),
                        )
                        val blocked = entity.copy(
                            state = TodoOutboxState.BLOCKED_IDENTITY.wireValue,
                            nextAttemptAt = null,
                            lastErrorKind = "IDENTITY_MISMATCH",
                            leaseOwner = null,
                            leaseExpiresAt = null,
                            updatedAt = now,
                        )
                        val encrypted = cipher.encrypt(payload, TodoOutboxEnvelope.canonicalAad(blocked))
                        dao.updateOutbox(
                            blocked.copy(
                                payloadCiphertext = encrypted.ciphertext,
                                payloadNonce = encrypted.nonce,
                                cryptoVersion = encrypted.version,
                            ),
                        )
                    }
                }
            val identity = activation.queueIdentity()
            val existing = dao.syncState(identity.origin, identity.namespace)
            if (existing?.activationRevision != activation.activationRevision) {
                dao.upsertSyncState(
                    TodoSyncStateEntity(
                        identity.origin,
                        identity.namespace,
                        true,
                        false,
                        false,
                        null,
                        activation.activationRevision,
                        TodoSyncGateState.READY.wireValue,
                        0,
                        null,
                        null,
                    ),
                )
            }
        }
    }
}

object TodoOutboxEnvelope {
    fun canonicalAad(entity: TodoOutboxEntity): String = buildString {
        append("webtag.todo-outbox-envelope\n")
        field("version", entity.envelopeVersion.toString())
        field("cryptoVersion", entity.cryptoVersion.toString())
        field("operationId", entity.operationId)
        field("apiOrigin", entity.apiOrigin)
        field("clientDataNamespace", entity.clientDataNamespace)
        field("activationRevision", entity.activationRevision.toString())
        field("kind", entity.kind)
        field("targetTodoId", entity.targetTodoId)
        field("state", entity.state)
        field("attemptCount", entity.attemptCount.toString())
        field("nextAttemptAt", entity.nextAttemptAt?.toString())
        field("lastErrorKind", entity.lastErrorKind)
        field("leaseOwner", entity.leaseOwner)
        field("leaseExpiresAt", entity.leaseExpiresAt?.toString())
        field("createdAt", entity.createdAt.toString())
        field("updatedAt", entity.updatedAt.toString())
        field("quarantinedLegacyState", entity.quarantinedLegacyState)
    }

    internal fun canonicalV2Aad(entity: TodoOutboxEntity): String = buildString {
        append("webtag.todo-outbox-envelope\n")
        field("version", TODO_OUTBOX_ENVELOPE_V2.toString())
        field("operationId", entity.operationId)
        field("apiOrigin", entity.apiOrigin)
        field("clientDataNamespace", entity.clientDataNamespace)
        field("activationRevision", entity.activationRevision.toString())
        field("kind", entity.kind)
        field("targetTodoId", entity.targetTodoId)
        field("state", entity.state)
        field("attemptCount", entity.attemptCount.toString())
        field("nextAttemptAt", entity.nextAttemptAt?.toString())
        field("lastErrorKind", entity.lastErrorKind)
        field("createdAt", entity.createdAt.toString())
        field("quarantinedLegacyState", entity.quarantinedLegacyState)
    }

    fun validatePayload(kind: TodoOutboxKind, payload: String) {
        val json = JSONObject(payload)
        val fields = buildSet {
            val keys = json.keys()
            while (keys.hasNext()) add(keys.next())
        }
        when (kind) {
            TodoOutboxKind.CREATE -> {
                require(fields == setOf("text", "due_at")) { "invalid legacy TODO create envelope" }
                require(json.getString("text").isNotBlank() && json.getString("text").length <= 4096)
                if (!json.isNull("due_at")) json.getLong("due_at")
            }
            TodoOutboxKind.PATCH -> {
                require(fields.all(PATCH_FIELDS::contains) && fields.size > 1 && "due_at_set" in fields) {
                    "invalid legacy TODO patch envelope"
                }
                json.optString("text").takeIf { json.has("text") }?.let { require(it.length <= 4096) }
                val dueAtSet = json.getBoolean("due_at_set")
                require(!json.has("due_at") || dueAtSet) { "TODO due date is present without due_at_set" }
                if (json.has("due_at") && !json.isNull("due_at")) json.getLong("due_at")
                if (json.has("done")) json.getBoolean("done")
                if (json.has("expected_host_revision")) require(json.getLong("expected_host_revision") >= 0)
            }
            TodoOutboxKind.DELETE -> require(fields.isEmpty()) { "invalid legacy TODO delete envelope" }
        }
    }

    private fun StringBuilder.field(name: String, value: String?) {
        val encoded = value ?: "~"
        append(name).append('=').append(encoded.toByteArray(Charsets.UTF_8).size).append(':').append(encoded).append('\n')
    }

    private val PATCH_FIELDS = setOf("text", "due_at_set", "due_at", "done", "expected_host_revision")
}

internal fun migrateLegacyTodoOutbox(database: SupportSQLiteDatabase) {
    val active = database.query(
        "SELECT apiOrigin, clientDataNamespace, activationRevision FROM active_session WHERE id = 1",
    ).use { cursor ->
        if (cursor.moveToFirst()) Triple(cursor.getString(0), cursor.getString(1), cursor.getLong(2)) else null
    }
    val cipher = AndroidKeystoreCipher("webtag-share-data-v1")
    database.query("SELECT * FROM todo_outbox").use { cursor ->
        val operationIdIndex = cursor.getColumnIndexOrThrow("operationId")
        val originIndex = cursor.getColumnIndexOrThrow("apiOrigin")
        val namespaceIndex = cursor.getColumnIndexOrThrow("clientDataNamespace")
        val targetIndex = cursor.getColumnIndexOrThrow("targetTodoId")
        val kindIndex = cursor.getColumnIndexOrThrow("kind")
        val ciphertextIndex = cursor.getColumnIndexOrThrow("payloadCiphertext")
        val nonceIndex = cursor.getColumnIndexOrThrow("payloadNonce")
        val cryptoIndex = cursor.getColumnIndexOrThrow("cryptoVersion")
        val stateIndex = cursor.getColumnIndexOrThrow("state")
        val attemptIndex = cursor.getColumnIndexOrThrow("attemptCount")
        val nextIndex = cursor.getColumnIndexOrThrow("nextAttemptAt")
        val errorIndex = cursor.getColumnIndexOrThrow("lastErrorKind")
        val createdIndex = cursor.getColumnIndexOrThrow("createdAt")
        val updatedIndex = cursor.getColumnIndexOrThrow("updatedAt")
        while (cursor.moveToNext()) {
            val operationId = cursor.getString(operationIdIndex)
            val origin = cursor.getString(originIndex)
            val namespace = cursor.getString(namespaceIndex)
            val matchesActive = active?.let { it.first == origin && it.second == namespace } == true
            if (!matchesActive) {
                quarantineLegacy(database, operationId)
                continue
            }
            val revision = requireNotNull(active).third
            val entity = TodoOutboxEntity(
                operationId,
                origin,
                namespace,
                cursor.getString(targetIndex),
                cursor.getString(kindIndex),
                cursor.getBlob(ciphertextIndex),
                cursor.getBlob(nonceIndex),
                cursor.getInt(cryptoIndex),
                cursor.getString(stateIndex),
                cursor.getInt(attemptIndex),
                if (cursor.isNull(nextIndex)) null else cursor.getLong(nextIndex),
                if (cursor.isNull(errorIndex)) null else cursor.getString(errorIndex),
                null,
                null,
                cursor.getLong(createdIndex),
                cursor.getLong(updatedIndex),
                revision,
                TODO_OUTBOX_ENVELOPE_V2,
            )
            runCatching {
                require(runCatching { UUID.fromString(entity.operationId) }.isSuccess)
                require(runCatching { UUID.fromString(entity.targetTodoId) }.isSuccess)
                val kind = TodoOutboxKind.fromWire(entity.kind)
                TodoOutboxState.fromWire(entity.state)
                val payload = cipher.decrypt(
                    EncryptedValue(entity.payloadCiphertext, entity.payloadNonce, entity.cryptoVersion),
                    legacyV1Aad(operationId, origin, namespace),
                )
                TodoOutboxEnvelope.validatePayload(kind, payload)
                // V1 never authenticated target or state. Even a structurally valid payload cannot
                // prove those columns were not changed before upgrade, so migration authenticates
                // the preserved record only in a non-sendable quarantine state.
                val migrated = entity.copy(
                    state = TodoOutboxState.QUARANTINED_LEGACY.wireValue,
                    quarantinedLegacyState = entity.state,
                )
                val encrypted = cipher.encrypt(payload, TodoOutboxEnvelope.canonicalV2Aad(migrated))
                database.execSQL(
                    "UPDATE todo_outbox SET payloadCiphertext = ?, payloadNonce = ?, cryptoVersion = ?, " +
                        "state = 'quarantined_legacy', activationRevision = ?, envelopeVersion = ?, " +
                        "quarantinedLegacyState = ?, leaseOwner = NULL, leaseExpiresAt = NULL " +
                        "WHERE operationId = ?",
                    arrayOf(
                        encrypted.ciphertext,
                        encrypted.nonce,
                        encrypted.version,
                        revision,
                        TODO_OUTBOX_ENVELOPE_V2,
                        entity.state,
                        operationId,
                    ),
                )
            }.onFailure {
                quarantineLegacy(database, operationId)
            }
        }
    }
    if (active != null) {
        database.execSQL(
            "UPDATE todo_sync_state SET activationRevision = ?, gateState = 'ready', attemptCount = 0, " +
                "nextAttemptAt = NULL, lastErrorKind = NULL " +
                "WHERE apiOrigin = ? AND clientDataNamespace = ?",
            arrayOf(active.third, active.first, active.second),
        )
    }
}

internal fun migrateTodoStorageV2ToV3(database: SupportSQLiteDatabase) {
    migrateTodoCacheV2ToV3(database)
    migrateTodoOutboxV2ToV3(database)
}

private fun migrateTodoCacheV2ToV3(database: SupportSQLiteDatabase) {
    database.execSQL(
        "CREATE TABLE IF NOT EXISTS todo_cache_v3 (" +
            "apiOrigin TEXT NOT NULL, clientDataNamespace TEXT NOT NULL, activationRevision INTEGER NOT NULL, " +
            "todoId TEXT NOT NULL, payloadCiphertext BLOB NOT NULL, payloadNonce BLOB NOT NULL, " +
            "cryptoVersion INTEGER NOT NULL, serverUpdatedAt INTEGER NOT NULL, fetchedAt INTEGER NOT NULL, " +
            "PRIMARY KEY(apiOrigin, clientDataNamespace, activationRevision, todoId))",
    )
    database.execSQL("DROP TABLE todo_cache")
    database.execSQL("ALTER TABLE todo_cache_v3 RENAME TO todo_cache")
    database.execSQL(
        "CREATE INDEX IF NOT EXISTS " +
            "index_todo_cache_apiOrigin_clientDataNamespace_activationRevision_serverUpdatedAt " +
            "ON todo_cache (apiOrigin, clientDataNamespace, activationRevision, serverUpdatedAt)",
    )
}

private fun migrateTodoOutboxV2ToV3(database: SupportSQLiteDatabase) {
    val cipher = AndroidKeystoreCipher("webtag-share-data-v1")
    database.query("SELECT * FROM todo_outbox WHERE envelopeVersion = 2").use { cursor ->
        while (cursor.moveToNext()) {
            val entity = cursor.toTodoOutboxEntity()
            runCatching {
                val kind = TodoOutboxKind.fromWire(entity.kind)
                TodoOutboxState.fromWire(entity.state)
                val payload = cipher.decrypt(
                    EncryptedValue(entity.payloadCiphertext, entity.payloadNonce, 1),
                    TodoOutboxEnvelope.canonicalV2Aad(entity),
                )
                TodoOutboxEnvelope.validatePayload(kind, payload)
                val migrated = entity.copy(
                    envelopeVersion = TODO_OUTBOX_ENVELOPE_VERSION,
                    cryptoVersion = 1,
                    leaseOwner = null,
                    leaseExpiresAt = null,
                    updatedAt = entity.createdAt,
                )
                val encrypted = cipher.encrypt(payload, TodoOutboxEnvelope.canonicalAad(migrated))
                database.execSQL(
                    "UPDATE todo_outbox SET payloadCiphertext = ?, payloadNonce = ?, cryptoVersion = ?, " +
                        "envelopeVersion = ?, leaseOwner = NULL, leaseExpiresAt = NULL, updatedAt = ? " +
                        "WHERE operationId = ? AND envelopeVersion = 2",
                    arrayOf(
                        encrypted.ciphertext,
                        encrypted.nonce,
                        encrypted.version,
                        TODO_OUTBOX_ENVELOPE_VERSION,
                        migrated.updatedAt,
                        entity.operationId,
                    ),
                )
            }.onFailure {
                database.execSQL(
                    "UPDATE todo_outbox SET quarantinedLegacyState = state, state = 'quarantined_legacy', " +
                        "leaseOwner = NULL, leaseExpiresAt = NULL WHERE operationId = ? AND envelopeVersion = 2",
                    arrayOf(entity.operationId),
                )
            }
        }
    }
}

private fun android.database.Cursor.toTodoOutboxEntity() = TodoOutboxEntity(
    operationId = getString(getColumnIndexOrThrow("operationId")),
    apiOrigin = getString(getColumnIndexOrThrow("apiOrigin")),
    clientDataNamespace = getString(getColumnIndexOrThrow("clientDataNamespace")),
    targetTodoId = getString(getColumnIndexOrThrow("targetTodoId")),
    kind = getString(getColumnIndexOrThrow("kind")),
    payloadCiphertext = getBlob(getColumnIndexOrThrow("payloadCiphertext")),
    payloadNonce = getBlob(getColumnIndexOrThrow("payloadNonce")),
    cryptoVersion = getInt(getColumnIndexOrThrow("cryptoVersion")),
    state = getString(getColumnIndexOrThrow("state")),
    attemptCount = getInt(getColumnIndexOrThrow("attemptCount")),
    nextAttemptAt = getColumnIndexOrThrow("nextAttemptAt").let { if (isNull(it)) null else getLong(it) },
    lastErrorKind = getColumnIndexOrThrow("lastErrorKind").let { if (isNull(it)) null else getString(it) },
    leaseOwner = getColumnIndexOrThrow("leaseOwner").let { if (isNull(it)) null else getString(it) },
    leaseExpiresAt = getColumnIndexOrThrow("leaseExpiresAt").let { if (isNull(it)) null else getLong(it) },
    createdAt = getLong(getColumnIndexOrThrow("createdAt")),
    updatedAt = getLong(getColumnIndexOrThrow("updatedAt")),
    activationRevision = getLong(getColumnIndexOrThrow("activationRevision")),
    envelopeVersion = getInt(getColumnIndexOrThrow("envelopeVersion")),
    quarantinedLegacyState = getColumnIndexOrThrow("quarantinedLegacyState").let {
        if (isNull(it)) null else getString(it)
    },
)

private fun quarantineLegacy(database: SupportSQLiteDatabase, operationId: String) {
    database.execSQL(
        "UPDATE todo_outbox SET quarantinedLegacyState = state, state = 'quarantined_legacy', " +
            "activationRevision = 0, envelopeVersion = 1, " +
            "leaseOwner = NULL, leaseExpiresAt = NULL WHERE operationId = ?",
        arrayOf(operationId),
    )
}

private fun legacyV1Aad(operationId: String, origin: String, namespace: String): String =
    "todo-outbox-v1|$operationId|$origin|$namespace"

private fun cacheV1Aad(todoId: String, origin: String, namespace: String): String =
    "todo-cache-v1|$todoId|$origin|$namespace"

internal fun cacheV2Aad(todoId: String, origin: String, namespace: String, activationRevision: Long): String =
    buildString {
        append("webtag.todo-cache-envelope\n")
        appendAadField("version", "2")
        appendAadField("todoId", todoId)
        appendAadField("apiOrigin", origin)
        appendAadField("clientDataNamespace", namespace)
        appendAadField("activationRevision", activationRevision.toString())
    }

private fun StringBuilder.appendAadField(name: String, value: String) {
    append(name).append('=').append(value.toByteArray(Charsets.UTF_8).size).append(':').append(value).append('\n')
}

private fun ActiveSessionSnapshot.queueIdentity() = QueueIdentity(identity.origin, identity.clientDataNamespace)
private fun TodoCacheEntity.identity() = QueueIdentity(apiOrigin, clientDataNamespace)
private fun TodoOutboxEntity.identity() = QueueIdentity(apiOrigin, clientDataNamespace)
private fun String.isPending() = this == TodoOutboxState.PENDING.wireValue || this == TodoOutboxState.RETRY_WAIT.wireValue
