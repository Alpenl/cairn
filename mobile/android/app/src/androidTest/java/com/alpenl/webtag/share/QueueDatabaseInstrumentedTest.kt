package com.alpenl.webtag.share

import android.content.Context
import androidx.room.Room
import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.work.WorkInfo
import androidx.work.WorkManager
import com.alpenl.webtag.share.data.QueueDatabase
import com.alpenl.webtag.share.data.QueueEntity
import com.alpenl.webtag.share.data.QueueRepository
import com.alpenl.webtag.share.data.ActiveSessionEntity
import com.alpenl.webtag.share.data.RecentResultEntity
import com.alpenl.webtag.share.data.TodoRepository
import com.alpenl.webtag.share.contract.QueueIdentity
import com.alpenl.webtag.share.contract.QueueState
import com.alpenl.webtag.share.contract.REPRESENTATION_CONTRACT
import com.alpenl.webtag.share.contract.ErrorKind
import com.alpenl.webtag.share.contract.ActiveSessionSnapshot
import com.alpenl.webtag.share.contract.CredentialConfig
import com.alpenl.webtag.share.contract.RecentResult
import com.alpenl.webtag.share.contract.RefreshCommitOutcome
import com.alpenl.webtag.share.contract.SessionIdentity
import com.alpenl.webtag.share.contract.SubmitCommitOutcome
import com.alpenl.webtag.share.contract.SubmitClaimOutcome
import com.alpenl.webtag.share.contract.SubmitResponse
import com.alpenl.webtag.share.network.ApiResult
import com.alpenl.webtag.share.network.ClassifiedFailure
import com.alpenl.webtag.share.network.WebTagApi
import com.alpenl.webtag.share.network.WebTagCompanionApi
import com.alpenl.webtag.share.queue.ConnectionCoordinator
import com.alpenl.webtag.share.queue.ConnectionResult
import com.alpenl.webtag.share.security.AndroidKeystoreCipher
import com.alpenl.webtag.share.security.EncryptedCredentialStore
import com.alpenl.webtag.share.todo.TodoCreate
import com.alpenl.webtag.share.todo.HomeSnapshot
import com.alpenl.webtag.share.todo.TodoCapabilities
import com.alpenl.webtag.share.todo.TodoItem
import com.alpenl.webtag.share.todo.TodoOriginKind
import com.alpenl.webtag.share.todo.TodoPatch
import com.alpenl.webtag.share.todo.TodoSyncScheduler
import com.alpenl.webtag.share.todo.TodoSyncCoordinator
import com.alpenl.webtag.share.data.QueueDueClaimOutcome
import com.alpenl.webtag.share.data.TodoCasOutcome
import com.alpenl.webtag.share.data.TodoClaimOutcome
import com.alpenl.webtag.share.data.TodoSnapshotOutcome
import com.alpenl.webtag.share.data.TodoGateDecision
import com.alpenl.webtag.share.data.TodoOutboxState
import com.alpenl.webtag.share.data.TodoOutboxKind
import com.alpenl.webtag.share.data.TodoSyncGateState
import java.security.GeneralSecurityException
import java.util.UUID
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assert.assertThrows
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class QueueDatabaseInstrumentedTest {
    private lateinit var database: QueueDatabase

    @Before
    fun setUp() {
        val context = ApplicationProvider.getApplicationContext<Context>()
        database = Room.inMemoryDatabaseBuilder(context, QueueDatabase::class.java)
            .allowMainThreadQueries()
            .build()
        database.queueDao().upsertActiveSession(
            ActiveSessionEntity(
                apiOrigin = "https://example.org",
                clientDataNamespace = "n".repeat(43),
                representationContract = REPRESENTATION_CONTRACT,
                activationRevision = 1,
            ),
        )
    }

    @After
    fun tearDown() {
        database.close()
    }

    @Test
    fun leaseClaimIsAtomicAndExpiredRowsBecomeDue() {
        val now = 1_000L
        val entry = queueEntity("entry-a", now)
        val dao = database.queueDao()
        dao.insert(entry)

        assertEquals(1, claim(dao, entry.id, "owner-a", now + 10, now))
        assertEquals(0, claim(dao, entry.id, "owner-b", now + 20, now + 1))
        assertNotNull(dao.findDue(entry.apiOrigin, entry.clientDataNamespace, now + 10))
    }

    @Test
    fun expiredLeaseCanBeReclaimedAtTheExactBoundaryButOldOwnerCannotWrite() {
        val now = 2_000L
        val entry = queueEntity("boundary", now)
        val dao = database.queueDao()
        dao.insert(entry)

        assertEquals(1, claim(dao, entry.id, "owner-a", now + 10, now))
        assertEquals(1, claim(dao, entry.id, "owner-b", now + 20, now + 10))
        assertEquals(
            0,
            dao.updateClaimed(
                id = entry.id,
                queueOrigin = entry.apiOrigin,
                queueNamespace = entry.clientDataNamespace,
                owner = "owner-a",
                state = "failed_permanent",
                firstFailedAt = now + 10,
                attemptCount = 1,
                nextAttemptAt = null,
                lastErrorKind = "HTTP_5XX",
                lastErrorCode = null,
                lastHttpStatus = 500,
                linkId = null,
                jobId = null,
                activeOrigin = entry.apiOrigin,
                activeNamespace = entry.clientDataNamespace,
                activationRevision = 1,
                updatedAt = now + 10,
            ),
        )
        assertEquals("pending_submit", dao.listAll().single().state)
        assertEquals("owner-b", dao.listAll().single().leaseOwner)
    }

    @Test
    fun resetForRetryOnlyChangesRetryableStatesWithoutAnActiveLease() {
        val now = 1_000L
        val dao = database.queueDao()
        val retryableStates = listOf(
            "retry_wait",
            "blocked_auth",
            "failed_permanent",
            "expired",
        )
        retryableStates.forEachIndexed { index, state ->
            dao.insert(queueEntity("retryable-$index", now, state = state))
        }
        dao.insert(queueEntity("pending", now, state = "pending_submit"))
        dao.insert(queueEntity("identity", now, state = "blocked_identity"))
        dao.insert(
            queueEntity("leased", now, state = "retry_wait").copy(
                leaseOwner = "active-owner",
                leaseExpiresAt = now + 10_000,
            ),
        )

        retryableStates.forEachIndexed { index, _ ->
            assertEquals(1, dao.resetForRetry("retryable-$index", null, now))
        }
        assertEquals(0, dao.resetForRetry("pending", null, now))
        assertEquals(0, dao.resetForRetry("identity", null, now))
        assertEquals(0, dao.resetForRetry("leased", null, now))
        assertEquals(
            "pending_submit",
            dao.listAll().single { it.id == "pending" }.state,
        )
        assertEquals(
            "blocked_identity",
            dao.listAll().single { it.id == "identity" }.state,
        )
    }

    @Test
    fun schedulerWaitsForRetryAtAndActiveLeaseBeforeWaking() {
        val now = 4_000L
        val dao = database.queueDao()
        dao.insert(
            queueEntity("future-retry", now, state = "retry_wait").copy(
                nextAttemptAt = now + 60_000,
            ),
        )
        assertEquals(now + 60_000, dao.earliestScheduleAt("https://example.org", "n".repeat(43), now))

        dao.insert(
            queueEntity("leased", now, state = "pending_submit").copy(
                leaseOwner = "active-owner",
                leaseExpiresAt = now + 30_000,
            ),
        )
        assertEquals(now + 30_000, dao.earliestScheduleAt("https://example.org", "n".repeat(43), now))
        assertEquals(now + 30_000, dao.earliestScheduleAt("https://example.org", "n".repeat(43), now + 30_000))
    }

    @Test
    fun claimDoesNotSendAFutureRetryRowBeforeItsDueTime() {
        val now = 4_500L
        val dao = database.queueDao()
        dao.insert(
            queueEntity("future-claim", now, state = "retry_wait").copy(
                nextAttemptAt = now + 60_000,
            ),
        )

        assertEquals(0, claim(dao, "future-claim", "early-owner", now + 30_000, now))
        assertEquals(1, claim(dao, "future-claim", "due-owner", now + 90_000, now + 60_000))
    }

    @Test
    fun keystoreCipherUsesNonceAndAuthenticatedAad() {
        val cipher = AndroidKeystoreCipher("instrumented-${UUID.randomUUID()}")
        val encrypted = cipher.encrypt("https://example.org/article", "entry|1|identity")
        assertEquals(12, encrypted.nonce.size)
        assertEquals("https://example.org/article", cipher.decrypt(encrypted, "entry|1|identity"))

        val tampered = encrypted.ciphertext.clone().also { it[it.lastIndex] = (it[it.lastIndex].toInt() xor 1).toByte() }
        assertThrows(GeneralSecurityException::class.java) {
            cipher.decrypt(encrypted.copy(ciphertext = tampered), "entry|1|identity")
        }
        assertArrayEquals(encrypted.nonce, encrypted.nonce)
        assertThrows(GeneralSecurityException::class.java) {
            cipher.decrypt(encrypted, "entry|1|other-identity")
        }
    }

    @Test
    fun repositoryRejectsADecryptedUrlWithAStaleFingerprint() {
        val repository = QueueRepository(
            database,
            AndroidKeystoreCipher("fingerprint-${UUID.randomUUID()}"),
        )
        val identity = QueueIdentity("https://example.org", "n".repeat(43))
        val entry = repository.enqueue("https://example.org/article", identity, now = 5_000L)

        assertEquals("https://example.org/article", repository.decode(entry))
        assertThrows(IllegalStateException::class.java) {
            repository.decode(entry.copy(requestFingerprint = "stale"))
        }
    }

    @Test
    fun findReusableReturnsTheExistingPendingRowForTheSameUrlAndIdentity() {
        val repository = QueueRepository(
            database,
            AndroidKeystoreCipher("reuse-${UUID.randomUUID()}"),
        )
        val identity = QueueIdentity("https://example.org", "r".repeat(43))
        val url = "https://example.org/reuse"
        val entry = repository.enqueue(url, identity, now = 5_500L)

        val reusable = repository.findReusable(url, identity)

        assertNotNull(reusable)
        assertEquals(entry.id, reusable!!.id)
        assertEquals(entry.idempotencyKey, reusable.idempotencyKey)
        assertEquals(1, repository.listAll().size)
        assertNull(repository.findReusable("https://example.org/other", identity))
        assertNull(repository.findReusable(url, QueueIdentity(identity.origin, "s".repeat(43))))
    }

    @Test
    fun enqueueOrReuseAtomicallyKeepsTheExistingIdempotencyKey() {
        val repository = QueueRepository(
            database,
            AndroidKeystoreCipher("atomic-reuse-${UUID.randomUUID()}"),
        )
        val identity = QueueIdentity("https://example.org", "a".repeat(43))
        val url = "https://example.org/atomic-reuse"

        val first = repository.enqueueOrReuse(url, identity, now = 5_600L)
        val second = repository.enqueueOrReuse(url, identity, now = 5_601L)

        assertFalse(first.reused)
        assertTrue(second.reused)
        assertEquals(first.entry.id, second.entry.id)
        assertEquals(first.entry.idempotencyKey, second.entry.idempotencyKey)
        assertEquals(1, repository.listAll().size)
    }

    @Test
    fun identityMigrationReencryptsAndResetsTransientQueueState() {
        val now = 6_000L
        val oldIdentity = QueueIdentity("https://old.example", "o".repeat(43))
        val newIdentity = QueueIdentity("https://new.example", "n".repeat(43))
        val repository = QueueRepository(
            database,
            AndroidKeystoreCipher("migration-${UUID.randomUUID()}"),
        )
        val entry = repository.enqueue("https://example.org/migrate", oldIdentity, now)
        val dao = database.queueDao()
        activate(repository, oldIdentity)

        assertEquals(1, claim(dao, entry.id, "migration-owner", now + 10_000, now))
        assertEquals(
            1,
            dao.updateClaimed(
                id = entry.id,
                queueOrigin = entry.apiOrigin,
                queueNamespace = entry.clientDataNamespace,
                owner = "migration-owner",
                state = "blocked_identity",
                firstFailedAt = now - 1_000,
                attemptCount = 4,
                nextAttemptAt = now + 60_000,
                lastErrorKind = "IDENTITY_MISMATCH",
                lastErrorCode = "namespace_changed",
                lastHttpStatus = 409,
                linkId = "old-link",
                jobId = "old-job",
                activeOrigin = entry.apiOrigin,
                activeNamespace = entry.clientDataNamespace,
                activationRevision = 1,
                updatedAt = now + 1,
            ),
        )
        val before = dao.findById(entry.id)!!

        assertTrue(repository.migrateIdentity(entry.id, newIdentity, now + 2))

        val migrated = dao.findById(entry.id)!!
        assertEquals(before.id, migrated.id)
        assertEquals(before.createdAt, migrated.createdAt)
        assertEquals(before.requestFingerprint, migrated.requestFingerprint)
        assertNotEquals(before.idempotencyKey, migrated.idempotencyKey)
        assertEquals(newIdentity.origin, migrated.apiOrigin)
        assertEquals(newIdentity.namespace, migrated.clientDataNamespace)
        assertEquals("pending_submit", migrated.state)
        assertEquals("https://example.org/migrate", repository.decode(migrated))
        assertNull(migrated.firstFailedAt)
        assertEquals(0, migrated.attemptCount)
        assertNull(migrated.nextAttemptAt)
        assertNull(migrated.lastErrorKind)
        assertNull(migrated.lastErrorCode)
        assertNull(migrated.lastHttpStatus)
        assertNull(migrated.linkId)
        assertNull(migrated.jobId)
        assertNull(migrated.leaseOwner)
        assertNull(migrated.leaseExpiresAt)
    }

    @Test
    fun identityMigrationRejectsAnActiveLeaseAndSameIdentity() {
        val now = 7_000L
        val oldIdentity = QueueIdentity("https://old.example", "o".repeat(43))
        val newIdentity = QueueIdentity("https://new.example", "n".repeat(43))
        val repository = QueueRepository(
            database,
            AndroidKeystoreCipher("migration-lease-${UUID.randomUUID()}"),
        )
        val entry = repository.enqueue("https://example.org/leased", oldIdentity, now)
        val dao = database.queueDao()

        assertEquals(1, claim(dao, entry.id, "active-owner", now + 10_000, now))
        assertFalse(repository.migrateIdentity(entry.id, newIdentity, now + 1))
        assertFalse(repository.migrateIdentity(entry.id, oldIdentity, now + 1))

        val unchanged = dao.findById(entry.id)!!
        assertEquals(oldIdentity.origin, unchanged.apiOrigin)
        assertEquals(oldIdentity.namespace, unchanged.clientDataNamespace)
        assertEquals("active-owner", unchanged.leaseOwner)
        assertEquals("https://example.org/leased", repository.decode(unchanged))
    }

    @Test
    fun identityMigrationRejectsNonCanonicalTargetBeforeChangingTheRow() {
        val now = 7_500L
        val oldIdentity = QueueIdentity("https://old.example", "o".repeat(43))
        val invalidTarget = QueueIdentity("http://new.example", "n".repeat(43))
        val repository = QueueRepository(
            database,
            AndroidKeystoreCipher("migration-invalid-${UUID.randomUUID()}"),
        )
        val entry = repository.enqueue("https://example.org/invalid-target", oldIdentity, now)

        assertThrows(IllegalStateException::class.java) {
            repository.migrateIdentity(entry.id, invalidTarget, now + 1)
        }

        val unchanged = database.queueDao().findById(entry.id)!!
        assertEquals(oldIdentity.origin, unchanged.apiOrigin)
        assertEquals(oldIdentity.namespace, unchanged.clientDataNamespace)
        assertEquals("https://example.org/invalid-target", repository.decode(unchanged))
    }

    @Test
    fun identityMatchedAuthBlocksAutomaticallyReturnToPending() {
        val now = 8_000L
        val identity = QueueIdentity("https://example.org", "i".repeat(43))
        val repository = QueueRepository(
            database,
            AndroidKeystoreCipher("identity-recovery-${UUID.randomUUID()}"),
        )
        val dao = database.queueDao()
        listOf("blocked_auth").forEachIndexed { index, state ->
            dao.insert(queueEntity("identity-recovery-$index", now, state = state).copy(
                apiOrigin = identity.origin,
                clientDataNamespace = identity.namespace,
            ))
        }

        assertEquals(1, repository.retryIdentityBlocked(identity, now))
        assertTrue(dao.listAll().all { it.state == "pending_submit" })
        assertTrue(dao.listAll().all { it.lastErrorKind == null && it.nextAttemptAt == null })
    }

    @Test
    fun retryRecoverableOnlyResetsRowsForTheRequestedIdentity() {
        val now = 8_500L
        val activeIdentity = QueueIdentity("https://active.example", "a".repeat(43))
        val otherIdentity = QueueIdentity("https://other.example", "b".repeat(43))
        val repository = QueueRepository(
            database,
            AndroidKeystoreCipher("retry-identity-${UUID.randomUUID()}"),
        )
        val dao = database.queueDao()
        dao.insert(
            queueEntity("active-retry", now, state = "retry_wait").copy(
                apiOrigin = activeIdentity.origin,
                clientDataNamespace = activeIdentity.namespace,
            ),
        )
        dao.insert(
            queueEntity("other-retry", now + 1, state = "retry_wait").copy(
                apiOrigin = otherIdentity.origin,
                clientDataNamespace = otherIdentity.namespace,
            ),
        )

        assertEquals(1, repository.retryRecoverable(activeIdentity, now))
        assertEquals("pending_submit", dao.findById("active-retry")!!.state)
        assertEquals("retry_wait", dao.findById("other-retry")!!.state)
    }

    @Test
    fun activeSessionMetadataRoundTripsWithoutStoringTheInstallationToken() {
        val repository = QueueRepository(
            database,
            AndroidKeystoreCipher("active-session-${UUID.randomUUID()}"),
        )
        val identity = SessionIdentity(
            origin = "https://example.org",
            clientDataNamespace = "s".repeat(43),
            representationContract = "v3",
        )

        repository.activateSession(identity)

        assertEquals(identity, repository.activeSessionIdentity())
        assertEquals(1, database.queueDao().activeSession()?.id)
    }

    @Test
    fun onlyTheLatestConnectionGenerationCanActivateAndAbaGetsNewRevisions() {
        val context = ApplicationProvider.getApplicationContext<Context>()
        val repository = QueueRepository(database, AndroidKeystoreCipher("connection-race-${UUID.randomUUID()}"))
        val credentials = EncryptedCredentialStore(
            context,
            AndroidKeystoreCipher("connection-race-credentials-${UUID.randomUUID()}"),
        )
        val namespaceA = "e".repeat(43)
        val namespaceB = "f".repeat(43)
        val delayedStarted = CountDownLatch(1)
        val releaseDelayed = CountDownLatch(1)
        val api = object : WebTagApi {
            override fun validateSession(rawOrigin: String, installationToken: String): ApiResult<SessionIdentity> = when (installationToken) {
                "old" -> {
                    delayedStarted.countDown()
                    check(releaseDelayed.await(5, TimeUnit.SECONDS))
                    ApiResult.Success(
                        SessionIdentity("https://a.example", namespaceA, REPRESENTATION_CONTRACT),
                        namespaceA,
                    )
                }
                "new" -> ApiResult.Success(
                    SessionIdentity("https://b.example", namespaceB, REPRESENTATION_CONTRACT),
                    namespaceB,
                )
                "again" -> ApiResult.Success(
                    SessionIdentity("https://a.example", namespaceA, REPRESENTATION_CONTRACT),
                    namespaceA,
                )
                else -> ApiResult.Failure(ClassifiedFailure(ErrorKind.INVALID_CLIENT_RESPONSE), null)
            }

            override fun submit(identity: SessionIdentity, installationToken: String, url: String, idempotencyKey: String): ApiResult<SubmitResponse> = error("not used")

            override fun refresh(identity: SessionIdentity, installationToken: String, linkId: String): ApiResult<SubmitResponse> = error("not used")
        }
        val coordinator = ConnectionCoordinator(repository, credentials, api)
        var delayedResult: ConnectionResult? = null
        val thread = Thread { delayedResult = coordinator.saveAndTest("https://a.example", "old") }
        try {
            thread.start()
            assertTrue(delayedStarted.await(5, TimeUnit.SECONDS))
            assertTrue(coordinator.saveAndTest("https://b.example", "new") is ConnectionResult.Activated)
            releaseDelayed.countDown()
            thread.join(5_000)
            assertEquals(ConnectionResult.IdentityChanged, delayedResult)
            val bSnapshot = repository.activeSessionSnapshot()!!
            assertEquals("https://b.example", bSnapshot.identity.origin)
            assertEquals(2L, bSnapshot.activationRevision)
            assertEquals(2L, credentials.recover(bSnapshot)!!.activationRevision)

            assertTrue(coordinator.saveAndTest("https://a.example", "again") is ConnectionResult.Activated)
            val aAgain = repository.activeSessionSnapshot()!!
            assertEquals("https://a.example", aAgain.identity.origin)
            assertEquals(3L, aAgain.activationRevision)
            assertEquals(3L, credentials.recover(aAgain)!!.activationRevision)
        } finally {
            releaseDelayed.countDown()
            thread.join(5_000)
            credentials.clear()
        }
    }

    @Test
    fun credentialRecoveryOnlyAcceptsTheMatchingPersistentSessionPair() {
        val context = ApplicationProvider.getApplicationContext<Context>()
        val credentialAlias = "recovery-credentials-${UUID.randomUUID()}"
        val repository = QueueRepository(database, AndroidKeystoreCipher("recovery-data-${UUID.randomUUID()}"))
        val store = EncryptedCredentialStore(context, AndroidKeystoreCipher(credentialAlias))
        val identityA = QueueIdentity("https://a.example", "g".repeat(43))
        val identityB = QueueIdentity("https://b.example", "h".repeat(43))
        try {
            val sessionA = activate(repository, identityA)
            store.save(CredentialConfig(identityA.origin, "token-a", identityA.namespace, 1))

            // A restarted store sees the durable matching pair, not an in-memory cache.
            val restarted = EncryptedCredentialStore(context, AndroidKeystoreCipher(credentialAlias))
            assertEquals("token-a", restarted.recover(sessionA)!!.installationToken)

            val sessionB = activateNext(repository, identityB)
            assertNull(restarted.recover(sessionB))
            restarted.stage(CredentialConfig(identityB.origin, "token-b", identityB.namespace, sessionB.activationRevision))
            assertEquals("token-b", restarted.recover(sessionB)!!.installationToken)
            assertEquals(sessionB.activationRevision, restarted.load()!!.activationRevision)
        } finally {
            store.clear()
        }
    }

    @Test
    fun submitCommitUsesTheCommitClockAndRejectsTheLeaseDeadline() {
        val now = 50_000L
        val identity = QueueIdentity("https://example.org", "t".repeat(43))
        val repository = QueueRepository(
            database,
            AndroidKeystoreCipher("commit-clock-${UUID.randomUUID()}"),
        )
        val activation = activate(repository, identity)

        val beforeDeadline = repository.enqueue("https://example.org/before", identity, now)
        assertEquals(
            SubmitClaimOutcome.APPLIED,
            repository.claimForEntry(beforeDeadline, "before-owner", activation, now, leaseMillis = 10),
        )
        assertEquals(
            SubmitCommitOutcome.APPLIED,
            repository.applyClaimed(
                beforeDeadline,
                "before-owner",
                QueueState.RETRY_WAIT,
                now,
                1,
                now + 100,
                ErrorKind.NO_NETWORK,
                null,
                null,
                null,
                null,
                activation,
                now + 9,
            ),
        )

        val exactDeadline = repository.enqueue("https://example.org/exact", identity, now)
        assertEquals(
            SubmitClaimOutcome.APPLIED,
            repository.claimForEntry(exactDeadline, "exact-owner", activation, now, leaseMillis = 10),
        )
        assertEquals(
            SubmitCommitOutcome.STALE_CLAIM,
            repository.applyClaimed(
                exactDeadline,
                "exact-owner",
                QueueState.FAILED_PERMANENT,
                now,
                1,
                null,
                ErrorKind.HTTP_5XX,
                null,
                500,
                null,
                null,
                activation,
                now + 10,
            ),
        )
        val unchanged = database.queueDao().findById(exactDeadline.id)!!
        assertEquals("exact-owner", unchanged.leaseOwner)
        assertEquals(now + 10, unchanged.leaseExpiresAt)
        assertEquals("pending_submit", unchanged.state)
    }

    @Test
    fun reclaimedOwnerCannotMutateTheNewClaimOrWriteRecentResult() {
        val now = 60_000L
        val identity = QueueIdentity("https://example.org", "u".repeat(43))
        val repository = QueueRepository(
            database,
            AndroidKeystoreCipher("reclaim-cas-${UUID.randomUUID()}"),
        )
        val activation = activate(repository, identity)
        val entry = repository.enqueue("https://example.org/reclaim", identity, now)
        assertEquals(
            SubmitClaimOutcome.APPLIED,
            repository.claimForEntry(entry, "owner-1", activation, now, leaseMillis = 10),
        )
        assertEquals(
            SubmitClaimOutcome.APPLIED,
            repository.claimForEntry(entry, "owner-2", activation, now + 10, leaseMillis = 10),
        )

        assertEquals(
            SubmitCommitOutcome.STALE_CLAIM,
            repository.applyClaimed(
                entry,
                "owner-1",
                QueueState.FAILED_PERMANENT,
                now,
                1,
                null,
                ErrorKind.HTTP_5XX,
                null,
                500,
                null,
                null,
                activation,
                now + 11,
            ),
        )
        assertEquals(
            SubmitCommitOutcome.STALE_CLAIM,
            repository.commitSuccess(
                entry,
                "owner-1",
                RecentResult(
                    "https://example.org/reclaim",
                    "11111111-1111-1111-1111-111111111111",
                    null,
                    "done",
                    now + 11,
                    identity,
                    null,
                    null,
                ),
                activation,
                now = now + 11,
            ),
        )
        val ownerTwo = database.queueDao().findById(entry.id)!!
        assertEquals("owner-2", ownerTwo.leaseOwner)
        assertEquals(now + 20, ownerTwo.leaseExpiresAt)
        assertEquals("pending_submit", ownerTwo.state)
        assertNull(database.queueDao().recent())
    }

    @Test
    fun identityChangeRejectsClaimedSuccessAndFailureWithoutTouchingEitherIdentity() {
        val now = 70_000L
        val identityA = QueueIdentity("https://a.example", "a".repeat(43))
        val identityB = QueueIdentity("https://b.example", "b".repeat(43))
        val repository = QueueRepository(
            database,
            AndroidKeystoreCipher("identity-cas-${UUID.randomUUID()}"),
        )
        val activationA = activate(repository, identityA)
        val entry = repository.enqueue("https://example.org/a", identityA, now)
        assertEquals(
            SubmitClaimOutcome.APPLIED,
            repository.claimForEntry(entry, "a-owner", activationA, now, leaseMillis = 100),
        )
        val activationB = activateNext(repository, identityB)
        database.queueDao().upsertRecent(recentEntity("b-link", "done", now, identityB))

        assertEquals(
            SubmitCommitOutcome.IDENTITY_CHANGED,
            repository.applyClaimed(
                entry,
                "a-owner",
                QueueState.FAILED_PERMANENT,
                now,
                1,
                null,
                ErrorKind.HTTP_5XX,
                null,
                500,
                null,
                null,
                activationA,
                now + 1,
            ),
        )
        assertEquals(
            SubmitCommitOutcome.IDENTITY_CHANGED,
            repository.commitSuccess(
                entry,
                "a-owner",
                RecentResult(
                    "https://example.org/a",
                    "22222222-2222-2222-2222-222222222222",
                    null,
                    "done",
                    now + 1,
                    identityA,
                    null,
                    null,
                ),
                activationA,
                now = now + 1,
            ),
        )
        assertEquals(identityB, QueueIdentity(activationB.identity.origin, activationB.identity.clientDataNamespace))
        assertEquals("a-owner", database.queueDao().findById(entry.id)!!.leaseOwner)
        assertEquals("b-link", database.queueDao().recent()!!.linkId)
    }

    @Test
    fun refreshClassifiesIdentityChangesBeforeLinkReplacementAndDoesNotMutate() {
        val now = 80_000L
        val identityA = QueueIdentity("https://a.example", "c".repeat(43))
        val identityB = QueueIdentity("https://b.example", "d".repeat(43))
        val repository = QueueRepository(
            database,
            AndroidKeystoreCipher("refresh-revision-${UUID.randomUUID()}"),
        )
        val activationA = activate(repository, identityA)
        database.queueDao().upsertRecent(recentEntity("a-link", "done", now, identityA))
        activateNext(repository, identityB)
        database.queueDao().upsertRecent(recentEntity("b-link", "done", now + 1, identityB))

        assertEquals(
            RefreshCommitOutcome.IDENTITY_CHANGED,
            repository.recordRefreshSuccess(
                activationA,
                "a-link",
                SubmitResponse("a-link", "processing", "late-job"),
                now + 2,
            ),
        )
        assertEquals("b-link", database.queueDao().recent()!!.linkId)

        val currentA = activateNext(repository, identityA)
        database.queueDao().upsertRecent(recentEntity("replacement", "done", now + 3, identityA))
        assertEquals(
            RefreshCommitOutcome.RECENT_REPLACED,
            repository.recordRefreshBlocked(currentA, "a-link", now + 60_000, "cooldown_active"),
        )
        val replacement = database.queueDao().recent()!!
        assertEquals("replacement", replacement.linkId)
        assertNull(replacement.refreshNotBefore)
        assertNull(replacement.refreshBlockReason)
    }

    @Test
    fun staleRefreshCannotOverwriteANewerRecentResultForTheSameIdentity() {
        val identity = QueueIdentity("https://example.org", "r".repeat(43))
        val repository = QueueRepository(
            database,
            AndroidKeystoreCipher("recent-cas-stale-${UUID.randomUUID()}"),
        )
        val dao = database.queueDao()
        val activation = activate(repository, identity)
        val oldResult = recentEntity("old-link", "failed", 1_000L, identity)
        val newResult = recentEntity("new-link", "done", 2_000L, identity)
        dao.upsertRecent(oldResult)
        dao.upsertRecent(newResult)

        assertEquals(RefreshCommitOutcome.RECENT_REPLACED,
            repository.recordRefreshSuccess(
                activation,
                expectedLinkId = oldResult.linkId,
                response = SubmitResponse(oldResult.linkId, "processing", "old-refresh-job"),
                now = 3_000L,
            ),
        )
        assertEquals(RefreshCommitOutcome.RECENT_REPLACED,
            repository.recordRefreshBlocked(
                activation,
                expectedLinkId = oldResult.linkId,
                refreshNotBefore = 63_000L,
                reason = "cooldown_active",
            ),
        )

        val stored = dao.recent()!!
        assertEquals(newResult.linkId, stored.linkId)
        assertEquals(newResult.jobId, stored.jobId)
        assertEquals(newResult.status, stored.status)
        assertEquals(newResult.createdAt, stored.createdAt)
        assertNull(stored.refreshNotBefore)
        assertNull(stored.refreshBlockReason)
        assertArrayEquals(newResult.urlCiphertext, stored.urlCiphertext)
        assertArrayEquals(newResult.urlNonce, stored.urlNonce)
    }

    @Test
    fun matchingRefreshCompareAndSetUpdatesTheCurrentRecentResult() {
        val identity = QueueIdentity("https://example.org", "c".repeat(43))
        val repository = QueueRepository(
            database,
            AndroidKeystoreCipher("recent-cas-current-${UUID.randomUUID()}"),
        )
        val current = recentEntity("current-link", "failed", 4_000L, identity).copy(
            refreshNotBefore = 64_000L,
            refreshBlockReason = "cooldown_active",
        )
        database.queueDao().upsertRecent(current)
        val activation = activate(repository, identity)

        assertEquals(RefreshCommitOutcome.APPLIED,
            repository.recordRefreshSuccess(
                activation,
                expectedLinkId = current.linkId,
                response = SubmitResponse(current.linkId, "processing", "current-refresh-job"),
                now = 5_000L,
            ),
        )
        assertEquals(RefreshCommitOutcome.APPLIED,
            repository.recordRefreshBlocked(
                activation,
                expectedLinkId = current.linkId,
                refreshNotBefore = 65_000L,
                reason = "cooldown_active",
            ),
        )

        val stored = database.queueDao().recent()!!
        assertEquals(current.linkId, stored.linkId)
        assertEquals("current-refresh-job", stored.jobId)
        assertEquals("processing", stored.status)
        assertEquals(5_000L, stored.createdAt)
        assertEquals(65_000L, stored.refreshNotBefore)
        assertEquals("cooldown_active", stored.refreshBlockReason)
    }

    @Test
    fun todoRepositoryKeepsEncryptedSnapshotsIdentityBoundAndOptimistic() {
        val cipher = AndroidKeystoreCipher("todo-store-${UUID.randomUUID()}")
        val repository = TodoRepository(database, cipher)
        val queueRepository = QueueRepository(database, cipher)
        val identityA = QueueIdentity("https://example.org", "a".repeat(43))
        val identityB = QueueIdentity("https://example.org", "b".repeat(43))
        val activation = activate(queueRepository, identityA)
        assertEquals(
            TodoCasOutcome.APPLIED,
            repository.replaceServerSnapshot(activation, listOf(todoItem("server-a", "server text", 2_000)), 3_000),
        )

        assertEquals(listOf("server text"), todoSnapshot(repository.snapshot(activation)).items.map { it.text })
        assertTrue(
            database.todoDao().listCache(
                identityB.origin,
                identityB.namespace,
                activation.activationRevision,
            ).isEmpty(),
        )

        val localID = repository.stageCreate(activation, TodoCreate("offline create", null), now = 4_000)
        val operationID = repository.stagePatch(
            activation,
            localID,
            TodoPatch(done = true),
            now = 4_001,
        )
        val optimistic = todoSnapshot(repository.snapshot(activation))
        assertEquals(2, optimistic.pendingOperations)
        assertTrue(optimistic.items.single { it.id == localID }.done)
        assertTrue(optimistic.items.single { it.id == localID }.localOnly)

        val createClaim = todoClaim(repository.claimDue(activation, 5_000, 30_000))
        val serverCreated = todoItem("server-created", "offline create", 5_000)
        assertEquals(
            TodoCasOutcome.APPLIED,
            repository.completeCreate(createClaim.operation, createClaim.owner, activation, serverCreated, 5_000),
        )

        val rebound = database.todoDao().findOperation(operationID)!!
        assertEquals(serverCreated.id, rebound.targetTodoId)
        assertTrue(todoSnapshot(repository.snapshot(activation)).items.single { it.id == serverCreated.id }.done)
        assertFalse(todoSnapshot(repository.snapshot(activation)).items.any { it.id == localID })
    }

    @Test
    fun todoOutboxClaimIsAtomicAndRejectsAStaleOwner() {
        val cipher = AndroidKeystoreCipher("todo-lease-${UUID.randomUUID()}")
        val repository = TodoRepository(database, cipher)
        val queueRepository = QueueRepository(database, cipher)
        val identity = QueueIdentity("https://example.org", "l".repeat(43))
        val activation = activate(queueRepository, identity)
        repository.stageCreate(activation, TodoCreate("leased create", null), now = 10_000)

        val first = todoClaim(repository.claimDue(activation, 11_000, 30_000))
        assertEquals(TodoClaimOutcome.NoWork, repository.claimDue(activation, 11_001, 30_000))
        val second = todoClaim(repository.claimDue(activation, 41_000, 30_000))
        val created = todoItem("leased-server", "leased create", 42_000)

        assertEquals(
            TodoCasOutcome.STALE_CLAIM,
            repository.completeCreate(first.operation, first.owner, activation, created, 42_000),
        )
        assertEquals(
            TodoCasOutcome.APPLIED,
            repository.completeCreate(second.operation, second.owner, activation, created, 42_000),
        )
        assertNull(database.todoDao().findOperation(first.operation.entity.operationId))
    }

    @Test
    fun replacingTodoServerSnapshotPreservesPendingDesiredState() {
        val cipher = AndroidKeystoreCipher("todo-replace-${UUID.randomUUID()}")
        val repository = TodoRepository(database, cipher)
        val queueRepository = QueueRepository(database, cipher)
        val identity = QueueIdentity("https://example.org", "c".repeat(43))
        val activation = activate(queueRepository, identity)
        val original = todoItem("replace", "old", 1_000)
        assertEquals(TodoCasOutcome.APPLIED, repository.replaceServerSnapshot(activation, listOf(original), 1_500))
        repository.stagePatch(activation, original.id, TodoPatch(text = "edited"), now = 1_600)
        assertEquals(
            TodoCasOutcome.APPLIED,
            repository.replaceServerSnapshot(activation, listOf(original.copy(updatedAt = 2_000)), 2_100),
        )

        val snapshot = todoSnapshot(repository.snapshot(activation))
        assertEquals("edited", snapshot.items.single().text)
        assertTrue(snapshot.items.single().pending)
    }

    @Test
    fun shareClaimRejectsAStaleActivationBeforeAnyLeaseMutation() {
        val cipher = AndroidKeystoreCipher("share-claim-revision-${UUID.randomUUID()}")
        val repository = QueueRepository(database, cipher)
        val identityA = QueueIdentity("https://a.example", "a".repeat(43))
        val identityB = QueueIdentity("https://b.example", "b".repeat(43))
        val activationA1 = activate(repository, identityA)
        val entry = repository.enqueue("https://example.org/stale-capture", identityA, now = 20_000)

        activateNext(repository, identityB)
        assertEquals(
            SubmitClaimOutcome.IDENTITY_CHANGED,
            repository.claimForEntry(entry, "stale-a1", activationA1, now = 20_001),
        )
        assertNull(database.queueDao().findById(entry.id)!!.leaseOwner)

        val activationA3 = activateNext(repository, identityA)
        assertEquals(
            SubmitClaimOutcome.IDENTITY_CHANGED,
            repository.claimForEntry(entry, "stale-a1-again", activationA1, now = 20_002),
        )
        assertNull(database.queueDao().findById(entry.id)!!.leaseOwner)
        assertEquals(
            SubmitClaimOutcome.APPLIED,
            repository.claimForEntry(entry, "current-a3", activationA3, now = 20_003),
        )
    }

    @Test
    fun todoRevisionFenceRejectsEveryOldCommitAndFreezesTheOperation() {
        val cipher = AndroidKeystoreCipher("todo-revision-cas-${UUID.randomUUID()}")
        val queueRepository = QueueRepository(database, cipher)
        val repository = TodoRepository(database, cipher)
        val identity = QueueIdentity("https://example.org", "v".repeat(43))
        val activation1 = activate(queueRepository, identity)
        repository.stageCreate(activation1, TodoCreate("old revision", null), now = 30_000)
        val claimed = todoClaim(repository.claimDue(activation1, 30_001, 30_000))

        val activation2 = activateNext(queueRepository, identity)
        assertEquals(
            TodoCasOutcome.IDENTITY_CHANGED,
            repository.completeCreate(
                claimed.operation,
                claimed.owner,
                activation1,
                todoItem("late-result", "old revision", 30_002),
                30_002,
            ),
        )
        assertEquals(
            TodoCasOutcome.IDENTITY_CHANGED,
            repository.updateOperation(
                claimed.operation.entity,
                claimed.owner,
                activation1,
                TodoOutboxState.RETRY_WAIT,
                90_000,
                ErrorKind.HTTP_5XX.name,
                30_002,
            ),
        )
        assertEquals(
            TodoCasOutcome.IDENTITY_CHANGED,
            repository.discardOperation(claimed.operation, claimed.owner, activation1, 30_002),
        )
        assertEquals(
            TodoCasOutcome.IDENTITY_CHANGED,
            repository.recordCapabilities(activation1, todos = true, home = true, inbox = true),
        )
        assertEquals(
            TodoCasOutcome.IDENTITY_CHANGED,
            repository.replaceServerSnapshot(activation1, listOf(todoItem("late-snapshot", "late", 30_003)), 30_003),
        )
        assertEquals(TodoSnapshotOutcome.IdentityChanged, repository.snapshot(activation1))
        assertEquals(TodoClaimOutcome.IdentityChanged, repository.claimDue(activation1, 60_001, 30_000))
        assertEquals(TodoClaimOutcome.NoWork, repository.claimDue(activation2, 60_001, 30_000))

        val stored = database.todoDao().findOperation(claimed.operation.entity.operationId)!!
        assertEquals(activation1.activationRevision, stored.activationRevision)
        assertEquals(TodoOutboxState.BLOCKED_IDENTITY.wireValue, stored.state)
        assertNull(stored.leaseOwner)
        assertTrue(
            database.todoDao().listCache(
                identity.origin,
                identity.namespace,
                activation2.activationRevision,
            ).isEmpty(),
        )
    }

    @Test
    fun todoCacheIsNotVisibleToANewerActivationOfTheSameIdentity() {
        val cipher = AndroidKeystoreCipher("todo-cache-revision-${UUID.randomUUID()}")
        val queueRepository = QueueRepository(database, cipher)
        val repository = TodoRepository(database, cipher)
        val identity = QueueIdentity("https://example.org", "r".repeat(43))
        val activation1 = activate(queueRepository, identity)
        assertEquals(
            TodoCasOutcome.APPLIED,
            repository.replaceServerSnapshot(
                activation1,
                listOf(todoItem(UUID.randomUUID().toString(), "revision one", 31_000)),
                31_001,
            ),
        )

        val activation2 = activateNext(queueRepository, identity)

        assertTrue(todoSnapshot(repository.snapshot(activation2)).items.isEmpty())
        assertEquals(1, database.todoDao().listCache(identity.origin, identity.namespace, activation1.activationRevision).size)
        assertTrue(database.todoDao().listCache(identity.origin, identity.namespace, activation2.activationRevision).isEmpty())
    }

    @Test
    fun todoCoordinatorStopsBeforePullWhenActivationChangesDuringPushCommit() {
        val cipher = AndroidKeystoreCipher("todo-sync-switch-${UUID.randomUUID()}")
        val queueRepository = QueueRepository(database, cipher)
        val repository = TodoRepository(database, cipher)
        val identity = QueueIdentity("https://example.org", "s".repeat(43))
        val activation1 = activate(queueRepository, identity)
        val configuration = CredentialConfig(identity.origin, "token", identity.namespace, activation1.activationRevision)
        var now = 32_000L
        repository.stageCreate(activation1, TodoCreate("late create", null), now)
        val api = CallbackTodoApi { _, request ->
            activateNext(queueRepository, identity)
            todoItem(UUID.randomUUID().toString(), request.text, now + 1)
        }
        val coordinator = TodoSyncCoordinator(
            repository,
            api,
            activeConfiguration = { configuration },
            activeSession = { queueRepository.activeSessionSnapshot() },
            clock = com.alpenl.webtag.share.queue.MobileClock { now },
        )

        val result = coordinator.synchronize()

        assertEquals(TodoCasOutcome.IDENTITY_CHANGED, result.casOutcome)
        assertEquals(0, result.pushed)
        assertFalse(result.pulled)
        assertEquals(0, api.listCalls)
        assertTrue(database.todoDao().listCache(identity.origin, identity.namespace, activation1.activationRevision).isEmpty())
    }

    @Test
    fun todoCoordinatorStopsBeforePullWhenLeaseIsReclaimedDuringPush() {
        val cipher = AndroidKeystoreCipher("todo-sync-stale-${UUID.randomUUID()}")
        val queueRepository = QueueRepository(database, cipher)
        val repository = TodoRepository(database, cipher)
        val identity = QueueIdentity("https://example.org", "t".repeat(43))
        val activation = activate(queueRepository, identity)
        val configuration = CredentialConfig(identity.origin, "token", identity.namespace, activation.activationRevision)
        var now = 33_000L
        repository.stageCreate(activation, TodoCreate("reclaimed create", null), now)
        var replacement: com.alpenl.webtag.share.data.ClaimedTodoOperation? = null
        val api = CallbackTodoApi { _, request ->
            now += TodoSyncCoordinator.LEASE_MILLIS + 1
            replacement = todoClaim(repository.claimDue(activation, now, TodoSyncCoordinator.LEASE_MILLIS))
            todoItem(UUID.randomUUID().toString(), request.text, now)
        }
        val coordinator = TodoSyncCoordinator(
            repository,
            api,
            activeConfiguration = { configuration },
            activeSession = { activation },
            clock = com.alpenl.webtag.share.queue.MobileClock { now },
        )

        val result = coordinator.synchronize()

        assertEquals(TodoCasOutcome.STALE_CLAIM, result.casOutcome)
        assertEquals(0, result.pushed)
        assertFalse(result.pulled)
        assertEquals(0, api.listCalls)
        assertEquals(replacement!!.owner, database.todoDao().findOperation(replacement!!.operation.entity.operationId)!!.leaseOwner)
    }

    @Test
    fun todoCoordinatorStopsBeforePullWhenClaimedEnvelopeFailsIntegrity() {
        val cipher = AndroidKeystoreCipher("todo-sync-integrity-${UUID.randomUUID()}")
        val queueRepository = QueueRepository(database, cipher)
        val repository = TodoRepository(database, cipher)
        val identity = QueueIdentity("https://example.org", "u".repeat(43))
        val activation = activate(queueRepository, identity)
        val configuration = CredentialConfig(identity.origin, "token", identity.namespace, activation.activationRevision)
        val now = 34_000L
        repository.stageCreate(activation, TodoCreate("tampered create", null), now)
        val api = CallbackTodoApi { operationId, request ->
            val claimed = requireNotNull(database.todoDao().findOperation(operationId))
            database.todoDao().updateOutbox(claimed.copy(updatedAt = claimed.updatedAt + 1))
            todoItem(UUID.randomUUID().toString(), request.text, now + 1)
        }
        val coordinator = TodoSyncCoordinator(
            repository,
            api,
            activeConfiguration = { configuration },
            activeSession = { activation },
            clock = com.alpenl.webtag.share.queue.MobileClock { now },
        )

        val result = coordinator.synchronize()

        assertEquals(TodoCasOutcome.INTEGRITY_FAILURE, result.casOutcome)
        assertEquals(0, result.pushed)
        assertFalse(result.pulled)
        assertEquals(0, api.listCalls)
        assertTrue(database.todoDao().listCache(identity.origin, identity.namespace, activation.activationRevision).isEmpty())
    }

    @Test
    fun todoEnvelopeTamperingFailsBeforeClaimAndDoesNotAdvanceState() {
        val cipher = AndroidKeystoreCipher("todo-envelope-tamper-${UUID.randomUUID()}")
        val queueRepository = QueueRepository(database, cipher)
        val repository = TodoRepository(database, cipher)
        val identity = QueueIdentity("https://example.org", "e".repeat(43))
        val activation = activate(queueRepository, identity)
        val operationId = repository.stagePatch(
            activation,
            UUID.randomUUID().toString(),
            TodoPatch(text = "authenticated"),
            now = 40_000,
        )
        val original = database.todoDao().findOperation(operationId)!!

        listOf(
            original.copy(kind = TodoOutboxKind.DELETE.wireValue),
            original.copy(targetTodoId = UUID.randomUUID().toString()),
            original.copy(state = TodoOutboxState.RETRY_WAIT.wireValue),
            original.copy(attemptCount = 9),
            original.copy(cryptoVersion = 2),
            original.copy(leaseOwner = "tampered", leaseExpiresAt = 40_000),
            original.copy(updatedAt = 40_001),
        ).forEach { tampered ->
            database.todoDao().updateOutbox(tampered)
            assertEquals(TodoClaimOutcome.IntegrityFailure, repository.claimDue(activation, 40_001, 30_000))
            val unchanged = database.todoDao().findOperation(operationId)!!
            assertNull(unchanged.leaseOwner)
            assertEquals(tampered.attemptCount, unchanged.attemptCount)
            database.todoDao().updateOutbox(original)
        }

        val secondId = repository.stageDelete(
            activation,
            UUID.randomUUID().toString(),
            now = 40_002,
        )
        val second = database.todoDao().findOperation(secondId)!!
        database.todoDao().updateOutbox(
            original.copy(
                payloadCiphertext = second.payloadCiphertext,
                payloadNonce = second.payloadNonce,
                cryptoVersion = second.cryptoVersion,
            ),
        )
        assertEquals(TodoClaimOutcome.IntegrityFailure, repository.claimDue(activation, 40_003, 30_000))
        assertNull(database.todoDao().findOperation(operationId)!!.leaseOwner)
    }

    @Test
    fun authenticatedTodoRetryTransitionCanBeDecodedAndReclaimed() {
        val cipher = AndroidKeystoreCipher("todo-envelope-retry-${UUID.randomUUID()}")
        val queueRepository = QueueRepository(database, cipher)
        val repository = TodoRepository(database, cipher)
        val identity = QueueIdentity("https://example.org", "q".repeat(43))
        val activation = activate(queueRepository, identity)
        repository.stageDelete(activation, UUID.randomUUID().toString(), now = 45_000)
        val first = todoClaim(repository.claimDue(activation, 45_001, 30_000))

        assertEquals(
            TodoCasOutcome.APPLIED,
            repository.updateOperation(
                first.operation.entity,
                first.owner,
                activation,
                TodoOutboxState.RETRY_WAIT,
                nextAttemptAt = 50_000,
                errorKind = ErrorKind.HTTP_5XX.name,
                now = 45_002,
            ),
        )
        assertEquals(TodoClaimOutcome.NoWork, repository.claimDue(activation, 49_999, 30_000))
        val retry = todoClaim(repository.claimDue(activation, 50_000, 30_000))
        assertEquals(first.operation.entity.operationId, retry.operation.entity.operationId)
        assertEquals(1, retry.operation.entity.attemptCount)
        assertEquals(TodoOutboxState.RETRY_WAIT.wireValue, retry.operation.entity.state)
    }

    @Test
    fun authenticatedTodoBlockedAndConflictTransitionsRemainDecodable() {
        val cipher = AndroidKeystoreCipher("todo-envelope-blocked-${UUID.randomUUID()}")
        val queueRepository = QueueRepository(database, cipher)
        val repository = TodoRepository(database, cipher)
        val identity = QueueIdentity("https://example.org", "b".repeat(43))
        val activation = activate(queueRepository, identity)

        listOf(TodoOutboxState.BLOCKED_AUTH, TodoOutboxState.CONFLICT).forEachIndexed { index, state ->
            val operationId = repository.stageDelete(activation, UUID.randomUUID().toString(), now = 46_000L + index)
            val claimed = todoClaim(repository.claimDue(activation, 46_100L + index, 30_000))
            assertEquals(operationId, claimed.operation.entity.operationId)
            assertEquals(
                TodoCasOutcome.APPLIED,
                repository.updateOperation(
                    claimed.operation.entity,
                    claimed.owner,
                    activation,
                    state,
                    nextAttemptAt = null,
                    errorKind = if (state == TodoOutboxState.CONFLICT) ErrorKind.HTTP_409.name else ErrorKind.HTTP_401.name,
                    now = 46_200L + index,
                ),
            )
            val stored = requireNotNull(database.todoDao().findOperation(operationId))
            assertEquals(state.wireValue, stored.state)
            assertEquals(
                "{}",
                cipher.decrypt(
                    com.alpenl.webtag.share.security.EncryptedValue(
                        stored.payloadCiphertext,
                        stored.payloadNonce,
                        stored.cryptoVersion,
                    ),
                    com.alpenl.webtag.share.data.TodoOutboxEnvelope.canonicalAad(stored),
                ),
            )
        }
    }

    @Test
    fun persistentTodoRetryGateCannotBeShortenedByNewWorkOrRepositoryRestart() {
        val cipher = AndroidKeystoreCipher("todo-retry-gate-${UUID.randomUUID()}")
        val queueRepository = QueueRepository(database, cipher)
        val repository = TodoRepository(database, cipher)
        val identity = QueueIdentity("https://example.org", "g".repeat(43))
        val activation = activate(queueRepository, identity)
        repository.stageCreate(activation, TodoCreate("first", null), now = 50_000)
        assertEquals(
            TodoCasOutcome.APPLIED,
            repository.recordCapabilitiesFailure(
                activation,
                TodoSyncGateState.RETRY_WAIT,
                nextAttemptAt = 110_000,
                errorKind = ErrorKind.HTTP_5XX.name,
            ),
        )
        assertEquals(110_000L, repository.nextScheduleAt(activation, 50_001))

        repository.stageCreate(activation, TodoCreate("second", null), now = 50_002)
        assertEquals(110_000L, TodoRepository(database, cipher).nextScheduleAt(activation, 50_003))
        assertEquals(
            TodoGateDecision.Deferred(110_000, 1),
            TodoRepository(database, cipher).gate(activation, 50_004),
        )

        assertEquals(
            TodoCasOutcome.APPLIED,
            repository.recordCapabilities(activation, todos = true, home = false, inbox = false),
        )
        assertEquals(50_005L, repository.nextScheduleAt(activation, 50_005))
    }

    @Test
    fun repeatedTodoSchedulingLeavesAtMostOneEffectiveWorkRequest() {
        val context = ApplicationProvider.getApplicationContext<Context>()
        val cipher = AndroidKeystoreCipher("todo-unique-work-${UUID.randomUUID()}")
        val queueRepository = QueueRepository(database, cipher)
        val repository = TodoRepository(database, cipher)
        val identity = QueueIdentity("https://example.org", "w".repeat(43))
        val activation = activate(queueRepository, identity)
        val now = System.currentTimeMillis()
        repository.stageCreate(activation, TodoCreate("scheduled", null), now)
        repository.recordCapabilitiesFailure(
            activation,
            TodoSyncGateState.RETRY_WAIT,
            nextAttemptAt = now + 60_000,
            errorKind = ErrorKind.HTTP_5XX.name,
        )
        val scheduler = TodoSyncScheduler(context, repository) { activation }
        val workManager = WorkManager.getInstance(context)
        try {
            scheduler.schedule(now)
            scheduler.schedule(now + 1)
            val work = workManager.getWorkInfosForUniqueWork(TodoSyncScheduler.UNIQUE_WORK_NAME)
                .get(5, TimeUnit.SECONDS)
            assertTrue(work.count { !it.state.isFinished } <= 1)
            assertTrue(work.none { it.state == WorkInfo.State.BLOCKED })
        } finally {
            workManager.cancelUniqueWork(TodoSyncScheduler.UNIQUE_WORK_NAME).result.get(5, TimeUnit.SECONDS)
        }
    }

    @Test
    fun capabilitiesFailureIsGatedAcrossRepeatedSyncNewWorkAndRepositoryRestart() {
        val cipher = AndroidKeystoreCipher("todo-capabilities-gate-${UUID.randomUUID()}")
        val queueRepository = QueueRepository(database, cipher)
        val repository = TodoRepository(database, cipher)
        val identity = QueueIdentity("https://example.org", "z".repeat(43))
        val activation = activate(queueRepository, identity)
        val configuration = CredentialConfig(
            identity.origin,
            "token",
            identity.namespace,
            activation.activationRevision,
        )
        val api = FailingCapabilitiesApi()
        var now = 60_000L
        fun coordinator(target: TodoRepository) = TodoSyncCoordinator(
            target,
            api,
            activeConfiguration = { configuration },
            activeSession = { activation },
            clock = com.alpenl.webtag.share.queue.MobileClock { now },
        )
        repository.stageCreate(activation, TodoCreate("first", null), now)

        val first = coordinator(repository).synchronize()
        assertEquals(1, api.capabilitiesCalls)
        assertEquals(90_000L, first.nextAttemptAt)

        now += 1
        repository.stageCreate(activation, TodoCreate("new work cannot shorten the gate", null), now)
        val beforeDeadline = coordinator(TodoRepository(database, cipher)).synchronize()
        assertEquals(1, api.capabilitiesCalls)
        assertEquals(90_000L, beforeDeadline.nextAttemptAt)

        now = 90_000L
        val second = coordinator(TodoRepository(database, cipher)).synchronize()
        assertEquals(2, api.capabilitiesCalls)
        assertEquals(150_000L, second.nextAttemptAt)
    }

    private fun todoSnapshot(outcome: TodoSnapshotOutcome): com.alpenl.webtag.share.data.TodoLocalSnapshot =
        (outcome as TodoSnapshotOutcome.Applied).snapshot

    private fun todoClaim(outcome: TodoClaimOutcome): com.alpenl.webtag.share.data.ClaimedTodoOperation =
        (outcome as TodoClaimOutcome.Claimed).value

    private fun claim(
        dao: com.alpenl.webtag.share.data.QueueDao,
        id: String,
        owner: String,
        expiresAt: Long,
        now: Long,
    ): Int = dao.claim(
        id = id,
        apiOrigin = "https://example.org",
        namespace = "n".repeat(43),
        owner = owner,
        expiresAt = expiresAt,
        now = now,
        activeOrigin = "https://example.org",
        activeNamespace = "n".repeat(43),
        activationRevision = 1,
    )

    private fun queueEntity(id: String, now: Long, state: String = "pending_submit") = QueueEntity(
        id = id,
        schemaVersion = 1,
        urlCiphertext = byteArrayOf(1, 2, 3),
        urlNonce = byteArrayOf(4, 5, 6),
        cryptoVersion = 1,
        idempotencyKey = "idempotency-$id",
        requestFingerprint = "fingerprint-$id",
        apiOrigin = "https://example.org",
        clientDataNamespace = "n".repeat(43),
        state = state,
        createdAt = now,
        firstFailedAt = null,
        attemptCount = 0,
        nextAttemptAt = null,
        lastErrorKind = null,
        lastErrorCode = null,
        lastHttpStatus = null,
        linkId = null,
        jobId = null,
        leaseOwner = null,
        leaseExpiresAt = null,
        updatedAt = now,
    )

    private fun todoItem(seed: String, text: String, updatedAt: Long): TodoItem = TodoItem(
        id = UUID.nameUUIDFromBytes(seed.toByteArray()).toString(),
        text = text,
        dueAt = null,
        done = false,
        originKind = TodoOriginKind.STANDALONE,
        originHostKind = null,
        originHostId = null,
        originRefJson = null,
        hostRevision = 0,
        completedAt = null,
        createdAt = 1_000,
        updatedAt = updatedAt,
        expired = false,
    )

    private fun recentEntity(
        linkId: String,
        status: String,
        createdAt: Long,
        identity: QueueIdentity,
    ) = RecentResultEntity(
        urlCiphertext = byteArrayOf(linkId.length.toByte(), 2, 3),
        urlNonce = byteArrayOf(4, 5, 6),
        cryptoVersion = 1,
        linkId = linkId,
        jobId = "job-$linkId",
        status = status,
        createdAt = createdAt,
        apiOrigin = identity.origin,
        clientDataNamespace = identity.namespace,
        refreshNotBefore = null,
        refreshBlockReason = null,
    )

    private fun activate(repository: QueueRepository, identity: QueueIdentity): ActiveSessionSnapshot {
        val session = SessionIdentity(identity.origin, identity.namespace, REPRESENTATION_CONTRACT)
        repository.activateSession(session)
        return repository.activeSessionSnapshot()!!
    }

    private fun activateNext(repository: QueueRepository, identity: QueueIdentity): ActiveSessionSnapshot {
        val session = SessionIdentity(identity.origin, identity.namespace, REPRESENTATION_CONTRACT)
        val generation = repository.beginActivationAttempt()
        assertEquals(com.alpenl.webtag.share.contract.ActivationCommitOutcome.APPLIED, repository.activateSessionIfLatest(session, generation))
        return repository.activeSessionSnapshot()!!
    }
}

private class FailingCapabilitiesApi : WebTagCompanionApi {
    var capabilitiesCalls = 0

    override fun capabilities(identity: SessionIdentity, installationToken: String): ApiResult<TodoCapabilities> {
        capabilitiesCalls += 1
        return ApiResult.Failure(ClassifiedFailure(ErrorKind.HTTP_5XX), identity.clientDataNamespace)
    }

    override fun listTodos(identity: SessionIdentity, installationToken: String): ApiResult<List<TodoItem>> =
        error("listTodos must remain behind the capabilities gate")

    override fun createTodo(
        identity: SessionIdentity,
        installationToken: String,
        request: TodoCreate,
        idempotencyKey: String,
    ): ApiResult<TodoItem> = error("createTodo must remain behind the capabilities gate")

    override fun patchTodo(
        identity: SessionIdentity,
        installationToken: String,
        todoId: String,
        patch: TodoPatch,
        idempotencyKey: String,
    ): ApiResult<TodoItem> = error("patchTodo must remain behind the capabilities gate")

    override fun deleteTodo(
        identity: SessionIdentity,
        installationToken: String,
        todoId: String,
        idempotencyKey: String,
    ): ApiResult<Unit> = error("deleteTodo must remain behind the capabilities gate")

    override fun home(identity: SessionIdentity, installationToken: String): ApiResult<HomeSnapshot> =
        error("home is not part of TODO synchronization")
}

private class CallbackTodoApi(
    private val create: (String, TodoCreate) -> TodoItem,
) : WebTagCompanionApi {
    var listCalls = 0

    override fun capabilities(identity: SessionIdentity, installationToken: String): ApiResult<TodoCapabilities> =
        ApiResult.Success(TodoCapabilities(todos = true, home = false, inbox = false), identity.clientDataNamespace)

    override fun listTodos(identity: SessionIdentity, installationToken: String): ApiResult<List<TodoItem>> {
        listCalls += 1
        return ApiResult.Success(emptyList(), identity.clientDataNamespace)
    }

    override fun createTodo(
        identity: SessionIdentity,
        installationToken: String,
        request: TodoCreate,
        idempotencyKey: String,
    ): ApiResult<TodoItem> = ApiResult.Success(create(idempotencyKey, request), identity.clientDataNamespace)

    override fun patchTodo(
        identity: SessionIdentity,
        installationToken: String,
        todoId: String,
        patch: TodoPatch,
        idempotencyKey: String,
    ): ApiResult<TodoItem> = error("patchTodo is not expected")

    override fun deleteTodo(
        identity: SessionIdentity,
        installationToken: String,
        todoId: String,
        idempotencyKey: String,
    ): ApiResult<Unit> = error("deleteTodo is not expected")

    override fun home(identity: SessionIdentity, installationToken: String): ApiResult<HomeSnapshot> =
        error("home is not part of TODO synchronization")
}
