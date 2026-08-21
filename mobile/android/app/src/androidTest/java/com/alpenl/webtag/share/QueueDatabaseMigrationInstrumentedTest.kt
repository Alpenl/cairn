package com.alpenl.webtag.share

import androidx.room.testing.MigrationTestHelper
import androidx.room.Room
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import com.alpenl.webtag.share.data.QueueDatabase
import com.alpenl.webtag.share.data.QueueRepository
import com.alpenl.webtag.share.data.TODO_OUTBOX_ENVELOPE_VERSION
import com.alpenl.webtag.share.data.TodoCasOutcome
import com.alpenl.webtag.share.data.TodoClaimOutcome
import com.alpenl.webtag.share.data.TodoOutboxEntity
import com.alpenl.webtag.share.data.TodoOutboxEnvelope
import com.alpenl.webtag.share.data.TodoOutboxState
import com.alpenl.webtag.share.data.TodoRepository
import com.alpenl.webtag.share.contract.QueueIdentity
import com.alpenl.webtag.share.security.AndroidKeystoreCipher
import com.alpenl.webtag.share.security.EncryptedValue
import java.util.UUID
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class QueueDatabaseMigrationInstrumentedTest {
    private val instrumentation = InstrumentationRegistry.getInstrumentation()
    private val helper = MigrationTestHelper(instrumentation, QueueDatabase::class.java)
    private val databaseName = "queue-v1-v4-${UUID.randomUUID()}.db"

    @Before
    fun cleanBefore() {
        instrumentation.targetContext.deleteDatabase(databaseName)
    }

    @After
    fun cleanAfter() {
        instrumentation.targetContext.deleteDatabase(databaseName)
    }

    @Test
    fun migrationRewrapsCurrentLegacyIntoAuthenticatedQuarantineAndDropsAmbiguousCache() {
        val origin = "https://example.org"
        val namespace = "m".repeat(43)
        val revision = 7L
        val activeOperation = UUID.randomUUID().toString()
        val inactiveOperation = UUID.randomUUID().toString()
        val invalidOperation = UUID.randomUUID().toString()
        val activeTarget = UUID.randomUUID().toString()
        val cachedTodo = UUID.randomUUID().toString()
        val cipher = AndroidKeystoreCipher("webtag-share-data-v1")

        helper.createDatabase(databaseName, 1).use { database ->
            database.execSQL(
                "INSERT INTO active_session " +
                    "(id, apiOrigin, clientDataNamespace, representationContract, activationRevision) " +
                    "VALUES (1, ?, ?, 'v3', ?)",
                arrayOf(origin, namespace, revision),
            )
            insertLegacyCache(database, cipher, origin, namespace, cachedTodo, "legacy cache")
            insertLegacy(
                database,
                cipher,
                activeOperation,
                origin,
                namespace,
                "patch",
                activeTarget,
                "{\"text\":\"preserved\",\"due_at_set\":false}",
            )
            insertLegacy(
                database,
                cipher,
                inactiveOperation,
                "https://inactive.example",
                "i".repeat(43),
                "delete",
                "inactive-todo",
                "{}",
            )
            insertLegacy(
                database,
                cipher,
                invalidOperation,
                origin,
                namespace,
                "delete",
                UUID.randomUUID().toString(),
                "{\"text\":\"must not become a delete\",\"due_at_set\":false}",
            )
        }

        helper.runMigrationsAndValidate(
            databaseName,
            3,
            true,
            QueueDatabase.MIGRATION_1_2,
            QueueDatabase.MIGRATION_2_3,
        ).use { database ->
            database.query("SELECT * FROM todo_outbox WHERE operationId = ?", arrayOf(activeOperation)).use { cursor ->
                check(cursor.moveToFirst())
                val entity = cursor.toOutboxEntity()
                assertEquals(revision, entity.activationRevision)
                assertEquals(TODO_OUTBOX_ENVELOPE_VERSION, entity.envelopeVersion)
                assertEquals("patch", entity.kind)
                assertEquals(activeTarget, entity.targetTodoId)
                assertEquals(TodoOutboxState.QUARANTINED_LEGACY.wireValue, entity.state)
                assertEquals("pending", entity.quarantinedLegacyState)
                assertEquals(
                    "{\"text\":\"preserved\",\"due_at_set\":false}",
                    cipher.decrypt(
                        EncryptedValue(entity.payloadCiphertext, entity.payloadNonce, entity.cryptoVersion),
                        TodoOutboxEnvelope.canonicalAad(entity),
                    ),
                )
            }
            database.query("SELECT * FROM todo_outbox WHERE operationId = ?", arrayOf(inactiveOperation)).use { cursor ->
                check(cursor.moveToFirst())
                assertEquals(0L, cursor.getLong(cursor.getColumnIndexOrThrow("activationRevision")))
                assertEquals(1, cursor.getInt(cursor.getColumnIndexOrThrow("envelopeVersion")))
                assertEquals(
                    TodoOutboxState.QUARANTINED_LEGACY.wireValue,
                    cursor.getString(cursor.getColumnIndexOrThrow("state")),
                )
            }
            database.query("SELECT * FROM todo_outbox WHERE operationId = ?", arrayOf(invalidOperation)).use { cursor ->
                check(cursor.moveToFirst())
                assertEquals(0L, cursor.getLong(cursor.getColumnIndexOrThrow("activationRevision")))
                assertEquals(
                    TodoOutboxState.QUARANTINED_LEGACY.wireValue,
                    cursor.getString(cursor.getColumnIndexOrThrow("state")),
                )
            }
            database.query("SELECT COUNT(*) FROM todo_cache").use { cursor ->
                check(cursor.moveToFirst())
                assertEquals(0, cursor.getInt(0))
            }
        }

        val room = Room.databaseBuilder(instrumentation.targetContext, QueueDatabase::class.java, databaseName)
            .addMigrations(QueueDatabase.MIGRATION_1_2, QueueDatabase.MIGRATION_2_3, QueueDatabase.MIGRATION_3_4)
            .allowMainThreadQueries()
            .build()
        try {
            val queueRepository = QueueRepository(room, cipher)
            val repository = TodoRepository(room, cipher)
            val activation = requireNotNull(queueRepository.activeSessionSnapshot())
            val legacy = (repository.snapshot(activation) as com.alpenl.webtag.share.data.TodoSnapshotOutcome.Applied)
                .snapshot.legacyOperations
            assertTrue(legacy.single { it.operationId == activeOperation }.recoverable)
            assertTrue(legacy.single { it.operationId == invalidOperation }.recoverable.not())

            assertEquals(TodoCasOutcome.APPLIED, repository.recoverLegacyOperation(activation, activeOperation, 2_000))
            val restored = room.todoDao().findOperation(activeOperation)
            assertNotNull(restored)
            assertEquals(TodoOutboxState.PENDING.wireValue, restored!!.state)
            assertEquals(activeTarget, restored.targetTodoId)
            assertEquals(TODO_OUTBOX_ENVELOPE_VERSION, restored.envelopeVersion)
            val claimed = repository.claimDue(activation, 2_001, 30_000) as TodoClaimOutcome.Claimed
            assertEquals(activeOperation, claimed.value.operation.entity.operationId)
            assertEquals("preserved", claimed.value.operation.patch?.text)

            assertEquals(TodoCasOutcome.APPLIED, repository.deleteLegacyOperation(activation, invalidOperation))
            assertNull(room.todoDao().findOperation(invalidOperation))

            val inactiveIdentity = QueueIdentity("https://inactive.example", "i".repeat(43))
            val generation = queueRepository.beginActivationAttempt()
            assertEquals(
                com.alpenl.webtag.share.contract.ActivationCommitOutcome.APPLIED,
                queueRepository.activateSessionIfLatest(
                    com.alpenl.webtag.share.contract.SessionIdentity(
                        inactiveIdentity.origin,
                        inactiveIdentity.namespace,
                        com.alpenl.webtag.share.contract.REPRESENTATION_CONTRACT,
                    ),
                    generation,
                ),
            )
            val inactiveActivation = requireNotNull(queueRepository.activeSessionSnapshot())
            val inactiveLegacy = (repository.snapshot(inactiveActivation) as com.alpenl.webtag.share.data.TodoSnapshotOutcome.Applied)
                .snapshot.legacyOperations.single { it.operationId == inactiveOperation }
            assertTrue(inactiveLegacy.recoverable.not())
            assertEquals(TodoCasOutcome.APPLIED, repository.deleteLegacyOperation(inactiveActivation, inactiveOperation))
            assertNull(room.todoDao().findOperation(inactiveOperation))
        } finally {
            room.close()
        }
    }

    @Test
    fun migrationClearsV2CacheWhenIdentityReturnsAtANewerActivation() {
        val originA = "https://a.example"
        val namespaceA = "a".repeat(43)
        val originB = "https://b.example"
        val namespaceB = "b".repeat(43)
        val cipher = AndroidKeystoreCipher("webtag-share-data-v1")

        helper.createDatabase(databaseName, 2).use { database ->
            database.execSQL(
                "INSERT INTO active_session " +
                    "(id, apiOrigin, clientDataNamespace, representationContract, activationRevision) " +
                    "VALUES (1, ?, ?, 'v3', 1)",
                arrayOf(originA, namespaceA),
            )
            insertLegacyCache(database, cipher, originA, namespaceA, UUID.randomUUID().toString(), "A1 cache")
            database.execSQL(
                "UPDATE active_session SET apiOrigin = ?, clientDataNamespace = ?, activationRevision = 2 WHERE id = 1",
                arrayOf(originB, namespaceB),
            )
            insertLegacyCache(database, cipher, originB, namespaceB, UUID.randomUUID().toString(), "B2 cache")
            database.execSQL(
                "UPDATE active_session SET apiOrigin = ?, clientDataNamespace = ?, activationRevision = 3 WHERE id = 1",
                arrayOf(originA, namespaceA),
            )
        }

        helper.runMigrationsAndValidate(
            databaseName,
            3,
            true,
            QueueDatabase.MIGRATION_2_3,
        ).use { database ->
            database.query("SELECT COUNT(*) FROM todo_cache").use { cursor ->
                check(cursor.moveToFirst())
                assertEquals(0, cursor.getInt(0))
            }
        }

        val room = Room.databaseBuilder(instrumentation.targetContext, QueueDatabase::class.java, databaseName)
            .addMigrations(QueueDatabase.MIGRATION_1_2, QueueDatabase.MIGRATION_2_3, QueueDatabase.MIGRATION_3_4)
            .allowMainThreadQueries()
            .build()
        try {
            val activationA3 = requireNotNull(QueueRepository(room, cipher).activeSessionSnapshot())
            val snapshot = TodoRepository(room, cipher).snapshot(activationA3)
                as com.alpenl.webtag.share.data.TodoSnapshotOutcome.Applied
            assertTrue(snapshot.snapshot.items.isEmpty())
        } finally {
            room.close()
        }
    }

    @Test
    fun migrationNormalizesUnauthenticatedV2LeaseBeforeSealingV3() {
        val origin = "https://example.org"
        val namespace = "l".repeat(43)
        val revision = 7L
        val operationId = UUID.randomUUID().toString()
        val targetId = UUID.randomUUID().toString()
        val cipher = AndroidKeystoreCipher("webtag-share-data-v1")
        val payload = "{\"text\":\"leased\",\"due_at_set\":false}"
        val untrusted = TodoOutboxEntity(
            operationId = operationId,
            apiOrigin = origin,
            clientDataNamespace = namespace,
            targetTodoId = targetId,
            kind = "patch",
            payloadCiphertext = byteArrayOf(),
            payloadNonce = byteArrayOf(),
            cryptoVersion = Int.MAX_VALUE,
            state = TodoOutboxState.PENDING.wireValue,
            attemptCount = 0,
            nextAttemptAt = null,
            lastErrorKind = null,
            leaseOwner = "untrusted-v2-owner",
            leaseExpiresAt = Long.MAX_VALUE,
            createdAt = 1_000,
            updatedAt = Long.MAX_VALUE,
            activationRevision = revision,
            envelopeVersion = 2,
        )
        val encrypted = cipher.encrypt(payload, TodoOutboxEnvelope.canonicalV2Aad(untrusted))

        helper.createDatabase(databaseName, 2).use { database ->
            database.execSQL(
                "INSERT INTO active_session " +
                    "(id, apiOrigin, clientDataNamespace, representationContract, activationRevision) " +
                    "VALUES (1, ?, ?, 'v3', ?)",
                arrayOf(origin, namespace, revision),
            )
            insertV2Outbox(database, untrusted, encrypted)
        }

        helper.runMigrationsAndValidate(
            databaseName,
            3,
            true,
            QueueDatabase.MIGRATION_2_3,
        ).use { database ->
            database.query("SELECT * FROM todo_outbox WHERE operationId = ?", arrayOf(operationId)).use { cursor ->
                check(cursor.moveToFirst())
                val migrated = cursor.toOutboxEntity()
                assertEquals(TODO_OUTBOX_ENVELOPE_VERSION, migrated.envelopeVersion)
                assertEquals(1, migrated.cryptoVersion)
                assertNull(migrated.leaseOwner)
                assertNull(migrated.leaseExpiresAt)
                assertEquals(migrated.createdAt, migrated.updatedAt)
                assertEquals(
                    payload,
                    cipher.decrypt(
                        EncryptedValue(migrated.payloadCiphertext, migrated.payloadNonce, migrated.cryptoVersion),
                        TodoOutboxEnvelope.canonicalAad(migrated),
                    ),
                )
            }
        }

        val room = Room.databaseBuilder(instrumentation.targetContext, QueueDatabase::class.java, databaseName)
            .addMigrations(QueueDatabase.MIGRATION_1_2, QueueDatabase.MIGRATION_2_3, QueueDatabase.MIGRATION_3_4)
            .allowMainThreadQueries()
            .build()
        try {
            val activation = requireNotNull(QueueRepository(room, cipher).activeSessionSnapshot())
            val claimed = TodoRepository(room, cipher).claimDue(activation, 1_001, 30_000)
                as TodoClaimOutcome.Claimed
            assertEquals(operationId, claimed.value.operation.entity.operationId)
        } finally {
            room.close()
        }
    }

    @Test
    fun recoveryMakesBlockedAuthLegacyOperationExecutable() {
        assertRecoveryNormalizesState(TodoOutboxState.BLOCKED_AUTH)
    }

    @Test
    fun migrationDropsLegacyTaskIdsWithoutChangingDurableQueueState() {
        val origin = "https://example.org"
        val namespace = "q".repeat(43)
        val queueId = UUID.randomUUID().toString()

        helper.createDatabase(databaseName, 3).use { database ->
            database.execSQL(
                """
                INSERT INTO queue_entries (
                    id, schemaVersion, urlCiphertext, urlNonce, cryptoVersion,
                    idempotencyKey, requestFingerprint, apiOrigin, clientDataNamespace,
                    state, createdAt, firstFailedAt, attemptCount, nextAttemptAt,
                    lastErrorKind, lastErrorCode, lastHttpStatus, linkId, jobId,
                    leaseOwner, leaseExpiresAt, updatedAt
                ) VALUES (?, 1, ?, ?, 1, ?, ?, ?, ?, 'retry_wait', 1000, NULL, 2,
                          2000, 'NO_NETWORK', NULL, NULL, ?, ?, NULL, NULL, 1000)
                """.trimIndent(),
                arrayOf(
                    queueId,
                    byteArrayOf(1, 2, 3),
                    byteArrayOf(4, 5, 6),
                    "idem-$queueId",
                    "fingerprint-$queueId",
                    origin,
                    namespace,
                    "link-$queueId",
                    "legacy-task-$queueId",
                ),
            )
            database.execSQL(
                """
                INSERT INTO recent_results (
                    id, urlCiphertext, urlNonce, cryptoVersion, linkId, jobId, status,
                    createdAt, apiOrigin, clientDataNamespace, refreshNotBefore,
                    refreshBlockReason
                ) VALUES (1, ?, ?, 1, ?, ?, 'failed', 3000, ?, ?, NULL, NULL)
                """.trimIndent(),
                arrayOf(
                    byteArrayOf(7, 8, 9),
                    byteArrayOf(10, 11, 12),
                    "recent-$queueId",
                    "legacy-recent-task-$queueId",
                    origin,
                    namespace,
                ),
            )
        }

        helper.runMigrationsAndValidate(
            databaseName,
            4,
            true,
            QueueDatabase.MIGRATION_3_4,
        ).use { database ->
            assertFalse(hasColumn(database, "queue_entries", "jobId"))
            assertFalse(hasColumn(database, "recent_results", "jobId"))
            database.query(
                "SELECT state, attemptCount, nextAttemptAt, linkId FROM queue_entries WHERE id=?",
                arrayOf(queueId),
            ).use {
                assertTrue(it.moveToFirst())
                assertEquals("retry_wait", it.getString(0))
                assertEquals(2, it.getInt(1))
                assertEquals(2000L, it.getLong(2))
                assertEquals("link-$queueId", it.getString(3))
            }
            database.query("SELECT status, linkId FROM recent_results WHERE id=1").use {
                assertTrue(it.moveToFirst())
                assertEquals("failed", it.getString(0))
                assertEquals("recent-$queueId", it.getString(1))
            }
        }
    }

    @Test
    fun recoveryMakesBlockedIdentityLegacyOperationExecutable() {
        assertRecoveryNormalizesState(TodoOutboxState.BLOCKED_IDENTITY)
    }

    @Test
    fun recoveryMakesConflictLegacyOperationExecutable() {
        assertRecoveryNormalizesState(TodoOutboxState.CONFLICT)
    }

    @Test
    fun recoveryMakesFailedPermanentLegacyOperationExecutable() {
        assertRecoveryNormalizesState(TodoOutboxState.FAILED_PERMANENT)
    }

    private fun assertRecoveryNormalizesState(originalState: TodoOutboxState) {
        val origin = "https://example.org"
        val namespace = "r".repeat(43)
        val revision = 7L
        val operationId = UUID.randomUUID().toString()
        val targetId = UUID.randomUUID().toString()
        val cipher = AndroidKeystoreCipher("webtag-share-data-v1")

        helper.createDatabase(databaseName, 1).use { database ->
            database.execSQL(
                "INSERT INTO active_session " +
                    "(id, apiOrigin, clientDataNamespace, representationContract, activationRevision) " +
                    "VALUES (1, ?, ?, 'v3', ?)",
                arrayOf(origin, namespace, revision),
            )
            insertLegacy(
                database,
                cipher,
                operationId,
                origin,
                namespace,
                "patch",
                targetId,
                "{\"text\":\"recover me\",\"due_at_set\":false}",
                state = originalState.wireValue,
                attemptCount = 9,
                nextAttemptAt = Long.MAX_VALUE,
                lastErrorKind = "legacy_error",
                leaseOwner = "legacy-owner",
                leaseExpiresAt = Long.MAX_VALUE,
                updatedAt = Long.MAX_VALUE,
            )
        }
        helper.runMigrationsAndValidate(
            databaseName,
            3,
            true,
            QueueDatabase.MIGRATION_1_2,
            QueueDatabase.MIGRATION_2_3,
        ).close()

        val room = Room.databaseBuilder(instrumentation.targetContext, QueueDatabase::class.java, databaseName)
            .addMigrations(QueueDatabase.MIGRATION_1_2, QueueDatabase.MIGRATION_2_3, QueueDatabase.MIGRATION_3_4)
            .allowMainThreadQueries()
            .build()
        try {
            val activation = requireNotNull(QueueRepository(room, cipher).activeSessionSnapshot())
            val repository = TodoRepository(room, cipher)
            assertEquals(TodoCasOutcome.APPLIED, repository.recoverLegacyOperation(activation, operationId, 2_000))
            val restored = requireNotNull(room.todoDao().findOperation(operationId))
            assertEquals(TodoOutboxState.PENDING.wireValue, restored.state)
            assertEquals(0, restored.attemptCount)
            assertNull(restored.nextAttemptAt)
            assertNull(restored.lastErrorKind)
            assertNull(restored.leaseOwner)
            assertNull(restored.leaseExpiresAt)
            assertNull(restored.quarantinedLegacyState)

            val claimed = repository.claimDue(activation, 2_001, 30_000) as TodoClaimOutcome.Claimed
            assertEquals(operationId, claimed.value.operation.entity.operationId)
        } finally {
            room.close()
        }
    }

    private fun insertLegacyCache(
        database: androidx.sqlite.db.SupportSQLiteDatabase,
        cipher: AndroidKeystoreCipher,
        origin: String,
        namespace: String,
        todoId: String,
        payload: String,
    ) {
        val encrypted = cipher.encrypt(payload, "todo-cache-v1|$todoId|$origin|$namespace")
        database.execSQL(
            "INSERT INTO todo_cache " +
                "(apiOrigin, clientDataNamespace, todoId, payloadCiphertext, payloadNonce, cryptoVersion, " +
                "serverUpdatedAt, fetchedAt) VALUES (?, ?, ?, ?, ?, ?, 1000, 1000)",
            arrayOf(origin, namespace, todoId, encrypted.ciphertext, encrypted.nonce, encrypted.version),
        )
    }

    private fun insertLegacy(
        database: androidx.sqlite.db.SupportSQLiteDatabase,
        cipher: AndroidKeystoreCipher,
        operationId: String,
        origin: String,
        namespace: String,
        kind: String,
        target: String,
        payload: String,
        state: String = TodoOutboxState.PENDING.wireValue,
        attemptCount: Int = 0,
        nextAttemptAt: Long? = null,
        lastErrorKind: String? = null,
        leaseOwner: String? = null,
        leaseExpiresAt: Long? = null,
        updatedAt: Long = 1_000,
    ) {
        val encrypted = cipher.encrypt(payload, "todo-outbox-v1|$operationId|$origin|$namespace")
        database.execSQL(
            "INSERT INTO todo_outbox " +
                "(operationId, apiOrigin, clientDataNamespace, targetTodoId, kind, payloadCiphertext, " +
                "payloadNonce, cryptoVersion, state, attemptCount, nextAttemptAt, lastErrorKind, " +
                "leaseOwner, leaseExpiresAt, createdAt, updatedAt) " +
                "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1000, ?)",
            arrayOf(
                operationId,
                origin,
                namespace,
                target,
                kind,
                encrypted.ciphertext,
                encrypted.nonce,
                encrypted.version,
                state,
                attemptCount,
                nextAttemptAt,
                lastErrorKind,
                leaseOwner,
                leaseExpiresAt,
                updatedAt,
            ),
        )
    }

    private fun insertV2Outbox(
        database: androidx.sqlite.db.SupportSQLiteDatabase,
        entity: TodoOutboxEntity,
        encrypted: EncryptedValue,
    ) {
        database.execSQL(
            "INSERT INTO todo_outbox " +
                "(operationId, apiOrigin, clientDataNamespace, targetTodoId, kind, payloadCiphertext, " +
                "payloadNonce, cryptoVersion, state, attemptCount, nextAttemptAt, lastErrorKind, " +
                "leaseOwner, leaseExpiresAt, createdAt, updatedAt, activationRevision, envelopeVersion, " +
                "quarantinedLegacyState) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            arrayOf(
                entity.operationId,
                entity.apiOrigin,
                entity.clientDataNamespace,
                entity.targetTodoId,
                entity.kind,
                encrypted.ciphertext,
                encrypted.nonce,
                entity.cryptoVersion,
                entity.state,
                entity.attemptCount,
                entity.nextAttemptAt,
                entity.lastErrorKind,
                entity.leaseOwner,
                entity.leaseExpiresAt,
                entity.createdAt,
                entity.updatedAt,
                entity.activationRevision,
                entity.envelopeVersion,
                entity.quarantinedLegacyState,
            ),
        )
    }

    private fun hasColumn(
        database: androidx.sqlite.db.SupportSQLiteDatabase,
        table: String,
        column: String,
    ): Boolean = database.query("PRAGMA table_info($table)").use { cursor ->
        val nameIndex = cursor.getColumnIndexOrThrow("name")
        while (cursor.moveToNext()) {
            if (cursor.getString(nameIndex) == column) return@use true
        }
        false
    }

    private fun android.database.Cursor.toOutboxEntity() = TodoOutboxEntity(
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
}
